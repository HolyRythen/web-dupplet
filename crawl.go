package main

import (
	"bufio"
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Crawler: durchsucht eine Webseite (nur derselbe Host, nur unterhalb des Start-Pfads)
// nach Links auf Dateien mit bestimmten Endungen (Standard: pdf) und übergibt sie der Engine.

type CrawlStatus struct {
	URL     string `json:"url"`
	Exts    string `json:"exts"`
	Base    string `json:"base,omitempty"`
	Pages   int64  `json:"pages"`
	Found   int64  `json:"found"`
	Queued  int    `json:"queued"`
	Running bool   `json:"running"`
	Err     string `json:"err,omitempty"`
	Index   bool   `json:"index"`
	cancel  context.CancelFunc
}

type crawlReg struct {
	mu sync.Mutex
	m  map[string]*CrawlStatus
}

var hrefRe = regexp.MustCompile(`(?i)\bhref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)

// Sammelbegriffe für die Dateityp-Auswahl
var extGroups = map[string]string{
	"word":      "doc,docx,docm,dot,dotx,odt,rtf",
	"office":    "doc,docx,docm,dot,dotx,odt,rtf,xls,xlsx,xlsm,ods,csv,ppt,pptx,pps,ppsx,odp,pdf",
	"dokumente": "doc,docx,docm,dot,dotx,odt,rtf,xls,xlsx,xlsm,ods,csv,ppt,pptx,pps,ppsx,odp,pdf,txt,epub,djvu,ps",
	"bilder":    "jpg,jpeg,png,gif,bmp,tif,tiff,webp,heic,heif,avif,svg,ico,psd,raw,cr2,cr3,nef,arw,dng,orf,rw2",
	"videos":    "mp4,m4v,mkv,avi,mov,wmv,flv,webm,mpg,mpeg,m2ts,mts,ts,vob,3gp,ogv",
	"audio":     "mp3,flac,wav,ogg,m4a,aac,wma,opus",
	"archive":   "zip,rar,7z,tar,gz,tgz,bz2,xz,iso,img",
}

// normExts: nil = ALLE Dateitypen ("*" oder "alle").
func normExts(s string) []string {
	var out []string
	for _, x := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		x = strings.TrimPrefix(x, ".")
		if x == "*" || x == "alle" || x == "all" {
			return nil
		}
		if g, ok := extGroups[x]; ok {
			out = append(out, normExts(g)...)
			continue
		}
		out = append(out, "."+x)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// matchExt prüft, ob ein Pfad zur Auswahl passt (exts == nil → alles).
func matchExt(exts []string, p string) bool {
	if exts == nil {
		return true
	}
	x := strings.ToLower(path.Ext(p))
	for _, w := range exts {
		if x == w {
			return true
		}
	}
	return false
}

// StartCrawl merkt sich den Crawl in der DB (wird nach Neustart fortgesetzt) und startet ihn.
func (e *Engine) StartCrawl(ctx context.Context, start, exts, base string) error {
	u, err := url.Parse(strings.TrimSpace(start))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "rsync") || u.Host == "" {
		return errPermanent{errBadURL}
	}
	if exts == "" {
		exts = "*"
	}
	start = u.String()
	e.crawls.mu.Lock()
	if c, ok := e.crawls.m[start]; ok && c.Running {
		e.crawls.mu.Unlock()
		return nil
	}
	if u.Scheme == "rsync" && base == "" {
		base = defaultBase(u)
	}
	st := &CrawlStatus{URL: start, Exts: exts, Base: base, Running: true, Index: u.Scheme == "rsync"}
	e.crawls.m[start] = st
	e.crawls.mu.Unlock()
	e.rdbExecW(`INSERT INTO crawls(url,exts,done,base) VALUES(?,?,0,?) ON CONFLICT(url) DO UPDATE SET exts=excluded.exts, base=excluded.base, done=0`, start, exts, base)
	cctx, cancel := context.WithCancel(ctx)
	st.cancel = cancel
	if u.Scheme == "rsync" {
		go func() {
			err := e.rsyncList(cctx, u, base, normExts(exts), st)
			e.crawls.mu.Lock()
			st.Running = false
			if err != nil {
				st.Err = err.Error()
			}
			e.crawls.mu.Unlock()
			if cctx.Err() == nil {
				e.rdbExecW(`UPDATE crawls SET done=1 WHERE url=?`, st.URL)
			}
			log.Printf("rsync %s beendet: %d Einträge, %d Dateien (%v)", st.URL, st.Pages, st.Found, err)
		}()
		return nil
	}
	go e.crawl(cctx, u, normExts(exts), st)
	return nil
}

var errBadURL = &strErr{"ungültige URL (http:// oder https:// erwartet)"}

type strErr struct{ s string }

func (e *strErr) Error() string { return e.s }

func (e *Engine) crawl(ctx context.Context, start *url.URL, exts []string, st *CrawlStatus) {
	defer func() {
		e.crawls.mu.Lock()
		st.Running = false
		e.crawls.mu.Unlock()
		if ctx.Err() == nil || e.ctx.Err() == nil {
			e.rdbExecW(`UPDATE crawls SET done=1 WHERE url=?`, st.URL)
		}
		log.Printf("Crawl %s beendet: %d Seiten, %d Dateien", st.URL, st.Pages, st.Found)
	}()
	prefix := start.Scheme + "://" + start.Host + start.Path[:strings.LastIndex(start.Path, "/")+1]
	disallow := e.robots(ctx, start)
	allowed := func(u *url.URL) bool {
		for _, d := range disallow {
			if strings.HasPrefix(u.Path, d) {
				return false
			}
		}
		return true
	}
	isPage := func(p string) bool {
		switch strings.ToLower(path.Ext(p)) {
		case "", ".html", ".htm", ".php", ".asp", ".aspx", ".jsp", ".shtml":
			return true
		}
		return false
	}
	isFile := func(p string) bool {
		if exts == nil { // "alle": jede verlinkte Datei, aber keine HTML-Seiten/Ordner
			return !isPage(p) && !strings.HasSuffix(p, "/")
		}
		return matchExt(exts, p)
	}

	seen := map[string]struct{}{start.String(): {}}
	queue := []string{start.String()}
	batch := make([]AddItem, 0, 500)
	flush := func() {
		if len(batch) > 0 {
			select {
			case e.addC <- batch:
			case <-ctx.Done():
			}
			batch = make([]AddItem, 0, 500)
		}
	}
	for len(queue) > 0 && ctx.Err() == nil {
		for e.paused.Load() && ctx.Err() == nil {
			time.Sleep(time.Second)
		}
		page := queue[0]
		queue = queue[1:]
		links, base, sizes, isIndex, err := e.fetchPage(ctx, page, isFile)
		e.crawls.mu.Lock()
		st.Pages++
		st.Queued = len(queue)
		if err != nil {
			st.Err = page + ": " + err.Error()
		}
		e.crawls.mu.Unlock()
		if isIndex {
			e.crawls.mu.Lock()
			st.Index = true
			e.crawls.mu.Unlock()
		}
		for i, l := range links {
			var u *url.URL
			var err error
			if isIndex { // Dateiliste: Pfade sind relativ zum Ordner der Liste
				u = base.ResolveReference(&url.URL{Path: l})
			} else {
				u, err = base.Parse(l)
			}
			if err != nil || u.Host != start.Host || (u.Scheme != "http" && u.Scheme != "https") {
				continue
			}
			u.Fragment = ""
			if u.RawQuery != "" && !isFile(u.Path) { // z.B. Sortier-Links in Verzeichnislisten
				continue
			}
			s := u.String()
			if _, ok := seen[s]; ok || !allowed(u) {
				continue
			}
			seen[s] = struct{}{}
			switch {
			case isFile(u.Path):
				sz := int64(-2)
				if sizes != nil && sizes[i] > 0 {
					sz = sizes[i] // Größe aus der Liste → spart den HEAD-Request
				}
				batch = append(batch, AddItem{Loc: s, Kind: kindHTTP, Size: sz})
				e.crawls.mu.Lock()
				st.Found++
				e.crawls.mu.Unlock()
				if len(batch) == cap(batch) {
					flush()
				}
			case strings.HasPrefix(s, prefix) && isPage(u.Path):
				queue = append(queue, s)
			}
		}
		flush()
		time.Sleep(150 * time.Millisecond) // höflich bleiben
	}
	flush()
}

// fetchPage lädt eine HTML-Seite (→ Links) oder eine Text-Dateiliste
// wie bitsavers "IndexByDate.txt" oder CTAN "FILES.byname" (→ Pfade + Größen).
func (e *Engine) fetchPage(ctx context.Context, page string, isFile func(string) bool) ([]string, *url.URL, []int64, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*e.cfg.Stall)
	defer cancel()
	resp, err := e.fet.req(ctx, http.MethodGet, page, "")
	if err != nil {
		return nil, nil, nil, false, err
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	if looksLikeIndex(ct, br) {
		links, sizes, err := parseIndex(br, isFile)
		return links, resp.Request.URL, sizes, true, err
	}
	if ct != "" && !strings.Contains(ct, "html") {
		return nil, resp.Request.URL, nil, false, nil
	}
	body, err := io.ReadAll(io.LimitReader(br, 32<<20))
	if err != nil {
		return nil, nil, nil, false, err
	}
	var links []string
	for _, m := range hrefRe.FindAllSubmatch(body, -1) {
		l := string(m[1]) + string(m[2]) + string(m[3])
		l = strings.ReplaceAll(l, "&amp;", "&")
		if l != "" && !strings.HasPrefix(l, "javascript:") && !strings.HasPrefix(l, "mailto:") {
			links = append(links, l)
		}
	}
	return links, resp.Request.URL, nil, false, nil
}

// looksLikeIndex: Text-Dateiliste statt HTML? (manche Server senden gar keinen Content-Type)
func looksLikeIndex(ct string, br *bufio.Reader) bool {
	if strings.HasPrefix(ct, "text/plain") {
		return true
	}
	if strings.Contains(ct, "html") {
		return false
	}
	head, _ := br.Peek(4096)
	l := strings.ToLower(string(head))
	if strings.Contains(l, "<html") || strings.Contains(l, "<!doctype") || strings.Contains(l, "<a ") {
		return false
	}
	return strings.HasPrefix(http.DetectContentType(head), "text/plain")
}

var dateTok = regexp.MustCompile(`^[0-9][0-9:/.\-]*$`)

// parseIndex versteht Zeilen wie
//   "2026-11-09 17:34:40 sds/datei.pdf"          (bitsavers)
//   "2026/08/01 |  29179 | biblio/datei.pdf"     (CTAN, mit Größe)
//   "pfad/datei.pdf"                             (einfache Liste)
func parseIndex(r io.Reader, isFile func(string) bool) ([]string, []int64, error) {
	var links []string
	var sizes []int64
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		var p string
		size := int64(-2)
		if strings.Contains(line, "|") {
			f := strings.Split(line, "|")
			p = strings.TrimSpace(f[len(f)-1])
			if len(f) >= 3 {
				if n, err := strconv.ParseInt(strings.TrimSpace(f[len(f)-2]), 10, 64); err == nil {
					size = n
				}
			}
		} else {
			rest := line
			for {
				tok, after, ok := strings.Cut(rest, " ")
				if !ok || !dateTok.MatchString(tok) {
					break
				}
				rest = strings.TrimLeft(after, " ")
			}
			p = rest
		}
		if p == "" || strings.HasPrefix(p, "#") || !isFile(p) {
			continue
		}
		links = append(links, strings.TrimPrefix(p, "./"))
		sizes = append(sizes, size)
	}
	return links, sizes, sc.Err()
}

// robots liest Disallow-Regeln für "*" bzw. unseren User-Agent.
func (e *Engine) robots(ctx context.Context, start *url.URL) []string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := e.fet.req(ctx, http.MethodGet, start.Scheme+"://"+start.Host+"/robots.txt", "")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/plain") {
		return nil
	}
	var out []string
	active := false
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	lastWasUA := false
	for sc.Scan() {
		line := strings.TrimSpace(strings.SplitN(sc.Text(), "#", 2)[0])
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "user-agent":
			ua := strings.ToLower(v)
			m := ua == "*" || strings.Contains(strings.ToLower(e.cfg.UserAgent), ua)
			if lastWasUA {
				active = active || m
			} else {
				active = m
			}
			lastWasUA = true
			continue
		case "disallow":
			if active && v != "" {
				out = append(out, v)
			}
		}
		lastWasUA = false
	}
	return out
}

// StopCrawl bricht einen Crawl ab (bereits gefundene Dateien bleiben).
func (e *Engine) StopCrawl(u string) bool {
	e.crawls.mu.Lock()
	c, ok := e.crawls.m[u]
	e.crawls.mu.Unlock()
	if !ok {
		return false
	}
	if c.cancel != nil {
		c.cancel()
	}
	e.rdbExecW(`UPDATE crawls SET done=1 WHERE url=?`, u)
	return true
}

func (e *Engine) CrawlList() []CrawlStatus {
	e.crawls.mu.Lock()
	defer e.crawls.mu.Unlock()
	out := make([]CrawlStatus, 0, len(e.crawls.m))
	for _, c := range e.crawls.m {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

// resumeCrawls startet nach einem Neustart alle nicht beendeten Crawls erneut
// (bereits bekannte Dateien werden dabei einfach übersprungen).
func (e *Engine) resumeCrawls(ctx context.Context) {
	rows, err := e.rdb.Query(`SELECT url, exts, coalesce(base,'') FROM crawls WHERE done=0`)
	if err != nil {
		return
	}
	var todo [][3]string
	for rows.Next() {
		var u, x, b string
		rows.Scan(&u, &x, &b)
		todo = append(todo, [3]string{u, x, b})
	}
	rows.Close()
	for _, t := range todo {
		e.StartCrawl(ctx, t[0], t[1], t[2])
	}
	for _, u := range e.cfg.Crawls {
		e.StartCrawl(ctx, u, e.cfg.Exts, "")
	}
}

// rdbExecW: kleine Schreibzugriffe außerhalb des Core (SQLite serialisiert über busy_timeout).
func (e *Engine) rdbExecW(q string, args ...any) {
	if _, err := e.db.Exec(q, args...); err != nil {
		log.Printf("crawls: %v", err)
	}
}

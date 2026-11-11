package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed ui
var uiFS embed.FS

type Stats struct {
	Total         int64   `json:"total"`
	Checked       int64   `json:"checked"`
	Pending       int64   `json:"pending"`
	Active        int64   `json:"active"`
	UniqueSize    int64   `json:"uniqueSize"`
	UniquePartial int64   `json:"uniquePartial"`
	Hashed        int64   `json:"hashed"`
	Errors        int64   `json:"errors"`
	NeedProbe     int64   `json:"needProbe"`
	NeedPartial   int64   `json:"needPartial"`
	NeedFull      int64   `json:"needFull"`
	KnownBytes    int64   `json:"knownBytes"`
	SavedBytes    int64   `json:"savedBytes"`
	DupGroups     int64   `json:"dupGroups"`
	DupFiles      int64   `json:"dupFiles"`
	Redundant     int64   `json:"redundant"`
	Wasted        int64   `json:"wasted"`
	BytesRead     int64   `json:"bytesRead"`
	Rate          float64 `json:"rate"`
	FilesRate     float64 `json:"filesRate"`
	Paused        bool    `json:"paused"`
	Scanning      int32   `json:"scanning"`
	Uptime        int64   `json:"uptime"`
	StatsMs       int64   `json:"statsMs"`
	Mode          string  `json:"mode"`
}

// statsLoop: Zählt die Tabelle über den abdeckenden Index (kein Tabellenzugriff).
// Das Intervall passt sich an: bei riesigen Datenbanken seltener.
func (e *Engine) statsLoop(ctx context.Context) {
	var lastB, lastF int64
	lastT := time.Now()
	var rate, frate float64
	var base Stats
	nextDB := time.Time{}
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		now := time.Now()
		if now.After(nextDB) {
			t0 := time.Now()
			if s, err := e.queryCounts(); err == nil {
				base = s
			}
			d := time.Since(t0)
			base.StatsMs = d.Milliseconds()
			nextDB = time.Now().Add(max(time.Second, 3*d))
		}
		b, f := e.bytesRead.Load(), e.processed.Load()
		dt := now.Sub(lastT).Seconds()
		if dt > 0 {
			a := 0.3
			rate = a*float64(b-lastB)/dt + (1-a)*rate
			frate = a*float64(f-lastF)/dt + (1-a)*frate
		}
		lastB, lastF, lastT = b, f, now

		s := base
		s.BytesRead, s.Rate, s.FilesRate = b, rate, frate
		s.DupGroups, s.DupFiles, s.Wasted = e.dupGroups.Load(), e.dupFiles.Load(), e.wasted.Load()
		s.Redundant = s.DupFiles - s.DupGroups
		s.Paused = e.paused.Load()
		s.Scanning = e.scanning.Load()
		s.Uptime = int64(time.Since(e.start).Seconds())
		s.Mode = "gestaffelt"
		if e.cfg.Full {
			s.Mode = "voll"
		}
		if e.cfg.Store != "" {
			s.Mode += " + speichern"
		}
		e.statsMu.Lock()
		e.stats = s
		e.statsMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
	}
}

func (e *Engine) queryCounts() (Stats, error) {
	var s Stats
	rows, err := e.rdb.Query(`SELECT state, need, busy, count(*), coalesce(sum(CASE WHEN size>0 THEN size END),0)
		FROM files GROUP BY state, need, busy`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var st, need, busy int
		var c, sz int64
		rows.Scan(&st, &need, &busy, &c, &sz)
		s.Total += c
		s.KnownBytes += sz
		if busy == 1 {
			s.Active += c
		}
		switch {
		case st == stError:
			s.Errors += c
		case st == stFull:
			s.Hashed += c
		case st == stProbed && need == needNone:
			s.UniqueSize += c
			s.SavedBytes += sz
		case st == stPartial && need <= needPartial:
			s.UniquePartial += c
			s.SavedBytes += sz
		case st == stNew:
			s.NeedProbe += c
		case st == stProbed && need == needPartial:
			s.NeedPartial += c
		default:
			s.NeedFull += c
		}
	}
	s.Checked = s.Errors + s.Hashed + s.UniqueSize + s.UniquePartial
	s.Pending = s.Total - s.Checked
	return s, rows.Err()
}

func (e *Engine) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(uiFS, "ui")
	mux.Handle("GET /", http.FileServerFS(sub))

	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		e.statsMu.RLock()
		s := e.stats
		e.statsMu.RUnlock()
		writeJSON(w, s)
	})

	mux.HandleFunc("GET /api/groups", func(w http.ResponseWriter, r *http.Request) {
		limit := clamp(atoi(r.URL.Query().Get("limit"), 50), 1, 500)
		offset := max(atoi(r.URL.Query().Get("offset"), 0), 0)
		rows, err := e.rdb.Query(`SELECT fhash, cnt, size FROM hashes WHERE cnt>1
			ORDER BY (cnt-1)*size DESC LIMIT ? OFFSET ?`, limit, offset)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		type G struct {
			Hash  string   `json:"hash"`
			Count int64    `json:"count"`
			Size  int64    `json:"size"`
			Files []string `json:"files"`
		}
		var gs []G
		var raw [][]byte
		for rows.Next() {
			var g G
			var h []byte
			rows.Scan(&h, &g.Count, &g.Size)
			g.Hash = hex.EncodeToString(h)
			gs = append(gs, g)
			raw = append(raw, h)
		}
		rows.Close()
		for i := range gs {
			gs[i].Files = e.locs(raw[i], 4)
		}
		writeJSON(w, map[string]any{"groups": gs, "total": e.dupGroups.Load()})
	})

	mux.HandleFunc("GET /api/group", func(w http.ResponseWriter, r *http.Request) {
		h, err := hex.DecodeString(r.URL.Query().Get("h"))
		if err != nil {
			http.Error(w, "ungültiger Hash", 400)
			return
		}
		writeJSON(w, map[string]any{"files": e.locs(h, 10000)})
	})

	mux.HandleFunc("GET /api/errors", func(w http.ResponseWriter, r *http.Request) {
		rows, err := e.rdb.Query(`SELECT loc, coalesce(err,''), coalesce(done,0) FROM files WHERE state=9 ORDER BY done DESC LIMIT 200`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		type E struct {
			Loc  string `json:"loc"`
			Err  string `json:"err"`
			Time int64  `json:"time"`
		}
		out := []E{}
		for rows.Next() {
			var x E
			rows.Scan(&x.Loc, &x.Err, &x.Time)
			out = append(out, x)
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("GET /api/export.csv", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="duplikate.csv"`)
		rows, err := e.rdb.Query(`SELECT h.fhash, h.cnt, h.size, f.loc FROM hashes h
			JOIN files f ON f.fhash = h.fhash WHERE h.cnt > 1 ORDER BY h.fhash`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		cw := csv.NewWriter(w)
		cw.Write([]string{"sha256", "anzahl", "groesse_bytes", "ort"})
		for rows.Next() {
			var h []byte
			var c, s int64
			var loc string
			rows.Scan(&h, &c, &s, &loc)
			cw.Write([]string{hex.EncodeToString(h), strconv.FormatInt(c, 10), strconv.FormatInt(s, 10), loc})
		}
		cw.Flush()
	})

	mux.HandleFunc("POST /api/add", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 512<<20)
		n, err := e.Add(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]int{"added": n})
	})

	mux.HandleFunc("POST /api/scan", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Path string }
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Path == "" {
			http.Error(w, "Pfad fehlt", 400)
			return
		}
		if st, err := os.Stat(req.Path); err != nil || !st.IsDir() {
			http.Error(w, "Ordner nicht gefunden: "+req.Path, 400)
			return
		}
		go e.ScanDir(req.Path)
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /api/crawls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, e.CrawlList())
	})

	mux.HandleFunc("POST /api/crawl", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ URL, Exts, Base string }
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.URL == "" {
			http.Error(w, "URL fehlt", 400)
			return
		}
		if err := e.StartCrawl(e.ctx, req.URL, req.Exts, req.Base); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/crawl/stop", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ URL string }
		json.NewDecoder(r.Body).Decode(&req)
		writeJSON(w, map[string]bool{"ok": e.StopCrawl(req.URL)})
	})

	mux.HandleFunc("POST /api/pause", func(w http.ResponseWriter, r *http.Request) {
		e.paused.Store(r.URL.Query().Get("on") == "1")
		writeJSON(w, map[string]bool{"paused": e.paused.Load()})
	})

	if e.cfg.Auth == "" {
		return mux
	}
	user, pass, _ := strings.Cut(e.cfg.Auth, ":")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Web-Dupplet"`)
			http.Error(w, "Anmeldung erforderlich", 401)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (e *Engine) locs(h []byte, limit int) []string {
	rows, err := e.rdb.Query(`SELECT loc FROM files WHERE fhash=? LIMIT ?`, h, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }

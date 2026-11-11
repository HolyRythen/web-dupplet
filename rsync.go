package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// rsync-Quelle: "rsync --list-only -r" liefert in EINEM Datenstrom alle Dateien
// samt Größe – ohne eine einzige HTTP-Anfrage. Die Dateien selbst werden danach
// (nur falls nötig) per HTTP vom selben Mirror gelesen.
//
// Beispiel: rsync://ftpmirror.your.org/pub/misc/bitsavers/  ↔  http://ftpmirror.your.org/pub/misc/bitsavers/

var rsyncLine = regexp.MustCompile(`^([-dlcbps])[rwxsStT-]{9}\S*\s+([\d,.]+)\s+\S+\s+\S+\s+(.+)$`)

// defaultBase rät die HTTP-Adresse: rsync://host/modul/pfad → https://host/modul/pfad
func defaultBase(u *url.URL) string {
	p := u.Path
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return "https://" + u.Host + p
}

func (e *Engine) rsyncList(ctx context.Context, src *url.URL, base string, exts []string, st *CrawlStatus) error {
	b, err := url.Parse(base)
	if err != nil || (b.Scheme != "http" && b.Scheme != "https") {
		return fmt.Errorf("ungültige HTTP-Basis %q", base)
	}
	if !strings.HasSuffix(b.Path, "/") {
		b.Path += "/"
	}
	s := src.String()
	if !strings.HasSuffix(s, "/") {
		s += "/"
	}
	cmd := exec.CommandContext(ctx, "rsync", "--list-only", "-r", "--no-h", "--no-motd", "--timeout=120", s)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("rsync nicht startbar: %w", err)
	}
	isFile := func(p string) bool { return matchExt(exts, p) }
	batch := make([]AddItem, 0, 2000)
	flush := func() {
		if len(batch) > 0 {
			select {
			case e.addC <- batch:
			case <-ctx.Done():
			}
			batch = make([]AddItem, 0, 2000)
		}
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var lines int64
	for sc.Scan() {
		lines++
		m := rsyncLine.FindStringSubmatch(sc.Text())
		if m != nil && m[1] == "-" && isFile(m[3]) {
			size, err := strconv.ParseInt(strings.NewReplacer(",", "", ".", "").Replace(m[2]), 10, 64)
			if err != nil {
				size = -2
			}
			loc := b.ResolveReference(&url.URL{Path: m[3]}).String()
			batch = append(batch, AddItem{Loc: loc, Kind: kindHTTP, Size: size})
			e.crawls.mu.Lock()
			st.Found++
			e.crawls.mu.Unlock()
			if len(batch) == cap(batch) {
				flush()
			}
		}
		if lines%5000 == 0 {
			e.crawls.mu.Lock()
			st.Pages = lines
			e.crawls.mu.Unlock()
		}
	}
	flush()
	e.crawls.mu.Lock()
	st.Pages = lines
	e.crawls.mu.Unlock()
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		msg := strings.TrimSpace(stderr.String())
		log.Printf("rsync %s: %v %s", s, err, msg)
		if lines == 0 {
			return fmt.Errorf("rsync: %s", msg)
		}
	}
	return nil
}

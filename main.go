// Web-Dupplet – findet identische Dateien im Netz (und lokal), auch bei >100 TB.
//
// Prinzip (gestaffelt, spart Bandbreite):
//  1. Größe per HEAD-Request (fast 0 Byte Traffic). Einmalige Größe ⇒ kann kein Duplikat sein.
//  2. Gleiche Größe ⇒ nur Anfang + Ende laden (2×64 KiB per Range) und hashen.
//  3. Gleicher Teil-Hash ⇒ erst dann komplett laden und SHA-256 bilden.
//
// Nur Dateien mit identischem SHA-256 gelten als Übereinstimmung.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	cfg := &Config{}
	var urls, scans listFlag
	flag.StringVar(&cfg.DB, "db", "dupplet.db", "SQLite-Datenbank (Fortschritt bleibt bei Neustart erhalten)")
	flag.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "Adresse der Web-UI (z.B. 0.0.0.0:8080 für Zugriff im LAN)")
	flag.StringVar(&cfg.Auth, "auth", "", "Passwortschutz für die Web-UI: benutzer:passwort")
	flag.Var(&urls, "urls", "Datei mit URLs (eine pro Zeile, '-' = stdin); mehrfach möglich")
	var crawls listFlag
	flag.Var(&crawls, "crawl", "Webseite nach Dateien durchsuchen (Start-URL); mehrfach möglich")
	flag.StringVar(&cfg.Exts, "ext", "*", "Dateitypen für -crawl: * = alle, oder z.B. pdf,docx / word,bilder,videos,audio,office,archive")
	flag.Var(&scans, "scan", "lokalen Ordner rekursiv mit aufnehmen; mehrfach möglich")
	flag.StringVar(&cfg.Store, "store", "", "Downloads dauerhaft in diesem Ordner speichern (inhaltsadressiert, Duplikate nur 1x)")
	flag.BoolVar(&cfg.Full, "full", false, "jede Datei komplett hashen (kein Abkürzen über Größe/Teil-Hash)")
	flag.IntVar(&cfg.ProbeWorkers, "probe-workers", 64, "parallele Größen-Abfragen (HEAD)")
	flag.IntVar(&cfg.FetchWorkers, "fetch-workers", 128, "parallele Downloads")
	flag.IntVar(&cfg.LocalWorkers, "local-workers", 4, "parallele Leser für lokale Dateien (HDD: 1-2, NVMe: 8+)")
	flag.IntVar(&cfg.PerHost, "per-host", 16, "max. gleichzeitige Verbindungen pro Server")
	flag.IntVar(&cfg.ProbePerHost, "probe-per-host", 16, "max. gleichzeitige Größenabfragen (HEAD) pro Server")
	flag.IntVar(&cfg.Chunk, "chunk", 64<<10, "Bytes von Anfang/Ende für den Teil-Hash (pro Datenbank fest)")
	flag.IntVar(&cfg.Retries, "retries", 3, "Versuche bei vorübergehenden Fehlern")
	flag.DurationVar(&cfg.Stall, "timeout", 60*time.Second, "Abbruch, wenn so lange keine Daten kommen")
	flag.StringVar(&cfg.UserAgent, "user-agent", "Web-Dupplet/1.0", "HTTP User-Agent")
	pprofAddr := flag.String("pprof", "", "Diagnose-Schnittstelle, z.B. 127.0.0.1:6060 (nur für Fehlersuche)")
	flag.Parse()
	if *pprofAddr != "" {
		go http.ListenAndServe(*pprofAddr, nil) // net/http/pprof registriert sich am DefaultServeMux
	}
	cfg.URLFiles, cfg.ScanDirs, cfg.Crawls = urls, scans, crawls
	for _, u := range flag.Args() { // URLs/Listen auch als freie Argumente
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			cfg.URLFiles = append(cfg.URLFiles, "inline:"+u)
		} else if st, err := os.Stat(u); err == nil && st.IsDir() {
			cfg.ScanDirs = append(cfg.ScanDirs, u)
		} else {
			cfg.URLFiles = append(cfg.URLFiles, u)
		}
	}

	if cfg.Store != "" {
		if err := os.MkdirAll(filepath.Join(cfg.Store, "tmp"), 0o755); err != nil {
			log.Fatal(err)
		}
	}
	w, r, err := openDB(cfg.DB)
	if err != nil {
		log.Fatalf("Datenbank: %v", err)
	}
	if c, err := chunkSetting(w, cfg.Chunk); err != nil {
		log.Fatal(err)
	} else if c != cfg.Chunk {
		log.Printf("Hinweis: Datenbank nutzt -chunk %d – wird beibehalten", c)
		cfg.Chunk = c
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e := NewEngine(cfg, w, r)
	srv := &http.Server{Addr: cfg.Listen, Handler: e.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("Web-UI: http://%s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Web-UI: %v", err)
		}
	}()
	go logLoop(ctx, e)

	e.Run(ctx)
	sctx, c := context.WithTimeout(context.Background(), 2*time.Second)
	defer c()
	srv.Shutdown(sctx)
	w.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	w.Close()
	r.Close()
	log.Printf("Beendet – Fortschritt gespeichert in %s", cfg.DB)
}

func logLoop(ctx context.Context, e *Engine) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.statsMu.RLock()
			s := e.stats
			e.statsMu.RUnlock()
			log.Printf("geprüft %d/%d | Übereinstimmungen %d Dateien in %d Gruppen | %s/s | Fehler %d",
				s.Checked, s.Total, s.DupFiles, s.DupGroups, human(int64(s.Rate)), s.Errors)
		}
	}
}

func human(b int64) string {
	const u = "KMGTPE"
	if b < 1024 {
		return itoa(b) + " B"
	}
	f, i := float64(b), -1
	for f >= 1024 && i < len(u)-1 {
		f /= 1024
		i++
	}
	return strings.TrimRight(strings.TrimRight(ftoa(f), "0"), ".") + " " + string(u[i]) + "iB"
}

func itoa(n int64) string   { return strconv.FormatInt(n, 10) }
func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

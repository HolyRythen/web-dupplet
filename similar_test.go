package main

import (
	"archive/zip"
	"bytes"
	"context"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testOrdner legt Varianten an: Bild als PNG + kleines JPEG, dasselbe PNG zusätzlich in einem ZIP,
// Text als TXT + HTML + in einem ZIP, außerdem ein fremdes Bild und eine Datei ohne Analysetyp.
func testOrdner(t *testing.T) string {
	dir := t.TempDir()
	w := func(name string, b []byte) {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var pngB, jpgB, fremd bytes.Buffer
	orig := testBild(11, 900, 600)
	png.Encode(&pngB, orig)
	jpeg.Encode(&jpgB, verkleinere(orig, 300, 200), &jpeg.Options{Quality: 35})
	png.Encode(&fremd, testBild(99, 900, 600))
	langerText := strings.Repeat(beispielText+"\n", 8) // > minExact Bytes
	w("bilder/foto.png", pngB.Bytes())
	w("bilder/foto_klein.jpg", jpgB.Bytes())
	w("bilder/fremd.png", fremd.Bytes())
	w("text/brief.txt", []byte(langerText))
	w("text/brief.html", []byte("<html><body><p>"+strings.ReplaceAll(langerText, "\n", "</p><p>")+"</p></body></html>"))
	w("sonstiges/programm.bin", bytes.Repeat([]byte{1, 2, 3}, 5000))

	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, data := range map[string][]byte{"in/foto.png": pngB.Bytes(), "in/brief.txt": []byte(langerText)} {
		f, _ := zw.Create(name)
		f.Write(data)
	}
	zw.Close()
	w("paket.zip", zb.Bytes())
	return dir
}

func laufeAnalyse(t *testing.T, dir string) *Engine {
	t.Helper()
	return laufeAnalyseMit(t, []string{dir}, nil, 7, nil)
}

// laufeAnalyseMit startet das ganze Programm (Ordner und/oder URLs), wartet, bis alles analysiert ist,
// und beendet es wieder. Die Datenbank bleibt für Abfragen offen.
func laufeAnalyseMit(t *testing.T, dirs, urls []string, minAnalysiert int64, mod func(*Config)) *Engine {
	t.Helper()
	cfg := &Config{DB: filepath.Join(t.TempDir(), "t.db"), ProbeWorkers: 2, FetchWorkers: 2, LocalWorkers: 2,
		PerHost: 2, ProbePerHost: 2, Chunk: 64 << 10, Retries: 2, Stall: 10 * time.Second, UserAgent: "test",
		Similar: true, AnalyzeWorkers: 4, MediaWorkers: 2, SimMaxMB: 64, VideoFrames: 8, ScanDirs: dirs}
	for _, u := range urls {
		cfg.URLFiles = append(cfg.URLFiles, "inline:"+u)
	}
	if mod != nil {
		mod(cfg)
	}
	w, r, err := openDB(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(cfg, w, r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	t.Cleanup(func() { w.Close(); r.Close() })
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if e.an == nil {
			continue
		}
		s := e.an.stats(Stats{})
		if s.Analyzed >= minAnalysiert && s.Active == 0 {
			var offen int
			r.QueryRow(`SELECT count(*) FROM files f WHERE NOT EXISTS (SELECT 1 FROM analysis a WHERE a.file_id=f.id AND a.state<>8)`).Scan(&offen)
			if offen == 0 {
				break
			}
		}
	}
	cancel()
	<-done
	return e
}

func TestAehnlichkeitsanalyseEndeZuEnde(t *testing.T) {
	dir := testOrdner(t)
	for lauf := 1; lauf <= 4; lauf++ { // mehrfach: Ergebnis darf nicht von der Reihenfolge abhängen
		e := laufeAnalyse(t, dir)
		r := e.rdb
		paare := map[string]int{}
		rows, err := r.Query(`SELECT p.kind, fa.loc, a.sub, fb.loc, b.sub FROM pairs p
			JOIN fps a ON a.id=p.a JOIN fps b ON b.id=p.b JOIN files fa ON fa.id=a.file_id JOIN files fb ON fb.id=b.file_id`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var k int
			var la, sa, lb, sb string
			rows.Scan(&k, &la, &sa, &lb, &sb)
			x, y := filepath.Base(la)+"›"+sa, filepath.Base(lb)+"›"+sb
			if y < x {
				x, y = y, x
			}
			paare[strings.Join([]string{mtName[map[int]int{1: mtImage, 2: mtText, 3: mtVideo, 4: mtArchive}[k]], x, y}, " | ")]++
		}
		rows.Close()
		erwartet := []string{
			"Bild | foto.png› | foto_klein.jpg›",
			"Bild | foto_klein.jpg› | paket.zip›in/foto.png",
			"Archiv | foto.png› | paket.zip›in/foto.png",
			"Text | brief.html› | brief.txt›",
			"Text | brief.html› | paket.zip›in/brief.txt",
			"Archiv | brief.txt› | paket.zip›in/brief.txt",
		}
		for _, p := range erwartet {
			if paare[p] != 1 {
				t.Errorf("Lauf %d: Paar %q %d-mal gefunden, erwartet genau 1-mal", lauf, p, paare[p])
			}
		}
		if len(paare) != len(erwartet) {
			t.Errorf("Lauf %d: %d verschiedene Paare, erwartet %d: %v", lauf, len(paare), len(erwartet), paare)
		}
		var ganz int
		r.QueryRow(`SELECT count(*) FROM (SELECT file_id FROM fps WHERE kind=5 GROUP BY file_id HAVING count(*)>1)`).Scan(&ganz)
		if ganz > 0 {
			t.Errorf("Lauf %d: %d Dateien mit mehrfachem exakten Fingerabdruck", lauf, ganz)
		}
		var skip int
		r.QueryRow(`SELECT count(*) FROM analysis a JOIN files f ON f.id=a.file_id WHERE f.loc LIKE '%programm.bin' AND a.state=?`, anSkip).Scan(&skip)
		if skip != 1 {
			t.Errorf("Lauf %d: Datei ohne Analysetyp nicht als übersprungen markiert", lauf)
		}
		if t.Failed() {
			return
		}
	}
}

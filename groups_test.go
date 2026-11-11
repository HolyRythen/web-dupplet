package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func schreibe(t *testing.T, p string, b []byte) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func zipDatei(t *testing.T, files map[string][]byte) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for n, c := range files {
		w, _ := zw.Create(n)
		w.Write(c)
	}
	zw.Close()
	return b.Bytes()
}

// Zwei verschiedene Archive mit gemeinsamen Einträgen → genau EINE Verwandtschaft, keine Einzelpaare.
// Eine byte-gleiche Kopie eines Archivs → keine zusätzliche Verwandtschaft.
func TestArchivVerwandtschaft(t *testing.T) {
	dir := t.TempDir()
	gemeinsam := map[string][]byte{}
	for i, n := range []string{"a.bin", "b.bin", "c.bin"} {
		gemeinsam["src/"+n] = bytes.Repeat([]byte{byte(i + 1), 7, 9}, 3000)
	}
	v1 := map[string][]byte{"neu-in-v1.bin": bytes.Repeat([]byte{42}, 9000)}
	v2 := map[string][]byte{"neu-in-v2.bin": bytes.Repeat([]byte{43}, 9000)}
	for k, v := range gemeinsam {
		v1[k], v2[k] = v, v
	}
	z1 := zipDatei(t, v1)
	schreibe(t, filepath.Join(dir, "paket-1.0.zip"), z1)
	schreibe(t, filepath.Join(dir, "paket-2.0.zip"), zipDatei(t, v2))
	schreibe(t, filepath.Join(dir, "spiegel/paket-1.0.zip"), z1) // byte-gleiche Kopie
	e := laufeAnalyseMit(t, []string{dir}, nil, 3, nil)
	var rels, shared, einzel int
	e.rdb.QueryRow(`SELECT count(*), coalesce(max(shared),0) FROM arcrel`).Scan(&rels, &shared)
	e.rdb.QueryRow(`SELECT count(*) FROM pairs p JOIN fps a ON a.id=p.a JOIN fps b ON b.id=p.b WHERE a.kind=4 AND b.kind=4`).Scan(&einzel)
	// nur paket-1.0 ↔ paket-2.0: die byte-gleiche Kopie wird gar nicht analysiert (steht bei den Duplikaten)
	if rels != 1 || shared != 3 {
		t.Errorf("Verwandtschaften %d (erwartet 1), gemeinsam %d (erwartet 3)", rels, shared)
	}
	var kopie int
	e.rdb.QueryRow(`SELECT count(*) FROM analysis WHERE state=?`, anDup).Scan(&kopie)
	if kopie != 1 {
		t.Errorf("%d Archive als byte-gleiche Kopie übersprungen, erwartet 1", kopie)
	}
	if einzel != 0 {
		t.Errorf("%d Einzelpaare zwischen Archiv-Einträgen, erwartet 0", einzel)
	}
}

// Gruppe aus drei Varianten eines Bildes: größte Auflösung wird behalten, Benutzerwahl gewinnt,
// Aufräum-Skript verschiebt nur lokale Dateien und ähnliche nur auskommentiert.
func TestGruppenBehaltenUndAufraeumen(t *testing.T) {
	dir := t.TempDir()
	orig := testBild(21, 1200, 800)
	var gross, mittel, klein, dup bytes.Buffer
	png.Encode(&gross, orig)
	jpeg.Encode(&mittel, verkleinere(orig, 600, 400), &jpeg.Options{Quality: 80})
	jpeg.Encode(&klein, verkleinere(orig, 300, 200), &jpeg.Options{Quality: 50})
	png.Encode(&dup, testBild(77, 400, 300))
	schreibe(t, filepath.Join(dir, "gross.png"), gross.Bytes())
	schreibe(t, filepath.Join(dir, "mittel.jpg"), mittel.Bytes())
	schreibe(t, filepath.Join(dir, "klein.jpg"), klein.Bytes())
	schreibe(t, filepath.Join(dir, "a/doppelt.png"), dup.Bytes()) // byte-gleich → „sicher“
	schreibe(t, filepath.Join(dir, "b/doppelt.png"), dup.Bytes())
	e := laufeAnalyseMit(t, []string{dir}, nil, 5, nil)
	g, err := e.an.buildGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(g.groups) != 1 || len(g.groups[0].Nodes) != 3 {
		t.Fatalf("erwartet 1 Gruppe mit 3 Bildern, bekommen %+v", g.groups)
	}
	gr := g.groups[0]
	if gr.Kind != pkImage || !strings.HasSuffix(gr.Nodes[0].Loc, "gross.png") || !gr.Nodes[0].Keep {
		t.Errorf("behalten sollte gross.png sein: %+v", gr.Nodes[0])
	}
	if want := int64(mittel.Len() + klein.Len()); gr.Savings != want {
		t.Errorf("Ersparnis %d, erwartet %d", gr.Savings, want)
	}
	// Benutzer wählt mittel.jpg (Programm ist schon beendet → Schreibverbindung neu öffnen)
	if e.an.db, err = sql.Open("sqlite", dsn(e.cfg.DB, false)); err != nil {
		t.Fatal(err)
	}
	defer e.an.db.Close()
	e.an.grp.mu.Lock()
	e.an.grp.g = g
	e.an.grp.mu.Unlock()
	var mid GNode
	for _, n := range gr.Nodes {
		if strings.HasSuffix(n.Loc, "mittel.jpg") {
			mid = n
		}
	}
	if err := e.an.SetKeep(mid.File, ""); err != nil {
		t.Fatal(err)
	}
	gr = e.an.grp.get().groups[0]
	if !strings.HasSuffix(gr.Nodes[0].Loc, "mittel.jpg") || !gr.Nodes[0].Manual {
		t.Errorf("nach Wahl sollte mittel.jpg behalten werden: %+v", gr.Nodes[0])
	}
	var sh bytes.Buffer
	if err := e.writeCleanupScript(context.Background(), &sh); err != nil {
		t.Fatal(err)
	}
	s := sh.String()
	aktiv := strings.Count(s, "\nweg ")
	auskommentiert := strings.Count(s, "\n# weg ")
	if aktiv != 1 || !strings.Contains(s, "doppelt.png") {
		t.Errorf("erwartet genau 1 aktive Zeile für das byte-gleiche Duplikat, %d:\n%s", aktiv, s)
	}
	if auskommentiert != 2 || !strings.Contains(s, "gross.png") || !strings.Contains(s, "klein.jpg") {
		t.Errorf("erwartet 2 auskommentierte Zeilen (gross, klein), %d:\n%s", auskommentiert, s)
	}
	var csvB bytes.Buffer
	e.writeCleanupCSV(context.Background(), &csvB)
	if strings.Count(csvB.String(), "\n") != 1+2+3 { // Kopf + 2 byte-gleich + 3 ähnlich
		t.Errorf("CSV hat unerwartet viele Zeilen:\n%s", csvB.String())
	}
}

// Datei ohne Endung wird am Inhalt erkannt; vorübergehender Serverfehler (503) wird wiederholt.
func TestInhaltserkennungUndWiederholung(t *testing.T) {
	old := retryDelay
	retryDelay = []time.Duration{0, 0, 0}
	defer func() { retryDelay = old }()
	var b1, b2 bytes.Buffer
	orig := testBild(5, 800, 600)
	png.Encode(&b1, orig)
	jpeg.Encode(&b2, verkleinere(orig, 400, 300), &jpeg.Options{Quality: 70})
	var fehler atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bild-ohne-endung":
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b1.Bytes()))
		case "/foto.jpg":
			// vollständige Downloads (ohne Range) schlagen zweimal mit 503 fehl
			if r.Method == http.MethodGet && r.Header.Get("Range") == "" && fehler.Add(1) <= 2 {
				http.Error(w, "überlastet", 503)
				return
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b2.Bytes()))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	e := laufeAnalyseMit(t, nil, []string{srv.URL + "/bild-ohne-endung", srv.URL + "/foto.jpg"}, 2, nil)
	var mt, st int
	e.rdb.QueryRow(`SELECT a.mt, a.state FROM analysis a JOIN files f ON f.id=a.file_id WHERE f.loc LIKE '%ohne-endung'`).Scan(&mt, &st)
	if mt != mtImage || st != anDone {
		t.Errorf("Datei ohne Endung: Typ %d Zustand %d, erwartet Bild/fertig", mt, st)
	}
	var tries int
	e.rdb.QueryRow(`SELECT a.state, a.tries FROM analysis a JOIN files f ON f.id=a.file_id WHERE f.loc LIKE '%foto.jpg'`).Scan(&st, &tries)
	if st != anDone || tries != 2 {
		t.Errorf("nach 503: Zustand %d, Versuche %d – erwartet fertig nach 2 Wiederholungen", st, tries)
	}
	var paare int
	e.rdb.QueryRow(`SELECT count(*) FROM pairs WHERE kind=1`).Scan(&paare)
	if paare != 1 {
		t.Errorf("%d Bild-Paare, erwartet 1", paare)
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(10) // 10 MB/s, Vorrat 2,5 MB
	t0 := time.Now()
	for i := 0; i < 50; i++ {
		l.take(100_000) // 5 MB
	}
	if d := time.Since(t0); d < 200*time.Millisecond || d > 600*time.Millisecond {
		t.Errorf("5 MB bei 10 MB/s (2,5 MB Vorrat) dauerten %s, erwartet ≈ 250 ms", d)
	}
	if newRateLimiter(0) != nil {
		t.Error("0 = unbegrenzt")
	}
}

// Byte-gleiche Kopien werden nicht noch einmal analysiert (gleicher Fingerabdruck) – Ergebnis bleibt gleich.
func TestByteGleicheKopieWirdNichtAnalysiert(t *testing.T) {
	dir := t.TempDir()
	orig := testBild(31, 900, 600)
	var p, j bytes.Buffer
	png.Encode(&p, orig)
	jpeg.Encode(&j, verkleinere(orig, 450, 300), &jpeg.Options{Quality: 60})
	for _, d := range []string{"a", "b", "c"} {
		schreibe(t, filepath.Join(dir, d, "foto.png"), p.Bytes())
	}
	schreibe(t, filepath.Join(dir, "foto_klein.jpg"), j.Bytes())
	e := laufeAnalyseMit(t, []string{dir}, nil, 4, nil)
	var fertig, kopie, paare int
	e.rdb.QueryRow(`SELECT count(*) FROM analysis WHERE state=?`, anDone).Scan(&fertig)
	e.rdb.QueryRow(`SELECT count(*) FROM analysis WHERE state=?`, anDup).Scan(&kopie)
	e.rdb.QueryRow(`SELECT count(*) FROM pairs`).Scan(&paare)
	if fertig != 2 || kopie != 2 || paare != 1 {
		t.Errorf("analysiert %d (erwartet 2), als Kopie übersprungen %d (erwartet 2), Paare %d (erwartet 1)", fertig, kopie, paare)
	}
}

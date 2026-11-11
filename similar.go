package main

// Stufe 4: Ähnlichkeitsanalyse (Option -similar).
//
// Läuft neben den Stufen 1–3 her und nimmt sich jede Datei mit bekannter Größe einmal vor:
//   - Bilder, Videos, Audio, Texte, Office-Dokumente, PDFs und Archive bekommen Fingerabdrücke,
//   - jeder neue Fingerabdruck wird sofort mit allen bisherigen verglichen,
//   - gefundene Paare landen in der Tabelle "pairs" (mit Ähnlichkeit 0–1), daraus werden Gruppen (groups.go).
//
// Archive: Jede enthaltene Datei bekommt einen SHA-256. Liegt dieselbe Datei auch einzeln vor, entsteht ein
// Paar „exakt gleich, einmal verpackt“. Zwei Archive mit gemeinsamen Dateien ergeben EINE Zeile in "arcrel"
// („teilen 290 Dateien“) statt hunderter Einzelpaare; byte-gleiche Archive stehen schon in den Duplikat-Gruppen.
//
// Videos und Audio laufen in einem eigenen Worker-Pool (langsam, netzlastig), damit Bilder/Texte nicht warten.
// Vorübergehende Fehler (429, 5xx, Zeitüberschreitung, Netzwerk) werden nach 5 min, 30 min und 3 h wiederholt.
//
// Die Analyse hat eine eigene Datenbankverbindung – der Kern (Stufen 1–3) wird nicht ausgebremst.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Arten von Fingerabdrücken (fps.kind)
const (
	fpImage  = 1
	fpText   = 2
	fpVideo  = 3
	fpMember = 4 // exakter SHA-256 einer Datei in einem Archiv
	fpWhole  = 5 // exakter SHA-256 einer einzelnen Datei (nur als Gegenstück zu fpMember)
	fpAudio  = 6
)

// Arten von Paaren (pairs.kind)
const (
	pkImage   = 1
	pkText    = 2
	pkVideo   = 3
	pkArchive = 4 // Archiv-Eintrag ↔ einzelne Datei, exakt gleich
	pkAudio   = 5
	pkKinds   = 6
)

// Analyse-Zustände (analysis.state)
const (
	anDone   = 1 // Fingerabdruck erstellt
	anSkip   = 2 // kein analysierbarer Dateityp
	anTooBig = 3 // größer als -sim-max
	anNoFP   = 4 // analysiert, aber nichts Vergleichbares (einfarbiges Bild, sehr kurzer Text …)
	anDup    = 5 // byte-gleiche Kopie einer schon analysierten Datei: gleicher Fingerabdruck, nicht nötig
	anRetry  = 8 // vorübergehender Fehler, wird später wiederholt
	anFail   = 9
)

const (
	minExact    = 4096     // kleinere Archiv-Einträge werden nicht exakt abgeglichen (README, LICENSE …)
	memBufMax   = 64 << 20 // Archiv-Einträge bis zu dieser Größe werden auch inhaltlich analysiert
	maxPairsNew = 25       // höchstens so viele Paare pro neuem Fingerabdruck
	genericHits = 100      // mehr Treffer = zu allgemeiner Inhalt (leere Seite, Standardmuster) → keine Paare
	pdfMaxImgs  = 60
	minImgSide  = 48 // kleinere Bilder (Symbole, Linien) aus PDFs/Office ignorieren
	maxRetries  = 3
)

var retryDelay = []time.Duration{5 * time.Minute, 30 * time.Minute, 3 * time.Hour}

const simSchema = `
CREATE TABLE IF NOT EXISTS analysis(
  file_id  INTEGER PRIMARY KEY,
  state    INTEGER NOT NULL,
  mt       INTEGER NOT NULL DEFAULT 0,
  err      TEXT,
  done     INTEGER
);
CREATE TABLE IF NOT EXISTS fps(
  id      INTEGER PRIMARY KEY,
  file_id INTEGER NOT NULL,
  sub     TEXT    NOT NULL DEFAULT '',  -- Pfad im Archiv bzw. "PDF-Bild 3"
  kind    INTEGER NOT NULL,
  h1      INTEGER,                      -- pHash / SimHash / SHA-256-Anfang
  h2      INTEGER,                      -- dHash
  size    INTEGER NOT NULL DEFAULT -1,
  w       INTEGER, h INTEGER,
  dur     REAL,
  words   INTEGER,
  snip    TEXT,
  extra   BLOB,                         -- Video/Audio: Ausschnitt-Hashes; exakt: voller SHA-256
  cs      INTEGER,                      -- Anfang des SHA-256 der analysierten Bytes
  ex      INTEGER NOT NULL DEFAULT 0    -- 1 = Byte-Gleichheit wird schon anderswo gemeldet
);
CREATE INDEX IF NOT EXISTS fps_file  ON fps(file_id, kind);
CREATE INDEX IF NOT EXISTS fps_exact ON fps(h1) WHERE kind IN (4,5);
CREATE INDEX IF NOT EXISTS fps_msize ON fps(size) WHERE kind = 4;
CREATE TABLE IF NOT EXISTS thumbs(fp INTEGER PRIMARY KEY, jpg BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS pairs(
  id    INTEGER PRIMARY KEY,
  kind  INTEGER NOT NULL,
  a     INTEGER NOT NULL,
  b     INTEGER NOT NULL,
  score REAL    NOT NULL,
  found INTEGER NOT NULL,
  UNIQUE(a, b)
);
CREATE INDEX IF NOT EXISTS pairs_score ON pairs(score DESC, id DESC);
CREATE INDEX IF NOT EXISTS pairs_kind  ON pairs(kind, score DESC, id DESC);
-- Archive: Zahl der (exakt verglichenen) Einträge und SHA-256 des Archivs selbst
CREATE TABLE IF NOT EXISTS arcinfo(file_id INTEGER PRIMARY KEY, members INTEGER NOT NULL, bytes INTEGER NOT NULL, sha BLOB);
-- Zwei verschiedene Archive mit gemeinsamen Einträgen (a < b)
CREATE TABLE IF NOT EXISTS arcrel(
  a INTEGER NOT NULL, b INTEGER NOT NULL,
  shared INTEGER NOT NULL, shared_bytes INTEGER NOT NULL, found INTEGER NOT NULL,
  PRIMARY KEY(a, b)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS arcrel_shared ON arcrel(shared_bytes DESC);
-- vom Benutzer gewählte „behalten“-Exemplare (groups.go)
CREATE TABLE IF NOT EXISTS keep(file_id INTEGER NOT NULL, sub TEXT NOT NULL DEFAULT '', PRIMARY KEY(file_id, sub)) WITHOUT ROWID;
`

type anJob struct {
	ID        int64
	Loc       string
	Kind      int // kindHTTP / kindLocal
	Size      int64
	FHash     []byte
	MT        int
	Ext       string // Endung aus der Inhaltserkennung (sonst aus dem Pfad)
	Sniff     bool   // Typ unbekannt → am Inhalt erkennen
	NeedWhole bool   // eine Archiv-Datei hat dieselbe Größe → exakten SHA-256 bilden
}

// fpItem ist ein neuer Fingerabdruck (noch ohne Datenbank-ID).
type fpItem struct {
	File   int64 // files.id (normalerweise die analysierte Datei)
	Sub    string
	Kind   int
	H1, H2 uint64
	Size   int64
	W, H   int
	Dur    float64
	Words  int
	Snip   string
	Extra  []byte
	Thumb  []byte
	CS     uint64 // Anfang des SHA-256 der Bytes
	Ex     bool   // ganze Datei oder Archiv-Eintrag ≥ minExact: Byte-Gleichheit steht schon anderswo
}

type anResult struct {
	State     int
	MT        int
	Err       string
	Transient bool
	Items     []fpItem
	Arc       *arcSummary // nur bei Archiven
}

type arcSummary struct {
	members int
	bytes   int64
	sha     []byte
}

type seqInfo struct {
	file int64
	segs []uint64
	dur  float64
}

// errTransient: vorübergehender Fehler (Netz, Server überlastet) → später erneut versuchen.
type errTransient struct{ error }

func (e errTransient) Unwrap() error { return e.error }

func transient(err error) error {
	if err == nil || errors.As(err, new(errPermanent)) {
		return err
	}
	return errTransient{err}
}

func isTransient(err error) bool { return errors.As(err, new(errTransient)) }

type Analyzer struct {
	e            *Engine
	db           *sql.DB
	t            tools
	workers      int
	mediaWorkers int
	maxBytes     int64
	frames       int

	jobs, mediaJobs chan anJob
	inflight        sync.Map
	ranges          sync.Map // Host → unterstützt Range-Anfragen

	mu          sync.Mutex // Index + Schreiben gehören zusammen
	img, txt    mih
	vid, aud    mih
	vidMeta     map[int64]seqInfo
	audMeta     map[int64]seqInfo
	txtMin      map[int64][]uint32    // MinHash je Text-Fingerabdruck
	cnt         [10]atomic.Int64      // Zähler je analysis.state
	pairs       [pkKinds]atomic.Int64 // Zähler je pairs.kind
	arcRels     atomic.Int64
	active      atomic.Int32
	activeMedia atomic.Int32
	hasMembers  atomic.Bool
	wg          sync.WaitGroup
	changed     atomic.Int64 // erhöht sich bei jeder Änderung an Paaren/„behalten“ (für groups.go)
	grp         groupCache
	retryNow    chan struct{}
}

func newAnalyzer(e *Engine) (*Analyzer, error) {
	db, err := sql.Open("sqlite", dsn(e.cfg.DB, false))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(simSchema); err != nil {
		return nil, fmt.Errorf("Schema Ähnlichkeit: %w", err)
	}
	// Migrationen (Fehler = Spalte existiert schon)
	db.Exec(`ALTER TABLE analysis ADD COLUMN tries INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE analysis ADD COLUMN retry_at INTEGER`)
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS analysis_retry ON analysis(retry_at) WHERE state = 8`); err != nil {
		return nil, err
	}
	a := &Analyzer{
		e: e, db: db, t: findTools(),
		workers:      max(1, e.cfg.AnalyzeWorkers),
		mediaWorkers: max(1, e.cfg.MediaWorkers),
		maxBytes:     int64(e.cfg.SimMaxMB) << 20,
		frames:       max(3, e.cfg.VideoFrames),
		vidMeta:      map[int64]seqInfo{},
		audMeta:      map[int64]seqInfo{},
		txtMin:       map[int64][]uint32{},
		retryNow:     make(chan struct{}, 1),
	}
	a.jobs = make(chan anJob, a.workers*4)
	a.mediaJobs = make(chan anJob, 4096) // groß: Videos stauen sich, ohne den Verteiler zu blockieren
	if a.t.ffmpeg == "" {
		log.Printf("Ähnlichkeit: ffmpeg fehlt – Videos und Audio werden übersprungen")
	}
	if a.t.pdftotext == "" {
		log.Printf("Ähnlichkeit: pdftotext/pdfimages fehlen (poppler-utils) – PDFs werden übersprungen")
	}
	if err := a.migrateArchives(); err != nil {
		return nil, fmt.Errorf("Migration Archive: %w", err)
	}
	if err := a.migrateTextMinHash(); err != nil {
		return nil, fmt.Errorf("Migration Texte: %w", err)
	}
	return a, a.load()
}

// migrateArchives (einmalig): Einzelpaare zwischen Archiv-Einträgen → eine Zeile pro Archiv-Paar.
func (a *Analyzer) migrateArchives() error {
	var v string
	if a.db.QueryRow(`SELECT v FROM meta WHERE k='sim_arcrel'`).Scan(&v) == nil {
		return nil
	}
	t0 := time.Now()
	type mem struct {
		file int64
		size int64
	}
	byHash := map[string][]mem{}
	rows, err := a.db.Query(`SELECT file_id, size, extra FROM fps WHERE kind=4`)
	if err != nil {
		return err
	}
	perFile := map[int64]*arcSummary{}
	for rows.Next() {
		var m mem
		var ex []byte
		rows.Scan(&m.file, &m.size, &ex)
		byHash[string(ex)] = append(byHash[string(ex)], m)
		s := perFile[m.file]
		if s == nil {
			s = &arcSummary{}
			perFile[m.file] = s
		}
		s.members++
		s.bytes += m.size
	}
	rows.Close()
	type rel struct{ shared, bytes int64 }
	rels := map[[2]int64]*rel{}
	for _, ms := range byHash {
		files := map[int64]int64{}
		for _, m := range ms {
			files[m.file] = m.size
		}
		ids := make([]int64, 0, len(files))
		for f := range files {
			ids = append(ids, f)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		if len(ids) > 200 { // extrem häufiger Eintrag (z.B. leere Vorlage): keine Aussage über Verwandtschaft
			continue
		}
		for i := range ids {
			for j := i + 1; j < len(ids); j++ {
				k := [2]int64{ids[i], ids[j]}
				r := rels[k]
				if r == nil {
					r = &rel{}
					rels[k] = r
				}
				r.shared++
				r.bytes += files[ids[i]]
			}
		}
	}
	fh := map[int64][]byte{}
	if fr, err := a.db.Query(`SELECT f.id, f.fhash FROM files f WHERE f.fhash IS NOT NULL AND f.id IN (SELECT DISTINCT file_id FROM fps WHERE kind=4)`); err == nil {
		for fr.Next() {
			var id int64
			var h []byte
			fr.Scan(&id, &h)
			fh[id] = h
		}
		fr.Close()
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for f, s := range perFile {
		tx.Exec(`INSERT OR IGNORE INTO arcinfo(file_id, members, bytes, sha) VALUES(?,?,?,?)`, f, s.members, s.bytes, fh[f])
	}
	n := 0
	for k, r := range rels {
		if ha, hb := fh[k[0]], fh[k[1]]; ha != nil && string(ha) == string(hb) {
			continue // byte-gleiche Archive: stehen in den Duplikat-Gruppen
		}
		tx.Exec(`INSERT OR REPLACE INTO arcrel(a, b, shared, shared_bytes, found) VALUES(?,?,?,?,?)`, k[0], k[1], r.shared, r.bytes, now)
		n++
	}
	res, err := tx.Exec(`DELETE FROM pairs WHERE kind=4 AND a IN (SELECT id FROM fps WHERE kind=4) AND b IN (SELECT id FROM fps WHERE kind=4)`)
	if err != nil {
		return err
	}
	del, _ := res.RowsAffected()
	tx.Exec(`INSERT OR REPLACE INTO meta(k, v) VALUES('sim_arcrel', '1')`)
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(perFile) > 0 {
		log.Printf("Ähnlichkeit: Archive umgestellt – %d Einzelpaare entfernt, %d Archiv-Verwandtschaften (%s)",
			del, n, time.Since(t0).Round(time.Millisecond))
	}
	return nil
}

// migrateTextMinHash (einmalig): Text-Fingerabdrücke ohne MinHash (ältere Version) lassen sich nicht bestätigen.
// Die betroffenen Dateien werden komplett neu analysiert; ihre Paare, Vorschauen und Archiv-Daten entfallen bis dahin.
func (a *Analyzer) migrateTextMinHash() error {
	var v string
	if a.db.QueryRow(`SELECT v FROM meta WHERE k='sim_minhash'`).Scan(&v) == nil {
		return nil
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	steps := []string{
		`CREATE TEMP TABLE redo AS SELECT DISTINCT file_id FROM fps WHERE kind=2 AND extra IS NULL`,
		`CREATE TEMP TABLE redofp AS SELECT id FROM fps WHERE file_id IN (SELECT file_id FROM redo)`,
		`DELETE FROM pairs WHERE a IN (SELECT id FROM redofp) OR b IN (SELECT id FROM redofp)`,
		`DELETE FROM thumbs WHERE fp IN (SELECT id FROM redofp)`,
		`DELETE FROM fps WHERE id IN (SELECT id FROM redofp)`,
		`DELETE FROM arcrel WHERE a IN (SELECT file_id FROM redo) OR b IN (SELECT file_id FROM redo)`,
		`DELETE FROM arcinfo WHERE file_id IN (SELECT file_id FROM redo)`,
		`DELETE FROM analysis WHERE file_id IN (SELECT file_id FROM redo)`,
	}
	for _, q := range steps {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", q[:40], err)
		}
	}
	var n int64
	tx.QueryRow(`SELECT count(*) FROM redo`).Scan(&n)
	tx.Exec(`DROP TABLE redo`)
	tx.Exec(`DROP TABLE redofp`)
	tx.Exec(`INSERT OR REPLACE INTO meta(k, v) VALUES('sim_minhash', '1')`)
	if err := tx.Commit(); err != nil {
		return err
	}
	if n > 0 {
		log.Printf("Ähnlichkeit: Textvergleich verbessert (MinHash) – %d Dateien werden neu analysiert", n)
	}
	return nil
}

// load baut die Such-Indizes und Zähler aus der Datenbank auf (nach Neustart).
func (a *Analyzer) load() error {
	t0 := time.Now()
	rows, err := a.db.Query(`SELECT id, file_id, kind, h1, coalesce(h2,0), coalesce(dur,0), extra, coalesce(cs,0), ex FROM fps WHERE kind IN (1,2,3,6)`)
	if err != nil {
		return err
	}
	n := 0
	for rows.Next() {
		var id, file, h1, h2 int64
		var kind int
		var dur float64
		var extra []byte
		var cs int64
		var ex bool
		if err := rows.Scan(&id, &file, &kind, &h1, &h2, &dur, &extra, &cs, &ex); err != nil {
			rows.Close()
			return err
		}
		a.index(id, fpItem{File: file, Kind: kind, H1: uint64(h1), H2: uint64(h2), Dur: dur, Extra: extra, CS: uint64(cs), Ex: ex})
		n++
	}
	rows.Close()
	count := func(q string, into func(k int, c int64)) error {
		r, err := a.db.Query(q)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var k int
			var c int64
			r.Scan(&k, &c)
			into(k, c)
		}
		return nil
	}
	if err := count(`SELECT state, count(*) FROM analysis GROUP BY state`, func(k int, c int64) {
		if k >= 0 && k < len(a.cnt) {
			a.cnt[k].Store(c)
		}
	}); err != nil {
		return err
	}
	if err := count(`SELECT kind, count(*) FROM pairs GROUP BY kind`, func(k int, c int64) {
		if k > 0 && k < pkKinds {
			a.pairs[k].Store(c)
		}
	}); err != nil {
		return err
	}
	var c int64
	a.db.QueryRow(`SELECT count(*) FROM arcrel`).Scan(&c)
	a.arcRels.Store(c)
	var one int
	if a.db.QueryRow(`SELECT 1 FROM fps WHERE kind=4 LIMIT 1`).Scan(&one) == nil {
		a.hasMembers.Store(true)
	}
	if n > 0 {
		log.Printf("Ähnlichkeit: %d Fingerabdrücke geladen (%s)", n, time.Since(t0).Round(time.Millisecond))
	}
	return nil
}

func (a *Analyzer) index(id int64, it fpItem) {
	switch it.Kind {
	case fpImage:
		a.img.add(mihEntry{fp: id, file: it.File, h: it.H1, aux: it.H2, cs: it.CS, ex: it.Ex})
	case fpText:
		a.txt.add(mihEntry{fp: id, file: it.File, h: it.H1, cs: it.CS, ex: it.Ex})
		if m := blobMin(it.Extra); m != nil {
			a.txtMin[id] = m
		}
	case fpVideo, fpAudio:
		segs := blobFrames(it.Extra)
		m, meta := &a.vid, a.vidMeta
		if it.Kind == fpAudio {
			m, meta = &a.aud, a.audMeta
		}
		meta[id] = seqInfo{file: it.File, segs: segs, dur: it.Dur}
		for i, h := range segs {
			if h != 0 {
				m.add(mihEntry{fp: id, file: it.File, h: h, aux: uint64(i)})
			}
		}
	}
}

// sameBytes: Inhalt ist Byte für Byte gleich und wird schon als Duplikat bzw. Archiv-Treffer gemeldet.
func sameBytes(x mihEntry, it fpItem) bool {
	return x.cs != 0 && x.cs == it.CS && x.ex && it.Ex
}

func shaPrefix(b []byte) uint64 {
	h := sha256.Sum256(b)
	return binary.BigEndian.Uint64(h[:])
}

func (a *Analyzer) run(ctx context.Context) {
	spawn := func(n int, q chan anJob, act *atomic.Int32) {
		for i := 0; i < n; i++ {
			a.wg.Add(1)
			go func() {
				defer a.wg.Done()
				for j := range q {
					a.active.Add(1)
					act.Add(1)
					res := a.analyze(ctx, j)
					if ctx.Err() == nil || res.State != anFail {
						if err := a.commit(j, res); err != nil {
							log.Printf("Ähnlichkeit speichern (%s): %v", j.Loc, err)
						}
					}
					a.inflight.Delete(j.ID)
					act.Add(-1)
					a.active.Add(-1)
				}
			}()
		}
	}
	var general atomic.Int32
	spawn(a.workers, a.jobs, &general)
	spawn(a.mediaWorkers, a.mediaJobs, &a.activeMedia)
	go a.groupLoop(ctx)
	a.dispatch(ctx)
	close(a.jobs)
	close(a.mediaJobs)
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		log.Printf("Ähnlichkeit: Analysen beim Beenden abgebrochen (werden beim Neustart wiederholt)")
	}
	a.db.Close()
}

type dispRow struct {
	id    int64
	loc   string
	kind  int
	size  int64
	fhash []byte
}

// dispatch sucht Dateien ohne Analyse (und fällige Wiederholungen) und verteilt sie auf die Worker.
func (a *Analyzer) dispatch(ctx context.Context) {
	var cursor int64
	sleep := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-a.retryNow:
			return true
		case <-time.After(d):
			return true
		}
	}
	for ctx.Err() == nil {
		if a.e.paused.Load() {
			if !sleep(time.Second) {
				return
			}
			continue
		}
		// 1. fällige Wiederholungen
		if rs, err := a.e.rdb.QueryContext(ctx, `SELECT f.id, f.loc, f.kind, f.size, f.fhash FROM analysis x
			JOIN files f ON f.id = x.file_id WHERE x.state = 8 AND coalesce(x.retry_at,0) <= ? LIMIT 200`, time.Now().Unix()); err == nil {
			var retry []dispRow
			for rs.Next() {
				var r dispRow
				rs.Scan(&r.id, &r.loc, &r.kind, &r.size, &r.fhash)
				retry = append(retry, r)
			}
			rs.Close()
			for _, r := range retry {
				if !a.send(ctx, r, nil) {
					return
				}
			}
		}
		// 2. neue Dateien
		// Dateien, über die Stufe 2/3 noch entscheidet (gleiche Größe bzw. gleicher Teil-Hash wie eine andere),
		// kommen erst danach dran: Ist sie eine byte-gleiche Kopie, muss sie gar nicht analysiert werden.
		rs, err := a.e.rdb.QueryContext(ctx, `SELECT f.id, f.loc, f.kind, f.size, f.fhash FROM files f
			WHERE f.id > ? AND f.size >= 0 AND f.state BETWEEN 1 AND 3
			  AND NOT ((f.state = 1 AND f.need >= 1) OR (f.state = 2 AND f.need = 2))
			  AND NOT EXISTS (SELECT 1 FROM analysis x WHERE x.file_id = f.id)
			ORDER BY f.id LIMIT 500`, cursor)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("Ähnlichkeit: Abfrage: %v", err)
			}
			if !sleep(5 * time.Second) {
				return
			}
			continue
		}
		var batch []dispRow
		for rs.Next() {
			var r dispRow
			rs.Scan(&r.id, &r.loc, &r.kind, &r.size, &r.fhash)
			batch = append(batch, r)
		}
		rs.Close()
		if len(batch) == 0 {
			cursor = 0
			if !sleep(3 * time.Second) {
				return
			}
			continue
		}
		var quick [][3]int64 // file_id, state, mt
		for _, r := range batch {
			cursor = r.id
			if !a.send(ctx, r, &quick) {
				return
			}
		}
		if len(quick) > 0 {
			if err := a.markMany(quick, time.Now().Unix()); err != nil {
				log.Printf("Ähnlichkeit: %v", err)
			}
		}
	}
}

// send ordnet eine Datei ein: sofort erledigt (quick) oder an den passenden Worker-Pool.
// quick == nil: Wiederholung – immer an einen Worker.
func (a *Analyzer) send(ctx context.Context, r dispRow, quick *[][3]int64) bool {
	if _, busy := a.inflight.Load(r.id); busy {
		return true
	}
	if quick != nil && len(r.fhash) > 0 && a.copyAnalyzed(r.id, r.fhash) {
		*quick = append(*quick, [3]int64{r.id, anDup, int64(classify(r.loc))})
		return true
	}
	mt := classify(r.loc)
	snf := mt == mtNone && (r.kind == kindLocal || extOf(r.loc) == "")
	needWhole := a.hasMembers.Load() && r.size >= minExact && a.memberWithSize(r.size)
	if quick != nil {
		switch {
		case mt == mtNone && !snf && !needWhole:
			*quick = append(*quick, [3]int64{r.id, anSkip, mtNone})
			return true
		case mt != mtVideo && mt != mtAudio && mt != mtNone && r.size > a.maxBytes:
			*quick = append(*quick, [3]int64{r.id, anTooBig, int64(mt)})
			return true
		}
	}
	j := anJob{ID: r.id, Loc: r.loc, Kind: r.kind, Size: r.size, FHash: r.fhash, MT: mt, Sniff: snf, NeedWhole: needWhole}
	q := a.jobs
	if (mt == mtVideo || mt == mtAudio) && !needWhole {
		q = a.mediaJobs
	}
	a.inflight.Store(r.id, true)
	select {
	case q <- j:
		return true
	case <-ctx.Done():
		a.inflight.Delete(r.id)
		return false
	}
}

// copyAnalyzed: Gibt es eine byte-gleiche Datei, deren Analyse schon fertig ist (oder gerade läuft)?
func (a *Analyzer) copyAnalyzed(id int64, fhash []byte) bool {
	rows, err := a.e.rdb.Query(`SELECT f.id, coalesce(x.state, 0) FROM files f LEFT JOIN analysis x ON x.file_id = f.id
		WHERE f.fhash = ? AND f.id <> ? LIMIT 20`, fhash, id)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var other int64
		var st int
		rows.Scan(&other, &st)
		if _, busy := a.inflight.Load(other); busy || st == anDone || st == anNoFP || st == anSkip || st == anTooBig {
			return true
		}
	}
	return false
}

func (a *Analyzer) memberWithSize(size int64) bool {
	var one int
	return a.e.rdb.QueryRow(`SELECT 1 FROM fps WHERE kind=4 AND size=? LIMIT 1`, size).Scan(&one) == nil
}

func (a *Analyzer) markMany(rows [][3]int64, now int64) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	st, err := tx.Prepare(`INSERT OR IGNORE INTO analysis(file_id, state, mt, done) VALUES(?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	var added []int64
	for _, r := range rows {
		if res, err := st.Exec(r[0], r[1], r[2], now); err == nil {
			if c, _ := res.RowsAffected(); c > 0 {
				added = append(added, r[1])
			}
		}
	}
	st.Close()
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, s := range added {
		a.cnt[s].Add(1)
	}
	return nil
}

// RetryFailed setzt endgültig fehlgeschlagene Analysen zurück (Knopf in der Web-UI).
func (a *Analyzer) RetryFailed() (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	res, err := a.db.Exec(`UPDATE analysis SET state=8, tries=0, retry_at=0 WHERE state=9`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	a.cnt[anFail].Add(-n)
	a.cnt[anRetry].Add(n)
	select {
	case a.retryNow <- struct{}{}:
	default:
	}
	return n, nil
}

// ---------- Analyse einer Datei ----------

func (a *Analyzer) analyze(ctx context.Context, j anJob) (res anResult) {
	fail := func(err error) anResult {
		return anResult{State: anFail, MT: j.MT, Err: err.Error(), Transient: isTransient(err)}
	}
	if j.Sniff {
		mt, ext, err := a.sniffJob(ctx, j)
		if err != nil {
			return fail(err)
		}
		j.MT, j.Ext = mt, ext
		switch {
		case mt == mtNone && !j.NeedWhole:
			return anResult{State: anSkip}
		case mt != mtVideo && mt != mtAudio && mt != mtNone && j.Size > a.maxBytes:
			return anResult{State: anTooBig, MT: mt}
		}
	}
	ext := j.Ext
	if ext == "" {
		ext = extOf(j.Loc)
	}

	// Videos/Audio werden nicht heruntergeladen: ffmpeg springt per Range-Anfrage zu den Ausschnitten.
	if (j.MT == mtVideo || j.MT == mtAudio) && !j.NeedWhole {
		src := j.Loc
		if j.Kind == kindHTTP && !a.rangeOK(ctx, j.Loc) {
			// Ohne Range-Unterstützung kann ffmpeg nicht springen und liefert stillschweigend
			// falsche Ausschnitte → dann komplett laden (sofern nicht größer als -sim-max).
			if j.Size > a.maxBytes {
				return fail(fmt.Errorf("Server kann keine Teil-Downloads und Datei ist größer als -sim-max (%d MB)", a.maxBytes>>20))
			}
			file, _, cleanup, err := a.download(ctx, j.Loc, j.Size)
			if err != nil {
				return fail(err)
			}
			defer cleanup()
			src = file
		}
		it, err := a.media(ctx, j, src)
		if err != nil {
			if j.Kind == kindHTTP && src == j.Loc {
				err = transient(err) // Server/Netz kann der Grund sein
			}
			return fail(err)
		}
		return anResult{State: anDone, MT: j.MT, Items: []fpItem{it}}
	}

	file, sha, cleanup, err := a.obtain(ctx, j)
	if err != nil {
		return fail(err)
	}
	defer cleanup()
	tmp, err := os.MkdirTemp("", "wd-an-*")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tmp)

	var items []fpItem
	var errs []string
	switch j.MT {
	case mtImage, mtText, mtOffice:
		b, err := os.ReadFile(file)
		if err != nil {
			return fail(err)
		}
		it, e2 := a.content(ctx, j.ID, "", ext, j.MT, b, tmp, true)
		items = append(items, it...)
		if e2 != nil {
			errs = append(errs, e2.Error())
		}
	case mtPDF:
		it, e2 := a.pdf(ctx, j.ID, "", file, j.Size, tmp)
		items = append(items, it...)
		if e2 != nil {
			errs = append(errs, e2.Error())
		}
	case mtVideo, mtAudio: // nur hier, wenn auch ein exakter Abgleich nötig war (Datei liegt schon vor)
		if it, err := a.media(ctx, j, file); err == nil {
			items = append(items, it)
		} else {
			errs = append(errs, err.Error())
		}
	case mtArchive:
		if sha == nil {
			if sha, err = fileSHA(file, &a.e.bytesRead); err != nil {
				return fail(err)
			}
		}
		it, sum, e2 := a.archive(ctx, j, file, ext, tmp)
		items = append(items, it...)
		if e2 != nil {
			errs = append(errs, e2.Error())
		}
		if sum != nil {
			sum.sha = sha
			res.Arc = sum
		}
	}
	if j.NeedWhole && sha != nil {
		items = append(items, fpItem{File: j.ID, Kind: fpWhole, H1: binary.BigEndian.Uint64(sha), Size: j.Size, Extra: sha})
	}
	switch {
	case len(items) > 0:
		res.State = anDone
	case len(errs) > 0:
		res.State = anFail
	case j.MT == mtNone:
		res.State = anSkip
	default:
		res.State = anNoFP
	}
	res.MT = j.MT
	res.Items = items
	res.Err = strings.Join(errs, "; ")
	return res
}

// media: Fingerabdruck für Video oder Audio (src = URL oder lokaler Pfad).
func (a *Analyzer) media(ctx context.Context, j anJob, src string) (fpItem, error) {
	if j.MT == mtAudio {
		af, err := a.t.audioFingerprint(ctx, src, a.e.cfg.UserAgent, a.frames)
		if err != nil {
			return fpItem{}, err
		}
		return fpItem{File: j.ID, Kind: fpAudio, Size: j.Size, Dur: af.Dur, Extra: framesBlob(af.Segs)}, nil
	}
	vf, err := a.t.videoFingerprint(ctx, src, a.e.cfg.UserAgent, a.frames)
	if err != nil {
		return fpItem{}, err
	}
	return fpItem{File: j.ID, Kind: fpVideo, Size: j.Size, Dur: vf.Dur, Extra: framesBlob(vf.Frames), Thumb: vf.Thumb}, nil
}

// sniffJob liest die ersten Bytes (lokal bzw. per Range-Anfrage) und erkennt daran den Typ.
func (a *Analyzer) sniffJob(ctx context.Context, j anJob) (int, string, error) {
	if j.Kind == kindLocal {
		fd, err := os.Open(j.Loc)
		if err != nil {
			return mtNone, "", errPermanent{err}
		}
		defer fd.Close()
		b := make([]byte, sniffLen)
		n, _ := io.ReadFull(fd, b)
		mt, ext := sniff(b[:n], "")
		return mt, ext, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*a.e.fet.stall)
	defer cancel()
	resp, err := a.e.fet.req(ctx, http.MethodGet, j.Loc, fmt.Sprintf("bytes=0-%d", sniffLen-1))
	if err != nil {
		return mtNone, "", transient(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, sniffLen))
	a.e.bytesRead.Add(int64(len(b)))
	mt, ext := sniff(b, resp.Header.Get("Content-Type"))
	return mt, ext, nil
}

// rangeOK: Beantwortet der Server Range-Anfragen mit 206? (Ergebnis pro Host gemerkt)
func (a *Analyzer) rangeOK(ctx context.Context, loc string) bool {
	host := hostOf(loc)
	if v, ok := a.ranges.Load(host); ok {
		return v.(bool)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*a.e.fet.stall)
	defer cancel()
	resp, err := a.e.fet.req(ctx, http.MethodGet, loc, "bytes=0-0")
	if err != nil {
		return true // unklar (z.B. Zeitüberschreitung) → ffmpeg versuchen lassen, nicht merken
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	ok := resp.StatusCode == http.StatusPartialContent
	a.ranges.Store(host, ok)
	return ok
}

// obtain liefert einen lokalen Pfad zur Datei (bei HTTP: Download in eine temporäre Datei)
// und den SHA-256 des Inhalts, falls er ohnehin anfällt oder gebraucht wird.
func (a *Analyzer) obtain(ctx context.Context, j anJob) (file string, sha []byte, cleanup func(), err error) {
	cleanup = func() {}
	if j.Kind == kindLocal {
		if j.NeedWhole {
			if len(j.FHash) == sha256.Size {
				sha = j.FHash
			} else if sha, err = fileSHA(j.Loc, &a.e.bytesRead); err != nil {
				return "", nil, cleanup, err
			}
		}
		return j.Loc, sha, cleanup, nil
	}
	return a.download(ctx, j.Loc, j.Size)
}

func fileSHA(p string, n *atomic.Int64) ([]byte, error) {
	fd, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer fd.Close()
	h := sha256.New()
	if _, err := io.Copy(h, &countReader{fd, n}); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// download lädt eine URL vollständig in eine temporäre Datei (mit SHA-256, Abbruch bei Stillstand).
// Netz- und Serverfehler sind vorübergehend (errTransient), 404/403 & Co. endgültig.
func (a *Analyzer) download(ctx context.Context, loc string, size int64) (string, []byte, func(), error) {
	noop := func() {}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	f := a.e.fet
	resp, err := f.req(ctx, http.MethodGet, loc, "")
	if err != nil {
		return "", nil, noop, transient(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", nil, noop, transient(fmt.Errorf("unerwarteter Status %d", resp.StatusCode))
	}
	tmp, err := os.CreateTemp("", "wd-dl-*")
	if err != nil {
		return "", nil, noop, err
	}
	cleanup := func() { tmp.Close(); os.Remove(tmp.Name()) }
	t := time.AfterFunc(f.stall, cancel)
	defer t.Stop()
	h := sha256.New()
	body := &stallReader{r: resp.Body, t: t, d: f.stall, b: f.bytes, lim: f.lim}
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, a.maxBytes+1))
	if err == nil && n > a.maxBytes {
		cleanup()
		return "", nil, noop, fmt.Errorf("größer als -sim-max (%d MB)", a.maxBytes>>20)
	}
	if err == nil && size >= 0 && n != size {
		err = errChanged
	}
	if err != nil {
		cleanup()
		if ctx.Err() != nil && !errors.Is(err, errChanged) {
			err = fmt.Errorf("Download abgebrochen (>%s ohne Daten oder Programmende)", f.stall)
		}
		return "", nil, noop, transient(err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, noop, err
	}
	return tmp.Name(), h.Sum(nil), cleanup, nil
}

// content analysiert Bild-, Text- und Office-Inhalte aus dem Speicher.
// exact: Byte-Gleichheit dieser Bytes wird schon anderswo gemeldet (ganze Datei / Archiv-Eintrag).
func (a *Analyzer) content(ctx context.Context, file int64, sub, ext string, mt int, b []byte, tmp string, exact bool) ([]fpItem, error) {
	cs := shaPrefix(b)
	switch mt {
	case mtImage:
		img, err := decodeImage(b)
		if err != nil {
			return nil, fmt.Errorf("Bild: %v", err)
		}
		f, ok := imageFingerprint(img)
		if !ok {
			return nil, nil
		}
		return []fpItem{{File: file, Sub: sub, Kind: fpImage, H1: f.P, H2: f.D, Size: int64(len(b)), W: f.W, H: f.H, Thumb: f.Thumb, CS: cs, Ex: exact}}, nil
	case mtText:
		if f, ok := textFingerprint(plainText(ext, b)); ok {
			return []fpItem{{File: file, Sub: sub, Kind: fpText, H1: f.Sim, Size: int64(len(b)), Words: f.Words, Snip: f.Snip, Extra: minBlob(f.Min), CS: cs, Ex: exact}}, nil
		}
		return nil, nil
	case mtOffice:
		ra := bytes.NewReader(b)
		var out []fpItem
		s, err := officeText(ra, int64(len(b)))
		if err == nil {
			if f, ok := textFingerprint(s); ok {
				out = append(out, fpItem{File: file, Sub: sub, Kind: fpText, H1: f.Sim, Size: int64(len(b)), Words: f.Words, Snip: f.Snip, Extra: minBlob(f.Min), CS: cs, Ex: exact})
			}
		}
		// eingebettete Bilder (word/media, ppt/media, Pictures/)
		p := filepath.Join(tmp, "office.zip")
		if os.WriteFile(p, b, 0o600) == nil {
			n := 0
			walkArchive(p, "zip", func(name string, size int64, r io.Reader) error {
				if classify(name) != mtImage || size > memBufMax || n >= pdfMaxImgs {
					return nil
				}
				ib, e := io.ReadAll(r)
				if e != nil {
					return nil
				}
				if img, e := decodeImage(ib); e == nil && img.Bounds().Dx() >= minImgSide && img.Bounds().Dy() >= minImgSide {
					if f, ok := imageFingerprint(img); ok {
						n++
						out = append(out, fpItem{File: file, Sub: joinSub(sub, name), Kind: fpImage, H1: f.P, H2: f.D,
							Size: int64(len(ib)), W: f.W, H: f.H, Thumb: f.Thumb, CS: shaPrefix(ib)})
					}
				}
				return nil
			})
			os.Remove(p)
		}
		return out, err
	}
	return nil, nil
}

func joinSub(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + " › " + child
}

// pdf: Text + eingebettete Bilder.
func (a *Analyzer) pdf(ctx context.Context, file int64, sub, path string, size int64, tmp string) ([]fpItem, error) {
	var out []fpItem
	var errs []string
	if s, err := a.t.pdfText(ctx, path); err == nil {
		if f, ok := textFingerprint(s); ok {
			out = append(out, fpItem{File: file, Sub: sub, Kind: fpText, H1: f.Sim, Size: size, Words: f.Words, Snip: f.Snip, Extra: minBlob(f.Min)})
		}
	} else {
		errs = append(errs, err.Error())
	}
	dir, err := os.MkdirTemp(tmp, "pdfimg-*")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(dir)
	imgs, err := a.t.pdfImages(ctx, path, dir, pdfMaxImgs)
	if err != nil {
		errs = append(errs, err.Error())
	}
	for i, p := range imgs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		img, err := decodeImage(b)
		if err != nil || img.Bounds().Dx() < minImgSide || img.Bounds().Dy() < minImgSide || isBitonal(img) {
			continue // zu klein oder eingescannte Schwarz-Weiß-Seite
		}
		if f, ok := imageFingerprint(img); ok {
			out = append(out, fpItem{File: file, Sub: joinSub(sub, fmt.Sprintf("PDF-Bild %d", i+1)), Kind: fpImage,
				H1: f.P, H2: f.D, Size: int64(len(b)), W: f.W, H: f.H, Thumb: f.Thumb, CS: shaPrefix(b)})
		}
	}
	if len(out) == 0 && len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}

// archive: exakter SHA-256 für jeden Eintrag + inhaltliche Analyse kleinerer Einträge.
func (a *Analyzer) archive(ctx context.Context, j anJob, file, ext, tmp string) ([]fpItem, *arcSummary, error) {
	var out []fpItem
	type mem struct {
		sha  []byte
		size int64
	}
	var members []mem
	sum := &arcSummary{}
	err := walkArchive(file, ext, func(name string, size int64, r io.Reader) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h := sha256.New()
		mt := classify(name)
		var buf []byte
		if mt != mtNone && mt != mtVideo && mt != mtAudio && mt != mtArchive && size <= memBufMax {
			b, err := io.ReadAll(io.TeeReader(r, h))
			if err != nil {
				return nil // beschädigter Eintrag: weiter mit dem nächsten
			}
			buf = b
		} else if _, err := io.Copy(h, r); err != nil {
			return nil
		}
		sha := h.Sum(nil)
		if size >= minExact {
			out = append(out, fpItem{File: j.ID, Sub: name, Kind: fpMember, H1: binary.BigEndian.Uint64(sha), Size: size, Extra: sha})
			members = append(members, mem{sha, size})
			sum.members++
			sum.bytes += size
		}
		if buf != nil {
			if mt == mtPDF {
				p := filepath.Join(tmp, "eintrag.pdf")
				if os.WriteFile(p, buf, 0o600) == nil {
					it, _ := a.pdf(ctx, j.ID, name, p, size, tmp)
					out = append(out, it...)
					os.Remove(p)
				}
			} else {
				it, _ := a.content(ctx, j.ID, name, extOf(name), mt, buf, tmp, size >= minExact)
				out = append(out, it...)
			}
		}
		return nil
	})
	if err != nil && len(out) == 0 {
		return nil, nil, fmt.Errorf("Archiv: %v", err)
	}
	// Gegenstücke: einzelne Dateien mit gleicher Größe exakt prüfen
	for _, m := range members {
		out = append(out, a.counterparts(ctx, j.ID, m.sha, m.size)...)
	}
	return out, sum, nil
}

// counterparts: einzelne Dateien gleicher Größe, deren SHA-256 dem Archiv-Eintrag entspricht.
func (a *Analyzer) counterparts(ctx context.Context, self int64, sha []byte, size int64) []fpItem {
	rows, err := a.e.rdb.QueryContext(ctx, `SELECT id, loc, kind, fhash FROM files
		WHERE size=? AND state BETWEEN 1 AND 3 AND id<>? LIMIT 4`, size, self)
	if err != nil {
		return nil
	}
	type cand struct {
		id    int64
		loc   string
		kind  int
		fhash []byte
	}
	var cs []cand
	for rows.Next() {
		var c cand
		rows.Scan(&c.id, &c.loc, &c.kind, &c.fhash)
		cs = append(cs, c)
	}
	rows.Close()
	var out []fpItem
	for _, c := range cs {
		h := c.fhash
		if len(h) != sha256.Size {
			var err error
			if c.kind == kindLocal {
				h, err = fileSHA(c.loc, &a.e.bytesRead)
			} else if size <= a.maxBytes {
				var cl func()
				_, h, cl, err = a.download(ctx, c.loc, size)
				cl()
			} else {
				continue
			}
			if err != nil {
				continue
			}
		}
		if string(h) == string(sha) {
			out = append(out, fpItem{File: c.id, Kind: fpWhole, H1: binary.BigEndian.Uint64(h), Size: size, Extra: h})
		}
	}
	return out
}

// ---------- Speichern + Vergleichen ----------

type pairCand struct {
	other int64
	score float64
	kind  int
}

type fpAdded struct {
	id int64
	it fpItem
}

func (a *Analyzer) commit(j anJob, res anResult) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()

	prevState, prevTries := -1, 0
	tx.QueryRow(`SELECT state, tries FROM analysis WHERE file_id=?`, j.ID).Scan(&prevState, &prevTries)

	insFP, err := tx.Prepare(`INSERT INTO fps(file_id, sub, kind, h1, h2, size, w, h, dur, words, snip, extra, cs, ex)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insFP.Close()
	var news []fpAdded
	type pairRec struct {
		a, b int64
		pairCand
	}
	var pairs []pairRec
	addPair := func(x int64, c pairCand) { pairs = append(pairs, pairRec{min(x, c.other), max(x, c.other), c}) }
	for _, it := range res.Items {
		if it.Kind == fpWhole {
			// höchstens ein exakter Fingerabdruck pro Datei (kann vom Archiv-Abgleich oder der Datei selbst kommen)
			var id int64
			if tx.QueryRow(`SELECT id FROM fps WHERE file_id=? AND kind=5`, it.File).Scan(&id) == nil {
				continue
			}
		}
		var w, h, words, dur, snip, cs any
		if it.W > 0 {
			w, h = it.W, it.H
		}
		if it.Words > 0 {
			words = it.Words
		}
		if it.Dur > 0 {
			dur = it.Dur
		}
		if it.Snip != "" {
			snip = it.Snip
		}
		if it.CS != 0 {
			cs = int64(it.CS)
		}
		r, err := insFP.Exec(it.File, it.Sub, it.Kind, int64(it.H1), int64(it.H2), it.Size, w, h, dur, words, snip, it.Extra, cs, it.Ex)
		if err != nil {
			return err
		}
		id, _ := r.LastInsertId()
		if len(it.Thumb) > 0 {
			tx.Exec(`INSERT OR REPLACE INTO thumbs(fp, jpg) VALUES(?,?)`, id, it.Thumb)
		}
		news = append(news, fpAdded{id, it})
	}

	// Archiv-Verwandtschaft: andere Archive → gemeinsame Einträge (SHA-256 → Größe)
	shared := map[int64]map[string]int64{}

	// Vergleich gegen alles Bisherige
	for _, n := range news {
		var cands []pairCand
		switch n.it.Kind {
		case fpImage:
			a.img.near(n.it.H1, imgMaxP, func(x mihEntry, d int) {
				if x.file != n.it.File && hamming(x.aux, n.it.H2) <= imgMaxD && !sameBytes(x, n.it) {
					cands = append(cands, pairCand{x.fp, 1 - float64(d)/64, pkImage})
				}
			})
		case fpText:
			// SimHash ist nur der Vorfilter (alle Texte durchgehen: ein Popcount je Eintrag, auch bei
			// 100.000 Texten unter 1 ms) – bestätigt wird über gemeinsame Wort-Dreiergruppen (MinHash).
			mine := blobMin(n.it.Extra)
			for _, x := range a.txt.e {
				if x.file == n.it.File || sameBytes(x, n.it) {
					continue
				}
				if d := hamming(x.h, n.it.H1); d <= txtMax {
					if j := jaccard(mine, a.txtMin[x.fp]); textSimilar(d, j) {
						cands = append(cands, pairCand{x.fp, j, pkText})
					}
				}
			}
		case fpVideo, fpAudio:
			m, meta, thr, pk := &a.vid, a.vidMeta, vidFrameD, pkVideo
			if n.it.Kind == fpAudio {
				m, meta, thr, pk = &a.aud, a.audMeta, audFrameD, pkAudio
			}
			segs := blobFrames(n.it.Extra)
			seen := map[int64]bool{}
			for _, h := range segs {
				if h == 0 {
					continue
				}
				m.near(h, 7, func(x mihEntry, d int) {
					if seen[x.fp] || x.file == n.it.File {
						return
					}
					seen[x.fp] = true
					v := meta[x.fp]
					if s, ok := seqScore(segs, v.segs, n.it.Dur, v.dur, thr); ok {
						cands = append(cands, pairCand{x.fp, s, pk})
					}
				})
			}
		case fpMember, fpWhole:
			rows, err := tx.Query(`SELECT id, file_id, kind, extra FROM fps WHERE kind IN (4,5) AND h1=? AND id<>?`, int64(n.it.H1), n.id)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id, file int64
				var kind int
				var ex []byte
				rows.Scan(&id, &file, &kind, &ex)
				if string(ex) != string(n.it.Extra) || file == n.it.File {
					continue
				}
				switch {
				case kind == fpWhole && n.it.Kind == fpWhole:
					// zwei einzelne Dateien sind Sache der Stufen 1–3
				case kind == fpMember && n.it.Kind == fpMember:
					// zwei Archive mit demselben Eintrag → Archiv-Verwandtschaft statt Einzelpaar
					if shared[file] == nil {
						shared[file] = map[string]int64{}
					}
					shared[file][string(ex)] = n.it.Size
				default:
					cands = append(cands, pairCand{id, 1, pkArchive})
				}
			}
			rows.Close()
		}
		if len(cands) > genericHits && (n.it.Kind == fpImage || n.it.Kind == fpText) {
			cands = nil // zu allgemein: Paare wären nur Rauschen
		}
		sort.Slice(cands, func(i, k int) bool { return cands[i].score > cands[k].score })
		if len(cands) > maxPairsNew {
			cands = cands[:maxPairsNew]
		}
		for _, c := range cands {
			addPair(n.id, c)
		}
	}

	var newPairs [pkKinds]int64
	for _, p := range pairs {
		r, err := tx.Exec(`INSERT OR IGNORE INTO pairs(kind, a, b, score, found) VALUES(?,?,?,?,?)`, p.kind, p.a, p.b, p.score, now)
		if err != nil {
			return err
		}
		if c, _ := r.RowsAffected(); c > 0 {
			newPairs[p.kind]++
		}
	}

	// Archive: Kennzahlen speichern, Verwandtschaft zu anderen (nicht byte-gleichen) Archiven
	var newRels int64
	if res.Arc != nil {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO arcinfo(file_id, members, bytes, sha) VALUES(?,?,?,?)`,
			j.ID, res.Arc.members, res.Arc.bytes, res.Arc.sha); err != nil {
			return err
		}
		for other, set := range shared {
			var osha, ofh, mfh []byte
			tx.QueryRow(`SELECT i.sha, f.fhash FROM files f LEFT JOIN arcinfo i ON i.file_id = f.id WHERE f.id=?`, other).Scan(&osha, &ofh)
			tx.QueryRow(`SELECT fhash FROM files WHERE id=?`, j.ID).Scan(&mfh)
			if (osha != nil && string(osha) == string(res.Arc.sha)) || (ofh != nil && string(ofh) == string(res.Arc.sha)) ||
				(mfh != nil && ofh != nil && string(mfh) == string(ofh)) {
				continue // byte-gleiches Archiv: steht in den Duplikat-Gruppen
			}
			var b int64
			for _, s := range set {
				b += s
			}
			x, y := min(j.ID, other), max(j.ID, other)
			var one int
			isNew := tx.QueryRow(`SELECT 1 FROM arcrel WHERE a=? AND b=?`, x, y).Scan(&one) != nil
			if _, err := tx.Exec(`INSERT OR REPLACE INTO arcrel(a, b, shared, shared_bytes, found) VALUES(?,?,?,?,?)`,
				x, y, len(set), b, now); err != nil {
				return err
			}
			if isNew {
				newRels++
			}
		}
	}

	state, tries := res.State, prevTries // Zahl der Versuche bleibt sichtbar
	var retryAt any
	if state == anFail && res.Transient && prevTries < maxRetries {
		state, tries = anRetry, prevTries+1
		retryAt = now + int64(retryDelay[prevTries]/time.Second)
	}
	var errv any
	if res.Err != "" {
		errv = res.Err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO analysis(file_id, state, mt, err, done, tries, retry_at) VALUES(?,?,?,?,?,?,?)`,
		j.ID, state, res.MT, errv, now, tries, retryAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// erst jetzt in den Suchindex (Einträge derselben Datei werden ohnehin nicht verglichen)
	for _, n := range news {
		a.index(n.id, n.it)
	}
	if prevState >= 0 && prevState < len(a.cnt) {
		a.cnt[prevState].Add(-1)
	}
	a.cnt[state].Add(1)
	var total int64
	for k, c := range newPairs {
		a.pairs[k].Add(c)
		total += c
	}
	a.arcRels.Add(newRels)
	if total > 0 || newRels > 0 {
		a.changed.Add(1)
	}
	if res.Arc != nil && res.Arc.members > 0 {
		a.hasMembers.Store(true)
	}
	return nil
}

// ---------- Statistik ----------

type SimStats struct {
	Analyzed    int64    `json:"analyzed"`
	Done        int64    `json:"done"`
	Skipped     int64    `json:"skipped"`
	NoFP        int64    `json:"noFP"`
	Dup         int64    `json:"dup"`
	TooBig      int64    `json:"tooBig"`
	Retry       int64    `json:"retry"`
	Failed      int64    `json:"failed"`
	Pending     int64    `json:"pending"`
	Active      int32    `json:"active"`
	ActiveMedia int32    `json:"activeMedia"`
	QueuedMedia int      `json:"queuedMedia"`
	Pairs       int64    `json:"pairs"`
	PairsImage  int64    `json:"pairsImage"`
	PairsText   int64    `json:"pairsText"`
	PairsVideo  int64    `json:"pairsVideo"`
	PairsAudio  int64    `json:"pairsAudio"`
	PairsArch   int64    `json:"pairsArchive"`
	ArcRels     int64    `json:"arcRels"`
	Groups      int      `json:"groups"`
	Savings     int64    `json:"savings"`
	SafeSavings int64    `json:"safeSavings"`
	Missing     []string `json:"missing,omitempty"`
}

func (a *Analyzer) stats(base Stats) *SimStats {
	s := &SimStats{
		Done: a.cnt[anDone].Load(), Skipped: a.cnt[anSkip].Load(), NoFP: a.cnt[anNoFP].Load(), Dup: a.cnt[anDup].Load(),
		TooBig: a.cnt[anTooBig].Load(), Retry: a.cnt[anRetry].Load(), Failed: a.cnt[anFail].Load(),
		Active: a.active.Load(), ActiveMedia: a.activeMedia.Load(), QueuedMedia: len(a.mediaJobs),
		PairsImage: a.pairs[pkImage].Load(), PairsText: a.pairs[pkText].Load(),
		PairsVideo: a.pairs[pkVideo].Load(), PairsAudio: a.pairs[pkAudio].Load(), PairsArch: a.pairs[pkArchive].Load(),
		ArcRels: a.arcRels.Load(),
	}
	s.Analyzed = s.Done + s.Skipped + s.NoFP + s.Dup + s.TooBig + s.Retry + s.Failed
	s.Pairs = s.PairsImage + s.PairsText + s.PairsVideo + s.PairsAudio + s.PairsArch
	// Dateien mit bekannter Größe, die noch keine Analyse haben (Näherung)
	s.Pending = max(0, base.Total-base.NeedProbe-base.Errors-s.Analyzed)
	g := a.grp.get()
	s.Groups, s.Savings, s.SafeSavings = len(g.groups), g.savings, g.safeSavings
	if a.t.ffmpeg == "" || a.t.ffprobe == "" {
		s.Missing = append(s.Missing, "ffmpeg")
	}
	if a.t.pdftotext == "" || a.t.pdfimages == "" {
		s.Missing = append(s.Missing, "poppler-utils")
	}
	return s
}

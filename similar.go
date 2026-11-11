package main

// Stufe 4: Ähnlichkeitsanalyse (Option -similar).
//
// Läuft neben den Stufen 1–3 her und nimmt sich jede Datei mit bekannter Größe einmal vor:
//   - Bilder, Videos, Texte, Office-Dokumente, PDFs und Archive bekommen Fingerabdrücke,
//   - jeder neue Fingerabdruck wird sofort mit allen bisherigen verglichen,
//   - gefundene Paare landen in der Tabelle "pairs" (mit Ähnlichkeit 0–1).
//
// Archive: Jede enthaltene Datei bekommt einen SHA-256. Liegt dieselbe Datei auch einzeln im Netz
// (oder in einem anderen Archiv), entsteht ein Paar „exakt gleich, einmal verpackt“.
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
)

// Arten von Paaren (pairs.kind)
const (
	pkImage   = 1
	pkText    = 2
	pkVideo   = 3
	pkArchive = 4
)

// Analyse-Zustände (analysis.state)
const (
	anDone   = 1 // Fingerabdruck erstellt
	anSkip   = 2 // kein analysierbarer Dateityp
	anTooBig = 3 // größer als -sim-max
	anNoFP   = 4 // analysiert, aber nichts Vergleichbares (einfarbiges Bild, sehr kurzer Text …)
	anFail   = 9
)

const (
	minExact    = 4096     // kleinere Archiv-Einträge werden nicht exakt abgeglichen (README, LICENSE …)
	memBufMax   = 64 << 20 // Archiv-Einträge bis zu dieser Größe werden auch inhaltlich analysiert
	maxPairsNew = 25       // höchstens so viele Paare pro neuem Fingerabdruck
	genericHits = 100      // mehr Treffer = zu allgemeiner Inhalt (leere Seite, Standardmuster) → keine Paare
	pdfMaxImgs  = 60
	minImgSide  = 48 // kleinere Bilder (Symbole, Linien) aus PDFs/Office ignorieren
)

const simSchema = `
CREATE TABLE IF NOT EXISTS analysis(
  file_id INTEGER PRIMARY KEY,
  state   INTEGER NOT NULL,
  mt      INTEGER NOT NULL DEFAULT 0,
  err     TEXT,
  done    INTEGER
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
  extra   BLOB,                         -- Video: Einzelbild-Hashes; exakt: voller SHA-256
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
`

type anJob struct {
	ID        int64
	Loc       string
	Kind      int // kindHTTP / kindLocal
	Size      int64
	FHash     []byte
	MT        int
	NeedWhole bool // eine Archiv-Datei hat dieselbe Größe → exakten SHA-256 bilden
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
	State int
	Err   string
	Items []fpItem
}

type vidInfo struct {
	file   int64
	frames []uint64
	dur    float64
}

type Analyzer struct {
	e        *Engine
	db       *sql.DB
	t        tools
	workers  int
	maxBytes int64
	frames   int

	jobs     chan anJob
	inflight sync.Map
	ranges   sync.Map // Host → unterstützt Range-Anfragen

	mu       sync.Mutex // Index + Schreiben gehören zusammen
	img, txt mih
	vid      mih
	vidMeta  map[int64]vidInfo

	cnt        [10]atomic.Int64 // Zähler je analysis.state
	pairs      [5]atomic.Int64  // Zähler je pairs.kind
	active     atomic.Int32
	hasMembers atomic.Bool
	wg         sync.WaitGroup
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
	a := &Analyzer{
		e: e, db: db, t: findTools(),
		workers:  max(1, e.cfg.AnalyzeWorkers),
		maxBytes: int64(e.cfg.SimMaxMB) << 20,
		frames:   max(3, e.cfg.VideoFrames),
		vidMeta:  map[int64]vidInfo{},
	}
	a.jobs = make(chan anJob, a.workers*4)
	if a.t.ffmpeg == "" {
		log.Printf("Ähnlichkeit: ffmpeg fehlt – Videos werden übersprungen")
	}
	if a.t.pdftotext == "" {
		log.Printf("Ähnlichkeit: pdftotext/pdfimages fehlen (poppler-utils) – PDFs werden übersprungen")
	}
	return a, a.load()
}

// load baut die Such-Indizes und Zähler aus der Datenbank auf (nach Neustart).
func (a *Analyzer) load() error {
	t0 := time.Now()
	rows, err := a.db.Query(`SELECT id, file_id, kind, h1, coalesce(h2,0), coalesce(dur,0), extra, coalesce(cs,0), ex FROM fps WHERE kind IN (1,2,3)`)
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
	r2, err := a.db.Query(`SELECT state, count(*) FROM analysis GROUP BY state`)
	if err != nil {
		return err
	}
	for r2.Next() {
		var st int
		var c int64
		r2.Scan(&st, &c)
		if st >= 0 && st < len(a.cnt) {
			a.cnt[st].Store(c)
		}
	}
	r2.Close()
	r3, err := a.db.Query(`SELECT kind, count(*) FROM pairs GROUP BY kind`)
	if err != nil {
		return err
	}
	for r3.Next() {
		var k int
		var c int64
		r3.Scan(&k, &c)
		if k > 0 && k < len(a.pairs) {
			a.pairs[k].Store(c)
		}
	}
	r3.Close()
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
	case fpVideo:
		fr := blobFrames(it.Extra)
		a.vidMeta[id] = vidInfo{file: it.File, frames: fr, dur: it.Dur}
		for i, h := range fr {
			if h != 0 {
				a.vid.add(mihEntry{fp: id, file: it.File, h: h, aux: uint64(i)})
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
	for i := 0; i < a.workers; i++ {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			for j := range a.jobs {
				a.active.Add(1)
				res := a.analyze(ctx, j)
				if ctx.Err() == nil || res.State != anFail {
					if err := a.commit(j, res); err != nil {
						log.Printf("Ähnlichkeit speichern (%s): %v", j.Loc, err)
					}
				}
				a.inflight.Delete(j.ID)
				a.active.Add(-1)
			}
		}()
	}
	a.dispatch(ctx)
	close(a.jobs)
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		log.Printf("Ähnlichkeit: Analysen beim Beenden abgebrochen (werden beim Neustart wiederholt)")
	}
	a.db.Close()
}

// dispatch sucht Dateien ohne Analyse und verteilt sie auf die Worker.
func (a *Analyzer) dispatch(ctx context.Context) {
	var cursor int64
	sleep := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	type row struct {
		id    int64
		loc   string
		kind  int
		size  int64
		fhash []byte
	}
	for ctx.Err() == nil {
		if a.e.paused.Load() {
			if !sleep(time.Second) {
				return
			}
			continue
		}
		rs, err := a.e.rdb.QueryContext(ctx, `SELECT f.id, f.loc, f.kind, f.size, f.fhash FROM files f
			WHERE f.id > ? AND f.size >= 0 AND f.state BETWEEN 1 AND 3
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
		var batch []row
		for rs.Next() {
			var r row
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
		now := time.Now().Unix()
		var quick [][3]int64 // file_id, state, mt
		for _, r := range batch {
			cursor = r.id
			if _, busy := a.inflight.Load(r.id); busy {
				continue
			}
			mt := classify(r.loc)
			needWhole := a.hasMembers.Load() && r.size >= minExact && a.memberWithSize(r.size)
			switch {
			case mt == mtNone && !needWhole:
				quick = append(quick, [3]int64{r.id, anSkip, mtNone})
				continue
			case mt != mtVideo && r.size > a.maxBytes:
				quick = append(quick, [3]int64{r.id, anTooBig, int64(mt)})
				continue
			}
			a.inflight.Store(r.id, true)
			select {
			case a.jobs <- anJob{ID: r.id, Loc: r.loc, Kind: r.kind, Size: r.size, FHash: r.fhash, MT: mt, NeedWhole: needWhole}:
			case <-ctx.Done():
				a.inflight.Delete(r.id)
				return
			}
		}
		if len(quick) > 0 {
			if err := a.markMany(quick, now); err != nil {
				log.Printf("Ähnlichkeit: %v", err)
			}
		}
	}
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
	for _, r := range rows {
		st.Exec(r[0], r[1], r[2], now)
	}
	st.Close()
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, r := range rows {
		a.cnt[r[1]].Add(1)
	}
	return nil
}

// ---------- Analyse einer Datei ----------

func (a *Analyzer) analyze(ctx context.Context, j anJob) (res anResult) {
	fail := func(err error) anResult { return anResult{State: anFail, Err: err.Error()} }
	ext := extOf(j.Loc)

	// Videos werden nicht heruntergeladen: ffmpeg springt per Range-Anfrage zu den Einzelbildern.
	if j.MT == mtVideo && !j.NeedWhole {
		src := j.Loc
		if j.Kind == kindHTTP && !a.rangeOK(ctx, j.Loc) {
			// Ohne Range-Unterstützung kann ffmpeg nicht springen und liefert stillschweigend
			// falsche Einzelbilder → dann komplett laden (sofern nicht größer als -sim-max).
			if j.Size > a.maxBytes {
				return fail(fmt.Errorf("Server kann keine Teil-Downloads und Video ist größer als -sim-max (%d MB)", a.maxBytes>>20))
			}
			file, _, cleanup, err := a.download(ctx, j.Loc, j.Size)
			if err != nil {
				return fail(err)
			}
			defer cleanup()
			src = file
		}
		vf, err := a.t.videoFingerprint(ctx, src, a.e.cfg.UserAgent, a.frames)
		if err != nil {
			return fail(err)
		}
		return anResult{State: anDone, Items: []fpItem{{File: j.ID, Kind: fpVideo, Size: j.Size, Dur: vf.Dur,
			Extra: framesBlob(vf.Frames), Thumb: vf.Thumb}}}
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
	case mtVideo: // nur hier, wenn auch ein exakter Abgleich nötig war (Datei liegt schon vor)
		if vf, err := a.t.videoFingerprint(ctx, file, a.e.cfg.UserAgent, a.frames); err == nil {
			items = append(items, fpItem{File: j.ID, Kind: fpVideo, Size: j.Size, Dur: vf.Dur, Extra: framesBlob(vf.Frames), Thumb: vf.Thumb})
		} else {
			errs = append(errs, err.Error())
		}
	case mtArchive:
		it, e2 := a.archive(ctx, j, file, ext, tmp)
		items = append(items, it...)
		if e2 != nil {
			errs = append(errs, e2.Error())
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
	res.Items = items
	res.Err = strings.Join(errs, "; ")
	return res
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
func (a *Analyzer) download(ctx context.Context, loc string, size int64) (string, []byte, func(), error) {
	noop := func() {}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	f := a.e.fet
	resp, err := f.req(ctx, http.MethodGet, loc, "")
	if err != nil {
		return "", nil, noop, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", nil, noop, fmt.Errorf("unerwarteter Status %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "wd-dl-*")
	if err != nil {
		return "", nil, noop, err
	}
	cleanup := func() { tmp.Close(); os.Remove(tmp.Name()) }
	t := time.AfterFunc(f.stall, cancel)
	defer t.Stop()
	h := sha256.New()
	body := &stallReader{r: resp.Body, t: t, d: f.stall, b: f.bytes}
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, a.maxBytes+1))
	if err == nil && n > a.maxBytes {
		err = fmt.Errorf("größer als -sim-max (%d MB)", a.maxBytes>>20)
	}
	if err == nil && size >= 0 && n != size {
		err = errChanged
	}
	if err != nil {
		cleanup()
		if ctx.Err() != nil && !errors.Is(err, errChanged) {
			err = fmt.Errorf("Download abgebrochen (>%s ohne Daten oder Programmende)", f.stall)
		}
		return "", nil, noop, err
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
			return []fpItem{{File: file, Sub: sub, Kind: fpText, H1: f.Sim, Size: int64(len(b)), Words: f.Words, Snip: f.Snip, CS: cs, Ex: exact}}, nil
		}
		return nil, nil
	case mtOffice:
		ra := bytes.NewReader(b)
		var out []fpItem
		s, err := officeText(ra, int64(len(b)))
		if err == nil {
			if f, ok := textFingerprint(s); ok {
				out = append(out, fpItem{File: file, Sub: sub, Kind: fpText, H1: f.Sim, Size: int64(len(b)), Words: f.Words, Snip: f.Snip, CS: cs, Ex: exact})
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
			out = append(out, fpItem{File: file, Sub: sub, Kind: fpText, H1: f.Sim, Size: size, Words: f.Words, Snip: f.Snip})
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
func (a *Analyzer) archive(ctx context.Context, j anJob, file, ext, tmp string) ([]fpItem, error) {
	var out []fpItem
	type mem struct {
		sha  []byte
		size int64
	}
	var members []mem
	err := walkArchive(file, ext, func(name string, size int64, r io.Reader) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h := sha256.New()
		mt := classify(name)
		var buf []byte
		if mt != mtNone && mt != mtVideo && mt != mtArchive && size <= memBufMax {
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
		return nil, fmt.Errorf("Archiv: %v", err)
	}
	// Gegenstücke im Netz: einzelne Dateien mit gleicher Größe exakt prüfen
	for _, m := range members {
		out = append(out, a.counterparts(ctx, j.ID, m.sha, m.size)...)
	}
	return out, nil
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

func (a *Analyzer) commit(j anJob, res anResult) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()

	insFP, err := tx.Prepare(`INSERT INTO fps(file_id, sub, kind, h1, h2, size, w, h, dur, words, snip, extra, cs, ex)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insFP.Close()
	type added struct {
		id int64
		it fpItem
	}
	var news []added
	var pairs []struct {
		a, b int64
		pairCand
	}
	addPair := func(x int64, c pairCand) {
		pairs = append(pairs, struct {
			a, b int64
			pairCand
		}{min(x, c.other), max(x, c.other), c})
	}
	for _, it := range res.Items {
		if it.Kind == fpWhole {
			// höchstens ein exakter Fingerabdruck pro Datei (kann vom Archiv-Abgleich oder der Datei selbst kommen)
			var id int64
			if tx.QueryRow(`SELECT id FROM fps WHERE file_id=? AND kind=5`, it.File).Scan(&id) == nil {
				continue
			}
		}
		var w, h, words any
		var dur any
		if it.W > 0 {
			w, h = it.W, it.H
		}
		if it.Words > 0 {
			words = it.Words
		}
		if it.Dur > 0 {
			dur = it.Dur
		}
		var snip any
		if it.Snip != "" {
			snip = it.Snip
		}
		var cs any
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
		news = append(news, added{id, it})
	}

	// Vergleich gegen alles Bisherige (und gegen die übrigen neuen Einträge dieser Datei)
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
			a.txt.near(n.it.H1, txtMax, func(x mihEntry, d int) {
				if x.file != n.it.File && !sameBytes(x, n.it) {
					cands = append(cands, pairCand{x.fp, 1 - float64(d)/64, pkText})
				}
			})
		case fpVideo:
			fr := blobFrames(n.it.Extra)
			seen := map[int64]bool{}
			for _, h := range fr {
				if h == 0 {
					continue
				}
				a.vid.near(h, 7, func(x mihEntry, d int) {
					if seen[x.fp] || x.file == n.it.File {
						return
					}
					seen[x.fp] = true
					v := a.vidMeta[x.fp]
					if s, ok := videoScore(fr, v.frames, n.it.Dur, v.dur); ok {
						cands = append(cands, pairCand{x.fp, s, pkVideo})
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
				// zwei einzelne Dateien sind Sache der Stufen 1–3; Einträge im selben Archiv ignorieren
				if string(ex) != string(n.it.Extra) || (kind == fpWhole && n.it.Kind == fpWhole) ||
					(file == n.it.File && kind == fpMember && n.it.Kind == fpMember) {
					continue
				}
				cands = append(cands, pairCand{id, 1, pkArchive})
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

	var newPairs [5]int64
	for _, p := range pairs {
		r, err := tx.Exec(`INSERT OR IGNORE INTO pairs(kind, a, b, score, found) VALUES(?,?,?,?,?)`, p.kind, p.a, p.b, p.score, now)
		if err != nil {
			return err
		}
		if c, _ := r.RowsAffected(); c > 0 {
			newPairs[p.kind]++
		}
	}
	var errv any
	if res.Err != "" {
		errv = res.Err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO analysis(file_id, state, mt, err, done) VALUES(?,?,?,?,?)`,
		j.ID, res.State, j.MT, errv, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// erst jetzt in den Suchindex (Einträge derselben Datei werden ohnehin nicht verglichen)
	for _, n := range news {
		a.index(n.id, n.it)
	}
	a.cnt[res.State].Add(1)
	for k, c := range newPairs {
		a.pairs[k].Add(c)
	}
	for _, n := range news {
		if n.it.Kind == fpMember {
			a.hasMembers.Store(true)
			break
		}
	}
	return nil
}

// ---------- Statistik ----------

type SimStats struct {
	Analyzed   int64    `json:"analyzed"`
	Done       int64    `json:"done"`
	Skipped    int64    `json:"skipped"`
	NoFP       int64    `json:"noFP"`
	TooBig     int64    `json:"tooBig"`
	Failed     int64    `json:"failed"`
	Pending    int64    `json:"pending"`
	Active     int32    `json:"active"`
	Pairs      int64    `json:"pairs"`
	PairsImage int64    `json:"pairsImage"`
	PairsText  int64    `json:"pairsText"`
	PairsVideo int64    `json:"pairsVideo"`
	PairsArch  int64    `json:"pairsArchive"`
	Missing    []string `json:"missing,omitempty"`
}

func (a *Analyzer) stats(base Stats) *SimStats {
	s := &SimStats{
		Done: a.cnt[anDone].Load(), Skipped: a.cnt[anSkip].Load(), NoFP: a.cnt[anNoFP].Load(),
		TooBig: a.cnt[anTooBig].Load(), Failed: a.cnt[anFail].Load(), Active: a.active.Load(),
		PairsImage: a.pairs[pkImage].Load(), PairsText: a.pairs[pkText].Load(),
		PairsVideo: a.pairs[pkVideo].Load(), PairsArch: a.pairs[pkArchive].Load(),
	}
	s.Analyzed = s.Done + s.Skipped + s.NoFP + s.TooBig + s.Failed
	s.Pairs = s.PairsImage + s.PairsText + s.PairsVideo + s.PairsArch
	// Dateien mit bekannter Größe, die noch keine Analyse haben (Näherung)
	s.Pending = max(0, base.Total-base.NeedProbe-base.Errors-s.Analyzed)
	if a.t.ffmpeg == "" || a.t.ffprobe == "" {
		s.Missing = append(s.Missing, "ffmpeg")
	}
	if a.t.pdftotext == "" || a.t.pdfimages == "" {
		s.Missing = append(s.Missing, "poppler-utils")
	}
	return s
}

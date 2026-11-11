package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	DB, Listen, Auth, Store, UserAgent string
	URLFiles, ScanDirs                 []string
	ProbeWorkers, FetchWorkers         int
	LocalWorkers, PerHost, Chunk       int
	ProbePerHost                       int
	Retries                            int
	Stall                              time.Duration
	Full                               bool
	Crawls                             []string
	Exts                               string
}

const (
	opProbe = iota
	opPartial
	opFull
)

type Job struct {
	ID    int64
	Loc   string
	Kind  int
	Size  int64
	State int
	Need  int
	Tries int
}

func (j Job) op() int {
	switch {
	case j.State == stNew:
		return opProbe
	case j.State == stProbed && j.Need == needPartial:
		return opPartial
	default:
		return opFull
	}
}

type Result struct {
	Job
	Op           int
	Size         int64
	PHash, FHash []byte
	Err          error
}

type AddItem struct {
	Loc  string
	Kind int
	Size int64 // -2 = unbekannt
}

type Engine struct {
	cfg  *Config
	db   *sql.DB // nur vom Core-Goroutine benutzt
	rdb  *sql.DB // Lesezugriffe (Web-UI)
	fet  *Fetcher
	addC chan []AddItem
	resC chan Result
	relC chan int64

	probeQ, fetchQ, localQ chan Job

	paused    atomic.Bool
	bytesRead atomic.Int64
	processed atomic.Int64
	scanning  atomic.Int32

	// Duplikat-Zähler (vom Core gepflegt)
	dupGroups, dupFiles, wasted atomic.Int64

	emptyP, emptyF []byte

	crawls crawlReg
	ctx    context.Context

	statsMu sync.RWMutex
	stats   Stats
	start   time.Time
}

func NewEngine(cfg *Config, w, r *sql.DB) *Engine {
	e := &Engine{
		cfg: cfg, db: w, rdb: r,
		addC:   make(chan []AddItem, 64),
		resC:   make(chan Result, 8192),
		relC:   make(chan int64, 8192),
		probeQ: make(chan Job, max(cfg.ProbeWorkers*16, 1024)),
		fetchQ: make(chan Job, max(cfg.FetchWorkers*8, 256)),
		localQ: make(chan Job, max(cfg.LocalWorkers*64, 1024)),
		start:  time.Now(),
		crawls: crawlReg{m: map[string]*CrawlStatus{}},
		ctx:    context.Background(),
	}
	e.fet = newFetcher(cfg, &e.bytesRead)
	e.emptyP = partialHash(0, nil, nil)
	h := sha256.Sum256(nil)
	e.emptyF = h[:]
	return e
}

func (e *Engine) Run(ctx context.Context) {
	if err := e.loadDupCounters(); err != nil {
		log.Fatalf("Zähler laden: %v", err)
	}
	var wg sync.WaitGroup
	spawn := func(n int, q chan Job) {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); e.worker(ctx, q) }()
		}
	}
	spawn(e.cfg.ProbeWorkers, e.probeQ)
	spawn(e.cfg.FetchWorkers, e.fetchQ)
	spawn(e.cfg.LocalWorkers, e.localQ)

	e.ctx = ctx
	go e.statsLoop(ctx)
	go e.resumeCrawls(ctx)
	for _, f := range e.cfg.URLFiles {
		go e.LoadURLFile(f)
	}
	for _, d := range e.cfg.ScanDirs {
		go e.ScanDir(d)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	e.core(ctx, done)
}

// ---------- Core: einziger Schreiber, trifft alle Entscheidungen ----------

func (e *Engine) core(ctx context.Context, workersDone <-chan struct{}) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	batch := make([]Result, 0, 4096)
	lastFlush := time.Now()
	flush := func() {
		lastFlush = time.Now()
		if len(batch) > 0 {
			if err := e.applyResults(batch); err != nil {
				log.Printf("DB-Fehler: %v", err)
			}
			batch = batch[:0]
		}
	}
	for {
		select {
		case r := <-e.resC:
			batch = append(batch, r)
		drain:
			for len(batch) < cap(batch) {
				select {
				case r := <-e.resC:
					batch = append(batch, r)
				default:
					break drain
				}
			}
			// große Transaktionen = schnell; aber nie länger als 50 ms warten
			if len(batch) >= 1024 || time.Since(lastFlush) > 50*time.Millisecond {
				flush()
				e.fill()
			}
		case items := <-e.addC:
			if err := e.insert(items); err != nil {
				log.Printf("Einfügen: %v", err)
			}
			e.fill()
		case id := <-e.relC:
			e.db.Exec(`UPDATE files SET busy=0 WHERE id=?`, id)
		case <-tick.C:
			flush()
			e.fill()
		case <-ctx.Done():
			close(e.probeQ)
			close(e.fetchQ)
			close(e.localQ)
			// Restliche Ergebnisse einsammeln, bis alle Worker beendet sind
			deadline := time.After(10 * time.Second)
			for {
				select {
				case r := <-e.resC:
					batch = append(batch, r)
				case <-workersDone:
					for len(e.resC) > 0 {
						batch = append(batch, <-e.resC)
					}
					flush()
					return
				case <-deadline:
					flush()
					return
				}
			}
		}
	}
}

func (e *Engine) fill() {
	if e.paused.Load() {
		return
	}
	e.fillQ(e.probeQ, kindHTTP, true, false)
	e.fillQ(e.fetchQ, kindHTTP, false, true)
	e.fillQ(e.localQ, kindLocal, true, true)
}

const jobCols = `SELECT id, loc, kind, size, state, need, tries FROM files `

func (e *Engine) fillQ(q chan Job, kind int, probe, fetch bool) {
	free := cap(q) - len(q)
	if free < cap(q)/4 {
		return
	}
	type qry struct {
		sql   string
		share int // max. Anteil der freien Plätze
	}
	var queries []qry
	if probe {
		queries = append(queries, qry{jobCols + `WHERE kind=? AND busy=0 AND state=0 LIMIT ?`, free})
	}
	if fetch {
		// Voll-Hashes bekommen garantiert die Hälfte der Plätze – sonst verhungern
		// sie hinter einem großen Rückstau an Teil-Hashes (gemessen: 72 Aufträge hingen fest).
		full := jobCols + `WHERE kind=? AND busy=0 AND state=2 AND need=2 LIMIT ?`
		queries = append(queries,
			qry{full, free / 2},
			qry{jobCols + `WHERE kind=? AND busy=0 AND state=1 AND need>=1 LIMIT ?`, free},
			qry{full, free}) // Rest wieder für Voll-Hashes, falls Teil-Hashes nicht alles füllen
	}
	var jobs []Job
	seen := map[int64]bool{}
	for _, q := range queries {
		n := min(q.share, free-len(jobs))
		if n <= 0 {
			continue
		}
		rows, err := e.db.Query(q.sql, kind, n)
		if err != nil {
			log.Printf("Abfrage: %v", err)
			return
		}
		for rows.Next() {
			var j Job
			rows.Scan(&j.ID, &j.Loc, &j.Kind, &j.Size, &j.State, &j.Need, &j.Tries)
			if !seen[j.ID] {
				seen[j.ID] = true
				jobs = append(jobs, j)
			}
		}
		rows.Close()
	}
	if len(jobs) == 0 {
		return
	}
	tx, err := e.db.Begin()
	if err != nil {
		return
	}
	st, _ := tx.Prepare(`UPDATE files SET busy=1 WHERE id=?`)
	for _, j := range jobs {
		st.Exec(j.ID)
	}
	st.Close()
	if err := tx.Commit(); err != nil {
		log.Printf("Commit: %v", err)
		return
	}
	for _, j := range jobs {
		q <- j // blockiert nie: nur so viele wie frei sind
	}
}

// ---------- Worker ----------

func (e *Engine) worker(ctx context.Context, q chan Job) {
	for j := range q {
		r := Result{Job: j, Op: j.op(), Size: j.Size}
		var n int64
		switch {
		case j.Kind == kindLocal && r.Op == opProbe:
			st, err := os.Stat(j.Loc)
			if err != nil {
				r.Err = errPermanent{err}
			} else {
				r.Size = st.Size()
			}
		case j.Kind == kindLocal && r.Op == opPartial:
			r.PHash, r.FHash, n, r.Err = e.fet.PartialLocal(j.Loc, j.Size, &e.bytesRead)
		case j.Kind == kindLocal:
			r.PHash, r.FHash, n, r.Err = e.fet.FullLocal(j.Loc, j.Size, &e.bytesRead)
		case r.Op == opProbe:
			r.Size, r.Err = e.fet.Probe(ctx, j.Loc)
		case r.Op == opPartial:
			r.PHash, r.FHash, n, r.Err = e.fet.PartialHTTP(ctx, j.Loc, j.Size)
		default:
			r.PHash, r.FHash, n, r.Err = e.fet.FullHTTP(ctx, j.Loc, j.Size)
		}
		if r.Err == nil && r.FHash != nil {
			r.Op, r.Size = opFull, n
		}
		if ctx.Err() != nil {
			if r.Err != nil {
				return // Programmende: Auftrag bleibt offen und wird beim Neustart wiederholt
			}
			select {
			case e.resC <- r:
			default:
			}
			continue
		}
		e.resC <- r
	}
}

// ---------- Ergebnisse verbuchen + Eskalation ----------

type txs struct {
	tx                                       *sql.Tx
	setSize, setNeed, setPartial, setFull    *sql.Stmt
	cntSize, cntP, upSize, upP, needOwn, hIns *sql.Stmt
	fail, retry, reset                       *sql.Stmt
}

func (e *Engine) begin() (*txs, error) {
	tx, err := e.db.Begin()
	if err != nil {
		return nil, err
	}
	t := &txs{tx: tx}
	p := func(q string) *sql.Stmt {
		s, err2 := tx.Prepare(q)
		if err2 != nil && err == nil {
			err = err2
		}
		return s
	}
	t.setSize = p(`UPDATE files SET size=?, state=1, busy=0, err=NULL WHERE id=?`)
	t.setNeed = p(`UPDATE files SET need=? WHERE id=? AND need<?`)
	t.setPartial = p(`UPDATE files SET phash=?, state=2, busy=0, err=NULL WHERE id=?`)
	t.setFull = p(`UPDATE files SET size=?, phash=?, fhash=?, state=3, busy=0, err=NULL, done=? WHERE id=?`)
	t.cntSize = p(`SELECT count(*) FROM (SELECT 1 FROM files WHERE size=? AND state BETWEEN 1 AND 3 LIMIT 3)`)
	t.cntP = p(`SELECT count(*) FROM (SELECT 1 FROM files WHERE phash=? AND state BETWEEN 2 AND 3 LIMIT 3)`)
	t.upSize = p(`UPDATE files SET need=1 WHERE size=? AND need<1 AND state BETWEEN 1 AND 3`)
	t.upP = p(`UPDATE files SET need=2 WHERE phash=? AND need<2 AND state BETWEEN 2 AND 3`)
	t.hIns = p(`INSERT INTO hashes(fhash,cnt,size) VALUES(?,1,?) ON CONFLICT(fhash) DO UPDATE SET cnt=cnt+1 RETURNING cnt`)
	t.fail = p(`UPDATE files SET state=9, busy=0, err=?, tries=tries+1, done=? WHERE id=?`)
	t.retry = p(`UPDATE files SET err=?, tries=tries+1 WHERE id=?`)
	t.reset = p(`UPDATE files SET state=0, need=0, size=-2, phash=NULL, busy=0, err=?, tries=tries+1 WHERE id=?`)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	return t, nil
}

func (e *Engine) applyResults(rs []Result) error {
	t, err := e.begin()
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	type rel struct {
		id    int64
		tries int
	}
	var release []rel
	for _, r := range rs {
		if r.Err != nil {
			msg := r.Err.Error()
			switch {
			case errors.Is(r.Err, errChanged) && r.Tries < e.cfg.Retries:
				t.reset.Exec(msg, r.ID)
			case errors.As(r.Err, new(errPermanent)) || r.Tries+1 >= e.cfg.Retries:
				t.fail.Exec(msg, now, r.ID)
			default:
				// vorübergehender Fehler: nach Wartezeit erneut versuchen
				t.retry.Exec(msg, r.ID)
				release = append(release, rel{r.ID, r.Tries})
			}
			continue
		}
		switch r.Op {
		case opProbe:
			t.setSize.Exec(r.Size, r.ID)
			e.sizeKnown(t, r.ID, r.Kind, r.Size, now)
		case opPartial:
			t.setPartial.Exec(r.PHash, r.ID)
			e.escPartial(t, r.ID, r.PHash)
		case opFull:
			e.full(t, r.ID, r.Size, r.PHash, r.FHash, now)
			if r.Job.Size < 0 { // Größe war vorher unbekannt
				e.escSize(t, r.ID, r.Size)
			}
		}
	}
	if err := t.tx.Commit(); err != nil {
		return err
	}
	e.processed.Add(int64(len(rs)))
	for _, x := range release {
		id := x.id
		d := time.Duration(15*(x.tries+1)) * time.Second // Backoff
		time.AfterFunc(d, func() { e.relC <- id })
	}
	return nil
}

// sizeKnown: neue Datei mit bekannter Größe einordnen.
func (e *Engine) sizeKnown(t *txs, id int64, kind int, size, now int64) {
	switch {
	case size == 0:
		e.full(t, id, 0, e.emptyP, e.emptyF, now) // leere Dateien: kein Download nötig
	case size < 0 || e.cfg.Full || (e.cfg.Store != "" && kind == kindHTTP):
		t.setNeed.Exec(needFull, id, needFull)
	default:
		e.escSize(t, id, size)
	}
}

// escSize: Sobald zwei Dateien gleich groß sind, brauchen beide einen Teil-Hash.
// LIMIT 3 + "nur beim Übergang 1→2 die ganze Gruppe" hält das O(1) pro Datei,
// auch wenn Millionen Dateien dieselbe Größe haben.
func (e *Engine) escSize(t *txs, id, size int64) {
	var c int
	t.cntSize.QueryRow(size).Scan(&c)
	switch {
	case c == 2:
		t.upSize.Exec(size)
	case c > 2:
		t.setNeed.Exec(needPartial, id, needPartial)
	}
}

func (e *Engine) escPartial(t *txs, id int64, ph []byte) {
	var c int
	t.cntP.QueryRow(ph).Scan(&c)
	switch {
	case c == 2:
		t.upP.Exec(ph)
	case c > 2:
		t.setNeed.Exec(needFull, id, needFull)
	}
}

func (e *Engine) full(t *txs, id, size int64, ph, fh []byte, now int64) {
	t.setFull.Exec(size, ph, fh, now, id)
	e.escPartial(t, id, ph)
	var cnt int64
	if err := t.hIns.QueryRow(fh, size).Scan(&cnt); err != nil {
		log.Printf("hashes: %v", err)
		return
	}
	switch {
	case cnt == 2:
		e.dupGroups.Add(1)
		e.dupFiles.Add(2)
		e.wasted.Add(size)
	case cnt > 2:
		e.dupFiles.Add(1)
		e.wasted.Add(size)
	}
}

func (e *Engine) loadDupCounters() error {
	var g, f, w int64
	err := e.db.QueryRow(`SELECT count(*), coalesce(sum(cnt),0), coalesce(sum((cnt-1)*size),0) FROM hashes WHERE cnt>1`).Scan(&g, &f, &w)
	e.dupGroups.Store(g)
	e.dupFiles.Store(f)
	e.wasted.Store(w)
	return err
}

// ---------- Neue Dateien aufnehmen ----------

func (e *Engine) insert(items []AddItem) error {
	t, err := e.begin()
	if err != nil {
		return err
	}
	ins, err := t.tx.Prepare(`INSERT INTO files(loc,kind,size,state,added) VALUES(?,?,?,?,?) ON CONFLICT(loc) DO NOTHING RETURNING id`)
	if err != nil {
		t.tx.Rollback()
		return err
	}
	now := time.Now().Unix()
	for _, it := range items {
		state := stNew
		if it.Size >= 0 {
			state = stProbed
		}
		var id int64
		if err := ins.QueryRow(it.Loc, it.Kind, it.Size, state, now).Scan(&id); err != nil {
			continue // existiert bereits
		}
		if it.Size >= 0 {
			e.sizeKnown(t, id, it.Kind, it.Size, now)
		}
	}
	return t.tx.Commit()
}

// Add nimmt Text mit URLs/Pfaden entgegen (eine pro Zeile).
func (e *Engine) Add(r io.Reader) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	batch := make([]AddItem, 0, 2000)
	n := 0
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if !strings.HasPrefix(l, "http://") && !strings.HasPrefix(l, "https://") {
			continue
		}
		batch = append(batch, AddItem{Loc: l, Kind: kindHTTP, Size: -2})
		n++
		if len(batch) == cap(batch) {
			e.addC <- batch
			batch = make([]AddItem, 0, 2000)
		}
	}
	if len(batch) > 0 {
		e.addC <- batch
	}
	return n, sc.Err()
}

func (e *Engine) LoadURLFile(p string) {
	var r io.Reader = os.Stdin
	if u, ok := strings.CutPrefix(p, "inline:"); ok {
		r, p = strings.NewReader(u), u
	} else if p != "-" {
		f, err := os.Open(p)
		if err != nil {
			log.Printf("URL-Liste: %v", err)
			return
		}
		defer f.Close()
		r = f
	}
	n, err := e.Add(r)
	log.Printf("URL-Liste %s: %d URLs eingelesen (Fehler: %v)", p, n, err)
}

// ScanDir nimmt lokale Dateien rekursiv auf (Größe kommt gratis vom Dateisystem).
func (e *Engine) ScanDir(root string) {
	e.scanning.Add(1)
	defer e.scanning.Add(-1)
	root, _ = filepath.Abs(root)
	batch := make([]AddItem, 0, 2000)
	n := 0
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		batch = append(batch, AddItem{Loc: p, Kind: kindLocal, Size: info.Size()})
		n++
		if len(batch) == cap(batch) {
			e.addC <- batch
			batch = make([]AddItem, 0, 2000)
		}
		return nil
	})
	if len(batch) > 0 {
		e.addC <- batch
	}
	log.Printf("Ordner %s: %d Dateien aufgenommen", root, n)
}

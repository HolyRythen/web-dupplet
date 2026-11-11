package main

// Gruppen ähnlicher Inhalte + Behalten-Vorschlag + Aufräum-Export.
//
// Aus den Paaren (A≈B, B≈C, …) werden Gruppen (Zusammenhangskomponenten). Ein Knoten ist eine ganze Datei
// oder ein Eintrag in einer Datei (Archiv-Eintrag, Bild in PDF/Office). Pro Gruppe wird ein Exemplar zum
// Behalten vorgeschlagen; der Benutzer kann es ändern (Tabelle "keep"). Löschbar sind nur ganze Dateien.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type GNode struct {
	FP        int64   `json:"fp"`
	File      int64   `json:"file"`
	Sub       string  `json:"sub,omitempty"`
	Loc       string  `json:"loc"`
	Local     bool    `json:"local"`
	MT        int     `json:"mt"`
	Size      int64   `json:"size"`
	W         int     `json:"w,omitempty"`
	H         int     `json:"h,omitempty"`
	Dur       float64 `json:"dur,omitempty"`
	Words     int     `json:"words,omitempty"`
	Snip      string  `json:"snip,omitempty"`
	Thumb     int64   `json:"thumb,omitempty"`
	Keep      bool    `json:"keep"`
	Manual    bool    `json:"manual,omitempty"`
	Removable bool    `json:"removable"`      // ganze Datei (nicht in Archiv/PDF eingebettet)
	Same      bool    `json:"same,omitempty"` // byte-gleich zu einer anderen Datei der Gruppe → zählt bei „sicher“
	fh        string
}

type Group struct {
	ID       int     `json:"id"`
	Kind     int     `json:"kind"`
	MinScore float64 `json:"minScore"`
	MaxScore float64 `json:"maxScore"`
	Savings  int64   `json:"savings"`
	Nodes    []GNode `json:"nodes"`
}

type groupSet struct {
	groups      []Group
	savings     int64 // ähnliche Inhalte: Summe der löschbaren Nicht-Behalten-Dateien
	safeSavings int64 // byte-gleiche Duplikate (Stufen 1–3)
	built       time.Time
}

type groupCache struct {
	mu  sync.RWMutex
	g   groupSet
	now chan struct{}
}

func (c *groupCache) get() groupSet {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.g
}

// groupLoop baut die Gruppen neu, sobald sich Paare oder „behalten“-Wahlen geändert haben (höchstens alle 15 s).
func (a *Analyzer) groupLoop(ctx context.Context) {
	last := int64(-1)
	tk := time.NewTicker(15 * time.Second)
	defer tk.Stop()
	for {
		if ch := a.changed.Load(); ch != last {
			if g, err := a.buildGroups(ctx); err == nil {
				a.grp.mu.Lock()
				a.grp.g = g
				a.grp.mu.Unlock()
				last = ch
			} else if ctx.Err() == nil {
				log.Printf("Gruppen: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		case <-a.grpNow():
		}
	}
}

func (a *Analyzer) grpNow() chan struct{} {
	a.grp.mu.Lock()
	defer a.grp.mu.Unlock()
	if a.grp.now == nil {
		a.grp.now = make(chan struct{}, 1)
	}
	return a.grp.now
}

func (a *Analyzer) rebuildNow() {
	a.changed.Add(1)
	select {
	case a.grpNow() <- struct{}{}:
	default:
	}
}

type nodeKey struct {
	file int64
	sub  string
}

// textRank: „vollständigeres“ Format bevorzugen, wenn derselbe Text mehrfach vorliegt.
var textRank = map[string]int{"pdf": 6, "docx": 5, "docm": 5, "odt": 5, "pptx": 5, "odp": 5, "xlsx": 5, "ods": 5,
	"epub": 4, "html": 4, "htm": 4, "xhtml": 4, "rtf": 3, "tex": 3, "md": 3, "markdown": 3, "xml": 2, "txt": 2}

func (a *Analyzer) buildGroups(ctx context.Context) (groupSet, error) {
	var gs groupSet
	db := a.e.rdb
	// 1. Paare laden → Union-Find über Knoten (Datei, Eintrag)
	rows, err := db.QueryContext(ctx, `SELECT p.kind, p.score, x.id, x.file_id, x.sub, y.id, y.file_id, y.sub
		FROM pairs p JOIN fps x ON x.id = p.a JOIN fps y ON y.id = p.b`)
	if err != nil {
		return gs, err
	}
	idx := map[nodeKey]int{}
	var keys []nodeKey
	var rep []int64 // ein Fingerabdruck je Knoten (für Details)
	var parent []int
	node := func(k nodeKey, fp int64) int {
		if i, ok := idx[k]; ok {
			return i
		}
		i := len(keys)
		idx[k] = i
		keys = append(keys, k)
		rep = append(rep, fp)
		parent = append(parent, i)
		return i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	type edge struct {
		x, y, kind int
		score      float64
	}
	var edges []edge
	for rows.Next() {
		var kind int
		var score float64
		var fa, fileA, fb, fileB int64
		var sa, sb string
		rows.Scan(&kind, &score, &fa, &fileA, &sa, &fb, &fileB, &sb)
		x, y := node(nodeKey{fileA, sa}, fa), node(nodeKey{fileB, sb}, fb)
		edges = append(edges, edge{x, y, kind, score})
		if rx, ry := find(x), find(y); rx != ry {
			parent[rx] = ry
		}
	}
	rows.Close()

	// 2. Details je Knoten (Inhalts-Fingerabdruck bevorzugt: hat Maße, Vorschau, Textanfang)
	nodes := make([]GNode, len(keys))
	files := map[int64]bool{}
	for i, k := range keys {
		nodes[i] = GNode{FP: rep[i], File: k.file, Sub: k.sub, Removable: k.sub == ""}
		files[k.file] = true
	}
	stmt, err := db.PrepareContext(ctx, `SELECT p.id, p.kind, p.size, coalesce(p.w,0), coalesce(p.h,0), coalesce(p.dur,0),
		coalesce(p.words,0), coalesce(p.snip,''), (SELECT count(*) FROM thumbs t WHERE t.fp = p.id)
		FROM fps p WHERE p.file_id = ? AND p.sub = ? ORDER BY (p.kind IN (4,5)), p.id`)
	if err != nil {
		return gs, err
	}
	for i := range nodes {
		n := &nodes[i]
		r, err := stmt.QueryContext(ctx, n.File, n.Sub)
		if err != nil {
			stmt.Close()
			return gs, err
		}
		first := true
		for r.Next() {
			var id, size int64
			var kind, w, h, words, th int
			var dur float64
			var snip string
			r.Scan(&id, &kind, &size, &w, &h, &dur, &words, &snip, &th)
			if first {
				n.FP, n.Size = id, size
				first = false
			}
			if n.W == 0 && w > 0 {
				n.W, n.H = w, h
			}
			if n.Dur == 0 && dur > 0 {
				n.Dur = dur
			}
			if n.Words == 0 && words > 0 {
				n.Words, n.Snip = words, snip
			}
			if n.Thumb == 0 && th > 0 {
				n.Thumb = id
			}
		}
		r.Close()
	}
	stmt.Close()
	fst, err := db.PrepareContext(ctx, `SELECT f.loc, f.kind, f.size, coalesce(a.mt, 0), coalesce(f.fhash, x'') FROM files f LEFT JOIN analysis a ON a.file_id = f.id WHERE f.id = ?`)
	if err != nil {
		return gs, err
	}
	type finfo struct {
		loc      string
		kind, mt int
		size     int64
		fh       []byte
	}
	fi := map[int64]finfo{}
	for f := range files {
		var x finfo
		fst.QueryRowContext(ctx, f).Scan(&x.loc, &x.kind, &x.size, &x.mt, &x.fh)
		fi[f] = x
	}
	fst.Close()
	for i := range nodes {
		n := &nodes[i]
		x := fi[n.File]
		n.Loc, n.Local, n.MT = x.loc, x.kind == kindLocal, x.mt
		if n.Sub == "" {
			n.Size = x.size // ganze Datei: echte Dateigröße
			n.fh = string(x.fh)
		} else {
			n.MT = classify(n.Sub)
		}
	}
	// byte-gleiche Kopien (nicht analysiert) übernehmen Vorschau und Maße ihres Zwillings
	twin := map[string]GNode{}
	for _, n := range nodes {
		if n.fh != "" && n.Thumb != 0 {
			twin[n.fh] = n
		}
	}
	for i := range nodes {
		n := &nodes[i]
		if t, ok := twin[n.fh]; ok && n.Thumb == 0 {
			n.Thumb, n.W, n.H, n.Dur, n.Words, n.Snip = t.Thumb, t.W, t.H, t.Dur, t.Words, t.Snip
		}
	}
	keep := map[nodeKey]bool{}
	if kr, err := db.QueryContext(ctx, `SELECT file_id, sub FROM keep`); err == nil {
		for kr.Next() {
			var k nodeKey
			kr.Scan(&k.file, &k.sub)
			keep[k] = true
		}
		kr.Close()
	}

	// 3. Komponenten bilden
	comp := map[int][]int{}
	for i := range nodes {
		r := find(i)
		comp[r] = append(comp[r], i)
	}
	type gstat struct {
		kinds      [pkKinds]int
		minS, maxS float64
	}
	st := map[int]*gstat{}
	for _, e := range edges {
		r := find(e.x)
		s := st[r]
		if s == nil {
			s = &gstat{minS: 1, maxS: 0}
			st[r] = s
		}
		s.kinds[e.kind]++
		s.minS, s.maxS = min(s.minS, e.score), max(s.maxS, e.score)
	}
	for r, members := range comp {
		s := st[r]
		kind, best := 0, -1
		for k, c := range s.kinds {
			if c > best && c > 0 && k != pkArchive {
				kind, best = k, c
			}
		}
		if kind == 0 {
			kind = pkArchive
		}
		g := Group{Kind: kind, MinScore: s.minS, MaxScore: s.maxS}
		for _, i := range members {
			n := nodes[i]
			n.Manual = keep[nodeKey{n.File, n.Sub}]
			g.Nodes = append(g.Nodes, n)
		}
		chooseKeeper(&g)
		gs.savings += g.Savings
		gs.groups = append(gs.groups, g)
	}
	sort.Slice(gs.groups, func(i, j int) bool {
		if gs.groups[i].Savings != gs.groups[j].Savings {
			return gs.groups[i].Savings > gs.groups[j].Savings
		}
		return gs.groups[i].MaxScore > gs.groups[j].MaxScore
	})
	for i := range gs.groups {
		gs.groups[i].ID = i + 1
	}
	db.QueryRowContext(ctx, `SELECT coalesce(sum((cnt-1)*size),0) FROM hashes WHERE cnt>1`).Scan(&gs.safeSavings)
	gs.built = time.Now()
	return gs, nil
}

// chooseKeeper markiert das zu behaltende Exemplar und berechnet die mögliche Ersparnis.
func chooseKeeper(g *Group) {
	quality := func(n GNode) float64 {
		switch g.Kind {
		case pkImage:
			return float64(n.W) * float64(n.H)
		case pkVideo, pkAudio:
			if n.Dur > 0 {
				return float64(n.Size) / n.Dur // Bitrate als Qualitätsmaß
			}
		case pkText:
			ext := extOf(n.Loc)
			if n.Sub != "" {
				ext = extOf(n.Sub)
			}
			return float64(textRank[ext])*1e9 + float64(n.Words)
		}
		return 0
	}
	better := func(x, y GNode) bool { // ist x besser als y?
		if x.Manual != y.Manual {
			return x.Manual
		}
		if g.Kind == pkArchive {
			// exakt gleich, einmal verpackt: das Archiv behalten, die lose Kopie kann weg
			if x.Removable != y.Removable {
				return !x.Removable
			}
		} else if x.Removable != y.Removable {
			return x.Removable // eine ganze Datei behalten (Eingebettetes lässt sich nicht einzeln behalten)
		}
		if qx, qy := quality(x), quality(y); qx != qy {
			return qx > qy
		}
		if x.Local != y.Local {
			return x.Local
		}
		if ex, ey := extOf(x.Loc) != "", extOf(y.Loc) != ""; ex != ey {
			return ex // sprechender Dateiname mit Endung
		}
		if x.Size != y.Size {
			return x.Size > y.Size
		}
		return x.File < y.File
	}
	k := 0
	for i := 1; i < len(g.Nodes); i++ {
		if better(g.Nodes[i], g.Nodes[k]) {
			k = i
		}
	}
	// Ersparnis: jede überflüssige ganze Datei einmal; byte-gleiche Kopien (gleicher SHA-256) zählen schon
	// bei den sicheren Duplikaten und werden hier nicht noch einmal gerechnet.
	g.Savings = 0
	seen := map[string]bool{}
	if g.Nodes[k].fh != "" {
		seen[g.Nodes[k].fh] = true
	}
	for i := range g.Nodes {
		g.Nodes[i].Keep = i == k
		if i == k || !g.Nodes[i].Removable {
			continue
		}
		if fh := g.Nodes[i].fh; fh != "" {
			if seen[fh] {
				g.Nodes[i].Same = true
				continue
			}
			seen[fh] = true
		}
		g.Savings += g.Nodes[i].Size
	}
	// Behalten zuerst, danach nach Größe
	sort.SliceStable(g.Nodes, func(i, j int) bool {
		if g.Nodes[i].Keep != g.Nodes[j].Keep {
			return g.Nodes[i].Keep
		}
		return g.Nodes[i].Size > g.Nodes[j].Size
	})
}

// SetKeep merkt sich das zu behaltende Exemplar einer Gruppe (andere Wahlen derselben Gruppe entfallen).
func (a *Analyzer) SetKeep(file int64, sub string) error {
	g := a.grp.get()
	var others []nodeKey
	for _, gr := range g.groups {
		hit := false
		for _, n := range gr.Nodes {
			if n.File == file && n.Sub == sub {
				hit = true
			}
		}
		if hit {
			for _, n := range gr.Nodes {
				others = append(others, nodeKey{n.File, n.Sub})
			}
			break
		}
	}
	a.mu.Lock()
	tx, err := a.db.Begin()
	if err != nil {
		a.mu.Unlock()
		return err
	}
	for _, o := range others {
		tx.Exec(`DELETE FROM keep WHERE file_id=? AND sub=?`, o.file, o.sub)
	}
	tx.Exec(`INSERT OR REPLACE INTO keep(file_id, sub) VALUES(?,?)`, file, sub)
	err = tx.Commit()
	a.mu.Unlock()
	if err != nil {
		return err
	}
	// sofort neu bauen, damit die Antwort schon stimmt
	if gs, err := a.buildGroups(context.Background()); err == nil {
		a.grp.mu.Lock()
		a.grp.g = gs
		a.grp.mu.Unlock()
	}
	a.rebuildNow()
	return nil
}

// ---------- Aufräum-Export ----------

type cleanupRow struct {
	safe   bool
	group  string
	kind   string
	action string // behalten / löschen / bleibt
	score  float64
	loc    string
	sub    string
	size   int64
	local  bool
}

var pkName = map[int]string{pkImage: "Bild", pkText: "Text", pkVideo: "Video", pkAudio: "Audio", pkArchive: "verpackt"}

// cleanupRows liefert zuerst die sicheren (byte-gleichen) Duplikate, dann die ähnlichen Gruppen.
func (e *Engine) cleanupRows(ctx context.Context, fn func(cleanupRow) error) error {
	keep := map[nodeKey]bool{}
	if kr, err := e.rdb.QueryContext(ctx, `SELECT file_id, sub FROM keep WHERE sub=''`); err == nil {
		for kr.Next() {
			var k nodeKey
			kr.Scan(&k.file, &k.sub)
			keep[k] = true
		}
		kr.Close()
	}
	rows, err := e.rdb.QueryContext(ctx, `SELECT h.fhash, f.id, f.loc, f.kind, f.size FROM hashes h
		JOIN files f ON f.fhash = h.fhash WHERE h.cnt > 1 ORDER BY (h.cnt-1)*h.size DESC, h.fhash, f.kind DESC, f.id`)
	if err != nil {
		return err
	}
	type ex struct {
		id    int64
		loc   string
		local bool
		size  int64
	}
	var cur []ex
	var curH string
	n := 0
	flush := func() error {
		if len(cur) < 2 {
			cur = cur[:0]
			return nil
		}
		n++
		k := 0 // Reihenfolge: lokal zuerst, dann kleinste ID – oder die vom Benutzer gewählte
		for i, x := range cur {
			if keep[nodeKey{x.id, ""}] {
				k = i
			}
		}
		for i, x := range cur {
			act := "löschen"
			if i == k {
				act = "behalten"
			}
			if err := fn(cleanupRow{safe: true, group: fmt.Sprintf("D%d", n), kind: "byte-gleich", action: act, score: 1,
				loc: x.loc, size: x.size, local: x.local}); err != nil {
				return err
			}
		}
		cur = cur[:0]
		return nil
	}
	for rows.Next() {
		var h []byte
		var x ex
		var kind int
		rows.Scan(&h, &x.id, &x.loc, &kind, &x.size)
		x.local = kind == kindLocal
		if string(h) != curH {
			if err := flush(); err != nil {
				rows.Close()
				return err
			}
			curH = string(h)
		}
		cur = append(cur, x)
	}
	rows.Close()
	if err := flush(); err != nil {
		return err
	}
	if e.an == nil {
		return nil
	}
	for _, g := range e.an.grp.get().groups {
		for _, nd := range g.Nodes {
			act := "löschen"
			switch {
			case nd.Keep:
				act = "behalten"
			case !nd.Removable:
				act = "bleibt (eingebettet)"
			}
			if err := fn(cleanupRow{group: fmt.Sprintf("G%d", g.ID), kind: pkName[g.Kind], action: act, score: g.MinScore,
				loc: nd.Loc, sub: nd.Sub, size: nd.Size, local: nd.Local}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) writeCleanupCSV(ctx context.Context, w io.Writer) error {
	cw := csv.NewWriter(w)
	cw.Write([]string{"sicherheit", "gruppe", "art", "aktion", "aehnlichkeit_min_prozent", "ort", "eintrag", "groesse_bytes"})
	err := e.cleanupRows(ctx, func(r cleanupRow) error {
		s := "prüfen (ähnlich)"
		if r.safe {
			s = "sicher (byte-gleich)"
		}
		return cw.Write([]string{s, r.group, r.kind, r.action, strconv.FormatFloat(r.score*100, 'f', 1, 64), r.loc, r.sub, strconv.FormatInt(r.size, 10)})
	})
	cw.Flush()
	return err
}

// shellQuote setzt einen Pfad sicher in einfache Anführungszeichen.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// writeCleanupScript erzeugt ein Bash-Skript, das überflüssige LOKALE Kopien in einen Ordner verschiebt.
// Byte-gleiche Duplikate sind aktiv, ähnliche Gruppen auskommentiert (bitte erst prüfen).
// Standard ist ein Probelauf; erst AUSFUEHREN=1 verschiebt wirklich. Gelöscht wird nichts.
func (e *Engine) writeCleanupScript(ctx context.Context, w io.Writer) error {
	fmt.Fprintf(w, `#!/bin/bash
# Web-Dupplet – Aufräum-Skript (erzeugt %s)
#
# Verschiebt überflüssige Kopien LOKALER Dateien nach $ZIEL – es wird nichts gelöscht.
#   Probelauf (Standard):   bash aufraeumen.sh
#   Wirklich verschieben:   AUSFUEHREN=1 bash aufraeumen.sh
#   Anderer Zielordner:     ZIEL=/mnt/papierkorb AUSFUEHREN=1 bash aufraeumen.sh
#
# Abschnitt 1 (aktiv):        byte-gleiche Duplikate – sicher, das behaltene Exemplar bleibt liegen.
# Abschnitt 2 (auskommentiert): ähnliche Inhalte – bitte prüfen und gewünschte Zeilen einkommentieren.
# Jede Zeile prüft vorher, ob die Datei noch existiert und dieselbe Größe hat.
set -uo pipefail
ZIEL="${ZIEL:-./aussortiert}"
n=0; frei=0
weg() {
  local groesse="$1" pfad="$2"
  if [ ! -f "$pfad" ]; then echo "fehlt:     $pfad"; return; fi
  if [ "$(stat -c %%s -- "$pfad")" != "$groesse" ]; then echo "geändert:  $pfad (übersprungen)"; return; fi
  if [ "${AUSFUEHREN:-0}" = 1 ]; then
    mkdir -p -- "$ZIEL$(dirname -- "$pfad")" && mv -n -- "$pfad" "$ZIEL$pfad" && echo "verschoben: $pfad"
  else
    echo "würde verschieben: $pfad"
  fi
  n=$((n+1)); frei=$((frei+groesse))
}
`, time.Now().Format("02.01.2006 15:04"))
	section := 0
	last := ""
	err := e.cleanupRows(ctx, func(r cleanupRow) error {
		if !r.local {
			return nil
		}
		if r.safe && section == 0 {
			section = 1
			fmt.Fprintln(w, "\n# ===== 1. Byte-gleiche Duplikate (sicher) =====")
		}
		if !r.safe && section < 2 {
			section = 2
			fmt.Fprintln(w, "\n# ===== 2. Ähnliche Inhalte (bitte prüfen, zum Verschieben „# “ entfernen) =====")
		}
		if r.group != last {
			last = r.group
			fmt.Fprintf(w, "\n# Gruppe %s (%s)\n", r.group, r.kind)
		}
		switch {
		case r.action == "behalten":
			fmt.Fprintf(w, "#   behalten: %s\n", r.loc)
		case r.sub != "" || r.action != "löschen":
			fmt.Fprintf(w, "#   bleibt:   %s › %s\n", r.loc, r.sub)
		case r.safe:
			fmt.Fprintf(w, "weg %d %s\n", r.size, shellQuote(r.loc))
		default:
			fmt.Fprintf(w, "# weg %d %s\n", r.size, shellQuote(r.loc))
		}
		return nil
	})
	fmt.Fprintln(w, `
echo
echo "$n Dateien, $((frei/1024/1024)) MB"
[ "${AUSFUEHREN:-0}" = 1 ] || echo "Das war ein Probelauf. Zum Verschieben: AUSFUEHREN=1 bash $0"`)
	return err
}

// ---------- Web-API ----------

func (e *Engine) groupHandlers(mux *http.ServeMux) {
	need := func(w http.ResponseWriter) bool {
		if e.an == nil {
			http.Error(w, "Ähnlichkeitsanalyse ist aus (Option -similar)", 404)
			return false
		}
		return true
	}

	// GET /api/clusters?kind=0..5&offset=&limit= – Gruppen, größte Ersparnis zuerst
	mux.HandleFunc("GET /api/clusters", func(w http.ResponseWriter, r *http.Request) {
		if !need(w) {
			return
		}
		q := r.URL.Query()
		kind := atoi(q.Get("kind"), 0)
		limit := clamp(atoi(q.Get("limit"), 20), 1, 200)
		offset := max(atoi(q.Get("offset"), 0), 0)
		g := e.an.grp.get()
		var sel []Group
		for _, x := range g.groups {
			if kind == 0 || x.Kind == kind {
				sel = append(sel, x)
			}
		}
		counts := map[string]int{"0": len(g.groups)}
		for _, x := range g.groups {
			counts[strconv.Itoa(x.Kind)]++
		}
		total := len(sel)
		if offset > len(sel) {
			offset = len(sel)
		}
		sel = sel[offset:min(len(sel), offset+limit)]
		if sel == nil {
			sel = []Group{}
		}
		writeJSON(w, map[string]any{"groups": sel, "total": total, "counts": counts, "savings": g.savings, "safeSavings": g.safeSavings,
			"built": g.built.Unix()})
	})

	// POST /api/keep {"file":1,"sub":""} – dieses Exemplar behalten
	mux.HandleFunc("POST /api/keep", func(w http.ResponseWriter, r *http.Request) {
		if !need(w) {
			return
		}
		var req struct {
			File int64  `json:"file"`
			Sub  string `json:"sub"`
		}
		if err := decodeJSON(r, &req); err != nil || req.File <= 0 {
			http.Error(w, "file fehlt", 400)
			return
		}
		if err := e.an.SetKeep(req.File, req.Sub); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	// GET /api/arcrels – Archive mit gemeinsamen Einträgen
	mux.HandleFunc("GET /api/arcrels", func(w http.ResponseWriter, r *http.Request) {
		if !need(w) {
			return
		}
		limit := clamp(atoi(r.URL.Query().Get("limit"), 20), 1, 200)
		offset := max(atoi(r.URL.Query().Get("offset"), 0), 0)
		rows, err := e.rdb.Query(`SELECT r.shared, r.shared_bytes, fa.loc, fb.loc, coalesce(ia.members,0), coalesce(ib.members,0)
			FROM arcrel r JOIN files fa ON fa.id = r.a JOIN files fb ON fb.id = r.b
			LEFT JOIN arcinfo ia ON ia.file_id = r.a LEFT JOIN arcinfo ib ON ib.file_id = r.b
			ORDER BY r.shared_bytes DESC LIMIT ? OFFSET ?`, limit, offset)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		type rel struct {
			Shared      int64  `json:"shared"`
			SharedBytes int64  `json:"sharedBytes"`
			A           string `json:"a"`
			B           string `json:"b"`
			MembersA    int64  `json:"membersA"`
			MembersB    int64  `json:"membersB"`
		}
		out := []rel{}
		for rows.Next() {
			var x rel
			rows.Scan(&x.Shared, &x.SharedBytes, &x.A, &x.B, &x.MembersA, &x.MembersB)
			out = append(out, x)
		}
		writeJSON(w, map[string]any{"rels": out, "total": e.an.arcRels.Load()})
	})

	mux.HandleFunc("GET /api/cleanup.csv", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="aufraeumen.csv"`)
		if err := e.writeCleanupCSV(r.Context(), w); err != nil && r.Context().Err() == nil {
			log.Printf("Aufräum-Liste: %v", err)
		}
	})

	mux.HandleFunc("GET /api/cleanup.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="aufraeumen.sh"`)
		if err := e.writeCleanupScript(r.Context(), w); err != nil && r.Context().Err() == nil {
			log.Printf("Aufräum-Skript: %v", err)
		}
	})

	// POST /api/similar/retry – fehlgeschlagene Analysen erneut versuchen
	mux.HandleFunc("POST /api/similar/retry", func(w http.ResponseWriter, r *http.Request) {
		if !need(w) {
			return
		}
		n, err := e.an.RetryFailed()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]int64{"retry": n})
	})
}

// decodeJSON liest höchstens 64 KiB JSON.
func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(v)
}

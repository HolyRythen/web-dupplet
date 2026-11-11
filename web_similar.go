package main

import (
	"database/sql"
	"encoding/csv"
	"net/http"
	"strconv"
)

// SimSide beschreibt eine Seite eines ähnlichen Paares.
type SimSide struct {
	FP    int64   `json:"fp"`
	File  int64   `json:"file"`
	Loc   string  `json:"loc"`
	Local bool    `json:"local"`
	Sub   string  `json:"sub,omitempty"`
	Kind  int     `json:"kind"`
	Size  int64   `json:"size"`
	W     int     `json:"w,omitempty"`
	H     int     `json:"h,omitempty"`
	Dur   float64 `json:"dur,omitempty"`
	Words int     `json:"words,omitempty"`
	Snip  string  `json:"snip,omitempty"`
	Thumb int64   `json:"thumb,omitempty"` // fps.id mit Vorschaubild (0 = keins)
}

type SimPair struct {
	ID    int64   `json:"id"`
	Kind  int     `json:"kind"`
	Score float64 `json:"score"`
	A     SimSide `json:"a"`
	B     SimSide `json:"b"`
}

func (e *Engine) side(fp int64) (s SimSide, err error) {
	var w, h, words sql.NullInt64
	var dur sql.NullFloat64
	var snip sql.NullString
	var fkind int
	var thumb sql.NullInt64
	// Vorschau/Textanfang notfalls vom Geschwister-Fingerabdruck derselben Datei bzw. desselben
	// Archiv-Eintrags (exakte Treffer haben selbst keine Vorschau).
	err = e.rdb.QueryRow(`SELECT p.id, p.file_id, p.sub, p.kind, p.size,
		coalesce(p.w, (SELECT q.w FROM fps q WHERE q.file_id=p.file_id AND q.sub=p.sub AND q.w IS NOT NULL LIMIT 1)),
		coalesce(p.h, (SELECT q.h FROM fps q WHERE q.file_id=p.file_id AND q.sub=p.sub AND q.h IS NOT NULL LIMIT 1)),
		p.dur,
		coalesce(p.words, (SELECT q.words FROM fps q WHERE q.file_id=p.file_id AND q.sub=p.sub AND q.words IS NOT NULL LIMIT 1)),
		coalesce(p.snip, (SELECT q.snip FROM fps q WHERE q.file_id=p.file_id AND q.sub=p.sub AND q.snip IS NOT NULL LIMIT 1)),
		f.loc, f.kind,
		coalesce((SELECT t.fp FROM thumbs t WHERE t.fp = p.id),
		         (SELECT t.fp FROM fps q JOIN thumbs t ON t.fp = q.id WHERE q.file_id=p.file_id AND q.sub=p.sub LIMIT 1))
		FROM fps p JOIN files f ON f.id = p.file_id WHERE p.id = ?`, fp).
		Scan(&s.FP, &s.File, &s.Sub, &s.Kind, &s.Size, &w, &h, &dur, &words, &snip, &s.Loc, &fkind, &thumb)
	s.W, s.H, s.Dur, s.Words, s.Snip = int(w.Int64), int(h.Int64), dur.Float64, int(words.Int64), snip.String
	s.Local, s.Thumb = fkind == kindLocal, thumb.Int64
	return
}

func (e *Engine) similarHandlers(mux *http.ServeMux) {
	off := func(w http.ResponseWriter) bool {
		if e.an == nil {
			http.Error(w, "Ähnlichkeitsanalyse ist aus (Option -similar)", 404)
			return true
		}
		return false
	}

	// GET /api/similar?kind=0..4&offset=0&limit=25 – Paare, ähnlichste zuerst
	mux.HandleFunc("GET /api/similar", func(w http.ResponseWriter, r *http.Request) {
		if off(w) {
			return
		}
		q := r.URL.Query()
		kind := atoi(q.Get("kind"), 0)
		limit := clamp(atoi(q.Get("limit"), 25), 1, 200)
		offset := max(atoi(q.Get("offset"), 0), 0)
		var rows *sql.Rows
		var err error
		if kind > 0 {
			rows, err = e.rdb.Query(`SELECT id, kind, score, a, b FROM pairs WHERE kind=? ORDER BY score DESC, id DESC LIMIT ? OFFSET ?`, kind, limit, offset)
		} else {
			rows, err = e.rdb.Query(`SELECT id, kind, score, a, b FROM pairs ORDER BY score DESC, id DESC LIMIT ? OFFSET ?`, limit, offset)
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		type raw struct {
			p    SimPair
			a, b int64
		}
		var rs []raw
		for rows.Next() {
			var x raw
			rows.Scan(&x.p.ID, &x.p.Kind, &x.p.Score, &x.a, &x.b)
			rs = append(rs, x)
		}
		rows.Close()
		out := make([]SimPair, 0, len(rs))
		for _, x := range rs {
			var e1, e2 error
			x.p.A, e1 = e.side(x.a)
			x.p.B, e2 = e.side(x.b)
			if e1 == nil && e2 == nil {
				out = append(out, x.p)
			}
		}
		st := e.an.stats(Stats{})
		total := st.Pairs
		switch kind {
		case pkImage:
			total = st.PairsImage
		case pkText:
			total = st.PairsText
		case pkVideo:
			total = st.PairsVideo
		case pkArchive:
			total = st.PairsArch
		case pkAudio:
			total = st.PairsAudio
		}
		writeJSON(w, map[string]any{"pairs": out, "total": total})
	})

	// GET /api/thumb?fp=ID – kleine JPEG-Vorschau
	mux.HandleFunc("GET /api/thumb", func(w http.ResponseWriter, r *http.Request) {
		if off(w) {
			return
		}
		fp, err := strconv.ParseInt(r.URL.Query().Get("fp"), 10, 64)
		if err != nil {
			http.Error(w, "fp fehlt", 400)
			return
		}
		var jpg []byte
		if err := e.rdb.QueryRow(`SELECT jpg FROM thumbs WHERE fp=?`, fp).Scan(&jpg); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Write(jpg)
	})

	// GET /api/similar.csv – alle Paare
	mux.HandleFunc("GET /api/similar.csv", func(w http.ResponseWriter, r *http.Request) {
		if off(w) {
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="aehnlich.csv"`)
		rows, err := e.rdb.Query(`SELECT p.kind, p.score, fa.loc, a.sub, a.size, fb.loc, b.sub, b.size
			FROM pairs p JOIN fps a ON a.id = p.a JOIN fps b ON b.id = p.b
			JOIN files fa ON fa.id = a.file_id JOIN files fb ON fb.id = b.file_id
			ORDER BY p.score DESC, p.id DESC`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		names := map[int]string{pkImage: "Bild", pkText: "Text", pkVideo: "Video", pkAudio: "Audio", pkArchive: "Archiv (exakt)"}
		cw := csv.NewWriter(w)
		cw.Write([]string{"art", "aehnlichkeit_prozent", "ort_a", "eintrag_a", "groesse_a", "ort_b", "eintrag_b", "groesse_b"})
		for rows.Next() {
			var k int
			var sc float64
			var la, sa, lb, sb string
			var za, zb int64
			rows.Scan(&k, &sc, &la, &sa, &za, &lb, &sb, &zb)
			cw.Write([]string{names[k], strconv.FormatFloat(sc*100, 'f', 1, 64), la, sa, strconv.FormatInt(za, 10), lb, sb, strconv.FormatInt(zb, 10)})
		}
		cw.Flush()
	})
}

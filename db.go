package main

import (
	"database/sql"
	"fmt"
	"strconv"

	_ "modernc.org/sqlite"
)

// Zustände einer Datei
const (
	stNew     = 0 // noch nicht untersucht (Größe unbekannt)
	stProbed  = 1 // Größe bekannt
	stPartial = 2 // Teil-Hash (Anfang+Ende) bekannt
	stFull    = 3 // vollständiger SHA-256 bekannt
	stError   = 9 // endgültig fehlgeschlagen
)

// Was eine Datei noch braucht ("need")
const (
	needNone    = 0 // eindeutig – keine weitere Arbeit
	needPartial = 1 // gleiche Größe wie eine andere Datei → Teil-Hash nötig
	needFull    = 2 // gleicher Teil-Hash → Voll-Hash nötig
)

const (
	kindHTTP  = 0
	kindLocal = 1
)

const schema = `
CREATE TABLE IF NOT EXISTS files(
  id     INTEGER PRIMARY KEY,
  loc    TEXT    NOT NULL UNIQUE,        -- URL oder lokaler Pfad
  kind   INTEGER NOT NULL,               -- 0 = HTTP, 1 = lokal
  size   INTEGER NOT NULL DEFAULT -2,    -- -2 = noch nicht geprüft, -1 = Server nennt keine Größe
  state  INTEGER NOT NULL DEFAULT 0,
  need   INTEGER NOT NULL DEFAULT 0,
  busy   INTEGER NOT NULL DEFAULT 0,
  phash  BLOB,
  fhash  BLOB,
  tries  INTEGER NOT NULL DEFAULT 0,
  err    TEXT,
  added  INTEGER NOT NULL DEFAULT 0,
  done   INTEGER
);
-- Abdeckender Index: Arbeitsverteilung + Statistik ohne Tabellenzugriff
CREATE INDEX IF NOT EXISTS files_work  ON files(kind, busy, state, need, size);
DROP INDEX IF EXISTS files_size; -- alte Version (partieller Index war für size=? nicht nutzbar)
CREATE INDEX IF NOT EXISTS files_size_state ON files(size, state);
CREATE INDEX IF NOT EXISTS files_phash ON files(phash) WHERE phash IS NOT NULL;
CREATE INDEX IF NOT EXISTS files_fhash ON files(fhash) WHERE fhash IS NOT NULL;
CREATE INDEX IF NOT EXISTS files_err   ON files(done)  WHERE state = 9;

-- Ein Eintrag pro eindeutigem Inhalt; cnt > 1 = Duplikatgruppe
CREATE TABLE IF NOT EXISTS hashes(
  fhash BLOB PRIMARY KEY,
  cnt   INTEGER NOT NULL,
  size  INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS hashes_waste ON hashes((cnt-1)*size DESC) WHERE cnt > 1;

CREATE TABLE IF NOT EXISTS crawls(url TEXT PRIMARY KEY, exts TEXT, done INTEGER) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID;
`

func dsn(path string, readonly bool) string {
	s := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(30000)&_pragma=cache_size(-262144)&_pragma=temp_store(MEMORY)" +
		"&_pragma=mmap_size(1073741824)"
	if readonly {
		s += "&_pragma=query_only(1)"
	}
	return s
}

func openDB(path string) (w *sql.DB, r *sql.DB, err error) {
	w, err = sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return
	}
	w.SetMaxOpenConns(1) // ein einziger Schreiber – keine Lock-Konflikte
	if _, err = w.Exec(schema); err != nil {
		return nil, nil, fmt.Errorf("schema: %w", err)
	}
	w.Exec(`ALTER TABLE crawls ADD COLUMN base TEXT`) // Migration; Fehler = Spalte existiert schon
	// Nach Absturz/Neustart: hängende Aufträge freigeben
	if _, err = w.Exec(`UPDATE files SET busy=0 WHERE busy=1`); err != nil {
		return
	}
	r, err = sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return
	}
	r.SetMaxOpenConns(4)
	return
}

// chunkSetting sorgt dafür, dass die Teil-Hash-Größe pro Datenbank konstant bleibt.
func chunkSetting(db *sql.DB, want int) (int, error) {
	var v string
	err := db.QueryRow(`SELECT v FROM meta WHERE k='chunk'`).Scan(&v)
	if err == sql.ErrNoRows {
		_, err = db.Exec(`INSERT INTO meta(k,v) VALUES('chunk',?)`, strconv.Itoa(want))
		return want, err
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

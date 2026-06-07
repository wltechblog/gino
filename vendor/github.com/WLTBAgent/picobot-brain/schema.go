package brain

import (
	"database/sql"
	"sync"

	_ "modernc.org/sqlite"
)

// sqliteDB wraps a *sql.DB to implement the DB interface.
type sqliteDB struct {
	db *sql.DB
	mu sync.RWMutex
}

type sqliteResult struct{ res sql.Result }
type sqliteRows struct {
	rows  *sql.Rows
	unlock func() // called on Close to release RLock
}
type sqliteRow struct{ row *sql.Row }

func openSQLite(path string) (*sqliteDB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// Enable WAL mode and other optimizations for Pi
	db.SetMaxOpenConns(1) // SQLite works best with single writer
	return &sqliteDB{db: db}, nil
}

func (s *sqliteDB) Exec(query string, args ...any) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return nil, err
	}
	return &sqliteResult{res: res}, nil
}

func (s *sqliteDB) Query(query string, args ...any) (Rows, error) {
	s.mu.RLock()
	rows, err := s.db.Query(query, args...)
	if err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	return &sqliteRows{
		rows:   rows,
		unlock: s.mu.RUnlock,
	}, nil
}

func (s *sqliteDB) QueryRow(query string, args ...any) Row {
	return &sqliteRow{row: s.db.QueryRow(query, args...)}
}

func (s *sqliteDB) Close() error {
	return s.db.Close()
}

func (r *sqliteResult) LastInsertId() (int64, error) { return r.res.LastInsertId() }
func (r *sqliteResult) RowsAffected() (int64, error) { return r.res.RowsAffected() }

func (r *sqliteRows) Next() bool         { return r.rows.Next() }
func (r *sqliteRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r *sqliteRows) Err() error         { return r.rows.Err() }
func (r *sqliteRows) Close() error {
	err := r.rows.Close()
	if r.unlock != nil {
		r.unlock()
		r.unlock = nil
	}
	return err
}

func (r *sqliteRow) Scan(dest ...any) error { return r.row.Scan(dest...) }

// runMigrations creates or updates the database schema.
func runMigrations(db DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS schema_version (
	version INTEGER PRIMARY KEY
);

-- Sources: logical partitions
CREATE TABLE IF NOT EXISTS sources (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL UNIQUE,
	local_path  TEXT,
	config      TEXT NOT NULL DEFAULT '{}',
	created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Pages: core content
CREATE TABLE IF NOT EXISTS pages (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	source_id     TEXT NOT NULL DEFAULT 'default' REFERENCES sources(id) ON DELETE CASCADE,
	slug          TEXT NOT NULL,
	type          TEXT NOT NULL DEFAULT 'note',
	title         TEXT NOT NULL DEFAULT '',
	content       TEXT NOT NULL DEFAULT '',
	content_hash  TEXT,
	metadata      TEXT NOT NULL DEFAULT '{}',
	created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(source_id, slug)
);

-- FTS5 full-text index
CREATE VIRTUAL TABLE IF NOT EXISTS pages_fts USING fts5(
	title, content, metadata,
	content='pages', content_rowid='id'
);

-- Triggers to keep FTS in sync
CREATE TRIGGER IF NOT EXISTS pages_fts_insert AFTER INSERT ON pages BEGIN
	INSERT INTO pages_fts(rowid, title, content, metadata) VALUES (new.id, new.title, new.content, new.metadata);
END;

CREATE TRIGGER IF NOT EXISTS pages_fts_update AFTER UPDATE ON pages BEGIN
	INSERT INTO pages_fts(pages_fts, rowid, title, content, metadata) VALUES ('delete', old.id, old.title, old.content, old.metadata);
	INSERT INTO pages_fts(rowid, title, content, metadata) VALUES (new.id, new.title, new.content, new.metadata);
END;

CREATE TRIGGER IF NOT EXISTS pages_fts_delete AFTER DELETE ON pages BEGIN
	INSERT INTO pages_fts(pages_fts, rowid, title, content, metadata) VALUES ('delete', old.id, old.title, old.content, old.metadata);
END;

-- Embeddings: vector per page
CREATE TABLE IF NOT EXISTS embeddings (
	page_id     INTEGER PRIMARY KEY REFERENCES pages(id) ON DELETE CASCADE,
	model       TEXT NOT NULL,
	vector      BLOB NOT NULL,
	updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Entities: extracted people, companies, concepts
CREATE TABLE IF NOT EXISTS entities (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	source_id   TEXT NOT NULL DEFAULT 'default' REFERENCES sources(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	type        TEXT NOT NULL DEFAULT 'person',
	slug        TEXT NOT NULL UNIQUE,
	page_id     INTEGER REFERENCES pages(id) ON DELETE SET NULL,
	metadata    TEXT NOT NULL DEFAULT '{}',
	created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Edges: typed relationships
CREATE TABLE IF NOT EXISTS edges (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	from_id     INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
	to_id       INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
	type        TEXT NOT NULL,
	source_page INTEGER REFERENCES pages(id) ON DELETE CASCADE,
	confidence  REAL NOT NULL DEFAULT 1.0,
	created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(from_id, to_id, type, source_page)
);

-- Ingest log: track imports
CREATE TABLE IF NOT EXISTS ingest_log (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	source_id    TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
	path         TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	status       TEXT NOT NULL DEFAULT 'ok',
	ingested_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(source_id, content_hash)
);

-- Indexes for common queries
CREATE INDEX IF NOT EXISTS idx_pages_source ON pages(source_id);
CREATE INDEX IF NOT EXISTS idx_pages_type ON pages(type);
CREATE INDEX IF NOT EXISTS idx_pages_slug ON pages(slug);
CREATE INDEX IF NOT EXISTS idx_pages_hash ON pages(content_hash);
CREATE INDEX IF NOT EXISTS idx_entities_type ON entities(type);
CREATE INDEX IF NOT EXISTS idx_entities_name ON entities(name);
CREATE INDEX IF NOT EXISTS idx_edges_from ON edges(from_id);
CREATE INDEX IF NOT EXISTS idx_edges_to ON edges(to_id);
CREATE INDEX IF NOT EXISTS idx_edges_type ON edges(type);
`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}
	// Mark schema version
	db.Exec(`INSERT OR IGNORE INTO schema_version (version) VALUES (1)`)
	return nil
}

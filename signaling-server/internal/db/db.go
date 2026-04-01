package db

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS api_keys (
		id TEXT PRIMARY KEY,
		key_hash TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		last_used_at TIMESTAMP,
		is_active BOOLEAN DEFAULT TRUE
	)`,
	`CREATE TABLE IF NOT EXISTS sessions (
		code TEXT PRIMARY KEY,
		api_key_id TEXT NOT NULL,
		agent_id TEXT,
		share_url TEXT NOT NULL,
		expires_at TIMESTAMP,
		max_downloads INTEGER,
		download_count INTEGER DEFAULT 0,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (api_key_id) REFERENCES api_keys(id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_api_key ON sessions(api_key_id)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at) WHERE expires_at IS NOT NULL`,
}

// Open opens the SQLite database and runs migrations.
func Open(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			db.Close()
			return nil, fmt.Errorf("migration failed: %w", err)
		}
	}

	return db, nil
}

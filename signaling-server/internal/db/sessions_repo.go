package db

import (
	"database/sql"
	"time"
)

type Session struct {
	Code          string
	APIKeyID      string
	AgentID       string
	ShareURL      string
	ExpiresAt     *time.Time
	MaxDownloads  *int
	DownloadCount int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type SessionRepo struct {
	db *sql.DB
}

func NewSessionRepo(db *sql.DB) *SessionRepo {
	return &SessionRepo{db: db}
}

// GetByCode retrieves a session by its code.
func (r *SessionRepo) GetByCode(code string) (*Session, error) {
	var s Session
	var expires sql.NullTime
	var maxDownloads sql.NullInt64

	err := r.db.QueryRow(
		`SELECT code, api_key_id, agent_id, share_url, expires_at,
		        max_downloads, download_count, created_at, updated_at
		 FROM sessions WHERE code = ?`,
		code,
	).Scan(&s.Code, &s.APIKeyID, &s.AgentID, &s.ShareURL, &expires, &maxDownloads, &s.DownloadCount, &s.CreatedAt, &s.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if expires.Valid {
		s.ExpiresAt = &expires.Time
	}
	if maxDownloads.Valid {
		max := int(maxDownloads.Int64)
		s.MaxDownloads = &max
	}
	return &s, nil
}

// Create inserts a new session.
func (r *SessionRepo) Create(s *Session) error {
	var expires interface{}
	if s.ExpiresAt != nil {
		expires = *s.ExpiresAt
	} else {
		expires = nil
	}

	var maxDownloads interface{}
	if s.MaxDownloads != nil {
		maxDownloads = *s.MaxDownloads
	} else {
		maxDownloads = nil
	}

	_, err := r.db.Exec(
		`INSERT INTO sessions (code, api_key_id, agent_id, share_url, expires_at, max_downloads)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		s.Code, s.APIKeyID, s.AgentID, s.ShareURL, expires, maxDownloads,
	)
	return err
}

// Update updates an existing session (for reconnection).
func (r *SessionRepo) Update(s *Session) error {
	_, err := r.db.Exec(
		`UPDATE sessions SET agent_id = ?, updated_at = CURRENT_TIMESTAMP WHERE code = ?`,
		s.AgentID, s.Code,
	)
	return err
}

// IncrementDownloadCount increments the download count for a session.
func (r *SessionRepo) IncrementDownloadCount(code string) error {
	_, err := r.db.Exec(
		"UPDATE sessions SET download_count = download_count + 1, updated_at = CURRENT_TIMESTAMP WHERE code = ?",
		code,
	)
	return err
}

// DeleteExpired removes all expired sessions.
func (r *SessionRepo) DeleteExpired() error {
	_, err := r.db.Exec("DELETE FROM sessions WHERE expires_at IS NOT NULL AND expires_at < CURRENT_TIMESTAMP")
	return err
}

// IsCodeAvailable checks if a code is not taken by a different API key.
func (r *SessionRepo) IsCodeAvailable(code string, apiKeyID string) (bool, error) {
	var existingKeyID string
	err := r.db.QueryRow("SELECT api_key_id FROM sessions WHERE code = ?", code).Scan(&existingKeyID)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return existingKeyID == apiKeyID, nil
}

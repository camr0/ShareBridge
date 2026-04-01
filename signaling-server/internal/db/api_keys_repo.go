package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type APIKey struct {
	ID         string
	KeyHash    string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	IsActive   bool
}

type APIKeyRepo struct {
	db *sql.DB
}

func NewAPIKeyRepo(db *sql.DB) *APIKeyRepo {
	return &APIKeyRepo{db: db}
}

// Create inserts a new API key with its bcrypt hash.
func (r *APIKeyRepo) Create(id string, keyHash string) error {
	_, err := r.db.Exec(
		"INSERT INTO api_keys (id, key_hash) VALUES (?, ?)",
		id, keyHash,
	)
	return err
}

// GetByID retrieves an API key by its ID (ak_xxx).
func (r *APIKeyRepo) GetByID(id string) (*APIKey, error) {
	var k APIKey
	var lastUsed sql.NullTime

	err := r.db.QueryRow(
		"SELECT id, key_hash, created_at, last_used_at, is_active FROM api_keys WHERE id = ?",
		id,
	).Scan(&k.ID, &k.KeyHash, &k.CreatedAt, &lastUsed, &k.IsActive)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if lastUsed.Valid {
		k.LastUsedAt = &lastUsed.Time
	}
	return &k, nil
}

// Validate checks if the provided full key (ak_xxx.secret) is valid.
func (r *APIKeyRepo) Validate(fullKey string) (*APIKey, error) {
	// Parse ak_xxx.secret format
	parts := splitKey(fullKey)
	if parts == nil {
		return nil, nil
	}

	key, err := r.GetByID(parts.id)
	if err != nil || key == nil || !key.IsActive {
		return nil, err
	}

	// bcrypt compare
	if err := bcrypt.CompareHashAndPassword([]byte(key.KeyHash), []byte(fullKey)); err != nil {
		return nil, nil
	}

	// Update last_used_at
	r.db.Exec("UPDATE api_keys SET last_used_at = CURRENT_TIMESTAMP WHERE id = ?", parts.id)

	return key, nil
}

// Revoke marks an API key as inactive.
func (r *APIKeyRepo) Revoke(id string) error {
	_, err := r.db.Exec("UPDATE api_keys SET is_active = FALSE WHERE id = ?", id)
	return err
}

// List returns all API keys.
func (r *APIKeyRepo) List() ([]APIKey, error) {
	rows, err := r.db.Query(
		"SELECT id, key_hash, created_at, last_used_at, is_active FROM api_keys ORDER BY created_at DESC",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		var lastUsed sql.NullTime
		if err := rows.Scan(&k.ID, &k.KeyHash, &k.CreatedAt, &lastUsed, &k.IsActive); err != nil {
			return nil, err
		}
		if lastUsed.Valid {
			k.LastUsedAt = &lastUsed.Time
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

type keyParts struct {
	id     string
	secret string
}

func splitKey(fullKey string) *keyParts {
	// Format: ak_xxx.yyyy
	dotIdx := -1
	for i, c := range fullKey {
		if c == '.' {
			dotIdx = i
			break
		}
	}
	if dotIdx == -1 || dotIdx == len(fullKey)-1 {
		return nil
	}

	return &keyParts{
		id:     fullKey[:dotIdx],
		secret: fullKey[dotIdx+1:],
	}
}

// GenerateKeyID generates a new API key ID (ak_ prefix + random suffix).
func GenerateKeyID() string {
	// ak_ + 12 random chars (9 bytes = 12 base64 chars)
	b := make([]byte, 9)
	rand.Read(b)
	return "ak_" + base64.RawURLEncoding.EncodeToString(b)
}

// GenerateSecret generates a new API key secret (24 random chars).
func GenerateSecret() string {
	// 24 random chars (18 bytes = 24 base64 chars)
	b := make([]byte, 18)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

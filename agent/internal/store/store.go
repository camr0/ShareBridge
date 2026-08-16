package store

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SessionEntry represents a single share session with full metadata.
type SessionEntry struct {
	Code                string    `json:"code"`
	ShareURL            string    `json:"share_url"`
	ShareType           string    `json:"share_type"`
	IsPasswordProtected bool      `json:"is_password_protected,omitempty"`
	FileID              string    `json:"file_id,omitempty"`
	Password            string    `json:"password,omitempty"`
	ExpiresAt           time.Time `json:"expires_at"`
	MaxDownloads        int       `json:"max_downloads,omitempty"` // 0 = unlimited
	Downloads           int       `json:"downloads"`
	RelayOnly           bool      `json:"relay_only"`
	CreatedAt           time.Time `json:"created_at"`
	Origin              string    `json:"origin,omitempty"`
}

type storeData struct {
	AgentID               string         `json:"agent_id"`                           // UUID for reconnection
	RelayStaticPrivateHex string         `json:"relay_static_private_hex,omitempty"` // P-256 private key for relay identity
	Sessions              []SessionEntry `json:"sessions"`
}

type Store struct {
	mu       sync.Mutex
	filePath string     // full path to sessions.json
	data     *storeData // in-memory cache
}

// New resolves the data directory (SHAREBRIDGE_DATA_DIR or ~/.sharebridge),
// creates it if needed, and returns a Store. Returns error if dir cannot be created
// or if sessions.json exists but is malformed.
func New() (*Store, error) {
	var dataDir string

	if envDir := os.Getenv("SHAREBRIDGE_DATA_DIR"); envDir != "" {
		dataDir = envDir
	} else {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get user home directory: %w", err)
		}
		dataDir = filepath.Join(homeDir, ".sharebridge")
	}

	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create data directory %s: %w", dataDir, err)
	}

	filePath := filepath.Join(dataDir, "sessions.json")

	store := &Store{
		filePath: filePath,
	}

	// Load existing data or initialize empty
	data, err := store.load()
	if err != nil {
		return nil, fmt.Errorf("sessions.json is malformed — fix or delete %s: %w", store.filePath, err)
	}
	store.data = data

	return store, nil
}

// GetAgentID returns the agent's unique ID, generating one if it doesn't exist.
func (s *Store) GetAgentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.data.AgentID == "" {
		s.data.AgentID = uuid.New().String()
		if err := s.save(); err != nil {
			log.Printf("failed to persist AgentID: %v", err)
		}
	}

	return s.data.AgentID
}

// GetRelayStaticPrivateKey returns the agent's long-lived P-256 relay static private key,
// generating one if it doesn't exist. The key is persisted in hex format.
func (s *Store) GetRelayStaticPrivateKey() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.data.RelayStaticPrivateHex == "" {
		priv, err := ecdh.P256().GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate relay static key: %w", err)
		}
		s.data.RelayStaticPrivateHex = hex.EncodeToString(priv.Bytes())
		if err := s.save(); err != nil {
			return nil, fmt.Errorf("persist relay static key: %w", err)
		}
		return append([]byte(nil), priv.Bytes()...), nil
	}

	raw, err := hex.DecodeString(s.data.RelayStaticPrivateHex)
	if err != nil {
		return nil, fmt.Errorf("decode relay static key: %w", err)
	}
	return raw, nil
}

// GetSession returns the session for a given code, or nil if not found.
func (s *Store) GetSession(code string) *SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.data.Sessions {
		if s.data.Sessions[i].Code == code {
			return &s.data.Sessions[i]
		}
	}
	return nil
}

// GetByShareURL returns the session for a given shareURL, or nil if not found.
// Used for single-session mode persistence.
func (s *Store) GetByShareURL(shareURL string) *SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.data.Sessions {
		if s.data.Sessions[i].ShareURL == shareURL {
			return &s.data.Sessions[i]
		}
	}
	return nil
}

// ListSessions returns all sessions, optionally filtering out expired ones.
func (s *Store) ListSessions(filterExpired bool) []SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !filterExpired {
		return s.data.Sessions
	}

	var result []SessionEntry
	now := time.Now()
	for _, session := range s.data.Sessions {
		if session.ExpiresAt.IsZero() || session.ExpiresAt.After(now) {
			result = append(result, session)
		}
	}
	return result
}

// SaveSession adds or updates a session. If a session with the same code exists,
// it is replaced.
func (s *Store) SaveSession(session SessionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Find existing session with same code
	for i := range s.data.Sessions {
		if s.data.Sessions[i].Code == session.Code {
			s.data.Sessions[i] = session
			return s.save()
		}
	}

	// Not found, append new session
	s.data.Sessions = append(s.data.Sessions, session)
	return s.save()
}

// DeleteSession removes a session by code. Returns nil if session didn't exist.
func (s *Store) DeleteSession(code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.data.Sessions {
		if s.data.Sessions[i].Code == code {
			// Remove element by appending remaining
			s.data.Sessions = append(s.data.Sessions[:i], s.data.Sessions[i+1:]...)
			return s.save()
		}
	}

	// Not found, no error
	return nil
}

// IncrementDownloads increments the download count for a session and returns the new count.
func (s *Store) IncrementDownloads(code string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.data.Sessions {
		if s.data.Sessions[i].Code == code {
			s.data.Sessions[i].Downloads++
			newCount := s.data.Sessions[i].Downloads
			return newCount, s.save()
		}
	}

	return 0, fmt.Errorf("session with code %q not found", code)
}

// load reads sessions.json. Returns empty storeData if file doesn't exist.
// Returns error if file exists but is malformed JSON.
func (s *Store) load() (*storeData, error) {
	var data storeData

	fileContent, err := os.ReadFile(s.filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &data, nil
		}
		return nil, fmt.Errorf("failed to read sessions file: %w", err)
	}

	if err := json.Unmarshal(fileContent, &data); err != nil {
		return nil, fmt.Errorf("malformed sessions.json: %w", err)
	}

	return &data, nil
}

// save writes s.data to sessions.json atomically (write to temp file, rename).
func (s *Store) save() error {
	tempFile := s.filePath + ".tmp"

	jsonData, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal store data: %w", err)
	}

	// Write to temp file with 0600 permissions
	if err := os.WriteFile(tempFile, jsonData, 0600); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tempFile, s.filePath); err != nil {
		// Clean up temp file on error
		os.Remove(tempFile)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

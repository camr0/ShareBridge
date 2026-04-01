package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
)

type sessionEntry struct {
	Code          string `json:"code"`
	DownloadCount int    `json:"download_count"`
	APIKeyID      string `json:"api_key_id"` // track which key created this
}

type storeData struct {
	AgentID  string                  `json:"agent_id"` // UUID for reconnection
	Sessions map[string]sessionEntry `json:"sessions"`
}

type Store struct {
	mu       sync.Mutex
	filePath string // full path to sessions.json
}

// New resolves the data directory (OPENCLOUDSHARE_DATA_DIR or ~/.opencloudshare),
// creates it if needed, and returns a Store. Returns error if dir cannot be created
// or if sessions.json exists but is malformed.
func New() (*Store, error) {
	var dataDir string

	if envDir := os.Getenv("OPENCLOUDSHARE_DATA_DIR"); envDir != "" {
		dataDir = envDir
	} else {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get user home directory: %w", err)
		}
		dataDir = filepath.Join(homeDir, ".opencloudshare")
	}

	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create data directory %s: %w", dataDir, err)
	}

	filePath := filepath.Join(dataDir, "sessions.json")

	store := &Store{
		filePath: filePath,
	}

	// Try to load existing data to validate it's not malformed
	_, err := store.load()
	if err != nil {
		return nil, fmt.Errorf("sessions.json is malformed — fix or delete %s: %w", store.filePath, err)
	}

	return store, nil
}

// GetCode returns the code for a given shareURL, or "" if not found.
func (s *Store) GetCode(shareURL string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return ""
	}

	entry, exists := data.Sessions[shareURL]
	if !exists {
		return ""
	}

	return entry.Code
}

// SetCode sets the code for a given shareURL.
func (s *Store) SetCode(shareURL string, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return err
	}

	if data.Sessions == nil {
		data.Sessions = make(map[string]sessionEntry)
	}

	entry := data.Sessions[shareURL]
	entry.Code = code
	data.Sessions[shareURL] = entry

	return s.save(data)
}

// GetDownloadCount returns the download count for a given shareURL, or 0 if not found.
func (s *Store) GetDownloadCount(shareURL string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return 0
	}

	entry, exists := data.Sessions[shareURL]
	if !exists {
		return 0
	}

	return entry.DownloadCount
}

// IncrementDownloadCount increments the download count for a given shareURL and persists it.
// Returns the new count.
func (s *Store) IncrementDownloadCount(shareURL string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return 0, err
	}

	if data.Sessions == nil {
		data.Sessions = make(map[string]sessionEntry)
	}

	entry := data.Sessions[shareURL]
	entry.DownloadCount++
	data.Sessions[shareURL] = entry

	if err := s.save(data); err != nil {
		return 0, err
	}

	return entry.DownloadCount, nil
}

// GetAgentID returns the agent's unique ID, generating one if it doesn't exist.
func (s *Store) GetAgentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		// Generate new ID even if load fails
		newID := uuid.New().String()
		s.save(storeData{AgentID: newID, Sessions: make(map[string]sessionEntry)})
		return newID
	}

	if data.AgentID == "" {
		data.AgentID = uuid.New().String()
		s.save(data)
	}

	return data.AgentID
}

// SetAPIKeyID sets the API key ID for a given shareURL.
func (s *Store) SetAPIKeyID(shareURL string, apiKeyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return err
	}

	if data.Sessions == nil {
		data.Sessions = make(map[string]sessionEntry)
	}

	entry := data.Sessions[shareURL]
	entry.APIKeyID = apiKeyID
	data.Sessions[shareURL] = entry

	return s.save(data)
}

// GetAPIKeyID returns the API key ID for a given shareURL, or "" if not found.
func (s *Store) GetAPIKeyID(shareURL string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return ""
	}

	entry, exists := data.Sessions[shareURL]
	if !exists {
		return ""
	}

	return entry.APIKeyID
}

// load reads sessions.json. Returns empty storeData if file doesn't exist.
// Returns error if file exists but is malformed JSON.
func (s *Store) load() (storeData, error) {
	var data storeData
	data.Sessions = make(map[string]sessionEntry)

	fileContent, err := os.ReadFile(s.filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return data, nil
		}
		return data, fmt.Errorf("failed to read sessions file: %w", err)
	}

	if err := json.Unmarshal(fileContent, &data); err != nil {
		return storeData{}, fmt.Errorf("malformed sessions.json: %w", err)
	}

	return data, nil
}

// save writes storeData to sessions.json atomically (write to temp file, rename).
func (s *Store) save(data storeData) error {
	tempFile := s.filePath + ".tmp"

	jsonData, err := json.MarshalIndent(data, "", "  ")
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

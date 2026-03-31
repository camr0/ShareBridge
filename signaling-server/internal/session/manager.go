package session

import (
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"
)

const codeChars = "abcdefghijklmnopqrstuvwxyz0123456789"
const codeLen = 8

type Session struct {
	ID        string
	Token     string
	ShareURL  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewManager() *Manager {
	return &Manager{sessions: make(map[string]*Session)}
}

func (m *Manager) Create(token, shareURL, preferredCode string, ttl time.Duration) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	code := preferredCode
	if code == "" || m.isCodeTakenLocked(code, token) {
		if code != "" {
			log.Printf("warning: requested code %s is already taken, assigning new code", code)
		}
		var err error
		code, err = generateCode()
		if err != nil {
			return nil, fmt.Errorf("generate code: %w", err)
		}
	}
	now := time.Now()
	s := &Session{
		ID:        code,
		Token:     token,
		ShareURL:  shareURL,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	m.sessions[code] = s
	return s, nil
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok || time.Now().After(s.ExpiresAt) {
		return nil, false
	}
	return s, true
}

func (m *Manager) Delete(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// isCodeTaken returns true if the code is in use by a different token.
// Same token re-registering the same code is allowed.
func (m *Manager) isCodeTaken(code, token string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.isCodeTakenLocked(code, token)
}

// isCodeTakenLocked is the internal version that requires the caller to hold
// mu (either read or write lock).
func (m *Manager) isCodeTakenLocked(code, token string) bool {
	s, ok := m.sessions[code]
	return ok && s.Token != token && time.Now().Before(s.ExpiresAt)
}

func generateCode() (string, error) {
	b := make([]byte, codeLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(codeChars))))
		if err != nil {
			return "", err
		}
		b[i] = codeChars[n.Int64()]
	}
	return string(b), nil
}

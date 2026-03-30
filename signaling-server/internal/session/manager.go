package session

import (
	"crypto/rand"
	"fmt"
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

func (m *Manager) Create(token, shareURL string, ttl time.Duration) (*Session, error) {
	id, err := generateCode()
	if err != nil {
		return nil, fmt.Errorf("generate code: %w", err)
	}
	s := &Session{
		ID:        id,
		Token:     token,
		ShareURL:  shareURL,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(ttl),
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	return s, nil
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
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

package relay

import (
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"
)

var (
	ErrUnknownSID = errors.New("relay: unknown sid")
	ErrReplay     = errors.New("relay: replayed jti")
)

type SessionState string

const (
	StatePendingBrowser SessionState = "pending_browser"
	StatePendingAgent   SessionState = "pending_agent"
	StateActive         SessionState = "active"
)

type PendingSession struct {
	SID               string
	AccountID         string
	SessionCode       string
	AgentID           string
	RelayAllowed      bool
	RelayOnly         bool
	ExpectedStaticPub string
	JTI               string
	ExpiresAt         time.Time
}

type Registry struct {
	mu            sync.Mutex
	pendingWindow time.Duration
	sessions      map[string]*sessionEntry
	spentJTI      map[string]time.Time
}

type sessionEntry struct {
	session        PendingSession
	agentSocket    *websocket.Conn
	browserSocket  *websocket.Conn
	forwardedBytes int64
	agentReady     chan struct{}
	browserReady   chan struct{}
	done           chan struct{}
}

func NewRegistry(pendingWindow time.Duration) *Registry {
	return &Registry{
		pendingWindow: pendingWindow,
		sessions:      make(map[string]*sessionEntry),
		spentJTI:      make(map[string]time.Time),
	}
}

func (r *Registry) CreatePendingSession(session PendingSession, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[session.SID] = &sessionEntry{
		session:      session,
		agentReady:   make(chan struct{}),
		browserReady: make(chan struct{}),
		done:         make(chan struct{}),
	}
	return nil
}

func (r *Registry) BindBrowserSocket(sid, jti string, conn *websocket.Conn, now time.Time) (*websocket.Conn, SessionState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, spent := r.spentJTI[jti]; spent {
		return nil, "", ErrReplay
	}
	entry, ok := r.sessions[sid]
	if !ok || now.After(entry.session.ExpiresAt) {
		return nil, "", ErrUnknownSID
	}
	r.spentJTI[jti] = now
	entry.browserSocket = conn
	select {
	case <-entry.browserReady:
	default:
		close(entry.browserReady)
	}
	select {
	case <-entry.agentReady:
		return entry.agentSocket, StateActive, nil
	default:
	}
	return nil, StatePendingBrowser, nil
}

func (r *Registry) WaitForAgent(sid string, now time.Time) (*websocket.Conn, error) {
	r.mu.Lock()
	entry, ok := r.sessions[sid]
	if !ok {
		r.mu.Unlock()
		return nil, ErrUnknownSID
	}
	deadline := now.Add(r.pendingWindow)
	if entry.session.ExpiresAt.Before(deadline) {
		deadline = entry.session.ExpiresAt
	}
	ready := entry.agentReady
	r.mu.Unlock()

	select {
	case <-ready:
	case <-time.After(time.Until(deadline)):
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.sessions, sid)
		return nil, errors.New("relay: pending wait window exceeded")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok = r.sessions[sid]
	if !ok {
		return nil, ErrUnknownSID
	}
	return entry.agentSocket, nil
}

func (r *Registry) BindAgentSocket(sid, agentID string, conn *websocket.Conn, now time.Time) (*websocket.Conn, SessionState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[sid]
	if !ok || now.After(entry.session.ExpiresAt) {
		return nil, "", ErrUnknownSID
	}
	if entry.session.AgentID != "" && entry.session.AgentID != agentID {
		return nil, "", ErrUnknownSID
	}
	entry.agentSocket = conn
	select {
	case <-entry.agentReady:
	default:
		close(entry.agentReady)
	}
	select {
	case <-entry.browserReady:
		return entry.browserSocket, StateActive, nil
	default:
	}
	return nil, StatePendingAgent, nil
}

func (r *Registry) WaitForBrowser(sid string, now time.Time) (*websocket.Conn, error) {
	r.mu.Lock()
	entry, ok := r.sessions[sid]
	if !ok {
		r.mu.Unlock()
		return nil, ErrUnknownSID
	}
	deadline := now.Add(r.pendingWindow)
	if entry.session.ExpiresAt.Before(deadline) {
		deadline = entry.session.ExpiresAt
	}
	ready := entry.browserReady
	r.mu.Unlock()

	select {
	case <-ready:
	case <-time.After(time.Until(deadline)):
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.sessions, sid)
		return nil, errors.New("relay: pending wait window exceeded")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok = r.sessions[sid]
	if !ok {
		return nil, ErrUnknownSID
	}
	return entry.browserSocket, nil
}

func (r *Registry) WatchSession(sid string) (<-chan struct{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[sid]
	if !ok {
		return nil, ErrUnknownSID
	}
	return entry.done, nil
}

func (r *Registry) AddForwardedBytes(sid string, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.sessions[sid]; ok {
		entry.forwardedBytes += n
	}
}

func (r *Registry) Get(sid string) (PendingSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[sid]
	if !ok {
		return PendingSession{}, ErrUnknownSID
	}
	return entry.session, nil
}

func (r *Registry) MarkDirectSuccess(sid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, sid)
}

func (r *Registry) CloseSession(sid string) (string, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[sid]
	if !ok {
		return "", 0, ErrUnknownSID
	}
	accountID := entry.session.AccountID
	bytes := entry.forwardedBytes
	select {
	case <-entry.done:
	default:
		close(entry.done)
	}
	delete(r.sessions, sid)
	return accountID, bytes, nil
}
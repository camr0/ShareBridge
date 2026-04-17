# Slice 13a — Relay + Token Issuance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Embed a go-libp2p Host inside the signaling server that issues and validates JWTs, forwards circuit streams between browsers and agents via a custom `/sharebridge/relay/1.0.0` protocol, and decommissions coturn.

**Architecture:** The signaling server binary gains a new `internal/relay/` package. The relay is a libp2p Host on `127.0.0.1:9001` (behind Caddy TLS) that uses AutoRelay v2's "static relay" primitive PLUS a custom stream handler that validates a signed JWT before any bytes are circuited. JWTs are issued inline by `handler/browser_ws.go` after the existing HMAC pre-challenge. The `internal/turn/` package is deleted entirely. No agent or browser code changes in this slice — verification uses a standalone go-libp2p test client.

**Tech Stack:** Go 1.22, go-libp2p v0.40+ (`github.com/libp2p/go-libp2p`, `.../go-libp2p/core/host`, `.../p2p/protocol/circuitv2/relay`, `.../go-libp2p-noise`, `.../go-libp2p/p2p/muxer/yamux`), `github.com/golang-jwt/jwt/v5`, PocketBase, coder/websocket (existing).

---

## File Structure

**New files:**
- `signaling-server/internal/relay/relay.go` — libp2p Host construction, lifecycle (Start/Stop), agent registry
- `signaling-server/internal/relay/relay_test.go` — Host lifecycle tests
- `signaling-server/internal/relay/token.go` — JWT signing + validation, JTI store
- `signaling-server/internal/relay/token_test.go` — JWT and JTI tests
- `signaling-server/internal/relay/protocol.go` — `/sharebridge/relay/1.0.0` stream handler
- `signaling-server/internal/relay/protocol_test.go` — protocol handler tests
- `signaling-server/internal/relay/registry.go` — agent peer ID → API key ID mapping
- `signaling-server/internal/relay/registry_test.go` — registry tests
- `signaling-server/internal/relay/counter.go` — per-circuit byte counter wrapper
- `signaling-server/internal/relay/counter_test.go` — counter tests

**Modified files:**
- `signaling-server/internal/handler/browser_ws.go` — replace `ice_config` send with `{ relay_multiaddr, jwt }` send after HMAC pre-challenge succeeds; remove `answer` and `ice_candidate` forwarding cases; keep knock/join/auth_failed flow
- `signaling-server/internal/handler/agent_ws.go` — replace `welcome` `ice_servers` with `relay_multiaddr`; remove `offer` and `ice_candidate` cases; remove `isRelayCandidate` + `refreshQuota` call sites; keep `hello`, `register_share`, `nonce`, `auth_failed`, `session_expired`, `download_complete`
- `signaling-server/internal/config/config.go` — drop `TurnHost`/`TurnPort`/`TurnSecret`/`HasTurn`/`TurnURL`; add `RelayListenAddr`, `RelayAnnounceAddr`, `RelayPrivateKeyPath`, `JWTSecret`, `JWTTTL`
- `signaling-server/cmd/server/main.go` — start the relay Host in `OnServe`, pass it to handlers, stop it in a shutdown hook; remove TURN-related branching
- `signaling-server/internal/quota/poller.go` — switch from coturn Prometheus metrics to relay-reported byte counts (relay emits counts into PocketBase per circuit close; quota poller still runs but consumes relay data, not Prometheus)

**Deleted files:**
- `signaling-server/internal/turn/` (entire directory: `credentials.go`, `credentials_test.go`, `ice_config.go`, `ice_config_test.go`)

**Caddy + infra (documented as steps, not code):**
- `deploy/Caddyfile` (or equivalent) — add `relay.sharebridge.app` route to `localhost:9001`, **without** the Cloudflare IP allowlist

---

## Notes for the implementing engineer

You are probably new to libp2p. Key concepts:

- **Host** — a libp2p peer. Identified by a `peer.ID` (hash of public key). Has transports, muxers, security protocols, and stream handlers.
- **Multiaddr** — self-describing address like `/dns4/relay.sharebridge.app/tcp/443/wss/p2p/12D3Koo...`. `p2p/<peerID>` at the end is required to dial a specific peer.
- **Circuit Relay v2** — a protocol (`/libp2p/circuit/relay/0.2.0/*`) where a relay Host forwards streams between two peers that can't directly connect. In this codebase we wrap it with our own `/sharebridge/relay/1.0.0` handshake that must succeed first.
- **DCUtR** — "Direct Connection Upgrade through Relay" (`/libp2p/dcutr`). After the circuit is up, both endpoints attempt a hole punch; on success, the circuit is torn down. We use the stock libp2p implementation — no custom code, just don't disable it.
- **Noise** — libp2p's default transport encryption. Auto-negotiated. We get E2E encryption for free.
- **PeerID vs libp2p address** — a Host can have multiple addresses (TCP, QUIC, WSS) but one peer ID. Always dial `/p2p/<peerID>` form.

When in doubt, consult [libp2p docs](https://docs.libp2p.io/concepts/) and the go-libp2p [circuit v2 example](https://github.com/libp2p/go-libp2p/tree/master/examples/relay).

---

## Task 1: Scaffold `internal/relay/` package and add libp2p deps

**Files:**
- Create: `signaling-server/internal/relay/relay.go`
- Create: `signaling-server/internal/relay/relay_test.go`
- Modify: `signaling-server/go.mod`

- [ ] **Step 1: Add dependencies**

Run:
```bash
cd signaling-server
go get github.com/libp2p/go-libp2p@latest
go get github.com/libp2p/go-libp2p/core@latest
go get github.com/golang-jwt/jwt/v5@latest
```
Expected: `go.mod` and `go.sum` updated, no build.

- [ ] **Step 2: Write failing test for Host construction**

```go
// signaling-server/internal/relay/relay_test.go
package relay

import (
	"context"
	"testing"
	"time"
)

func TestNewHost_startsAndStops(t *testing.T) {
	cfg := Config{
		ListenAddr:     "/ip4/127.0.0.1/tcp/0/ws", // port 0 = kernel-assigned
		PrivateKeyPath: "",                         // empty = generate ephemeral
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.Host() == nil {
		t.Fatal("Host() returned nil")
	}
	if r.Host().ID().String() == "" {
		t.Fatal("empty peer ID")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
```

Run: `go test ./internal/relay/ -run TestNewHost_startsAndStops -v`
Expected: FAIL — package `relay` is not defined.

- [ ] **Step 3: Write minimal `relay.go` to pass**

```go
// signaling-server/internal/relay/relay.go
package relay

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/multiformats/go-multiaddr"
)

// Config configures the relay Host.
type Config struct {
	ListenAddr      string // multiaddr string, e.g. "/ip4/127.0.0.1/tcp/9001/ws"
	AnnounceAddr    string // optional public multiaddr advertised to peers
	PrivateKeyPath  string // PEM path; empty = generate ephemeral Ed25519
}

// Relay wraps a libp2p Host configured as a sharebridge relay.
type Relay struct {
	h host.Host
}

// New starts a libp2p Host with the configured listen address.
// TODO: stream handler + identity persistence are added in later tasks.
func New(ctx context.Context, cfg Config) (*Relay, error) {
	priv, err := loadOrGenerateKey(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load key: %w", err)
	}
	listen, err := multiaddr.NewMultiaddr(cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("parse listen addr: %w", err)
	}
	opts := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.ListenAddrs(listen),
		libp2p.DisableRelay(), // we run our own protocol, not stock circuit v2 yet
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("new libp2p host: %w", err)
	}
	return &Relay{h: h}, nil
}

// Host returns the underlying libp2p Host.
func (r *Relay) Host() host.Host { return r.h }

// Close stops the Host.
func (r *Relay) Close() error { return r.h.Close() }

func loadOrGenerateKey(path string) (crypto.PrivKey, error) {
	if path == "" {
		priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
		return priv, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return crypto.UnmarshalPrivateKey(b)
}
```

Run: `go test ./internal/relay/ -run TestNewHost_startsAndStops -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/go.mod signaling-server/go.sum signaling-server/internal/relay/
git commit -m "feat(slice-13a): scaffold libp2p relay package"
```

---

## Task 2: Persistent relay identity

The relay's peer ID must be stable across restarts so browsers can cache and dial it. Generate an Ed25519 key at first boot, persist as PEM, load on subsequent boots.

**Files:**
- Modify: `signaling-server/internal/relay/relay.go`
- Modify: `signaling-server/internal/relay/relay_test.go`

- [ ] **Step 1: Write failing test for persistent identity**

Append to `relay_test.go`:
```go
func TestNewHost_persistsIdentity(t *testing.T) {
	tmp := t.TempDir()
	keyPath := tmp + "/relay.key"

	r1, err := New(context.Background(), Config{
		ListenAddr:     "/ip4/127.0.0.1/tcp/0/ws",
		PrivateKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	firstID := r1.Host().ID()
	r1.Close()

	r2, err := New(context.Background(), Config{
		ListenAddr:     "/ip4/127.0.0.1/tcp/0/ws",
		PrivateKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer r2.Close()

	if r2.Host().ID() != firstID {
		t.Fatalf("peer ID changed across restart: %s != %s", firstID, r2.Host().ID())
	}
}
```

Run: `go test ./internal/relay/ -run TestNewHost_persistsIdentity -v`
Expected: FAIL — `loadOrGenerateKey` doesn't write a new key when the file is missing.

- [ ] **Step 2: Fix `loadOrGenerateKey` to persist on generate**

Replace the function body with:
```go
func loadOrGenerateKey(path string) (crypto.PrivKey, error) {
	if path == "" {
		priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
		return priv, err
	}
	b, err := os.ReadFile(path)
	if err == nil {
		return crypto.UnmarshalPrivateKey(b)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, err
	}
	marshaled, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, marshaled, 0600); err != nil {
		return nil, fmt.Errorf("write key: %w", err)
	}
	return priv, nil
}
```

Run: `go test ./internal/relay/ -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add signaling-server/internal/relay/
git commit -m "feat(slice-13a): persist relay identity to disk"
```

---

## Task 3: JWT issuance and validation

JWTs are issued by `handler/browser_ws.go` after the HMAC pre-challenge (or immediately for no-password shares). Validated by the relay when the browser opens the custom stream. Signed with a server-side secret (HS256).

**Files:**
- Create: `signaling-server/internal/relay/token.go`
- Create: `signaling-server/internal/relay/token_test.go`

- [ ] **Step 1: Write failing tests for JWT round-trip**

```go
// signaling-server/internal/relay/token_test.go
package relay

import (
	"testing"
	"time"
)

func TestIssueAndValidate_roundTrip(t *testing.T) {
	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), 5*time.Minute)

	claims := Claims{
		ShareCode:     "abc12345",
		BrowserPeerID: "12D3KooWBrowser",
		RelayAllowed:  true,
		DCUtRAllowed:  true,
	}
	tok, err := issuer.Issue(claims)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}

	got, err := issuer.Validate(tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.ShareCode != claims.ShareCode ||
		got.BrowserPeerID != claims.BrowserPeerID ||
		got.RelayAllowed != claims.RelayAllowed ||
		got.DCUtRAllowed != claims.DCUtRAllowed {
		t.Fatalf("claims mismatch: got %+v want %+v", got, claims)
	}
	if got.JTI == "" {
		t.Fatal("JTI empty")
	}
}

func TestValidate_rejectsTamperedClaim(t *testing.T) {
	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), 5*time.Minute)
	tok, _ := issuer.Issue(Claims{ShareCode: "abc12345", RelayAllowed: true})

	// Corrupt one char in the middle segment (claims).
	parts := []byte(tok)
	for i, c := range parts {
		if c == '.' {
			parts[i+5] = parts[i+5] ^ 0x01
			break
		}
	}
	if _, err := issuer.Validate(string(parts)); err == nil {
		t.Fatal("expected signature error")
	}
}

func TestValidate_rejectsExpired(t *testing.T) {
	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Millisecond)
	tok, _ := issuer.Issue(Claims{ShareCode: "abc12345"})
	time.Sleep(10 * time.Millisecond)
	if _, err := issuer.Validate(tok); err == nil {
		t.Fatal("expected expiry error")
	}
}
```

Run: `go test ./internal/relay/ -run TestIssueAndValidate -v`
Expected: FAIL — `token.go` missing.

- [ ] **Step 2: Implement `token.go`**

```go
// signaling-server/internal/relay/token.go
package relay

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the sharebridge JWT payload.
type Claims struct {
	JTI           string `json:"jti"`
	ShareCode     string `json:"share_code"`
	BrowserPeerID string `json:"browser_peer_id"`
	RelayAllowed  bool   `json:"relay_allowed"`
	DCUtRAllowed  bool   `json:"dcutr_allowed"`
	ExpiresAt     int64  `json:"exp"`
}

// Issuer signs and validates JWTs with a shared HS256 secret.
type Issuer struct {
	secret []byte
	ttl    time.Duration
}

func NewIssuer(secret []byte, ttl time.Duration) *Issuer {
	return &Issuer{secret: secret, ttl: ttl}
}

// Issue mints a new token. JTI is generated randomly; ExpiresAt is now+ttl.
func (i *Issuer) Issue(c Claims) (string, error) {
	if len(i.secret) < 32 {
		return "", errors.New("jwt secret must be >= 32 bytes")
	}
	if c.JTI == "" {
		c.JTI = randomHex(16)
	}
	c.ExpiresAt = time.Now().Add(i.ttl).Unix()

	mapClaims := jwt.MapClaims{
		"jti":             c.JTI,
		"share_code":      c.ShareCode,
		"browser_peer_id": c.BrowserPeerID,
		"relay_allowed":   c.RelayAllowed,
		"dcutr_allowed":   c.DCUtRAllowed,
		"exp":             c.ExpiresAt,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, mapClaims)
	return tok.SignedString(i.secret)
}

// Validate parses and verifies the token. Does NOT check JTI replay (see JTIStore).
func (i *Issuer) Validate(tok string) (*Claims, error) {
	parsed, err := jwt.Parse(tok, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return i.secret, nil
	})
	if err != nil {
		return nil, err
	}
	mc, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || !parsed.Valid {
		return nil, errors.New("invalid claims")
	}
	c := &Claims{}
	if v, ok := mc["jti"].(string); ok {
		c.JTI = v
	}
	if v, ok := mc["share_code"].(string); ok {
		c.ShareCode = v
	}
	if v, ok := mc["browser_peer_id"].(string); ok {
		c.BrowserPeerID = v
	}
	if v, ok := mc["relay_allowed"].(bool); ok {
		c.RelayAllowed = v
	}
	if v, ok := mc["dcutr_allowed"].(bool); ok {
		c.DCUtRAllowed = v
	}
	if v, ok := mc["exp"].(float64); ok {
		c.ExpiresAt = int64(v)
	}
	if c.JTI == "" {
		return nil, errors.New("missing jti")
	}
	return c, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
```

Run: `go test ./internal/relay/ -run TestIssueAndValidate -v`
Expected: PASS (all three).

- [ ] **Step 3: Commit**

```bash
git add signaling-server/internal/relay/token.go signaling-server/internal/relay/token_test.go
git commit -m "feat(slice-13a): JWT issue + validate"
```

---

## Task 4: JTI replay store

In-memory. Single-use per JTI within TTL window. Restart-tolerant (5-min window acceptable per spec).

**Files:**
- Modify: `signaling-server/internal/relay/token.go`
- Modify: `signaling-server/internal/relay/token_test.go`

- [ ] **Step 1: Write failing test**

Append to `token_test.go`:
```go
func TestJTIStore_firstUseAcceptedSecondRejected(t *testing.T) {
	store := NewJTIStore(5 * time.Minute)
	defer store.Close()

	if !store.ConsumeOnce("jti-A") {
		t.Fatal("first use should be accepted")
	}
	if store.ConsumeOnce("jti-A") {
		t.Fatal("second use should be rejected")
	}
}

func TestJTIStore_expiredEntriesPruned(t *testing.T) {
	store := &JTIStore{
		entries: make(map[string]time.Time),
		ttl:     20 * time.Millisecond,
	}
	store.ConsumeOnce("jti-B")
	time.Sleep(40 * time.Millisecond)
	store.prune()
	if _, ok := store.entries["jti-B"]; ok {
		t.Fatal("expected jti-B to be pruned")
	}
}
```

Run: `go test ./internal/relay/ -run TestJTIStore -v`
Expected: FAIL.

- [ ] **Step 2: Implement `JTIStore`**

Append to `token.go`:
```go
// JTIStore tracks consumed JTIs within the TTL window.
type JTIStore struct {
	mu      sync.Mutex
	entries map[string]time.Time // jti → issuedAt
	ttl     time.Duration
	stop    chan struct{}
}

// NewJTIStore starts a background prune goroutine. Call Close to stop it.
func NewJTIStore(ttl time.Duration) *JTIStore {
	s := &JTIStore{
		entries: make(map[string]time.Time),
		ttl:     ttl,
		stop:    make(chan struct{}),
	}
	go s.pruneLoop()
	return s
}

// ConsumeOnce returns true iff this is the first time jti has been seen.
func (s *JTIStore) ConsumeOnce(jti string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[jti]; exists {
		return false
	}
	s.entries[jti] = time.Now()
	return true
}

func (s *JTIStore) Close() { close(s.stop) }

func (s *JTIStore) pruneLoop() {
	t := time.NewTicker(s.ttl)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.prune()
		case <-s.stop:
			return
		}
	}
}

func (s *JTIStore) prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-s.ttl)
	for jti, ts := range s.entries {
		if ts.Before(cutoff) {
			delete(s.entries, jti)
		}
	}
}
```

Add `"sync"` to the imports.

Run: `go test ./internal/relay/ -run TestJTIStore -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add signaling-server/internal/relay/
git commit -m "feat(slice-13a): JTI replay store"
```

---

## Task 5: Agent registry

Maps agent peer IDs to API key IDs so the relay knows which agent to dial when a browser connects with a share code.

**Files:**
- Create: `signaling-server/internal/relay/registry.go`
- Create: `signaling-server/internal/relay/registry_test.go`

- [ ] **Step 1: Write failing tests**

```go
// signaling-server/internal/relay/registry_test.go
package relay

import (
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
)

func TestRegistry_registerAndLookup(t *testing.T) {
	reg := NewAgentRegistry()
	pid, err := peer.Decode("12D3KooWGRUacMd4aSXwsNyEYxh3nC1dT3rBjoJ5ycnqZRuiETxF")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	reg.Register("api-key-123", pid)

	got, ok := reg.Lookup("api-key-123")
	if !ok || got != pid {
		t.Fatalf("lookup: got %v ok=%v want %v true", got, ok, pid)
	}

	keyID, ok := reg.Reverse(pid)
	if !ok || keyID != "api-key-123" {
		t.Fatalf("reverse: got %q ok=%v", keyID, ok)
	}
}

func TestRegistry_unregisterRemovesBothDirections(t *testing.T) {
	reg := NewAgentRegistry()
	pid, _ := peer.Decode("12D3KooWGRUacMd4aSXwsNyEYxh3nC1dT3rBjoJ5ycnqZRuiETxF")
	reg.Register("key-A", pid)
	reg.Unregister("key-A")

	if _, ok := reg.Lookup("key-A"); ok {
		t.Fatal("lookup still present")
	}
	if _, ok := reg.Reverse(pid); ok {
		t.Fatal("reverse still present")
	}
}
```

Run: `go test ./internal/relay/ -run TestRegistry -v`
Expected: FAIL.

- [ ] **Step 2: Implement `registry.go`**

```go
// signaling-server/internal/relay/registry.go
package relay

import (
	"sync"

	"github.com/libp2p/go-libp2p/core/peer"
)

// AgentRegistry maps API key IDs ↔ agent libp2p peer IDs.
// Populated when an agent opens its libp2p connection to the relay
// (agent-side code in Slice 13b) and cleared on disconnect.
type AgentRegistry struct {
	mu      sync.RWMutex
	byKey   map[string]peer.ID // apiKeyID → peer.ID
	byPeer  map[peer.ID]string // peer.ID  → apiKeyID
}

func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{
		byKey:  make(map[string]peer.ID),
		byPeer: make(map[peer.ID]string),
	}
}

func (r *AgentRegistry) Register(apiKeyID string, pid peer.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byKey[apiKeyID] = pid
	r.byPeer[pid] = apiKeyID
}

func (r *AgentRegistry) Unregister(apiKeyID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pid, ok := r.byKey[apiKeyID]; ok {
		delete(r.byPeer, pid)
	}
	delete(r.byKey, apiKeyID)
}

func (r *AgentRegistry) Lookup(apiKeyID string) (peer.ID, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pid, ok := r.byKey[apiKeyID]
	return pid, ok
}

func (r *AgentRegistry) Reverse(pid peer.ID) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.byPeer[pid]
	return k, ok
}
```

Run: `go test ./internal/relay/ -run TestRegistry -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add signaling-server/internal/relay/
git commit -m "feat(slice-13a): agent registry"
```

---

## Task 6: Per-circuit byte counter

Wraps a `network.Stream` to count bytes forwarded in each direction. Used for quota accounting.

**Files:**
- Create: `signaling-server/internal/relay/counter.go`
- Create: `signaling-server/internal/relay/counter_test.go`

- [ ] **Step 1: Write failing test**

```go
// signaling-server/internal/relay/counter_test.go
package relay

import (
	"bytes"
	"io"
	"testing"
)

func TestCountingReader_countsBytes(t *testing.T) {
	src := bytes.NewReader([]byte("hello world"))
	cr := &CountingReader{R: src}
	out, _ := io.ReadAll(cr)
	if string(out) != "hello world" {
		t.Fatalf("got %q", out)
	}
	if cr.N != int64(len("hello world")) {
		t.Fatalf("count: got %d want %d", cr.N, len("hello world"))
	}
}

func TestCountingWriter_countsBytes(t *testing.T) {
	var buf bytes.Buffer
	cw := &CountingWriter{W: &buf}
	_, _ = cw.Write([]byte("abcde"))
	_, _ = cw.Write([]byte("fg"))
	if cw.N != 7 {
		t.Fatalf("count: got %d want 7", cw.N)
	}
}
```

Run: `go test ./internal/relay/ -run TestCounting -v`
Expected: FAIL.

- [ ] **Step 2: Implement `counter.go`**

```go
// signaling-server/internal/relay/counter.go
package relay

import (
	"io"
	"sync/atomic"
)

type CountingReader struct {
	R io.Reader
	N int64 // atomic
}

func (c *CountingReader) Read(p []byte) (int, error) {
	n, err := c.R.Read(p)
	atomic.AddInt64(&c.N, int64(n))
	return n, err
}

type CountingWriter struct {
	W io.Writer
	N int64 // atomic
}

func (c *CountingWriter) Write(p []byte) (int, error) {
	n, err := c.W.Write(p)
	atomic.AddInt64(&c.N, int64(n))
	return n, err
}
```

Run: `go test ./internal/relay/ -run TestCounting -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add signaling-server/internal/relay/
git commit -m "feat(slice-13a): byte counters for circuits"
```

---

## Task 7: Stream handler for `/sharebridge/relay/1.0.0`

This is the core of the relay. A browser opens this protocol stream, sends the JWT as the first line, the relay validates, then opens a second stream (same protocol) to the target agent, and copies bytes bidirectionally with counters.

**Files:**
- Create: `signaling-server/internal/relay/protocol.go`
- Create: `signaling-server/internal/relay/protocol_test.go`
- Modify: `signaling-server/internal/relay/relay.go` (register handler)

Protocol framing: the first frame is a length-prefixed JWT (2-byte big-endian length + token bytes). Subsequent bytes are opaque payload forwarded to the agent. Keep it tiny.

- [ ] **Step 1: Write the first failing test — reject no-JWT stream**

```go
// signaling-server/internal/relay/protocol_test.go
package relay

import (
	"context"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

const protoID = "/sharebridge/relay/1.0.0"

// helper to build a connected pair of libp2p hosts (in-memory via tcp/localhost)
func newTestHost(t *testing.T) libp2pHost {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// libp2pHost aliases host.Host for brevity in test code.
type libp2pHost = interface {
	ID() peer.ID
	Close() error
	Addrs() []multiaddr.Multiaddr
	NewStream(ctx context.Context, p peer.ID, pids ...any) (network.Stream, error)
	// ...etc
}
```

The test helper above is convoluted — replace with the direct import:

Replace the whole test file top matter with:
```go
// signaling-server/internal/relay/protocol_test.go
package relay

import (
	"context"
	"encoding/binary"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/multiformats/go-multiaddr"
)

const protoID = protocol.ID("/sharebridge/relay/1.0.0")

// connect wires h1 to h2 via h1.Peerstore and opens a test stream.
func connect(t *testing.T, h1, h2 host.Host) {
	t.Helper()
	h1.Peerstore().AddAddrs(h2.ID(), h2.Addrs(), time.Minute)
}

func newLibp2pHost(t *testing.T) host.Host {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func TestProtocol_rejectsShortJWT(t *testing.T) {
	relay := newLibp2pHost(t)
	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Minute)
	jti := NewJTIStore(time.Minute)
	defer jti.Close()
	reg := NewAgentRegistry()

	h := Handler{Issuer: issuer, JTIs: jti, Agents: reg, Host: relay}
	relay.SetStreamHandler(protoID, h.Handle)

	browser := newLibp2pHost(t)
	connect(t, browser, relay)

	s, err := browser.NewStream(context.Background(), relay.ID(), protoID)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	// Send a zero-length frame — should be rejected immediately.
	_ = binary.Write(s, binary.BigEndian, uint16(0))

	// Relay should close.
	_ = s.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	n, err := s.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("expected stream close; got %d bytes", n)
	}
}

// silence unused imports in the scaffold
var _ = multiaddr.NewMultiaddr
var _ = io.EOF
var _ sync.Mutex
var _ network.Stream
```

Run: `go test ./internal/relay/ -run TestProtocol_rejectsShortJWT -v`
Expected: FAIL — `Handler` undefined.

- [ ] **Step 2: Implement `protocol.go` — reject short/malformed framing**

```go
// signaling-server/internal/relay/protocol.go
package relay

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// ProtocolID is the sharebridge custom relay protocol.
const ProtocolID = protocol.ID("/sharebridge/relay/1.0.0")

// Handler implements the browser-facing side of the relay protocol.
type Handler struct {
	Issuer *Issuer
	JTIs   *JTIStore
	Agents *AgentRegistry
	Host   host.Host

	// OnCircuitClosed is fired with the per-circuit byte counts after the
	// circuit tears down. Used by the main server to accrue quota bytes.
	OnCircuitClosed func(apiKeyID, shareCode string, toAgent, fromAgent int64)

	// CodeToAPIKey resolves a share_code to the agent's API key ID. Injected
	// so the relay package has no PocketBase dependency.
	CodeToAPIKey func(shareCode string) (apiKeyID string, ok bool)
}

func (h *Handler) Handle(s network.Stream) {
	defer s.Close()

	claims, err := readAndValidateJWT(s, h.Issuer, h.JTIs)
	if err != nil {
		log.Printf("relay: JWT rejected from %s: %v", s.Conn().RemotePeer(), err)
		return
	}
	if !claims.RelayAllowed && !claims.DCUtRAllowed {
		log.Printf("relay: claim forbids both relay and dcutr")
		return
	}

	// Resolve share_code → API key → agent peer ID.
	apiKeyID, ok := h.CodeToAPIKey(claims.ShareCode)
	if !ok {
		log.Printf("relay: unknown share_code %q", claims.ShareCode)
		return
	}
	agentPID, ok := h.Agents.Lookup(apiKeyID)
	if !ok {
		log.Printf("relay: agent for api_key %s not connected", apiKeyID)
		return
	}

	// Open the agent-bound stream.
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()
	agentStream, err := h.Host.NewStream(dialCtx, agentPID, ProtocolID)
	if err != nil {
		log.Printf("relay: dial agent %s: %v", agentPID, err)
		return
	}
	defer agentStream.Close()

	// Only allow data forwarding if relay_allowed=true. If only dcutr_allowed,
	// let the libp2p DCUtR protocol run through a separate circuit path.
	if !claims.RelayAllowed {
		log.Printf("relay: data forwarding refused (relay_allowed=false) for share %s", claims.ShareCode)
		return
	}

	toAgent := &CountingWriter{W: agentStream}
	fromAgent := &CountingReader{R: agentStream}

	done := make(chan struct{}, 2)
	go func() { io.Copy(toAgent, s); done <- struct{}{} }()
	go func() { io.Copy(s, fromAgent); done <- struct{}{} }()
	<-done

	if h.OnCircuitClosed != nil {
		h.OnCircuitClosed(apiKeyID, claims.ShareCode, toAgent.N, fromAgent.N)
	}
}

// readAndValidateJWT reads a length-prefixed JWT and checks signature + JTI replay.
func readAndValidateJWT(s network.Stream, iss *Issuer, jtis *JTIStore) (*Claims, error) {
	var length uint16
	if err := binary.Read(s, binary.BigEndian, &length); err != nil {
		return nil, err
	}
	if length == 0 || length > 4096 {
		return nil, errors.New("invalid JWT length")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(s, buf); err != nil {
		return nil, err
	}
	claims, err := iss.Validate(string(buf))
	if err != nil {
		return nil, err
	}
	if !jtis.ConsumeOnce(claims.JTI) {
		return nil, errors.New("jti already used")
	}
	// Verify the browser peer ID in claims matches the stream's remote peer.
	if claims.BrowserPeerID != "" {
		remote := s.Conn().RemotePeer()
		claimed, err := peer.Decode(claims.BrowserPeerID)
		if err != nil || claimed != remote {
			return nil, errors.New("browser_peer_id does not match stream peer")
		}
	}
	return claims, nil
}
```

Run: `go test ./internal/relay/ -run TestProtocol_rejectsShortJWT -v`
Expected: PASS (the stream is closed because JWT length is 0).

- [ ] **Step 3: Test — end-to-end valid JWT + data forwarding**

Append to `protocol_test.go`:
```go
func TestProtocol_validJWT_forwardsBidirectionally(t *testing.T) {
	relayHost := newLibp2pHost(t)
	agentHost := newLibp2pHost(t)
	browserHost := newLibp2pHost(t)

	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Minute)
	jtis := NewJTIStore(time.Minute)
	defer jtis.Close()
	reg := NewAgentRegistry()
	reg.Register("api-key-xyz", agentHost.ID())

	h := Handler{
		Issuer: issuer, JTIs: jtis, Agents: reg, Host: relayHost,
		CodeToAPIKey: func(code string) (string, bool) {
			if code == "abc12345" {
				return "api-key-xyz", true
			}
			return "", false
		},
	}
	relayHost.SetStreamHandler(ProtocolID, h.Handle)

	// Agent: echo handler.
	var wg sync.WaitGroup
	wg.Add(1)
	agentHost.SetStreamHandler(ProtocolID, func(s network.Stream) {
		defer s.Close()
		defer wg.Done()
		io.Copy(s, s)
	})

	// Peerstore wiring so hosts can dial each other.
	relayHost.Peerstore().AddAddrs(agentHost.ID(), agentHost.Addrs(), time.Minute)
	browserHost.Peerstore().AddAddrs(relayHost.ID(), relayHost.Addrs(), time.Minute)

	// Issue JWT bound to the browser's peer ID.
	tok, err := issuer.Issue(Claims{
		ShareCode:     "abc12345",
		BrowserPeerID: browserHost.ID().String(),
		RelayAllowed:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := browserHost.NewStream(ctx, relayHost.ID(), ProtocolID)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	// Write length-prefixed JWT.
	if err := binary.Write(s, binary.BigEndian, uint16(len(tok))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte(tok)); err != nil {
		t.Fatal(err)
	}
	// Send payload, read echo.
	payload := []byte("ping-data")
	if _, err := s.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = s.CloseWrite()
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q want %q", got, payload)
	}
}
```

Run: `go test ./internal/relay/ -run TestProtocol_validJWT -v`
Expected: PASS.

- [ ] **Step 4: Test — replay rejected**

Append:
```go
func TestProtocol_rejectsJTIReplay(t *testing.T) {
	relayHost := newLibp2pHost(t)
	agentHost := newLibp2pHost(t)
	browserHost := newLibp2pHost(t)

	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Minute)
	jtis := NewJTIStore(time.Minute)
	defer jtis.Close()
	reg := NewAgentRegistry()
	reg.Register("k", agentHost.ID())

	h := Handler{
		Issuer: issuer, JTIs: jtis, Agents: reg, Host: relayHost,
		CodeToAPIKey: func(string) (string, bool) { return "k", true },
	}
	relayHost.SetStreamHandler(ProtocolID, h.Handle)
	agentHost.SetStreamHandler(ProtocolID, func(s network.Stream) { io.Copy(s, s) })

	relayHost.Peerstore().AddAddrs(agentHost.ID(), agentHost.Addrs(), time.Minute)
	browserHost.Peerstore().AddAddrs(relayHost.ID(), relayHost.Addrs(), time.Minute)

	tok, _ := issuer.Issue(Claims{
		ShareCode:     "abc12345",
		BrowserPeerID: browserHost.ID().String(),
		RelayAllowed:  true,
	})

	open := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s, err := browserHost.NewStream(ctx, relayHost.ID(), ProtocolID)
		if err != nil {
			return err
		}
		defer s.Close()
		_ = binary.Write(s, binary.BigEndian, uint16(len(tok)))
		s.Write([]byte(tok))
		s.Write([]byte("x"))
		s.CloseWrite()
		_, err = io.ReadAll(s)
		return err
	}
	if err := open(); err != nil {
		t.Fatalf("first open: %v", err)
	}
	// Second open must fail: JTI already consumed.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, _ := browserHost.NewStream(ctx, relayHost.ID(), ProtocolID)
	_ = binary.Write(s, binary.BigEndian, uint16(len(tok)))
	s.Write([]byte(tok))
	s.CloseWrite()
	got, _ := io.ReadAll(s)
	if len(got) != 0 {
		t.Fatalf("expected no echo on replayed jti, got %q", got)
	}
}
```

Run: `go test ./internal/relay/ -run TestProtocol_rejectsJTIReplay -v`
Expected: PASS.

- [ ] **Step 5: Wire handler registration into `Relay.New`**

Modify `relay.go`. Update `Relay` struct and `New`:

```go
type Relay struct {
	h       host.Host
	issuer  *Issuer
	jtis    *JTIStore
	agents  *AgentRegistry
	handler Handler
}

func New(ctx context.Context, cfg Config) (*Relay, error) {
	priv, err := loadOrGenerateKey(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load key: %w", err)
	}
	listen, err := multiaddr.NewMultiaddr(cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("parse listen addr: %w", err)
	}
	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.ListenAddrs(listen),
		libp2p.DisableRelay(),
	)
	if err != nil {
		return nil, fmt.Errorf("new libp2p host: %w", err)
	}
	if len(cfg.JWTSecret) < 32 {
		return nil, errors.New("JWTSecret must be >= 32 bytes")
	}
	r := &Relay{
		h:      h,
		issuer: NewIssuer(cfg.JWTSecret, cfg.JWTTTL),
		jtis:   NewJTIStore(cfg.JWTTTL),
		agents: NewAgentRegistry(),
	}
	r.handler = Handler{
		Issuer: r.issuer,
		JTIs:   r.jtis,
		Agents: r.agents,
		Host:   h,
	}
	return r, nil
}

// Issuer returns the JWT issuer (used by the browser_ws handler).
func (r *Relay) Issuer() *Issuer { return r.issuer }

// Agents returns the agent registry (used by agent handshake code in 13b).
func (r *Relay) Agents() *AgentRegistry { return r.agents }

// SetCodeResolver wires the share_code→api_key lookup (PocketBase-backed).
// Must be called before Start.
func (r *Relay) SetCodeResolver(f func(shareCode string) (apiKeyID string, ok bool)) {
	r.handler.CodeToAPIKey = f
}

// SetCircuitClosedHook sets the byte-count callback fired at circuit close.
func (r *Relay) SetCircuitClosedHook(f func(apiKeyID, shareCode string, toAgent, fromAgent int64)) {
	r.handler.OnCircuitClosed = f
}

// Start registers the protocol handler. Returns the listen multiaddrs.
func (r *Relay) Start() []multiaddr.Multiaddr {
	r.h.SetStreamHandler(ProtocolID, r.handler.Handle)
	return r.h.Addrs()
}

// Close stops the Host and releases resources.
func (r *Relay) Close() error {
	r.jtis.Close()
	return r.h.Close()
}
```

Add `JWTSecret []byte` and `JWTTTL time.Duration` to `Config`. Add `"errors"` and `"time"` imports.

Run: `go test ./internal/relay/ -v`
Expected: PASS. (`TestNewHost_*` tests set empty JWTSecret — fix by giving them a dummy secret.)

Update `TestNewHost_startsAndStops` and `TestNewHost_persistsIdentity` to include:
```go
JWTSecret: []byte("test-secret-do-not-use-in-prod-abcd1234"),
JWTTTL:    time.Minute,
```

Run tests again. Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add signaling-server/internal/relay/
git commit -m "feat(slice-13a): custom /sharebridge/relay/1.0.0 protocol handler"
```

---

## Task 8: Config updates — drop TURN, add relay settings

**Files:**
- Modify: `signaling-server/internal/config/config.go`
- Modify: `signaling-server/internal/config/config_test.go`

- [ ] **Step 1: Read current test file**

Run: `cat signaling-server/internal/config/config_test.go`
Identify which test cases reference `TurnHost`, `TurnSecret`, `HasTurn`, `TurnURL`.

- [ ] **Step 2: Rewrite config fields**

Replace the `Config` struct in `config.go` with:
```go
type Config struct {
	Port    string
	DataDir string

	// Relay (libp2p Host)
	RelayListenAddr     string        // libp2p multiaddr, e.g. "/ip4/127.0.0.1/tcp/9001/ws"
	RelayAnnounceAddr   string        // public multiaddr, e.g. "/dns4/relay.sharebridge.app/tcp/443/wss"
	RelayPrivateKeyPath string        // PEM file; empty = ephemeral (dev only)
	JWTSecret           []byte
	JWTTTL              time.Duration

	// SMTP (optional)
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string

	// Bandwidth quota
	DefaultQuotaGB     float64
	QuotaCheckInterval time.Duration
}
```

Update `Load`:
```go
func Load() *Config {
	ttl, _ := time.ParseDuration(getEnv("JWT_TTL", "5m"))
	return &Config{
		Port:                getEnv("PORT", "8080"),
		DataDir:             getEnv("DATA_DIR", "./pb_data"),
		RelayListenAddr:     getEnv("RELAY_LISTEN_ADDR", "/ip4/127.0.0.1/tcp/9001/ws"),
		RelayAnnounceAddr:   getEnv("RELAY_ANNOUNCE_ADDR", ""),
		RelayPrivateKeyPath: getEnv("RELAY_PRIVATE_KEY_PATH", ""),
		JWTSecret:           []byte(getEnv("JWT_SECRET", "")),
		JWTTTL:              ttl,
		SMTPHost:            getEnv("SMTP_HOST", ""),
		SMTPPort:            getEnv("SMTP_PORT", "587"),
		SMTPUser:            getEnv("SMTP_USER", ""),
		SMTPPassword:        getEnv("SMTP_PASSWORD", ""),
		DefaultQuotaGB:      getEnvFloat("DEFAULT_QUOTA_GB", 50.0),
		QuotaCheckInterval:  getEnvDuration("QUOTA_CHECK_INTERVAL", 5*time.Minute),
	}
}
```

Delete `HasTurn()` and `TurnURL()`.

Remove the `PROMETHEUS_URL` env read — quota accounting now comes from relay byte counts.

Remove `PrometheusURL` from the struct.

- [ ] **Step 3: Update `config_test.go`**

Delete all tests that reference `TurnHost`, `TurnPort`, `TurnSecret`, `HasTurn`, `TurnURL`, `PrometheusURL`. Add new tests covering the relay fields.

Append:
```go
func TestLoad_relayDefaults(t *testing.T) {
	t.Setenv("RELAY_LISTEN_ADDR", "")
	cfg := Load()
	if cfg.RelayListenAddr != "/ip4/127.0.0.1/tcp/9001/ws" {
		t.Fatalf("default RelayListenAddr: %q", cfg.RelayListenAddr)
	}
	if cfg.JWTTTL != 5*time.Minute {
		t.Fatalf("default JWTTTL: %v", cfg.JWTTTL)
	}
}

func TestLoad_jwtSecretFromEnv(t *testing.T) {
	t.Setenv("JWT_SECRET", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	cfg := Load()
	if len(cfg.JWTSecret) < 32 {
		t.Fatalf("JWTSecret len: %d", len(cfg.JWTSecret))
	}
}
```

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/internal/config/
git commit -m "refactor(slice-13a): replace TURN config with relay config"
```

---

## Task 9: Update browser_ws to issue JWTs, drop ICE/SDP

**Files:**
- Modify: `signaling-server/internal/handler/browser_ws.go`
- Modify: `signaling-server/internal/handler/browser_ws_test.go`

- [ ] **Step 1: Read existing test**

Run: `cat signaling-server/internal/handler/browser_ws_test.go | head -200`
Identify tests that assert on `ice_config` messages or `answer`/`ice_candidate` forwarding. Those tests must be rewritten or deleted.

- [ ] **Step 2: Add a test asserting knock is forwarded to the agent**

Replace or add in `browser_ws_test.go` (keep existing helpers):
```go
func TestBrowserWS_knockForwardedToAgent(t *testing.T) {
	// Setup: in-memory PB app with a session (no-password — server can't know
	// either way, so behaviour is identical).
	app := tests.NewTestApp(t.TempDir())
	defer app.Cleanup()
	// seed the session and agent connection inline using PocketBase record API:
	// app.Dao().SaveRecord(...)

	hub := hub.New()
	rly := newTestRelay(t) // in-memory relay for tests
	cfg := &config.Config{
		RelayAnnounceAddr: "/ip4/127.0.0.1/tcp/9001/ws",
		JWTSecret:         []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:            time.Minute,
	}

	server := httptest.NewServer(http.HandlerFunc(handler.BrowserWS(app, hub, cfg, rly)))
	defer server.Close()

	// Dial WS, send knock with browser_peer_id.
	// Assert the hub received a knock message destined for the agent.
	// Do NOT assert relay_info here — that only arrives after auth_ok from the agent (Task 10).
}
```

Note: JWT issuance is **not** tested here — it happens in `agent_ws.go` on receipt of `auth_ok` (Task 10). A helper `newTestRelay` constructs an in-memory relay for tests.

- [ ] **Step 3: Change `BrowserWS` signature and message flow**

Replace the entire `browser_ws.go` with a version that removes TURN and emits `relay_info`:

```go
package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/relay"
)

type browserMsg struct {
	Type   string `json:"type"`
	HMAC   string `json:"hmac,omitempty"`
	PeerID string `json:"peer_id,omitempty"` // libp2p peer ID of the browser
}

func BrowserWS(app core.App, sessionHub *hub.Hub, cfg *config.Config, rly *relay.Relay) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionCode := r.URL.Query().Get("session")
		sessionRecord, err := loadValidSession(app, sessionCode)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
			return
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			log.Printf("browser_ws accept: %v", err)
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()

		if _, agentConn := sessionHub.GetAgentConn(sessionCode); !agentConn {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			return
		}
		if err := sessionHub.PairSession(sessionCode, conn); err != nil {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			return
		}
		defer sessionHub.UnpairSession(sessionCode)

		connID := generateConnID()
		sessionHub.RegisterBrowserConn(connID, conn)
		defer sessionHub.UnregisterBrowserConn(connID)

		apiKeyID := sessionRecord.GetString("api_key_id")
		accountRecord, err := lookupAccountForAPIKey(app, apiKeyID)
		if err != nil {
			log.Printf("browser_ws: lookup account: %v", err)
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "internal error"})
			return
		}

		quotaExceeded, _ := checkRelayQuota(accountRecord)
		relayOnly := sessionRecord.GetBool("relay_only")
		if relayOnly && quotaExceeded {
			hub.SendDirect(ctx, conn, map[string]string{
				"type":    "error",
				"message": "file host's relay quota exceeded - this share requires relay which is unavailable",
			})
			conn.Close(websocket.StatusNormalClosure, "relay quota exceeded")
			return
		}

		// Main message loop. JWT is issued in agent_ws when the agent sends auth_ok —
		// not here. The server does not know whether the share has a password; that
		// knowledge lives on the agent only.
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var msg browserMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "knock":
				// Remember browser peer ID so we can bind it into the JWT when auth_ok arrives.
				if msg.PeerID != "" {
					sessionHub.RememberBrowserPeerID(connID, msg.PeerID)
				}
				// Forward unconditionally to agent to trigger nonce → join → auth_ok flow.
				sessionHub.SendToAgent(ctx, apiKeyID, map[string]any{
					"type":    "knock",
					"conn_id": connID,
					"code":    sessionCode,
				})

			case "join":
				// Forward HMAC to agent for verification. JWT is issued when agent replies auth_ok.
				if msg.PeerID != "" {
					sessionHub.RememberBrowserPeerID(connID, msg.PeerID)
				}
				sessionHub.SendToAgent(ctx, apiKeyID, map[string]any{
					"type":    "join",
					"conn_id": connID,
					"code":    sessionCode,
					"hmac":    msg.HMAC,
				})
			}
		}
	}
}

// issueAndSend is NOT in browser_ws.go. JWT issuance lives in agent_ws.go
// inside the auth_ok handler (Task 10) — the server does not know whether a
// share has a password and therefore cannot issue a JWT at knock time.
```

This sketch references helpers that don't exist yet:
- `loadValidSession(app, code)` — factor out the existing session lookup + expiry check.
- `lookupAccountForAPIKey(app, apiKeyID)` — factor out the api_keys → users join.
- `sessionHub.RememberBrowserPeerID(connID, peerID)` — new hub method added in Step 4.
- `rly.Host()` — add this accessor on `relay.Relay`.

Extract `loadValidSession` and `lookupAccountForAPIKey` from inline code in the current file.

Run: `go build ./...`
Expected: compile errors identifying missing pieces — fix them in Step 4.

- [ ] **Step 4: Add `RememberBrowserPeerID` to hub + JWT-on-auth-ok path**

Add to `hub.go`:
```go
func (h *Hub) RememberBrowserPeerID(connID, peerID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connPeerIDs == nil {
		h.connPeerIDs = make(map[string]string)
	}
	h.connPeerIDs[connID] = peerID
}

func (h *Hub) GetBrowserPeerID(connID string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	p, ok := h.connPeerIDs[connID]
	return p, ok
}
```
Add `connPeerIDs map[string]string` to the `Hub` struct, and clear it in `UnregisterBrowserConn`.

The password-share flow also needs a new agent → server message for "HMAC verified, please issue JWT". The current flow has the agent send `nonce` and receive `join` (with HMAC); on verification success the agent currently moves straight to SDP exchange. We need the agent (in 13b) to instead send `auth_ok { conn_id }` — in 13a we only add the server-side receive path.

In `agent_ws.go` (after Task 10), a new case `"auth_ok"` will: look up peerID for connID, forward a `relay_info` to the browser.

Add `rly.Host()` accessor on `Relay`:
```go
func (r *Relay) Host() host.Host { return r.h }
```
(already added in Task 7 — verify.)

- [ ] **Step 5: Run unit tests**

Run: `go test ./internal/handler/ -run TestBrowserWS -v`
Expected: PASS (new test) — likely requires iterating on the helper `newTestRelay`.

- [ ] **Step 6: Commit**

```bash
git add signaling-server/internal/handler/browser_ws.go signaling-server/internal/handler/browser_ws_test.go signaling-server/internal/hub/hub.go
git commit -m "feat(slice-13a): issue relay JWTs from browser_ws, drop ICE/SDP"
```

---

## Task 10: Update agent_ws — drop ICE/SDP, add auth_ok

**Files:**
- Modify: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/internal/handler/agent_ws_test.go`

- [ ] **Step 1: Replace agentMsg + switch**

In `agent_ws.go`:
- Remove `SDP`, `Candidate` fields from `agentMsg`.
- Remove `case "offer":`, `case "ice_candidate":`.
- Remove the `refreshQuota` closure and the quota state vars (server-side quota is now consumed via relay byte counts — poller change in Task 12).
- Remove `isRelayCandidate`.
- Replace the welcome message send with a relay-info variant:

Replace `handleHello`:
```go
func handleHello(ctx context.Context, conn *websocket.Conn, h *hub.Hub, apiKeyID, accountID, agentID string, cfg *config.Config, rly *relay.Relay) {
	if agentID == "" {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent_id required"})
		return
	}
	h.RegisterAgent(apiKeyID, conn)
	log.Printf("agent hello received: api_key_id=%s agent_id=%s", apiKeyID, agentID)

	hub.SendDirect(ctx, conn, map[string]any{
		"type":             "welcome",
		"relay_multiaddr":  cfg.RelayAnnounceAddr + "/p2p/" + rly.Host().ID().String(),
		"stun_servers":     []string{"stun:stun.cloudflare.com:3478"},
	})
}
```

- Add new `auth_ok` case:
```go
case "auth_ok":
	// Agent has verified the browser's HMAC. Issue a JWT and forward relay_info to the browser.
	peerID, ok := h.GetBrowserPeerID(msg.ConnID)
	if !ok {
		continue
	}
	// Look up session for relay_only flag.
	records, _ := app.FindRecordsByFilter("sessions", "code = {:code}", "", 1, 0, map[string]any{"code": msg.Code})
	relayOnly := false
	if len(records) > 0 {
		relayOnly = records[0].GetBool("relay_only")
	}
	// Quota check.
	accountRecord, _ := app.FindRecordById("users", accountID)
	quotaExceeded, _ := checkRelayQuota(accountRecord)
	if relayOnly && quotaExceeded {
		h.CloseBrowserConnWithError(ctx, msg.ConnID, "file host's relay quota exceeded")
		continue
	}
	tok, err := rly.Issuer().Issue(relay.Claims{
		ShareCode:     msg.Code,
		BrowserPeerID: peerID,
		RelayAllowed:  !quotaExceeded,
		DCUtRAllowed:  !relayOnly,
	})
	if err != nil {
		log.Printf("issue: %v", err)
		continue
	}
	h.ForwardToBrowserByConnID(ctx, msg.ConnID, map[string]any{
		"type":            "relay_info",
		"relay_multiaddr": cfg.RelayAnnounceAddr + "/p2p/" + rly.Host().ID().String(),
		"jwt":             tok,
		"relay_allowed":   !quotaExceeded,
		"dcutr_allowed":   !relayOnly,
	})
```

- Change the function signature:
```go
func AgentWS(app core.App, h *hub.Hub, cfg *config.Config, rly *relay.Relay) http.HandlerFunc {
```

- [ ] **Step 2: Update the existing `agent_ws_test.go` tests**

Read the current file (`wc -l signaling-server/internal/handler/agent_ws_test.go`). Any test that:
- Sends `offer` / `ice_candidate`: DELETE.
- Asserts `welcome` → `ice_servers`: REPLACE with assertion that welcome contains `relay_multiaddr` string.
- Needs a `*relay.Relay` argument: pass `newTestRelay(t)`.

Add one new test:
```go
func TestAgentWS_authOkIssuesRelayInfoToBrowser(t *testing.T) {
	// Setup agent connects and sends hello,
	// Simulate browser WS registered under connID with a peer_id stored via RememberBrowserPeerID.
	// Send auth_ok { conn_id, code }.
	// Assert the browser WS receives a `relay_info` message with a valid JWT.
}
```

Run: `go test ./internal/handler/ -run TestAgentWS -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add signaling-server/internal/handler/agent_ws.go signaling-server/internal/handler/agent_ws_test.go
git commit -m "feat(slice-13a): agent_ws drops ICE/SDP, adds auth_ok → relay_info path"
```

---

## Task 11: Delete `internal/turn/` and remove references

**Files:**
- Delete: `signaling-server/internal/turn/credentials.go`
- Delete: `signaling-server/internal/turn/credentials_test.go`
- Delete: `signaling-server/internal/turn/ice_config.go`
- Delete: `signaling-server/internal/turn/ice_config_test.go`
- Modify: any remaining importer

- [ ] **Step 1: Delete files**

```bash
rm -rf signaling-server/internal/turn/
```

- [ ] **Step 2: Find stragglers**

Use Grep for `internal/turn` across the repo; any remaining import → remove or replace.

- [ ] **Step 3: Build**

Run: `cd signaling-server && go build ./...`
Expected: success.

- [ ] **Step 4: Commit**

```bash
git add -A signaling-server/internal/turn
git commit -m "refactor(slice-13a): delete internal/turn package"
```

---

## Task 12: Quota poller — switch from Prometheus to relay-reported bytes

The current quota poller (`internal/quota/poller.go`) pulls byte counts from Prometheus (coturn exporter). Replace with in-process event sink fed by `Relay.SetCircuitClosedHook`.

**Files:**
- Modify: `signaling-server/internal/quota/poller.go`
- Modify: `signaling-server/internal/quota/poller_test.go`
- Modify: `signaling-server/cmd/server/main.go` (hook wiring)
- Delete: `signaling-server/internal/metrics/prometheus.go`, `prometheus_test.go` (Prometheus client no longer used)

- [ ] **Step 1: Read current poller**

Run: `cat signaling-server/internal/quota/poller.go`
Understand: the poller runs on `QuotaCheckInterval`, fetches bytes per account from Prometheus, updates `users.current_period_usage_gb`.

- [ ] **Step 2: Replace with event accumulator**

Rewrite `poller.go`:
```go
// signaling-server/internal/quota/poller.go
package quota

import (
	"log"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
)

// Accumulator receives per-circuit byte counts and flushes to PocketBase periodically.
type Accumulator struct {
	app    core.App
	cfg    *config.Config
	mu     sync.Mutex
	buf    map[string]int64 // apiKeyID → bytes accrued since last flush
	stop   chan struct{}
}

func NewAccumulator(app core.App, cfg *config.Config) *Accumulator {
	return &Accumulator{
		app:  app,
		cfg:  cfg,
		buf:  make(map[string]int64),
		stop: make(chan struct{}),
	}
}

// Record adds bytes to the per-apiKeyID bucket. Safe for concurrent callers.
// Bytes in both directions are summed: for quota purposes, every circuited byte counts.
func (a *Accumulator) Record(apiKeyID string, toAgent, fromAgent int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.buf[apiKeyID] += toAgent + fromAgent
}

func (a *Accumulator) Start() {
	go a.run()
}

func (a *Accumulator) Stop() { close(a.stop) }

func (a *Accumulator) run() {
	t := time.NewTicker(a.cfg.QuotaCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.flush()
		case <-a.stop:
			a.flush()
			return
		}
	}
}

func (a *Accumulator) flush() {
	a.mu.Lock()
	snapshot := a.buf
	a.buf = make(map[string]int64)
	a.mu.Unlock()

	for apiKeyID, bytes := range snapshot {
		if bytes == 0 {
			continue
		}
		apiKey, err := a.app.FindRecordById("api_keys", apiKeyID)
		if err != nil {
			log.Printf("quota flush: api_keys %s: %v", apiKeyID, err)
			continue
		}
		accountID := apiKey.GetString("account_id")
		acct, err := a.app.FindRecordById("users", accountID)
		if err != nil {
			log.Printf("quota flush: users %s: %v", accountID, err)
			continue
		}
		currentGB := acct.GetFloat("current_period_usage_gb")
		addGB := float64(bytes) / (1024 * 1024 * 1024)
		acct.Set("current_period_usage_gb", currentGB+addGB)
		if err := a.app.Save(acct); err != nil {
			log.Printf("quota flush: save account %s: %v", accountID, err)
		}
	}
}
```

- [ ] **Step 3: Rewrite `poller_test.go`**

```go
package quota

import (
	"testing"
	"time"

	"sharebridge/server/internal/config"
)

func TestAccumulator_flushesToAccount(t *testing.T) {
	app := tests.NewTestApp(t.TempDir()) // from github.com/pocketbase/pocketbase/tests
	defer app.Cleanup()
	apiKeyID, accountID := seedAPIKeyAndAccount(t, app)

	acc := NewAccumulator(app, &config.Config{QuotaCheckInterval: time.Millisecond})
	acc.Start()
	defer acc.Stop()

	acc.Record(apiKeyID, 500*1024*1024, 500*1024*1024) // 1 GB total

	time.Sleep(50 * time.Millisecond)
	rec, _ := app.FindRecordById("users", accountID)
	got := rec.GetFloat("current_period_usage_gb")
	if got < 0.99 || got > 1.01 {
		t.Fatalf("usage got %f want ≈1.0", got)
	}
}
```

- [ ] **Step 4: Delete Prometheus metrics client**

```bash
rm signaling-server/internal/metrics/prometheus.go
rm signaling-server/internal/metrics/prometheus_test.go
```
If `metrics/` becomes empty, remove the directory. Remove the import from `cmd/server/main.go`.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/quota/ signaling-server/internal/metrics/
git commit -m "refactor(slice-13a): quota poller switches to relay byte events"
```

---

## Task 13: Wire it all up in `cmd/server/main.go`

**Files:**
- Modify: `signaling-server/cmd/server/main.go`

- [ ] **Step 1: Construct + start the relay, plumb into handlers and accumulator**

Replace the `OnServe` binding body. Key additions:
```go
// After cfg + hub:
ctx := context.Background()
rly, err := relay.New(ctx, relay.Config{
	ListenAddr:     cfg.RelayListenAddr,
	AnnounceAddr:   cfg.RelayAnnounceAddr,
	PrivateKeyPath: cfg.RelayPrivateKeyPath,
	JWTSecret:      cfg.JWTSecret,
	JWTTTL:         cfg.JWTTTL,
})
if err != nil {
	log.Fatalf("relay: %v", err)
}
rly.SetCodeResolver(func(code string) (string, bool) {
	records, err := app.FindRecordsByFilter("sessions", "code = {:code}", "", 1, 0, map[string]any{"code": code})
	if err != nil || len(records) == 0 {
		return "", false
	}
	return records[0].GetString("api_key_id"), true
})

acc := quota.NewAccumulator(app, cfg)
acc.Start()

rly.SetCircuitClosedHook(func(apiKeyID, shareCode string, toAgent, fromAgent int64) {
	acc.Record(apiKeyID, toAgent, fromAgent)
})

rly.Start()
log.Printf("relay host %s listening on %v", rly.Host().ID(), rly.Host().Addrs())
```

Update handler wiring:
```go
router.GET("/ws/agent", func(e *core.RequestEvent) error {
	authMiddleware := middleware.APIKeyAuth(app)
	handlerFunc := handler.AgentWS(app, h, cfg, rly)
	authMiddleware(http.HandlerFunc(handlerFunc)).ServeHTTP(e.Response, e.Request)
	return nil
})
router.GET("/ws/client", func(e *core.RequestEvent) error {
	handler.BrowserWS(app, h, cfg, rly)(e.Response, e.Request)
	return nil
})
```

Remove the `metrics.NewPrometheusClient` block and the `if cfg.HasTurn()` branch around the quota poller.

Add shutdown:
```go
app.OnTerminate().BindFunc(func(_ *core.TerminateEvent) error {
	acc.Stop()
	return rly.Close()
})
```

- [ ] **Step 2: Build**

Run: `cd signaling-server && go build ./...`
Expected: success.

- [ ] **Step 3: Commit**

```bash
git add signaling-server/cmd/server/main.go
git commit -m "feat(slice-13a): wire relay + accumulator into server main"
```

---

## Task 14: End-to-end integration test — standalone go-libp2p client

Prove browser→relay→agent forwarding works without any agent or browser code.

**Files:**
- Create: `signaling-server/internal/relay/integration_test.go`

- [ ] **Step 1: Write the test**

```go
// signaling-server/internal/relay/integration_test.go
package relay_test

import (
	"context"
	"encoding/binary"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"

	"sharebridge/server/internal/relay"
)

func TestIntegration_browserRelayAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Relay.
	rly, err := relay.New(ctx, relay.Config{
		ListenAddr:     "/ip4/127.0.0.1/tcp/0",
		JWTSecret:      []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:         time.Minute,
	})
	if err != nil {
		t.Fatalf("relay.New: %v", err)
	}
	defer rly.Close()

	// Agent.
	agent, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	defer agent.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	agent.SetStreamHandler(relay.ProtocolID, func(s network.Stream) {
		defer s.Close()
		defer wg.Done()
		io.Copy(s, s)
	})

	// Wire up.
	rly.Agents().Register("api-key-A", agent.ID())
	rly.SetCodeResolver(func(c string) (string, bool) {
		if c == "test-share" {
			return "api-key-A", true
		}
		return "", false
	})

	var closed struct {
		sync.Mutex
		toAgent, fromAgent int64
	}
	rly.SetCircuitClosedHook(func(_, _ string, to, from int64) {
		closed.Lock()
		closed.toAgent = to
		closed.fromAgent = from
		closed.Unlock()
	})
	rly.Start()

	// Peerstore: tell relay where agent lives.
	rly.Host().Peerstore().AddAddrs(agent.ID(), agent.Addrs(), time.Minute)

	// Browser dials relay.
	browser, _ := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	defer browser.Close()
	browser.Peerstore().AddAddrs(rly.Host().ID(), rly.Host().Addrs(), time.Minute)

	// Mint JWT.
	tok, err := rly.Issuer().Issue(relay.Claims{
		ShareCode:     "test-share",
		BrowserPeerID: browser.ID().String(),
		RelayAllowed:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	s, err := browser.NewStream(ctx, rly.Host().ID(), relay.ProtocolID)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	_ = binary.Write(s, binary.BigEndian, uint16(len(tok)))
	s.Write([]byte(tok))
	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	if _, err := s.Write(payload); err != nil {
		t.Fatal(err)
	}
	s.CloseWrite()

	echoed, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if len(echoed) != len(payload) {
		t.Fatalf("echo length: got %d want %d", len(echoed), len(payload))
	}
	for i := range echoed {
		if echoed[i] != payload[i] {
			t.Fatalf("echo byte %d: got %d want %d", i, echoed[i], payload[i])
		}
	}

	// Stream closed — counter hook should have fired.
	time.Sleep(100 * time.Millisecond)
	closed.Lock()
	defer closed.Unlock()
	if closed.toAgent != int64(len(payload)) {
		t.Errorf("toAgent bytes: got %d want %d", closed.toAgent, len(payload))
	}
	if closed.fromAgent != int64(len(payload)) {
		t.Errorf("fromAgent bytes: got %d want %d", closed.fromAgent, len(payload))
	}
}
```

Run: `go test ./internal/relay/ -run TestIntegration_browserRelayAgent -v`
Expected: PASS.

- [ ] **Step 2: Commit**

```bash
git add signaling-server/internal/relay/integration_test.go
git commit -m "test(slice-13a): e2e relay integration with stub agent"
```

---

## Task 15: Deployment — Caddy + firewall + coturn decommission

These steps are manual / infra; not TDD. Documented so the engineer executes them in order against staging first.

- [ ] **Step 1: Caddy config — add relay route without Cloudflare allowlist**

In the Caddy configuration file (location: see `deploy/` or `docs/` in the repo for the current Caddyfile):

```
relay.sharebridge.app {
    reverse_proxy localhost:9001
}
```

Do NOT include the Cloudflare IP allowlist that guards `sharebridge.app`. This route must accept connections from the public internet.

- [ ] **Step 2: DNS**

In Cloudflare, add an A or CNAME record for `relay.sharebridge.app` pointing to the VPS public IP. Set proxy status to **DNS only** (grey cloud). File transfer data must not flow through Cloudflare.

- [ ] **Step 3: Firewall**

On the VPS:
```bash
# Block external access to 9001 (relay binds to 127.0.0.1 already, but belt-and-suspenders).
sudo ufw deny 9001/tcp
```

- [ ] **Step 4: Environment**

On the staging signaling server host, set:
```
RELAY_LISTEN_ADDR=/ip4/127.0.0.1/tcp/9001/ws
RELAY_ANNOUNCE_ADDR=/dns4/relay.sharebridge.app/tcp/443/wss
RELAY_PRIVATE_KEY_PATH=/var/lib/sharebridge/relay.key
JWT_SECRET=<openssl rand -hex 32>
JWT_TTL=5m
```

Remove: `TURN_HOST`, `TURN_PORT`, `TURN_SECRET`, `PROMETHEUS_URL`.

- [ ] **Step 5: Deploy signaling server**

Ship the new binary. Verify `relay host 12D3Koo... listening on [...]` appears in logs.

- [ ] **Step 6: Decommission coturn**

After confirming the relay works:
```bash
sudo systemctl stop coturn
sudo systemctl disable coturn
sudo apt remove coturn
```
Remove the coturn Caddy route and DNS for `turn.sharebridge.app`.

- [ ] **Step 7: Document**

Update `README.md` or `docs/deployment.md` — remove TURN/coturn sections, add relay section referencing the new envs and Caddy route.

- [ ] **Step 8: Commit infra changes**

```bash
git add deploy/ docs/
git commit -m "chore(slice-13a): deploy relay, decommission coturn"
```

---

## Self-review checklist

Before marking 13a done, re-confirm:

- [ ] Every section in the spec under "13a — Relay + Token Issuance" maps to at least one task above.
- [ ] `internal/turn/` is gone.
- [ ] `internal/metrics/prometheus.go` is gone.
- [ ] `go build ./...` and `go test ./...` pass in `signaling-server/`.
- [ ] A standalone libp2p test client can: dial relay → send JWT → forward bytes to a stub agent → receive echo. The integration test proves this.
- [ ] Relay listen addr is loopback; external access blocked by firewall + Caddy TLS termination only.
- [ ] The JWT spec matches `## JWT Structure` in the design doc (jti, share_code, browser_peer_id, relay_allowed, dcutr_allowed, exp).
- [ ] JTI store is in-memory only; 5-minute TTL; pruned in a goroutine.
- [ ] Agent and browser code are unchanged in 13a (they break until 13b/13c ship — acceptable because this is a clean-cutover).

---

## Deliverable

A signaling server binary that:
1. Starts a libp2p Host on `127.0.0.1:9001/ws` behind Caddy at `relay.sharebridge.app`.
2. Issues JWTs to browsers after the HMAC pre-challenge.
3. Validates JWTs + JTI uniqueness when browsers open `/sharebridge/relay/1.0.0` streams.
4. Forwards bytes bidirectionally between the browser stream and the agent stream.
5. Reports byte counts into the quota accumulator for per-account GB tracking.
6. Does NOT contain any WebRTC, SDP, ICE, TURN, or coturn logic.

Ready for Slice 13b (agent transport replacement).

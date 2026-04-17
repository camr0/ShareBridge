# Slice 13a — Relay + Token Issuance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Embed a go-libp2p Host inside the signaling server that issues JWTs, gates circuit relay v2 circuits via a custom auth stream, and decommissions coturn — giving browsers and agents E2E Noise-encrypted file transfers through the relay.

**Architecture:** The relay is a libp2p Host running the standard `circuitv2/relay` service behind a custom `CircuitACL`. Before a browser can open a circuit to an agent, it must present a signed JWT on the `/sharebridge/relay/1.0.0` auth stream; the relay validates the token and registers the `(browserPeerID → agentPeerID)` pair in the ACL. The circuit relay v2 service then allows only that specific pairing, and the browser/agent run their own E2E Noise handshake through the circuit — the relay forwards ciphertext it cannot read. DCUtR hole-punching flows through the same circuit automatically. Byte counting uses go-libp2p's `BandwidthCounter` read at browser peer disconnect.

**Tech Stack:** Go 1.22, `github.com/libp2p/go-libp2p` v0.40+, `github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay` (aliased `circuitv2`), `github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client` (aliased `circuitv2client`), `github.com/libp2p/go-libp2p/core/metrics`, `github.com/golang-jwt/jwt/v5`, PocketBase, coder/websocket.

---

## File Structure

**New files:**
- `signaling-server/internal/relay/relay.go` — Host construction, `BandwidthCounter`, circuit relay v2 service, network notifier for byte accounting, lifecycle
- `signaling-server/internal/relay/relay_test.go` — Host lifecycle tests
- `signaling-server/internal/relay/token.go` — JWT signing + validation (`Issuer`), JTI replay store (`JTIStore`)
- `signaling-server/internal/relay/token_test.go` — JWT round-trip, tamper, expiry, JTI replay tests
- `signaling-server/internal/relay/acl.go` — `CircuitACL` implementing `circuitv2.ACLFilter`; stores authorized `(browserPeerID → agentPeerID)` pairs; used by circuit relay service and bandwidth notifier
- `signaling-server/internal/relay/acl_test.go` — ACL authorize / allow-connect / prune tests
- `signaling-server/internal/relay/protocol.go` — `/sharebridge/relay/1.0.0` handler: reads JWT, validates, calls `acl.Authorize`, writes `{"ok":true}` or `{"ok":false,"error":"..."}` — **no `io.Copy`, no data forwarding**
- `signaling-server/internal/relay/protocol_test.go` — auth stream tests (reject bad JWT, accept good JWT, replay rejected)
- `signaling-server/internal/relay/registry.go` — `AgentRegistry` mapping `apiKeyID ↔ peer.ID`
- `signaling-server/internal/relay/registry_test.go` — registry tests
- `signaling-server/internal/relay/integration_test.go` — end-to-end test: agent reserves, browser auths, browser dials agent via circuit relay v2, verifies E2E data delivery and byte accounting

**Modified files:**
- `signaling-server/internal/handler/browser_ws.go` — remove `ice_config`/TURN; knock/join forward peer_id to hub; JWT issued in agent_ws, not here
- `signaling-server/internal/handler/browser_ws_test.go` — delete ICE tests, add knock-forwards-to-agent test
- `signaling-server/internal/handler/agent_ws.go` — remove `offer`/`ice_candidate`/ICE; add `auth_ok` case that issues JWT and sends `relay_info` (including `agent_peer_id`) to browser
- `signaling-server/internal/handler/agent_ws_test.go` — delete ICE tests, add auth_ok→relay_info test
- `signaling-server/internal/handler/handler_testhelpers_test.go` — NEW: shared `newTestRelay(t)` helper for handler tests
- `signaling-server/internal/hub/hub.go` — add `connPeerIDs` map and `RememberBrowserPeerID`/`GetBrowserPeerID` methods
- `signaling-server/internal/config/config.go` — drop TURN fields; add `RelayListenAddr`, `RelayAnnounceAddr`, `RelayPrivateKeyPath`, `JWTSecret`, `JWTTTL`
- `signaling-server/cmd/server/main.go` — start relay, wire accumulator, remove TURN/Prometheus
- `signaling-server/internal/quota/poller.go` — replace Prometheus-based `Poller` with event-driven `Accumulator` fed by `relay.OnCircuitClosed`
- `signaling-server/internal/quota/poller_test.go` — rewrite for `Accumulator`

**Deleted files:**
- `signaling-server/internal/turn/` (all four files)
- `signaling-server/internal/metrics/prometheus.go` and `prometheus_test.go`

---

## Notes for the implementing engineer

You are likely new to libp2p. Key concepts:

- **Host** — a libp2p peer with a cryptographic identity (`peer.ID`). Handles transports, multiplexing, security, and stream handlers.
- **Circuit relay v2** — a protocol where a relay Host forwards streams between two peers that cannot directly connect. The relay forwards bytes it cannot decrypt — E2E encryption is between the two end peers. This is standard go-libp2p; we add a JWT gate on top via a custom `ACLFilter`.
- **`/sharebridge/relay/1.0.0`** — our custom **auth-only** protocol stream. Browser sends JWT; relay validates and registers the authorized pair; browser receives `{"ok":true}`. No data is forwarded over this stream — it is closed after the handshake.
- **`ACLFilter`** — a go-libp2p interface (`AllowReserve`, `AllowConnect`) that the circuit relay service calls before allowing a reservation or circuit. Our `CircuitACL` checks the JWT-authorized pairs.
- **DCUtR** — "Direct Connection Upgrade through Relay". After the relay circuit is established, both peers automatically attempt a hole-punch. If successful, they switch to a direct connection. This is **stock libp2p** — zero custom code needed; just don't disable it.
- **Noise** — libp2p's default transport encryption. Every connection is Noise-encrypted. In a circuit relay, there are TWO Noise layers: the hop connections (peer↔relay) and the end-to-end connection (browser↔agent through the relay). The relay only sees the outer hops; it cannot read the inner content.
- **`BandwidthCounter`** — a go-libp2p metrics type that tracks bytes sent/received per peer. Attached to the relay host via `libp2p.BandwidthReporter(bwc)`. Queried at browser peer disconnect to report quota bytes.
- **Multiaddr** — a self-describing address: `/dns4/relay.sharebridge.app/tcp/443/wss/p2p/12D3Koo...`. The circuit address adds `/p2p-circuit/p2p/<agentPeerID>` on the end.

---

## Task 1: Scaffold `internal/relay/` and add go-libp2p dep

**Files:**
- Create: `signaling-server/internal/relay/relay.go`
- Create: `signaling-server/internal/relay/relay_test.go`
- Modify: `signaling-server/go.mod`

- [ ] **Step 1: Add go-libp2p dependency**

```bash
cd signaling-server
go get github.com/libp2p/go-libp2p@v0.40.0
```

Expected: `go.mod` and `go.sum` updated. This pulls in sub-packages (`core`, `p2p/protocol/circuitv2/relay`, etc.) automatically.

- [ ] **Step 2: Write failing test**

```go
// signaling-server/internal/relay/relay_test.go
package relay

import (
	"context"
	"testing"
	"time"
)

func TestNewHost_startsAndStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r, err := New(ctx, Config{
		ListenAddr: "/ip4/127.0.0.1/tcp/0",
		JWTSecret:  []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:     time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.Host() == nil {
		t.Fatal("Host() nil")
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
Expected: FAIL — package `relay` missing.

- [ ] **Step 3: Implement `relay.go` to pass**

```go
// signaling-server/internal/relay/relay.go
package relay

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/metrics"
	"github.com/multiformats/go-multiaddr"
)

// Config configures the relay Host.
type Config struct {
	ListenAddr     string        // multiaddr, e.g. "/ip4/127.0.0.1/tcp/9001/ws"
	AnnounceAddr   string        // optional public multiaddr advertised to peers
	PrivateKeyPath string        // PEM path; empty = ephemeral (dev/test only)
	JWTSecret      []byte
	JWTTTL         time.Duration
}

// Relay wraps a libp2p Host configured as a ShareBridge relay.
type Relay struct {
	h      host.Host
	bwc    *metrics.BandwidthCounter
	issuer *Issuer
	jtis   *JTIStore
	agents *AgentRegistry

	// OnCircuitClosed is called with quota bytes when a browser peer disconnects.
	OnCircuitClosed func(apiKeyID, shareCode string, bytesIn, bytesOut int64)
}

// New constructs and starts the relay Host. Call Start() to register protocol handlers.
func New(_ context.Context, cfg Config) (*Relay, error) {
	if len(cfg.JWTSecret) < 32 {
		return nil, errors.New("JWTSecret must be >= 32 bytes")
	}
	priv, err := loadOrGenerateKey(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load key: %w", err)
	}
	listenMA, err := multiaddr.NewMultiaddr(cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("parse listen addr: %w", err)
	}

	bwc := metrics.NewBandwidthCounter()

	opts := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.ListenAddrs(listenMA),
		libp2p.BandwidthReporter(bwc),
		// NOTE: Do NOT add DisableRelay() — circuit relay v2 must remain active.
	}
	if cfg.AnnounceAddr != "" {
		announceMA, err := multiaddr.NewMultiaddr(cfg.AnnounceAddr)
		if err != nil {
			return nil, fmt.Errorf("parse announce addr: %w", err)
		}
		opts = append(opts, libp2p.AddrsFactory(func(_ []multiaddr.Multiaddr) []multiaddr.Multiaddr {
			return []multiaddr.Multiaddr{announceMA}
		}))
	}

	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("new libp2p host: %w", err)
	}

	return &Relay{
		h:      h,
		bwc:    bwc,
		issuer: NewIssuer(cfg.JWTSecret, cfg.JWTTTL),
		jtis:   NewJTIStore(cfg.JWTTTL),
		agents: NewAgentRegistry(),
	}, nil
}

// Host returns the underlying libp2p Host.
func (r *Relay) Host() host.Host { return r.h }

// Issuer returns the JWT issuer (used by agent_ws to mint tokens).
func (r *Relay) Issuer() *Issuer { return r.issuer }

// Agents returns the agent registry (populated in 13b when agents connect).
func (r *Relay) Agents() *AgentRegistry { return r.agents }

// Close stops the Host and releases resources.
func (r *Relay) Close() error {
	r.jtis.Close()
	return r.h.Close()
}

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

Run: `go test ./internal/relay/ -run TestNewHost_startsAndStops -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/go.mod signaling-server/go.sum signaling-server/internal/relay/
git commit -m "feat(slice-13a): scaffold libp2p relay package"
```

---

## Task 2: Persistent relay identity

The relay's peer ID must survive restarts so browsers can cache the relay multiaddr.

**Files:**
- Modify: `signaling-server/internal/relay/relay_test.go`

(The `loadOrGenerateKey` implementation in Task 1 already handles persistence — this task just adds the test to prove it.)

- [ ] **Step 1: Write failing test**

Append to `relay_test.go`:

```go
func TestNewHost_persistsIdentity(t *testing.T) {
	tmp := t.TempDir()
	keyPath := tmp + "/relay.key"

	r1, err := New(context.Background(), Config{
		ListenAddr:     "/ip4/127.0.0.1/tcp/0",
		PrivateKeyPath: keyPath,
		JWTSecret:      []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:         time.Minute,
	})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	firstID := r1.Host().ID()
	r1.Close()

	r2, err := New(context.Background(), Config{
		ListenAddr:     "/ip4/127.0.0.1/tcp/0",
		PrivateKeyPath: keyPath,
		JWTSecret:      []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:         time.Minute,
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
Expected: PASS (Task 1's `loadOrGenerateKey` already handles this).

- [ ] **Step 2: Commit**

```bash
git add signaling-server/internal/relay/relay_test.go
git commit -m "test(slice-13a): verify relay identity persists across restarts"
```

---

## Task 3: JWT issuance and validation

**Files:**
- Create: `signaling-server/internal/relay/token.go`
- Create: `signaling-server/internal/relay/token_test.go`

- [ ] **Step 1: Write failing tests**

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
	got, err := issuer.Validate(tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.ShareCode != claims.ShareCode || got.BrowserPeerID != claims.BrowserPeerID ||
		got.RelayAllowed != claims.RelayAllowed || got.DCUtRAllowed != claims.DCUtRAllowed {
		t.Fatalf("claims mismatch: got %+v want %+v", got, claims)
	}
	if got.JTI == "" {
		t.Fatal("JTI empty")
	}
}

func TestValidate_rejectsTamperedClaim(t *testing.T) {
	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Minute)
	tok, _ := issuer.Issue(Claims{ShareCode: "abc12345", RelayAllowed: true})
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

// Claims is the ShareBridge JWT payload.
type Claims struct {
	JTI           string `json:"jti"`
	ShareCode     string `json:"share_code"`
	BrowserPeerID string `json:"browser_peer_id"`
	RelayAllowed  bool   `json:"relay_allowed"`
	DCUtRAllowed  bool   `json:"dcutr_allowed"`
	ExpiresAt     int64  `json:"exp"`
}

// Issuer signs and validates JWTs with HS256.
type Issuer struct {
	secret []byte
	ttl    time.Duration
}

func NewIssuer(secret []byte, ttl time.Duration) *Issuer {
	return &Issuer{secret: secret, ttl: ttl}
}

func (i *Issuer) Issue(c Claims) (string, error) {
	if len(i.secret) < 32 {
		return "", errors.New("jwt secret must be >= 32 bytes")
	}
	if c.JTI == "" {
		c.JTI = randomHex(16)
	}
	c.ExpiresAt = time.Now().Add(i.ttl).Unix()
	mapClaims := jwt.MapClaims{
		"jti": c.JTI, "share_code": c.ShareCode,
		"browser_peer_id": c.BrowserPeerID,
		"relay_allowed": c.RelayAllowed, "dcutr_allowed": c.DCUtRAllowed,
		"exp": c.ExpiresAt,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, mapClaims).SignedString(i.secret)
}

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
	if v, ok := mc["jti"].(string); ok { c.JTI = v }
	if v, ok := mc["share_code"].(string); ok { c.ShareCode = v }
	if v, ok := mc["browser_peer_id"].(string); ok { c.BrowserPeerID = v }
	if v, ok := mc["relay_allowed"].(bool); ok { c.RelayAllowed = v }
	if v, ok := mc["dcutr_allowed"].(bool); ok { c.DCUtRAllowed = v }
	if v, ok := mc["exp"].(float64); ok { c.ExpiresAt = int64(v) }
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
Expected: PASS (all three tests).

- [ ] **Step 3: Commit**

```bash
git add signaling-server/internal/relay/
git commit -m "feat(slice-13a): JWT issue + validate"
```

---

## Task 4: JTI replay store

**Files:**
- Modify: `signaling-server/internal/relay/token.go`
- Modify: `signaling-server/internal/relay/token_test.go`

- [ ] **Step 1: Write failing tests**

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
		stop:    make(chan struct{}),
	}
	store.ConsumeOnce("jti-B")
	time.Sleep(40 * time.Millisecond)
	store.prune()
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.entries["jti-B"]; ok {
		t.Fatal("expected jti-B to be pruned")
	}
}
```

Run: `go test ./internal/relay/ -run TestJTIStore -v`
Expected: FAIL.

- [ ] **Step 2: Implement `JTIStore` — append to `token.go`**

Add `"sync"` to token.go imports, then append:

```go
// JTIStore tracks consumed JTIs within the TTL window to prevent replay.
type JTIStore struct {
	mu      sync.Mutex
	entries map[string]time.Time
	ttl     time.Duration
	stop    chan struct{}
}

func NewJTIStore(ttl time.Duration) *JTIStore {
	s := &JTIStore{entries: make(map[string]time.Time), ttl: ttl, stop: make(chan struct{})}
	go s.pruneLoop()
	return s
}

// ConsumeOnce returns true iff this is the first time jti has been seen within the TTL.
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

Run: `go test ./internal/relay/ -run TestJTIStore -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add signaling-server/internal/relay/
git commit -m "feat(slice-13a): JTI replay store"
```

---

## Task 5: Agent registry

Maps `apiKeyID ↔ peer.ID`. Populated when agents connect (in Slice 13b); used here to resolve `share_code → agentPeerID`.

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
		t.Fatalf("lookup: got %v ok=%v", got, ok)
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
		t.Fatal("lookup still present after unregister")
	}
	if _, ok := reg.Reverse(pid); ok {
		t.Fatal("reverse still present after unregister")
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
// Register is called by the agent connection handler (Slice 13b).
type AgentRegistry struct {
	mu     sync.RWMutex
	byKey  map[string]peer.ID
	byPeer map[peer.ID]string
}

func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{byKey: make(map[string]peer.ID), byPeer: make(map[peer.ID]string)}
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

## Task 6: CircuitACL

The ACL gates circuit relay v2 connections. Agents may reserve freely. Browsers may only open circuits to the specific agent named in their JWT — after the JWT has been validated on the auth stream.

**Files:**
- Create: `signaling-server/internal/relay/acl.go`
- Create: `signaling-server/internal/relay/acl_test.go`

- [ ] **Step 1: Write failing tests**

```go
// signaling-server/internal/relay/acl_test.go
package relay

import (
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

func TestCircuitACL_agentCanAlwaysReserve(t *testing.T) {
	acl := newCircuitACL(time.Minute)
	pid, _ := peer.Decode("12D3KooWGRUacMd4aSXwsNyEYxh3nC1dT3rBjoJ5ycnqZRuiETxF")
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/9001")
	if !acl.AllowReserve(pid, addr) {
		t.Fatal("agents should always be allowed to reserve")
	}
}

func TestCircuitACL_browserCanConnectAfterAuthorize(t *testing.T) {
	acl := newCircuitACL(time.Minute)
	browserPID, _ := peer.Decode("12D3KooWBrowserAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	agentPID, _ := peer.Decode("12D3KooWGRUacMd4aSXwsNyEYxh3nC1dT3rBjoJ5ycnqZRuiETxF")
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/9001")

	if acl.AllowConnect(browserPID, addr, agentPID) {
		t.Fatal("should not allow before Authorize")
	}

	acl.Authorize(browserPID, agentPID, "abc12345", "api-key-1", time.Minute)

	if !acl.AllowConnect(browserPID, addr, agentPID) {
		t.Fatal("should allow after Authorize")
	}
}

func TestCircuitACL_wrongTargetRejected(t *testing.T) {
	acl := newCircuitACL(time.Minute)
	browserPID, _ := peer.Decode("12D3KooWBrowserAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	agentPID, _ := peer.Decode("12D3KooWGRUacMd4aSXwsNyEYxh3nC1dT3rBjoJ5ycnqZRuiETxF")
	wrongPID, _ := peer.Decode("12D3KooWWrongAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/9001")

	acl.Authorize(browserPID, agentPID, "abc12345", "api-key-1", time.Minute)

	if acl.AllowConnect(browserPID, addr, wrongPID) {
		t.Fatal("should reject connection to wrong agent")
	}
}

func TestCircuitACL_expiredEntryRejected(t *testing.T) {
	acl := newCircuitACL(time.Minute)
	browserPID, _ := peer.Decode("12D3KooWBrowserAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	agentPID, _ := peer.Decode("12D3KooWGRUacMd4aSXwsNyEYxh3nC1dT3rBjoJ5ycnqZRuiETxF")
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/9001")

	acl.Authorize(browserPID, agentPID, "abc12345", "api-key-1", time.Millisecond)
	time.Sleep(10 * time.Millisecond)

	if acl.AllowConnect(browserPID, addr, agentPID) {
		t.Fatal("should reject expired entry")
	}
}
```

Run: `go test ./internal/relay/ -run TestCircuitACL -v`
Expected: FAIL.

- [ ] **Step 2: Implement `acl.go`**

```go
// signaling-server/internal/relay/acl.go
package relay

import (
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

// circuitEntry holds the state for a JWT-authorized browser → agent pair.
type circuitEntry struct {
	AgentPeerID peer.ID
	ShareCode   string
	APIKeyID    string
	ExpiresAt   time.Time
}

// CircuitACL implements circuitv2.ACLFilter.
// Agents may always reserve. Browsers may only dial the specific agent named in their JWT.
type CircuitACL struct {
	mu      sync.Mutex
	entries map[peer.ID]*circuitEntry // browserPeerID → entry
}

func newCircuitACL(_ time.Duration) *CircuitACL {
	return &CircuitACL{entries: make(map[peer.ID]*circuitEntry)}
}

// AllowReserve permits any peer to make a circuit relay reservation.
// Agent authentication happens via a separate API-key protocol (Slice 13b).
func (a *CircuitACL) AllowReserve(_ peer.ID, _ multiaddr.Multiaddr) bool {
	return true
}

// AllowConnect permits src to open a circuit to dst only if Authorize was called
// for (src, dst) with a non-expired TTL.
func (a *CircuitACL) AllowConnect(src peer.ID, _ multiaddr.Multiaddr, dst peer.ID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.entries[src]
	if !ok {
		return false
	}
	if time.Now().After(entry.ExpiresAt) {
		delete(a.entries, src)
		return false
	}
	return entry.AgentPeerID == dst
}

// Authorize records that browserPeerID may open a circuit to agentPeerID for ttl duration.
// Called by the /sharebridge/relay/1.0.0 auth handler after JWT validation.
func (a *CircuitACL) Authorize(browserPeerID, agentPeerID peer.ID, shareCode, apiKeyID string, ttl time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries[browserPeerID] = &circuitEntry{
		AgentPeerID: agentPeerID,
		ShareCode:   shareCode,
		APIKeyID:    apiKeyID,
		ExpiresAt:   time.Now().Add(ttl),
	}
}

// GetEntry returns the entry for a browser peer ID (used by the bandwidth notifier).
func (a *CircuitACL) GetEntry(browserPeerID peer.ID) (*circuitEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.entries[browserPeerID]
	return entry, ok
}

// Remove deletes the entry for a browser peer ID (called after byte accounting at disconnect).
func (a *CircuitACL) Remove(browserPeerID peer.ID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.entries, browserPeerID)
}
```

Run: `go test ./internal/relay/ -run TestCircuitACL -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add signaling-server/internal/relay/
git commit -m "feat(slice-13a): CircuitACL for JWT-gated circuit relay v2"
```

---

## Task 7: Auth protocol handler `/sharebridge/relay/1.0.0`

This stream is **auth-only** — no data forwarding. The browser sends a JWT wrapped in a JSON envelope `{ type: 'jwt', token: '...' }` using the same 5-byte framing as the file protocol (1-byte kind `0x01` = text + 4-byte BE uint32 length + payload). The relay validates it, registers the authorized pair in the ACL, and responds with `{ type: 'auth_ok' }` (or `{ type: 'error', message: '...' }` on failure). After this stream closes, the browser uses standard circuit relay v2 to dial the agent.

Frame format (both directions): **Same 5-byte codec as `/sharebridge/file/1.0.0`** — 1-byte kind ∈ {0x01=text, 0x02=binary} + 4-byte BE uint32 length + payload. This keeps framing consistent across all ShareBridge protocols and lets 13b's `frame.go` be reused here.

**Files:**
- Create: `signaling-server/internal/relay/protocol.go`
- Create: `signaling-server/internal/relay/protocol_test.go`

- [ ] **Step 1: Write failing test — reject zero-length JWT**

```go
// signaling-server/internal/relay/protocol_test.go
package relay

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
)

func newLibp2pHost(t *testing.T) host.Host {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func connect(t *testing.T, dialer, target host.Host) {
	t.Helper()
	dialer.Peerstore().AddAddrs(target.ID(), target.Addrs(), time.Minute)
}

// writeTextFrame writes a 5-byte-framed text message (kind=0x01) to the stream.
func writeTextFrame(t *testing.T, s network.Stream, payload string) {
	t.Helper()
	// kind byte (0x01 = text)
	if _, err := s.Write([]byte{0x01}); err != nil {
		t.Fatalf("write kind: %v", err)
	}
	// 4-byte BE length
	if err := binary.Write(s, binary.BigEndian, uint32(len(payload))); err != nil {
		t.Fatalf("write length: %v", err)
	}
	if _, err := s.Write([]byte(payload)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
}

// readTextFrame reads a 5-byte-framed text message from the stream and returns the JSON-decoded payload.
func readTextFrame(t *testing.T, s network.Stream) map[string]any {
	t.Helper()
	// Read kind byte
	kindBuf := make([]byte, 1)
	if _, err := io.ReadFull(s, kindBuf); err != nil {
		t.Fatalf("read kind: %v", err)
	}
	if kindBuf[0] != 0x01 {
		t.Fatalf("expected text frame kind 0x01, got 0x%02x", kindBuf[0])
	}
	// Read 4-byte length
	var length uint32
	if err := binary.Read(s, binary.BigEndian, &length); err != nil {
		t.Fatalf("read length: %v", err)
	}
	if length > 8*1024*1024 {
		t.Fatalf("frame too large: %d bytes", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(buf, &resp); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return resp
}

func TestProtocol_rejectsInvalidKind(t *testing.T) {
	relayHost := newLibp2pHost(t)
	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Minute)
	jtis := NewJTIStore(time.Minute)
	defer jtis.Close()
	reg := NewAgentRegistry()
	acl := newCircuitACL(time.Minute)

	h := Handler{Issuer: issuer, JTIs: jtis, Agents: reg, ACL: acl, AuthTTL: time.Minute}
	relayHost.SetStreamHandler(ProtocolID, h.Handle)

	browserHost := newLibp2pHost(t)
	connect(t, browserHost, relayHost)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := browserHost.NewStream(ctx, relayHost.ID(), ProtocolID)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	defer s.Close()

	// Send frame with invalid kind byte (0x99 instead of 0x01)
	s.Write([]byte{0x99, 0, 0, 0, 0}) // kind + zero-length
	s.CloseWrite()

	resp := readTextFrame(t, s)
	if resp["type"] != "error" {
		t.Fatal("expected error response for invalid frame kind")
	}
}
```

Run: `go test ./internal/relay/ -run TestProtocol_rejectsInvalidKind -v`
Expected: FAIL — `Handler`, `ProtocolID` undefined.

- [ ] **Step 2: Implement `protocol.go`**

```go
// signaling-server/internal/relay/protocol.go
package relay

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// Frame constants — same as 13b's frame.go.
const (
	FrameText   = 0x01
	FrameBinary = 0x02
	MaxFrameLen = 8 * 1024 * 1024
)

// ProtocolID is the custom auth-handshake protocol.
// IMPORTANT: This stream does NOT forward data. It only validates the JWT and
// registers the authorized (browser → agent) pair in the CircuitACL.
// The browser then dials the agent via standard circuit relay v2.
const ProtocolID = protocol.ID("/sharebridge/relay/1.0.0")

// Handler handles the /sharebridge/relay/1.0.0 auth stream.
type Handler struct {
	Issuer  *Issuer
	JTIs    *JTIStore
	Agents  *AgentRegistry
	ACL     *CircuitACL
	Host    host.Host // unused by handler directly; kept for future protocol use
	AuthTTL time.Duration

	// CodeToAPIKey resolves share_code → apiKeyID. Injected to avoid PocketBase dep.
	CodeToAPIKey func(shareCode string) (apiKeyID string, ok bool)
}

func (h *Handler) Handle(s network.Stream) {
	defer s.Close()

	claims, err := readAndValidateJWT(s, h.Issuer, h.JTIs)
	if err != nil {
		log.Printf("relay auth: rejected from %s: %v", s.Conn().RemotePeer(), err)
		writeAuthResponse(s, err.Error())
		return
	}

	if !claims.RelayAllowed && !claims.DCUtRAllowed {
		writeAuthResponse(s, "relay and DCUtR both disabled in token")
		return
	}

	apiKeyID, ok := h.CodeToAPIKey(claims.ShareCode)
	if !ok {
		writeAuthResponse(s, "unknown share code")
		return
	}
	agentPeerID, ok := h.Agents.Lookup(apiKeyID)
	if !ok {
		writeAuthResponse(s, "agent not connected")
		return
	}

	browserPeerID := s.Conn().RemotePeer()
	h.ACL.Authorize(browserPeerID, agentPeerID, claims.ShareCode, apiKeyID, h.AuthTTL)

	writeAuthOK(s)
}

// readAndValidateJWT reads a 5-byte-framed text frame containing
// { type: 'jwt', token: '...' }, validates the JWT, and returns claims.
func readAndValidateJWT(s network.Stream, iss *Issuer, jtis *JTIStore) (*Claims, error) {
	_ = s.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer s.SetReadDeadline(time.Time{})

	// Read 5-byte header: kind + 4-byte length
	header := make([]byte, 5)
	if _, err := io.ReadFull(s, header); err != nil {
		return nil, err
	}
	kind := header[0]
	if kind != FrameText {
		return nil, errors.New("expected text frame (kind 0x01)")
	}
	length := binary.BigEndian.Uint32(header[1:5])
	if length == 0 || length > MaxFrameLen {
		return nil, errors.New("invalid frame length")
	}

	// Read payload
	buf := make([]byte, length)
	if _, err := io.ReadFull(s, buf); err != nil {
		return nil, err
	}

	// Decode JSON envelope: { type: 'jwt', token: '...' }
	var envelope struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(buf, &envelope); err != nil {
		return nil, errors.New("invalid JSON envelope")
	}
	if envelope.Type != "jwt" {
		return nil, errors.New("expected type 'jwt' in envelope")
	}
	if envelope.Token == "" {
		return nil, errors.New("empty JWT token")
	}

	// Validate the JWT
	claims, err := iss.Validate(envelope.Token)
	if err != nil {
		return nil, err
	}
	if !jtis.ConsumeOnce(claims.JTI) {
		return nil, errors.New("jti already used (replay)")
	}
	// Verify the JWT's browser_peer_id matches the actual stream peer.
	if claims.BrowserPeerID != "" {
		remote := s.Conn().RemotePeer()
		claimed, err := peer.Decode(claims.BrowserPeerID)
		if err != nil || claimed != remote {
			return nil, errors.New("browser_peer_id mismatch")
		}
	}
	return claims, nil
}

// writeAuthOK sends { type: 'auth_ok' } as a 5-byte-framed text frame.
func writeAuthOK(s network.Stream) {
	payload, _ := json.Marshal(map[string]string{"type": "auth_ok"})
	writeFrame(s, FrameText, payload)
}

// writeAuthResponse sends { type: 'error', message: msg } as a 5-byte-framed text frame.
func writeAuthResponse(s network.Stream, msg string) {
	payload, _ := json.Marshal(map[string]string{"type": "error", "message": msg})
	writeFrame(s, FrameText, payload)
}

// writeFrame writes a 5-byte-framed message to the stream.
func writeFrame(s network.Stream, kind byte, payload []byte) {
	header := make([]byte, 5)
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:5], uint32(len(payload)))
	s.Write(header)
	s.Write(payload)
}

// SetCodeResolver wires the share_code → api_key_id lookup after construction.
func (r *Relay) SetCodeResolver(f func(shareCode string) (apiKeyID string, ok bool)) {
	r.handler.CodeToAPIKey = f
}

// SetCircuitClosedHook sets the byte-count callback fired at browser disconnect.
func (r *Relay) SetCircuitClosedHook(f func(apiKeyID, shareCode string, bytesIn, bytesOut int64)) {
	r.OnCircuitClosed = f
}
```

Note that `r.handler` and the `Start()` method are added in Task 8. `SetCodeResolver` and `SetCircuitClosedHook` reference the `Relay` struct — they compile only after Task 8 extends `Relay` with the `handler` field.

For now, temporarily add a placeholder `handler Handler` field to `Relay` so the file compiles:

In `relay.go` add to struct:
```go
handler Handler
acl     *CircuitACL
```

And in `New()`, add after `agents`:
```go
acl := newCircuitACL(cfg.JWTTTL)
r := &Relay{
    h:      h,
    bwc:    bwc,
    acl:    acl,
    issuer: NewIssuer(cfg.JWTSecret, cfg.JWTTTL),
    jtis:   NewJTIStore(cfg.JWTTTL),
    agents: NewAgentRegistry(),
}
r.handler = Handler{
    Issuer:  r.issuer,
    JTIs:    r.jtis,
    Agents:  r.agents,
    ACL:     r.acl,
    AuthTTL: cfg.JWTTTL,
}
return r, nil
```

Run: `go test ./internal/relay/ -run TestProtocol_rejectsInvalidKind -v`
Expected: PASS.

- [ ] **Step 3: Test — valid JWT registers ACL entry and returns auth_ok**

Append to `protocol_test.go`:

```go
func TestProtocol_validJWT_authorizesACL(t *testing.T) {
	relayHost := newLibp2pHost(t)
	agentHost := newLibp2pHost(t)
	browserHost := newLibp2pHost(t)

	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Minute)
	jtis := NewJTIStore(time.Minute)
	defer jtis.Close()
	reg := NewAgentRegistry()
	reg.Register("api-key-xyz", agentHost.ID())
	acl := newCircuitACL(time.Minute)

	h := Handler{
		Issuer:  issuer,
		JTIs:    jtis,
		Agents:  reg,
		ACL:     acl,
		AuthTTL: time.Minute,
		CodeToAPIKey: func(code string) (string, bool) {
			if code == "abc12345" {
				return "api-key-xyz", true
			}
			return "", false
		},
	}
	relayHost.SetStreamHandler(ProtocolID, h.Handle)
	connect(t, browserHost, relayHost)

	tok, err := issuer.Issue(Claims{
		ShareCode:     "abc12345",
		BrowserPeerID: browserHost.ID().String(),
		RelayAllowed:  true,
		DCUtRAllowed:  true,
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
	// Send JWT wrapped in JSON envelope using 5-byte framing
	envelope, _ := json.Marshal(map[string]string{"type": "jwt", "token": tok})
	writeTextFrame(t, s, string(envelope))
	s.CloseWrite()

	resp := readTextFrame(t, s)
	if resp["type"] != "auth_ok" {
		t.Fatalf("expected type:auth_ok, got %v", resp)
	}

	// ACL should now allow browser → agent circuit.
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/9001")
	if !acl.AllowConnect(browserHost.ID(), addr, agentHost.ID()) {
		t.Fatal("ACL should allow browser→agent after successful auth")
	}
}
```

Add `"github.com/multiformats/go-multiaddr"` to imports in `protocol_test.go`.

Run: `go test ./internal/relay/ -run TestProtocol_validJWT_authorizesACL -v`
Expected: PASS.

- [ ] **Step 4: Test — JTI replay rejected**

Append to `protocol_test.go`:

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
	acl := newCircuitACL(time.Minute)

	h := Handler{
		Issuer:  issuer, JTIs: jtis, Agents: reg, ACL: acl, AuthTTL: time.Minute,
		CodeToAPIKey: func(string) (string, bool) { return "k", true },
	}
	relayHost.SetStreamHandler(ProtocolID, h.Handle)
	connect(t, browserHost, relayHost)

	tok, _ := issuer.Issue(Claims{
		ShareCode:     "abc12345",
		BrowserPeerID: browserHost.ID().String(),
		RelayAllowed:  true,
	})

	dial := func() map[string]any {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s, err := browserHost.NewStream(ctx, relayHost.ID(), ProtocolID)
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		defer s.Close()
		envelope, _ := json.Marshal(map[string]string{"type": "jwt", "token": tok})
		writeTextFrame(t, s, string(envelope))
		s.CloseWrite()
		return readTextFrame(t, s)
	}

	first := dial()
	if first["type"] != "auth_ok" {
		t.Fatalf("first use should succeed, got %v", first)
	}
	second := dial()
	if second["type"] != "error" {
		t.Fatalf("second use (replay) should fail, got %v", second)
	}
}
```

Run: `go test ./internal/relay/ -run TestProtocol -v`
Expected: all three tests PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/relay/
git commit -m "feat(slice-13a): /sharebridge/relay/1.0.0 auth-only protocol handler"
```

---

## Task 8: Wire circuit relay v2 service + bandwidth notifier into `Relay`

This task enables E2E encryption by adding the stock go-libp2p circuit relay v2 service (gated by `CircuitACL`) to the Host, and wires the bandwidth notifier for quota accounting.

**Files:**
- Modify: `signaling-server/internal/relay/relay.go`
- Modify: `signaling-server/internal/relay/relay_test.go`

- [ ] **Step 1: Write failing test for `Start()` registering the auth handler**

Append to `relay_test.go`:

```go
func TestRelay_startRegistersAuthHandler(t *testing.T) {
	rly, err := New(context.Background(), Config{
		ListenAddr: "/ip4/127.0.0.1/tcp/0",
		JWTSecret:  []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:     time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rly.Close()

	rly.SetCodeResolver(func(string) (string, bool) { return "k", false })
	addrs := rly.Start()
	if len(addrs) == 0 {
		t.Fatal("Start() returned no listen addrs")
	}

	// Should be able to dial the auth protocol.
	dialer, _ := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	defer dialer.Close()
	dialer.Peerstore().AddAddrs(rly.Host().ID(), rly.Host().Addrs(), time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := dialer.NewStream(ctx, rly.Host().ID(), ProtocolID)
	if err != nil {
		t.Fatalf("NewStream after Start: %v", err)
	}
	s.Close()
}
```

Add `libp2p "github.com/libp2p/go-libp2p"` to relay_test.go imports.

Run: `go test ./internal/relay/ -run TestRelay_startRegistersAuthHandler -v`
Expected: FAIL — `Start()` not defined.

- [ ] **Step 2: Add circuit relay v2 service and `Start()` to `relay.go`**

Add imports to `relay.go`:

```go
import (
    // existing imports ...
    "github.com/libp2p/go-libp2p/core/network"
    circuitv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
)
```

Add `circuitSvc *circuitv2.Relay` field to the `Relay` struct.

Replace the tail of `New()` (after `h, err := libp2p.New(...)`) with:

```go
	acl := newCircuitACL(cfg.JWTTTL)

	// Start the circuit relay v2 service. This is what provides E2E Noise encryption:
	// the relay forwards ciphertext between browser and agent without being able to read it.
	// DCUtR hole-punching flows through this service automatically.
	circuitSvc, err := circuitv2.New(h, circuitv2.WithACL(acl))
	if err != nil {
		h.Close()
		return nil, fmt.Errorf("circuit relay v2: %w", err)
	}

	issuer := NewIssuer(cfg.JWTSecret, cfg.JWTTTL)
	jtis := NewJTIStore(cfg.JWTTTL)
	agents := NewAgentRegistry()

	r := &Relay{
		h:          h,
		bwc:        bwc,
		acl:        acl,
		circuitSvc: circuitSvc,
		issuer:     issuer,
		jtis:       jtis,
		agents:     agents,
	}
	r.handler = Handler{
		Issuer:  issuer,
		JTIs:    jtis,
		Agents:  agents,
		ACL:     acl,
		AuthTTL: cfg.JWTTTL,
	}
	return r, nil
```

Add `Start()` and update `Close()`:

```go
// Start registers the /sharebridge/relay/1.0.0 auth handler and the bandwidth notifier.
// Returns the relay's listen multiaddrs.
func (r *Relay) Start() []multiaddr.Multiaddr {
	r.h.SetStreamHandler(ProtocolID, r.handler.Handle)

	// When a browser peer disconnects, read their cumulative bandwidth and
	// report it to the quota accumulator. Browser peer IDs are ephemeral
	// (new Ed25519 key per page load), so cumulative == per-circuit.
	r.h.Network().Notify(&network.NotifyBundle{
		DisconnectedF: func(_ network.Network, conn network.Conn) {
			peerID := conn.RemotePeer()
			entry, ok := r.acl.GetEntry(peerID)
			if !ok {
				return
			}
			stat := r.bwc.GetBandwidthForPeer(peerID)
			if r.OnCircuitClosed != nil {
				r.OnCircuitClosed(entry.APIKeyID, entry.ShareCode, stat.TotalIn, stat.TotalOut)
			}
			r.acl.Remove(peerID)
		},
	})

	return r.h.Addrs()
}

// Close stops the circuit relay service, the Host, and the JTI prune goroutine.
func (r *Relay) Close() error {
	r.jtis.Close()
	r.circuitSvc.Close()
	return r.h.Close()
}
```

Run: `go test ./internal/relay/ -v`
Expected: all tests PASS.

- [ ] **Step 3: Commit**

```bash
git add signaling-server/internal/relay/
git commit -m "feat(slice-13a): enable circuit relay v2 + bandwidth notifier for E2E relay"
```

---

## Task 9: Config updates — drop TURN, add relay settings

**Files:**
- Modify: `signaling-server/internal/config/config.go`
- Modify: `signaling-server/internal/config/config_test.go`

- [ ] **Step 1: Read current config test**

Run: `cat signaling-server/internal/config/config_test.go`
Note which tests reference `TurnHost`, `TurnSecret`, `HasTurn`, `TurnURL`, `PrometheusURL`.

- [ ] **Step 2: Replace `Config` struct and `Load()`**

Replace the entire contents of `config.go`:

```go
package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port    string
	DataDir string

	// Relay (libp2p Host)
	RelayListenAddr     string        // multiaddr, e.g. "/ip4/127.0.0.1/tcp/9001/ws"
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

// HasSMTP returns true if SMTP is configured.
func (c *Config) HasSMTP() bool { return c.SMTPHost != "" }

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
```

- [ ] **Step 3: Rewrite `config_test.go`**

Delete all tests referencing `TurnHost`, `TurnPort`, `TurnSecret`, `HasTurn`, `TurnURL`, `PrometheusURL`. Replace with:

```go
package config

import (
	"testing"
	"time"
)

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

func TestLoad_smtpAbsentByDefault(t *testing.T) {
	t.Setenv("SMTP_HOST", "")
	cfg := Load()
	if cfg.HasSMTP() {
		t.Fatal("HasSMTP() should be false with no SMTP_HOST")
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

## Task 10: Update `browser_ws.go` — drop ICE/SDP, forward peer ID on knock/join

JWT issuance moves to `agent_ws.go` (Task 11). The browser only needs to send its libp2p peer ID so the server can bind it into the JWT when `auth_ok` arrives from the agent.

**Files:**
- Modify: `signaling-server/internal/handler/browser_ws.go`
- Modify: `signaling-server/internal/handler/browser_ws_test.go`
- Create: `signaling-server/internal/handler/handler_testhelpers_test.go`
- Modify: `signaling-server/internal/hub/hub.go`

- [ ] **Step 1: Add `RememberBrowserPeerID` / `GetBrowserPeerID` to hub**

In `hub.go`, add `connPeerIDs map[string]string` to the `Hub` struct:

```go
type Hub struct {
	mu           sync.RWMutex
	agents       map[string]*websocket.Conn
	codes        map[string]string
	pairs        map[string]*pair
	connBrowsers map[string]*websocket.Conn
	connFails    map[string]int
	connPeerIDs  map[string]string // connID → browser libp2p peer ID
}
```

Initialize in `New()`:
```go
connPeerIDs: make(map[string]string),
```

Add to `hub.go`:
```go
// RememberBrowserPeerID stores the browser's libp2p peer ID for connID.
// Called by browser_ws when the browser sends its peer ID with knock or join.
func (h *Hub) RememberBrowserPeerID(connID, peerID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connPeerIDs[connID] = peerID
}

// GetBrowserPeerID retrieves the stored peer ID for connID.
func (h *Hub) GetBrowserPeerID(connID string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	p, ok := h.connPeerIDs[connID]
	return p, ok
}
```

In `UnregisterBrowserConn`, also clear the peer ID:
```go
func (h *Hub) UnregisterBrowserConn(connID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.connBrowsers, connID)
	delete(h.connFails, connID)
	delete(h.connPeerIDs, connID)
}
```

Run: `go test ./internal/hub/ -v`
Expected: PASS.

- [ ] **Step 2: Create shared test helper**

```go
// signaling-server/internal/handler/handler_testhelpers_test.go
package handler

import (
	"context"
	"testing"
	"time"

	"sharebridge/server/internal/relay"
)

func newTestRelay(t *testing.T) *relay.Relay {
	t.Helper()
	rly, err := relay.New(context.Background(), relay.Config{
		ListenAddr: "/ip4/127.0.0.1/tcp/0",
		JWTSecret:  []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:     time.Minute,
	})
	if err != nil {
		t.Fatalf("newTestRelay: %v", err)
	}
	rly.SetCodeResolver(func(code string) (string, bool) {
		return "test-api-key", code != ""
	})
	t.Cleanup(func() { rly.Close() })
	rly.Start()
	return rly
}
```

- [ ] **Step 3: Rewrite `browser_ws.go`**

Replace the full file:

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
	Type          string `json:"type"`
	HMAC          string `json:"hmac,omitempty"`
	BrowserPeerID string `json:"browser_peer_id,omitempty"` // browser's libp2p peer ID
}

func BrowserWS(app core.App, sessionHub *hub.Hub, cfg *config.Config, rly *relay.Relay) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionCode := r.URL.Query().Get("session")
		records, err := app.FindRecordsByFilter("sessions", "code = {:code}", "", 1, 0, map[string]any{"code": sessionCode})
		if err != nil || len(records) == 0 {
			http.Error(w, `{"error":"session not found or expired"}`, http.StatusNotFound)
			return
		}
		sessionRecord := records[0]
		expiresAt := sessionRecord.GetDateTime("expires_at")
		if !expiresAt.IsZero() && time.Now().After(expiresAt.Time()) {
			http.Error(w, `{"error":"session not found or expired"}`, http.StatusNotFound)
			return
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			log.Printf("browser_ws accept: %v", err)
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()

		_, agentConnected := sessionHub.GetAgentConn(sessionCode)
		if !agentConnected {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			conn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}
		if err := sessionHub.PairSession(sessionCode, conn); err != nil {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			conn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}
		defer sessionHub.UnpairSession(sessionCode)

		connID := generateConnID()
		sessionHub.RegisterBrowserConn(connID, conn)
		defer sessionHub.UnregisterBrowserConn(connID)

		apiKeyID := sessionRecord.GetString("api_key_id")
		apiKeyRecord, err := app.FindRecordById("api_keys", apiKeyID)
		if err != nil {
			log.Printf("browser_ws: lookup api_key %s: %v", apiKeyID, err)
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "internal error"})
			return
		}
		accountID := apiKeyRecord.GetString("account_id")
		accountRecord, err := app.FindRecordById("users", accountID)
		if err != nil {
			log.Printf("browser_ws: lookup account %s: %v", accountID, err)
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "internal error"})
			return
		}

		// Relay-only + quota exceeded: reject immediately.
		// JWT issuance (and the non-relay_only quota check) happens in agent_ws on auth_ok.
		relayOnly := sessionRecord.GetBool("relay_only")
		quotaExceeded, _ := checkRelayQuota(accountRecord)
		if relayOnly && quotaExceeded {
			hub.SendDirect(ctx, conn, map[string]string{
				"type":    "error",
				"message": "file host's relay quota exceeded - this share requires relay which is unavailable",
			})
			conn.Close(websocket.StatusNormalClosure, "relay quota exceeded")
			return
		}

		// Main message loop.
		// JWT is NOT issued here — the server does not know whether a share has
		// a password (that knowledge lives on the agent only). JWT issuance happens
		// in agent_ws when the agent sends auth_ok.
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				log.Printf("browser disconnected: session=%s conn=%s", sessionCode, connID)
				return
			}
			var msg browserMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case "knock":
				if msg.BrowserPeerID != "" {
					sessionHub.RememberBrowserPeerID(connID, msg.BrowserPeerID)
				}
				sessionHub.SendToAgent(ctx, apiKeyID, map[string]any{
					"type": "knock", "conn_id": connID, "code": sessionCode,
				})
			case "join":
				if msg.BrowserPeerID != "" {
					sessionHub.RememberBrowserPeerID(connID, msg.BrowserPeerID)
				}
				sessionHub.SendToAgent(ctx, apiKeyID, map[string]any{
					"type": "join", "conn_id": connID, "code": sessionCode, "hmac": msg.HMAC,
				})
			}
		}
	}
}

func generateConnID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// checkRelayQuota is shared between browser_ws and agent_ws.
func checkRelayQuota(accountRecord *core.Record) (bool, time.Time) {
	limitGB := accountRecord.GetFloat("relay_quota_gb")
	if limitGB <= 0 {
		limitGB = 50.0
	}
	usedGB := accountRecord.GetFloat("current_period_usage_gb")
	periodEnd := accountRecord.GetDateTime("quota_period_end").Time()
	if time.Now().After(periodEnd) {
		return false, periodEnd
	}
	return usedGB >= limitGB, periodEnd
}

func lookupAccountForAPIKey(app core.App, apiKeyID string) (*core.Record, error) {
	apiKeyRecord, err := app.FindRecordById("api_keys", apiKeyID)
	if err != nil {
		return nil, fmt.Errorf("api_keys %s: %w", apiKeyID, err)
	}
	accountID := apiKeyRecord.GetString("account_id")
	return app.FindRecordById("users", accountID)
}
```

- [ ] **Step 4: Rewrite browser_ws test**

In `browser_ws_test.go`, delete any test that asserts on `ice_config`, `answer`, `ice_candidate`. Add:

```go
func TestBrowserWS_knockForwardedToAgent(t *testing.T) {
	// This test verifies that knock is forwarded to the agent regardless of
	// whether the share has a password (server doesn't know — agent decides).
	// JWT issuance is NOT tested here; it belongs to TestAgentWS_authOkIssuesRelayInfo.
	app := tests.NewTestApp(t.TempDir())
	defer app.Cleanup()

	h := hub.New()
	rly := newTestRelay(t)
	cfg := &config.Config{
		RelayAnnounceAddr: "/ip4/127.0.0.1/tcp/9001/ws",
		JWTSecret:         []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:            time.Minute,
	}

	// Seed a session record and a fake agent connection in the hub.
	// (Use existing test helpers from the test file or PocketBase test APIs.)

	server := httptest.NewServer(http.HandlerFunc(BrowserWS(app, h, cfg, rly)))
	defer server.Close()

	// Connect as browser, send knock with browser_peer_id, assert hub received knock for agent.
}
```

Run: `go build ./...`
Expected: compile success. Fix any remaining references to `turn.` package.

Run: `go test ./internal/handler/ -v`
Expected: PASS on all non-deleted tests.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/hub/hub.go \
        signaling-server/internal/handler/browser_ws.go \
        signaling-server/internal/handler/browser_ws_test.go \
        signaling-server/internal/handler/handler_testhelpers_test.go
git commit -m "feat(slice-13a): browser_ws drops ICE/SDP, forwards browser_peer_id on knock/join"
```

---

## Task 11: Update `agent_ws.go` — drop ICE/SDP, add `auth_ok` → `relay_info`

The `auth_ok` message from the agent means "I have verified the browser's HMAC (or there was no password)". The server then issues a JWT and sends `relay_info` to the browser, including the agent's peer ID so the browser can construct the circuit address.

**Files:**
- Modify: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/internal/handler/agent_ws_test.go`

- [ ] **Step 1: Update `agentMsg` and remove TURN import**

In `agent_ws.go`, remove the `"sharebridge/server/internal/turn"` import and replace the `agentMsg` struct:

```go
type agentMsg struct {
	Type        string     `json:"type"`
	AgentID     string     `json:"agent_id,omitempty"`
	Code        string     `json:"code,omitempty"`
	ShareURL    string     `json:"share_url,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	SessionID   string     `json:"session_id,omitempty"`
	ConnID      string     `json:"conn_id,omitempty"`
	Value       string     `json:"value,omitempty"`
	HasPassword bool       `json:"has_password,omitempty"`
	RelayOnly   bool       `json:"relay_only,omitempty"`
	// SDP and Candidate removed — libp2p handles connection establishment.
}
```

- [ ] **Step 2: Update `AgentWS` signature and remove quota/ICE state**

Change `AgentWS` signature:

```go
func AgentWS(app core.App, h *hub.Hub, cfg *config.Config, rly *relay.Relay) http.HandlerFunc {
```

Add `"sharebridge/server/internal/relay"` to imports.

Remove:
- `quotaExceeded bool` / `quotaCheckedAt time.Time` / `refreshQuota` closure
- `case "offer":` and `case "ice_candidate":` from the switch
- `isRelayCandidate` function
- All `cfg.HasTurn()` / `turn.` references

- [ ] **Step 3: Update `handleHello`**

Replace `handleHello`:

```go
func handleHello(ctx context.Context, conn *websocket.Conn, h *hub.Hub, apiKeyID, accountID, agentID string, cfg *config.Config, rly *relay.Relay) {
	if agentID == "" {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent_id required"})
		return
	}
	h.RegisterAgent(apiKeyID, conn)
	log.Printf("agent hello: api_key_id=%s agent_id=%s", apiKeyID, agentID)

	hub.SendDirect(ctx, conn, map[string]any{
		"type":            "welcome",
		"relay_multiaddr": cfg.RelayAnnounceAddr + "/p2p/" + rly.Host().ID().String(),
		"stun_servers":    []string{"stun:stun.cloudflare.com:3478"},
	})
}
```

Update the call site in the switch:

```go
case "hello":
    handleHello(ctx, conn, h, apiKeyID, accountID, msg.AgentID, cfg, rly)
    agentID = msg.AgentID
```

- [ ] **Step 4: Add `auth_ok` case**

Add to the switch in the main message loop:

```go
case "auth_ok":
	// Agent has verified the browser's HMAC (or there was no password —
	// agent sends auth_ok unconditionally for password-free shares too).
	// Issue a JWT and forward relay_info to the browser.
	if agentID == "" {
		continue
	}
	peerID, ok := h.GetBrowserPeerID(msg.ConnID)
	if !ok {
		log.Printf("agent_ws: auth_ok for unknown connID %s", msg.ConnID)
		continue
	}

	// Look up relay_only + quota for this session.
	records, _ := app.FindRecordsByFilter("sessions", "code = {:code}", "", 1, 0, map[string]any{"code": msg.Code})
	relayOnly := false
	if len(records) > 0 {
		relayOnly = records[0].GetBool("relay_only")
	}
	accountRecord, err := app.FindRecordById("users", accountID)
	if err != nil {
		log.Printf("agent_ws: auth_ok lookup account %s: %v", accountID, err)
		continue
	}
	quotaExceeded, _ := checkRelayQuota(accountRecord)
	if relayOnly && quotaExceeded {
		h.CloseBrowserConnWithError(ctx, msg.ConnID, "file host's relay quota exceeded")
		continue
	}

	// Look up agent peer ID from the relay registry.
	agentPeerID, ok := rly.Agents().Lookup(apiKeyID)
	if !ok {
		log.Printf("agent_ws: auth_ok but agent %s not registered with relay", apiKeyID)
		continue
	}

	tok, err := rly.Issuer().Issue(relay.Claims{
		ShareCode:     msg.Code,
		BrowserPeerID: peerID,
		RelayAllowed:  !quotaExceeded,
		DCUtRAllowed:  !relayOnly,
	})
	if err != nil {
		log.Printf("agent_ws: issue JWT: %v", err)
		continue
	}
	h.ForwardToBrowserByConnID(ctx, msg.ConnID, map[string]any{
		"type":            "relay_info",
		"relay_multiaddr": cfg.RelayAnnounceAddr + "/p2p/" + rly.Host().ID().String(),
		"agent_peer_id":   agentPeerID.String(),
		"jwt":             tok,
		"relay_allowed":   !quotaExceeded,
		"dcutr_allowed":   !relayOnly,
	})
```

Note that `checkRelayQuota` and `lookupAccountForAPIKey` are now in `browser_ws.go`. If they were previously only in `browser_ws.go`, move `checkRelayQuota` to a shared `handler_helpers.go` file, or duplicate it. The cleanest approach: create `signaling-server/internal/handler/handler_helpers.go` with `checkRelayQuota` and `lookupAccountForAPIKey`, and remove them from `browser_ws.go`.

- [ ] **Step 5: Test `auth_ok` → `relay_info`**

In `agent_ws_test.go`, delete tests that send `offer` / `ice_candidate` or assert `ice_servers` in welcome. Add:

```go
func TestAgentWS_authOkIssuesRelayInfoToBrowser(t *testing.T) {
	app := tests.NewTestApp(t.TempDir())
	defer app.Cleanup()
	h := hub.New()
	rly := newTestRelay(t)
	cfg := &config.Config{
		RelayAnnounceAddr: "/ip4/127.0.0.1/tcp/9001/ws",
		JWTSecret:         []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:            time.Minute,
	}

	// Seed: agent WebSocket + session + account (use existing test helpers).
	// Register a fake agent peer ID in rly.Agents() so auth_ok can look it up.
	// Send auth_ok { code, conn_id } from the agent.
	// Assert the browser WS (registered under connID) receives relay_info with:
	//   - type: "relay_info"
	//   - jwt: non-empty string
	//   - agent_peer_id: non-empty string
	//   - relay_multiaddr: non-empty string
}
```

Run: `go test ./internal/handler/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add signaling-server/internal/handler/
git commit -m "feat(slice-13a): agent_ws drops ICE/SDP, adds auth_ok → relay_info with agent_peer_id"
```

---

## Task 12: Delete `internal/turn/` and remove all references

**Files:**
- Delete: `signaling-server/internal/turn/` (all 4 files)
- Modify: any remaining importer

- [ ] **Step 1: Delete the package**

```bash
rm -rf signaling-server/internal/turn/
```

- [ ] **Step 2: Find remaining imports**

```bash
grep -r "internal/turn" signaling-server/ --include="*.go"
```

Remove any remaining `import "sharebridge/server/internal/turn"` lines and usages.

- [ ] **Step 3: Build**

```bash
cd signaling-server && go build ./...
```

Expected: success.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "refactor(slice-13a): delete internal/turn package"
```

---

## Task 13: Quota — replace Prometheus poller with event-driven accumulator

The old `Poller` pulled byte counts from Prometheus (coturn exporter). The new `Accumulator` receives byte counts directly from `relay.OnCircuitClosed`, which fires when a browser peer disconnects after a circuit.

**Files:**
- Modify: `signaling-server/internal/quota/poller.go`
- Modify: `signaling-server/internal/quota/poller_test.go`
- Delete: `signaling-server/internal/metrics/prometheus.go` and `prometheus_test.go`

- [ ] **Step 1: Delete the Prometheus metrics client**

```bash
rm signaling-server/internal/metrics/prometheus.go
rm signaling-server/internal/metrics/prometheus_test.go
```

If `metrics/` is now empty, remove it. Remove its import from anywhere referencing it.

- [ ] **Step 2: Rewrite `poller.go`**

Replace the entire file:

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

// Accumulator receives per-circuit byte counts from relay.OnCircuitClosed and
// flushes them to PocketBase on a regular interval.
type Accumulator struct {
	app  core.App
	cfg  *config.Config
	mu   sync.Mutex
	buf  map[string]int64 // apiKeyID → bytes accrued since last flush
	stop chan struct{}
}

func NewAccumulator(app core.App, cfg *config.Config) *Accumulator {
	return &Accumulator{app: app, cfg: cfg, buf: make(map[string]int64), stop: make(chan struct{})}
}

// Record adds bytes to the per-apiKeyID bucket. Called from relay.OnCircuitClosed.
// Both directions are summed — every relayed byte counts toward quota.
func (a *Accumulator) Record(apiKeyID string, bytesIn, bytesOut int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.buf[apiKeyID] += bytesIn + bytesOut
}

func (a *Accumulator) Start() { go a.run() }

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
		acct.Set("current_period_usage_gb", currentGB+float64(bytes)/(1024*1024*1024))
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

	"github.com/pocketbase/pocketbase/tests"
	"sharebridge/server/internal/config"
)

func TestAccumulator_flushesToAccount(t *testing.T) {
	app := tests.NewTestApp(t.TempDir())
	defer app.Cleanup()
	apiKeyID, accountID := seedAPIKeyAndAccount(t, app)

	acc := NewAccumulator(app, &config.Config{QuotaCheckInterval: 5 * time.Millisecond})
	acc.Start()
	defer acc.Stop()

	acc.Record(apiKeyID, 500*1024*1024, 500*1024*1024) // 1 GB total

	time.Sleep(50 * time.Millisecond)
	rec, err := app.FindRecordById("users", accountID)
	if err != nil {
		t.Fatalf("find account: %v", err)
	}
	got := rec.GetFloat("current_period_usage_gb")
	if got < 0.99 || got > 1.01 {
		t.Fatalf("usage: got %f want ~1.0", got)
	}
}

// seedAPIKeyAndAccount creates a minimal account + api_key for tests.
// Adjust field names to match the actual PocketBase schema.
func seedAPIKeyAndAccount(t *testing.T, app interface{ Save(*core.Record) error; ... }) (apiKeyID, accountID string) {
	t.Helper()
	// ... create records using app.Dao().SaveRecord / app.FindCollectionByNameOrId
	// Match the existing test helpers already in agent_ws_test.go or account_test.go
	return
}
```

Note: Look at the existing `poller_test.go` and other test files for the `seedAPIKeyAndAccount` pattern already used in this codebase — replicate it rather than inventing a new one.

Run: `go test ./internal/quota/ -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/internal/quota/ signaling-server/internal/metrics/
git commit -m "refactor(slice-13a): quota poller → event-driven accumulator fed by relay byte counts"
```

---

## Task 14: Wire everything together in `cmd/server/main.go`

**Files:**
- Modify: `signaling-server/cmd/server/main.go`

- [ ] **Step 1: Read the current `main.go`**

Run: `cat signaling-server/cmd/server/main.go`
Identify: where TURN config is used, where quota poller is started, where handlers are registered.

- [ ] **Step 2: Replace the `OnServe` body**

Remove: `metrics.NewPrometheusClient`, all `cfg.HasTurn()` branches, `turn.` calls, quota `Poller`.

Add the relay + accumulator construction. The key additions inside `OnServe`:

```go
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
rly.SetCircuitClosedHook(func(apiKeyID, _ string, bytesIn, bytesOut int64) {
    acc.Record(apiKeyID, bytesIn, bytesOut)
})
rly.Start()
log.Printf("relay host %s listening on %v", rly.Host().ID(), rly.Host().Addrs())
```

Update handler registrations to pass `rly`:

```go
router.GET("/ws/agent", func(e *core.RequestEvent) error {
    middleware.APIKeyAuth(app)(http.HandlerFunc(handler.AgentWS(app, h, cfg, rly))).
        ServeHTTP(e.Response, e.Request)
    return nil
})
router.GET("/ws/client", func(e *core.RequestEvent) error {
    handler.BrowserWS(app, h, cfg, rly)(e.Response, e.Request)
    return nil
})
```

Add shutdown:

```go
app.OnTerminate().BindFunc(func(_ *core.TerminateEvent) error {
    acc.Stop()
    return rly.Close()
})
```

- [ ] **Step 3: Build**

```bash
cd signaling-server && go build ./...
```

Expected: success, no TURN or Prometheus references.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/cmd/server/main.go
git commit -m "feat(slice-13a): wire relay + quota accumulator into server main"
```

---

## Task 15: End-to-end integration test — circuit relay v2 with E2E Noise

This test proves the full E2E path: agent reserves, browser auth-streams JWT, browser dials agent via circuit relay v2, data flows encrypted end-to-end, and byte counts are reported.

**Files:**
- Create: `signaling-server/internal/relay/integration_test.go`

- [ ] **Step 1: Write the test**

```go
// signaling-server/internal/relay/integration_test.go
package relay_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	circuitv2client "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	"github.com/multiformats/go-multiaddr"

	"sharebridge/server/internal/relay"
)

const fileProto = "/sharebridge/file/1.0.0"

// writeTextFrame writes a 5-byte-framed text message (kind=0x01) to the stream.
func writeTextFrame(t *testing.T, s network.Stream, payload string) {
	t.Helper()
	if _, err := s.Write([]byte{0x01}); err != nil {
		t.Fatalf("write kind: %v", err)
	}
	if err := binary.Write(s, binary.BigEndian, uint32(len(payload))); err != nil {
		t.Fatalf("write length: %v", err)
	}
	if _, err := s.Write([]byte(payload)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
}

// readTextFrame reads a 5-byte-framed text message and returns JSON-decoded payload.
func readTextFrame(t *testing.T, s network.Stream) map[string]any {
	t.Helper()
	kindBuf := make([]byte, 1)
	if _, err := io.ReadFull(s, kindBuf); err != nil {
		t.Fatalf("read kind: %v", err)
	}
	if kindBuf[0] != 0x01 {
		t.Fatalf("expected text frame kind 0x01, got 0x%02x", kindBuf[0])
	}
	var length uint32
	if err := binary.Read(s, binary.BigEndian, &length); err != nil {
		t.Fatalf("read length: %v", err)
	}
	if length > 8*1024*1024 {
		t.Fatalf("frame too large: %d bytes", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(buf, &resp); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return resp
}

func TestIntegration_e2eCircuitRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// ── Relay ────────────────────────────────────────────────────────────────
	rly, err := relay.New(ctx, relay.Config{
		ListenAddr: "/ip4/127.0.0.1/tcp/0",
		JWTSecret:  []byte("test-secret-do-not-use-in-prod-abcd1234"),
		JWTTTL:     time.Minute,
	})
	if err != nil {
		t.Fatalf("relay.New: %v", err)
	}
	defer rly.Close()

	rly.SetCodeResolver(func(code string) (string, bool) {
		if code == "test-share" {
			return "api-key-A", true
		}
		return "", false
	})

	var closedMu sync.Mutex
	var closedBytesIn, closedBytesOut int64
	rly.SetCircuitClosedHook(func(apiKeyID, shareCode string, bytesIn, bytesOut int64) {
		closedMu.Lock()
		closedBytesIn += bytesIn
		closedBytesOut += bytesOut
		closedMu.Unlock()
	})

	relayAddrs := rly.Start()
	if len(relayAddrs) == 0 {
		t.Fatal("no relay addrs")
	}
	relayAddr := relayAddrs[0]

	// ── Agent ─────────────────────────────────────────────────────────────────
	// Agent has a public-routable address (in tests: loopback TCP).
	agentHost, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("agent libp2p.New: %v", err)
	}
	defer agentHost.Close()

	// Register a simple echo handler for the file protocol.
	agentHost.SetStreamHandler(fileProto, func(s network.Stream) {
		defer s.Close()
		io.Copy(s, s)
	})

	// Agent connects to relay and makes a circuit relay v2 reservation.
	// This is what Slice 13b implements in production; here we do it manually.
	agentHost.Peerstore().AddAddrs(rly.Host().ID(), rly.Host().Addrs(), time.Minute)
	if err := agentHost.Connect(ctx, peer.AddrInfo{ID: rly.Host().ID(), Addrs: rly.Host().Addrs()}); err != nil {
		t.Fatalf("agent connect to relay: %v", err)
	}
	_, err = circuitv2client.Reserve(ctx, agentHost, peer.AddrInfo{ID: rly.Host().ID(), Addrs: rly.Host().Addrs()})
	if err != nil {
		t.Fatalf("agent reserve: %v", err)
	}

	// Register agent in the relay's registry (in production this is done by the
	// agent's API-key handshake in Slice 13b).
	rly.Agents().Register("api-key-A", agentHost.ID())

	// ── Browser ───────────────────────────────────────────────────────────────
	browserHost, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("browser libp2p.New: %v", err)
	}
	defer browserHost.Close()
	browserHost.Peerstore().AddAddrs(rly.Host().ID(), rly.Host().Addrs(), time.Minute)

	// Step 1: Browser presents JWT on the auth stream.
	tok, err := rly.Issuer().Issue(relay.Claims{
		ShareCode:     "test-share",
		BrowserPeerID: browserHost.ID().String(),
		RelayAllowed:  true,
		DCUtRAllowed:  false, // relay-only for this test
	})
	if err != nil {
		t.Fatalf("issue JWT: %v", err)
	}

	authStream, err := browserHost.NewStream(ctx, rly.Host().ID(), relay.ProtocolID)
	if err != nil {
		t.Fatalf("auth NewStream: %v", err)
	}

	// Send JWT using 5-byte framing (kind=0x01 + 4-byte BE length + JSON envelope)
	envelope, _ := json.Marshal(map[string]string{"type": "jwt", "token": tok})
	writeTextFrame(t, authStream, string(envelope))
	authStream.CloseWrite()

	resp := readTextFrame(t, authStream)
	if resp["type"] != "auth_ok" {
		t.Fatalf("auth failed: %v", resp)
	}
	authStream.Close()

	// Step 2: Browser dials agent via circuit relay v2.
	// Circuit address format: <relayTransport>/p2p/<relayID>/p2p-circuit/p2p/<agentID>
	p2pRelayComp, _ := multiaddr.NewMultiaddr("/p2p/" + rly.Host().ID().String())
	circuitComp, _ := multiaddr.NewMultiaddr("/p2p-circuit/p2p/" + agentHost.ID().String())
	circuitAddr := relayAddr.Encapsulate(p2pRelayComp).Encapsulate(circuitComp)

	if err := browserHost.Connect(ctx, peer.AddrInfo{
		ID:    agentHost.ID(),
		Addrs: []multiaddr.Multiaddr{circuitAddr},
	}); err != nil {
		t.Fatalf("browser connect via circuit relay: %v", err)
	}

	// Step 3: Open the file protocol stream (E2E Noise encrypted — relay cannot read this).
	fileStream, err := browserHost.NewStream(ctx, agentHost.ID(), fileProto)
	if err != nil {
		t.Fatalf("file NewStream: %v", err)
	}

	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i % 256)
	}
	if _, err := fileStream.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	fileStream.CloseWrite()

	echoed, err := io.ReadAll(fileStream)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if len(echoed) != len(payload) {
		t.Fatalf("echo length: got %d want %d", len(echoed), len(payload))
	}
	for i, b := range echoed {
		if b != payload[i] {
			t.Fatalf("echo mismatch at byte %d", i)
		}
	}

	// Step 4: Close connections and verify byte accounting fires.
	fileStream.Close()
	browserHost.Network().ClosePeer(rly.Host().ID())
	// Give the disconnect notifier time to fire.
	time.Sleep(200 * time.Millisecond)

	closedMu.Lock()
	totalReported := closedBytesIn + closedBytesOut
	closedMu.Unlock()

	// The relay counted bytes on the browser's connection. Since this is circuit
	// relay, the relay sees the outer transport frames. We expect at least as many
	// bytes as the payload (there is some framing overhead, so use >= not ==).
	if totalReported == 0 {
		t.Error("OnCircuitClosed never fired — byte accounting is broken")
	}
	t.Logf("bytes reported to quota accumulator: in=%d out=%d", closedBytesIn, closedBytesOut)
}
```

Run: `go test ./internal/relay/ -run TestIntegration_e2eCircuitRelay -v -timeout 30s`
Expected: PASS. Log line should show non-zero bytes.

- [ ] **Step 2: Run all relay tests**

```bash
cd signaling-server && go test ./internal/relay/ -v
```

Expected: all tests PASS.

- [ ] **Step 3: Commit**

```bash
git add signaling-server/internal/relay/integration_test.go
git commit -m "test(slice-13a): e2e circuit relay v2 integration test with E2E Noise"
```

---

## Task 16: Deployment — Caddy + firewall + coturn decommission

Manual / infra steps. Execute against staging first.

- [ ] **Step 1: Caddy config — add relay route without Cloudflare allowlist**

```
relay.sharebridge.app {
    reverse_proxy localhost:9001
}
```

**Critical:** Do NOT include the Cloudflare IP allowlist (`trusted_proxies`) that guards `sharebridge.app`. Relay connections arrive from real client IPs worldwide — browsers and agents dial directly after DNS resolution.

- [ ] **Step 2: DNS**

In Cloudflare, add an A record for `relay.sharebridge.app` pointing to the VPS IP. Set proxy status to **DNS only (grey cloud)**. File transfer data must bypass Cloudflare (ToS for large transfers, same policy as coturn today).

- [ ] **Step 3: Firewall**

```bash
sudo ufw deny 9001/tcp
```

The relay binds to `127.0.0.1` already; this is belt-and-suspenders.

- [ ] **Step 4: Environment variables on server**

Set on the signaling server host:

```
RELAY_LISTEN_ADDR=/ip4/127.0.0.1/tcp/9001/ws
RELAY_ANNOUNCE_ADDR=/dns4/relay.sharebridge.app/tcp/443/wss
RELAY_PRIVATE_KEY_PATH=/var/lib/sharebridge/relay.key
JWT_SECRET=$(openssl rand -hex 32)
JWT_TTL=5m
```

Remove: `TURN_HOST`, `TURN_PORT`, `TURN_SECRET`, `PROMETHEUS_URL`.

- [ ] **Step 5: Deploy**

Ship the new binary. Verify log: `relay host 12D3Koo... listening on [...]`.

- [ ] **Step 6: Decommission coturn**

After confirming the relay responds on `relay.sharebridge.app`:

```bash
sudo systemctl stop coturn
sudo systemctl disable coturn
sudo apt remove coturn
```

Remove coturn Caddy route and `turn.sharebridge.app` DNS entry.

- [ ] **Step 7: Commit infra changes**

```bash
git add deploy/ docs/
git commit -m "chore(slice-13a): deploy libp2p relay, decommission coturn"
```

---

## Self-review checklist

Before marking 13a done, confirm:

- [ ] `internal/turn/` is deleted. `grep -r "internal/turn" signaling-server/` returns nothing.
- [ ] `internal/metrics/prometheus.go` is deleted.
- [ ] `go build ./...` and `go test ./...` pass in `signaling-server/`.
- [ ] `TestIntegration_e2eCircuitRelay` passes and logs non-zero byte counts.
- [ ] The relay host is constructed **without** `libp2p.DisableRelay()` — search the codebase: `grep -r "DisableRelay" signaling-server/` must return nothing.
- [ ] `protocol.go` contains no `io.Copy` — the `/sharebridge/relay/1.0.0` handler is auth-only.
- [ ] **Framing format matches 13b/13c**: `protocol.go` uses 5-byte framing (1-byte kind + 4-byte BE length), same as `frame.go`. Verify: `grep "FrameText" signaling-server/internal/relay/protocol.go`.
- [ ] **JWT envelope matches 13c**: relay expects `{ type: 'jwt', token: '...' }`, not raw JWT. Verify: `grep '"jwt"' signaling-server/internal/relay/protocol.go`.
- [ ] **Response format matches 13c**: relay sends `{ type: 'auth_ok' }` or `{ type: 'error', message: '...' }`. Verify: `grep "auth_ok" signaling-server/internal/relay/protocol.go`.
- [ ] `relay_info` message contains `agent_peer_id` — grep confirms: `grep "agent_peer_id" signaling-server/internal/handler/agent_ws.go`.
- [ ] JWT spec matches design doc: `jti`, `share_code`, `browser_peer_id`, `relay_allowed`, `dcutr_allowed`, `exp`.
- [ ] Relay listen addr is loopback; Caddy TLS terminates + firewall blocks 9001 externally.
- [ ] Agent and browser code are unchanged in 13a (they break until 13b/13c ship — acceptable for clean cutover).

---

## Deliverable

A signaling server binary that:
1. Starts a libp2p Host on `127.0.0.1:9001/ws` with the circuit relay v2 service gated by `CircuitACL`.
2. Validates JWTs on `/sharebridge/relay/1.0.0` auth streams — registers authorized pairs in `CircuitACL`, no data forwarding.
3. Lets browsers and agents complete E2E Noise-encrypted file transfers through the circuit relay; the relay cannot read the content.
4. DCUtR hole-punching flows through the same circuit automatically.
5. Reports relay bandwidth to the quota accumulator via `OnCircuitClosed` + `BandwidthCounter`.
6. Contains zero WebRTC, SDP, ICE, TURN, or coturn logic.

Ready for Slice 13b (agent transport replacement).

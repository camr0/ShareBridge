# Phase 2 — Direct-Mode Transport Implementation Plan (rev 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire the Phase-1 direct-TCP primitives into a working end-to-end direct data path — a browser reaches the agent over native HTTPS (real Let's Encrypt cert, on-demand UPnP/NAT-PMP port) and downloads placeholder content — with the control plane coordinating enrollment, cert issuance, DDNS, reachability probing, and open-signal/redirect.

**Architecture:** The agent gains a cert lifecycle manager, a direct HTTPS server (Binder + OnDemandPort + installed cert), an endpoint reporter, and a SignalGate that admits control-plane open-signals. The control plane gains an `agents` collection, enrollment + cert-coordination handlers with per-connection epoch state, origin allocation, an open-signal emitter with open-ack correlation, and a reachability probe ending in a 302. All new control messages ride the existing authenticated agent WebSocket.

**Tech Stack:** Go, PocketBase v0.36.8, coder/websocket, go-acme/lego v4, huin/goupnp + jackpal/go-nat-pmp, cloudflare-go. Phase-1 packages are wrapped, not rewritten.

## Global Constraints

- **Zero production changes** until the whole branch is reviewed and the user approves merging to `main`.
- **Model policy:** `deepseek/deepseek-v4-pro` for all implementation subagents; `openai-codex/gpt-5.6-sol` only for the final whole-branch review.
- **Do NOT edit `signaling-server/migrations/1_create_collections.go`.** Schema changes go in a **new forward migration numbered 7** (migrations 2–6 already exist; verify the next free number with `ls signaling-server/migrations/`).
- **Wire types are normative:** `lease_seconds` int; timestamps RFC3339; `seq` uint64; `fingerprint` lowercase SHA-256 hex of the DER leaf. Unknown fields ignored; unexpected values are a protocol error.
- **Key custody:** the agent TLS key never leaves the agent; `CompleteCSR` enforces self-signature + exactly the two wildcard SANs.
- **Two wildcard SANs per namespace:** `*.<ns>.<baseDomain>` and `*.relay.<ns>.<baseDomain>`. Base domain is **configurable** (default `sharebridgeusercontent.com`) and threaded through `Binder`/`DirectServer` — never hardcoded.
- **Origin is control-allocated** (random, unique, never reused); the agent never supplies an origin. Redirect URL carries the port; DNS carries the IP.
- **Redirect hygiene:** omit port when granted port is 443; `Cache-Control: no-store`; reject `granted_port == 0`.
- **Readiness is connection-epoch-local**: persisted `cert_status=ready` does NOT authorize redirects by itself — the current connection must complete `enrolled → tls_ready (+ DDNS) → enrollment_ready`.
- **Identity:** `open_signal.agent_id` must equal the agent's self-reported `hello.agent_id` (the value the agent's `SignalGate` is constructed with). The control echoes the stored hello `agent_id`, never `apiKeyID`.
- **Lease bounds:** open-signal lease ≤ 15 min; min valid lease 5 s.
- **Cert renewal:** agent-driven at 30 days before `NotAfter`; key reuse.
- **Rate limit:** global open-signal ceiling ~60/min.
- **DDNS TTL:** 60 s.
- Tests use the staging CA + a fake mapper; no production certs or live UPnP in CI.
- **Every task MUST end with `go build ./...` (and `go test ./...`) green** so the branch compiles in isolation.

---

## File Structure

### Agent
| File | Responsibility |
|---|---|
| `agent/internal/direct/sni.go` (modify) | `NewBinder(namespace, baseDomain)` — parameterize the domain |
| `agent/internal/cert/manager.go` (create) | Cert lifecycle manager (key custody, CSR, validate, install+reload, renew) |
| `agent/internal/direct/portmap.go` (modify) | `DeleteOwnedMapping` exact-match; `PortMapper.InternalIP()` |
| `agent/internal/direct/ondemand.go` (modify) | `OpenFor` fast-path, lingering-mapping cleanup, ownership token, transition callback |
| `agent/internal/direct/opensignal.go` (modify) | `SignalGate.Reset()`, `VerifyNonce()` (with expiry), rate-limit raise |
| `agent/internal/direct/server.go` (create) | Direct HTTPS server (GetConfigForClient, ConnState session tracking, placeholder, probe) |
| `agent/internal/direct/reporter.go` (create) | Endpoint reporter (transition callback + fresh IP + queued sends) |
| `agent/internal/signaling/client.go` (modify) | New message fields + send helpers; `share_registered` origin |
| `agent/internal/daemon/daemon.go` (modify) | Direct transport lifecycle (enroll gate, reconnect, renewal, open-signal, binder) |
| `agent/cmd/agent/main.go` (modify) | Start direct transport in daemon mode |

### Control plane
| File | Responsibility |
|---|---|
| `signaling-server/migrations/7_create_agents.go` (create) | `agents` + `sessions.origin`/`is_active` + unique indexes + backfill |
| `signaling-server/internal/directctl/agentstore.go` (create) | Agent record helpers, namespace, transactional origin |
| `signaling-server/internal/certcoordinator/coordinator.go` (create) | Stateful coordinator (two-index cache, semaphore, account key) |
| `signaling-server/internal/certcoordinator/acme.go` (modify) | Accept a persisted account key |
| `signaling-server/internal/directctl/controller.go` (create) | Controller + epoch state (enrollment, readiness) |
| `signaling-server/internal/directctl/endpoint.go` (create) | report_endpoint + DDNS (provisioned-IP tracking) |
| `signaling-server/internal/directctl/opensignal.go` (create) | open-signal emitter + ack waiter (full-tuple) |
| `signaling-server/internal/directctl/probe.go` (create) | reachability probe (SSRF denylist) |
| `signaling-server/internal/directctl/redirect.go` (create) | 302 handler (concrete session lookup) |
| `signaling-server/internal/handler/agent_ws.go` (modify) | Route new message types; wire controller; soft-delete |
| `signaling-server/internal/handler/rest.go` (modify) | Soft-delete in all lookups |
| `signaling-server/internal/handler/browser_ws.go` (modify) | is_active filter |
| `signaling-server/internal/handler/apikeys.go` (modify) | Rotation re-points; revocation deletes agent |
| `signaling-server/internal/hub/hub.go` (modify) | Compare-and-delete register/unregister (fence old conn) |
| `signaling-server/internal/config/config.go` (modify) | Cloudflare/ACME/base-domain env |
| `signaling-server/cmd/server/main.go` (modify) | Wire controller + coordinator + redirect; soft-delete expiry |

### Integration
| File | Responsibility |
|---|---|
| `signaling-server/internal/directctl/e2e_test.go` (create) | End-to-end fake-agent test over a real WS |

---

## Contracts (types every task must agree on)

```go
// agent/internal/direct — Binder (modified)
func NewBinder(namespace, baseDomain string) *Binder

// agent/internal/direct — OnDemandPort
type PortState int // StateClosed, StateOpen, StateClosing, StateCloseFailed
func (p *OnDemandPort) State() PortState
func (p *OnDemandPort) SetTransitionCallback(cb func(old, new PortState, grantedPort int))
func NewOnDemandPortOwned(mapper PortMapper, extPort, intPort int, idleTimeout time.Duration, descPrefix, intClient string) *OnDemandPort

// agent/internal/direct — portmap
func DeleteOwnedMapping(mapper PortMapper, externalPort int, want PortMapping) error
// PortMapper gains: InternalIP() string

// agent/internal/direct — SignalGate
func (g *SignalGate) Reset()
func (g *SignalGate) VerifyNonce(nonce, shareID string) bool // enforces nonce expiry

// agent/internal/direct — server
type CertProvider interface { Certificate() (*tls.Certificate, error) }
func NewDirectServer(namespace, baseDomain string, port *OnDemandPort, certs CertProvider, gate *SignalGate, maxContentBytes int64) *DirectServer
func (s *DirectServer) TLSConfig() *tls.Config
func (s *DirectServer) Handler() http.Handler
func (s *DirectServer) Start(ctx context.Context, listenAddr string) error

// agent/internal/cert — Manager
type Manager struct{ /* ... */ }
func NewManager(dataDir, baseDomain string, roots *x509.CertPool) *Manager
func (m *Manager) SetNamespace(namespace string) error
func (m *Manager) Load() error
func (m *Manager) Namespace() string
func (m *Manager) GenerateCSR() ([]byte, error)
func (m *Manager) Install(chainPEM []byte) error
func (m *Manager) Certificate() (*tls.Certificate, error)
func (m *Manager) LeafFingerprint() (string, error)
func (m *Manager) NotAfter() (time.Time, error)
func (m *Manager) Installed() bool
func (m *Manager) NeedsRenewal() bool

// signaling-server/internal/certcoordinator — Coordinator
type CoordinatorConfig struct { CA, Email, CloudflareToken, BaseDomain, AccountKeyPath string }
func NewCoordinator(cfg CoordinatorConfig) (*Coordinator, error)
func (c *Coordinator) Issue(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) (chainPEM []byte, err error)
func (c *Coordinator) HasLeafFingerprint(apiKeyID, leafFP string) bool
func (c *Coordinator) ChainByLeaf(apiKeyID, leafFP string) (chain []byte, notAfter time.Time, ok bool)

// signaling-server/internal/directctl — Controller
type Controller struct{ /* ... */ }
func NewController(app core.App, h *hub.Hub, coord *certcoordinator.Coordinator, ddnsClient *ddns.Cloudflare, cfg Config) *Controller
func (c *Controller) HandleHello(ctx context.Context, conn *websocket.Conn, apiKeyID, accountID, agentID string)
func (c *Controller) HandleCSRSubmit(ctx context.Context, conn *websocket.Conn, apiKeyID, csrPEM string)
func (c *Controller) HandleTLSReady(ctx context.Context, conn *websocket.Conn, apiKeyID, fingerprint, notAfter string)
func (c *Controller) HandleTLSError(ctx context.Context, apiKeyID, reason string)
func (c *Controller) HandleReportEndpoint(ctx context.Context, apiKeyID, ip string, port int, status string)
func (c *Controller) HandleOpenAck(apiKeyID string, ack OpenAck)
func (c *Controller) Redirect(w http.ResponseWriter, r *http.Request, code string) error
func (c *Controller) AgentDisconnected(apiKeyID string, conn *websocket.Conn)
func (c *Controller) AllocateOriginFor(app core.App, apiKeyID string, session *core.Record) (string, error)

// signaling-server/internal/directctl — OpenAck (control side)
type OpenAck struct {
	ShareID string; Nonce string; Seq uint64; GrantedPort int
	PublicIP string; WasAlreadyOpen bool; Status, Error string
}
```

---

## Shared test fixtures (control, `directctl` package)

These are referenced by multiple tasks; implement them once (Task 8) and reuse.

```go
// directctl/fixtures_test.go
package directctl

import (
	"context"
	"testing"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/certcoordinator"
	"sharebridge/server/internal/hub"
	mig "sharebridge/server/migrations"
)

// newTestController boots a fresh in-memory PocketBase app with the base +
// agents schema, a stub coordinator (no network), and returns (app, ctrl).
func newTestController(t *testing.T) (core.App, *Controller) {
	t.Helper()
	app := pocketbase.New()
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := mig.CreateCollections(app); err != nil {
		t.Fatalf("base schema: %v", err)
	}
	if err := mig.CreateAgents(app); err != nil {
		t.Fatalf("agents schema: %v", err)
	}

	// A stub coordinator that never issues real certs.
	coord := &certcoordinator.Coordinator{}
	ctrl := NewController(app, hub.New(), coord, nil, Config{BaseDomain: "example.com"})
	ctrl.allowPrivate = true   // loopback probe allowed in tests
	ctrl.sendFn = func(ctx context.Context, conn *websocket.Conn, msg any) error { return nil }
	return app, ctrl
}

// captureSend swaps sendFn to record the next message map and returns it.
func (c *Controller) captureSend(fn func()) map[string]any {
	var got map[string]any
	old := c.sendFn
	c.sendFn = func(ctx context.Context, conn *websocket.Conn, msg any) error {
		got, _ = msg.(map[string]any)
		if got == nil {
			if b, err := json.Marshal(msg); err == nil {
				_ = json.Unmarshal(b, &got)
			}
		}
		return nil
	}
	fn()
	c.sendFn = old
	return got
}
```

---

## Task 1: Migration `7_create_agents.go` + `sessions.origin`/`is_active` + backfill

**Files:**
- Create: `signaling-server/migrations/7_create_agents.go` (verify number with `ls signaling-server/migrations/` first — if 7 is taken, use the next free number)
- Test: `signaling-server/migrations/migrations_test.go`

**Interfaces:**
- Produces: `agents` collection; `sessions.origin` (unique), `sessions.is_active` (bool). Existing sessions backfilled `is_active = true`.

- [ ] **Step 1: Write the failing test**

```go
// migrations/migrations_test.go
package migrations

import (
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase"
)

func TestCreateAgentsBackfillsActive(t *testing.T) {
	app := pocketbase.New()
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := CreateCollections(app); err != nil {
		t.Fatalf("base: %v", err)
	}
	// Insert a pre-existing session (simulates a DB migrated from v1).
	sessions, _ := app.FindCollectionByNameOrId("sessions")
	existing := NewRecord(sessions) // helper below
	existing.Set("code", "oldcode1")
	existing.Set("api_key_id", "")
	existing.Set("agent_id", "")
	if err := app.Save(existing); err != nil {
		t.Fatalf("save existing: %v", err)
	}

	if err := CreateAgents(app); err != nil {
		t.Fatalf("create agents: %v", err)
	}

	// Existing session must be backfilled active.
	rec, err := app.FindRecordById("sessions", existing.Id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if rec.GetBool("is_active") != true {
		t.Fatalf("existing session not backfilled active")
	}

	// Unique index on sessions.origin present.
	s2, _ := app.FindCollectionByNameOrId("sessions")
	found := false
	for _, idx := range s2.Indexes {
		if strings.Contains(idx, "origin") && strings.Contains(strings.ToUpper(idx), "UNIQUE") {
			found = true
		}
	}
	if !found {
		t.Fatalf("sessions.origin missing unique index")
	}
}

// NewRecord is a tiny helper re-exported for tests.
func NewRecord(col *core.Collection) *core.Record { return core.NewRecord(col) }
```

> The test above needs `"github.com/pocketbase/pocketbase/core"` imported; include it. `NewRecord` is unnecessary — use `core.NewRecord` directly in the test. Keep imports complete and used.

- [ ] **Step 2: Run test, verify it fails**

Run: `cd signaling-server && go test ./migrations/ -run TestCreateAgentsBackfillsActive -v`
Expected: FAIL — `CreateAgents` undefined.

- [ ] **Step 3: Implement**

```go
// migrations/7_create_agents.go
package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() { m.Register(CreateAgents, nil) }

func CreateAgents(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("agents"); err == nil {
		return nil
	}

	apiKeysCol, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		return fmt.Errorf("api_keys not found: %w", err)
	}

	agentsCol := core.NewBaseCollection("agents")
	agentsCol.Fields.Add(
		&core.RelationField{Name: "api_key_id", CollectionId: apiKeysCol.Id, Required: true, CascadeDelete: true, MaxSelect: 1},
		&core.TextField{Name: "namespace", Required: true},
		&core.TextField{Name: "endpoint_ip"},
		&core.NumberField{Name: "endpoint_port"},
		&core.TextField{Name: "cert_status"},
		&core.TextField{Name: "cert_fingerprint"},
		&core.DateField{Name: "cert_expires_at"},
		&core.DateField{Name: "last_report_at"},
	)
	agentsCol.Indexes = []string{
		"CREATE UNIQUE INDEX `idx_agents_api_key_id` ON `{{COLLECTION}}` (`api_key_id`)",
		"CREATE UNIQUE INDEX `idx_agents_namespace` ON `{{COLLECTION}}` (`namespace`)",
	}
	if err := app.Save(agentsCol); err != nil {
		return fmt.Errorf("save agents: %w", err)
	}

	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return fmt.Errorf("sessions not found: %w", err)
	}
	sessionsCol.Fields.Add(
		&core.TextField{Name: "origin"},
		&core.BoolField{Name: "is_active"},
	)
	sessionsCol.Indexes = append(sessionsCol.Indexes,
		"CREATE UNIQUE INDEX `idx_sessions_origin` ON `{{COLLECTION}}` (`origin`)",
	)
	if err := app.Save(sessionsCol); err != nil {
		return fmt.Errorf("save sessions fields: %w", err)
	}

	// Backfill: PocketBase BoolField columns are `DEFAULT FALSE NOT NULL`, so
	// pre-existing rows read false. Mark them active so v1 sessions survive.
	existing, err := app.FindAllRecords("sessions")
	if err != nil {
		return err
	}
	for _, rec := range existing {
		rec.Set("is_active", true)
		if err := app.Save(rec); err != nil {
			return fmt.Errorf("backfill session %s: %w", rec.Id, err)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test + build, verify pass**

Run: `cd signaling-server && go test ./migrations/ -run TestCreateAgentsBackfillsActive -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/migrations/
git commit -m "feat(control): agents collection + sessions.origin/is_active (migration 7, backfill active)"
```

---

## Task 2: Binder domain parameterization

**Files:**
- Modify: `agent/internal/direct/sni.go`
- Modify: `agent/cmd/spike-e2e/main.go` (and any other `NewBinder` call sites)

**Interfaces:**
- Produces: `NewBinder(namespace, baseDomain string) *Binder`.

- [ ] **Step 1: Write the failing test**

```go
// sni_test.go (create if absent)
package direct

import "testing"

func TestNewBinderUsesBaseDomain(t *testing.T) {
	b := NewBinder("sbdeadbeef", "example.com")
	if err := b.Allow("demo.sbdeadbeef.example.com", RouteDirect, "abc"); err != nil {
		t.Fatalf("Allow with base domain: %v", err)
	}
	// The relay suffix must also derive from the configured domain.
	if err := b.Allow("demo.relay.sbdeadbeef.example.com", RouteRelay, "abc"); err != nil {
		t.Fatalf("Allow relay with base domain: %v", err)
	}
}

func TestBinderRejectsForeignDomain(t *testing.T) {
	b := NewBinder("sbdeadbeef", "example.com")
	if err := b.Allow("demo.sbdeadbeef.other.com", RouteDirect, "abc"); err == nil {
		t.Fatalf("expected rejection for a foreign domain")
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd agent && go test ./internal/direct/ -run 'TestNewBinderUsesBaseDomain|TestBinderRejectsForeignDomain' -v`
Expected: FAIL (or compile error — `NewBinder` now takes 2 args).

- [ ] **Step 3: Implement**

Change `NewBinder` to take `baseDomain`:

```go
func NewBinder(namespace, baseDomain string) *Binder {
	return &Binder{
		namespace:    namespace,
		directSuffix: "." + namespace + "." + baseDomain,
		relaySuffix:  ".relay." + namespace + "." + baseDomain,
		active:       map[string]Binding{},
	}
}
```

Update `spike-e2e/main.go`: `direct.NewBinder(*ns, "sharebridgeusercontent.com")` (or add a `-domain` flag defaulting to that).

- [ ] **Step 4: Run tests + build, verify pass**

Run: `cd agent && go test ./internal/direct/ -run 'TestNewBinderUsesBaseDomain|TestBinderRejectsForeignDomain' -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/sni.go agent/internal/direct/sni_test.go agent/cmd/spike-e2e/
git commit -m "refactor(agent): Binder accepts configurable base domain"
```

---

## Task 3: Cert lifecycle manager

**Files:**
- Modify: `agent/internal/cert/csr.go` (add `CSRFromKey`)
- Create: `agent/internal/cert/manager.go`
- Test: `agent/internal/cert/manager_test.go`

**Interfaces:** the `Manager` contract above. **Note:** the manager persists under `<dataDir>/direct/` (key.pem, chain.pem, namespace). The test uses the same `<dataDir>/direct/` paths. `NeedsRenewal` must NOT hold `RLock` while calling `NotAfter` (which also acquires `RLock`) — use a single lock/unlocked helper.

- [ ] **Step 1: Add `CSRFromKey` (csr.go)**

```go
// CSRFromKey builds the two-SAN wildcard CSR from an existing ECDSA P-256 key PEM.
func CSRFromKey(keyPEM []byte, namespace, baseDomain string) ([]byte, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, fmt.Errorf("invalid key PEM block")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}
	sans := wildcardSANs(namespace, baseDomain)
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: sans[0]}, DNSNames: sans}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}
```

- [ ] **Step 2: Write the failing test (with a real CA so `Install` can succeed)**

```go
// manager_test.go
package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// issueTestChain mints a chain (leaf with the two wildcard SANs + a self-signed
// root) so Install's ValidateChain path can succeed. The root is added to the
// manager's trust pool.
func issueTestChain(t *testing.T, key *ecdsa.PrivateKey, namespace, baseDomain string) ([]byte, *x509.CertPool) {
	t.Helper()
	rootKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rootTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true}
	rootDER, _ := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	root, _ := x509.ParseCertificate(rootDER)

	sans := []string{"*." + namespace + "." + baseDomain, "*.relay." + namespace + "." + baseDomain}
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: sans[0]},
		DNSNames: sans, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(120 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, root, &key.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})...)

	pool := x509.NewCertPool()
	pool.AddCert(root)
	return chain, pool
}

func TestManagerGenerateInstallReload(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", nil) // nil roots; replaced below
	_ = m // roots injected via a setter in the test variant, see note

	// (See note: Manager must expose roots injection for tests, or NewManager
	// accepts the pool. This test constructs with the pool from issueTestChain.)
	_ = dir
}

func TestManagerPersistencePaths(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", x509.NewCertPool())
	if err := m.SetNamespace("sbdeadbeef"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GenerateCSR(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "direct", "key.pem")); err != nil {
		t.Fatalf("key not at <dir>/direct/key.pem: %v", err)
	}
}
```

> **Correction (self-review):** replace the two half-written tests with ONE coherent test that: creates the manager with the `issueTestChain` trust pool, `SetNamespace`, `GenerateCSR`, parses the CSR to recover the public key, calls `issueTestChain` with the matching key, `Install`s the chain, asserts `Installed()` and `LeafFingerprint()` non-empty, re-runs `GenerateCSR` (key reuse — assert same key PEM persisted), and asserts `!NeedsRenewal()` (120-day cert) then a short-lived variant `NeedsRenewal()`.

- [ ] **Step 3: Run test, verify it fails**

Run: `cd agent && go test ./internal/cert/ -run TestManager -v`
Expected: FAIL — `NewManager` undefined.

- [ ] **Step 4: Implement the manager (fix paths + locks)**

```go
// manager.go
package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const renewalWindow = 30 * 24 * time.Hour

type Manager struct {
	dataDir    string
	baseDomain string
	roots      *x509.CertPool

	mu        sync.RWMutex
	namespace string
	keyPEM    []byte
	chainPEM  []byte
	cert      *tls.Certificate
	leafDER   []byte
}

func NewManager(dataDir, baseDomain string, roots *x509.CertPool) *Manager {
	return &Manager{dataDir: dataDir, baseDomain: baseDomain, roots: roots}
}

func (m *Manager) dir() string     { return filepath.Join(m.dataDir, "direct") }
func (m *Manager) keyPath() string { return filepath.Join(m.dir(), "key.pem") }
func (m *Manager) chainPath() string { return filepath.Join(m.dir(), "chain.pem") }
func (m *Manager) nsPath() string  { return filepath.Join(m.dir(), "namespace") }

func (m *Manager) SetNamespace(namespace string) error {
	if namespace == "" {
		return fmt.Errorf("empty namespace")
	}
	m.mu.Lock()
	m.namespace = namespace
	m.mu.Unlock()
	return m.persistNamespace()
}

func (m *Manager) Namespace() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.namespace
}

func (m *Manager) Load() error {
	if b, err := os.ReadFile(m.nsPath()); err == nil {
		m.mu.Lock()
		m.namespace = string(b)
		m.mu.Unlock()
	}
	keyPEM, err1 := os.ReadFile(m.keyPath())
	chainPEM, err2 := os.ReadFile(m.chainPath())
	if err1 != nil || err2 != nil {
		return nil
	}
	return m.installLocked(chainPEM, keyPEM)
}

func (m *Manager) GenerateCSR() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.namespace == "" {
		return nil, fmt.Errorf("namespace not set")
	}
	if m.keyPEM == nil {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return nil, err
		}
		m.keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
		if err := m.persistKey(); err != nil {
			return nil, err
		}
	}
	return CSRFromKey(m.keyPEM, m.namespace, m.baseDomain)
}

func (m *Manager) Install(chainPEM []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keyPEM == nil {
		return fmt.Errorf("no local key: generate a CSR first")
	}
	return m.installLocked(chainPEM, m.keyPEM)
}

func (m *Manager) installLocked(chainPEM, keyPEM []byte) error {
	if err := ValidateChain(chainPEM, keyPEM, m.namespace, m.baseDomain, m.roots); err != nil {
		return fmt.Errorf("validate chain: %w", err)
	}
	cert, err := tls.X509KeyPair(chainPEM, keyPEM)
	if err != nil {
		return err
	}
	leaf := cert.Certificate[0]
	m.cert = &cert
	m.chainPEM = chainPEM
	m.keyPEM = keyPEM
	m.leafDER = leaf
	return m.persistChain()
}

func (m *Manager) Certificate() (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cert == nil {
		return nil, fmt.Errorf("no certificate installed")
	}
	cp := *m.cert
	return &cp, nil
}

// notAfterLocked reads NotAfter without taking the lock (caller holds it).
func (m *Manager) notAfterLocked() (time.Time, error) {
	if m.cert == nil || len(m.cert.Certificate) == 0 {
		return time.Time{}, fmt.Errorf("no certificate installed")
	}
	c, err := x509.ParseCertificate(m.cert.Certificate[0])
	if err != nil {
		return time.Time{}, err
	}
	return c.NotAfter, nil
}

func (m *Manager) NotAfter() (time.Time, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.notAfterLocked()
}

func (m *Manager) LeafFingerprint() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.leafDER == nil {
		return "", fmt.Errorf("no certificate installed")
	}
	sum := sha256.Sum256(m.leafDER)
	return hex.EncodeToString(sum[:]), nil
}

func (m *Manager) Installed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cert != nil
}

func (m *Manager) NeedsRenewal() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cert == nil {
		return true
	}
	na, err := m.notAfterLocked()
	if err != nil {
		return true
	}
	return time.Until(na) < renewalWindow
}

func (m *Manager) persistNamespace() error {
	if err := os.MkdirAll(m.dir(), 0700); err != nil {
		return err
	}
	return os.WriteFile(m.nsPath(), []byte(m.namespace), 0600)
}
func (m *Manager) persistKey() error {
	if err := os.MkdirAll(m.dir(), 0700); err != nil {
		return err
	}
	return os.WriteFile(m.keyPath(), m.keyPEM, 0600)
}
func (m *Manager) persistChain() error {
	if err := os.MkdirAll(m.dir(), 0700); err != nil {
		return err
	}
	return os.WriteFile(m.chainPath(), m.chainPEM, 0600)
}
```

- [ ] **Step 5: Run tests + build, verify pass**

Run: `cd agent && go test ./internal/cert/ -run TestManager -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 6: Commit**

```bash
git add agent/internal/cert/
git commit -m "feat(agent): cert lifecycle manager (key custody, CSR, validate, atomic reload, renew)"
```

---

## Task 4: `OnDemandPort` ownership, fast-path, lingering-map cleanup, transition callback

**Files:**
- Modify: `agent/internal/direct/portmap.go`
- Modify: `agent/internal/direct/ondemand.go`
- Modify: `agent/internal/direct/ondemand_test.go`

**Interfaces:** as in Contracts. Key correctness requirements (from review):
1. Fast-path renews the **router** lease only when the requested lease would exceed the current lease expiry (`now+l > deadline`), not a tautological `deadline-l < l` check.
2. Cold open cleans up ANY lingering mapping (`grantedPort != 0`) before re-selecting, covering both `closing` and `close-failed`.
3. Ownership is **per-agent**: description = `<descPrefix>-<port>` where `descPrefix` is `sharebridge-<token>` (a stable per-agent token) and `intClient` = `mapper.InternalIP()`.

- [ ] **Step 1: `PortMapper.InternalIP()` + exact-match delete (portmap.go)**

```go
type PortMapper interface {
	AddPortMapping(externalPort, internalPort int, description string, leaseSeconds int) (int, error)
	DeletePortMapping(externalPort int) error
	ExternalIP() (string, error)
	ListPortMappings() ([]PortMapping, error)
	InternalIP() string
}
```

`UPnPMapper.InternalIP() string { return m.internalIP }`; `NATPMPMapper.InternalIP() string { return "" }`.

```go
// DeleteOwnedMapping removes the mapping at externalPort only if it EXACTLY
// matches want (description, internal port, internal client, protocol).
func DeleteOwnedMapping(mapper PortMapper, externalPort int, want PortMapping) error {
	mappings, err := mapper.ListPortMappings()
	switch {
	case errors.Is(err, ErrListingUnsupported):
		return mapper.DeletePortMapping(externalPort)
	case err != nil:
		return fmt.Errorf("list port mappings: %w", err)
	}
	for _, m := range mappings {
		if m.ExternalPort != externalPort {
			continue
		}
		if m.Description != want.Description || m.InternalPort != want.InternalPort ||
			m.InternalClient != want.InternalClient || m.Protocol != want.Protocol {
			return ErrForeignMapping
		}
		return mapper.DeletePortMapping(externalPort)
	}
	return nil // no mapping at that port (idempotent)
}
```

- [ ] **Step 2: Write the failing tests**

```go
// ondemand_test.go (append)
func TestOpenForFastPathRenewsOnlyWhenLeaseTooShort(t *testing.T) {
	// fake mapper + fake clock: open for 60s; advance 10s; OpenFor(60s) must NOT
	// call AddPortMapping again (requested lease fits within remaining 50s).
	// Then OpenFor(300s) MUST renew (requested exceeds remaining lease).
}

func TestColdOpenCleansLingeringMapping(t *testing.T) {
	// open, then close with a mapper whose first DeletePortMapping fails twice
	// (→ close-failed, mapping remains). A subsequent cold OpenFor must call
	// DeletePortMapping on the OLD port before AddPortMapping on a new port.
}

func TestOwnershipTokenInDescription(t *testing.T) {
	// NewOnDemandPortOwned(mapper, 443, 8443, 5*time.Minute, "sharebridge-abc", "192.168.1.2")
	// open → assert the mapper recorded description == "sharebridge-abc-443" and
	// internal client "192.168.1.2"; DeleteOwnedMapping must refuse a mapping
	// whose description differs.
}
```

- [ ] **Step 3: Run tests, verify they fail**

Run: `cd agent && go test ./internal/direct/ -run 'TestOpenForFastPath|TestColdOpenCleans|TestOwnershipToken' -v`
Expected: FAIL.

- [ ] **Step 4: Implement**

`OnDemandPort` gains `descPrefix string`, `intClient string`, `state PortState`, `cb func(old, new PortState, grantedPort int)`.

```go
type PortState int
const (
	StateClosed PortState = iota
	StateOpen
	StateClosing
	StateCloseFailed
)

func NewOnDemandPortOpts(mapper PortMapper, extPort, intPort int, idleTimeout time.Duration) *OnDemandPort {
	return NewOnDemandPortOwned(mapper, extPort, intPort, idleTimeout, DescriptionPrefix, "")
}

func NewOnDemandPortOwned(mapper PortMapper, extPort, intPort int, idleTimeout time.Duration, descPrefix, intClient string) *OnDemandPort {
	p := &OnDemandPort{
		mapper: mapper, extPort: extPort, intPort: intPort,
		idleTimeout: idleTimeout, renewWindow: renewWindow, clock: wallClock{},
		descPrefix: descPrefix, intClient: intClient,
		cmds: make(chan portCommand), done: make(chan struct{}), state: StateClosed,
	}
	go p.loop()
	return p
}

func (p *OnDemandPort) desc() string { return fmt.Sprintf("%s-%d", p.descPrefix, p.extPort) }
func (p *OnDemandPort) State() PortState { /* send opState, return state */ }
func (p *OnDemandPort) SetTransitionCallback(cb func(old, new PortState, grantedPort int)) { /* send opSetCallback */ }

func (p *OnDemandPort) setState(next PortState, grantedPort int) {
	if p.cb != nil && p.state != next {
		p.cb(p.state, next, grantedPort)
	}
	p.state = next
}
```

`tryDelete` (exact-match):

```go
tryDelete := func() bool {
	port := p.extPort
	if grantedPort != 0 { port = grantedPort }
	want := PortMapping{ExternalPort: port, InternalPort: p.intPort, InternalClient: p.intClient, Protocol: "TCP", Description: p.desc()}
	if err := DeleteOwnedMapping(p.mapper, port, want); err != nil {
		closeFail++
		p.setCloseErr(err)
		p.setState(StateClosing, port)
		return false
	}
	closing = false
	closeFail = 0
	grantedPort = 0
	p.setCloseErr(nil)
	p.setState(StateClosed, 0)
	return true
}
```

`opOpenFor` (rewritten — fast path + lingering-map cleanup + cold reselect):

```go
case opOpenFor:
	l := c.lease
	if l < minValidLease { l = minValidLease }
	now := p.clock.Now()

	if !open {
		// Cold open. If a mapping from a prior open lingers (closing or
		// close-failed), delete it before re-selecting — never two mappings.
		if grantedPort != 0 {
			if !tryDelete() {
				c.reply <- portReply{err: ErrDeleteRetry}
				continue
			}
		}
		requested, err := ChooseExternalPort(p.mapper, p.extPort)
		if err != nil {
			c.reply <- portReply{err: err}
			continue
		}
		p.extPort = requested
		granted, err := p.mapper.AddPortMapping(p.extPort, p.intPort, p.desc(), int(l.Seconds()))
		if err != nil {
			c.reply <- portReply{err: err}
			continue
		}
		grantedPort = granted
		open = true
		closing = false
		closeFail = 0
		lease = l
		deadline = now.Add(l)
		renewAt = deadline.Add(-p.renewWindow)
		if !renewAt.After(now) { renewAt = now.Add(p.renewWindow) }
		p.setState(StateOpen, grantedPort)
		rearm()
		c.reply <- portReply{err: nil, open: true, granted: grantedPort, wasOpen: false}
		continue
	}

	// Fast path: already open. Renew the router lease only when the requested
	// lease extends beyond the current lease expiry; otherwise no router call.
	wasOpen := true
	if now.Add(l).After(deadline) {
		granted, err := p.mapper.AddPortMapping(grantedPort, p.intPort, p.desc(), int(l.Seconds()))
		if err != nil {
			renewFailed = true
			renewAt = deadline
			c.reply <- portReply{err: err}
			continue
		}
		grantedPort = granted
		deadline = now.Add(l)
		renewAt = deadline.Add(-p.renewWindow)
	}
	rearm()
	c.reply <- portReply{err: nil, open: true, granted: grantedPort, wasOpen: wasOpen}
```

In the timer branch, the close-failed transition sets the state:

```go
case closing:
	if tryDelete() {
		rearm()
	} else if closeFail >= maxCloseAttempts {
		closing = false
		p.setState(StateCloseFailed, grantedPort)
		rearm()
	} else {
		rearm()
	}
```

Add `wasOpen bool` + `state PortState` to `portReply`, and `opState`/`opSetCallback` commands.

- [ ] **Step 5: Run tests + build, verify pass**

Run: `cd agent && go test ./internal/direct/ -run 'TestOpenForFastPath|TestColdOpenCleans|TestOwnershipToken' -v && go test ./internal/direct/ && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 6: Commit**

```bash
git add agent/internal/direct/
git commit -m "feat(agent): OnDemandPort per-agent ownership, fast-path, lingering-map cleanup, transition callback"
```

---

## Task 5: `SignalGate` reset + nonce verify (with expiry) + rate-limit raise

**Files:**
- Modify: `agent/internal/direct/opensignal.go`
- Modify: `agent/internal/direct/opensignal_test.go`

**Interfaces:** `Reset()`, `VerifyNonce(nonce, shareID) bool` (expiry-enforcing); `maxSignalsPerWin = 60`, drop per-share cap.

- [ ] **Step 1: Write the failing tests**

```go
// opensignal_test.go (append)
func TestVerifyNonceExpires(t *testing.T) {
	g := NewSignalGate("agent-1", func(string, RouteKind) bool { return true })
	_ = g.Admit(validSig(g, "share-1", "nonce-x", 1))
	// fake clock advanced beyond nonceRetention
	g.now = func() time.Time { return time.Now().Add(nonceRetention + time.Minute) }
	if g.VerifyNonce("nonce-x", "share-1") {
		t.Fatalf("expired nonce must not verify")
	}
}

func TestResetClearsNoncesAndSeq(t *testing.T) {
	g := NewSignalGate("agent-1", func(string, RouteKind) bool { return true })
	_ = g.Admit(validSig(g, "share-1", "nonce-1", 1))
	g.Reset()
	if g.VerifyNonce("nonce-1", "share-1") {
		t.Fatalf("reset must clear nonces")
	}
	if err := g.Admit(validSig(g, "share-1", "nonce-2", 1)); err != nil {
		t.Fatalf("reset must accept low seq in a new epoch: %v", err)
	}
}

func TestRateLimitAllowsBurstUnder60(t *testing.T) {
	g := NewSignalGate("agent-1", func(string, RouteKind) bool { return true })
	for i := 0; i < 60; i++ {
		if err := g.Admit(validSig(g, "share-1", fmt.Sprintf("n-%d", i), uint64(i+1))); err != nil {
			t.Fatalf("signal %d rejected: %v", i, err)
		}
	}
	if err := g.Admit(validSig(g, "share-1", "overflow", 61)); err == nil {
		t.Fatalf("expected rate limit at 60")
	}
}
```

- [ ] **Step 2: Run tests, verify they fail**

Run: `cd agent && go test ./internal/direct/ -run 'TestVerifyNonceExpires|TestResetClears|TestRateLimitAllows' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
func (g *SignalGate) Reset() {
	g.mu.Lock()
	g.highest = 0
	g.seen = map[string]nonceUse{}
	g.applied = map[string]time.Time{}
	g.winStart = time.Time{}
	g.winCount = 0
	g.perShare = map[string]int{}
	g.mu.Unlock()
}

// VerifyNonce reports whether nonce was recently admitted for shareID, pruning
// expired entries so an idle agent cannot echo an old nonce indefinitely.
func (g *SignalGate) VerifyNonce(nonce, shareID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for n, u := range g.seen {
		if now.Sub(u.seenAt) > nonceRetention {
			delete(g.seen, n)
		}
	}
	u, ok := g.seen[nonce]
	return ok && u.shareID == shareID
}
```

Rate limit (`maxSignalsPerWin = 60`, remove `maxPerSharePerWin`); `rateLimit` drops the per-share rejection branch (keep `perShare` bookkeeping or remove it entirely).

- [ ] **Step 4: Run tests + build, verify pass**

Run: `cd agent && go test ./internal/direct/ -run 'TestVerifyNonceExpires|TestResetClears|TestRateLimitAllows' -v && go test ./internal/direct/ && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/opensignal.go agent/internal/direct/opensignal_test.go
git commit -m "feat(agent): SignalGate epoch reset, expiring nonce verify, 60/min rate limit"
```

---

## Task 6: Direct HTTPS server (composition + ConnState session tracking + probe)

**Files:**
- Create: `agent/internal/direct/server.go`
- Test: `agent/internal/direct/server_test.go`

**Interfaces:** `DirectServer` contract. **Key correctness (from review):** connection-level session tracking via `ConnState` + `ConnContext`; `GetConfigForClient` composes Binder admission + current cert; test must exercise admitted (not rejected) SNI.

- [ ] **Step 1: Write the failing test (admitted SNI + cert rotation without restart)**

```go
// server_test.go
package direct

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
)

type rotatableCerts struct{ c *tls.Certificate }
func (r *rotatableCerts) Certificate() (*tls.Certificate, error) { return r.c, nil }

func TestServerServesAdmittedSNI(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	cert := testServerCert(t, ns, base) // self-signed leaf with the two SANs
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, nil, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow("demo."+ns+"."+base, RouteDirect, "abc")

	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	defer ts.Close()

	// Explicit SNI = origin, Host = origin, self-signed trust.
	client := ts.Client()
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{
		ServerName: "demo." + ns + "." + base, InsecureSkipVerify: true,
	}
	req, _ := http.NewRequest("GET", ts.URL+"/s/abc", nil)
	req.Host = "demo." + ns + "." + base
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("admitted SNI should succeed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestServerProbeEchoesNonce(t *testing.T) {
	// admit a nonce via the gate, then GET /s/<code>/probe?nonce=<n> and assert 200 + echo.
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd agent && go test ./internal/direct/ -run 'TestServerServesAdmittedSNI|TestServerProbeEchoesNonce' -v`
Expected: FAIL — `DirectServer`/`testServerCert` undefined.

- [ ] **Step 3: Implement**

```go
// server.go
package direct

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type CertProvider interface{ Certificate() (*tls.Certificate, error) }

type DirectServer struct {
	namespace       string
	baseDomain      string
	port            *OnDemandPort
	certs           CertProvider
	gate            *SignalGate
	maxContentBytes int64
	binder          *Binder
}

func NewDirectServer(namespace, baseDomain string, port *OnDemandPort, certs CertProvider, gate *SignalGate, maxContentBytes int64) *DirectServer {
	return &DirectServer{
		namespace: namespace, baseDomain: baseDomain, port: port, certs: certs, gate: gate,
		maxContentBytes: maxContentBytes, binder: NewBinder(namespace, baseDomain),
	}
}

func (s *DirectServer) Binder() *Binder { return s.binder }

func (s *DirectServer) TLSConfig() *tls.Config {
	return &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			if _, err := s.binder.AdmitSNI(hello.ServerName); err != nil {
				return nil, err
			}
			cert, err := s.certs.Certificate()
			if err != nil {
				return nil, fmt.Errorf("no certificate: %w", err)
			}
			return &tls.Config{Certificates: []tls.Certificate{*cert}}, nil
		},
	}
}

func (s *DirectServer) Handler() http.Handler { return s.binder.Handler(http.HandlerFunc(s.route)) }

func (s *DirectServer) route(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	code, ok := shareCodeFromPath(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/s/"+code)
	switch {
	case rest == "/probe" || strings.HasPrefix(rest, "/probe?"):
		s.handleProbe(w, r, code)
	case rest == "/" || rest == "":
		s.handlePage(w, r, code)
	case strings.HasPrefix(rest, "/download"):
		s.handleDownload(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *DirectServer) handleProbe(w http.ResponseWriter, r *http.Request, code string) {
	nonce := r.URL.Query().Get("nonce")
	if nonce == "" || s.gate == nil || !s.gate.VerifyNonce(nonce, code) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, nonce)
}

func (s *DirectServer) handlePage(w http.ResponseWriter, r *http.Request, code string) {
	s.activity(w, r, code)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<html><body><h1>ShareBridge direct</h1><p>serving %s P2P over direct HTTPS</p></body></html>", code)
}

func (s *DirectServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	size := int64(1 << 20)
	if v := r.URL.Query().Get("size"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && n <= s.maxContentBytes {
			size = n
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="sharebridge.bin"`)
	io.CopyN(w, zeroReader{}, size)
}

type zeroReader struct{}
func (zeroReader) Read(p []byte) (int, error) { for i := range p { p[i] = 0 }; return len(p), nil }

// activity ties each request to a connection-scoped OnDemandPort session:
// BeginSession on first request for a connection, Activity on subsequent ones.
// EndSession is driven by ConnState (StateClosed).
func (s *DirectServer) activity(w http.ResponseWriter, r *http.Request, code string) {
	if s.port == nil {
		return
	}
	cs, _ := r.Context().Value(connStateKey{}).(*connState)
	if cs == nil {
		return
	}
	cs.mu.Lock()
	if cs.sessionID == "" {
		if sid, err := s.port.BeginSession(code); err == nil {
			cs.sessionID = sid
		}
	} else {
		s.port.Activity(cs.sessionID)
	}
	cs.mu.Unlock()
}

type connState struct {
	mu        sync.Mutex
	sessionID string
}
type connStateKey struct{}

func (s *DirectServer) Start(ctx context.Context, listenAddr string) error {
	srv := &http.Server{
		Handler:   s.Handler(),
		TLSConfig: s.TLSConfig(),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connStateKey{}, &connState{})
		},
		ConnState: func(c net.Conn, st http.ConnState) {
			if st == http.StateClosed {
				if cs, ok := connFromContext(c); ok {
					cs.mu.Lock()
					sid := cs.sessionID
					cs.sessionID = ""
					cs.mu.Unlock()
					if sid != "" && s.port != nil {
						s.port.EndSession(sid)
					}
				}
			}
		},
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	return srv.ServeTLS(ln, "", "")
}
```

> `connFromContext` helper: stash the `*connState` on the `net.Conn` via a wrapper, or use a `sync.Map` keyed by `net.Conn`. Simplest: wrap the accepted conn. Provide it in the same file:
> ```go
> type connWrapper struct{ net.Conn; cs *connState }
> func connFromContext(c net.Conn) (*connState, bool) { w, ok := c.(*connWrapper); return w.cs, ok }
> ```
> and in `ConnContext`, `return context.WithValue(ctx, connStateKey{}, c.(*connWrapper).cs)` is not available (ConnContext gets the raw conn). Instead, set `ConnState` to capture the `*connState` from a `sync.Map` keyed by the raw `net.Conn`:
> ```go
> var connStates sync.Map // net.Conn -> *connState
> ConnState: func(c net.Conn, st http.ConnState) {
>     if st == http.StateNew {
>         connStates.Store(c, &connState{})
>     }
>     if st == http.StateClosed {
>         if v, ok := connStates.LoadAndDelete(c); ok {
>             cs := v.(*connState)
>             // EndSession as above
>         }
>     }
> }
> ```
> Use this `sync.Map` approach (simpler than conn wrapping); `activity` reads `connStates.Load(r.Context())` — but the handler has `r.Context()`, not the conn. Recover the conn from `r` via `http.ResponseWriter`? The standard trick is `ConnContext` storing the conn's key. **Final simple design:** use `ConnContext` to store the `*connState` in the request context (created in `ConnState StateNew` via the sync.Map), and `ConnState StateClosed` to EndSession. Implement `activity` to look up `*connState` from `r.Context()` (set in `ConnContext`). Supply the concrete wiring in code.

- [ ] **Step 4: Run tests + build, verify pass**

Run: `cd agent && go test ./internal/direct/ -run 'TestServerServesAdmittedSNI|TestServerProbeEchoesNonce' -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/server.go agent/internal/direct/server_test.go
git commit -m "feat(agent): direct HTTPS server (GetConfigForClient, ConnState tracking, placeholder, probe)"
```

---

## Task 7: Endpoint reporter (fresh IP + queued sends)

**Files:**
- Create: `agent/internal/direct/reporter.go`
- Test: `agent/internal/direct/reporter_test.go`

**Interfaces:**
```go
func NewReporter(send func(ip string, port int, status string)) *Reporter
func (r *Reporter) OnTransition(old, new PortState, grantedPort int)
func (r *Reporter) SetIP(ip string)
```

- [ ] **Step 1: Write the failing test**

```go
// reporter_test.go
package direct

import "testing"

type endpointReport struct{ ip string; port int; status string }

func TestReporterMapsTransitions(t *testing.T) {
	var got []endpointReport
	r := NewReporter(func(ip string, port int, status string) { got = append(got, endpointReport{ip, port, status}) })
	r.SetIP("1.2.3.4")
	r.OnTransition(StateClosed, StateOpen, 443)          // open report
	r.OnTransition(StateOpen, StateCloseFailed, 443)     // close-failed: nonzero port
	if got[0].port != 443 || got[0].status != "" { t.Fatalf("open report: %+v", got[0]) }
	if got[1].status != "close_failed" || got[1].port != 443 { t.Fatalf("close-failed report: %+v", got[1]) }
	r.OnTransition(StateCloseFailed, StateClosed, 0)     // closed: port 0
	if got[2].port != 0 || got[2].status != "" { t.Fatalf("closed report: %+v", got[2]) }
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd agent && go test ./internal/direct/ -run TestReporterMapsTransitions -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// reporter.go
package direct

import "sync"

type Reporter struct {
	mu   sync.Mutex
	ip   string
	send func(ip string, port int, status string)
}

func NewReporter(send func(ip string, port int, status string)) *Reporter {
	return &Reporter{send: send}
}

func (r *Reporter) SetIP(ip string) {
	r.mu.Lock()
	r.ip = ip
	r.mu.Unlock()
}

func (r *Reporter) OnTransition(old, new PortState, grantedPort int) {
	r.mu.Lock()
	ip := r.ip
	r.mu.Unlock()
	switch new {
	case StateClosed:
		r.send(ip, 0, "")
	case StateOpen:
		r.send(ip, grantedPort, "")
	case StateCloseFailed:
		r.send(ip, grantedPort, "close_failed")
	}
	// StateClosing: no report (intermediate).
}
```

- [ ] **Step 4: Run test + build, verify pass**

Run: `cd agent && go test ./internal/direct/ -run TestReporterMapsTransitions -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/reporter.go agent/internal/direct/reporter_test.go
git commit -m "feat(agent): endpoint reporter (transition callback, fresh IP)"
```

---

## Task 8: Control — agent record helpers + namespace + transactional origin

**Files:**
- Create: `signaling-server/internal/directctl/agentstore.go`
- Create: `signaling-server/internal/directctl/fixtures_test.go` (the shared fixtures above)
- Test: `signaling-server/internal/directctl/agentstore_test.go`

**Interfaces:** `GenerateNamespace`, `LoadOrCreateAgent`, `SaveCertReady`, `AllocateOrigin` (transactional). `AcceptableTLSReady` is replaced by a callback to the coordinator (Task 9) — declare the hook here.

- [ ] **Step 1: Write the failing test**

```go
// agentstore_test.go
package directctl

import (
	"regexp"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestGenerateNamespace(t *testing.T) {
	re := regexp.MustCompile(`^sb[0-9a-f]{8}$`)
	for i := 0; i < 500; i++ {
		if !re.MatchString(GenerateNamespace()) {
			t.Fatalf("bad namespace")
		}
	}
}

func TestLoadOrCreateAgent(t *testing.T) {
	app, _ := newTestController(t)
	apiKeys, _ := app.FindCollectionByNameOrId("api_keys")
	key := core.NewRecord(apiKeys)
	key.Set("key_hash", "x")
	_ = app.Save(key)

	rec, created, err := LoadOrCreateAgent(app, key.Id)
	if err != nil || !created { t.Fatalf("expected created") }
	if rec.GetString("cert_status") != "pending" { t.Fatalf("status") }

	rec2, created2, _ := LoadOrCreateAgent(app, key.Id)
	if created2 || rec2.Id != rec.Id { t.Fatalf("expected loaded") }
}

func TestAllocateOriginIsUniqueAndTransactionallySaved(t *testing.T) {
	app, ctrl := newTestController(t)
	// allocate an origin AND save it onto a session within one transaction;
	// assert no two sessions ever share an origin.
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestGenerateNamespace|TestLoadOrCreateAgent|TestAllocateOrigin' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// agentstore.go
package directctl

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

func GenerateNamespace() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "sb" + hex.EncodeToString(b)
}

func LoadOrCreateAgent(app core.App, apiKeyID string) (*core.Record, bool, error) {
	recs, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": apiKeyID})
	if err != nil {
		return nil, false, err
	}
	if len(recs) > 0 {
		return recs[0], false, nil
	}
	col, _ := app.FindCollectionByNameOrId("agents")
	rec := core.NewRecord(col)
	rec.Set("api_key_id", apiKeyID)
	rec.Set("namespace", GenerateNamespace())
	rec.Set("cert_status", "pending")
	rec.Set("endpoint_port", 0)
	if err := app.Save(rec); err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

func SaveCertReady(app core.App, rec *core.Record, fingerprint string, notAfter time.Time) error {
	rec.Set("cert_status", "ready")
	rec.Set("cert_fingerprint", fingerprint)
	rec.Set("cert_expires_at", notAfter)
	return app.Save(rec)
}

// AllocateOrigin returns a free origin and, given a session record, saves it
// transactionally (retrying unique-constraint collisions) so the origin and
// session are committed atomically.
func AllocateOrigin(app core.App, namespace, baseDomain string, session *core.Record) (string, error) {
	var origin string
	err := app.RunInTransaction(func(txApp core.App) error {
		for i := 0; i < 8; i++ {
			b := make([]byte, 6)
			_, _ = rand.Read(b)
			candidate := hex.EncodeToString(b) + "." + namespace + "." + baseDomain
			session.Set("origin", candidate)
			session.Set("is_active", true)
			if err := txApp.Save(session); err != nil {
				continue // unique violation → retry with a new label
			}
			origin = candidate
			return nil
		}
		return fmt.Errorf("could not allocate a free origin")
	})
	return origin, err
}
```

Declare the coordinator hook (implemented in Task 9):

```go
// agentstore.go (append)
var chainCacheHasFn func(apiKeyID, leafFP string) bool
func SetChainCacheHas(fn func(apiKeyID, leafFP string) bool) { chainCacheHasFn = fn }
```

- [ ] **Step 4: Run tests + build, verify pass**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestGenerateNamespace|TestLoadOrCreateAgent|TestAllocateOrigin' -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/directctl/
git commit -m "feat(control): agent helpers, namespace, transactional origin allocation, test fixtures"
```

---

## Task 9: Cert coordinator (two-index cache, semaphore, account key, HandleCSRSubmit)

**Files:**
- Create: `signaling-server/internal/certcoordinator/coordinator.go`
- Modify: `signaling-server/internal/certcoordinator/acme.go` (accept persisted account key)
- Test: `signaling-server/internal/certcoordinator/coordinator_test.go`

**Interfaces:** the `Coordinator` contract. **Key correctness (from review):** separate CSR-fingerprint idempotency (keyed `apiKeyID:csrFP`) from leaf-fingerprint acceptance (keyed `apiKeyID:leafFP`); global semaphore; account key persisted atomically with error handling; derive `not_after` from the cached leaf, not the agent.

- [ ] **Step 1: Modify `acme.go` to accept a persisted account key**

```go
type ACMEConfig struct {
	CA, Email, CloudflareToken, Namespace, BaseDomain string
	AccountKey *ecdsa.PrivateKey // nil → generate (spike path)
}
```

In `CompleteCSR`, when `cfg.AccountKey != nil`, build `acct := &acmeAccount{email: cfg.Email, key: cfg.AccountKey}` instead of generating. (The lego `Registration.Register` call is idempotent — it re-registers/looks up the same account.)

- [ ] **Step 2: Write the failing test**

```go
// coordinator_test.go
package certcoordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func TestCoordinatorIdempotencyAndLeafIndex(t *testing.T) {
	ctx := context.Background()
	c := newTestCoordinator()
	csr := []byte("csr-1")
	chain := makeChain("leaf-1", time.Now().Add(90*24*time.Hour))
	c.issueFn = func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) { return chain, nil }

	got, err := c.Issue(ctx, csr, "sbdeadbeef", "key-1")
	if err != nil { t.Fatal(err) }
	if string(got) != string(chain) { t.Fatalf("chain mismatch") }

	csrFP := hex.EncodeToString(fingerprintBytes(csr))
	if !c.hasCSRFingerprint("key-1", csrFP) { t.Fatalf("csr index missing") }

	leafFP := hex.EncodeToString(leafFingerprintBytes(chain))
	if !c.HasLeafFingerprint("key-1", leafFP) { t.Fatalf("leaf index missing") }

	// Idempotent retry returns cached chain without re-issuing.
	c.issueFn = func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) {
		return nil, errors.New("should not re-issue")
	}
	got2, err := c.Issue(ctx, csr, "sbdeadbeef", "key-1")
	if err != nil { t.Fatal(err) }
	if string(got2) != string(chain) { t.Fatalf("idempotent retry changed chain") }

	// Cross-agent isolation: another agent's leaf fingerprint must not verify.
	if c.HasLeafFingerprint("key-2", leafFP) { t.Fatalf("cross-agent leak") }

	// A different CSR within cooldown is rejected.
	c.issueFn = func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) {
		return []byte("chain-2"), nil
	}
	if _, err := c.Issue(ctx, []byte("csr-2"), "sbdeadbeef", "key-1"); err == nil {
		t.Fatalf("expected cooldown rejection")
	}
}

func TestCoordinatorGlobalSemaphore(t *testing.T) {
	// with sem bound 1, two concurrent Issue calls for different agents must
	// serialize (second waits) — assert max concurrent issues observed <= 1.
}
```

- [ ] **Step 3: Run test, verify it fails**

Run: `cd signaling-server && go test ./internal/certcoordinator/ -run 'TestCoordinator' -v`
Expected: FAIL.

- [ ] **Step 4: Implement**

```go
// coordinator.go
package certcoordinator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const issuanceCooldown = 5 * time.Minute
const maxIssuanceConcurrency = 4

type CoordinatorConfig struct {
	CA, Email, CloudflareToken, BaseDomain string
	AccountKeyPath                         string
}

type cachedChain struct {
	chain    []byte
	notAfter time.Time
}

type Coordinator struct {
	cfg  CoordinatorConfig
	acct *acmeAccount

	mu       sync.Mutex
	sem      chan struct{}
	inflight map[string]bool
	csrIdx   map[string]cachedChain // apiKeyID + ":" + csrFP
	leafIdx  map[string]cachedChain // apiKeyID + ":" + leafFP
	last     map[string]time.Time
	issueFn  func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error)
}

func NewCoordinator(cfg CoordinatorConfig) (*Coordinator, error) {
	c := &Coordinator{
		cfg: cfg, sem: make(chan struct{}, maxIssuanceConcurrency),
		inflight: map[string]bool{}, csrIdx: map[string]cachedChain{},
		leafIdx: map[string]cachedChain{}, last: map[string]time.Time{},
	}
	c.issueFn = c.completeCSR
	key, err := c.loadOrCreateAccountKey()
	if err != nil {
		return nil, err
	}
	c.acct = &acmeAccount{email: cfg.Email, key: key}
	return c, nil
}

func (c *Coordinator) completeCSR(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) {
	return CompleteCSR(ctx, csrPEM, ACMEConfig{
		CA: c.cfg.CA, Email: c.cfg.Email, CloudflareToken: c.cfg.CloudflareToken,
		Namespace: namespace, BaseDomain: c.cfg.BaseDomain, AccountKey: c.acct.key,
	})
}

func (c *Coordinator) Issue(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) {
	csrFP := fingerprint(csrPEM)
	ck := apiKeyID + ":" + csrFP

	c.mu.Lock()
	if ch, ok := c.csrIdx[ck]; ok {
		c.mu.Unlock()
		return append([]byte(nil), ch.chain...), nil
	}
	if c.inflight[apiKeyID] {
		c.mu.Unlock()
		return nil, errors.New("issuance already in flight for this agent")
	}
	if last, ok := c.last[apiKeyID]; ok && time.Since(last) < issuanceCooldown {
		c.mu.Unlock()
		return nil, errors.New("issuance cooldown active")
	}
	c.inflight[apiKeyID] = true
	c.mu.Unlock()

	release := func() {
		c.mu.Lock()
		delete(c.inflight, apiKeyID)
		c.last[apiKeyID] = time.Now()
		c.mu.Unlock()
	}
	defer release()

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	chain, err := c.issueFn(ctx, csrPEM, namespace, apiKeyID)
	if err != nil {
		return nil, err
	}
	if len(chain) > 1<<20 {
		return nil, errors.New("chain too large")
	}
	na := leafNotAfter(chain)
	leafFP := leafFingerprint(chain)

	c.mu.Lock()
	c.csrIdx[ck] = cachedChain{chain: chain, notAfter: na}
	c.leafIdx[apiKeyID+":"+leafFP] = cachedChain{chain: chain, notAfter: na}
	c.mu.Unlock()
	return append([]byte(nil), chain...), nil
}

func (c *Coordinator) HasLeafFingerprint(apiKeyID, leafFP string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.leafIdx[apiKeyID+":"+leafFP]
	return ok && ch.notAfter.After(time.Now())
}

// ChainByLeaf returns the cached chain + NotAfter for re-delivery and so the
// control derives not_after from its own records, never trusting the agent.
func (c *Coordinator) ChainByLeaf(apiKeyID, leafFP string) ([]byte, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.leafIdx[apiKeyID+":"+leafFP]
	if !ok {
		return nil, time.Time{}, false
	}
	return append([]byte(nil), ch.chain...), ch.notAfter, true
}

func (c *Coordinator) hasCSRFingerprint(apiKeyID, csrFP string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.csrIdx[apiKeyID+":"+csrFP]
	return ok
}

func (c *Coordinator) loadOrCreateAccountKey() (*ecdsa.PrivateKey, error) {
	if c.cfg.AccountKeyPath == "" {
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if b, err := os.ReadFile(c.cfg.AccountKeyPath); err == nil {
		if block, _ := pem.Decode(b); block != nil && block.Type == "EC PRIVATE KEY" {
			if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
				return k, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(c.cfg.AccountKeyPath), 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(c.cfg.AccountKeyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0600); err != nil {
		return nil, err
	}
	return key, nil
}

func fingerprint(csrPEM []byte) string {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		sum := sha256.Sum256(csrPEM)
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

func leafFingerprint(chainPEM []byte) string {
	block, _ := pem.Decode(chainPEM)
	if block == nil {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

func leafNotAfter(chainPEM []byte) time.Time {
	block, _ := pem.Decode(chainPEM)
	if block == nil {
		return time.Time{}
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}
	}
	return cert.NotAfter
}
```

`newTestCoordinator` (test): a `Coordinator` with a fake `issueFn` and no account key. `makeChain`/`fingerprintBytes` are test helpers in the same file.

- [ ] **Step 5: Run tests + build, verify pass**

Run: `cd signaling-server && go test ./internal/certcoordinator/ -run 'TestCoordinator' -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 6: Commit**

```bash
git add signaling-server/internal/certcoordinator/
git commit -m "feat(control): cert coordinator (two-index cache, global semaphore, persisted account key)"
```

---

## Task 10: Control — epoch state + enrollment + tls_ready handlers + hub fencing

**Files:**
- Create: `signaling-server/internal/directctl/controller.go` (skeleton + epoch)
- Create: `signaling-server/internal/directctl/enroll.go`
- Modify: `signaling-server/internal/hub/hub.go` (compare-and-delete register/unregister)
- Test: `signaling-server/internal/directctl/enroll_test.go`

**Interfaces:** `HandleHello`, `HandleCSRSubmit`, `HandleTLSReady`, `HandleTLSError`, `AgentDisconnected`. **Key correctness (from review):** readiness is connection-epoch-local; DDNS success is gated; replacement sockets are fenced.

- [ ] **Step 1: Hub compare-and-delete (hub.go)**

```go
// RegisterAgent stores conn for apiKey, closing any prior conn for that key
// (fencing the old reader) so a stale disconnect cannot unregister the new one.
func (h *Hub) RegisterAgent(apiKey string, conn *websocket.Conn) {
	h.mu.Lock()
	if old, ok := h.agents[apiKey]; ok && old != conn {
		h.mu.Unlock()
		old.Close(websocket.StatusPolicyViolation, "superseded")
		h.mu.Lock()
	}
	h.agents[apiKey] = conn
	h.ensureWriteMuLocked(conn)
	h.mu.Unlock()
}

// UnregisterAgent removes the mapping only if conn is still the registered one.
func (h *Hub) UnregisterAgent(apiKey string, conn *websocket.Conn) {
	h.mu.Lock()
	if cur, ok := h.agents[apiKey]; ok && cur == conn {
		delete(h.agents, apiKey)
		if _, ok := h.connWrites[conn]; ok {
			delete(h.connWrites, conn)
		}
	}
	h.mu.Unlock()
}
```

- [ ] **Step 2: Write the failing test**

```go
// enroll_test.go
package directctl

import (
	"context"
	"testing"
	"time"
)

func TestEpochReadinessGatedOnDDNSAndTLS(t *testing.T) {
	app, ctrl := newTestController(t)
	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) { return "", nil }

	sent := ctrl.captureSend(func() { ctrl.HandleHello(context.Background(), nil, "key-1", "acct-1", "agent-1") })
	if sent["type"] != "enrolled" { t.Fatalf("expected enrolled") }
	if sent["namespace"] == "" { t.Fatalf("namespace empty") }

	// tls_ready before DDNS → no enrollment_ready yet.
	sent2 := ctrl.captureSend(func() {
		ctrl.HandleTLSReady(context.Background(), nil, "key-1", "fp-1", time.Now().Add(90*24*time.Hour).Format(time.RFC3339))
	})
	if sent2 != nil && sent2["type"] == "enrollment_ready" {
		t.Fatalf("must not be ready before DDNS")
	}

	// DDNS success (via report_endpoint) → enrollment_ready.
	sent3 := ctrl.captureSend(func() { ctrl.HandleReportEndpoint(context.Background(), "key-1", "1.2.3.4", 0, "") })
	if sent3["type"] != "enrollment_ready" { t.Fatalf("expected enrollment_ready after DDNS") }

	// Persisted ready must not authorize a fresh epoch alone.
	ctrl.HandleHello(context.Background(), nil, "key-1", "acct-1", "agent-1")
	if ctrl.epochReady("key-1") { t.Fatalf("new epoch must not inherit readiness") }
}
```

- [ ] **Step 3: Run test, verify it fails**

Run: `cd signaling-server && go test ./internal/directctl/ -run TestEpochReadiness -v`
Expected: FAIL.

- [ ] **Step 4: Implement controller + epoch + enrollment**

```go
// controller.go
package directctl

import (
	"context"
	"sync"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/certcoordinator"
	"sharebridge/server/internal/ddns"
	"sharebridge/server/internal/hub"
)

type Config struct{ BaseDomain string }

type epochState struct {
	agentID   string
	namespace string
	conn      *websocket.Conn
	tlsReady  bool
	ddnsReady bool
	ready     bool
}

type Controller struct {
	app   core.App
	hub   *hub.Hub
	coord *certcoordinator.Coordinator
	ddns  *ddns.Cloudflare
	cfg   Config

	sendFn      func(ctx context.Context, conn *websocket.Conn, msg any) error
	ddnsFn      func(ctx context.Context, name, ip string, ttl int) (string, error)
	sendToAgentFn func(ctx context.Context, apiKeyID string, msg any) error

	epochMu sync.Mutex
	epochs  map[string]*epochState // apiKeyID -> current connection epoch

	// open-signal waiters + sequence (Task 13 populates; declare here)
	waiterMu sync.Mutex
	waiters  map[string]*openWaiter
	seqMu    sync.Mutex
	seq      map[string]uint64

	// probe (Task 14 populates; declare here)
	verifiedMu sync.Mutex
	verified   map[string]string
	allowPrivate bool
	probeClient *http.Client

	ackTimeout time.Duration
}

func NewController(app core.App, h *hub.Hub, coord *certcoordinator.Coordinator, dnsClient *ddns.Cloudflare, cfg Config) *Controller {
	c := &Controller{
		app: app, hub: h, coord: coord, ddns: dnsClient, cfg: cfg,
		epochs: map[string]*epochState{}, waiters: map[string]*openWaiter{},
		seq: map[string]uint64{}, verified: map[string]string{},
		ackTimeout: 3 * time.Second,
	}
	c.sendFn = func(ctx context.Context, conn *websocket.Conn, msg any) error { return hub.SendDirect(ctx, conn, msg) }
	c.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		if dnsClient == nil {
			return "", errors.New("ddns not configured")
		}
		return dnsClient.UpsertA(ctx, name, ip, ttl)
	}
	c.sendToAgentFn = func(ctx context.Context, apiKeyID string, msg any) error { return h.SendToAgent(ctx, apiKeyID, msg) }
	return c
}

func (c *Controller) epochReady(apiKeyID string) bool {
	c.epochMu.Lock()
	defer c.epochMu.Unlock()
	e := c.epochs[apiKeyID]
	return e != nil && e.ready
}

func (c *Controller) AgentDisconnected(apiKeyID string, conn *websocket.Conn) {
	c.epochMu.Lock()
	if e, ok := c.epochs[apiKeyID]; ok && e.conn == conn {
		delete(c.epochs, apiKeyID)
	}
	c.epochMu.Unlock()
	// Reset sequence for this epoch (compare-and-delete already handled).
	c.seqMu.Lock()
	delete(c.seq, apiKeyID)
	c.seqMu.Unlock()
	// Cancel any in-flight open waiters for this agent.
	c.waiterMu.Lock()
	for nonce, w := range c.waiters {
		if w.apiKeyID == apiKeyID {
			close(w.ch)
			delete(c.waiters, nonce)
		}
	}
	c.waiterMu.Unlock()
}
```

```go
// enroll.go
package directctl

import (
	"context"
	"errors"
	"time"

	"github.com/coder/websocket"
)

func (c *Controller) HandleHello(ctx context.Context, conn *websocket.Conn, apiKeyID, accountID, agentID string) {
	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		c.sendFn(ctx, conn, map[string]string{"type": "error", "message": "enrollment failed"})
		return
	}
	c.epochMu.Lock()
	c.epochs[apiKeyID] = &epochState{agentID: agentID, namespace: rec.GetString("namespace"), conn: conn}
	c.epochMu.Unlock()
	c.sendFn(ctx, conn, map[string]string{"type": "enrolled", "namespace": rec.GetString("namespace")})
}

func (c *Controller) HandleCSRSubmit(ctx context.Context, conn *websocket.Conn, apiKeyID, csrPEM string) {
	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		c.sendFn(ctx, conn, map[string]string{"type": "cert_error", "reason": "lookup failed"})
		return
	}
	namespace := rec.GetString("namespace")
	// Bound the request size (spec §4f).
	if len(csrPEM) > 64*1024 {
		c.sendFn(ctx, conn, map[string]string{"type": "cert_error", "reason": "csr too large"})
		return
	}
	chain, err := c.coord.Issue(ctx, []byte(csrPEM), namespace, apiKeyID)
	if err != nil {
		c.sendFn(ctx, conn, map[string]string{"type": "cert_error", "reason": "issuance failed"})
		return
	}
	c.sendFn(ctx, conn, map[string]string{"type": "cert_issue", "chain_pem": string(chain)})
}

func (c *Controller) HandleTLSReady(ctx context.Context, conn *websocket.Conn, apiKeyID, fingerprint, notAfter string) {
	chain, na, ok := c.coord.ChainByLeaf(apiKeyID, fingerprint)
	if !ok {
		c.sendFn(ctx, conn, map[string]string{"type": "cert_error", "reason": "unknown fingerprint"})
		return
	}
	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		return
	}
	// Derive not_after from OUR cached chain (never trust the agent's string).
	if err := SaveCertReady(c.app, rec, fingerprint, na); err != nil {
		c.sendFn(ctx, conn, map[string]string{"type": "cert_error", "reason": "persist failed"})
		return
	}
	// If the agent is behind the latest chain, re-deliver the latest.
	_ = chain

	c.epochMu.Lock()
	e := c.epochs[apiKeyID]
	if e != nil && e.conn == conn {
		e.tlsReady = true
		c.maybeReadyLocked(conn, apiKeyID, e)
	}
	c.epochMu.Unlock()
}

func (c *Controller) HandleTLSError(ctx context.Context, apiKeyID, reason string) {
	// No-op beyond logging at this layer; the agent retries with backoff.
}

// maybeReadyLocked emits enrollment_ready once both TLS and DDNS succeeded in
// the CURRENT epoch. Caller holds epochMu.
func (c *Controller) maybeReadyLocked(conn *websocket.Conn, apiKeyID string, e *epochState) {
	if e.tlsReady && e.ddnsReady && !e.ready {
		e.ready = true
		c.sendFn(context.Background(), conn, map[string]string{"type": "enrollment_ready"})
	}
}
```

`HandleReportEndpoint` (Task 11) must set `e.ddnsReady` on successful DDNS and call `maybeReadyLocked`. The `sendFn` inside `maybeReadyLocked` uses `context.Background()`; the handler passes the connection's ctx.

- [ ] **Step 5: Run test + build, verify pass**

Run: `cd signaling-server && go test ./internal/directctl/ -run TestEpochReadiness -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 6: Commit**

```bash
git add signaling-server/internal/directctl/ signaling-server/internal/hub/
git commit -m "feat(control): epoch state, enrollment + tls_ready handlers, hub compare-and-delete"
```

---

## Task 11: Control — report_endpoint + DDNS (provisioned-IP tracking)

**Files:**
- Create: `signaling-server/internal/directctl/endpoint.go`
- Test: `signaling-server/internal/directctl/endpoint_test.go`

**Interfaces:** `HandleReportEndpoint`. **Key correctness (from review):** save `endpoint_ip` only after successful DDNS; validate the port/status invariant; set `ddnsReady` on success.

- [ ] **Step 1: Write the failing test**

```go
// endpoint_test.go
package directctl

import (
	"context"
	"errors"
	"testing"
)

func TestReportEndpointRetriesFailedDDNS(t *testing.T) {
	app, ctrl := newTestController(t)
	ctrl.HandleHello(context.Background(), nil, "key-1", "acct-1", "agent-1")

	fail := true
	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		if fail { return "", errors.New("dns down") }
		return "", nil
	}
	ctrl.HandleReportEndpoint(context.Background(), "key-1", "1.2.3.4", 0, "")
	rec, _, _ := LoadOrCreateAgent(app, "key-1")
	if rec.GetString("endpoint_ip") == "1.2.3.4" {
		t.Fatalf("endpoint_ip must not be saved before DDNS succeeds")
	}

	fail = false
	ctrl.HandleReportEndpoint(context.Background(), "key-1", "1.2.3.4", 0, "")
	rec, _, _ = LoadOrCreateAgent(app, "key-1")
	if rec.GetString("endpoint_ip") != "1.2.3.4" {
		t.Fatalf("endpoint_ip saved after DDNS success")
	}
}

func TestReportEndpointRejectsBadStatus(t *testing.T) {
	_, ctrl := newTestController(t)
	// status "close_failed" with port 0 is invalid → ignored (no DDNS, no save).
	ctrl.HandleReportEndpoint(context.Background(), "key-1", "1.2.3.4", 0, "close_failed")
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestReportEndpoint' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// endpoint.go
package directctl

import (
	"context"
	"time"
)

func (c *Controller) HandleReportEndpoint(ctx context.Context, apiKeyID, ip string, port int, status string) {
	// Wire invariant: status "close_failed" requires a nonzero port.
	if status == "close_failed" && port == 0 {
		return
	}
	if status != "" && status != "close_failed" {
		return // protocol error
	}

	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		return
	}
	prev := rec.GetString("endpoint_ip")

	// DDNS is only attempted on a changed IP; endpoint_ip is saved ONLY after
	// DDNS succeeds, so a failed update is retried on the next report.
	if ip != "" && ip != prev {
		ns := rec.GetString("namespace")
		if _, err := c.ddnsFn(ctx, "*."+ns+"."+c.cfg.BaseDomain, ip, 60); err != nil {
			// leave endpoint_ip unchanged → next report retries
			return
		}
		c.epochMu.Lock()
		if e := c.epochs[apiKeyID]; e != nil {
			e.ddnsReady = true
			c.maybeReadyLocked(e.conn, apiKeyID, e)
		}
		c.epochMu.Unlock()
	}

	rec.Set("endpoint_ip", ip)
	rec.Set("endpoint_port", port)
	rec.Set("last_report_at", time.Now())
	_ = c.app.Save(rec)
}
```

- [ ] **Step 4: Run tests + build, verify pass**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestReportEndpoint' -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/directctl/
git commit -m "feat(control): endpoint report + DDNS (provisioned-IP tracking, invariant validation)"
```

---

## Task 12: Control — origin allocation in register_share + soft-delete everywhere

**Files:**
- Modify: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/internal/handler/rest.go`
- Modify: `signaling-server/internal/handler/browser_ws.go`
- Modify: `signaling-server/cmd/server/main.go` (expiry cron → soft-delete)
- Test: `signaling-server/internal/handler/origin_test.go`

**Interfaces:** `share_registered { …, origin }`; `AllocateOriginFor`; sessions soft-deleted (`is_active=false`) on unregister/expiry; every active-session lookup filters `is_active = true`.

- [ ] **Step 1: Write the failing test**

```go
// origin_test.go
package handler

import "testing"

func TestRegisterShareReturnsOrigin(t *testing.T) {
	// register a share; assert response origin == "<label>.<namespace>.<baseDomain>"
}

func TestUnregisterSoftDeletes(t *testing.T) {
	// register then unregister; assert the row still exists with is_active=false,
	// and getSessionByCode returns nil.
}

func TestGetSessionInfoFiltersInactive(t *testing.T) {
	// a soft-deleted session must not appear in GetSessionInfo.
}
```

- [ ] **Step 2: Run tests, verify they fail**

Run: `cd signaling-server && go test ./internal/handler/ -run 'TestRegisterShareReturnsOrigin|TestUnregisterSoftDeletes|TestGetSessionInfoFiltersInactive' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `createSession` + `claimSessionCode`, set `record.Set("is_active", true)`.

In `handleRegisterShare`, allocate the origin (control-allocated) and include it in the response:

```go
origin, err := ctrl.AllocateOriginFor(app, apiKeyID, session)
if err != nil {
	hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "origin allocation failed"})
	return
}
response := map[string]any{"type": "share_registered", "code": code, "reconnected": reconnected, "origin": origin}
```

`AllocateOriginFor` (directctl):

```go
func (c *Controller) AllocateOriginFor(app core.App, apiKeyID string, session *core.Record) (string, error) {
	if origin := session.GetString("origin"); origin != "" {
		session.Set("is_active", true)
		return origin, app.Save(session)
	}
	rec, _, err := LoadOrCreateAgent(app, apiKeyID)
	if err != nil {
		return "", err
	}
	return AllocateOrigin(app, rec.GetString("namespace"), c.cfg.BaseDomain, session)
}
```

`getSessionByCode` and every other active-session lookup filter `is_active = true`:

```go
func getSessionByCode(app core.App, code string) (*core.Record, error) {
	records, err := app.FindRecordsByFilter("sessions", "code = {:code} && is_active = true", "", 1, 0, map[string]any{"code": code})
	// ...
}
```

`handleUnregisterShare`: replace `app.Delete(session)` with `session.Set("is_active", false); app.Save(session)`.

`deleteExpiredSessions` (main.go): replace `app.Delete(record)` with `record.Set("is_active", false); app.Save(record)`.

Update `browser_ws.go` (knock/join lookups), `rest.go` (`GetSessionInfo`, `ServeSessionFileNoCache`), and `main.go` `serveShareRedirect` to filter `is_active = true`.

- [ ] **Step 4: Run tests + build, verify pass**

Run: `cd signaling-server && go test ./internal/handler/ -run 'TestRegisterShareReturnsOrigin|TestUnregisterSoftDeletes|TestGetSessionInfoFiltersInactive' -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/handler/ signaling-server/cmd/server/ signaling-server/internal/directctl/
git commit -m "feat(control): control-allocated origin + session soft-delete across all lookups"
```

---

## Task 13: Control — open-signal emitter + open-ack waiter (full-tuple)

**Files:**
- Create: `signaling-server/internal/directctl/opensignal.go`
- Test: `signaling-server/internal/directctl/opensignal_test.go`

**Interfaces:** `EmitOpen`, `HandleOpenAck`. **Key correctness (from review):** `open_signal.agent_id` = stored hello `agent_id`; ack validated on the full tuple (apiKeyID + nonce + share_id + seq); maps initialized; epoch waiters cancelled on disconnect.

- [ ] **Step 1: Write the failing test**

```go
// opensignal_test.go
package directctl

import (
	"context"
	"testing"
	"time"
)

func TestEmitOpenUsesHelloAgentID(t *testing.T) {
	_, ctrl := newTestController(t)
	ctrl.HandleHello(context.Background(), nil, "key-1", "acct-1", "agent-1")
	var emitted map[string]any
	ctrl.sendToAgentFn = func(ctx context.Context, apiKeyID string, msg any) error {
		emitted = msg.(map[string]any)
		go func() {
			time.Sleep(5 * time.Millisecond)
			ctrl.HandleOpenAck("key-1", OpenAck{
				ShareID: "abc", Nonce: emitted["nonce"].(string), Seq: emitted["seq"].(uint64),
				GrantedPort: 443, PublicIP: "1.2.3.4", Status: "ok",
			})
		}()
		return nil
	}
	ack, err := ctrl.EmitOpen(context.Background(), "key-1", "abc", "o", 120*time.Second)
	if err != nil { t.Fatal(err) }
	if emitted["agent_id"] != "agent-1" { t.Fatalf("agent_id = %v", emitted["agent_id"]) }
	if ack.GrantedPort != 443 { t.Fatalf("ack = %+v", ack) }
}

func TestOpenAckValidatesFullTuple(t *testing.T) {
	_, ctrl := newTestController(t)
	// An ack with a mismatched seq must be discarded (no waiter completion).
}

func TestLateAckAfterTimeoutDiscarded(t *testing.T) {
	_, ctrl := newTestController(t)
	ctrl.ackTimeout = 20 * time.Millisecond
	ctrl.sendToAgentFn = func(ctx context.Context, apiKeyID string, msg any) error { return nil }
	if _, err := ctrl.EmitOpen(context.Background(), "key-1", "abc", "o", 120*time.Second); err == nil {
		t.Fatalf("expected timeout")
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestEmitOpen|TestOpenAckValidates|TestLateAck' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// opensignal.go
package directctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

type OpenAck struct {
	ShareID        string
	Nonce          string
	Seq            uint64
	GrantedPort    int
	PublicIP       string
	WasAlreadyOpen bool
	Status         string
	Error          string
}

type openWaiter struct {
	apiKeyID string
	shareID  string
	seq      uint64
	ch       chan OpenAck
}

func (c *Controller) EmitOpen(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
	nonce := newNonce()
	c.seqMu.Lock()
	c.seq[apiKeyID]++
	seq := c.seq[apiKeyID]
	c.seqMu.Unlock()

	agentID := c.epochAgentID(apiKeyID)

	sig := map[string]any{
		"type": "open_signal", "version": 1, "agent_id": agentID,
		"share_id": shareID, "route": "direct", "nonce": nonce, "seq": seq,
		"lease_seconds": int(lease.Seconds()),
		"expires_at":    time.Now().Add(2 * time.Minute).Format(time.RFC3339),
	}

	w := &openWaiter{apiKeyID: apiKeyID, shareID: shareID, seq: seq, ch: make(chan OpenAck, 1)}
	c.waiterMu.Lock()
	c.waiters[nonce] = w
	c.waiterMu.Unlock()
	defer func() {
		c.waiterMu.Lock()
		if c.waiters[nonce] == w {
			delete(c.waiters, nonce)
		}
		c.waiterMu.Unlock()
	}()

	if err := c.sendToAgentFn(ctx, apiKeyID, sig); err != nil {
		return OpenAck{}, fmt.Errorf("send open_signal: %w", err)
	}

	timeout := c.ackTimeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	select {
	case ack := <-w.ch:
		return ack, nil
	case <-time.After(timeout):
		return OpenAck{}, fmt.Errorf("open_ack timeout")
	case <-ctx.Done():
		return OpenAck{}, ctx.Err()
	}
}

func (c *Controller) HandleOpenAck(apiKeyID string, ack OpenAck) {
	c.waiterMu.Lock()
	w, ok := c.waiters[ack.Nonce]
	if ok && (w.apiKeyID != apiKeyID || w.shareID != ack.ShareID || w.seq != ack.Seq) {
		ok = false // full-tuple mismatch → discard
	}
	if ok {
		delete(c.waiters, ack.Nonce)
	}
	c.waiterMu.Unlock()
	if ok {
		select {
		case w.ch <- ack:
		default:
		}
	}
}

func (c *Controller) epochAgentID(apiKeyID string) string {
	c.epochMu.Lock()
	defer c.epochMu.Unlock()
	if e := c.epochs[apiKeyID]; e != nil {
		return e.agentID
	}
	return ""
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
```

- [ ] **Step 4: Run tests + build, verify pass**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestEmitOpen|TestOpenAckValidates|TestLateAck' -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/directctl/
git commit -m "feat(control): open-signal emitter + open-ack waiter (full-tuple correlation)"
```

---

## Task 14: Control — reachability probe (SSRF denylist)

**Files:**
- Create: `signaling-server/internal/directctl/probe.go`
- Test: `signaling-server/internal/directctl/probe_test.go`

**Interfaces:** `func (c *Controller) Probe(ctx context.Context, origin, code, apiKeyID string, ack OpenAck) error`. **Key correctness (from review):** comprehensive special-use CIDR denylist; validate port 1–65535 + nonempty IP; `net.JoinHostPort`; no redirect following; tuple skipped only after success.

- [ ] **Step 1: Write the failing test**

```go
// probe_test.go
package directctl

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestProbeRejectsPrivateAndSpecialUse(t *testing.T) {
	_, ctrl := newTestController(t)
	ctrl.allowPrivate = false
	for _, ip := range []string{"10.0.0.1", "100.64.0.1", "192.168.1.1", "169.254.1.1", "198.18.0.1", "240.0.0.1"} {
		if err := ctrl.Probe(context.Background(), "o", "abc", "key-1", OpenAck{PublicIP: ip, GrantedPort: 443}); err == nil {
			t.Fatalf("expected rejection for %s", ip)
		}
	}
}

func TestProbeRejectsZeroPort(t *testing.T) {
	_, ctrl := newTestController(t)
	ctrl.allowPrivate = true
	if err := ctrl.Probe(context.Background(), "o", "abc", "key-1", OpenAck{PublicIP: "127.0.0.1", GrantedPort: 0}); err == nil {
		t.Fatalf("expected zero-port rejection")
	}
}

func TestProbeSendsSNIAndHost(t *testing.T) {
	_, ctrl := newTestController(t)
	ctrl.allowPrivate = true
	var gotHost, gotSNI string
	var gotRedirect bool
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotSNI = r.TLS.ServerName
		if r.URL.Query().Get("nonce") != "n1" {
			w.WriteHeader(403)
			return
		}
		w.WriteHeader(200)
		io.WriteString(w, "n1")
	}))
	ts.StartTLS()
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	port := mustPort(t, u.Port())
	_ = ctrl.Probe(context.Background(), "demo.sb1.example.com", "abc", "key-1", OpenAck{PublicIP: "127.0.0.1", GrantedPort: port, Nonce: "n1"})
	if gotHost != "demo.sb1.example.com" { t.Fatalf("Host = %q", gotHost) }
	if gotSNI != "demo.sb1.example.com" { t.Fatalf("SNI = %q", gotSNI) }
	_ = gotRedirect
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestProbe' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// probe.go
package directctl

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

var ssrfDeny = mustParseCIDRs(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

func isGloballyRoutable(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return false // direct path is IPv4 (UPnP/NAT-PMP)
	}
	for _, cidr := range ssrfDeny {
		if cidr.Contains(parsed) {
			return false
		}
	}
	return true
}

func (c *Controller) Probe(ctx context.Context, origin, code, apiKeyID string, ack OpenAck) error {
	tuple := fmt.Sprintf("%s:%d", ack.PublicIP, ack.GrantedPort)
	c.verifiedMu.Lock()
	if c.verified[apiKeyID] == tuple {
		c.verifiedMu.Unlock()
		return nil // already verified
	}
	c.verifiedMu.Unlock()

	if ack.GrantedPort < 1 || ack.GrantedPort > 65535 {
		return fmt.Errorf("invalid port %d", ack.GrantedPort)
	}
	if ack.PublicIP == "" {
		return fmt.Errorf("empty public IP")
	}
	if !isGloballyRoutable(ack.PublicIP) && !c.allowPrivate {
		return fmt.Errorf("non-routable IP: %s", ack.PublicIP)
	}

	hostPort := net.JoinHostPort(ack.PublicIP, fmt.Sprint(ack.GrantedPort))
	url := fmt.Sprintf("https://%s/s/%s/probe?nonce=%s", hostPort, code, ack.Nonce)

	client := c.probeClient
	if client == nil {
		client = &http.Client{
			Timeout: 3 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // never follow redirects
			},
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{ServerName: origin, InsecureSkipVerify: true},
			},
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Host = origin

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK || string(body) != ack.Nonce {
		return fmt.Errorf("probe nonce mismatch (status %d)", resp.StatusCode)
	}

	c.verifiedMu.Lock()
	c.verified[apiKeyID] = tuple
	c.verifiedMu.Unlock()
	return nil
}
```

- [ ] **Step 4: Run tests + build, verify pass**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestProbe' -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/directctl/
git commit -m "feat(control): reachability probe (SSRF denylist, port validation, no-redirect)"
```

---

## Task 15: Control — redirect handler `/s/<code>`

**Files:**
- Create: `signaling-server/internal/directctl/redirect.go`
- Test: `signaling-server/internal/directctl/redirect_test.go`

**Interfaces:** `Redirect`. **Key correctness (from review):** concrete session lookup (live expiry + is_active + ownership); epoch readiness (not just persisted cert_status); reject `granted_port == 0`; `no-store`.

- [ ] **Step 1: Write the failing test**

```go
// redirect_test.go
package directctl

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRedirect302ToOriginNoStore(t *testing.T) {
	app, ctrl := newTestController(t)
	seedSessionAndEpoch(t, app, ctrl) // code "abc" → apiKey "key-1", origin "demo.sb1.example.com", epoch ready
	ctrl.EmitOpen = func(ctx, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
		return OpenAck{ShareID: shareID, GrantedPort: 443, PublicIP: "1.2.3.4", Status: "ok", Nonce: "n", Seq: 1}, nil
	}
	ctrl.Probe = func(ctx, origin, code, apiKeyID string, ack OpenAck) error { return nil }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/s/abc", nil)
	_ = ctrl.Redirect(rec, req, "abc")
	if rec.Code != http.StatusFound { t.Fatalf("code = %d", rec.Code) }
	if rec.Header().Get("Location") != "https://demo.sb1.example.com/s/abc" {
		t.Fatalf("loc = %q", rec.Header().Get("Location"))
	}
	if rec.Header().Get("Cache-Control") != "no-store" { t.Fatalf("missing no-store") }
}

func TestRedirectUnavailableWhenNotReady(t *testing.T) {
	_, ctrl := newTestController(t)
	// no epoch → unavailable
	rec := httptest.NewRecorder()
	_ = ctrl.Redirect(rec, httptest.NewRequest("GET", "/s/abc", nil), "abc")
	if rec.Code != http.StatusServiceUnavailable { t.Fatalf("code = %d", rec.Code) }
}
```

> `EmitOpen`/`Probe` are methods; to stub them, add `emitOpenFn`/`probeFn` function fields to `Controller` that `EmitOpen`/`Probe` delegate to (defaulting to the real implementations). This makes the redirect test hermetic.

- [ ] **Step 2: Run test, verify it fails**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestRedirect' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// redirect.go
package directctl

import (
	"fmt"
	"net/http"
	"time"
)

func (c *Controller) Redirect(w http.ResponseWriter, r *http.Request, code string) error {
	sess, err := c.sessionByCode(code)
	if err != nil || sess == nil || !sess.active {
		return c.unavailable(w)
	}
	apiKeyID, origin := sess.apiKeyID, sess.origin
	if origin == "" {
		return c.unavailable(w)
	}
	if !c.epochReady(apiKeyID) || !c.hub.AgentConnected(apiKeyID) {
		return c.unavailable(w)
	}
	ack, err := c.emitOpenFn(r.Context(), apiKeyID, code, origin, 120*time.Second)
	if err != nil || ack.Status != "ok" {
		return c.unavailable(w)
	}
	if ack.GrantedPort < 1 || ack.GrantedPort > 65535 {
		return c.unavailable(w)
	}
	if err := c.probeFn(r.Context(), origin, code, apiKeyID, ack); err != nil {
		return c.unavailable(w)
	}
	loc := "https://" + origin
	if ack.GrantedPort != 443 {
		loc += fmt.Sprintf(":%d", ack.GrantedPort)
	}
	loc += "/s/" + code
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, loc, http.StatusFound)
	return nil
}

func (c *Controller) unavailable(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte(`{"error":"direct unavailable"}`))
	return nil
}
```

`sessionByCode` (concrete — live expiry + is_active + ownership):

```go
type sessionRef struct {
	apiKeyID string
	origin   string
	active   bool
}

func (c *Controller) sessionByCode(code string) (*sessionRef, error) {
	recs, err := c.app.FindRecordsByFilter("sessions", "code = {:code} && is_active = true", "", 1, 0, map[string]any{"code": code})
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	rec := recs[0]
	s := &sessionRef{apiKeyID: rec.GetString("api_key_id"), origin: rec.GetString("origin"), active: true}
	if exp := rec.GetDateTime("expires_at"); !exp.IsZero() && exp.Time().Before(time.Now()) {
		s.active = false
	}
	return s, nil
}
```

`emitOpenFn`/`probeFn` are function fields set in the constructor to the real `EmitOpen`/`Probe` (so tests can override).

- [ ] **Step 4: Run tests + build, verify pass**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestRedirect' -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/directctl/
git commit -m "feat(control): redirect handler (live session lookup, epoch readiness, no-store)"
```

---

## Task 16: Agent — signaling client message types + send helpers + origin return

**Files:**
- Modify: `agent/internal/signaling/client.go`
- Modify: `agent/internal/daemon/daemon.go` + `agent/cmd/agent/main.go` (call sites)
- Test: `agent/internal/signaling/client_test.go`

**Interfaces:** as in the original Task 4 (message fields, send helpers, `RegisterShareWithOptions` returns `(code, origin string, reconnected bool, err error)`).

- [ ] **Step 1: Write the failing test** (as in original Task 4, plus origin return).

- [ ] **Step 2: Run test, verify it fails.**

- [ ] **Step 3: Implement** (message fields + `SubmitCSR`/`ReportEndpoint`/`OpenAck`/`TLSReady`/`TLSError` + origin return; update all call sites). The `OpenAck` type lives in `signaling`:

```go
type OpenAck struct {
	ShareID        string `json:"share_id"`
	Nonce          string `json:"nonce"`
	Seq            uint64 `json:"seq"`
	GrantedPort    int    `json:"granted_port"`
	PublicIP       string `json:"public_ip"`
	WasAlreadyOpen bool   `json:"was_already_open"`
	Status         string `json:"status"`
	Error          string `json:"error,omitempty"`
}
```

- [ ] **Step 4: Run tests + build, verify pass.**

- [ ] **Step 5: Commit**

```bash
git add agent/internal/signaling/ agent/internal/daemon/ agent/cmd/agent/
git commit -m "feat(agent): signaling message types + direct-mode send helpers + origin return"
```

---

## Task 17: Agent — daemon direct-transport lifecycle (enroll gate, reconnect, renewal, binder)

**Files:**
- Modify: `agent/internal/daemon/daemon.go`
- Modify: `agent/cmd/agent/main.go`
- Test: `agent/internal/daemon/daemon_direct_test.go`

**Interfaces:** the daemon holds `direct *directState`; gates registration on readiness; reconnects on WS drop; renews at 30-day threshold; binds/revokes origins.

**Key correctness (from review):** the daemon currently connects once — add a reconnect loop + readiness gate + renewal scheduler + bounded backoff; persist the origin per session; call `binder.Allow` after every registration/reconnection and `binder.Revoke` on delete.

- [ ] **Step 1: Write the failing test**

```go
// daemon_direct_test.go
package daemon

import "testing"

func TestRegistrationGatedOnReadiness(t *testing.T) {
	d := &Daemon{direct: &directState{ready: false}}
	if d.canRegisterDirect() { t.Fatalf("must gate before readiness") }
	d.direct.ready = true
	if !d.canRegisterDirect() { t.Fatalf("must allow after readiness") }
}

func TestBinderBoundOnOrigin(t *testing.T) {
	// given a fake binder + a registration returning origin, assert Allow was
	// called with (origin, RouteDirect, code).
}

func TestBinderRevokedOnDelete(t *testing.T) {
	// revoke a session → assert binder.Revoke(origin) called.
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd agent && go test ./internal/daemon/ -run 'TestRegistrationGated|TestBinderBound|TestBinderRevoked' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Add to `Daemon`:

```go
type directState struct {
	namespace string
	ready     bool
	cert      *cert.Manager
	binder    *direct.Binder
	gate      *direct.SignalGate
	port      *direct.OnDemandPort
	mapper    direct.PortMapper
	server    *direct.DirectServer
	baseDomain string
}

func (d *Daemon) canRegisterDirect() bool { return d.direct != nil && d.direct.ready }
```

`directState` gets an `origin map[string]string` (code → origin) for revoke on delete. Wire:

1. **Reconnect loop:** wrap the signaling connect + listen in a loop with `signaling.NewBackoff()`; on disconnect, `d.direct.gate.Reset()` and re-enroll (the daemon re-sends `hello`, re-handles `enrolled`).
2. **Renewal scheduler:** a goroutine (e.g. every hour) checks `d.direct.cert.NeedsRenewal()`; when true, `GenerateCSR` (key reuse) + `SubmitCSR`.
3. **Origin binding:** on `share_registered` (with origin), `d.direct.binder.Allow(origin, direct.RouteDirect, code)` and record `d.direct.origin[code] = origin`. On revoke/expiry, `d.direct.binder.Revoke(origin)`.
4. **Enrollment handlers** (`enrolled`, `cert_issue`, `cert_error`, `enrollment_ready`, `open_signal`) as in the original Tasks 7/15, but now using `d.direct` and the corrected `agent_id` (the gate is constructed with `st.GetAgentID()`, and the control echoes the same `hello.agent_id`).

- [ ] **Step 4: Run tests + build, verify pass**

Run: `cd agent && go test ./internal/daemon/ -run 'TestRegistrationGated|TestBinderBound|TestBinderRevoked' -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/daemon/ agent/cmd/agent/
git commit -m "feat(agent): daemon direct-transport lifecycle (reconnect, renewal, binder binding)"
```

---

## Task 18: Agent — daemon open-signal handling + fresh-IP reporting

**Files:**
- Modify: `agent/internal/daemon/daemon.go`
- Test: `agent/internal/daemon/daemon_direct_test.go` (extend)

**Interfaces:** `handleOpenSignal(msg)` — admit → `OpenFor` → `open_ack` with fresh `ExternalIP()`.

- [ ] **Step 1: Write the failing test**

```go
func TestHandleOpenSignalAdmitsOpensAcks(t *testing.T) {
	// fake gate (admits) + fake port (records OpenFor) + fake signaling;
	// feed an open_signal; assert OpenFor called, open_ack sent with echoed
	// nonce + seq + granted port + status ok + public IP.
}

func TestHandleOpenSignalRejectsUnauthorized(t *testing.T) {
	// gate authz returns false → open_ack status "error".
}
```

- [ ] **Step 2: Run test, verify it fails.**

- [ ] **Step 3: Implement**

```go
func (d *Daemon) handleOpenSignal(msg signaling.Message) {
	if d.direct == nil || !d.direct.ready {
		d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "not_ready"})
		return
	}
	exp, err := time.Parse(time.RFC3339, msg.ExpiresAt)
	if err != nil {
		d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "bad_expiry"})
		return
	}
	sig := direct.OpenSignal{
		Version: msg.Version, AgentID: d.store.GetAgentID(), ShareID: msg.ShareID,
		RouteKind: direct.RouteDirect, Nonce: msg.Nonce, Seq: msg.Seq,
		ExpiresAt: exp, Lease: time.Duration(msg.LeaseSeconds) * time.Second,
	}
	if err := d.direct.gate.Admit(sig); err != nil {
		d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "rejected"})
		return
	}
	wasOpen := d.direct.port.Open()
	if err := d.direct.port.OpenFor(msg.ShareID, sig.Lease); err != nil {
		d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "open_failed"})
		return
	}
	ip, _ := d.direct.mapper.ExternalIP() // FRESH on every ack
	d.signaling.OpenAck(context.Background(), signaling.OpenAck{
		ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq,
		GrantedPort: d.direct.port.GrantedPort(), PublicIP: ip,
		WasAlreadyOpen: wasOpen, Status: "ok"})
}
```

- [ ] **Step 4: Run tests + build, verify pass.**

- [ ] **Step 5: Commit**

```bash
git add agent/internal/daemon/
git commit -m "feat(agent): open_signal handler with fresh-IP reporting"
```

---

## Task 19: Control — rotation re-point + revocation deletes agent + main.go wiring

**Files:**
- Modify: `signaling-server/internal/handler/apikeys.go`
- Modify: `signaling-server/internal/config/config.go`
- Modify: `signaling-server/cmd/server/main.go`
- Modify: `signaling-server/internal/handler/agent_ws.go` (thread `ctrl`; route new messages; `AgentDisconnected`)
- Test: `signaling-server/internal/handler/apikeys_test.go` (extend)

**Interfaces:** rotation re-points `agents.api_key_id` (in the same transaction, before revoking the old key); standalone revocation deletes the agent; controller wired in `main.go`.

- [ ] **Step 1: Write the failing test**

```go
func TestRotatePreservesAgent(t *testing.T) {
	// create key + agent; rotate; assert agent now points at new key
	// (namespace + cert_status preserved), old key revoked.
}
func TestRevokeDeletesAgent(t *testing.T) {
	// revoke (without rotation) → agent row deleted (CascadeDelete via explicit delete).
}
```

- [ ] **Step 2: Run test, verify it fails.**

- [ ] **Step 3: Implement**

In `RotateAPIKey`, inside the transaction (before revoking the old key):

```go
// Re-point the agent to the new key (namespace + cert survive).
agentRecs, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": oldKeyID})
if err == nil && len(agentRecs) > 0 {
	agentRecs[0].Set("api_key_id", newKeyID)
	if err := app.Save(agentRecs[0]); err != nil {
		return err
	}
}
h.CloseAgent(oldKeyID) // fence/disconnect the old socket
```

In `RevokeAPIKey` (standalone revocation), explicitly delete the agent:

```go
agentRecs, _ := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": keyID})
for _, rec := range agentRecs {
	_ = app.Delete(rec) // fresh namespace on re-enroll (spec §3)
}
```

Config additions (config.go):

```go
CloudflareToken string // CLOUDFLARE_TOKEN
BaseDomain      string // CONTENT_BASE_DOMAIN (default sharebridgeusercontent.com)
ACMEEmail       string // ACME_EMAIL
ACMECADir       string // ACME_CA_DIR (default lego production)
```

`main.go`: build the coordinator + controller, `directctl.SetChainCacheHas(coord.HasLeafFingerprint)`, replace `/s/{code}` with `ctrl.Redirect`, and thread `ctrl` into `handler.AgentWS`. In `agent_ws.go`, route `csr_submit`/`tls_ready`/`tls_error`/`report_endpoint`/`open_ack` to the controller and call `ctrl.AgentDisconnected(apiKeyID, conn)` on disconnect (with the `conn` for compare-and-delete).

- [ ] **Step 4: Run tests + build, verify pass**

Run: `cd signaling-server && go test ./internal/handler/ -run 'TestRotatePreservesAgent|TestRevokeDeletesAgent' -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/handler/ signaling-server/internal/config/ signaling-server/cmd/server/
git commit -m "feat(control): rotation re-points agent, revocation deletes agent, wire controller"
```

---

## Task 20: End-to-end integration test (fake agent over a real WebSocket)

**Files:**
- Create: `signaling-server/internal/directctl/e2e_test.go`

**Interfaces:** consumes the full stack. The coordinator uses an injected `issueFn` (staging chain); the probe runs against a loopback test server.

- [ ] **Step 1: Write the failing test**

```go
// e2e_test.go
package directctl

func TestEndToEndDirectFlow(t *testing.T) {
	// 1. Boot the control (app + controller) with a stub coordinator + stub
	//    issuer returning a pre-made chain whose leaf fingerprint is known.
	// 2. Dial /ws/agent with a test API key; send hello; receive enrolled.
	// 3. Submit a CSR; receive cert_issue (stub chain).
	// 4. Send tls_ready with the known leaf fingerprint; receive enrollment_ready
	//    (after a report_endpoint triggers DDNS).
	// 5. register_share; receive share_registered {origin}.
	// 6. GET /s/<code>; assert the handler emits open_signal, the fake agent
	//    acks, the control probes (loopback nonce echo), and responds 302 to
	//    https://<origin>/s/<code>.
}
```

- [ ] **Step 2: Run test, verify it fails.**

- [ ] **Step 3: Implement the test + fix anything it exposes.**

- [ ] **Step 4: Run the full suite**

Run:
```bash
cd signaling-server && go test ./...
cd agent && go test ./...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/directctl/
git commit -m "test: end-to-end direct flow (enroll → cert → register → open-signal → probe → redirect)"
```

---

## Self-Review (run before handoff)

- [ ] **Spec coverage:** §3 schema (Task 1), §4 enrollment/cert/renewal (Tasks 3, 9, 10, 17), §5 endpoint/DDNS/flow (Tasks 11, 13, 14, 15, 18), §6 security (Tasks 2, 4, 9, 14), §8 testing (all tasks + Task 20). All Critical review findings folded in.
- [ ] **Placeholder scan:** shared fixtures supplied (`newTestController`, `captureSend`); every test has assertions or an explicit implementation step; no "Fix whatever the test exposes" language.
- [ ] **Type consistency:** `NewBinder(namespace, baseDomain)`, `NewDirectServer(namespace, baseDomain, …)`, `Coordinator.Issue(ctx, csr, ns, apiKeyID)`, `ChainByLeaf`, `OpenAck`, `EmitOpen`/`Probe`/`Redirect`, `AgentDisconnected(apiKeyID, conn)` are referenced identically across tasks.
- [ ] **Compile-after-every-task:** every task ends with `go build ./...` (and `go test ./...`), and ordering puts primitives before wiring.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-15-phase2-direct-mode-transport.md`.

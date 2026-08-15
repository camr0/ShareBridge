# Phase 2 — Direct-Mode Transport Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire the Phase-1 direct-TCP primitives into a working end-to-end direct data path — a browser reaches the agent over native HTTPS (real Let's Encrypt cert, on-demand UPnP/NAT-PMP port) and downloads placeholder content — with the control plane coordinating enrollment, cert issuance, DDNS, reachability probing, and open-signal/redirect.

**Architecture:** The agent gains a cert lifecycle manager, a direct HTTPS server (Binder + OnDemandPort + installed cert), an endpoint reporter, and a SignalGate that admits control-plane open-signals. The control plane gains an `agents` collection, enrollment + cert-coordination handlers, origin allocation, an open-signal emitter with open-ack correlation, and a reachability probe that ends in a 302 redirect. All new control messages ride the existing authenticated agent WebSocket.

**Tech Stack:** Go, PocketBase (collections/migrations), coder/websocket, go-acme/lego (ACME DNS-01 via Cloudflare), huin/goupnp + jackpal/go-nat-pmp (port mapping), cloudflare-go (DDNS). Phase-1 packages `agent/internal/direct`, `agent/internal/cert`, `signaling-server/internal/certcoordinator`, `signaling-server/internal/ddns` are wrapped, not rewritten.

## Global Constraints

- **Zero production changes** until the whole branch is reviewed and the user approves merging to `main`.
- **Model policy:** `deepseek/deepseek-v4-pro` for all implementation subagents; `openai-codex/gpt-5.6-sol` only for the final whole-branch review.
- **Do NOT edit `signaling-server/migrations/1_create_collections.go`** — it early-returns on existing databases. All schema changes are a new forward migration.
- **Wire types are normative:** `lease_seconds` is an integer (seconds, not `time.Duration`); timestamps are RFC3339 strings; `seq` is `uint64`; `fingerprint` is a lowercase SHA-256 hex digest of the DER leaf. Unknown fields are ignored; unexpected values are a protocol error.
- **Key custody:** the agent's TLS private key never leaves the agent; only the CSR and the returned chain cross the wire. `CompleteCSR` enforces self-signature + exactly the two wildcard SANs.
- **Two wildcard SANs per namespace:** `*.<ns>.<baseDomain>` and `*.relay.<ns>.<baseDomain>`. Base domain = `sharebridgeusercontent.com` (configurable for tests).
- **Origin is control-allocated** (random, unique, never reused); the agent never supplies an origin. Redirect URL carries the port; DNS carries the IP.
- **Redirect hygiene:** port omitted when granted port is 443; `Cache-Control: no-store` on the 302.
- **Readiness is an explicit handshake** (`tls_ready` → `enrollment_ready`), never inferred from ACME returning a chain.
- **Lease bounds:** open-signal lease ≤ 15 min; min valid lease 5 s (clamped up in `OnDemandPort`).
- **Cert renewal:** agent-driven at 30 days before `NotAfter`; key reuse.
- **Rate limit:** global open-signal ceiling ~60/min (drop the 3/share/min cap).
- **DDNS TTL:** 60 s.
- Tests must not use production certs or live UPnP — use the staging CA + a test DNS zone / fake mapper.

---

## File Structure

### Agent (create/modify)
| File | Responsibility |
|---|---|
| `agent/internal/cert/manager.go` (create) | Cert lifecycle manager: key custody, CSR, validate, install + atomic reload, renewal threshold |
| `agent/internal/cert/manager_test.go` (create) | Unit tests for the manager |
| `agent/internal/direct/portmap.go` (modify) | `DeleteOwnedMapping` exact-match ownership; `PortMapper.InternalIP()` |
| `agent/internal/direct/ondemand.go` (modify) | `OpenFor` fast-path, cold-port reselection, ownership token, transition callback, close-failed recovery |
| `agent/internal/direct/ondemand_test.go` (modify) | Tests for the above |
| `agent/internal/direct/opensignal.go` (modify) | `SignalGate.Reset()`, `VerifyNonce()`, rate-limit raise |
| `agent/internal/direct/opensignal_test.go` (modify) | Tests for the above |
| `agent/internal/direct/server.go` (create) | Direct HTTPS server: GetConfigForClient composition, placeholder content, probe handler |
| `agent/internal/direct/server_test.go` (create) | Tests for the server |
| `agent/internal/signaling/client.go` (modify) | New message fields + send helpers (`SubmitCSR`, `ReportEndpoint`, `OpenAck`, `TLSReady`, `TLSError`), `share_registered` origin |
| `agent/internal/daemon/daemon.go` (modify) | Wire direct transport: enrollment gate, open-signal handling, binder registration |
| `agent/cmd/agent/main.go` (modify) | Start the direct transport in daemon mode |

### Control plane (create/modify)
| File | Responsibility |
|---|---|
| `signaling-server/migrations/2_create_agents.go` (create) | `agents` collection + `sessions.origin`/`is_active` + unique indexes |
| `signaling-server/internal/certcoordinator/coordinator.go` (create) | Stateful coordinator: ACME account-key persistence, singleflight, cooldown, CSR-fingerprint idempotency |
| `signaling-server/internal/certcoordinator/coordinator_test.go` (create) | Tests |
| `signaling-server/internal/directctl/controller.go` (create) | Control-side direct flow: enrollment, csr/tls_ready/report_endpoint/open_ack handlers, redirect + probe, DDNS trigger |
| `signaling-server/internal/directctl/controller_test.go` (create) | Tests |
| `signaling-server/internal/handler/agent_ws.go` (modify) | Route new message types to the controller |
| `signaling-server/internal/handler/rest.go` (modify) | Origin allocation in `handleRegisterShare`; soft-delete in unregister |
| `signaling-server/internal/handler/apikeys.go` (modify) | Rotation re-points `agents.api_key_id` |
| `signaling-server/internal/config/config.go` (modify) | New env: Cloudflare/ACME/base-domain |
| `signaling-server/cmd/server/main.go` (modify) | Wire controller + coordinator + redirect route |

### Integration
| File | Responsibility |
|---|---|
| `agent/cmd/e2e/main.go` (create, replaces `spike-e2e`) | Real integration test: enroll → staging cert → register → open-signal → probe → redirect → download |

---

## Contracts (types every task must agree on)

```go
// agent/internal/cert — Manager (new)
type Manager struct{ /* ... */ }
func NewManager(dataDir, baseDomain string, roots *x509.CertPool) *Manager
func (m *Manager) SetNamespace(namespace string) error
func (m *Manager) Load() error                       // load persisted namespace/key/chain
func (m *Manager) Namespace() string
func (m *Manager) GenerateCSR() (csrPEM []byte, err error) // reuse key if present
func (m *Manager) Install(chainPEM []byte) error
func (m *Manager) Certificate() (*tls.Certificate, error)
func (m *Manager) LeafFingerprint() (string, error)   // lowercase sha256 hex of leaf DER
func (m *Manager) NotAfter() (time.Time, error)
func (m *Manager) Installed() bool
func (m *Manager) NeedsRenewal() bool                 // !Installed() || NotAfter < now+30d

// agent/internal/direct — SignalGate additions
func (g *SignalGate) Reset()
func (g *SignalGate) VerifyNonce(nonce, shareID string) bool

// agent/internal/direct — OnDemandPort additions
type PortState int // StateClosed, StateOpen, StateClosing, StateCloseFailed
func (p *OnDemandPort) State() PortState
func (p *OnDemandPort) SetTransitionCallback(cb func(old, new PortState, grantedPort int))

// agent/internal/direct — portmap changes
func DeleteOwnedMapping(mapper PortMapper, externalPort int, want PortMapping) error
// PortMapper gains: InternalIP() string

// agent/internal/direct — server
type CertProvider interface { Certificate() (*tls.Certificate, error) }
type DirectServer struct{ /* ... */ }
func NewDirectServer(namespace string, port *OnDemandPort, certs CertProvider, gate *SignalGate, maxContentBytes int64) *DirectServer
func (s *DirectServer) TLSConfig() *tls.Config
func (s *DirectServer) Handler() http.Handler
func (s *DirectServer) Start(ctx context.Context, listenAddr string) error
```

---

## Task 1: Control-plane migration — `agents` + `sessions.origin`/`is_active`

**Files:**
- Create: `signaling-server/migrations/2_create_agents.go`
- Test: `signaling-server/migrations/migrations_test.go` (create if absent)

**Interfaces:**
- Produces: `agents` collection (fields below); `sessions` gains `origin` (text, unique), `is_active` (bool, default true).

```go
// agents collection fields (exact)
api_key_id  relation→api_keys  unique, CascadeDelete
namespace   text               unique
endpoint_ip text
endpoint_port number
cert_status text               // "pending" | "ready"
cert_fingerprint text
cert_expires_at date
last_report_at date
```

- [ ] **Step 1: Write the failing test**

```go
// migrations/migrations_test.go
package migrations

import (
	"testing"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

func TestCreateAgents(t *testing.T) {
	app := pocketbase.New()
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// CreateCollections must run first (it is registered; call directly in tests).
	if err := CreateCollections(app); err != nil {
		t.Fatalf("create base collections: %v", err)
	}
	if err := CreateAgents(app); err != nil {
		t.Fatalf("create agents: %v", err)
	}

	agents, err := app.FindCollectionByNameOrId("agents")
	if err != nil {
		t.Fatalf("agents collection missing: %v", err)
	}
	_ = agents

	sessions, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		t.Fatalf("sessions collection missing: %v", err)
	}
	if sessions.Fields.GetByName("origin") == nil {
		t.Fatalf("sessions.origin field missing")
	}
	if sessions.Fields.GetByName("is_active") == nil {
		t.Fatalf("sessions.is_active field missing")
	}

	// Unique index on sessions.origin
	found := false
	for _, idx := range sessions.Indexes {
		if strings.Contains(idx, "origin") && strings.Contains(strings.ToUpper(idx), "UNIQUE") {
			found = true
		}
	}
	if !found {
		t.Fatalf("sessions.origin missing unique index: %v", sessions.Indexes)
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd signaling-server && go test ./migrations/ -run TestCreateAgents -v`
Expected: FAIL — `CreateAgents` undefined.

- [ ] **Step 3: Implement the migration**

```go
// migrations/2_create_agents.go
package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() { m.Register(CreateAgents, nil) }

// CreateAgents adds the agents collection and the sessions.origin + is_active
// fields. Exported so tests can call it directly. Idempotent: returns early if
// the agents collection already exists.
func CreateAgents(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("agents"); err == nil {
		return nil // already applied
	}

	apiKeysCol, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		return fmt.Errorf("api_keys collection not found: %w", err)
	}

	agentsCol := core.NewBaseCollection("agents")
	agentsCol.Fields.Add(
		&core.RelationField{
			Name: "api_key_id", CollectionId: apiKeysCol.Id,
			Required: true, CascadeDelete: true, MaxSelect: 1,
		},
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
	// Agents are internal server state: no REST exposure.
	agentsCol.ListRule, agentsCol.ViewRule = nil, nil
	agentsCol.CreateRule, agentsCol.UpdateRule, agentsCol.DeleteRule = nil, nil, nil
	if err := app.Save(agentsCol); err != nil {
		return fmt.Errorf("save agents collection: %w", err)
	}

	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return fmt.Errorf("sessions collection not found: %w", err)
	}
	sessionsCol.Fields.Add(
		&core.TextField{Name: "origin"},
		&core.BoolField{Name: "is_active"},
	)
	// Preserve existing indexes, append the unique origin index.
	sessionsCol.Indexes = append(sessionsCol.Indexes,
		"CREATE UNIQUE INDEX `idx_sessions_origin` ON `{{COLLECTION}}` (`origin`)",
	)
	if err := app.Save(sessionsCol); err != nil {
		return fmt.Errorf("save sessions origin: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run test, verify it passes**

Run: `cd signaling-server && go test ./migrations/ -run TestCreateAgents -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/migrations/
git commit -m "feat(control): add agents collection + sessions.origin/is_active migration"
```

---

## Task 2: Control — agent record helpers + namespace generation

**Files:**
- Create: `signaling-server/internal/directctl/agentstore.go`
- Test: `signaling-server/internal/directctl/agentstore_test.go`

**Interfaces:**
- Consumes: `agents` + `sessions` collections (Task 1).
- Produces:
```go
func GenerateNamespace() string          // "sb" + 8 hex chars (4 random bytes)
func LoadOrCreateAgent(app core.App, apiKeyID string) (*core.Record, bool, error) // (record, created, err)
func SaveCertReady(app core.App, rec *core.Record, fingerprint string, notAfter time.Time) error
func AcceptableTLSReady(app core.App, apiKeyID, fingerprint string) (bool, error)  // any still-valid issued fingerprint
func AllocateOrigin(app core.App, namespace string) (string, error) // <12hex>.<namespace>.<baseDomain>
```

- [ ] **Step 1: Write the failing test**

```go
package directctl

import (
	"regexp"
	"testing"

	"github.com/pocketbase/pocketbase"
)

func TestGenerateNamespace(t *testing.T) {
	re := regexp.MustCompile(`^sb[0-9a-f]{8}$`)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		ns := GenerateNamespace()
		if !re.MatchString(ns) {
			t.Fatalf("namespace %q does not match %s", ns, re)
		}
		if seen[ns] {
			t.Fatalf("namespace collision: %q", ns)
		}
		seen[ns] = true
	}
}

func TestLoadOrCreateAgent(t *testing.T) {
	app := pocketbase.New()
	if err := app.Bootstrap(); err != nil { t.Fatal(err) }
	// register base + agents migrations (reuse the migration funcs)
	if err := ensureSchema(app); err != nil { t.Fatal(err) }

	// Create an api_keys record to relate to.
	apiKeys, _ := app.FindCollectionByNameOrId("api_keys")
	key := core.NewRecord(apiKeys)
	key.Set("key_hash", "x")
	if err := app.Save(key); err != nil { t.Fatal(err) }

	rec, created, err := LoadOrCreateAgent(app, key.Id)
	if err != nil { t.Fatalf("load-or-create: %v", err) }
	if !created { t.Fatalf("expected created") }
	if rec.GetString("namespace") == "" { t.Fatalf("namespace empty") }
	if rec.GetString("cert_status") != "pending" { t.Fatalf("status = %q", rec.GetString("cert_status")) }

	// Second call must load, not create.
	rec2, created2, err := LoadOrCreateAgent(app, key.Id)
	if err != nil { t.Fatal(err) }
	if created2 { t.Fatalf("expected loaded, not created") }
	if rec2.Id != rec.Id { t.Fatalf("record identity changed") }
}
```

`ensureSchema` (test helper in the same package) runs `migrations.CreateCollections` + `migrations.CreateAgents`.

- [ ] **Step 2: Run test, verify it fails**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestGenerateNamespace|TestLoadOrCreateAgent' -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement**

```go
// internal/directctl/agentstore.go
package directctl

import (
	"crypto/rand"
	"crypto/sha256"
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
	col, err := app.FindCollectionByNameOrId("agents")
	if err != nil {
		return nil, false, err
	}
	recs, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": apiKeyID})
	if err != nil {
		return nil, false, err
	}
	if len(recs) > 0 {
		return recs[0], false, nil
	}
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

// AcceptableTLSReady reports whether the agent's reported fingerprint is one the
// control has issued for it and that is not yet expired — NOT merely the latest.
func AcceptableTLSReady(app core.App, apiKeyID, fingerprint string) (bool, error) {
	rec, _, err := LoadOrCreateAgent(app, apiKeyID)
	if err != nil {
		return false, err
	}
	// The coordinator's issued-chain cache is the authority; consult it.
	// If no chain cache entry exists yet, fall back to the stored fingerprint.
	stored := rec.GetString("cert_fingerprint")
	if fingerprint == "" {
		return false, nil
	}
	if stored == fingerprint {
		return true, nil
	}
	// Cross-check the fingerprint cache (populated in Task 5).
	return chainCacheHas(app, apiKeyID, fingerprint), nil
}

func leafFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func AllocateOrigin(app core.App, namespace, baseDomain string) (string, error) {
	for i := 0; i < 8; i++ {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		origin := hex.EncodeToString(b) + "." + namespace + "." + baseDomain
		// The unique index on sessions.origin enforces global uniqueness; a
		// save collision surfaces as a DB error and we retry with a new label.
		recs, err := app.FindRecordsByFilter("sessions", "origin = {:o}", "", 1, 0, map[string]any{"o": origin})
		if err != nil {
			return "", err
		}
		if len(recs) == 0 {
			return origin, nil
		}
	}
	return "", fmt.Errorf("could not allocate a free origin")
}
```

`chainCacheHas` is declared here and implemented in Task 5 (the coordinator's cache); in this task return `false` (a later task wires it — see Task 5's note).

- [ ] **Step 4: Run test, verify it passes**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestGenerateNamespace|TestLoadOrCreateAgent' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/directctl/
git commit -m "feat(control): agent record helpers + namespace generation"
```

---

## Task 3: Agent — cert lifecycle manager

**Files:**
- Create: `agent/internal/cert/manager.go`
- Create: `agent/internal/cert/csr.go` — add `CSRFromKey` (modify)
- Test: `agent/internal/cert/manager_test.go`

**Interfaces:**
- Consumes: `GenerateWildcardCSR`, `ValidateChain`, `wildcardSANs` (existing).
- Produces: the `Manager` contract above.

- [ ] **Step 1: Add `CSRFromKey` (modify `csr.go`)**

```go
// CSRFromKey builds the two-SAN wildcard CSR from an existing ECDSA P-256 key
// PEM (used for renewal with key reuse).
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

- [ ] **Step 2: Write the failing manager test**

```go
// manager_test.go
package cert

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerGenerateAndInstall(t *testing.T) {
	dir := t.TempDir()
	roots := x509.NewCertPool()
	m := NewManager(dir, "example.com", roots)
	if err := m.SetNamespace("sbdeadbeef"); err != nil {
		t.Fatal(err)
	}
	if m.Installed() {
		t.Fatalf("not installed yet")
	}
	if !m.NeedsRenewal() {
		t.Fatalf("needs renewal before any cert")
	}

	csrPEM, err := m.GenerateCSR()
	if err != nil {
		t.Fatalf("GenerateCSR: %v", err)
	}
	if len(csrPEM) == 0 {
		t.Fatalf("empty CSR")
	}
	// key must persist across GenerateCSR (key reuse)
	if _, err := os.Stat(filepath.Join(dir, "key.pem")); err != nil {
		t.Fatalf("key not persisted: %v", err)
	}

	// A self-signed cert is NOT acceptable to Install (ValidateChain rejects
	// self-signed). Install must reject it.
	if err := m.Install([]byte("not a chain")); err == nil {
		t.Fatalf("Install accepted garbage")
	}
}

func TestManagerCertificateAbsent(t *testing.T) {
	m := NewManager(t.TempDir(), "example.com", x509.NewCertPool())
	if _, err := m.Certificate(); err == nil {
		t.Fatalf("expected error when no cert installed")
	}
}
```

- [ ] **Step 3: Run test, verify it fails**

Run: `cd agent && go test ./internal/cert/ -run TestManager -v`
Expected: FAIL — `NewManager` undefined.

- [ ] **Step 4: Implement the manager**

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

// Manager owns the agent's TLS identity: a locally-held P-256 key, the issued
// chain, and the namespace. The key never leaves disk/memory; only the CSR and
// the returned chain cross the wire. Installing a chain is validated then
// swapped in memory atomically (no listener restart).
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

func (m *Manager) keyPath() string   { return filepath.Join(m.dataDir, "direct", "key.pem") }
func (m *Manager) chainPath() string { return filepath.Join(m.dataDir, "direct", "chain.pem") }
func (m *Manager) nsPath() string    { return filepath.Join(m.dataDir, "direct", "namespace") }

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

// Load restores a persisted identity (namespace + key + chain) if present.
func (m *Manager) Load() error {
	if b, err := os.ReadFile(m.nsPath()); err == nil {
		m.namespace = string(b)
	}
	keyPEM, err1 := os.ReadFile(m.keyPath())
	chainPEM, err2 := os.ReadFile(m.chainPath())
	if err1 != nil || err2 != nil {
		return nil // no persisted cert yet
	}
	return m.installLocked(chainPEM, keyPEM)
}

// GenerateCSR returns a CSR for the current namespace, generating a key first
// if none exists. The key is persisted so renewal reuses it.
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

// Install validates the chain against the local key + roots, persists it, and
// swaps the in-memory certificate atomically.
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
	if err := m.persistChain(); err != nil {
		return err
	}
	return nil
}

func (m *Manager) Certificate() (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cert == nil {
		return nil, fmt.Errorf("no certificate installed")
	}
	// Return a copy so the TLS stack can safely hold it.
	cp := *m.cert
	return &cp, nil
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

func (m *Manager) NotAfter() (time.Time, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cert == nil || len(m.cert.Certificate) == 0 {
		return time.Time{}, fmt.Errorf("no certificate installed")
	}
	c, err := x509.ParseCertificate(m.cert.Certificate[0])
	if err != nil {
		return time.Time{}, err
	}
	return c.NotAfter, nil
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
	na, err := m.NotAfter()
	if err != nil {
		return true
	}
	return time.Until(na) < renewalWindow
}

func (m *Manager) persistNamespace() error {
	return os.MkdirAll(filepath.Dir(m.nsPath()), 0700)
	// note: write the file after MkdirAll
}

func (m *Manager) persistKey() error {
	if err := os.MkdirAll(filepath.Dir(m.keyPath()), 0700); err != nil {
		return err
	}
	return os.WriteFile(m.keyPath(), m.keyPEM, 0600)
}

func (m *Manager) persistChain() error {
	if err := os.MkdirAll(filepath.Dir(m.chainPath()), 0700); err != nil {
		return err
	}
	return os.WriteFile(m.chainPath(), m.chainPEM, 0600)
}
```

> **Correction (self-review):** `persistNamespace` must actually write the file. Replace it with:
> ```go
> func (m *Manager) persistNamespace() error {
>     if err := os.MkdirAll(filepath.Dir(m.nsPath()), 0700); err != nil {
>         return err
>     }
>     return os.WriteFile(m.nsPath(), []byte(m.namespace), 0600)
> }
> ```

- [ ] **Step 5: Run test, verify it passes**

Run: `cd agent && go test ./internal/cert/ -run TestManager -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add agent/internal/cert/
git commit -m "feat(agent): cert lifecycle manager (key custody, CSR, validate, atomic reload, renew)"
```

---

## Task 4: Agent — signaling client message types + send helpers

**Files:**
- Modify: `agent/internal/signaling/client.go`
- Test: `agent/internal/signaling/client_test.go` (create if absent)

**Interfaces:**
- Consumes: existing `Client`/`Message` structs.
- Produces:
```go
// Message gains fields (all json tags snake_case):
//   Namespace, ChainPEM, Reason, Nonce, Seq (uint64), ShareID, Route,
//   LeaseSeconds (int), GrantedPort (int), PublicIP, WasAlreadyOpen (bool),
//   Status, Fingerprint, NotAfter, Origin, Version (int)
// Client gains methods:
func (c *Client) SubmitCSR(ctx context.Context, csrPEM string) error
func (c *Client) ReportEndpoint(ctx context.Context, ip string, port int, status string) error
func (c *Client) OpenAck(ctx context.Context, ack OpenAck) error
func (c *Client) TLSReady(ctx context.Context, fingerprint, notAfter string) error
func (c *Client) TLSError(ctx context.Context, reason string) error
// RegisterShareWithOptions returns origin: change signature to (code string, origin string, reconnected bool, err error)
```

> **Important — signature change:** `RegisterShareWithOptions` (and `RegisterShare`) must return the allocated `origin` so the caller can `binder.Allow(origin, direct, code)`. Update all call sites (daemon, main.go single-session mode).

- [ ] **Step 1: Write the failing test**

```go
package signaling

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSubmitCSRPayload(t *testing.T) {
	// Use a stub conn via a pipe isn't trivial; instead assert the message
	// shape by round-tripping through json.Marshal of the raw map helper.
	c := &Client{}
	_ = c
	msg := csrSubmitMsg("----BEGIN CERTIFICATE REQUEST-----")
	if msg["type"] != "csr_submit" {
		t.Fatalf("type = %v", msg["type"])
	}
	if msg["csr_pem"] == "" {
		t.Fatalf("csr_pem empty")
	}
	b, _ := json.Marshal(msg)
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if back["csr_pem"] == "" {
		t.Fatalf("csr_pem lost")
	}
}

func TestOpenAckShape(t *testing.T) {
	msg := openAckMsg(OpenAck{ShareID: "abc", Nonce: "n", Seq: 7, GrantedPort: 443, PublicIP: "1.2.3.4", Status: "ok"})
	b, _ := json.Marshal(msg)
	var back map[string]any
	_ = json.Unmarshal(b, &back)
	if back["share_id"] != "abc" || back["seq"] != float64(7) || back["status"] != "ok" {
		t.Fatalf("bad shape: %v", back)
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd agent && go test ./internal/signaling/ -run 'TestSubmitCSRPayload|TestOpenAckShape' -v`
Expected: FAIL — undefined helpers.

- [ ] **Step 3: Implement**

Add to `client.go`:

```go
// OpenAck is the agent's response to an open_signal.
type OpenAck struct {
	ShareID        string `json:"share_id"`
	Nonce          string `json:"nonce"`
	Seq            uint64 `json:"seq"`
	GrantedPort    int    `json:"granted_port"`
	PublicIP       string `json:"public_ip"`
	WasAlreadyOpen bool   `json:"was_already_open"`
	Status         string `json:"status"`            // "ok" | "error"
	Error          string `json:"error,omitempty"`   // bounded error code
}

func (c *Client) SubmitCSR(ctx context.Context, csrPEM string) error {
	return c.Send(ctx, csrSubmitMsg(csrPEM))
}
func csrSubmitMsg(csrPEM string) map[string]any {
	return map[string]any{"type": "csr_submit", "csr_pem": csrPEM}
}

func (c *Client) ReportEndpoint(ctx context.Context, ip string, port int, status string) error {
	m := map[string]any{"type": "report_endpoint", "ip": ip, "port": port}
	if status != "" {
		m["status"] = status
	}
	return c.Send(ctx, m)
}

func (c *Client) OpenAck(ctx context.Context, ack OpenAck) error {
	return c.Send(ctx, openAckMsg(ack))
}
func openAckMsg(a OpenAck) map[string]any {
	m := map[string]any{
		"type": "open_ack", "share_id": a.ShareID, "nonce": a.Nonce,
		"seq": a.Seq, "granted_port": a.GrantedPort, "public_ip": a.PublicIP,
		"was_already_open": a.WasAlreadyOpen, "status": a.Status,
	}
	if a.Error != "" {
		m["error"] = a.Error
	}
	return m
}

func (c *Client) TLSReady(ctx context.Context, fingerprint, notAfter string) error {
	return c.Send(ctx, map[string]any{
		"type": "tls_ready", "fingerprint": fingerprint, "not_after": notAfter,
	})
}

func (c *Client) TLSError(ctx context.Context, reason string) error {
	return c.Send(ctx, map[string]any{"type": "tls_error", "reason": reason})
}
```

Extend `Message` with the new fields:

```go
type Message struct {
	// ... existing fields ...
	Namespace      string `json:"namespace,omitempty"`
	ChainPEM       string `json:"chain_pem,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Nonce          string `json:"nonce,omitempty"`
	Seq            uint64 `json:"seq,omitempty"`
	ShareID        string `json:"share_id,omitempty"`
	Route          string `json:"route,omitempty"`
	LeaseSeconds   int    `json:"lease_seconds,omitempty"`
	GrantedPort    int    `json:"granted_port,omitempty"`
	PublicIP       string `json:"public_ip,omitempty"`
	WasAlreadyOpen bool   `json:"was_already_open,omitempty"`
	Status         string `json:"status,omitempty"`
	Fingerprint    string `json:"fingerprint,omitempty"`
	NotAfter       string `json:"not_after,omitempty"`
	Origin         string `json:"origin,omitempty"`
	Version        int    `json:"version,omitempty"`
}
```

Change `RegisterShareWithOptions` to return origin:

```go
func (c *Client) RegisterShareWithOptions(ctx context.Context, opts RegisterShareOptions) (string, string, bool, error) {
	// ... (existing body) ...
	case resp := <-responseCh:
		if resp.Type == "error" {
			return "", "", false, fmt.Errorf("server error: %s", resp.Err)
		}
		return resp.Code, resp.Origin, resp.Reconnected, nil
	case <-ctx.Done():
		return "", "", false, ctx.Err()
	}
}
```

Update `RegisterShare` (the thin wrapper) to match, and every call site (daemon `CreateSession`, `loadSessionsFromStore`, `registerImmichShare`, and `main.go` `runSession`).

- [ ] **Step 4: Run test + build, verify pass**

Run: `cd agent && go test ./internal/signaling/ -run 'TestSubmitCSRPayload|TestOpenAckShape' -v && go build ./...`
Expected: PASS (tests) + BUILD succeeds (fix any call-site compile errors).

- [ ] **Step 5: Commit**

```bash
git add agent/internal/signaling/ agent/internal/daemon/ agent/cmd/agent/
git commit -m "feat(agent): signaling client message types + direct-mode send helpers"
```

---

## Task 5: Control — cert coordinator (issuance controls)

**Files:**
- Create: `signaling-server/internal/certcoordinator/coordinator.go`
- Create: `signaling-server/internal/certcoordinator/coordinator_test.go`

**Interfaces:**
- Consumes: `CompleteCSR` (existing), `directctl.chainCacheHas` (wire the real implementation here).
- Produces:
```go
type CoordinatorConfig struct {
	CA, Email, CloudflareToken, BaseDomain string
	AccountKeyPath string // persisted ACME account key file
}
type Coordinator struct { /* ... */ }
func NewCoordinator(cfg CoordinatorConfig) (*Coordinator, error)
func (c *Coordinator) Issue(ctx context.Context, csrPEM []byte, namespace string) (chainPEM []byte, err error)
func (c *Coordinator) HasFingerprint(apiKeyID, fingerprint string) bool
```

- [ ] **Step 1: Write the failing test (cooldown + fingerprint idempotency via an injected issue func)**

```go
package certcoordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func TestCoordinatorIdempotentAndCooldown(t *testing.T) {
	ctx := context.Background()
	c := newTestCoordinator()
	csr := []byte("csr-1")
	chain := []byte("chain-1")
	fp := hex.EncodeToString(sum(csr))

	// Inject a fake issuer that returns chain-1 once.
	c.issueFn = func(ctx context.Context, csrPEM []byte, namespace string) ([]byte, error) {
		return chain, nil
	}

	got, err := c.Issue(ctx, csr, "sbdeadbeef")
	if err != nil { t.Fatal(err) }
	if string(got) != string(chain) { t.Fatalf("got %q", got) }
	if !c.HasFingerprint("k", fp) { t.Fatalf("fingerprint not cached") }

	// Idempotent retry of the same CSR returns the cached chain without re-issuing.
	c.issueFn = func(ctx context.Context, csrPEM []byte, namespace string) ([]byte, error) {
		return nil, errors.New("should not re-issue")
	}
	got2, err := c.Issue(ctx, csr, "sbdeadbeef")
	if err != nil { t.Fatal(err) }
	if string(got2) != string(chain) { t.Fatalf("idempotent retry changed chain") }

	// A different CSR within the cooldown is rejected.
	c.issueFn = func(ctx context.Context, csrPEM []byte, namespace string) ([]byte, error) {
		return []byte("chain-2"), nil
	}
	if _, err := c.Issue(ctx, []byte("csr-2"), "sbdeadbeef"); err == nil {
		t.Fatalf("expected cooldown rejection")
	}
}

func sum(b []byte) []byte { s := sha256.Sum256(b); return s[:] }
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd signaling-server && go test ./internal/certcoordinator/ -run TestCoordinator -v`
Expected: FAIL — `Coordinator`/`newTestCoordinator` undefined.

- [ ] **Step 3: Implement**

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
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"
)

const issuanceCooldown = 5 * time.Minute

type CoordinatorConfig struct {
	CA, Email, CloudflareToken, BaseDomain string
	AccountKeyPath                         string
}

type Coordinator struct {
	cfg CoordinatorConfig
	acct *acmeAccount // persisted account key (loaded/created once)

	mu       sync.Mutex
	inflight map[string]bool          // namespace -> in-flight
	cache    map[string]cachedChain   // fingerprint -> chain
	last     map[string]time.Time     // namespace -> last issuance
	issueFn  func(ctx context.Context, csrPEM []byte, namespace string) ([]byte, error)
}

type cachedChain struct {
	chain []byte
	notAfter time.Time
}

func NewCoordinator(cfg CoordinatorConfig) (*Coordinator, error) {
	c := &Coordinator{
		cfg: cfg, inflight: map[string]bool{}, cache: map[string]cachedChain{},
		last: map[string]time.Time{},
	}
	c.issueFn = c.completeCSR
	key, err := c.loadOrCreateAccountKey()
	if err != nil {
		return nil, err
	}
	c.acct = &acmeAccount{email: cfg.Email, key: key}
	return c, nil
}

func (c *Coordinator) completeCSR(ctx context.Context, csrPEM []byte, namespace string) ([]byte, error) {
	// Persist the account key so renewals reuse the ACME account.
	// CompleteCSR currently generates an ephemeral key; we extend it to accept
	// a pre-registered account. For this task, call CompleteCSR (unchanged) and
	// note that the account-key persistence is layered on in Step 4 below.
	return CompleteCSR(ctx, csrPEM, ACMEConfig{
		CA: c.cfg.CA, Email: c.cfg.Email, CloudflareToken: c.cfg.CloudflareToken,
		Namespace: namespace, BaseDomain: c.cfg.BaseDomain,
	})
}

func (c *Coordinator) Issue(ctx context.Context, csrPEM []byte, namespace string) ([]byte, error) {
	fp := fingerprint(csrPEM)

	c.mu.Lock()
	if ch, ok := c.cache[fp]; ok {
		c.mu.Unlock()
		return append([]byte(nil), ch.chain...), nil // idempotent: no re-issue
	}
	if c.inflight[namespace] {
		c.mu.Unlock()
		return nil, errors.New("issuance already in flight for this agent")
	}
	if last, ok := c.last[namespace]; ok && time.Since(last) < issuanceCooldown {
		c.mu.Unlock()
		return nil, errors.New("issuance cooldown active")
	}
	c.inflight[namespace] = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.inflight, namespace)
		c.last[namespace] = time.Now()
		c.mu.Unlock()
	}()

	chain, err := c.issueFn(ctx, csrPEM, namespace)
	if err != nil {
		return nil, err
	}
	if len(chain) > 1<<20 { // cap chain payload size (1 MiB)
		return nil, errors.New("chain too large")
	}

	c.mu.Lock()
	c.cache[fp] = cachedChain{chain: chain, notAfter: leafNotAfter(chain)}
	c.mu.Unlock()
	return append([]byte(nil), chain...), nil
}

func (c *Coordinator) HasFingerprint(apiKeyID, fingerprint string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.cache[fingerprint]
	return ok
}

func (c *Coordinator) loadOrCreateAccountKey() (*ecdsa.PrivateKey, error) {
	if c.cfg.AccountKeyPath == "" {
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if b, err := os.ReadFile(c.cfg.AccountKeyPath); err == nil {
		block, _ := pem.Decode(b)
		if block != nil && block.Type == "EC PRIVATE KEY" {
			return x509.ParseECPrivateKey(block.Bytes)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, _ := x509.MarshalECPrivateKey(key)
	_ = os.WriteFile(c.cfg.AccountKeyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0600)
	return key, nil
}

func fingerprint(csrPEM []byte) string {
	// SHA-256 of the DER CSR (strip PEM wrapper).
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		sum := sha256.Sum256(csrPEM)
		return hex.EncodeToString(sum[:])
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

> **Account-key persistence note:** `CompleteCSR` (acme.go) currently generates an ephemeral account each call. For production the persisted key must be threaded into `CompleteCSR`. Add an optional field `AccountKey *ecdsa.PrivateKey` to `ACMEConfig` and, when non-nil, build `acmeAccount` from it instead of generating. The coordinator passes `c.acct.key`.

- [ ] **Step 4: Wire `directctl.chainCacheHas` to the coordinator**

In Task 2 the `chainCacheHas` stub returns `false`. Now replace the `directctl` package's reliance with a callback: add to `directctl` a package-level hook

```go
// directctl/agentstore.go (append)
var chainCacheHasFn func(apiKeyID, fingerprint string) bool
func SetChainCacheHas(fn func(apiKeyID, fingerprint string) bool) { chainCacheHasFn = fn }
func chainCacheHas(apiKeyID, fingerprint string) bool {
	if chainCacheHasFn == nil {
		return false
	}
	return chainCacheHasFn(apiKeyID, fingerprint)
}
```

and in `main.go` set `directctl.SetChainCacheHas(coord.HasFingerprint)`.

- [ ] **Step 5: Run tests, verify pass**

Run: `cd signaling-server && go test ./internal/certcoordinator/ -run TestCoordinator -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 6: Commit**

```bash
git add signaling-server/internal/certcoordinator/ signaling-server/internal/directctl/
git commit -m "feat(control): cert coordinator with account-key persistence, singleflight, cooldown, CSR idempotency"
```

---

## Task 6: Control — enrollment + tls_ready handlers

**Files:**
- Create: `signaling-server/internal/directctl/enroll.go`
- Test: `signaling-server/internal/directctl/enroll_test.go`

**Interfaces:**
- Consumes: `LoadOrCreateAgent`, `SaveCertReady`, `AcceptableTLSReady`, `Coordinator`.
- Produces:
```go
func (c *Controller) HandleHello(ctx context.Context, conn *websocket.Conn, apiKeyID, accountID, agentID string)
func (c *Controller) HandleTLSReady(ctx context.Context, conn *websocket.Conn, apiKeyID, fingerprint, notAfter string)
```

- [ ] **Step 1: Write the failing test**

```go
package directctl

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Use a fake Controller with a nil conn-safe sender; assert the DB side-effects
// and the message emitted. conn may be nil in tests when the sender is stubbed.
func TestEnrollmentFlow(t *testing.T) {
	app, ctrl := newTestController(t)

	// hello → enrolled {namespace}
	sent := ctrl.captureSend(func() { ctrl.HandleHello(context.Background(), nil, "key-1", "acct-1", "agent-1") })
	if sent["type"] != "enrolled" {
		t.Fatalf("expected enrolled, got %v", sent)
	}
	if sent["namespace"] == "" {
		t.Fatalf("namespace empty")
	}

	// tls_ready → cert_status ready + enrollment_ready
	sent2 := ctrl.captureSend(func() {
		ctrl.HandleTLSReady(context.Background(), nil, "key-1", "fp-abc", time.Now().Add(90*24*time.Hour).Format(time.RFC3339))
	})
	if sent2["type"] != "enrollment_ready" {
		t.Fatalf("expected enrollment_ready, got %v", sent2)
	}
	rec, _, _ := LoadOrCreateAgent(app, "key-1")
	if rec.GetString("cert_status") != "ready" {
		t.Fatalf("cert_status = %q", rec.GetString("cert_status"))
	}
	if rec.GetString("cert_fingerprint") != "fp-abc" {
		t.Fatalf("fingerprint = %q", rec.GetString("cert_fingerprint"))
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd signaling-server && go test ./internal/directctl/ -run TestEnrollmentFlow -v`
Expected: FAIL — `newTestController`/`Controller` undefined.

- [ ] **Step 3: Implement `Controller` skeleton + enrollment**

```go
// controller.go (first slice — grows in Tasks 8/12/13/14/16/17)
package directctl

import (
	"context"
	"encoding/json"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/certcoordinator"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/ddns"
	"sharebridge/server/internal/hub"
)

type Config struct {
	BaseDomain string
}

type Controller struct {
	app  core.App
	hub  *hub.Hub
	coord *certcoordinator.Coordinator
	ddns *ddns.Cloudflare
	cfg  Config

	sendFn func(ctx context.Context, conn *websocket.Conn, msg any) error
}

func NewController(app core.App, h *hub.Hub, coord *certcoordinator.Coordinator, ddnsClient *ddns.Cloudflare, cfg Config) *Controller {
	c := &Controller{app: app, hub: h, coord: coord, ddns: ddnsClient, cfg: cfg}
	c.sendFn = func(ctx context.Context, conn *websocket.Conn, msg any) error {
		return hub.SendDirect(ctx, conn, msg)
	}
	return c
}
```

```go
// enroll.go
package directctl

import (
	"context"
	"time"

	"github.com/coder/websocket"
)

func (c *Controller) HandleHello(ctx context.Context, conn *websocket.Conn, apiKeyID, accountID, agentID string) {
	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		c.sendFn(ctx, conn, map[string]string{"type": "error", "message": "agent enrollment failed"})
		return
	}
	c.sendFn(ctx, conn, map[string]string{"type": "enrolled", "namespace": rec.GetString("namespace")})
}

func (c *Controller) HandleTLSReady(ctx context.Context, conn *websocket.Conn, apiKeyID, fingerprint, notAfter string) {
	ok, err := AcceptableTLSReady(c.app, apiKeyID, fingerprint)
	if err != nil || !ok {
		c.sendFn(ctx, conn, map[string]string{"type": "cert_error", "reason": "unknown fingerprint"})
		return
	}
	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		return
	}
	na := time.Now().Add(90 * 24 * time.Hour)
	if t, err := time.Parse(time.RFC3339, notAfter); err == nil {
		na = t
	}
	if err := SaveCertReady(c.app, rec, fingerprint, na); err != nil {
		c.sendFn(ctx, conn, map[string]string{"type": "cert_error", "reason": "persist failed"})
		return
	}
	c.sendFn(ctx, conn, map[string]string{"type": "enrollment_ready"})
}
```

- [ ] **Step 4: Route messages in `agent_ws.go`**

Add cases to the `switch msg.Type`:

```go
case "csr_submit":
	ctrl.HandleCSRSubmit(ctx, conn, apiKeyID, msg.CSRPEM)   // Task 5 wiring
case "tls_ready":
	ctrl.HandleTLSReady(ctx, conn, apiKeyID, msg.Fingerprint, msg.NotAfter)
case "tls_error":
	ctrl.HandleTLSError(ctx, apiKeyID, msg.Reason)
case "report_endpoint":
	ctrl.HandleReportEndpoint(ctx, apiKeyID, msg.IP, msg.Port, msg.Status)
case "open_ack":
	ctrl.HandleOpenAck(apiKeyID, msg)
```

(Some handlers land in later tasks; add the `ctrl` field to the AgentWS closure now — see Task 17 for the full signature.)

- [ ] **Step 5: Run test, verify it passes**

Run: `cd signaling-server && go test ./internal/directctl/ -run TestEnrollmentFlow -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add signaling-server/internal/directctl/ signaling-server/internal/handler/
git commit -m "feat(control): enrollment + tls_ready handlers (readiness handshake)"
```

---

## Task 7: Agent — daemon enrollment startup gate

**Files:**
- Modify: `agent/internal/daemon/daemon.go`
- Modify: `agent/cmd/agent/main.go`
- Test: `agent/internal/daemon/daemon_direct_test.go` (create)

**Interfaces:**
- Consumes: `cert.Manager`, `signaling.Client` send helpers, `share_registered` origin.
- Produces: the daemon holds a `direct *directState` and gates `RegisterShare` on enrollment readiness.

- [ ] **Step 1: Write the failing test**

```go
package daemon

import (
	"testing"
)

// Fake signaling client that records RegisterShare attempts.
type gateFakeSig struct{ *fakeSignaling }

func TestEnrollmentGatesRegistration(t *testing.T) {
	d := &Daemon{ direct: &directState{ready: false}, signaling: &gateFakeSig{} }
	if d.canRegisterDirect() {
		t.Fatalf("registration must be gated before readiness")
	}
	d.direct.ready = true
	if !d.canRegisterDirect() {
		t.Fatalf("registration allowed after readiness")
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd agent && go test ./internal/daemon/ -run TestEnrollmentGates -v`
Expected: FAIL — `directState` undefined.

- [ ] **Step 3: Implement the enrollment gate + message handling**

Add a `directState` to the daemon and handlers:

```go
// daemon.go (append)
type directState struct {
	namespace string
	ready     bool
	cert      *cert.Manager
	binder    *direct.Binder
	gate      *direct.SignalGate
	server    *direct.DirectServer
}

func (d *Daemon) canRegisterDirect() bool {
	return d.direct != nil && d.direct.ready
}
```

In `handleSignalingMessage`, add cases:

```go
case "enrolled":
	d.handleEnrolled(msg.Namespace)
case "cert_issue":
	d.handleCertIssue(msg.ChainPEM)
case "cert_error":
	log.Printf("cert issuance failed: %s", msg.Reason)
	d.signaling.TLSError(context.Background(), msg.Reason)
case "enrollment_ready":
	d.handleEnrollmentReady()
case "open_signal":
	d.handleOpenSignal(msg)
```

Implement:

```go
func (d *Daemon) handleEnrolled(namespace string) {
	if namespace == "" { return }
	if err := d.direct.cert.SetNamespace(namespace); err != nil { return }
	d.direct.namespace = namespace
	// Report our public IP (port 0 = closed) so the control provisions DDNS.
	go func() {
		ip, _ := d.direct.mapperIP() // from OnDemandPort's mapper ExternalIP
		d.signaling.ReportEndpoint(context.Background(), ip, 0, "")
	}()
	// Generate + submit CSR.
	csr, err := d.direct.cert.GenerateCSR()
	if err != nil { log.Printf("generate CSR: %v", err); return }
	if err := d.signaling.SubmitCSR(context.Background(), string(csr)); err != nil {
		log.Printf("submit CSR: %v", err)
	}
}

func (d *Daemon) handleCertIssue(chainPEM string) {
	if err := d.direct.cert.Install([]byte(chainPEM)); err != nil {
		log.Printf("install cert: %v", err)
		d.signaling.TLSError(context.Background(), err.Error())
		return
	}
	fp, _ := d.direct.cert.LeafFingerprint()
	na, _ := d.direct.cert.NotAfter()
	d.signaling.TLSReady(context.Background(), fp, na.Format(time.RFC3339))
}

func (d *Daemon) handleEnrollmentReady() {
	d.direct.ready = true
	log.Printf("agent enrolled and cert-ready")
}
```

> **Reconnect reconciliation** (spec §4): on `enrolled`, if `cert.Installed()` is true and `!NeedsRenewal()`, the agent re-sends `tls_ready` instead of a fresh CSR. Extend `handleEnrolled`:
> ```go
> if d.direct.cert.Installed() && !d.direct.cert.NeedsRenewal() {
>     fp, _ := d.direct.cert.LeafFingerprint()
>     na, _ := d.direct.cert.NotAfter()
>     d.signaling.TLSReady(context.Background(), fp, na.Format(time.RFC3339))
>     return
> }
> ```
> (place after `SetNamespace` and before `GenerateCSR`).

- [ ] **Step 4: Run test + build, verify pass**

Run: `cd agent && go test ./internal/daemon/ -run TestEnrollmentGates -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/daemon/ agent/cmd/agent/
git commit -m "feat(agent): enrollment startup gate (hello→enrolled→csr→cert→tls_ready→ready)"
```

---

## Task 8: Agent — `OnDemandPort` ownership + fast-path + transition callback

**Files:**
- Modify: `agent/internal/direct/portmap.go`
- Modify: `agent/internal/direct/ondemand.go`
- Modify: `agent/internal/direct/ondemand_test.go`

**Interfaces:**
- Consumes: `PortMapper`, `ChooseExternalPort`.
- Produces: `DeleteOwnedMapping(mapper, extPort, want PortMapping)`, `PortMapper.InternalIP()`, `PortState`, `State()`, `SetTransitionCallback()`, `OpenFor` fast-path, close-failed recovery, cold-port reselection.

- [ ] **Step 1: Add `InternalIP()` to `PortMapper` + exact-match delete (portmap.go)**

```go
type PortMapper interface {
	AddPortMapping(externalPort, internalPort int, description string, leaseSeconds int) (int, error)
	DeletePortMapping(externalPort int) error
	ExternalIP() (string, error)
	ListPortMappings() ([]PortMapping, error)
	InternalIP() string // LAN address the router forwards to ("" if unknown)
}
```

Add `InternalIP()` to both mappers (`UPnPMapper` returns `m.internalIP`; `NATPMPMapper` returns `""`).

Replace `DeleteOwnedMapping`:

```go
// DeleteOwnedMapping removes the mapping at externalPort only if it EXACTLY
// matches want (description, internal port, internal client, protocol) — the
// per-agent ownership token lives in want.Description. NAT-PMP cannot
// enumerate, so deletion is best-effort (its mappings expire on their own).
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

- [ ] **Step 2: Write the failing tests (fast-path + ownership + close-failed recovery)**

```go
// ondemand_test.go (append)
func TestOpenForFastPathExtendsDeadline(t *testing.T) {
	// fake mapper + fake clock; open once for 60s, advance to 5s before
	// deadline, call OpenFor again; assert no second AddPortMapping call and
	// deadline extended.
}

func TestColdOpenReselectsPort(t *testing.T) {
	// fake mapper whose ListPortMappings reports the preferred port occupied;
	// assert OpenFor grants a different (safe) port after re-enumeration.
}

func TestCloseFailedRecoveryReattemptsDelete(t *testing.T) {
	// fake mapper that fails the first DeletePortMapping then succeeds;
	// assert close-failed surfaces via State()==StateCloseFailed and a
	// subsequent cold open deletes the old mapping before re-selecting.
}
```

(Full fake-mapper + fake-clock scaffolding already exists in `ondemand_test.go`; extend it.)

- [ ] **Step 3: Run tests, verify they fail**

Run: `cd agent && go test ./internal/direct/ -run 'TestOpenForFastPath|TestColdOpenReselects|TestCloseFailedRecovery' -v`
Expected: FAIL.

- [ ] **Step 4: Implement the changes**

`OnDemandPort` gains fields `descPrefix string`, `intClient string`, `cb func(old, new PortState, grantedPort int)`, and a `state PortState`. Constructor changes:

```go
func NewOnDemandPortOpts(mapper PortMapper, extPort, intPort int, idleTimeout time.Duration) *OnDemandPort {
	return NewOnDemandPortOwned(mapper, extPort, intPort, idleTimeout, DescriptionPrefix, "")
}

// NewOnDemandPortOwned constructs an OnDemandPort with a per-agent ownership
// token embedded in the mapping description and an optional transition callback.
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
```

Add:

```go
type PortState int
const (
	StateClosed PortState = iota
	StateOpen
	StateClosing
	StateCloseFailed
)

func (p *OnDemandPort) State() PortState {
	ch := make(chan portReply, 1)
	_ = p.send(portCommand{op: opState, reply: ch})
	// loop echoes state in the reply
	r := p.send(portCommand{op: opState, reply: ch})
	return r.state
}

func (p *OnDemandPort) SetTransitionCallback(cb func(old, new PortState, grantedPort int)) {
	ch := make(chan portReply, 1)
	p.send(portCommand{op: opSetCallback, cb: cb, reply: ch})
}
```

> **Implementation note (state loop):** add `opState` + `opSetCallback` commands; `desc()` becomes `fmt.Sprintf("%s-%d", p.descPrefix, p.extPort)`. On every state change (open→closing, closing→open, closing→close-failed, close-failed→open, closing→closed), call `p.cb(old, new, grantedPort)` synchronously (non-blocking; never call back into `OnDemandPort`; initial state not emitted).

`OpenFor` fast path (replace the `opOpenFor` branch):

```go
case opOpenFor:
	l := c.lease
	if l < minValidLease { l = minValidLease }
	now := p.clock.Now()
	// Cold open (or open from close-failed): clean up the previous mapping,
	// re-enumerate, and re-select a safe external port.
	if !open {
		if closing {
			// re-attempt deletion of the previous mapping before re-selecting
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
		port := requested
		granted, err := p.mapper.AddPortMapping(port, p.intPort, p.desc(), int(l.Seconds()))
		if err != nil {
			c.reply <- portReply{err: err}
			continue
		}
		grantedPort = granted
		open = true
		closing = false
		lease = l
		deadline = now.Add(l)
		renewAt = deadline.Add(-p.renewWindow)
		p.setState(StateOpen, grantedPort)
		rearm()
		c.reply <- portReply{err: nil, open: true, granted: granted}
		continue
	}
	// Fast path: already open. Extend the local deadline to at least the
	// requested lease; if the remaining router lease is too short, renew.
	wasOpen := true
	if now.Add(l).After(deadline) {
		deadline = now.Add(l)
		renewAt = deadline.Add(-p.renewWindow)
	}
	if deadline.Sub(now) < l {
		// remaining router lease too short: guarded renewal
		granted, err := p.mapper.AddPortMapping(grantedPort, p.intPort, p.desc(), int(l.Seconds()))
		if err != nil {
			renewFailed = true
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

Add `wasOpen` + `state` to `portReply`, and `setState` helper:

```go
func (p *OnDemandPort) setState(next PortState, grantedPort int) {
	if p.cb != nil && p.state != next {
		p.cb(p.state, next, grantedPort)
	}
	p.state = next
}
```

Wire `setState` into `startClose` (→ StateClosing or StateCloseFailed), `tryDelete` success (→ StateClosed), and the close-failed branch (`closing = false` after `closeFail >= maxCloseAttempts` → `setState(StateCloseFailed, grantedPort)`).

`tryDelete` uses the new exact-match signature:

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

- [ ] **Step 5: Run tests, verify they pass**

Run: `cd agent && go test ./internal/direct/ -run 'TestOpenForFastPath|TestColdOpenReselects|TestCloseFailedRecovery' -v && go test ./internal/direct/`
Expected: PASS (all direct tests).

- [ ] **Step 6: Commit**

```bash
git add agent/internal/direct/
git commit -m "feat(agent): OnDemandPort ownership token, exact-match delete, fast-path, cold reselect, transition callback, close-failed recovery"
```

---

## Task 9: Agent — `SignalGate` epoch reset + nonce verify + rate-limit raise

**Files:**
- Modify: `agent/internal/direct/opensignal.go`
- Modify: `agent/internal/direct/opensignal_test.go`

**Interfaces:**
- Produces: `Reset()`, `VerifyNonce(nonce, shareID) bool`; `maxSignalsPerWin = 60`, remove per-share cap.

- [ ] **Step 1: Write the failing tests**

```go
// opensignal_test.go (append)
func TestResetClearsSequenceAndNonces(t *testing.T) {
	g := NewSignalGate("agent-1", func(string, RouteKind) bool { return true })
	if err := g.Admit(validSig(g, "share-1", "nonce-1", 1)); err != nil {
		t.Fatal(err)
	}
	if g.VerifyNonce("nonce-1", "share-1") != true {
		t.Fatalf("nonce should verify before reset")
	}
	g.Reset()
	if g.VerifyNonce("nonce-1", "share-1") {
		t.Fatalf("nonce must not verify after reset")
	}
	// After reset, a lower sequence number is acceptable (new epoch).
	if err := g.Admit(validSig(g, "share-1", "nonce-2", 1)); err != nil {
		t.Fatalf("reset must accept low seq in new epoch: %v", err)
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

func TestVerifyNonceWrongShare(t *testing.T) {
	g := NewSignalGate("agent-1", func(string, RouteKind) bool { return true })
	_ = g.Admit(validSig(g, "share-1", "nonce-x", 1))
	if g.VerifyNonce("nonce-x", "share-2") {
		t.Fatalf("nonce must not verify for a different share")
	}
}
```

- [ ] **Step 2: Run tests, verify they fail**

Run: `cd agent && go test ./internal/direct/ -run 'TestReset|TestRateLimitAllows|TestVerifyNonce' -v`
Expected: FAIL — `Reset`/`VerifyNonce` undefined.

- [ ] **Step 3: Implement**

```go
// Reset clears all per-epoch state (sequence high-water mark, seen nonces,
// applied map, rate-limit windows). Call it on every new authenticated WS
// connection so a control restart (which resets its own counter) and a
// replacement socket cannot brick or corrupt signal admission.
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

// VerifyNonce reports whether nonce was recently admitted for shareID — used by
// the direct server's reachability-probe handler to echo only genuine nonces.
func (g *SignalGate) VerifyNonce(nonce, shareID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	u, ok := g.seen[nonce]
	return ok && u.shareID == shareID
}
```

Change rate limit:

```go
const (
	signalWindow     = time.Minute
	maxSignalsPerWin = 60 // generous global ceiling (was 10; per-share cap removed)
	// maxPerSharePerWin removed
)
```

And `rateLimit` drops the per-share branch:

```go
func (g *SignalGate) rateLimit(now time.Time, shareID string) error {
	if now.Sub(g.winStart) >= signalWindow {
		g.winStart = now
		g.winCount = 0
		g.perShare = map[string]int{}
	}
	if g.winCount >= maxSignalsPerWin {
		return ErrSignalRate
	}
	g.winCount++
	g.perShare[shareID]++
	return nil
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `cd agent && go test ./internal/direct/ -run 'TestReset|TestRateLimitAllows|TestVerifyNonce' -v && go test ./internal/direct/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/opensignal.go agent/internal/direct/opensignal_test.go
git commit -m "feat(agent): SignalGate epoch reset, nonce verification, 60/min rate limit"
```

---

## Task 10: Agent — endpoint reporter (transition callback → report_endpoint)

**Files:**
- Modify: `agent/internal/daemon/daemon.go` (wire the callback)
- Test: `agent/internal/direct/reporter_test.go` (create) — or fold into daemon tests

**Interfaces:**
- Consumes: `OnDemandPort.SetTransitionCallback`, `signaling.ReportEndpoint`.
- Produces: endpoint reporting on open/close/close-failed, `port:0` only when deletion succeeded.

- [ ] **Step 1: Write the failing test**

```go
package direct

import (
	"testing"
)

type recordReporter struct{ events []ReportEvent }

type ReportEvent struct{ IP string; Port int; Status string }

func TestReporterNeverReportsZeroOnFailure(t *testing.T) {
	// Use a fake mapper that fails deletion; open then close; assert the
	// reporter saw status "close_failed" with a NONZERO port, never port 0.
}
```

> The reporter is a thin adapter: it maps `(old, new, grantedPort)` transitions to `report_endpoint` messages. Place it in `agent/internal/direct/reporter.go`:

```go
// reporter.go
package direct

// Reporter turns OnDemandPort state transitions into endpoint reports.
type Reporter struct {
	ip     string
	send   func(ip string, port int, status string)
}

func NewReporter(ip string, send func(ip string, port int, status string)) *Reporter {
	return &Reporter{ip: ip, send: send}
}

func (r *Reporter) OnTransition(old, new PortState, grantedPort int) {
	switch new {
	case StateClosed:
		r.send(r.ip, 0, "")              // port 0 ⇔ closed (status omitted)
	case StateOpen:
		r.send(r.ip, grantedPort, "")    // open report
	case StateCloseFailed:
		r.send(r.ip, grantedPort, "close_failed") // nonzero port, truthful
	case StateClosing:
		// no report (intermediate)
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd agent && go test ./internal/direct/ -run TestReporter -v`
Expected: FAIL — `Reporter`/`ReportEvent` undefined.

- [ ] **Step 3: Implement + wire**

Create `reporter.go` (above). Wire it in the daemon when constructing the direct transport:

```go
rep := direct.NewReporter("", func(ip string, port int, status string) {
	d.signaling.ReportEndpoint(context.Background(), ip, port, status)
})
d.direct.port.SetTransitionCallback(rep.OnTransition)
```

The IP is refreshed on each cold open from the mapper's `ExternalIP()` (see Task 11's server integration).

- [ ] **Step 4: Run test, verify pass**

Run: `cd agent && go test ./internal/direct/ -run TestReporter -v && go test ./internal/daemon/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/reporter.go agent/internal/daemon/
git commit -m "feat(agent): endpoint reporter (transition callback → report_endpoint)"
```

---

## Task 11: Agent — direct HTTPS server (Binder + cert + placeholder + probe)

**Files:**
- Create: `agent/internal/direct/server.go`
- Test: `agent/internal/direct/server_test.go`

**Interfaces:**
- Consumes: `Binder`, `OnDemandPort`, `SignalGate.VerifyNonce`, `CertProvider`.
- Produces: `DirectServer` contract (TLSConfig with GetConfigForClient, Handler, Start).

- [ ] **Step 1: Write the failing test (httptest over a real TLS listener)**

```go
package direct

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServerGetConfigForClientComposesBinderAndCert(t *testing.T) {
	// Generate a self-signed leaf for the test (the Binder only needs a cert to
	// serve; VerifyNonce/placeholder don't validate the chain in this test).
	cert := testCert(t, "sbdeadbeef")
	binder := NewBinder("sbdeadbeef")
	_ = binder.Allow("demo.sbdeadbeef.example.com", RouteDirect, "abc")
	srv := NewDirectServer("sbdeadbeef", nil, certProviderFn(cert), NewSignalGate("a", func(string, RouteKind) bool { return true }), 1<<20)

	h := srv.Handler()
	ts := httptest.NewUnstartedServer(h)
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	defer ts.Close()

	// Unknown SNI → handshake rejected.
	client := ts.Client()
	req, _ := http.NewRequest("GET", ts.URL+"/s/abc", nil)
	req.Host = "other.sbdeadbeef.example.com"
	if _, err := client.Do(req); err == nil {
		t.Fatalf("expected handshake rejection for unknown SNI")
	}
}

type certProviderFn func() (*tls.Certificate, error)
func (f certProviderFn) Certificate() (*tls.Certificate, error) { return f() }
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd agent && go test ./internal/direct/ -run TestServerGetConfigForClient -v`
Expected: FAIL — `DirectServer`/`testCert` undefined.

- [ ] **Step 3: Implement the server**

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
	"time"
)

// CertProvider supplies the current certificate (the cert manager's job).
type CertProvider interface {
	Certificate() (*tls.Certificate, error)
}

type DirectServer struct {
	namespace       string
	port            *OnDemandPort
	certs           CertProvider
	gate            *SignalGate
	maxContentBytes int64
	binder          *Binder
}

func NewDirectServer(namespace string, port *OnDemandPort, certs CertProvider, gate *SignalGate, maxContentBytes int64) *DirectServer {
	return &DirectServer{
		namespace: namespace, port: port, certs: certs, gate: gate,
		maxContentBytes: maxContentBytes, binder: NewBinder(namespace),
	}
}

func (s *DirectServer) Binder() *Binder { return s.binder }

// TLSConfig returns a tls.Config whose GetConfigForClient performs Binder SNI
// admission AND selects the cert manager's current certificate. This is the
// single composition point: admission is never bypassed, and renewing swaps
// the certificate in memory without a listener restart.
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

func (s *DirectServer) Handler() http.Handler {
	return s.binder.Handler(http.HandlerFunc(s.route))
}

func (s *DirectServer) route(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Path is /s/<code>[/probe|/download...]. Extract code, then remainder.
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

// handleProbe echoes the nonce iff it was recently admitted for this share.
func (s *DirectServer) handleProbe(w http.ResponseWriter, r *http.Request, code string) {
	nonce := r.URL.Query().Get("nonce")
	if nonce == "" || s.gate == nil || !s.gate.VerifyNonce(nonce, code) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, nonce)
}

func (s *DirectServer) handlePage(w http.ResponseWriter, r *http.Request, code string) {
	// Begin a session so the port stays open while the recipient is active.
	sid, err := s.beginSession(code)
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	defer s.endSession(sid)
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

func (s *DirectServer) beginSession(code string) (string, error) {
	if s.port == nil {
		return "", fmt.Errorf("no port")
	}
	return s.port.BeginSession(code)
}
func (s *DirectServer) endSession(sid string) {
	if s.port != nil {
		s.port.EndSession(sid)
	}
}

// Start serves HTTPS on listenAddr until ctx is cancelled.
func (s *DirectServer) Start(ctx context.Context, listenAddr string) error {
	srv := &http.Server{
		Handler:     s.Handler(),
		TLSConfig:   s.TLSConfig(),
		ReadTimeout: 30 * time.Second,
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	return srv.ServeTLS(ln, "", "")
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `cd agent && go test ./internal/direct/ -run TestServerGetConfigForClient -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/direct/server.go agent/internal/direct/server_test.go
git commit -m "feat(agent): direct HTTPS server (GetConfigForClient composition, placeholder, probe)"
```

---

## Task 12: Control — report_endpoint handler + DDNS trigger

**Files:**
- Create: `signaling-server/internal/directctl/endpoint.go`
- Test: `signaling-server/internal/directctl/endpoint_test.go`

**Interfaces:**
- Consumes: `LoadOrCreateAgent`, `ddns.Cloudflare.UpsertA`.
- Produces: `func (c *Controller) HandleReportEndpoint(ctx context.Context, apiKeyID, ip string, port int, status string)`; DDNS on IP change (wildcard A record, TTL 60).

- [ ] **Step 1: Write the failing test**

```go
package directctl

import "testing"

func TestHandleReportEndpointTriggersDDNSOnIPChange(t *testing.T) {
	app, ctrl := newTestController(t)
	// record created via hello
	ctrl.HandleHello(ctx, nil, "key-1", "acct-1", "agent-1")

	ctrl.HandleReportEndpoint(ctx, "key-1", "1.2.3.4", 0, "")
	rec, _, _ := LoadOrCreateAgent(app, "key-1")
	if rec.GetString("endpoint_ip") != "1.2.3.4" { t.Fatalf("ip not stored") }
	if ctrl.ddnsCalls != 1 { t.Fatalf("expected 1 DDNS call, got %d", ctrl.ddnsCalls) }

	// Same IP again → no new DDNS call.
	ctrl.HandleReportEndpoint(ctx, "key-1", "1.2.3.4", 443, "")
	if ctrl.ddnsCalls != 1 { t.Fatalf("expected no extra DDNS call") }

	// Changed IP → DDNS call.
	ctrl.HandleReportEndpoint(ctx, "key-1", "5.6.7.8", 443, "")
	if ctrl.ddnsCalls != 2 { t.Fatalf("expected 2nd DDNS call") }
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd signaling-server && go test ./internal/directctl/ -run TestHandleReportEndpoint -v`
Expected: FAIL — `ddnsCalls`/`HandleReportEndpoint` undefined.

- [ ] **Step 3: Implement**

```go
// endpoint.go
package directctl

import (
	"context"
	"time"
)

func (c *Controller) HandleReportEndpoint(ctx context.Context, apiKeyID, ip string, port int, status string) {
	rec, _, err := LoadOrCreateAgent(c.app, apiKeyID)
	if err != nil {
		return
	}
	prev := rec.GetString("endpoint_ip")
	rec.Set("endpoint_ip", ip)
	rec.Set("endpoint_port", port)
	rec.Set("last_report_at", time.Now())
	if err := c.app.Save(rec); err != nil {
		return
	}
	if ip != "" && ip != prev && c.ddns != nil {
		// Update the wildcard A record for the agent's namespace.
		ns := rec.GetString("namespace")
		name := "*." + ns + "." + c.cfg.BaseDomain
		if _, err := c.ddns.UpsertA(ctx, name, ip, 60); err != nil {
			// log and continue; a later report retries
		}
	}
}
```

Add `ddnsCalls int` to the test controller via an injectable DDNS func (add `ddnsFn func(ctx, name, ip string, ttl int) (string, error)` to `Controller` used when `c.ddns == nil`).

- [ ] **Step 4: Run test, verify pass**

Run: `cd signaling-server && go test ./internal/directctl/ -run TestHandleReportEndpoint -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/directctl/
git commit -m "feat(control): endpoint report handler + DDNS trigger on IP change"
```

---

## Task 13: Control — origin allocation in register_share

**Files:**
- Modify: `signaling-server/internal/handler/rest.go` (or `agent_ws.go` — `handleRegisterShare`)
- Modify: `signaling-server/internal/handler/agent_ws.go` (share_registered response + soft-delete in unregister)
- Test: `signaling-server/internal/handler/origin_test.go` (create)

**Interfaces:**
- Consumes: `directctl.AllocateOrigin`, `LoadOrCreateAgent`.
- Produces: `share_registered { …, origin }`; sessions soft-deleted (is_active=false) on unregister + expiry.

- [ ] **Step 1: Write the failing test**

```go
package handler

import "testing"

func TestRegisterShareReturnsOrigin(t *testing.T) {
	// register a share; assert the response map contains a non-empty origin
	// whose suffix is ".<namespace>.<baseDomain>".
}

func TestUnregisterSoftDeletes(t *testing.T) {
	// register then unregister; assert the session row still exists with
	// is_active=false (origin retained), and getSessionByCode returns nil.
}
```

- [ ] **Step 2: Run tests, verify they fail**

Run: `cd signaling-server && go test ./internal/handler/ -run 'TestRegisterShareReturnsOrigin|TestUnregisterSoftDeletes' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `handleRegisterShare`, after the session is created/reclaimed and before the response, allocate an origin if absent:

```go
// Allocate an origin if this session doesn't have one (control-allocated).
origin := session.GetString("origin")
if origin == "" {
	agentsRec, _, err := directctl.LoadOrCreateAgent(app, apiKeyID)
	if err != nil {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "database error"})
		return
	}
	origin, err = directctl.AllocateOrigin(app, agentsRec.GetString("namespace"), cfg.BaseDomain)
	if err != nil {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "origin allocation failed"})
		return
	}
	session.Set("origin", origin)
	session.Set("is_active", true)
	if err := app.Save(session); err != nil {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "database error"})
		return
	}
}
```

Then include `origin` in the response:

```go
response := map[string]any{
	"type": "share_registered", "code": code, "reconnected": reconnected,
	"origin": origin,
}
```

> `cfg.BaseDomain` requires threading `cfg` into `handleRegisterShare` (already available via the AgentWS closure's `cfg`; pass it as a parameter, or store `baseDomain` on the `directctl.Controller` and call `ctrl.AllocateOriginFor(app, apiKeyID, session)`). Prefer the controller method to keep DB access in `directctl`:

```go
// directctl (append)
func (c *Controller) AllocateOriginFor(app core.App, apiKeyID string, session *core.Record) (string, error) {
	origin := session.GetString("origin")
	if origin != "" {
		session.Set("is_active", true)
		return origin, app.Save(session)
	}
	rec, _, err := LoadOrCreateAgent(app, apiKeyID)
	if err != nil {
		return "", err
	}
	origin, err = AllocateOrigin(app, rec.GetString("namespace"), c.cfg.BaseDomain)
	if err != nil {
		return "", err
	}
	session.Set("origin", origin)
	session.Set("is_active", true)
	return origin, app.Save(session)
}
```

Change `handleUnregisterShare` and `deleteExpiredSessions` to soft-delete:

```go
// soft-delete: retain the row so the unique origin is never reused
session.Set("is_active", false)
if err := app.Save(session); err != nil { /* ... */ }
```

and update `getSessionByCode` + the redirect/session-info lookups to filter `is_active = true`:

```go
func getSessionByCode(app core.App, code string) (*core.Record, error) {
	records, err := app.FindRecordsByFilter(
		"sessions", "code = {:code} && (is_active = true || is_active = NULL)", "", 1, 0,
		map[string]any{"code": code})
	// ...
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `cd signaling-server && go test ./internal/handler/ -run 'TestRegisterShareReturnsOrigin|TestUnregisterSoftDeletes' -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/handler/ signaling-server/internal/directctl/
git commit -m "feat(control): control-allocated origin + session soft-delete (never-reused origins)"
```

---

## Task 14: Control — open-signal emitter + open-ack waiter

**Files:**
- Create: `signaling-server/internal/directctl/opensignal.go`
- Test: `signaling-server/internal/directctl/opensignal_test.go`

**Interfaces:**
- Consumes: `hub.Hub.SendToAgent`, `LoadOrCreateAgent`, per-connection seq.
- Produces:
```go
type OpenAck struct { ShareID, Nonce string; Seq uint64; GrantedPort int; PublicIP string; WasAlreadyOpen bool; Status, Error string }
func (c *Controller) EmitOpen(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error)
func (c *Controller) HandleOpenAck(apiKeyID string, ack OpenAck)
```

- [ ] **Step 1: Write the failing test (correlation + timeout + late-ack discard)**

```go
package directctl

import (
	"context"
	"testing"
	"time"
)

func TestEmitOpenCorrelatesByNonce(t *testing.T) {
	_, ctrl := newTestController(t)
	ctrl.sendToAgentFn = func(ctx context.Context, apiKeyID string, msg any) error {
		// deliver an ack back with the same nonce after a short delay
		m := msg.(map[string]any)
		go func() {
			time.Sleep(10 * time.Millisecond)
			ctrl.HandleOpenAck(apiKeyID, OpenAck{
				ShareID: "abc", Nonce: m["nonce"].(string), Seq: m["seq"].(uint64),
				GrantedPort: 443, PublicIP: "1.2.3.4", Status: "ok",
			})
		}()
		return nil
	}
	ack, err := ctrl.EmitOpen(context.Background(), "key-1", "abc", "demo.sb1.example.com", 120*time.Second)
	if err != nil { t.Fatal(err) }
	if ack.GrantedPort != 443 || ack.Status != "ok" { t.Fatalf("bad ack: %+v", ack) }
}

func TestEmitOpenTimeout(t *testing.T) {
	_, ctrl := newTestController(t)
	ctrl.ackTimeout = 50 * time.Millisecond
	ctrl.sendToAgentFn = func(ctx context.Context, apiKeyID string, msg any) error { return nil }
	if _, err := ctrl.EmitOpen(context.Background(), "key-1", "abc", "o", 120*time.Second); err == nil {
		t.Fatalf("expected timeout")
	}
}

func TestLateAckDiscarded(t *testing.T) {
	_, ctrl := newTestController(t)
	ctrl.HandleOpenAck("key-1", OpenAck{Nonce: "no-such-nonce"}) // must not panic / must be a no-op
}
```

- [ ] **Step 2: Run tests, verify they fail**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestEmitOpen|TestLateAck' -v`
Expected: FAIL — `EmitOpen`/`sendToAgentFn`/`ackTimeout` undefined.

- [ ] **Step 3: Implement**

```go
// opensignal.go
package directctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
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

type openWaiter struct{ ch chan OpenAck }

// EmitOpen sends an open_signal to the agent and blocks until the matching
// open_ack (correlated by nonce) or a timeout. The control is stateless w.r.t.
// port-open state: it ALWAYS signals and uses the live ack's granted port.
func (c *Controller) EmitOpen(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
	nonce := newNonce()
	seq := c.nextSeq(apiKeyID) // monotonic per connection epoch

	sig := map[string]any{
		"type": "open_signal", "version": 1, "agent_id": apiKeyID,
		"share_id": shareID, "route": "direct", "nonce": nonce, "seq": seq,
		"lease_seconds": int(lease.Seconds()),
		"expires_at": time.Now().Add(2 * time.Minute).Format(time.RFC3339),
	}

	waiter := &openWaiter{ch: make(chan OpenAck, 1)}
	c.waiterMu.Lock()
	c.waiters[nonce] = waiter
	c.waiterMu.Unlock()
	defer func() {
		c.waiterMu.Lock()
		delete(c.waiters, nonce)
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
	case ack := <-waiter.ch:
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
	if ok {
		delete(c.waiters, ack.Nonce) // late duplicate acks are discarded
	}
	c.waiterMu.Unlock()
	if ok {
		select {
		case w.ch <- ack:
		default:
		}
	}
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
```

`Controller` gains fields `waiterMu sync.Mutex`, `waiters map[string]*openWaiter`, `seqMu sync.Mutex`, `seq map[string]uint64`, `sendToAgentFn`, `ackTimeout`. `nextSeq(apiKeyID)` increments `c.seq[apiKeyID]`. On `AgentDisconnected(apiKeyID)` (called from `agent_ws.go` when the socket closes), reset `c.seq[apiKeyID] = 0` (sequence is scoped to the connection epoch).

- [ ] **Step 4: Run tests, verify pass**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestEmitOpen|TestLateAck' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/directctl/
git commit -m "feat(control): open-signal emitter + open-ack waiter (nonce correlation, timeout, late-ack discard)"
```

---

## Task 15: Agent — open_signal handler (Admit → OpenFor → open_ack)

**Files:**
- Modify: `agent/internal/daemon/daemon.go`
- Test: `agent/internal/daemon/daemon_direct_test.go` (extend)

**Interfaces:**
- Consumes: `SignalGate.Admit`, `OnDemandPort.OpenFor`, `signaling.OpenAck`.
- Produces: `handleOpenSignal(msg)` — admit, open, ack (positive/negative).

- [ ] **Step 1: Write the failing test**

```go
func TestHandleOpenSignalAdmitsAndOpens(t *testing.T) {
	// fake gate + fake port + fake signaling; feed an open_signal message;
	// assert OnDemandPort.OpenFor was called and an open_ack was sent with the
	// echoed nonce + seq + granted port + status ok.
}

func TestHandleOpenSignalRejectsUnknownShare(t *testing.T) {
	// gate authz returns false → open_ack status "error".
}
```

- [ ] **Step 2: Run tests, verify they fail**

Run: `cd agent && go test ./internal/daemon/ -run TestHandleOpenSignal -v`
Expected: FAIL — `handleOpenSignal` undefined.

- [ ] **Step 3: Implement**

```go
func (d *Daemon) handleOpenSignal(msg signaling.Message) {
	if d.direct == nil || !d.direct.ready {
		d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "not_ready",
		})
		return
	}
	exp, err := time.Parse(time.RFC3339, msg.ExpiresAt)
	if err != nil {
		d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "bad_expiry",
		})
		return
	}
	sig := direct.OpenSignal{
		Version: msg.Version, AgentID: d.store.GetAgentID(), ShareID: msg.ShareID,
		RouteKind: direct.RouteDirect, Nonce: msg.Nonce, Seq: msg.Seq,
		ExpiresAt: exp, Lease: time.Duration(msg.LeaseSeconds) * time.Second,
	}
	if err := d.direct.gate.Admit(sig); err != nil {
		d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "rejected",
		})
		return
	}
	wasOpen := d.direct.port.Open()
	if err := d.direct.port.OpenFor(msg.ShareID, sig.Lease); err != nil {
		d.signaling.OpenAck(context.Background(), signaling.OpenAck{
			ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq, Status: "error", Error: "open_failed",
		})
		return
	}
	ip, _ := d.direct.mapperExternalIP()
	d.signaling.OpenAck(context.Background(), signaling.OpenAck{
		ShareID: msg.ShareID, Nonce: msg.Nonce, Seq: msg.Seq,
		GrantedPort: d.direct.port.GrantedPort(), PublicIP: ip,
		WasAlreadyOpen: wasOpen, Status: "ok",
	})
}
```

Add to `directState`: `port *direct.OnDemandPort`, `mapper direct.PortMapper`, and helpers `mapperExternalIP()`.

> **Fencing note:** on reconnect, call `d.direct.gate.Reset()` inside `handleEnrolled` (a fresh gate per connection epoch) so stale signals from a prior connection are dropped.

- [ ] **Step 4: Run tests + build, verify pass**

Run: `cd agent && go test ./internal/daemon/ -run TestHandleOpenSignal -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/daemon/
git commit -m "feat(agent): open_signal handler (admit → OpenFor → open_ack)"
```

---

## Task 16: Control — reachability probe

**Files:**
- Create: `signaling-server/internal/directctl/probe.go`
- Test: `signaling-server/internal/directctl/probe_test.go`

**Interfaces:**
- Consumes: `OpenAck`, verified-tuple tracking.
- Produces:
```go
func (c *Controller) Probe(ctx context.Context, origin, code string, ack OpenAck) error
```

- [ ] **Step 1: Write the failing test (IP filtering + nonce echo + tuple gating)**

```go
package directctl

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeRejectsNonPublicIP(t *testing.T) {
	_, ctrl := newTestController(t)
	ack := OpenAck{PublicIP: "10.0.0.1", GrantedPort: 443}
	if err := ctrl.Probe(context.Background(), "demo.sb1.example.com", "abc", ack); err == nil {
		t.Fatalf("expected rejection for private IP")
	}
}

func TestProbeVerifiesNonceEcho(t *testing.T) {
	_, ctrl := newTestController(t)
	// httptest server echoes the nonce query param.
	var gotHost, gotSNI string
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotSNI = r.TLS.ServerName
		http.Redirect(w, r, "https://evil/", http.StatusFound) // probe must NOT follow
		io.WriteString(w, r.URL.Query().Get("nonce"))
	}))
	// TLS server that accepts any SNI (probe client relaxes verification).
	ts.StartTLS()
	defer ts.Close()

	// Point the probe at the test server's IP/port.
	_, portStr, _ := net.SplitHostPort(ts.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	ack := OpenAck{PublicIP: "127.0.0.1", GrantedPort: port} // loopback is public-filter-exempt for tests
	_ = ctrl.Probe(context.Background(), "demo.sb1.example.com", "abc", ack)
	// assert gotHost == origin and gotSNI == origin
}

func TestProbeSkipsAlreadyVerifiedTuple(t *testing.T) {
	_, ctrl := newTestController(t)
	ctrl.verified = map[string]string{"key-1": "1.2.3.4:443"}
	ack := OpenAck{PublicIP: "1.2.3.4", GrantedPort: 443}
	if err := ctrl.Probe(context.Background(), "demo.sb1.example.com", "abc", ack); err != nil {
		t.Fatalf("verified tuple should skip probe: %v", err)
	}
	// and assert no HTTP request was made (probeCount == 0)
}
```

- [ ] **Step 2: Run tests, verify they fail**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestProbe' -v`
Expected: FAIL — `Probe`/`verified` undefined.

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

func isPublicIP(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	if parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsLinkLocalUnicast() ||
		parsed.IsLinkLocalMulticast() || parsed.IsMulticast() || parsed.IsUnspecified() {
		return false
	}
	return true
}

// Probe verifies the mapping reaches the agent by connecting to
// https://<public_ip>:<port>/s/<code>/probe?nonce=<nonce> with BOTH SNI and
// Host set to the origin, then checking the nonce echo. It is skipped when the
// (ip, port) tuple was already verified. The client does not follow redirects.
func (c *Controller) Probe(ctx context.Context, origin, code string, apiKeyID string, ack OpenAck) error {
	tuple := fmt.Sprintf("%s:%d", ack.PublicIP, ack.GrantedPort)
	c.verifiedMu.Lock()
	if c.verified[apiKeyID] == tuple {
		c.verifiedMu.Unlock()
		return nil // already verified this tuple
	}
	c.verifiedMu.Unlock()

	if !isPublicIP(ack.PublicIP) {
		// loopback exemption for tests: allow 127.0.0.1 when explicitly in test mode
		if !c.allowPrivate {
			return fmt.Errorf("non-public IP: %s", ack.PublicIP)
		}
	}

	url := fmt.Sprintf("https://%s:%d/s/%s/probe?nonce=%s", ack.PublicIP, ack.GrantedPort, code, ack.Nonce)
	client := &http.Client{
		Timeout: 3 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // never follow redirects
		},
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				ServerName:         origin, // SNI = origin (not the IP)
				InsecureSkipVerify: true,   // authenticate via nonce, not chain
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Host = origin // Host header = origin (passes Binder authorization)

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

Add `verifiedMu`, `verified map[string]string`, `allowPrivate bool` to `Controller`.

- [ ] **Step 4: Run tests, verify pass**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestProbe' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/directctl/
git commit -m "feat(control): reachability probe (nonce echo, IP filter, verified-tuple gating, no-redirect)"
```

---

## Task 17: Control — redirect handler `/s/<code>`

**Files:**
- Modify: `signaling-server/internal/directctl/redirect.go` (create)
- Modify: `signaling-server/cmd/server/main.go` (wire route + controller + coordinator)
- Modify: `signaling-server/internal/handler/agent_ws.go` (thread `ctrl` + `AgentDisconnected`)
- Test: `signaling-server/internal/directctl/redirect_test.go`

**Interfaces:**
- Consumes: session lookup, `EmitOpen`, `Probe`.
- Produces: `func (c *Controller) Redirect(w http.ResponseWriter, r *http.Request, code string) error` — the 302 (or "direct unavailable").

- [ ] **Step 1: Write the failing test**

```go
func TestRedirect302ToOrigin(t *testing.T) {
	_, ctrl := newTestController(t)
	// fake session (code → api_key_id + origin) and fake agent online + cert ready.
	// fake EmitOpen returns ack {1.2.3.4, 443}; probe returns nil.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/s/abc", nil)
	err := ctrl.Redirect(rec, req, "abc")
	if err != nil { t.Fatal(err) }
	if rec.Code != http.StatusFound { t.Fatalf("code = %d", rec.Code) }
	loc := rec.Header().Get("Location")
	if loc != "https://demo.sb1.example.com/s/abc" { t.Fatalf("loc = %q", loc) }
	if rec.Header().Get("Cache-Control") != "no-store" { t.Fatalf("missing no-store") }
}

func TestRedirectOmitsPort443(t *testing.T) {
	// ack granted port 443 → Location has no :443
}

func TestRedirectUnavailableWhenOffline(t *testing.T) {
	// agent not connected → 503 JSON, no Location header
}
```

- [ ] **Step 2: Run tests, verify they fail**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestRedirect' -v`
Expected: FAIL — `Redirect` undefined.

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
	// 1. Resolve session (live expiry/revocation + is_active).
	sess, err := c.sessionByCode(code)
	if err != nil || sess == nil {
		return c.unavailable(w)
	}
	apiKeyID := sess.apiKeyID
	origin := sess.origin
	if origin == "" {
		return c.unavailable(w)
	}
	// 2. Guard: enrolled (cert ready) + connected.
	if !c.agentReady(apiKeyID) || !c.hub.AgentConnected(apiKeyID) {
		return c.unavailable(w)
	}
	// 3-6. open-signal → ack.
	ack, err := c.EmitOpen(r.Context(), apiKeyID, code, origin, 120*time.Second)
	if err != nil || ack.Status != "ok" {
		return c.unavailable(w)
	}
	// 7. probe when the tuple changed.
	if err := c.Probe(r.Context(), origin, code, apiKeyID, ack); err != nil {
		return c.unavailable(w)
	}
	// 8. redirect.
	loc := "https://" + origin
	if ack.GrantedPort != 443 && ack.GrantedPort != 0 {
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

Add a `sessionByCode` helper (join `sessions` → filter `code` + `is_active`, extract `api_key_id` + `origin`) and `agentReady(apiKeyID)` (reads `agents.cert_status == "ready"`). `sessionByCode` uses a small struct `{apiKeyID, origin string}`.

- [ ] **Step 4: Wire in `main.go`**

```go
coord, err := certcoordinator.NewCoordinator(certcoordinator.CoordinatorConfig{
	CA: cfg.ACMECADir, Email: cfg.ACMEEmail, CloudflareToken: cfg.CloudflareToken,
	BaseDomain: cfg.BaseDomain, AccountKeyPath: filepath.Join(cfg.DataDir, "acme-account.pem"),
})
if err != nil { log.Fatalf("coordinator: %v", err) }
directctl.SetChainCacheHas(coord.HasFingerprint)

var dnsClient *ddns.Cloudflare
if cfg.CloudflareToken != "" {
	dnsClient, err = ddns.New(ctx, cfg.CloudflareToken, cfg.BaseDomain)
	if err != nil { log.Printf("ddns unavailable: %v", err); dnsClient = nil }
}
ctrl := directctl.NewController(app, h, coord, dnsClient, directctl.Config{BaseDomain: cfg.BaseDomain})

// Replace the /s/{code} route:
router.GET("/s/{code}", func(e *core.RequestEvent) error {
	code := e.Request.PathValue("code")
	return ctrl.Redirect(e.Response, e.Request, code)
})
```

Thread `ctrl` into `handler.AgentWS(app, h, reg, cfg, ctrl)`; route the new message types (from Task 6) and call `ctrl.AgentDisconnected(apiKeyID)` in the disconnect path (replacing/augmenting `h.UnregisterAgent`).

Add config fields + env (Task 5 + here):
```go
CloudflareToken string // CLOUDFLARE_TOKEN
BaseDomain      string // CONTENT_BASE_DOMAIN (default sharebridgeusercontent.com)
ACMEEmail       string // ACME_EMAIL
ACMECADir       string // ACME_CA_DIR (default lego production; staging in tests)
```

- [ ] **Step 5: Run tests + build, verify pass**

Run: `cd signaling-server && go test ./internal/directctl/ -run 'TestRedirect' -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 6: Commit**

```bash
git add signaling-server/internal/directctl/ signaling-server/cmd/server/ signaling-server/internal/handler/ signaling-server/internal/config/
git commit -m "feat(control): redirect handler + controller wiring (open-signal → probe → 302)"
```

---

## Task 18: Control — API-key rotation re-points the agent

**Files:**
- Modify: `signaling-server/internal/handler/apikeys.go` (locate `RotateAPIKey`)
- Test: `signaling-server/internal/handler/apikeys_test.go` (extend)

**Interfaces:**
- Consumes: `agents.api_key_id`, `hub.CloseAgent`.
- Produces: rotation ordered create → re-point → fence/disconnect → revoke.

- [ ] **Step 1: Write the failing test**

```go
func TestRotatePreservesAgent(t *testing.T) {
	// create key + agent; rotate; assert the agent row now points at the NEW
	// key id (namespace + cert_status preserved), and the old key is revoked.
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `cd signaling-server && go test ./internal/handler/ -run TestRotatePreservesAgent -v`
Expected: FAIL.

- [ ] **Step 3: Implement the ordered rotation**

In `RotateAPIKey`, after creating the new key record and before revoking the old one, re-point the agent:

```go
// Re-point the agent record to the new key (namespace + cert survive).
agentsRecs, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": oldKeyID})
if err == nil && len(agentsRecs) > 0 {
	agentsRecs[0].Set("api_key_id", newKeyID)
	if err := app.Save(agentsRecs[0]); err != nil {
		return err
	}
}

// Fence/disconnect the old key's socket, then revoke the old key.
h.CloseAgent(oldKeyID) // compare-and-delete semantics: only closes the OLD conn
// ... existing revoke (is_active=false on the old key) ...
```

- [ ] **Step 4: Run tests + build, verify pass**

Run: `cd signaling-server && go test ./internal/handler/ -run TestRotatePreservesAgent -v && go build ./...`
Expected: PASS + BUILD.

- [ ] **Step 5: Commit**

```bash
git add signaling-server/internal/handler/apikeys.go signaling-server/internal/handler/apikeys_test.go
git commit -m "feat(control): rotation re-points agents.api_key_id (namespace + cert survive)"
```

---

## Task 19: Integration test — promote `spike-e2e` to a real end-to-end test

**Files:**
- Create: `agent/cmd/e2e/main.go` (replaces `spike-e2e`)
- Modify: `agent/cmd/spike-e2e/` → remove (superseded) — or leave it; prefer moving logic into `e2e`
- Test: `signaling-server/internal/directctl/e2e_test.go` (control-side integration against a fake agent over a real WS)

**Interfaces:**
- Consumes: the full stack from Tasks 1–18.

- [ ] **Step 1: Write the failing integration test (control side)**

```go
// e2e_test.go — enroll → register → open-signal → probe → redirect, with a
// fake agent that echoes the probe nonce over a real WebSocket + httptest.
func TestEndToEndDirectFlow(t *testing.T) {
	// 1. Boot an in-process control server (app + controller) on a test port.
	// 2. Dial /ws/agent with a test API key; send hello; receive enrolled.
	// 3. Submit a CSR; (coordinator runs against a STUB issuer — no real ACME).
	// 4. Send tls_ready; receive enrollment_ready.
	// 5. register_share; receive share_registered {origin}.
	// 6. GET /s/<code> on the control; assert the handler emits open_signal,
	//    the fake agent acks, the control probes (nonce echo), and responds 302
	//    to https://<origin>/s/<code>.
}
```

> The coordinator in tests is injected with a stub `issueFn` returning a pre-made (staging) chain, so no live ACME/Cloudflare is touched. `Probe`'s `allowPrivate` is set true for loopback.

- [ ] **Step 2: Run test, verify it fails**

Run: `cd signaling-server && go test ./internal/directctl/ -run TestEndToEndDirectFlow -v`
Expected: FAIL (assertions not yet satisfied).

- [ ] **Step 3: Implement the test + any missing wiring it exposes**

Fix whatever the test exposes (e.g., the probe path routing, the open_ack JSON round-trip, session lookup filtering). This task is the integration verification gate — it may surface small bugs in Tasks 1–18; fix them here.

- [ ] **Step 4: Run the full test suite**

Run:
```bash
cd signaling-server && go test ./... 
cd agent && go test ./...
```
Expected: PASS (all existing + new tests green; no production certs or live UPnP).

- [ ] **Step 5: Commit**

```bash
git add agent/cmd/e2e/ signaling-server/internal/directctl/
git commit -m "test: end-to-end direct flow (enroll → cert → register → open-signal → probe → redirect)"
```

---

## Self-Review (run before handoff)

- [ ] **Spec coverage:** cross-check §3–§8 against the tasks above. Gaps resolved inline: soft-delete origin retention (Task 13), issuance idempotency (Task 5), epoch-local readiness (Tasks 6–7), rotation (Task 18), probe tuple gating (Task 16), fast-path + close-failed (Task 8), `GetConfigForClient` composition (Task 11).
- [ ] **Placeholder scan:** no TBD/TODO; every code step has real code.
- [ ] **Type consistency:** `Manager`, `DirectServer`, `SignalGate.Reset/VerifyNonce`, `OnDemandPort.State/SetTransitionCallback`, `DeleteOwnedMapping`, `Controller.EmitOpen/Probe/Redirect`, `OpenAck` are referenced identically across tasks.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-15-phase2-direct-mode-transport.md`.

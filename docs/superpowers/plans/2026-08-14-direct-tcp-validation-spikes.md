# Direct-TCP Mode (v2) — Phase 1: Validation Spikes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove the four feasibility gates for the direct-TCP data plane before committing to the full build.

**Architecture:** Four independent spikes, each a small Go program plus a minimal testable unit, each ending in a written finding committed to `docs/superpowers/spikes/`. No production transport path is modified; the legacy WebRTC/relay data plane stays untouched.

**Tech Stack:** Go (agent and signaling-server are both Go), `github.com/huin/goupnp` (UPnP IGD), `github.com/jackpal/go-nat-pmp` (NAT-PMP), ACME DNS-01, the existing `agent/cmd/benchdirect` harness.

**Spec:** `docs/superpowers/specs/2026-08-14-direct-tcp-mode-design.md` (§13 validation list, §5 reachability, §5.1 on-demand port, §8 certificate).

## Global Constraints

- Zero manual router configuration — reachability is UPnP/NAT-PMP/PCP only (spec §5).
- The public port is closed by default and opened on demand; the switch is the UPnP mapping (spec §5.1).
- 443 is preferred but a non-standard port is acceptable (spec §6); never require the user to forward a port.
- The agent's TLS private key never leaves the agent; the control plane completes DNS-01 and returns only the public chain (spec §8).
- Strict origin→share binding: unknown SNI hostnames are rejected before serving (spec §11).
- All spike code lives under `agent/internal/direct`, `agent/cmd/spike-*`, or `signaling-server/internal/certcoordinator`; none of it is wired into `main`.

---

### Task 1: UPnP/NAT-PMP reachability spike

**Files:**
- Create: `agent/internal/direct/portmap.go`
- Create: `agent/internal/direct/portmap_test.go`
- Create: `agent/cmd/spike-upnp/main.go`
- Create: `docs/superpowers/spikes/2026-08-14-upnp-reachability.md`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `direct.PortMapper` interface and `direct.UPnPMapper` / `direct.NATPMPMapper` implementations, used by Task 4's `OnDemandPort`.

- [ ] **Step 1: Write the failing test for the PortMapper interface**

```go
// agent/internal/direct/portmap_test.go
package direct

import "testing"

type fakeMapper struct {
	externalIP string
	mapped     map[int]int
	err        error
}

func (f *fakeMapper) AddPortMapping(ext, internal int, desc string, lease int) error {
	if f.err != nil {
		return f.err
	}
	if f.mapped == nil {
		f.mapped = map[int]int{}
	}
	f.mapped[ext] = internal
	return nil
}
func (f *fakeMapper) DeletePortMapping(ext int) error {
	if f.mapped != nil {
		delete(f.mapped, ext)
	}
	return f.err
}
func (f *fakeMapper) ExternalIP() (string, error) { return f.externalIP, f.err }

func TestPortMapperRoundTrip(t *testing.T) {
	f := &fakeMapper{externalIP: "203.0.113.7"}
	var m PortMapper = f
	if err := m.AddPortMapping(8443, 8443, "sharebridge-direct", 60); err != nil {
		t.Fatalf("AddPortMapping: %v", err)
	}
	if f.mapped[8443] != 8443 {
		t.Fatalf("mapping not recorded: %v", f.mapped)
	}
	if ip, err := m.ExternalIP(); err != nil || ip != "203.0.113.7" {
		t.Fatalf("ExternalIP = %q, %v", ip, err)
	}
	if err := m.DeletePortMapping(8443); err != nil {
		t.Fatalf("DeletePortMapping: %v", err)
	}
	if _, ok := f.mapped[8443]; ok {
		t.Fatalf("mapping not removed: %v", f.mapped)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./internal/direct/ -run TestPortMapperRoundTrip -v`
Expected: FAIL — `undefined: PortMapper`.

- [ ] **Step 3: Write the PortMapper interface and real implementations**

```go
// agent/internal/direct/portmap.go
package direct

import (
	"context"
	"fmt"

	"github.com/huin/goupnp/dcps/internetgateway1"
	"github.com/jackpal/go-nat-pmp"
)

// PortMapper maps an external port on the router to the agent's internal address.
type PortMapper interface {
	AddPortMapping(externalPort, internalPort int, description string, leaseSeconds int) error
	DeletePortMapping(externalPort int) error
	ExternalIP() (string, error)
}

// UPnPMapper maps ports using UPnP IGD (internetgateway1).
type UPnPMapper struct {
	client *internetgateway1.WANIPConnection1
}

func NewUPnPMapper(ctx context.Context) (*UPnPMapper, error) {
	clients, _, err := internetgateway1.NewWANIPConnection1Clients()
	if err != nil || len(clients) == 0 {
		return nil, fmt.Errorf("no UPnP IGD gateway found: %v", err)
	}
	return &UPnPMapper{client: clients[0]}, nil
}

func (m *UPnPMapper) AddPortMapping(ext, internal int, desc string, lease int) error {
	return m.client.AddPortMapping("", uint16(ext), "TCP", uint16(internal), "0.0.0.0", true, desc, uint32(lease))
}
func (m *UPnPMapper) DeletePortMapping(ext int) error { return m.client.DeletePortMapping("", uint16(ext), "TCP") }
func (m *UPnPMapper) ExternalIP() (string, error)    { return m.client.GetExternalIPAddress() }

// NATPMPMapper maps ports using NAT-PMP (Apple/older routers).
type NATPMPMapper struct {
	client *natpmp.Client
}

func NewNATPMPMapper(gatewayIP string) *NATPMPMapper {
	return &NATPMPMapper{client: natpmp.NewClient(gatewayIP)}
}

func (m *NATPMPMapper) AddPortMapping(ext, internal int, desc string, lease int) error {
	_, err := m.client.AddPortMapping("tcp", internal, ext, lease)
	return err
}
func (m *NATPMPMapper) DeletePortMapping(ext int) error { return nil } // NAT-PMP mappings expire on their own
func (m *NATPMPMapper) ExternalIP() (string, error) {
	resp, err := m.client.GetExternalAddress()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d.%d.%d.%d", resp.ExternalIPAddress[0], resp.ExternalIPAddress[1], resp.ExternalIPAddress[2], resp.ExternalIPAddress[3]), nil
}

// MapperForRouter returns the first available mapper, trying UPnP then NAT-PMP.
// Gateway discovery (via github.com/jackpal/gateway) is a Phase 2 concern; the
// spike assumes a common gateway address for the NAT-PMP fallback.
func MapperForRouter(ctx context.Context) (PortMapper, error) {
	if m, err := NewUPnPMapper(ctx); err == nil {
		return m, nil
	}
	return NewNATPMPMapper("192.168.1.1"), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./internal/direct/ -run TestPortMapperRoundTrip -v`
Expected: PASS.

- [ ] **Step 5: Write the spike program**

```go
// agent/cmd/spike-upnp/main.go
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"sharebridge/agent/internal/direct"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mapper, err := direct.MapperForRouter(ctx)
	if err != nil {
		log.Fatalf("no mapper: %v", err)
	}
	ip, err := mapper.ExternalIP()
	if err != nil {
		log.Fatalf("external IP: %v", err)
	}
	ext := 8443
	if p := os.Getenv("SPIKE_EXT_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &ext)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", ext))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	if err := mapper.AddPortMapping(ext, ext, "sharebridge-spike", 300); err != nil {
		log.Fatalf("AddPortMapping: %v", err)
	}
	defer mapper.DeletePortMapping(ext)

	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "reachable via %s:%d\n", ip, ext)
	}))

	log.Printf("MAPPED external=%s:%d -> internal :%d (lease 300s). Probe me from outside this LAN.", ip, ext, ext)
	select {}
}
```

- [ ] **Step 6: Run the spike against a real router and self-probe**

Run: `cd agent && go run ./cmd/spike-upnp`
Then, from a host NOT on the same LAN (e.g. a phone on cellular), run: `curl http://<external-ip>:8443`
Expected: the response `reachable via <ip>:8443`. Record whether UPnP or NAT-PMP succeeded, and whether 443 (vs 8443) was available.

- [ ] **Step 7: Write the findings doc and commit**

Write `docs/superpowers/spikes/2026-08-14-upnp-reachability.md` with: which mapper succeeded, the observed external IP/port, whether 443 was taken, and the self-probe result. Then:

```bash
git add agent/internal/direct/ agent/cmd/spike-upnp/ docs/superpowers/spikes/
git commit -m "spike: prove UPnP/NAT-PMP reachability for direct mode"
```

---

### Task 2: Wildcard certificate + DDNS spike

**Files:**
- Create: `signaling-server/internal/certcoordinator/csr.go`
- Create: `signaling-server/internal/certcoordinator/csr_test.go`
- Create: `signaling-server/cmd/spike-cert/main.go`
- Create: `docs/superpowers/spikes/2026-08-14-cert-dns.md`

**Interfaces:**
- Consumes: nothing.
- Produces: `certcoordinator.GenerateWildcardCSR(namespace string) (keyPEM, csrPEM []byte, err error)` and `certcoordinator.ValidateSAN(certPEM []byte, namespace string) error`, reused by Phase 2's real cert coordinator.

- [ ] **Step 1: Write the failing test for CSR generation + SAN validation**

```go
// signaling-server/internal/certcoordinator/csr_test.go
package certcoordinator

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestGenerateWildcardCSR_SAN(t *testing.T) {
	_, csrPEM, err := GenerateWildcardCSR("v7q4km2x9pz6dn3w")
	if err != nil {
		t.Fatalf("GenerateWildcardCSR: %v", err)
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		t.Fatalf("no PEM block in CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	want := "*.v7q4km2x9pz6dn3w.sharebridgeusercontent.com"
	if len(csr.DNSNames) != 1 || csr.DNSNames[0] != want {
		t.Fatalf("DNSNames = %v, want [%s]", csr.DNSNames, want)
	}
	if !strings.Contains(csr.Subject.CommonName, want) && csr.Subject.CommonName != want {
		t.Fatalf("CommonName = %q, want %q", csr.Subject.CommonName, want)
	}
}

func TestValidateSAN(t *testing.T) {
	// A self-signed cert with the expected wildcard SAN, generated in-test.
	certPEM, err := testSelfSignedCert("*.v7q4km2x9pz6dn3w.sharebridgeusercontent.com")
	if err != nil {
		t.Fatalf("self-signed: %v", err)
	}
	if err := ValidateSAN(certPEM, "v7q4km2x9pz6dn3w"); err != nil {
		t.Fatalf("ValidateSAN: %v", err)
	}
	if err := ValidateSAN(certPEM, "other-namespace"); err == nil {
		t.Fatalf("ValidateSAN accepted the wrong namespace")
	}
}

// testSelfSignedCert builds a throwaway self-signed certificate used ONLY to
// exercise ValidateSAN in tests. It is never served to a browser; the real
// certificate is issued by a public ACME CA (Let's Encrypt / Google / ZeroSSL).
func testSelfSignedCert(cn string) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd signaling-server && go test ./internal/certcoordinator/ -v`
Expected: FAIL — `undefined: GenerateWildcardCSR`.

- [ ] **Step 3: Implement CSR generation and SAN validation**

```go
// signaling-server/internal/certcoordinator/csr.go
package certcoordinator

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
)

// GenerateWildcardCSR generates an agent-held ECDSA P-256 key and a wildcard
// CSR for *.<namespace>.sharebridgeusercontent.com. The private key never
// leaves the agent; only the CSR (and later the cert chain) cross the wire.
func GenerateWildcardCSR(namespace string) (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	cn := fmt.Sprintf("*.%s.sharebridgeusercontent.com", namespace)
	tmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: cn},
		DNSNames: []string{cn},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// ValidateSAN checks a returned certificate chain's leaf for the exact
// wildcard SAN and matching public key namespace before the agent installs it.
func ValidateSAN(certPEM []byte, namespace string) error {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("no PEM block in certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	want := fmt.Sprintf("*.%s.sharebridgeusercontent.com", namespace)
	for _, n := range cert.DNSNames {
		if n == want {
			return nil
		}
	}
	return fmt.Errorf("certificate SAN does not include %q", want)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd signaling-server && go test ./internal/certcoordinator/ -v`
Expected: PASS.

- [ ] **Step 5: Write the ACME DNS-01 spike program**

`signaling-server/cmd/spike-cert/main.go`:
1. Read `DNS_PROVIDER` + provider credentials from env (lego abstracts the provider — see https://go-acme.github.io/lego/dns/).
2. Call `GenerateWildcardCSR(namespace)`.
3. Use `github.com/go-acme/lego/v4` to run an ACME DNS-01 order for `*.<namespace>.sharebridgeusercontent.com`, placing and removing the `_acme-challenge` TXT record via the configured DNS provider.
4. Write the returned chain to `cert.pem`, then call `ValidateSAN` on it.

- [ ] **Step 6: Run the spike against a real domain + CA**

Run: `cd signaling-server && go run ./cmd/spike-cert` with a real (or test) `sharebridgeusercontent.com` subdomain + DNS credentials.
Expected: a valid wildcard certificate issued and `ValidateSAN` passes. Record the CA used, issuance latency, and any rate-limit/terms surprises.

CA choice: start with **Let's Encrypt** (simplest, free, no account); its 50-cert/week/domain limit only bounds agent-enrollment rate. Google Public CA (100 orders/hour) and ZeroSSL (unlimited) remain the scale candidates behind the provider abstraction (spec §8, FRP §9.4) — no need to decide scale now.

- [ ] **Step 7: Write the findings doc and commit**

Write `docs/superpowers/spikes/2026-08-14-cert-dns.md` (CA chosen, DNS provider, nested-wildcard behavior, renewal plan). Then commit with message `spike: prove wildcard cert issuance + DDNS for direct mode`.

---

### Task 3: Direct-TCP throughput spike

**Files:**
- Modify: `agent/cmd/benchdirect/server.go` (add an HTTPS file-serving mode)
- Create: `docs/superpowers/spikes/2026-08-14-direct-tcp-throughput.md`

**Interfaces:**
- Consumes: the existing benchdirect `--rtt`/`--bandwidth`/`--size` flags.
- Produces: a written throughput finding comparing direct TCP (HTTPS) vs relay at RTT ≥ 50 ms. No production code.

- [ ] **Step 1: Add an HTTPS file-serving mode to benchdirect**

Extend `agent/cmd/benchdirect/server.go` with a `--https` mode that serves a fixed-size file over `net/http` with `http.ServeContent` (so `Range` works) on a plain TCP listener, and a `--rtt`-shaped connection (reuse the existing shim's latency/jitter model).

- [ ] **Step 2: Add a Go HTTP client mode that downloads the file and reports throughput**

Add a `--fetch` mode that performs a ranged/sequential GET with the same RTT shaping, measuring bytes/sec over 100 ms windows (reuse the trace instrumentation already in `bench.js`/the shim).

- [ ] **Step 3: Run direct-TCP vs relay at RTT ≥ 50 ms**

Run the HTTPS mode with `--rtt 100 --size 1024` (1 GiB) and `--bandwidth` set to the owner's real upstream (e.g. 25 MB/s), then run the existing relay-mode benchmark at the same settings.
Expected: direct-TCP ≥ relay throughput, with no SCTP-collapse signature (no multi-second stalls).

- [ ] **Step 4: Write findings and commit**

Write `docs/superpowers/spikes/2026-08-14-direct-tcp-throughput.md` (numbers, median + range, ≥12 reps). Commit with message `spike: measure direct-TCP vs relay throughput`.

---

### Task 4: On-demand port + SNI binding prototype

**Files:**
- Create: `agent/internal/direct/ondemand.go`
- Create: `agent/internal/direct/ondemand_test.go`
- Create: `agent/internal/direct/sni.go`
- Create: `agent/internal/direct/sni_test.go`
- Create: `docs/superpowers/spikes/2026-08-14-ondemand-sni.md`

**Interfaces:**
- Consumes: `direct.PortMapper` (Task 1).
- Produces: `direct.OnDemandPort` and `direct.Binder`, reused by Phase 2's agent HTTPS server.

- [ ] **Step 1: Write the failing test for the on-demand state machine**

```go
// agent/internal/direct/ondemand_test.go
package direct

import (
	"testing"
	"time"
)

type recordingMapper struct{ opened, closed int }

func (r *recordingMapper) AddPortMapping(ext, internal int, desc string, lease int) error {
	r.opened++
	return nil
}
func (r *recordingMapper) DeletePortMapping(ext int) error { r.closed++; return nil }
func (r *recordingMapper) ExternalIP() (string, error)    { return "203.0.113.7", nil }

func TestOnDemandPort_ClosedByDefaultAndLease(t *testing.T) {
	rm := &recordingMapper{}
	p := NewOnDemandPort(rm, 8443)
	if p.Open() {
		t.Fatalf("port should be closed by default")
	}
	if err := p.OpenFor("share-1", 1*time.Second); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !p.Open() {
		t.Fatalf("port should be open after OpenFor")
	}
	if rm.opened != 1 {
		t.Fatalf("opened = %d, want 1", rm.opened)
	}
	// Lease expiry without activity closes the port.
	if err := p.OpenFor("share-1", 1*time.Millisecond); err != nil {
		t.Fatalf("OpenFor short lease: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if p.Open() {
		t.Fatalf("port should have closed after lease expiry")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./internal/direct/ -run TestOnDemandPort -v`
Expected: FAIL — `undefined: OnDemandPort`.

- [ ] **Step 3: Implement the on-demand port state machine**

```go
// agent/internal/direct/ondemand.go
package direct

import (
	"sync"
	"time"
)

// OnDemandPort manages a public port that is closed by default and opened
// only for an active, agent-verified share, closing on lease expiry or
// inactivity. The switch is the PortMapper mapping, not a local listener.
type OnDemandPort struct {
	mu       sync.Mutex
	mapper   PortMapper
	extPort  int
	open     bool
	deadline time.Time
	stop     chan struct{}
}

func NewOnDemandPort(mapper PortMapper, extPort int) *OnDemandPort {
	return &OnDemandPort{mapper: mapper, extPort: extPort, stop: make(chan struct{})}
}

// Open reports whether the port is currently mapped.
func (p *OnDemandPort) Open() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.open && time.Now().Before(p.deadline)
}

// OpenFor maps the port for shareID with the given lease. Only shares the
// agent has independently registered should ever be passed here; the caller
// (the control-plane signal handler) enforces that, not this type.
func (p *OnDemandPort) OpenFor(shareID string, lease time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.mapper.AddPortMapping(p.extPort, p.extPort, "sharebridge-"+shareID, int(lease.Seconds())); err != nil {
		return err
	}
	p.open = true
	p.deadline = time.Now().Add(lease)
	go p.expireAfter(lease)
	return nil
}

func (p *OnDemandPort) expireAfter(d time.Duration) {
	select {
	case <-time.After(d):
		p.Close()
	case <-p.stop:
	}
}

// Close removes the mapping immediately (lockdown or lease expiry).
func (p *OnDemandPort) Close() {
	p.mu.Lock()
	if p.open {
		_ = p.mapper.DeletePortMapping(p.extPort)
		p.open = false
	}
	p.mu.Unlock()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./internal/direct/ -run TestOnDemandPort -v`
Expected: PASS.

- [ ] **Step 5: Write the failing test for SNI binding**

```go
// agent/internal/direct/sni_test.go
package direct

import "testing"

func TestBinder_RejectsUnknownSNI(t *testing.T) {
	b := NewBinder()
	b.Allow("r7k2m9p4x6.v7q4km2x9pz6dn3w.sharebridgeusercontent.com")
	if !b.Validate("r7k2m9p4x6.v7q4km2x9pz6dn3w.sharebridgeusercontent.com") {
		t.Fatalf("known origin rejected")
	}
	if b.Validate("other.v7q4km2x9pz6dn3w.sharebridgeusercontent.com") {
		t.Fatalf("unknown child under known namespace allowed")
	}
	if b.Validate("r7k2m9p4x6.v7q4km2x9pz6dn3w.sharebridgeusercontent.com.evil.example") {
		t.Fatalf("suffix-spoofed hostname allowed")
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `cd agent && go test ./internal/direct/ -run TestBinder -v`
Expected: FAIL — `undefined: Binder`.

- [ ] **Step 7: Implement SNI binding**

```go
// agent/internal/direct/sni.go
package direct

import "sync"

// Binder holds the set of exact active share-origin hostnames. Only exact
// matches pass; wildcard children and suffix tricks are rejected.
type Binder struct {
	mu     sync.RWMutex
	active map[string]struct{}
}

func NewBinder() *Binder { return &Binder{active: map[string]struct{}{}} }

func (b *Binder) Allow(host string) {
	b.mu.Lock()
	b.active[host] = struct{}{}
	b.mu.Unlock()
}

func (b *Binder) Revoke(host string) {
	b.mu.Lock()
	delete(b.active, host)
	b.mu.Unlock()
}

// Validate reports whether host is an exact active origin.
func (b *Binder) Validate(host string) bool {
	b.mu.RLock()
	_, ok := b.active[host]
	b.mu.RUnlock()
	return ok
}
```

- [ ] **Step 8: Run test to verify it passes**

Run: `cd agent && go test ./internal/direct/ -run TestBinder -v`
Expected: PASS.

- [ ] **Step 9: Write findings and commit**

Write `docs/superpowers/spikes/2026-08-14-ondemand-sni.md` summarizing the closed-by-default + on-demand + SNI-binding behavior, and note the strict-open-signal rule (the signal handler must only call `OpenFor` with agent-registered shares). Commit with message `spike: on-demand port + SNI binding prototype`.

---

## Phase Gate

After all four tasks, the spikes together answer whether direct-TCP is viable:

- Task 1 → UPnP/NAT-PMP reachability works (and how often).
- Task 2 → wildcard cert + DDNS issuance works with the chosen CA/DNS.
- Task 3 → direct-TCP matches or exceeds relay throughput (no SCTP collapse).
- Task 4 → closed-by-default + on-demand + SNI binding behave as designed.

Any blocker here (per spec §13: "Failure of a certificate or raw-TLS spike is a design blocker") returns to the spec before Phase 2. Otherwise Phase 2 (agent HTTPS server + direct wiring + Immich migration) gets its own plan.

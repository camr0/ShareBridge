# Direct-TCP Mode (v2) — Phase 1: Validation Spikes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove the four feasibility gates for the direct-TCP data plane before committing to the full build.

**Architecture:** Four spikes, each a small Go program plus a minimal testable unit, each ending in a written finding committed to `docs/superpowers/spikes/`. Tasks 1–3 are independent; Task 4 depends on Task 1's `PortMapper`. No production transport path is modified; the legacy WebRTC/relay data plane stays untouched.

**Tech Stack:** Go (agent and signaling-server are both Go), `github.com/huin/goupnp` (UPnP IGD), `github.com/jackpal/go-nat-pmp` (NAT-PMP), `github.com/jackpal/gateway` (gateway discovery), `github.com/go-acme/lego/v4` (ACME DNS-01), `github.com/cloudflare/cloudflare-go` (DDNS), the existing `agent/cmd/benchdirect` harness.

**Spec:** `docs/superpowers/specs/2026-08-14-direct-tcp-mode-design.md` (§13 validation list, §5 reachability, §5.1 on-demand port, §6 port strategy, §7 DNS/DDNS, §8 certificate).

## Global Constraints

- Zero manual router configuration — reachability is UPnP/NAT-PMP only (PCP is future work; spec §5).
- The public port is closed by default and opened on demand; the switch is the UPnP mapping (spec §5.1).
- 443 is preferred but a non-standard port is acceptable (spec §6); never require the user to forward a port; never clobber an existing mapping this agent didn't create (spec §6).
- The agent's TLS private key never leaves the agent; the control plane completes DNS-01 and returns only the public chain (spec §8).
- Strict origin→share binding: unknown SNI hostnames are rejected before serving (spec §11).
- All spike code lives under `agent/internal/direct`, `agent/internal/cert`, `agent/cmd/spike-*`, `signaling-server/internal/certcoordinator`, `signaling-server/internal/ddns`, or `signaling-server/cmd/spike-*`; none of it is wired into `main`.

---

## Phase 0 — Manual Smoke Test (optional, no LE/CF/domain)

Before investing in certificates, DNS, or the control plane, prove the core transport with a throwaway self-signed cert on your own machine. This answers "does the agent even become reachable over HTTPS via UPnP?" in an afternoon.

- [ ] **Step 1:** Run the Task 1 spike in HTTPS mode: `go run ./cmd/spike-upnp -https -ext-port 443`. The spike maps an external port **and serves HTTPS on that mapped port itself** (self-signed cert, smoke-test only). There is no separate server to stand up — the listener and the mapping live in the same process, so the earlier "stand up a separate HTTPS server on the same port" port-conflict is avoided.
- [ ] **Step 2:** From a phone on cellular (off the LAN), browse to `https://<your-public-ip>:<granted-port>` — accept the self-signed warning — and download a file (the spike serves `/file`).
- [ ] **Step 3:** Record the result. Success proves UPnP/NAT-PMP reachability + direct TCP + TLS-at-agent end-to-end with zero external infrastructure. Failure (port unreachable, ISP blocks inbound) means direct mode leans on relay, and we learn that before building anything else.

This is deliberately crude: self-signed TLS, no control plane, no cert automation. Its only job is to confirm the transport works before we spend anything on LE/CF/domain. Note the external probe here is a human on a phone (off-LAN, so it is not a hairpin self-probe); in production this probe is the control plane's job — see the findings doc in Task 1 Step 7.

---

### Task 1: UPnP/NAT-PMP reachability spike

**Files:**
- Create: `agent/internal/direct/portmap.go`
- Create: `agent/internal/direct/portmap_test.go`
- Create: `agent/cmd/spike-upnp/main.go`
- Create: `docs/superpowers/spikes/2026-08-14-upnp-reachability.md`
- Modify: `agent/go.mod` / `agent/go.sum` — add `github.com/huin/goupnp` (UPnP IGD), `github.com/jackpal/go-nat-pmp` (NAT-PMP), and `github.com/jackpal/gateway` (gateway discovery) via `go get`.

**Interfaces:**
- Consumes: `github.com/huin/goupnp` (UPnP IGD), `github.com/jackpal/go-nat-pmp` (NAT-PMP), `github.com/jackpal/gateway` (gateway IP discovery).
- Produces: `direct.PortMapper` (reports the *granted* external port and can enumerate existing mappings) and its `direct.UPnPMapper` / `direct.NATPMPMapper` implementations, plus the spec-§6 helpers `direct.ChooseExternalPort` and `direct.DeleteOwnedMapping` and the `direct.PortMapping` type. Used by Task 4's `OnDemandPort`.

- [ ] **Step 1: Write the failing tests for the PortMapper contract**

```go
// agent/internal/direct/portmap_test.go
package direct

import "testing"

// fakeMapper implements PortMapper without any network, letting us assert the
// contract (granted-port reporting, enumeration, ownership) in-process.
type fakeMapper struct {
	externalIP string
	granted    int // non-zero forces AddPortMapping to return this external port
	mappings   map[int]PortMapping
	listErr    error
	err        error
	deleted    []int
}

func (f *fakeMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	if f.mappings == nil {
		f.mappings = map[int]PortMapping{}
	}
	granted := ext
	if f.granted != 0 {
		granted = f.granted
	}
	f.mappings[granted] = PortMapping{
		ExternalPort:   granted,
		InternalPort:   internal,
		InternalClient: "192.168.1.20",
		Protocol:       "TCP",
		Description:    desc,
	}
	return granted, nil
}

func (f *fakeMapper) DeletePortMapping(ext int) error {
	if f.err != nil {
		return f.err
	}
	delete(f.mappings, ext)
	f.deleted = append(f.deleted, ext)
	return nil
}

func (f *fakeMapper) ExternalIP() (string, error) { return f.externalIP, f.err }

func (f *fakeMapper) ListPortMappings() ([]PortMapping, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]PortMapping, 0, len(f.mappings))
	for _, m := range f.mappings {
		out = append(out, m)
	}
	return out, nil
}

func TestPortMapperRoundTrip(t *testing.T) {
	f := &fakeMapper{externalIP: "203.0.113.7"}
	var m PortMapper = f

	granted, err := m.AddPortMapping(443, 8443, "sharebridge-test", 60)
	if err != nil {
		t.Fatalf("AddPortMapping: %v", err)
	}
	if granted != 443 {
		t.Fatalf("granted = %d, want 443", granted)
	}
	if f.mappings[443].InternalPort != 8443 {
		t.Fatalf("mapping not recorded: %v", f.mappings)
	}

	if ip, err := m.ExternalIP(); err != nil || ip != "203.0.113.7" {
		t.Fatalf("ExternalIP = %q, %v", ip, err)
	}
	if err := m.DeletePortMapping(443); err != nil {
		t.Fatalf("DeletePortMapping: %v", err)
	}
	if _, ok := f.mappings[443]; ok {
		t.Fatalf("mapping not removed: %v", f.mappings)
	}
}

// NAT-PMP may grant a different external port than requested; the mapper must
// report the granted port so the spike can advertise the real endpoint.
func TestPortMapper_ReportsGrantedPort(t *testing.T) {
	f := &fakeMapper{externalIP: "203.0.113.7", granted: 52000}
	var m PortMapper = f

	granted, err := m.AddPortMapping(443, 8443, "sharebridge-test", 60)
	if err != nil {
		t.Fatalf("AddPortMapping: %v", err)
	}
	if granted != 52000 {
		t.Fatalf("granted = %d, want 52000", granted)
	}
	if _, ok := f.mappings[52000]; !ok {
		t.Fatalf("mapping not recorded under granted port: %v", f.mappings)
	}
}

// Spec §6: never clobber an existing mapping — prefer 443 when free, fall back
// to a high dynamic-range port when 443 is occupied.
func TestChooseExternalPort_PrefersFree443(t *testing.T) {
	f := &fakeMapper{}
	got, err := ChooseExternalPort(f, 443)
	if err != nil {
		t.Fatalf("ChooseExternalPort: %v", err)
	}
	if got != 443 {
		t.Fatalf("got %d, want 443 when free", got)
	}
}

func TestChooseExternalPort_FallsBackWhen443Taken(t *testing.T) {
	f := &fakeMapper{mappings: map[int]PortMapping{
		443: {ExternalPort: 443, Description: "existing-nginx"},
	}}
	got, err := ChooseExternalPort(f, 443)
	if err != nil {
		t.Fatalf("ChooseExternalPort: %v", err)
	}
	if got == 443 {
		t.Fatalf("must not choose occupied 443")
	}
	if got < 49152 || got > 65535 {
		t.Fatalf("fallback %d outside dynamic range 49152-65535", got)
	}
}

// Spec §6: never delete a mapping this agent didn't create.
func TestDeleteOwnedMapping_RefusesForeign(t *testing.T) {
	f := &fakeMapper{mappings: map[int]PortMapping{
		443: {ExternalPort: 443, Description: "existing-nginx"},
	}}
	if err := DeleteOwnedMapping(f, 443); err != ErrForeignMapping {
		t.Fatalf("DeleteOwnedMapping = %v, want ErrForeignMapping", err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("foreign mapping was deleted: %v", f.deleted)
	}
	if _, ok := f.mappings[443]; !ok {
		t.Fatalf("foreign mapping disappeared: %v", f.mappings)
	}
}

func TestDeleteOwnedMapping_DeletesOwn(t *testing.T) {
	f := &fakeMapper{mappings: map[int]PortMapping{
		443: {ExternalPort: 443, Description: "sharebridge-test"},
	}}
	if err := DeleteOwnedMapping(f, 443); err != nil {
		t.Fatalf("DeleteOwnedMapping: %v", err)
	}
	if _, ok := f.mappings[443]; ok {
		t.Fatalf("own mapping not removed: %v", f.mappings)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && go test ./internal/direct/ -run 'TestPortMapper|TestChooseExternalPort|TestDeleteOwnedMapping' -v`
Expected: FAIL — compile error `undefined: PortMapper` (plus `PortMapping`, `ChooseExternalPort`, `DeleteOwnedMapping`, `ErrForeignMapping`, and the other symbols not yet produced).

- [ ] **Step 3: Implement the PortMapper interface, real mappers, and spec-§6 helpers**

```go
// agent/internal/direct/portmap.go
package direct

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/huin/goupnp/dcps/internetgateway1"
	"github.com/jackpal/gateway"
	"github.com/jackpal/go-nat-pmp"
)

// DescriptionPrefix marks mappings this agent created, so it never deletes or
// overwrites a mapping owned by another service (spec §6).
const DescriptionPrefix = "sharebridge"

var (
	// ErrListingUnsupported is returned by ListPortMappings for devices
	// (NAT-PMP) that cannot enumerate their mappings.
	ErrListingUnsupported = errors.New("port mapping listing not supported")
	// ErrForeignMapping is returned when refusing to delete a mapping this
	// agent did not create.
	ErrForeignMapping = errors.New("refusing to delete a port mapping this agent did not create")
)

// PortMapping is one existing mapping on the router.
type PortMapping struct {
	ExternalPort   int
	InternalPort   int
	InternalClient string
	Protocol       string
	Description    string
}

// PortMapper maps an external port on the router to the agent's internal
// address. AddPortMapping returns the external port actually granted, which
// may differ from the requested one (NAT-PMP may remap).
type PortMapper interface {
	AddPortMapping(externalPort, internalPort int, description string, leaseSeconds int) (int, error)
	DeletePortMapping(externalPort int) error
	ExternalIP() (string, error)
	ListPortMappings() ([]PortMapping, error)
}

// upnpConnection abstracts WANIPConnection1 and WANPPPConnection1, which have
// identical method sets but distinct generated types.
type upnpConnection interface {
	AddPortMapping(NewRemoteHost string, NewExternalPort uint16, NewProtocol string, NewInternalPort uint16, NewInternalClient string, NewEnabled bool, NewPortMappingDescription string, NewLeaseDuration uint32) error
	DeletePortMapping(NewRemoteHost string, NewExternalPort uint16, NewProtocol string) error
	GetExternalIPAddress() (string, error)
	GetGenericPortMappingEntry(NewPortMappingIndex uint16) (NewRemoteHost string, NewExternalPort uint16, NewProtocol string, NewInternalPort uint16, NewInternalClient string, NewEnabled bool, NewPortMappingDescription string, NewLeaseDuration uint32, err error)
}

// UPnPMapper maps ports using UPnP IGD (WANIPConnection1 or WANPPPConnection1).
type UPnPMapper struct {
	client     upnpConnection
	internalIP string
}

func NewUPnPMapper(ctx context.Context) (*UPnPMapper, error) {
	client, err := discoverUPnPClient(ctx)
	if err != nil {
		return nil, err
	}
	internal, err := lanAddressTowardsGateway()
	if err != nil {
		return nil, fmt.Errorf("resolve LAN address toward gateway: %w", err)
	}
	return &UPnPMapper{client: client, internalIP: internal}, nil
}

// discoverUPnPClient tries WANIP then WANPPP (some routers expose only one).
func discoverUPnPClient(ctx context.Context) (upnpConnection, error) {
	if clients, _, err := internetgateway1.NewWANIPConnection1ClientsCtx(ctx); err == nil && len(clients) > 0 {
		return clients[0], nil
	}
	if clients, _, err := internetgateway1.NewWANPPPConnection1ClientsCtx(ctx); err == nil && len(clients) > 0 {
		return clients[0], nil
	}
	return nil, fmt.Errorf("no UPnP IGD gateway (WANIPConnection or WANPPPConnection) found")
}

func (m *UPnPMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	// The internal client is this host's real LAN address (not 0.0.0.0),
	// resolved toward the gateway so the router forwards to the right host.
	if err := m.client.AddPortMapping("", uint16(ext), "TCP", uint16(internal), m.internalIP, true, desc, uint32(lease)); err != nil {
		return 0, err
	}
	return ext, nil
}

func (m *UPnPMapper) DeletePortMapping(ext int) error {
	return m.client.DeletePortMapping("", uint16(ext), "TCP")
}

func (m *UPnPMapper) ExternalIP() (string, error) {
	return m.client.GetExternalIPAddress()
}

func (m *UPnPMapper) ListPortMappings() ([]PortMapping, error) {
	var out []PortMapping
	for i := 0; ; i++ {
		_, ext, proto, internal, client, _, desc, _, err := m.client.GetGenericPortMappingEntry(uint16(i))
		if err != nil {
			break // index past the last entry
		}
		out = append(out, PortMapping{
			ExternalPort:   int(ext),
			InternalPort:   int(internal),
			InternalClient: client,
			Protocol:       proto,
			Description:    desc,
		})
	}
	return out, nil
}

// NATPMPMapper maps ports using NAT-PMP (Apple/older routers).
type NATPMPMapper struct {
	client *natpmp.Client

	mu       sync.Mutex
	internal int // internal port of the active mapping (0 = none)
	external int // granted external port of the active mapping
}

func NewNATPMPMapper(gatewayIP net.IP) *NATPMPMapper {
	return &NATPMPMapper{client: natpmp.NewClient(gatewayIP)}
}

func (m *NATPMPMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	// The returned MappedExternalPort is the router's chosen port; it may
	// differ from the requested ext, so it must be retained and returned.
	result, err := m.client.AddPortMapping("tcp", internal, ext, lease)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	m.internal, m.external = internal, int(result.MappedExternalPort)
	m.mu.Unlock()
	return int(result.MappedExternalPort), nil
}

func (m *NATPMPMapper) DeletePortMapping(ext int) error {
	// NAT-PMP has no separate delete; re-issuing the mapping with a zero lease
	// removes it. `ext` is used only if no mapping was recorded by a prior
	// AddPortMapping.
	m.mu.Lock()
	internal, external := m.internal, m.external
	m.mu.Unlock()
	if external == 0 {
		external = ext
	}
	_, err := m.client.AddPortMapping("tcp", internal, external, 0)
	return err
}

func (m *NATPMPMapper) ExternalIP() (string, error) {
	resp, err := m.client.GetExternalAddress()
	if err != nil {
		return "", err
	}
	ip := resp.ExternalIPAddress
	return fmt.Sprintf("%d.%d.%d.%d", ip[0], ip[1], ip[2], ip[3]), nil
}

func (m *NATPMPMapper) ListPortMappings() ([]PortMapping, error) {
	return nil, ErrListingUnsupported
}

// MapperForRouter returns the first available mapper, trying UPnP then NAT-PMP.
// The NAT-PMP gateway address is discovered via github.com/jackpal/gateway,
// never hardcoded.
func MapperForRouter(ctx context.Context) (PortMapper, error) {
	if m, err := NewUPnPMapper(ctx); err == nil {
		return m, nil
	}
	gw, err := gateway.DiscoverGateway()
	if err != nil {
		return nil, fmt.Errorf("no UPnP IGD or NAT-PMP gateway found: %w", err)
	}
	return NewNATPMPMapper(gw), nil
}

// ChooseExternalPort picks the external port for a new mapping honoring spec
// §6: prefer `preferred` (443), but never clobber an existing mapping — fall
// back to the first free dynamic-range port when `preferred` is occupied.
// When listing is unsupported (NAT-PMP), 443 is treated as unsafe (assumed
// occupied) and any other preferred port is used as-is.
func ChooseExternalPort(mapper PortMapper, preferred int) (int, error) {
	mappings, err := mapper.ListPortMappings()
	switch {
	case errors.Is(err, ErrListingUnsupported):
		if preferred == 443 {
			return firstFreePort(nil), nil
		}
		return preferred, nil
	case err != nil:
		return 0, fmt.Errorf("list port mappings: %w", err)
	}
	if occupied(mappings, preferred) {
		p := firstFreePort(mappings)
		if p == 0 {
			return 0, fmt.Errorf("no free port in dynamic range 49152-65535")
		}
		return p, nil
	}
	return preferred, nil
}

// DeleteOwnedMapping removes a mapping only if this agent created it. Mappers
// without listing support (NAT-PMP) are deleted best-effort: their mappings
// expire on their own and we tracked the granted port ourselves.
func DeleteOwnedMapping(mapper PortMapper, externalPort int) error {
	mappings, err := mapper.ListPortMappings()
	switch {
	case errors.Is(err, ErrListingUnsupported):
		return mapper.DeletePortMapping(externalPort)
	case err != nil:
		return fmt.Errorf("list port mappings: %w", err)
	}
	for _, m := range mappings {
		if m.ExternalPort == externalPort && !strings.HasPrefix(m.Description, DescriptionPrefix) {
			return ErrForeignMapping
		}
	}
	return mapper.DeletePortMapping(externalPort)
}

// lanAddressTowardsGateway returns this host's IP on the interface that routes
// toward the default gateway — the address the router must forward the mapped
// port to (never 0.0.0.0, which some routers reject).
func lanAddressTowardsGateway() (string, error) {
	gw, err := gateway.DiscoverGateway()
	if err != nil {
		return "", err
	}
	return localAddrTowards(gw)
}

func localAddrTowards(dst net.IP) (string, error) {
	conn, err := net.Dial("udp", net.JoinHostPort(dst.String(), "9"))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	return host, err
}

func occupied(mappings []PortMapping, port int) bool {
	for _, m := range mappings {
		if m.ExternalPort == port {
			return true
		}
	}
	return false
}

// firstFreePort returns the first port in the IANA dynamic range (49152-65535)
// not present in mappings. A deterministic scan (rather than a random pick)
// keeps the spike testable; a random high port is equally valid in production.
func firstFreePort(mappings []PortMapping) int {
	for p := 49152; p <= 65535; p++ {
		if !occupied(mappings, p) {
			return p
		}
	}
	return 0
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent && go test ./internal/direct/ -run 'TestPortMapper|TestChooseExternalPort|TestDeleteOwnedMapping' -v`
Expected: PASS.

- [ ] **Step 5: Write the spike program**

```go
// agent/cmd/spike-upnp/main.go
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sharebridge/agent/internal/direct"
)

func main() {
	var (
		internalPort = flag.Int("internal-port", 8443, "local port the spike listens on")
		extPort      = flag.Int("ext-port", 443, "preferred external port (falls back to a high port if occupied)")
		lease        = flag.Int("lease", 300, "port mapping lease in seconds")
		serveHTTPS   = flag.Bool("https", false, "serve HTTPS with a self-signed cert (smoke test only)")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mapper, err := direct.MapperForRouter(ctx)
	if err != nil {
		log.Fatalf("no mapper: %v", err)
	}
	ipStr, err := mapper.ExternalIP()
	if err != nil {
		log.Fatalf("external IP: %v", err)
	}

	// Spec §6: never clobber an existing (foreign) mapping; prefer 443, fall
	// back to a high dynamic-range port when 443 is taken.
	requested, err := direct.ChooseExternalPort(mapper, *extPort)
	if err != nil {
		log.Fatalf("choose external port: %v", err)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *internalPort))
	if err != nil {
		log.Fatalf("listen :%d: %v", *internalPort, err)
	}
	defer ln.Close()

	// NAT-PMP may grant a different external port than requested; the returned
	// value is the endpoint to advertise.
	granted, err := mapper.AddPortMapping(requested, *internalPort, "sharebridge-spike", *lease)
	if err != nil {
		log.Fatalf("AddPortMapping: %v", err)
	}
	log.Printf("MAPPED external=%s:%d -> local :%d (lease %ds)", ipStr, granted, *internalPort, *lease)

	// Remove the mapping on SIGINT/SIGTERM as well as on normal return, and
	// only if this agent created it (§6).
	defer func() {
		if err := direct.DeleteOwnedMapping(mapper, granted); err != nil {
			log.Printf("DeleteOwnedMapping(%d): %v", granted, err)
		}
	}()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/file" {
			// ~1 MiB of deterministic data, so the smoke test can prove a real
			// download (and later, that Range/throughput work).
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="spike.bin"`)
			w.Write(bytes.Repeat([]byte("sharebridge-spike\n"), 1<<16))
			return
		}
		fmt.Fprintf(w, "reachable via %s:%d\n", ipStr, granted)
	})

	var srv *http.Server
	if *serveHTTPS {
		cert, err := selfSignedCert(net.ParseIP(ipStr))
		if err != nil {
			log.Fatalf("self-signed cert: %v", err)
		}
		srv = &http.Server{
			Handler:   handler,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		}
		log.Printf("serving HTTPS (self-signed; smoke test only). Probe from off-LAN: https://%s:%d", ipStr, granted)
	} else {
		srv = &http.Server{Handler: handler}
		log.Printf("serving HTTP. Probe from off-LAN: http://%s:%d", ipStr, granted)
	}

	serveErr := make(chan error, 1)
	go func() {
		if *serveHTTPS {
			serveErr <- srv.ServeTLS(ln, "", "")
		} else {
			serveErr <- srv.Serve(ln)
		}
	}()

	select {
	case err := <-serveErr:
		log.Printf("serve: %v", err)
	case <-ctx.Done():
		log.Println("SIGINT/SIGTERM received; removing port mapping")
	}
}

// selfSignedCert builds a throwaway self-signed certificate for the public IP,
// used ONLY for the Phase 0 smoke test. It is never served to real users; the
// production certificate is issued by a public ACME CA (spec §8).
func selfSignedCert(ip net.IP) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: ip.String()},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{ip},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return tls.X509KeyPair(certPEM, keyPEM)
}
```

- [ ] **Step 6: Run the spike against a real router and self-probe from off-LAN**

Run: `cd agent && go run ./cmd/spike-upnp` (plain HTTP first, to isolate library/mapping from TLS), then from a host NOT on the same LAN (e.g. a phone on cellular): `curl http://<external-ip>:<granted-port>` and `curl http://<external-ip>:<granted-port>/file`.
Expected: the response `reachable via <ip>:<granted-port>` and a successful `/file` download. Record whether UPnP (WANIP or WANPPP) or NAT-PMP succeeded, the requested vs granted port, and whether 443 (vs the fallback high port) was available. **Crucially, record failures honestly: library discovery succeeding is not the same as the port being reachable from outside** (double NAT, CGNAT, hairpin, lease-lax routers). The HTTPS self-signed variant is exercised in Phase 0 via `-https`.

- [ ] **Step 7: Write the findings doc and commit**

Write `docs/superpowers/spikes/2026-08-14-upnp-reachability.md` with:

1. **Result:** which mapper succeeded (UPnP WANIP / WANPPP / NAT-PMP), the observed external IP, the requested external port, and the **granted** external port (they may differ for NAT-PMP).
2. **Port strategy:** whether 443 was free or fell back to a high port, and confirmation that no pre-existing foreign mapping was clobbered (spec §6).
3. **Off-LAN probe result:** reachable or not, from what vantage (cellular, not hairpin). Explicitly separate "library discovery succeeded" from "external probe reached the mapping" — a discovery success with a failed probe is recorded as a failure.
4. **Self-probe trust boundary (spec §5):** in production the external probe is performed by the **control plane, not the agent** (hairpin NAT makes a LAN self-probe unreliable). The control plane probes only the `publicIP:port` the agent just reported — restricted to public IPs that match the agent's own `GetExternalIPAddress` result — with a control-plane-generated nonce that the agent's HTTPS server echoes only for the share being opened, and probes are rate-limited per agent. This spike is single-process, so a real control-plane probe is out of scope; the phone-on-cellular check is the manual stand-in for that probe.
5. **Representative-router matrix:** define and fill in this minimum matrix, recording a pass/fail (and the failure mode) per cell rather than treating library discovery as reachability:

| Dimension | Values to cover |
|---|---|
| Vendor / model | ≥ 3 distinct vendors (e.g. ASUS, TP-Link, Ubiquiti, Fritz!Box, Eero) |
| Protocol | UPnP IGD v1, UPnP IGD v2, NAT-PMP |
| UPnP service flavor | WANIPConnection vs WANPPPConnection |
| External port state | 443 free, 443 occupied (foreign mapping present) |
| Lease behavior | strict lease expiry vs lease-lax routers that keep the mapping until reboot |
| NAT topology | single NAT, double NAT, CGNAT |
| Hairpin | hairpin NAT present vs absent (why the agent never self-probes from LAN) |

Then commit:

```bash
git add agent/internal/direct/ agent/cmd/spike-upnp/ agent/go.mod agent/go.sum docs/superpowers/spikes/
git commit -m "spike: prove UPnP/NAT-PMP reachability for direct mode"
```

---

### Task 2: Wildcard certificate + DDNS spike

> **Key custody (spec §8):** the agent generates and holds its TLS private key locally — it never leaves the agent. The agent submits a CSR for **both** wildcard SANs; the control plane's certificate coordinator completes DNS-01 ACME issuance and returns **only the public chain**. This task's code enforces that split: CSR/key generation and chain validation live in the agent, while CSR→chain completion lives in the control plane.

**Prerequisite (blocking):** the production content domain (`sharebridgeusercontent.com`) is **not yet purchased** (spec §15.1). This spike therefore runs against a **delegated test zone or a throwaway subdomain on an existing domain** (e.g. `sbx.example.com`, or `test.sharebridgeusercontent.com` once the domain exists). The base domain is a parameter (`baseDomain`) in every function and env var, so the spike needs no production domain. A Cloudflare API token scoped to that one zone with **DNS-edit only** (least privilege — no account-wide scope, no other permissions) is required. All credentials are supplied via env and are **never committed**.

**Files:**
- Create: `agent/internal/cert/csr.go`
- Create: `agent/internal/cert/csr_test.go`
- Create: `agent/internal/cert/validate.go`
- Create: `agent/internal/cert/validate_test.go`
- Create: `agent/cmd/spike-certagent/main.go` (agent half: generate key+CSR, validate the returned chain)
- Create: `signaling-server/internal/certcoordinator/acme.go`
- Create: `signaling-server/cmd/spike-cert/main.go` (control-plane half: CSR → chain via lego)
- Create: `signaling-server/internal/ddns/ddns.go`
- Create: `signaling-server/cmd/spike-ddns/main.go` (DDNS A-record create/update/delete + propagation)
- Create: `docs/superpowers/spikes/2026-08-14-cert-dns.md`

**Interfaces:**
- Consumes: `github.com/go-acme/lego/v4` (ACME DNS-01 via its built-in Cloudflare provider), `github.com/cloudflare/cloudflare-go` (DDNS A-record updates), `github.com/miekg/dns` (authoritative propagation probe). Nothing from other tasks.
- Produces (agent-side, reused by Phase 2):
  - `cert.GenerateWildcardCSR(namespace, baseDomain string) (keyPEM, csrPEM []byte, err error)` — generates the key **locally** and requests **both** wildcard SANs (`*.<namespace>.<baseDomain>` and `*.relay.<namespace>.<baseDomain>`); the key never leaves the agent.
  - `cert.ValidateChain(certChainPEM, keyPEM []byte, namespace, baseDomain string, roots *x509.CertPool) error` — full-chain validation (trust root, public-key equality with the locally-held key, exact SAN set, validity window, EKU/key usage).
- Produces (control-plane side, reused by Phase 2):
  - `certcoordinator.CompleteCSR(ctx context.Context, csrPEM []byte, cfg ACMEConfig) (chainPEM []byte, err error)` — accepts the agent's CSR and returns **only** the leaf-first chain. It never sees the agent's private key.
  - `ddns.Cloudflare` with `UpsertA` / `DeleteA` — direct-namespace wildcard A-record create/update/delete via `cloudflare-go`. (The relay namespace `*.relay.<namespace>.<baseDomain>` → static gateway IP is set once at enrollment and is **out of scope** for the DDNS test; only the direct A record is exercised here.)

- [ ] **Step 1: Write the failing tests for CSR generation and chain validation**

```go
// agent/internal/cert/csr_test.go
package cert

import (
	"crypto/x509"
	"encoding/pem"
	"sort"
	"testing"
)

// testBase is a throwaway subdomain on the reserved example.com domain; the
// spike runs against a delegated test zone before the production content
// domain is purchased (spec §15.1).
const testBase = "sbx.example.com"

func TestGenerateWildcardCSR_BothSANs(t *testing.T) {
	keyPEM, csrPEM, err := GenerateWildcardCSR("v7q4km2x9pz6dn3w", testBase)
	if err != nil {
		t.Fatalf("GenerateWildcardCSR: %v", err)
	}
	if len(keyPEM) == 0 {
		t.Fatalf("empty key PEM")
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("no CERTIFICATE REQUEST PEM block in CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	want := []string{
		"*.v7q4km2x9pz6dn3w.sbx.example.com",
		"*.relay.v7q4km2x9pz6dn3w.sbx.example.com",
	}
	got := append([]string(nil), csr.DNSNames...)
	sort.Strings(want)
	sort.Strings(got)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("DNSNames = %v, want %v", csr.DNSNames, want)
	}
}
```

```go
// agent/internal/cert/validate_test.go
package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// --- test helpers ---------------------------------------------------------
//
// These build throwaway test CA/leaf certs used ONLY to exercise ValidateChain.
// Production validation MUST reject self-signed, expired, and wrong-key chains;
// the negative tests below assert exactly that.

func makeTestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "spike-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return ca, caKey, pool
}

func signLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, leafKey *ecdsa.PrivateKey, template *x509.Certificate) []byte {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func leafTemplate(ns, base string, notBefore, notAfter time.Time) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "*.relay." + ns + "." + base},
		DNSNames:     wildcardSANs(ns, base),
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
}

func keyPEMFor(t *testing.T, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

// --- tests ----------------------------------------------------------------

func TestValidateChain_AcceptsValidChain(t *testing.T) {
	ca, caKey, roots := makeTestCA(t)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	chain := signLeaf(t, ca, caKey, leafKey, leafTemplate("v7q4km2x9pz6dn3w", testBase, now.Add(-time.Hour), now.Add(time.Hour)))
	if err := ValidateChain(chain, keyPEMFor(t, leafKey), "v7q4km2x9pz6dn3w", testBase, roots); err != nil {
		t.Fatalf("ValidateChain rejected a valid chain: %v", err)
	}
}

func TestValidateChain_RejectsSelfSigned(t *testing.T) {
	// Self-signed leaf with the exact SANs. Production roots = the ACME trust
	// root, not the leaf itself, so chain verification must fail.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	tmpl := leafTemplate("v7q4km2x9pz6dn3w", testBase, now.Add(-time.Hour), now.Add(time.Hour))
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	_, _, roots := makeTestCA(t) // a real CA, NOT the self-signed leaf
	if err := ValidateChain(chain, keyPEMFor(t, key), "v7q4km2x9pz6dn3w", testBase, roots); err == nil {
		t.Fatalf("ValidateChain accepted a self-signed leaf")
	}
}

func TestValidateChain_RejectsExpired(t *testing.T) {
	ca, caKey, roots := makeTestCA(t)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	chain := signLeaf(t, ca, caKey, leafKey, leafTemplate("v7q4km2x9pz6dn3w", testBase, now.Add(-2*time.Hour), now.Add(-time.Hour)))
	if err := ValidateChain(chain, keyPEMFor(t, leafKey), "v7q4km2x9pz6dn3w", testBase, roots); err == nil {
		t.Fatalf("ValidateChain accepted an expired leaf")
	}
}

func TestValidateChain_RejectsWrongKey(t *testing.T) {
	ca, caKey, roots := makeTestCA(t)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	otherKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	chain := signLeaf(t, ca, caKey, leafKey, leafTemplate("v7q4km2x9pz6dn3w", testBase, now.Add(-time.Hour), now.Add(time.Hour)))
	if err := ValidateChain(chain, keyPEMFor(t, otherKey), "v7q4km2x9pz6dn3w", testBase, roots); err == nil {
		t.Fatalf("ValidateChain accepted a chain whose leaf key differs from the local key")
	}
}

func TestValidateChain_RejectsExtraSAN(t *testing.T) {
	ca, caKey, roots := makeTestCA(t)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	tmpl := leafTemplate("v7q4km2x9pz6dn3w", testBase, now.Add(-time.Hour), now.Add(time.Hour))
	tmpl.DNSNames = append(tmpl.DNSNames, "*.evil.example")
	chain := signLeaf(t, ca, caKey, leafKey, tmpl)
	if err := ValidateChain(chain, keyPEMFor(t, leafKey), "v7q4km2x9pz6dn3w", testBase, roots); err == nil {
		t.Fatalf("ValidateChain accepted a leaf with an extra SAN")
	}
}

func TestValidateChain_RejectsMissingSAN(t *testing.T) {
	ca, caKey, roots := makeTestCA(t)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	tmpl := leafTemplate("v7q4km2x9pz6dn3w", testBase, now.Add(-time.Hour), now.Add(time.Hour))
	tmpl.DNSNames = []string{"*.v7q4km2x9pz6dn3w." + testBase} // only the direct SAN
	chain := signLeaf(t, ca, caKey, leafKey, tmpl)
	if err := ValidateChain(chain, keyPEMFor(t, leafKey), "v7q4km2x9pz6dn3w", testBase, roots); err == nil {
		t.Fatalf("ValidateChain accepted a leaf missing the relay SAN")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd agent && go test ./internal/cert/ -v`
Expected: FAIL — `undefined: GenerateWildcardCSR` / `undefined: ValidateChain`.

- [ ] **Step 3: Implement CSR generation and chain validation (agent-side, key never leaves)**

```go
// agent/internal/cert/csr.go
package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
)

// GenerateWildcardCSR generates an agent-held ECDSA P-256 private key and a
// CSR requesting BOTH wildcard SANs:
//   *.      <namespace>.<baseDomain>   (direct namespace, default mode)
//   *.relay.<namespace>.<baseDomain>   (relay namespace, FRP fallback)
// The private key is generated locally and NEVER leaves the agent; only the
// CSR (and later the returned chain) cross the wire. baseDomain is a parameter
// so the spike can run against a delegated test zone before the production
// domain is purchased.
func GenerateWildcardCSR(namespace, baseDomain string) (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sans := wildcardSANs(namespace, baseDomain)
	tmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: sans[0]},
		DNSNames: sans,
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

// wildcardSANs returns the two wildcard SANs for a namespace, in a fixed,
// canonical order.
func wildcardSANs(namespace, baseDomain string) []string {
	return []string{
		fmt.Sprintf("*.%s.%s", namespace, baseDomain),
		fmt.Sprintf("*.relay.%s.%s", namespace, baseDomain),
	}
}
```

```go
// agent/internal/cert/validate.go
package cert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sort"
	"time"
)

// ValidateChain verifies the chain the control plane returned, before the
// agent installs it. It checks, in order:
//   1. every PEM block parses and the chain is non-empty;
//   2. the chain verifies against the configured trust root with the leaf as
//      the server cert (rejects self-signed, expired, and wrong-issuer chains);
//   3. the leaf's public key equals the locally-held key's public key
//      (rejects a cert minted for a different/attacker-controlled key);
//   4. the leaf's SAN set is EXACTLY the two expected wildcards, no extras;
//   5. the leaf is currently valid (NotBefore <= now <= NotAfter);
//   6. the leaf asserts serverAuth EKU and digital-signature key usage.
func ValidateChain(certChainPEM, keyPEM []byte, namespace, baseDomain string, roots *x509.CertPool) error {
	if roots == nil {
		return fmt.Errorf("nil trust root pool")
	}

	// 1. Parse all certs in the returned chain.
	var certs []*x509.Certificate
	rest := certChainPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("parse certificate: %w", err)
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return fmt.Errorf("no certificates in chain")
	}
	leaf := certs[0]

	// 2. Chain of trust against the configured root.
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("chain verify: %w", err)
	}

	// 3. Public-key equality with the locally-held key.
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return fmt.Errorf("no PEM block in key")
	}
	priv, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return fmt.Errorf("parse local key: %w", err)
	}
	if !publicKeysEqual(leaf.PublicKey, priv.Public()) {
		return fmt.Errorf("leaf public key does not match the locally-held key")
	}

	// 4. Exact SAN set (both wildcards, no extras).
	want := wildcardSANs(namespace, baseDomain)
	if !stringSetsEqual(leaf.DNSNames, want) {
		return fmt.Errorf("SAN set = %v, want exactly %v", leaf.DNSNames, want)
	}

	// 5. Validity window.
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return fmt.Errorf("certificate not currently valid: %v..%v", leaf.NotBefore, leaf.NotAfter)
	}

	// 6. EKU + key usage.
	if !hasExtKeyUsage(leaf, x509.ExtKeyUsageServerAuth) {
		return fmt.Errorf("leaf missing serverAuth EKU")
	}
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return fmt.Errorf("leaf missing digital-signature key usage")
	}

	return nil
}

func publicKeysEqual(a, b crypto.PublicKey) bool {
	ae, ok := a.(*ecdsa.PublicKey)
	if !ok {
		return false
	}
	be, ok := b.(*ecdsa.PublicKey)
	if !ok {
		return false
	}
	return ae.Curve == be.Curve && ae.X.Cmp(be.X) == 0 && ae.Y.Cmp(be.Y) == 0
}

func stringSetsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func hasExtKeyUsage(c *x509.Certificate, want x509.ExtKeyUsage) bool {
	for _, u := range c.ExtKeyUsage {
		if u == want {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd agent && go test ./internal/cert/ -v`
Expected: PASS.

- [ ] **Step 5: Implement the control-plane cert coordinator (CSR in → chain out)**

```go
// signaling-server/internal/certcoordinator/acme.go
package certcoordinator

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/go-acme/lego/v4/registration"
)

// ACMEConfig carries operator-supplied ACME/DNS credentials. All values come
// from env/config at runtime and are NEVER committed to the repo.
type ACMEConfig struct {
	CA              string // lego.LEDirectoryStaging or lego.LEDirectoryProduction
	Email           string // ACME account contact
	CloudflareToken string // Cloudflare API token: single zone, DNS-edit only
}

// acmeAccount implements lego's registration.User. The spike uses an ephemeral
// account key; Phase 2 MUST persist this key securely (it identifies the ACME
// account and is required for renewals).
type acmeAccount struct {
	email string
	key   *ecdsa.PrivateKey
}

func (a *acmeAccount) GetEmail() string                        { return a.email }
func (a *acmeAccount) GetRegistration() *registration.Resource { return nil }
func (a *acmeAccount) GetPrivateKey() crypto.PrivateKey        { return a.key }

// CompleteCSR takes an agent-submitted CSR PEM, runs an ACME DNS-01 order via
// lego's Cloudflare provider, and returns ONLY the issued leaf-first chain.
// It never sees the agent's private key: lego finalizes the pre-made CSR via
// ObtainForCSR (the external-CSR API) instead of generating a key. lego
// derives the order's domains from the CSR's SANs, so a single call covers
// BOTH wildcards in one order.
func CompleteCSR(ctx context.Context, csrPEM []byte, cfg ACMEConfig) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("invalid CSR PEM block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	acct := &acmeAccount{email: cfg.Email, key: key}

	config := lego.NewConfig(acct)
	config.CADirURL = cfg.CA
	client, err := lego.NewClient(config)
	if err != nil {
		return nil, err
	}

	// Register (or look up) the ACME account.
	if _, err := client.Registration.Register(registration.RegisterOptions{
		TermsOfServiceAgreed: true,
	}); err != nil {
		return nil, fmt.Errorf("ACME account registration: %w", err)
	}

	// DNS-01 via Cloudflare. The token MUST be scoped to the target zone with
	// DNS-edit permission only (least privilege).
	pcfg := cloudflare.NewDefaultConfig()
	pcfg.AuthToken = cfg.CloudflareToken
	provider, err := cloudflare.NewDNSProviderConfig(pcfg)
	if err != nil {
		return nil, fmt.Errorf("cloudflare provider: %w", err)
	}
	if err := client.Challenge.SetDNS01Provider(provider); err != nil {
		return nil, err
	}

	// Finalize the agent's CSR. lego creates the _acme-challenge TXT records,
	// waits for propagation, and removes them on completion.
	resource, err := client.Certificate.ObtainForCSR(certificate.ObtainForCSRRequest{
		CSR:    csr,
		Bundle: true,
	})
	if err != nil {
		return nil, fmt.Errorf("ObtainForCSR: %w", err)
	}

	// Leaf first, then the issuer chain. resource.Certificate is the leaf.
	chain := append([]byte(nil), resource.Certificate...)
	chain = append(chain, resource.IssuerCertificate...)
	return chain, nil
}
```

- [ ] **Step 6: Implement the DDNS package and spike program (create/update/delete + propagation)**

```go
// signaling-server/internal/ddns/ddns.go
package ddns

import (
	"context"
	"fmt"

	"github.com/cloudflare/cloudflare-go"
)

// Cloudflare manages the direct namespace's wildcard A record via the
// operator's Cloudflare API token (single zone, DNS-edit only).
type Cloudflare struct {
	api    *cloudflare.API
	zoneID string
}

func New(ctx context.Context, apiToken, zoneName string) (*Cloudflare, error) {
	api, err := cloudflare.NewWithAPIToken(apiToken)
	if err != nil {
		return nil, fmt.Errorf("cloudflare client: %w", err)
	}
	zoneID, err := api.ZoneIDByName(zoneName)
	if err != nil {
		return nil, fmt.Errorf("resolve zone %q: %w", zoneName, err)
	}
	return &Cloudflare{api: api, zoneID: zoneID}, nil
}

// UpsertA creates or updates the direct wildcard A record to point at ip with
// the given TTL. proxied=false so the agent's real IP is served (no Cloudflare
// edge in the data path).
func (c *Cloudflare) UpsertA(ctx context.Context, name, ip string, ttl int) (string, error) {
	proxied := false
	rec := cloudflare.DNSRecord{Type: "A", Name: name, Content: ip, TTL: ttl, Proxied: &proxied}

	existing, err := c.api.DNSRecords(ctx, c.zoneID, cloudflare.DNSRecord{Type: "A", Name: name})
	if err != nil {
		return "", fmt.Errorf("list A record %q: %w", name, err)
	}
	if len(existing) == 0 {
		resp, err := c.api.CreateDNSRecord(ctx, c.zoneID, rec)
		if err != nil {
			return "", fmt.Errorf("create A record %q: %w", name, err)
		}
		return resp.Result.ID, nil
	}
	if err := c.api.UpdateDNSRecord(ctx, c.zoneID, existing[0].ID, rec); err != nil {
		return "", fmt.Errorf("update A record %q: %w", name, err)
	}
	return existing[0].ID, nil
}

// DeleteA removes the named A record (spike cleanup).
func (c *Cloudflare) DeleteA(ctx context.Context, name string) error {
	existing, err := c.api.DNSRecords(ctx, c.zoneID, cloudflare.DNSRecord{Type: "A", Name: name})
	if err != nil {
		return err
	}
	for _, r := range existing {
		if err := c.api.DeleteDNSRecord(ctx, c.zoneID, r.ID); err != nil {
			return err
		}
	}
	return nil
}
```

```go
// signaling-server/cmd/spike-ddns/main.go
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/miekg/dns"

	"sharebridge/server/internal/ddns"
)

// DDNS spike: create/update/delete the direct wildcard A record and measure
// authoritative + recursive propagation at TTL 60 (spec §13.3).
//
// The record NAME is the wildcard (e.g. *.v7q4km2x9pz6dn3w.sbx.example.com),
// but propagation is verified by resolving a CONCRETE host under it
// (e.g. probe.v7q4km2x9pz6dn3w.sbx.example.com) that the wildcard covers.
// The relay wildcard (*.relay.<ns>... -> gateway IP) is static and out of
// scope for this DDNS test.
func main() {
	ctx := context.Background()

	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	zone := os.Getenv("CLOUDFLARE_ZONE")
	name := os.Getenv("SPIKE_DDNS_NAME")    // wildcard A record to create
	probe := os.Getenv("SPIKE_PROBE_NAME")  // concrete host under the wildcard
	ip := os.Getenv("SPIKE_DDNS_IP")        // agent's reported public IP
	if token == "" || zone == "" || name == "" || probe == "" || ip == "" {
		log.Fatal("set CLOUDFLARE_API_TOKEN, CLOUDFLARE_ZONE, SPIKE_DDNS_NAME, SPIKE_PROBE_NAME, SPIKE_DDNS_IP")
	}

	c, err := ddns.New(ctx, token, zone)
	if err != nil {
		log.Fatalf("ddns client: %v", err)
	}

	// 1. Create (or update) the direct wildcard A record with TTL 60.
	id, err := c.UpsertA(ctx, name, ip, 60)
	if err != nil {
		log.Fatalf("UpsertA: %v", err)
	}
	log.Printf("A record %s -> %s (id=%s, ttl=60)", name, ip, id)

	// 2. Authoritative propagation: query the zone's own nameservers directly.
	log.Printf("authoritative: %v", resolveAuthoritative(zone, probe))

	// 3. Recursive propagation: poll public resolvers until they see the IP.
	log.Printf("recursive: %v", waitRecursive(probe, ip, 5*time.Minute))

	// 4. Update: point at a second IP and confirm both see the change.
	if ip2 := os.Getenv("SPIKE_DDNS_IP2"); ip2 != "" && ip2 != ip {
		if _, err := c.UpsertA(ctx, name, ip2, 60); err != nil {
			log.Fatalf("update UpsertA: %v", err)
		}
		log.Printf("recursive after update: %v", waitRecursive(probe, ip2, 5*time.Minute))
	}

	// 5. Delete and confirm removal.
	if err := c.DeleteA(ctx, name); err != nil {
		log.Fatalf("DeleteA: %v", err)
	}
	log.Printf("A record %s deleted; cleanup verified", name)
}

// resolveAuthoritative discovers the zone's NS records via a recursive
// resolver, then queries each authoritative server directly for the A record.
func resolveAuthoritative(zone, probe string) []string {
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(zone), dns.TypeNS)
	r, _, err := c.Exchange(m, "1.1.1.1:53")
	if err != nil {
		return []string{fmt.Sprintf("NS lookup error: %v", err)}
	}
	var ips []string
	m2 := new(dns.Msg)
	m2.SetQuestion(dns.Fqdn(probe), dns.TypeA)
	for _, ans := range r.Answer {
		ns, ok := ans.(*dns.NS)
		if !ok {
			continue
		}
		r2, _, err := c.Exchange(m2, dns.Fqdn(ns.Ns)+":53")
		if err != nil {
			continue
		}
		for _, a := range r2.Answer {
			if rec, ok := a.(*dns.A); ok {
				ips = append(ips, rec.A.String())
			}
		}
	}
	return ips
}

// waitRecursive polls the system resolver (recursive) until it returns wantIP.
func waitRecursive(name, wantIP string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ips, err := net.LookupHost(name); err == nil {
			for _, ip := range ips {
				if ip == wantIP {
					return nil
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("recursive resolver did not return %s for %s within %s", wantIP, name, timeout)
}
```

- [ ] **Step 7: Write the two spike programs (control-plane ACME order, agent validate)**

```go
// signaling-server/cmd/spike-cert/main.go
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/go-acme/lego/v4/lego"

	"sharebridge/server/internal/certcoordinator"
)

// Control-plane half: read the agent's CSR, complete DNS-01 ACME issuance,
// write the chain. It never sees the agent's private key.
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	csrPEM, err := os.ReadFile(os.Getenv("CSR_FILE"))
	if err != nil {
		log.Fatalf("read CSR_FILE: %v", err)
	}
	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	email := os.Getenv("ACME_EMAIL")
	if token == "" || email == "" {
		log.Fatal("set CLOUDFLARE_API_TOKEN and ACME_EMAIL")
	}
	// STAGING first: run the whole flow against the ACME staging CA before
	// touching production. Set ACME_CA to the production directory URL only
	// after staging succeeds end-to-end.
	ca := os.Getenv("ACME_CA")
	if ca == "" {
		ca = lego.LEDirectoryStaging
	}

	chain, err := certcoordinator.CompleteCSR(ctx, csrPEM, certcoordinator.ACMEConfig{
		CA:              ca,
		Email:           email,
		CloudflareToken: token,
	})
	if err != nil {
		log.Fatalf("CompleteCSR: %v", err)
	}
	if err := os.WriteFile("chain.pem", chain, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("chain written to chain.pem (%d bytes) via %s", len(chain), ca)
}
```

```go
// agent/cmd/spike-certagent/main.go
package main

import (
	"crypto/x509"
	"log"
	"os"

	"sharebridge/agent/internal/cert"
)

// Agent half: generate the key+CSR locally (key never leaves), and, when a
// chain is present, validate it against the locally-held key and the
// configured trust root.
func main() {
	ns := os.Getenv("SPIKE_NS")     // agent namespace, e.g. v7q4km2x9pz6dn3w
	base := os.Getenv("SPIKE_BASE") // base domain, e.g. sbx.example.com
	if ns == "" || base == "" {
		log.Fatal("set SPIKE_NS and SPIKE_BASE")
	}

	keyPEM, csrPEM, err := cert.GenerateWildcardCSR(ns, base)
	if err != nil {
		log.Fatalf("GenerateWildcardCSR: %v", err)
	}
	if err := os.WriteFile("key.pem", keyPEM, 0o600); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("csr.pem", csrPEM, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("agent key + CSR written: key.pem (0600), csr.pem")

	chain, err := os.ReadFile("chain.pem")
	if err != nil {
		return // chain not yet returned; the control-plane half runs next
	}
	if err := cert.ValidateChain(chain, keyPEM, ns, base, loadRoots()); err != nil {
		log.Fatalf("ValidateChain rejected returned chain: %v", err)
	}
	log.Printf("returned chain validated: exact SANs, key matches, chain verifies")
}

// loadRoots returns the trust root the agent validates against. For the spike,
// pass the ACME STAGING root PEM via SPIKE_CA_ROOT (the staging root is not in
// the system trust store). In production the agent pins the configured CA root.
func loadRoots() *x509.CertPool {
	if caPath := os.Getenv("SPIKE_CA_ROOT"); caPath != "" {
		pemBytes, err := os.ReadFile(caPath)
		if err != nil {
			log.Fatalf("read SPIKE_CA_ROOT: %v", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pemBytes) {
			log.Fatal("no certs in SPIKE_CA_ROOT")
		}
		return roots
	}
	if sys, err := x509.SystemCertPool(); err == nil {
		return sys
	}
	return x509.NewCertPool()
}
```

- [ ] **Step 8: Run the spike end-to-end — ACME staging first, then DDNS, then production**

1. Set env (never committed): `CLOUDFLARE_API_TOKEN` (single zone, DNS-edit only), `CLOUDFLARE_ZONE`, `ACME_EMAIL`, `SPIKE_NS`, `SPIKE_BASE`.
2. Agent: `cd agent && go run ./cmd/spike-certagent` → writes `key.pem` (0600) + `csr.pem` (two wildcard SANs).
3. Control plane (staging): `cd signaling-server && export ACME_CA=https://acme-staging-v02.api.letsencrypt.org/directory CSR_FILE=../agent/csr.pem && go run ./cmd/spike-cert`. Verify `chain.pem` has the leaf + issuer, and that no `_acme-challenge` TXT records remain in the zone afterward (challenge cleanup check via the Cloudflare dashboard/API).
4. Agent validates: `cd agent && SPIKE_CA_ROOT=<staging-root.pem> go run ./cmd/spike-certagent` → `ValidateChain` passes against the staging root (download the LE staging root from https://letsencrypt.org/certs/staging/).
5. DDNS: `cd signaling-server && SPIKE_DDNS_NAME='*.v7q4km2x9pz6dn3w.sbx.example.com' SPIKE_PROBE_NAME='probe.v7q4km2x9pz6dn3w.sbx.example.com' SPIKE_DDNS_IP=<agent-ip> SPIKE_DDNS_IP2=<second-ip> go run ./cmd/spike-ddns`. Record authoritative + recursive propagation times at TTL 60 for create and update, and confirm delete.
6. Repeat steps 2–4 against production (`ACME_CA=https://acme-v02.api.letsencrypt.org/directory`) only after staging succeeds, with `SPIKE_CA_ROOT` pointing at the production ISRG root.

Expected: a valid two-SAN wildcard certificate issued (staging then production), `ValidateChain` passes with the exact two SANs, and DDNS create/update/delete + propagation is measured. Record the CA used, issuance latency, propagation latency, and any rate-limit/terms surprises.

CA choice: start with **Let's Encrypt** (simplest, free). Its 50-cert/week/domain limit bounds *agent-enrollment* rate (one cert per namespace regardless of SAN count — spec §7/§8); Google Public CA (100 orders/hour) and ZeroSSL (unlimited) remain the scale candidates behind the provider abstraction (FRP §9.4).

- [ ] **Step 9: Write the findings doc and commit**

Write `docs/superpowers/spikes/2026-08-14-cert-dns.md` recording: the delegated test zone used (production domain still unpurchased — spec §15.1), CA (staging vs production), DNS provider, token scope (single-zone DNS-edit verified), the two-wildcard-SAN issuance behavior (one order, one cert, no extra rate-limit cost), `ValidateChain` behavior (self-signed/expired/wrong-key/extraneous-SAN all rejected), DDNS authoritative + recursive propagation times at TTL 60, challenge cleanup verification, the account-key persistence requirement, and the renewal plan. Then:

```bash
git add agent/internal/cert/ agent/cmd/spike-certagent/ signaling-server/internal/certcoordinator/ signaling-server/internal/ddns/ signaling-server/cmd/spike-cert/ signaling-server/cmd/spike-ddns/ docs/superpowers/spikes/
git commit -m "spike: prove 2-SAN wildcard cert (agent-held key) + DDNS for direct mode"
```

---

### Task 3: Direct-TCP throughput spike

**Files:**
- Modify: `agent/cmd/benchdirect/main.go` (new `--mode` values `https|fetch|tcpproxy`, new `--url/--target/--reps` flags, validation + dispatch)
- Create: `agent/cmd/benchdirect/tcpshim.go` (net.Conn-wrapping TCP impairment proxy)
- Create: `agent/cmd/benchdirect/tcpshim_test.go`
- Create: `agent/cmd/benchdirect/httpsbench.go` (`--mode https` file server + `--mode fetch` client + result types/summary)
- Create: `agent/cmd/benchdirect/httpsbench_test.go`
- Create: `docs/superpowers/spikes/2026-08-14-direct-tcp-throughput.md`

`server.go` and `rawbench.go` (and the WebRTC/`raw`/`prod` path) are left untouched. The HTTPS server is a new, separate listener — `server.go`'s `benchServer` is the browser-signaling path, not the data plane.

**Interfaces:**
- Consumes: the existing `--rtt/--jitter/--bandwidth/--loss` flags (now applied by the new TCP proxy, **not** the UDP shim), `parseByteSize` from `main.go`, and the `rateLimiter` type from `shim.go` (reused by the TCP proxy).
- Produces: `tcpProxy` (a TCP listener that dials a target and shapes each conn) and `runHTTPServer`/`runFetch`, plus the written finding. No production code; no WebRTC/SCTP path is modified.

**Corrections this task fixes (from code review, verified against the real source):**

1. `--size 1024` is **1024 bytes**, not 1 GiB. `parseByteSize` accepts unit-suffixed values and falls through to `strconv.ParseInt(s, 10, 64)` for bare numbers, which is bytes. Use `--size 1GiB` / `--size 100MiB` / `--size 8MiB`. Note `--bandwidth 25MB` = 25×10⁶ bytes/s (~200 Mbps) because `MB` is a decimal multiplier.
2. `--mode` today is `raw|prod` only (both WebRTC/SCTP). There is **no** relay mode and **no** HTTPS mode. The two new data-plane modes are specified below; do not assume they exist.
3. `shim.go` is `net.UDPConn`-only (`ReadFromUDP`/`WriteToUDP`) — it cannot shape a TCP/HTTPS connection. A net.Conn-wrapping TCP proxy is specified below, with TCP loss modeled as drop→retransmit (not UDP drop-and-continue).
4. The comparison measures the same payload over the real transport paths at RTT ≥ 50 ms, with TLS, warm-up, ≥12 reps, median + range + percentile, and an explicit pass/fail criterion.

- [ ] **Step 1: Write the failing test for the TCP impairment proxy**

```go
// agent/cmd/benchdirect/tcpshim_test.go
package main

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestTCPProxyForwardsIntact: a large transfer through the proxy arrives
// complete and in order even with loss enabled — TCP retransmits the dropped
// bytes (the receiver never ACKs them, the sender re-sends). This is the
// drop→retransmit semantic the UDP shim cannot model.
func TestTCPProxyForwardsIntact(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(c, c) // echo
		c.Close()
	}()

	p, err := newTCPProxy(backend.Addr().String(), 5*time.Millisecond, 2*time.Millisecond, 0.05, 0)
	if err != nil {
		t.Fatal(err)
	}
	go p.Serve()
	defer p.Close()

	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	payload := make([]byte, 1<<20) // 1 MiB
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read back: %v (loss broke the stream)", err)
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("byte %d corrupted", i)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./cmd/benchdirect/ -run TestTCPProxyForwardsIntact -v`
Expected: FAIL — `undefined: newTCPProxy`.

- [ ] **Step 3: Implement the TCP impairment proxy**

```go
// agent/cmd/benchdirect/tcpshim.go
package main

import (
	"errors"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"
)

// shapingConn wraps a net.Conn and applies one-way latency, jitter, packet
// loss, and a bandwidth cap to the byte stream it writes.
//
// Loss is modeled by DROPPING whole write buffers (reporting success without
// forwarding), so the receiving TCP never ACKs the bytes and the sender's TCP
// retransmits with backoff — the same drop→retransmit behavior as a real link.
// This is deliberately different from the UDP Shim, where a dropped datagram
// simply never arrives and nothing retransmits.
type shapingConn struct {
	conn      net.Conn
	delay     time.Duration // one-way latency (RTT/2)
	jitter    time.Duration
	loss      float64
	limiter   *rateLimiter // nil = unlimited
	onClose   func()
	closeOnce sync.Once
}

func (c *shapingConn) Read(p []byte) (int, error) { return c.conn.Read(p) }

func (c *shapingConn) Write(p []byte) (int, error) {
	if c.loss > 0 && rand.Float64() < c.loss {
		return len(p), nil // drop: never forwarded; peer TCP retransmits
	}
	if c.delay > 0 || c.jitter > 0 {
		d := c.delay
		if c.jitter > 0 {
			d += time.Duration((rand.Float64()*2 - 1) * float64(c.jitter))
		}
		if d > 0 {
			time.Sleep(d)
		}
	}
	if c.limiter != nil {
		c.limiter.take(len(p))
	}
	return c.conn.Write(p)
}

func (c *shapingConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.conn.Close()
		if c.onClose != nil {
			c.onClose()
		}
	})
	return err
}

func (c *shapingConn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *shapingConn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *shapingConn) SetDeadline(t time.Time) error      { return c.conn.SetDeadline(t) }
func (c *shapingConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *shapingConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// tcpProxy accepts a connection, dials target, and wires the two sides through
// shapingConns, each direction shaped independently. It is the TCP analog of
// the UDP Shim and is the ONLY way the benchmark shapes a TCP/HTTPS link.
type tcpProxy struct {
	ln        net.Listener
	target    string
	delay     time.Duration
	jitter    time.Duration
	loss      float64
	bandwidth int64
}

func newTCPProxy(target string, delay, jitter time.Duration, loss float64, bandwidth int64) (*tcpProxy, error) {
	if loss < 0 || loss > 1 {
		return nil, errors.New("loss must be between 0 and 1")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return &tcpProxy{ln: ln, target: target, delay: delay, jitter: jitter, loss: loss, bandwidth: bandwidth}, nil
}

func (p *tcpProxy) Addr() net.Addr { return p.ln.Addr() }

func (p *tcpProxy) Serve() {
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.pipe(down)
	}
}

func (p *tcpProxy) pipe(down net.Conn) {
	up, err := net.Dial("tcp", p.target)
	if err != nil {
		down.Close()
		return
	}
	var limiter *rateLimiter
	if p.bandwidth > 0 {
		limiter = newRateLimiter(float64(p.bandwidth))
	}
	a := &shapingConn{conn: down, delay: p.delay, jitter: p.jitter, loss: p.loss, limiter: limiter}
	b := &shapingConn{conn: up, delay: p.delay, jitter: p.jitter, loss: p.loss, limiter: limiter}
	a.onClose = func() { b.Close() }
	b.onClose = func() { a.Close() }
	go func() { _, _ = io.Copy(b, a); b.Close() }()
	_, _ = io.Copy(a, b)
	a.Close()
}

func (p *tcpProxy) Close() error { return p.ln.Close() }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./cmd/benchdirect/ -run TestTCPProxyForwardsIntact -v`
Expected: PASS (the full 1 MiB round-trips despite 5% per-direction loss).

- [ ] **Step 5: Write the failing test for the HTTPS server + fetch client**

```go
// agent/cmd/benchdirect/httpsbench_test.go
package main

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestHTTPSRangeAndFullFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url, err := serveFile(ctx, 1<<20) // 1 MiB
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:  insecureTLSConfig(),
		DisableKeepAlives: true,
	}}

	// Range request returns 206 with exactly the requested slice.
	req, _ := http.NewRequest(http.MethodGet, url+"/bench.bin", nil)
	req.Header.Set("Range", "bytes=0-1023")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range status = %d, want 206", resp.StatusCode)
	}
	if b, _ := io.ReadAll(resp.Body); len(b) != 1024 {
		t.Fatalf("Range body = %d bytes, want 1024", len(b))
	}

	// Full fetch returns the whole payload.
	results, err := fetch(ctx, url+"/bench.bin", 1<<20, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Received != 1<<20 {
			t.Fatalf("received = %d, want %d", r.Received, int64(1<<20))
		}
	}
	_ = time.Now()
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `cd agent && go test ./cmd/benchdirect/ -run TestHTTPSRangeAndFullFetch -v`
Expected: FAIL — `undefined: serveFile` / `undefined: fetch`.

- [ ] **Step 7: Implement the HTTPS server + fetch client**

```go
// agent/cmd/benchdirect/httpsbench.go
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"sort"
	"time"
)

// selfSignedCert builds an in-memory self-signed ECDSA P-256 cert for the
// loopback harness. It models the agent's TLS termination (kernel TCP + TLS,
// same record overhead) without a public CA; it is never served to real users.
func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "benchdirect-local"},
		DNSNames:     []string{"127.0.0.1", "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
}

func insecureTLSConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
}

type sizeReader struct {
	b   []byte
	off int64
}

func (r *sizeReader) Read(p []byte) (int, error) {
	if r.off >= int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += int64(n)
	return n, nil
}
func (r *sizeReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.off = offset
	case io.SeekCurrent:
		r.off += offset
	case io.SeekEnd:
		r.off = int64(len(r.b)) + offset
	}
	return r.off, nil
}

// serveFile runs the HTTPS data-plane server: a deterministic in-memory
// size-byte file served via http.ServeContent (real Range/206) over TLS on a
// plain TCP listener — exactly the agent's data-plane semantics (kernel TCP +
// TLS + Range). Returns the base URL.
func serveFile(ctx context.Context, size int64) (string, error) {
	cert, err := selfSignedCert()
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/bench.bin", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "bench.bin", time.Time{}, &sizeReader{b: make([]byte, size)})
	})
	srv := &http.Server{
		Handler:   mux,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	return "https://" + ln.Addr().String(), nil
}

type fetchResult struct {
	Mode      string    `json:"mode"`
	RTT       int       `json:"rtt_ms"`
	Requested int64     `json:"requested_bytes"`
	Received  int64     `json:"received_bytes"`
	Mbps      float64   `json:"mbps"`
	Samples   []float64 `json:"samples_mbps"` // per-100ms throughput
	Stalls    int       `json:"stalls"`       // ≥2s runs of <1 Mbps
}

// fetch downloads url with a fresh TCP+TLS connection each rep (no keep-alive,
// so every rep models a fresh recipient connection), measuring per-100ms
// throughput windows. The FIRST rep is a warm-up (TLS handshake + slow-start)
// and is excluded from the returned slice.
func fetch(ctx context.Context, url string, size int64, rttMs int, reps int) ([]fetchResult, error) {
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   insecureTLSConfig(),
		DisableKeepAlives: true,
	}}
	run := func() (fetchResult, error) {
		start := time.Now()
		resp, err := client.Get(url)
		if err != nil {
			return fetchResult{}, err
		}
		samples, received, err := measureCopy(resp.Body)
		resp.Body.Close()
		if err != nil {
			return fetchResult{}, err
		}
		return fetchResult{
			Mode: "https", RTT: rttMs, Requested: size, Received: received,
			Mbps: float64(received) * 8 / time.Since(start).Seconds() / 1e6,
			Samples: samples, Stalls: countStalls(samples),
		}, nil
	}
	if _, err := run(); err != nil { // warm-up, discarded
		return nil, err
	}
	out := make([]fetchResult, 0, reps)
	for i := 0; i < reps; i++ {
		r, err := run()
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func measureCopy(r io.Reader) ([]float64, int64, error) {
	buf := make([]byte, 128*1024)
	winStart := time.Now()
	var winBytes, total int64
	var samples []float64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			total += int64(n)
			winBytes += int64(n)
		}
		if el := time.Since(winStart); el >= 100*time.Millisecond {
			samples = append(samples, float64(winBytes)*8/el.Seconds()/1e6)
			winStart, winBytes = time.Now(), 0
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return samples, total, err
		}
	}
	if winBytes > 0 {
		samples = append(samples, float64(winBytes)*8/time.Since(winStart).Seconds()/1e6)
	}
	return samples, total, nil
}

// countStalls counts ≥2s runs where every 100ms window is <1 Mbps (the SCTP
// collapse signature: multi-second stalls). Direct-TCP must show zero.
func countStalls(samples []float64) int {
	stalls, run := 0, 0
	for _, s := range samples {
		if s < 1.0 {
			run++
		} else {
			run = 0
		}
		if run == 20 { // 20 × 100ms = 2s
			stalls++
		}
	}
	return stalls
}

// summarize returns median, min, max, and p95 Mbps across reps.
func summarize(rs []fetchResult) (median, min, max, p95 float64) {
	m := make([]float64, len(rs))
	for i, r := range rs {
		m[i] = r.Mbps
	}
	sort.Float64s(m)
	min, max = m[0], m[len(m)-1]
	median = m[len(m)/2]
	p95 = m[int(0.95*float64(len(m)-1))]
	return
}
```

- [ ] **Step 8: Run test to verify it passes**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestHTTPSRangeAndFullFetch|TestTCPProxyForwardsIntact' -v`
Expected: PASS.

- [ ] **Step 9: Wire the new modes into `main.go`**

Only `main.go` changes (leave `server.go`, `rawbench.go`, `shim.go` untouched):

1. `--mode` help string becomes `"raw|prod|https|fetch|tcpproxy"`.
2. Add flags: `--url` (string, fetch target), `--target` (string, tcpproxy backend `host:port`), `--reps` (int, default **12**), optional `--cert`/`--key` (string, default `""` = generate self-signed).
3. `validateRunConfig` (and the dispatch switch) accept the three new modes with these rules: `https` requires `--size > 0`; `fetch` requires `--url != ""` and `--reps >= 1`; `tcpproxy` requires `--target != ""`. The existing `raw|prod` validation is unchanged.
4. The dispatch switch gains:
   - `case "https":` call `runHTTPServer(ctx, cfg)` → runs `serveFile(ctx, cfg.size)`, prints the listen URL to stderr, and blocks until signal.
   - `case "tcpproxy":` build `newTCPProxy(cfg.target, rtt/2, jitter, loss, bandwidth)`, `Serve()`, block until signal.
   - `case "fetch":` call `fetch(ctx, cfg.url, cfg.size, cfg.rttMs, cfg.reps)`, then print `summarize(...)` (median/min/max/p95) to stderr and write the `[]fetchResult` array as JSON to `--out` (a new writer beside the existing `rawResult` path; the `raw`/`prod` JSON shape is unchanged).

- [ ] **Step 10: Run direct-TCP vs relay at RTT ≥ 50 ms (same payload, same shaping)**

The relay data plane is also kernel TCP + TLS (browser → VPS → agent); its throughput is bounded by the same kernel-TCP physics as direct plus an extra hop (higher RTT, shared VPS bandwidth). The spike therefore measures the kernel-TCP/HTTPS data plane with one harness and treats the relay's documented **20–30 MB/s ≈ 160–240 Mbps** (BENCH_RESULTS.md, spec §2) as the baseline to meet — re-derived at the **same** payload, RTT, and bandwidth so the comparison is apples-to-apples. (If the real relay/VPS endpoint is reachable, `--url` can target it directly; the equal-RTT in-harness comparison is the deterministic gate.)

Primary run — RTT 100 ms, owner's real upstream (25 MB/s ≈ 200 Mbps), 100 MiB payload, no loss, 12 reps:

```bash
cd agent
# (1) HTTPS file server: 100 MiB, Range-capable.  NOTE: --size takes unit-suffixed
#     values; a bare number is BYTES, so --size 1024 would be 1024 bytes, not 1 GiB.
go run ./cmd/benchdirect -mode https -size 100MiB &
SRV=$!

# (2) TCP impairment proxy in front of the server: RTT=100ms (50ms each way),
#     jitter 10ms, 25 MB/s cap, 0 loss.
go run ./cmd/benchdirect -mode tcpproxy -target 127.0.0.1:<server-port> -rtt 100 -jitter 10 -bandwidth 25MB -loss 0 &
PROXY=$!

# (3) Fetch client: 1 warm-up + 12 measured reps (median/min/max/p95 reported).
go run ./cmd/benchdirect -mode fetch -url https://127.0.0.1:<proxy-port>/bench.bin -size 100MiB -rtt 100 -reps 12
```

Secondary runs (same server/proxy pattern):
- **RTT 50 ms** (`-rtt 50`) to cover the spec's "≥ 50 ms" lower bound.
- **1% loss** (`-loss 0.01`, RTT 100) to confirm kernel TCP survives the loss level at which SCTP collapsed ~120× (BENCH_RESULTS.md).

Expected: direct-TCP median ≈ the 25 MB/s cap (~200 Mbps) — i.e. **≥ relay** (160–240 Mbps) — with `stalls == 0` in every rep. This is the pass condition.

- [ ] **Step 11: Write findings and commit**

Write `docs/superpowers/spikes/2026-08-14-direct-tcp-throughput.md` with: TLS setup (in-memory self-signed ECDSA P-256, TLS 1.2+), warm-up policy (first rep discarded), the per-rep `median / min / max / p95` Mbps table, the per-100ms sample trace (to eyeball the absence of stalls), the stall count per rep, and the explicit verdict:

> **Pass/fail:** PASS iff (a) direct-TCP median Mbps ≥ relay baseline (≥ 160 Mbps at RTT=100, 25 MB/s cap) at the same payload/RTT/bandwidth, **and** (b) every rep has `stalls == 0` (no SCTP-collapse signature — the SCTP path shows multi-second zero-throughput stalls at these settings). Fail on either condition means direct does not become the default (relay remains), per spec §13.4.

Then:

```bash
git add agent/cmd/benchdirect/ docs/superpowers/spikes/
git commit -m "spike: measure direct-TCP (HTTPS) vs relay throughput at RTT>=50ms"
```

---

### Task 4: On-demand port + SNI binding prototype

This task also folds in the strict open-signal protocol from spec §5.1: a versioned, expiring, idempotent signal bound to `(agent, share, route, nonce)`, verified and rate-limited at the agent.

> **Spike simplification (granted port):** `OnDemandPort` tracks a single external port and assumes `AddPortMapping` grants the requested port (true for UPnP). NAT-PMP may remap the external port — Task 1 proves granted-port reporting; Phase 2's production `OnDemandPort` will store and use the granted port. The spike's `recordingMapper` echoes the requested port.

**Files:**
- Create: `agent/internal/direct/ondemand.go`
- Create: `agent/internal/direct/ondemand_test.go`
- Create: `agent/internal/direct/sni.go`
- Create: `agent/internal/direct/sni_test.go`
- Create: `agent/internal/direct/opensignal.go`
- Create: `agent/internal/direct/opensignal_test.go`
- Create: `docs/superpowers/spikes/2026-08-14-ondemand-sni.md`

**Interfaces:**
- Consumes: `direct.PortMapper` (Task 1) — Task 4 depends on Task 1; Tasks 1–3 are independent.
- Produces: `direct.OnDemandPort`, `direct.Binder`, and `direct.SignalGate`, reused by Phase 2's agent HTTPS server and open-signal handler.

- [ ] **Step 1: Write the failing test for the on-demand state machine**

```go
// agent/internal/direct/ondemand_test.go
package direct

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// --- fake clock: drives the single state-loop timer deterministically ---

type fakeTimer struct {
	c    chan time.Time
	at   time.Time
	stop bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }
func (t *fakeTimer) Stop() bool {
	if t.stop {
		return false
	}
	t.stop = true
	return true
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) NewTimer(d time.Duration) portTimer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{c: make(chan time.Time, 1), at: f.now.Add(d)}
	f.timers = append(f.timers, t)
	return t
}

// advance moves time forward and fires every non-stopped timer whose deadline
// is reached. Firing happens after unlocking so the loop can re-arm.
func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	var fire []*fakeTimer
	var keep []*fakeTimer
	for _, t := range f.timers {
		if !t.stop && !t.at.After(f.now) {
			fire = append(fire, t)
		} else {
			keep = append(keep, t)
		}
	}
	f.timers = keep
	f.mu.Unlock()
	for _, t := range fire {
		t.stop = true
		select {
		case t.c <- f.now:
		default:
		}
	}
}

// --- recording mapper ---

type recordingMapper struct {
	mu            sync.Mutex
	opened, closed int
	delCalls      int
	lastLease     int
	delFails      int  // the first N DeletePortMapping calls fail
	alwaysFailDel bool
}

func (r *recordingMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opened++
	r.lastLease = lease
	return ext, nil
}
func (r *recordingMapper) DeletePortMapping(ext int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delCalls++
	if r.alwaysFailDel || r.delCalls <= r.delFails {
		return fmt.Errorf("delete failed (attempt %d)", r.delCalls)
	}
	r.closed++
	return nil
}
func (r *recordingMapper) ExternalIP() (string, error) { return "203.0.113.7", nil }

func (r *recordingMapper) ListPortMappings() ([]PortMapping, error) {
	return nil, ErrListingUnsupported
}

func (r *recordingMapper) snapshot() (opened, closed, delCalls, lastLease int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.opened, r.closed, r.delCalls, r.lastLease
}

// --- helpers ---

func newTestPort(fc *fakeClock, mapper PortMapper, idleTimeout time.Duration) *OnDemandPort {
	p := &OnDemandPort{
		mapper:      mapper,
		extPort:     8443,
		intPort:     8443,
		idleTimeout: idleTimeout,
		renewWindow: 2 * time.Second,
		clock:       fc,
		cmds:        make(chan portCommand),
		done:        make(chan struct{}),
	}
	go p.loop()
	return p
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within timeout")
}

// --- tests ---

func TestOnDemandPort_ClosedByDefaultAndClamp(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, time.Minute)
	defer p.Close()

	if p.Open() {
		t.Fatalf("port must be closed by default")
	}
	// A 1ms lease must be clamped up so int(lease.Seconds()) is never 0.
	if err := p.OpenFor("share-1", time.Millisecond); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !p.Open() {
		t.Fatalf("port must be open after OpenFor")
	}
	_, _, _, lastLease := rm.snapshot()
	if lastLease < int(minValidLease.Seconds()) {
		t.Fatalf("lease = %d, want >= %d (clamped)", lastLease, int(minValidLease.Seconds()))
	}
}

func TestOnDemandPort_LeaseExpiresUnusedCloses(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, time.Minute)

	if err := p.OpenFor("share-1", minValidLease); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	fc.advance(minValidLease + time.Second) // lease expiry fires → start close
	waitFor(t, func() bool { return !p.Open() })
	fc.advance(closeRetryDelay + time.Millisecond) // first delete attempt
	waitFor(t, func() bool { _, c, _, _ := rm.snapshot(); return c >= 1 })
}

func TestOnDemandPort_ConcurrentSessionsKeepOpenAndRenew(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, time.Minute)
	defer p.Close()

	if err := p.OpenFor("share-1", 30*time.Second); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	a, err := p.BeginSession("share-1")
	if err != nil {
		t.Fatalf("BeginSession a: %v", err)
	}
	b, err := p.BeginSession("share-1")
	if err != nil {
		t.Fatalf("BeginSession b: %v", err)
	}

	opened, _, _, _ := rm.snapshot()
	// Just past the renewal point (lease - renewWindow): with sessions active
	// the mapping is renewed (AddPortMapping again) instead of closed.
	fc.advance(30*time.Second - 2*time.Second + time.Millisecond)
	waitFor(t, func() bool { o, _, _, _ := rm.snapshot(); return o > opened })
	if !p.Open() {
		t.Fatalf("port must stay open while sessions are active")
	}

	// Ending one session keeps the port open (another is still active).
	p.EndSession(a)
	if !p.Open() {
		t.Fatalf("port must stay open while a session remains")
	}

	// Ending the last session starts the inactivity/lease close path.
	p.EndSession(b)
	fc.advance(time.Minute + time.Second)
	waitFor(t, func() bool { return !p.Open() })
	fc.advance(closeRetryDelay + time.Millisecond)
	waitFor(t, func() bool { _, c, _, _ := rm.snapshot(); return c >= 1 })
}

func TestOnDemandPort_ActivityPostponesIdleClose(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, 20*time.Second) // idle timeout 20s

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	s, err := p.BeginSession("share-1")
	if err != nil {
		t.Fatalf("BeginSession: %v", err)
	}
	// Idle for 19s (just under the timeout), then activity resets it.
	fc.advance(19 * time.Second)
	p.Activity(s)
	fc.advance(19 * time.Second) // 38s since start, only 19s since activity
	if !p.Open() {
		t.Fatalf("port should still be open after activity reset the idle timer")
	}
	fc.advance(2 * time.Second) // now 21s since activity → idle close
	waitFor(t, func() bool { return !p.Open() })
}

func TestOnDemandPort_CloseIdempotent(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{}
	p := newTestPort(fc, rm, time.Minute)

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close (idempotent): %v", err)
	}
	_, closed, _, _ := rm.snapshot()
	if closed != 1 {
		t.Fatalf("DeletePortMapping called %d times, want 1", closed)
	}
}

func TestOnDemandPort_CloseRetriesThenSucceeds(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{delFails: 2}
	p := newTestPort(fc, rm, time.Minute)

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if err := p.Close(); !errors.Is(err, ErrDeleteRetry) {
		t.Fatalf("want ErrDeleteRetry, got %v", err)
	}
	fc.advance(closeRetryDelay + time.Millisecond)
	waitFor(t, func() bool { _, _, d, _ := rm.snapshot(); return d >= 2 })
	fc.advance(closeRetryDelay + time.Millisecond)
	waitFor(t, func() bool { return p.CloseError() == nil })
	_, closed, _, _ := rm.snapshot()
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}
}

func TestOnDemandPort_CloseEscalatesAfterMaxAttempts(t *testing.T) {
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	rm := &recordingMapper{alwaysFailDel: true}
	p := newTestPort(fc, rm, time.Minute)

	if err := p.OpenFor("share-1", time.Minute); err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if err := p.Close(); !errors.Is(err, ErrDeleteRetry) {
		t.Fatalf("want ErrDeleteRetry, got %v", err)
	}
	for i := 0; i < maxCloseAttempts; i++ {
		fc.advance(closeRetryDelay + time.Millisecond)
	}
	waitFor(t, func() bool { _, _, d, _ := rm.snapshot(); return d >= maxCloseAttempts })
	if p.CloseError() == nil {
		t.Fatalf("CloseError must be non-nil after escalation")
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
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// minValidLease is the smallest lease honored. A sub-second lease would
	// collapse to int(lease.Seconds()) == 0, which some UPnP devices treat as a
	// permanent mapping; we clamp up instead.
	minValidLease = 5 * time.Second

	// renewWindow is how early, before lease expiry, the mapping is renewed
	// while any session is active, avoiding a gap where the router reclaims the
	// mapping mid-transfer.
	renewWindow = 2 * time.Second

	// closeRetryDelay is the backoff between DeletePortMapping retries.
	closeRetryDelay = 500 * time.Millisecond

	// maxCloseAttempts bounds deletion retries before the failure is escalated.
	maxCloseAttempts = 5
)

var (
	ErrPortClosed  = errors.New("direct: port is closed")
	ErrDeleteRetry = errors.New("direct: mapping deletion failed; retrying")
)

// portClock abstracts time so tests can drive the single state-loop timer
// deterministically.
type portClock interface {
	Now() time.Time
	NewTimer(d time.Duration) portTimer
}

type portTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

type wallTimer struct{ t *time.Timer }

func (w wallTimer) C() <-chan time.Time { return w.t.C }
func (w wallTimer) Stop() bool          { return w.t.Stop() }

func (wallClock) NewTimer(d time.Duration) portTimer {
	return wallTimer{t: time.NewTimer(d)}
}

type portOp int

const (
	opOpenFor portOp = iota
	opBeginSession
	opActivity
	opEndSession
	opClose
	opIsOpen
)

type portReply struct {
	open      bool
	sessionID string
	err       error
}

type portCommand struct {
	op        portOp
	shareID   string
	lease     time.Duration
	sessionID string
	reply     chan portReply
}

// OnDemandPort manages a public port that is closed by default and opened only
// for agent-verified, active shares. The switch is the PortMapper mapping, not
// a local listener.
//
// Concurrency model: ALL state transitions run on one goroutine (loop). A
// single timer is stopped-and-drained, then re-armed, on every transition, so a
// stale timer can never close a newly renewed mapping (the generation guard).
// The mapping stays open while any recipient session is active and is renewed
// before the lease expires; when the last session ends, an inactivity timeout
// (or the unrenewed lease) closes it. Close is idempotent, and deletion
// failures are retried with backoff then surfaced via CloseError.
type OnDemandPort struct {
	mapper      PortMapper
	extPort     int
	intPort     int
	idleTimeout time.Duration
	renewWindow time.Duration
	clock       portClock

	cmds chan portCommand
	done chan struct{}

	closeMu  sync.RWMutex
	closeErr error
}

func NewOnDemandPort(mapper PortMapper, extPort int) *OnDemandPort {
	return NewOnDemandPortOpts(mapper, extPort, extPort, 5*time.Minute)
}

func NewOnDemandPortOpts(mapper PortMapper, extPort, intPort int, idleTimeout time.Duration) *OnDemandPort {
	p := &OnDemandPort{
		mapper:      mapper,
		extPort:     extPort,
		intPort:     intPort,
		idleTimeout: idleTimeout,
		renewWindow: renewWindow,
		clock:       wallClock{},
		cmds:        make(chan portCommand),
		done:        make(chan struct{}),
	}
	go p.loop()
	return p
}

func (p *OnDemandPort) desc() string { return fmt.Sprintf("sharebridge-direct-%d", p.extPort) }

func (p *OnDemandPort) send(c portCommand) portReply {
	select {
	case p.cmds <- c:
		return <-c.reply
	case <-p.done:
		return portReply{open: false, err: ErrPortClosed}
	}
}

// Open reports whether the port is currently mapped.
func (p *OnDemandPort) Open() bool {
	ch := make(chan portReply, 1)
	return p.send(portCommand{op: opIsOpen, reply: ch}).open
}

// OpenFor maps (or re-maps/renews) the port for shareID with the given lease.
// Leases below minValidLease are clamped up. Only shares the agent has
// independently registered and source-verified may be passed here; the signal
// handler enforces that (see opensignal.go), not this type.
func (p *OnDemandPort) OpenFor(shareID string, lease time.Duration) error {
	ch := make(chan portReply, 1)
	return p.send(portCommand{op: opOpenFor, shareID: shareID, lease: lease, reply: ch}).err
}

// BeginSession records a new recipient session for shareID and returns an
// opaque session token. It fails while the port is closed.
func (p *OnDemandPort) BeginSession(shareID string) (string, error) {
	ch := make(chan portReply, 1)
	r := p.send(portCommand{op: opBeginSession, shareID: shareID, reply: ch})
	return r.sessionID, r.err
}

// Activity records that sessionID is still active, postponing the idle close.
func (p *OnDemandPort) Activity(sessionID string) {
	ch := make(chan portReply, 1)
	p.send(portCommand{op: opActivity, sessionID: sessionID, reply: ch})
}

// EndSession ends sessionID. When the last session ends, the port stays mapped
// only until the inactivity timeout (or the unrenewed lease), then closes.
func (p *OnDemandPort) EndSession(sessionID string) {
	ch := make(chan portReply, 1)
	p.send(portCommand{op: opEndSession, sessionID: sessionID, reply: ch})
}

// Close removes the mapping immediately and idempotently (lockdown or expiry).
// On deletion failure it returns ErrDeleteRetry and retries with backoff; after
// maxCloseAttempts the failure is escalated and stays visible via CloseError.
func (p *OnDemandPort) Close() error {
	ch := make(chan portReply, 1)
	return p.send(portCommand{op: opClose, reply: ch}).err
}

// CloseError returns the last mapping-deletion failure (nil once deletion
// succeeds), for alerting/escalation.
func (p *OnDemandPort) CloseError() error {
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	return p.closeErr
}

func (p *OnDemandPort) setCloseErr(err error) {
	p.closeMu.Lock()
	p.closeErr = err
	p.closeMu.Unlock()
}

func (p *OnDemandPort) loop() {
	defer close(p.done)

	var (
		open      bool
		closing   bool
		lease     time.Duration
		deadline  time.Time // lease expiry if never renewed
		renewAt   time.Time // when to renew (while sessions are active)
		idleAt    time.Time // inactivity close (zero until a session exists)
		sessions  = map[string]time.Time{}
		seq       uint64
		timer     portTimer
		timerC    <-chan time.Time
		closeFail int
	)

	clearTimer := func() {
		if timer != nil {
			if !timer.Stop() {
				select { case <-timer.C(): default: }
			}
		}
		timer, timerC = nil, nil
	}
	arm := func(d time.Duration) {
		clearTimer()
		timer = p.clock.NewTimer(d)
		timerC = timer.C()
	}
	until := func(now, next time.Time) time.Duration {
		d := next.Sub(now)
		if d < 0 {
			return 0
		}
		return d
	}
	rearm := func() {
		clearTimer()
		now := p.clock.Now()
		switch {
		case closing:
			arm(closeRetryDelay)
		case open && len(sessions) > 0:
			next := renewAt
			if idleAt.Before(next) {
				next = idleAt
			}
			arm(until(now, next))
		case open:
			next := deadline
			if !idleAt.IsZero() && idleAt.Before(next) {
				next = idleAt
			}
			arm(until(now, next))
		}
	}
	tryDelete := func() bool {
		if err := p.mapper.DeletePortMapping(p.extPort); err != nil {
			closeFail++
			p.setCloseErr(err)
			return false
		}
		closing = false
		closeFail = 0
		p.setCloseErr(nil)
		return true
	}
	startClose := func() {
		open = false
		closing = true
		closeFail = 0
		rearm() // first DeletePortMapping happens on the next tick
	}

	for {
		select {
		case c := <-p.cmds:
			switch c.op {
			case opOpenFor:
				l := c.lease
				if l < minValidLease {
					l = minValidLease
				}
				if closing {
					closing = false
					closeFail = 0
				}
				if _, err := p.mapper.AddPortMapping(p.extPort, p.intPort, p.desc(), int(l.Seconds())); err != nil {
					c.reply <- portReply{err: err}
					continue
				}
				open = true
				now := p.clock.Now()
				lease = l
				deadline = now.Add(l)
				renewAt = deadline - p.renewWindow
				if !renewAt.After(now) {
					renewAt = now.Add(p.renewWindow)
				}
				rearm()
				c.reply <- portReply{err: nil}

			case opBeginSession:
				if !open {
					c.reply <- portReply{err: ErrPortClosed}
					continue
				}
				seq++
				id := fmt.Sprintf("sess-%d", seq)
				sessions[id] = p.clock.Now()
				idleAt = sessions[id].Add(p.idleTimeout)
				rearm()
				c.reply <- portReply{sessionID: id}

			case opActivity:
				if _, ok := sessions[c.sessionID]; ok {
					now := p.clock.Now()
					sessions[c.sessionID] = now
					idleAt = now.Add(p.idleTimeout)
					rearm()
				}
				c.reply <- portReply{}

			case opEndSession:
				if _, ok := sessions[c.sessionID]; ok {
					delete(sessions, c.sessionID)
					if len(sessions) == 0 {
						idleAt = p.clock.Now().Add(p.idleTimeout)
					}
					rearm()
				}
				c.reply <- portReply{}

			case opClose:
				switch {
				case open:
					open = false
					closing = true
					closeFail = 0
					if tryDelete() {
						c.reply <- portReply{}
					} else {
						rearm()
						c.reply <- portReply{err: ErrDeleteRetry}
					}
				case closing:
					c.reply <- portReply{err: ErrDeleteRetry}
				default:
					c.reply <- portReply{} // idempotent: already closed
				}

			case opIsOpen:
				c.reply <- portReply{open: open}
			}

		case <-timerC:
			// The timer can only be the one armed for the current state:
			// clearTimer stops+drains before every re-arm, so a stale fire is
			// impossible. Each branch re-derives from current state.
			switch {
			case closing:
				if tryDelete() {
					rearm()
				} else if closeFail >= maxCloseAttempts {
					closing = false
					rearm() // mapping remains; failure surfaced via CloseError
				} else {
					rearm()
				}
			case open && len(sessions) > 0:
				now := p.clock.Now()
				if !now.Before(idleAt) {
					startClose()
				} else if !now.Before(renewAt) {
					if _, err := p.mapper.AddPortMapping(p.extPort, p.intPort, p.desc(), int(lease.Seconds())); err != nil {
						renewAt = deadline // stop renewing; close at lease expiry
					} else {
						deadline = now.Add(lease)
						renewAt = deadline - p.renewWindow
					}
					rearm()
				} else {
					rearm()
				}
			case open:
				now := p.clock.Now()
				if !idleAt.IsZero() && !now.Before(idleAt) {
					startClose()
				} else if !now.Before(deadline) {
					startClose()
				} else {
					rearm()
				}
			}
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./internal/direct/ -run TestOnDemandPort -v`
Expected: PASS.

- [ ] **Step 5: Write the failing test for SNI + origin→share binding (real TLS + HTTP)**

```go
// agent/internal/direct/sni_test.go
package direct

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testNamespace = "v7q4km2x9pz6dn3w"
const directOrigin = "r7k2m9p4x6.v7q4km2x9pz6dn3w.sharebridgeusercontent.com"
const relayOrigin = "r7k2m9p4x6.relay.v7q4km2x9pz6dn3w.sharebridgeusercontent.com"

func startBindServer(t *testing.T, b *Binder) *httptest.Server {
	t.Helper()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	srv := httptest.NewUnstartedServer(b.Handler(ok))
	srv.TLS = b.TLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func tlsClient(sni string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName:         sni,
			InsecureSkipVerify: true, // test only: self-signed httptest cert
		},
	}}
}

func get(t *testing.T, c *http.Client, srvURL, host, path string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srvURL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Host = host
	return c.Do(req)
}

func TestBinder_EndToEndAuthorization(t *testing.T) {
	b := NewBinder(testNamespace)
	if err := b.Allow(directOrigin, RouteDirect, "code-1"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	srv := startBindServer(t, b)

	// Correct SNI + Host + code → 200.
	c := tlsClient(directOrigin)
	resp, err := get(t, c, srv.URL, directOrigin, "/s/code-1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown SNI → handshake rejected before any HTTP is read.
	c2 := tlsClient("other." + testNamespace + ".sharebridgeusercontent.com")
	if _, err := get(t, c2, srv.URL, "other."+testNamespace+".sharebridgeusercontent.com", "/s/code-1"); err == nil {
		t.Fatalf("unknown SNI: want handshake error, got nil")
	}

	// Known SNI but wrong Host → 403.
	c3 := tlsClient(directOrigin)
	resp, err = get(t, c3, srv.URL, "evil.example.com", "/s/code-1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong Host: status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Known SNI + Host but wrong code → 403.
	resp, err = get(t, c3, srv.URL, directOrigin, "/s/wrong-code")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong code: status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBinder_EmptySNIRejected(t *testing.T) {
	b := NewBinder(testNamespace)
	_ = b.Allow(directOrigin, RouteDirect, "code-1")
	srv := startBindServer(t, b)

	// Empty ServerName + IP dial address → the client omits SNI, so the server
	// sees an empty SNI and rejects the handshake.
	c := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/s/code-1", nil)
	req.Host = directOrigin
	if _, err := c.Do(req); err == nil {
		t.Fatalf("empty SNI: want handshake error, got nil")
	}
}

func TestBinder_Normalization(t *testing.T) {
	b := NewBinder(testNamespace)
	// Register with uppercase + trailing dot; requests arrive lowercase/no-dot.
	if err := b.Allow("R7K2M9P4X6.V7Q4KM2X9PZ6DN3W.sharebridgeusercontent.com.", RouteDirect, "code-1"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	srv := startBindServer(t, b)
	c := tlsClient(directOrigin)

	// Host with port is accepted (port ignored in comparison).
	resp, err := get(t, c, srv.URL, directOrigin+":8443", "/s/code-1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Host with port: status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBinder_Revocation(t *testing.T) {
	b := NewBinder(testNamespace)
	_ = b.Allow(directOrigin, RouteDirect, "code-1")
	if bd, err := b.AdmitSNI(directOrigin); err != nil || bd.Origin != directOrigin {
		t.Fatalf("AdmitSNI before revoke: %v", err)
	}
	b.Revoke(directOrigin)
	if _, err := b.AdmitSNI(directOrigin); !errors.Is(err, ErrUnknownOrigin) {
		t.Fatalf("after revoke: want ErrUnknownOrigin, got %v", err)
	}
}

func TestBinder_DuplicateAndMalformedHost(t *testing.T) {
	b := NewBinder(testNamespace)
	_ = b.Allow(directOrigin, RouteDirect, "code-1")
	bd, _ := b.AdmitSNI(directOrigin)

	// Duplicate Host headers → rejected.
	req := httptest.NewRequest(http.MethodGet, "https://"+directOrigin+"/s/code-1", nil)
	req.Host = directOrigin
	req.Header["Host"] = []string{directOrigin, directOrigin}
	if err := b.Authorize(bd, req); !errors.Is(err, ErrBadHost) {
		t.Fatalf("duplicate Host: want ErrBadHost, got %v", err)
	}

	// Malformed Host (whitespace) → rejected.
	req2 := httptest.NewRequest(http.MethodGet, "https://"+directOrigin+"/s/code-1", nil)
	req2.Host = directOrigin + " evil"
	if err := b.Authorize(bd, req2); !errors.Is(err, ErrBadHost) {
		t.Fatalf("malformed Host: want ErrBadHost, got %v", err)
	}
}

func TestBinder_AbsoluteFormRequestTarget(t *testing.T) {
	b := NewBinder(testNamespace)
	_ = b.Allow(directOrigin, RouteDirect, "code-1")
	bd, _ := b.AdmitSNI(directOrigin)

	// Absolute-form request-target: GET http://origin/s/code HTTP/1.1. Go's
	// server sets r.Host from the request-target authority, so authorization
	// is unchanged.
	req := httptest.NewRequest(http.MethodGet, "http://"+directOrigin+"/s/code-1", nil)
	req.Host = directOrigin
	req.RequestURI = "http://" + directOrigin + "/s/code-1"
	if err := b.Authorize(bd, req); err != nil {
		t.Fatalf("absolute-form: %v", err)
	}
}

func TestBinder_WrongRouteKindAndWrongCode(t *testing.T) {
	b := NewBinder(testNamespace)

	// Wrong route kind at registration time is rejected.
	if err := b.Allow(directOrigin, RouteRelay, "code-1"); !errors.Is(err, ErrWrongRouteKind) {
		t.Fatalf("Allow wrong kind: want ErrWrongRouteKind, got %v", err)
	}

	_ = b.Allow(directOrigin, RouteDirect, "code-1")

	// Request-time double-check: a binding mislabeled with the wrong route kind
	// (injected directly) is rejected during HTTP authorization.
	bad := Binding{Origin: normalizeHost(directOrigin), RouteKind: RouteRelay, ShareCode: "code-1"}
	req := httptest.NewRequest(http.MethodGet, "https://"+directOrigin+"/s/code-1", nil)
	req.Host = directOrigin
	if err := b.Authorize(bad, req); !errors.Is(err, ErrWrongRouteKind) {
		t.Fatalf("request wrong kind: want ErrWrongRouteKind, got %v", err)
	}

	// Wrong share code at request time.
	bd, _ := b.AdmitSNI(directOrigin)
	req2 := httptest.NewRequest(http.MethodGet, "https://"+directOrigin+"/s/other-code", nil)
	req2.Host = directOrigin
	if err := b.Authorize(bd, req2); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("wrong code: want ErrWrongCode, got %v", err)
	}
}

func TestBinder_BothNamespaces(t *testing.T) {
	b := NewBinder(testNamespace)
	if err := b.Allow(directOrigin, RouteDirect, "code-d"); err != nil {
		t.Fatalf("Allow direct: %v", err)
	}
	if err := b.Allow(relayOrigin, RouteRelay, "code-r"); err != nil {
		t.Fatalf("Allow relay: %v", err)
	}

	bd, err := b.AdmitSNI(directOrigin)
	if err != nil {
		t.Fatalf("direct SNI: %v", err)
	}
	if bd.RouteKind != RouteDirect {
		t.Fatalf("direct kind = %s", bd.RouteKind)
	}
	br, err := b.AdmitSNI(relayOrigin)
	if err != nil {
		t.Fatalf("relay SNI: %v", err)
	}
	if br.RouteKind != RouteRelay {
		t.Fatalf("relay kind = %s", br.RouteKind)
	}

	// A request whose Host is the relay origin must not authorize against the
	// direct binding (cross-namespace bleed is rejected).
	reqRelay := httptest.NewRequest(http.MethodGet, "https://"+relayOrigin+"/s/code-r", nil)
	reqRelay.Host = relayOrigin
	if err := b.Authorize(bd, reqRelay); !errors.Is(err, ErrHostMismatch) {
		t.Fatalf("cross-namespace: want ErrHostMismatch, got %v", err)
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `cd agent && go test ./internal/direct/ -run TestBinder -v`
Expected: FAIL — `undefined: Binder`.

- [ ] **Step 7: Implement SNI admission + HTTP origin→share binding**

```go
// agent/internal/direct/sni.go
package direct

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
)

// RouteKind distinguishes the two DNS namespaces (spec §7): the clean
// <origin>.<ns>.sharebridgeusercontent.com namespace (direct) and the
// <origin>.relay.<ns>.sharebridgeusercontent.com namespace (relay).
type RouteKind string

const (
	RouteDirect RouteKind = "direct"
	RouteRelay  RouteKind = "relay"
)

// Binding is an active origin→share authorization: an exact, normalized origin
// hostname bound to its route kind and the native share code that authorizes
// content for it (spec §11).
type Binding struct {
	Origin    string
	RouteKind RouteKind
	ShareCode string
}

var (
	ErrNoSNI          = errors.New("direct: empty or invalid SNI")
	ErrUnknownOrigin  = errors.New("direct: unknown or revoked origin")
	ErrHostMismatch   = errors.New("direct: Host does not match the admitted origin")
	ErrBadHost        = errors.New("direct: malformed or duplicate Host header")
	ErrWrongRouteKind = errors.New("direct: route kind mismatch")
	ErrWrongCode      = errors.New("direct: share code mismatch")
)

// Binder enforces strict origin→share binding at the agent. TLS admission
// (SNI) is separate from HTTP authorization (Host + route kind + share code):
// an unknown SNI is rejected during the handshake, before any HTTP is read;
// a known SNI is then authorized per-request against Host, route kind, and the
// native share code.
type Binder struct {
	mu           sync.RWMutex
	namespace    string
	directSuffix string
	relaySuffix  string
	active       map[string]Binding
}

func NewBinder(namespace string) *Binder {
	return &Binder{
		namespace:    namespace,
		directSuffix: "." + namespace + ".sharebridgeusercontent.com",
		relaySuffix:  ".relay." + namespace + ".sharebridgeusercontent.com",
		active:       map[string]Binding{},
	}
}

// routeKindFor classifies a normalized origin into its namespace route kind.
func (b *Binder) routeKindFor(host string) (RouteKind, bool) {
	switch {
	case strings.HasSuffix(host, b.relaySuffix):
		return RouteRelay, true
	case strings.HasSuffix(host, b.directSuffix):
		return RouteDirect, true
	default:
		return "", false
	}
}

// Allow registers an origin→share binding. It refuses origins outside the
// agent's two namespaces and refuses a route kind that does not match the
// origin's namespace (belt-and-suspenders; the request path re-checks too).
func (b *Binder) Allow(origin string, kind RouteKind, shareCode string) error {
	host := normalizeHost(origin)
	if !validHostname(host) {
		return fmt.Errorf("direct: invalid origin %q", origin)
	}
	k, ok := b.routeKindFor(host)
	if !ok {
		return fmt.Errorf("direct: origin %q is not in the agent namespace %q", origin, b.namespace)
	}
	if k != kind {
		return fmt.Errorf("%w: %q is %s, not %s", ErrWrongRouteKind, host, k, kind)
	}
	b.mu.Lock()
	b.active[host] = Binding{Origin: host, RouteKind: kind, ShareCode: shareCode}
	b.mu.Unlock()
	return nil
}

func (b *Binder) Revoke(origin string) {
	host := normalizeHost(origin)
	b.mu.Lock()
	delete(b.active, host)
	b.mu.Unlock()
}

// AdmitSNI is TLS admission: the ClientHello SNI must be a non-empty, exact,
// active origin. Rejecting here fails the handshake before any HTTP is read.
func (b *Binder) AdmitSNI(serverName string) (Binding, error) {
	host := normalizeHost(serverName)
	if host == "" || !validHostname(host) {
		return Binding{}, ErrNoSNI
	}
	b.mu.RLock()
	bd, ok := b.active[host]
	b.mu.RUnlock()
	if !ok {
		return Binding{}, fmt.Errorf("%w: %q", ErrUnknownOrigin, serverName)
	}
	return bd, nil
}

// Authorize performs HTTP authorization for a connection already admitted by
// AdmitSNI: Host must equal the admitted origin (ignoring case, trailing dot,
// and port), the route kind must match the Host's namespace, and the request
// path must carry the binding's native share code.
func (b *Binder) Authorize(bd Binding, r *http.Request) error {
	host, ok := normalizeRequestHost(r)
	if !ok {
		return ErrBadHost
	}
	if host != bd.Origin {
		return fmt.Errorf("%w: Host %q, admitted origin %q", ErrHostMismatch, host, bd.Origin)
	}
	if k, _ := b.routeKindFor(host); k != bd.RouteKind {
		return fmt.Errorf("%w: %q is %s, binding is %s", ErrWrongRouteKind, host, k, bd.RouteKind)
	}
	code, ok := shareCodeFromPath(r)
	if !ok || code != bd.ShareCode {
		return ErrWrongCode
	}
	return nil
}

// TLSConfig returns a tls.Config whose GetConfigForClient performs SNI
// admission. Unknown/empty SNI aborts the handshake; known SNI proceeds.
func (b *Binder) TLSConfig() *tls.Config {
	return &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			if _, err := b.AdmitSNI(hello.ServerName); err != nil {
				return nil, err
			}
			return nil, nil
		},
	}
}

// Handler wraps next with HTTP authorization, re-deriving the admitted binding
// from the connection's SNI (r.TLS.ServerName). Wire it as:
//
//	server := &http.Server{Handler: binder.Handler(actualHandler)}
//	server.TLSConfig = binder.TLSConfig()
func (b *Binder) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sni := ""
		if r.TLS != nil {
			sni = r.TLS.ServerName
		}
		bd, err := b.AdmitSNI(sni)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if err := b.Authorize(bd, r); err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// normalizeHost lowercases, strips a trailing dot, and strips an optional
// ":port" so Host and SNI compare on the bare hostname.
func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// validHostname accepts only lowercase DNS labels (letters, digits, hyphen,
// dot). This rejects whitespace, underscores, and other malformed Host values.
func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// normalizeRequestHost extracts a single, well-formed Host value from the
// request, falling back to r.Host (HTTP/2 :authority) when no Host header is
// present, and rejecting duplicate Host headers.
func normalizeRequestHost(r *http.Request) (string, bool) {
	vals := r.Header.Values("Host")
	switch len(vals) {
	case 0:
		if r.Host == "" {
			return "", false
		}
		h := normalizeHost(r.Host)
		if !validHostname(h) {
			return "", false
		}
		return h, true
	case 1:
		h := normalizeHost(vals[0])
		if !validHostname(h) {
			return "", false
		}
		return h, true
	default:
		return "", false
	}
}

// shareCodeFromPath extracts the native share code from a /s/<code> path.
func shareCodeFromPath(r *http.Request) (string, bool) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "s" && parts[1] != "" {
		return parts[1], true
	}
	return "", false
}
```

- [ ] **Step 8: Run test to verify it passes**

Run: `cd agent && go test ./internal/direct/ -run TestBinder -v`
Expected: PASS.

- [ ] **Step 9: Write the failing test for the open-signal protocol (replay/reorder/reuse)**

```go
// agent/internal/direct/opensignal_test.go
package direct

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSignalGate_ReplayReorderReuse(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gate := NewSignalGate("agent-1", func(shareID string, k RouteKind) bool {
		return shareID == "share-a" || shareID == "share-b"
	})
	gate.now = func() time.Time { return now }

	mk := func(shareID, nonce string) OpenSignal {
		return OpenSignal{
			Version: signalVersion, AgentID: "agent-1",
			ShareID: shareID, RouteKind: RouteDirect, Nonce: nonce,
			ExpiresAt: now.Add(time.Minute), Lease: 30 * time.Second,
		}
	}

	if err := gate.Admit(mk("share-a", "n1")); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	// Replay: same nonce, same share → ErrReplaySignal.
	if err := gate.Admit(mk("share-a", "n1")); !errors.Is(err, ErrReplaySignal) {
		t.Fatalf("replay: want ErrReplaySignal, got %v", err)
	}
	// Reuse: same nonce, different share → ErrNonceReuse.
	if err := gate.Admit(mk("share-b", "n1")); !errors.Is(err, ErrNonceReuse) {
		t.Fatalf("reuse: want ErrNonceReuse, got %v", err)
	}
	// Reorder: a newer nonce is accepted, then the old nonce is replayed.
	if err := gate.Admit(mk("share-a", "n2")); err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if err := gate.Admit(mk("share-a", "n1")); !errors.Is(err, ErrReplaySignal) {
		t.Fatalf("reorder: want ErrReplaySignal, got %v", err)
	}
}

func TestSignalGate_ExpiryLockdownSourceAuthRateLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gate := NewSignalGate("agent-1", func(shareID string, k RouteKind) bool {
		return shareID == "share-a"
	})
	gate.now = func() time.Time { return now }

	mk := func() OpenSignal {
		return OpenSignal{
			Version: signalVersion, AgentID: "agent-1", ShareID: "share-a",
			RouteKind: RouteDirect, Nonce: "n", ExpiresAt: now.Add(time.Minute),
			Lease: 30 * time.Second,
		}
	}

	s := mk()
	s.Version = 99
	s.Nonce = "v"
	if err := gate.Admit(s); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("version: %v", err)
	}
	s = mk()
	s.AgentID = "agent-2"
	s.Nonce = "a"
	if err := gate.Admit(s); !errors.Is(err, ErrWrongAgent) {
		t.Fatalf("agent: %v", err)
	}
	s = mk()
	s.ExpiresAt = now.Add(-time.Second)
	s.Nonce = "e"
	if err := gate.Admit(s); !errors.Is(err, ErrExpiredSignal) {
		t.Fatalf("expired: %v", err)
	}
	s = mk()
	s.ShareID = "not-registered"
	s.Nonce = "u"
	if err := gate.Admit(s); !errors.Is(err, ErrSignalNotAuth) {
		t.Fatalf("unregistered: %v", err)
	}
	gate.SetLockdown(true)
	s = mk()
	s.Nonce = "l"
	if err := gate.Admit(s); !errors.Is(err, ErrSignalLockdown) {
		t.Fatalf("lockdown: %v", err)
	}
	gate.SetLockdown(false)

	// Rate limit: per-share limit exceeded.
	for i := 0; i < maxPerSharePerWin; i++ {
		nm := mk()
		nm.Nonce = fmt.Sprintf("r%d", i)
		if err := gate.Admit(nm); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	s = mk()
	s.Nonce = "overflow"
	if err := gate.Admit(s); !errors.Is(err, ErrSignalRate) {
		t.Fatalf("rate limit: %v", err)
	}
}
```

- [ ] **Step 10: Run test to verify it fails**

Run: `cd agent && go test ./internal/direct/ -run TestSignalGate -v`
Expected: FAIL — `undefined: SignalGate`.

- [ ] **Step 11: Implement the open-signal gate**

```go
// agent/internal/direct/opensignal.go
package direct

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// OpenSignal is a short-lived, versioned, idempotent request to open the public
// port for one share. It is bound to (agent, share, route, nonce) and carries
// an expiration and a per-share open lease (spec §5.1).
type OpenSignal struct {
	Version   int
	AgentID   string
	ShareID   string
	RouteKind RouteKind
	Nonce     string
	ExpiresAt time.Time
	Lease     time.Duration
}

const signalVersion = 1

var (
	ErrBadVersion     = errors.New("direct: unsupported open-signal version")
	ErrWrongAgent     = errors.New("direct: open signal for a different agent")
	ErrExpiredSignal  = errors.New("direct: open signal expired")
	ErrReplaySignal   = errors.New("direct: replayed open signal")
	ErrNonceReuse     = errors.New("direct: nonce reused for a different share/route")
	ErrSignalLockdown = errors.New("direct: open signals refused during lockdown")
	ErrSignalNotAuth  = errors.New("direct: share not registered/source-authorized locally")
	ErrSignalRate     = errors.New("direct: open-signal rate limit exceeded")
)

// shareAuthorizer reports whether the agent has independently registered and
// source-verified a share for a route. The signal only ACTIVATES a route the
// agent already knows; it never creates one.
type shareAuthorizer func(shareID string, kind RouteKind) bool

// SignalGate validates open signals at the agent. It drops expired, replayed,
// or nonce-reused signals, refuses all signals during lockdown, re-verifies
// local source authorization, and rate-limits per window and per share.
type SignalGate struct {
	mu       sync.Mutex
	agentID  string
	lockdown bool
	now      func() time.Time
	authz    shareAuthorizer

	seen    map[string]nonceUse
	applied map[string]time.Time // "share:route" -> appliedAt

	winStart time.Time
	winCount int
	perShare map[string]int
}

type nonceUse struct {
	shareID string
	kind    RouteKind
	seenAt  time.Time
}

const (
	signalWindow      = time.Minute
	maxSignalsPerWin  = 10
	maxPerSharePerWin = 3
)

func NewSignalGate(agentID string, authz shareAuthorizer) *SignalGate {
	return &SignalGate{
		agentID:  agentID,
		now:      time.Now,
		authz:    authz,
		seen:     map[string]nonceUse{},
		applied:  map[string]time.Time{},
		perShare: map[string]int{},
	}
}

func (g *SignalGate) SetLockdown(on bool) {
	g.mu.Lock()
	g.lockdown = on
	g.mu.Unlock()
}

// Admit validates one open signal. It returns nil when the signal may proceed
// (the caller then calls OnDemandPort.OpenFor, which refreshes the lease if the
// share is already applied, making the signal idempotent).
func (g *SignalGate) Admit(sig OpenSignal) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()

	if sig.Version != signalVersion {
		return fmt.Errorf("%w: got %d want %d", ErrBadVersion, sig.Version, signalVersion)
	}
	if sig.AgentID != g.agentID {
		return ErrWrongAgent
	}
	if !sig.ExpiresAt.After(now) {
		return ErrExpiredSignal
	}
	if u, ok := g.seen[sig.Nonce]; ok {
		if u.shareID != sig.ShareID || u.kind != sig.RouteKind {
			return ErrNonceReuse
		}
		return ErrReplaySignal
	}
	if g.lockdown {
		return ErrSignalLockdown
	}
	if !g.authz(sig.ShareID, sig.RouteKind) {
		return ErrSignalNotAuth
	}
	if err := g.rateLimit(now, sig.ShareID); err != nil {
		return err
	}

	g.seen[sig.Nonce] = nonceUse{shareID: sig.ShareID, kind: sig.RouteKind, seenAt: now}
	g.applied[sig.ShareID+":"+string(sig.RouteKind)] = now

	// Best-effort prune of expired nonce entries to bound memory.
	for nonce, u := range g.seen {
		if now.Sub(u.seenAt) > signalWindow*4 {
			delete(g.seen, nonce)
		}
	}
	return nil
}

func (g *SignalGate) rateLimit(now time.Time, shareID string) error {
	if now.Sub(g.winStart) >= signalWindow {
		g.winStart = now
		g.winCount = 0
		g.perShare = map[string]int{}
	}
	if g.winCount >= maxSignalsPerWin {
		return ErrSignalRate
	}
	if g.perShare[shareID] >= maxPerSharePerWin {
		return ErrSignalRate
	}
	g.winCount++
	g.perShare[shareID]++
	return nil
}
```

- [ ] **Step 12: Run test to verify it passes**

Run: `cd agent && go test ./internal/direct/ -run TestSignalGate -v`
Expected: PASS.

- [ ] **Step 13: Write the findings doc and commit**

Write `docs/superpowers/spikes/2026-08-14-ondemand-sni.md` covering: closed-by-default + on-demand semantics, the single generation-guarded state loop (one timer, stopped+drained then re-armed on every transition, so a stale timer can never close a renewed mapping), the minimum-lease clamp (sub-second leases would become `0` seconds, which some UPnP devices treat as permanent), renewal-before-expiry, recipient/session reference counting with activity tracking and idle-timeout close, idempotent `Close` with observable delete retry/escalation, SNI admission vs HTTP authorization (Host + route kind + native share code), both DNS namespaces (direct and `.relay.`), and the open-signal gate (versioned, expiring, idempotent, `(agent, share, route, nonce)`-bound, source-re-verified, lockdown-refusing, rate-limited). Note the strict-open-signal rule: the signal handler must call `SignalGate.Admit` first and only pass agent-registered, source-verified shares to `OnDemandPort.OpenFor`. Then:

```bash
git add agent/internal/direct/ docs/superpowers/spikes/
git commit -m "spike: on-demand port + SNI binding + open-signal prototype"
```

---

## Phase Gate

After all four tasks, the spikes together answer whether direct-TCP is viable:

- Task 1 → UPnP/NAT-PMP reachability works (and how often).
- Task 2 → wildcard cert + DDNS issuance works with the chosen CA/DNS.
- Task 3 → direct-TCP matches or exceeds relay throughput (no SCTP collapse).
- Task 4 → closed-by-default + on-demand + SNI binding behave as designed.

Any blocker here returns to the spec before Phase 2. Specifically: a failed certificate/DNS spike blocks the direct path entirely (no cert = no direct HTTPS); a failed throughput spike means direct does not become the default (relay remains); a failed UPnP spike does not block the direction but determines how often direct is available; a failed binding spike must be fixed before any production direct serving. Otherwise Phase 2 (agent HTTPS server + direct wiring + Immich migration) gets its own plan.

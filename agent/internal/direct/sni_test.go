// agent/internal/direct/sni_test.go
package direct

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testNamespace = "v7q4km2x9pz6dn3w"
const testBaseDomain = "sharebridgeusercontent.com"
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
	b := NewBinder(testNamespace, testBaseDomain)
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
	b := NewBinder(testNamespace, testBaseDomain)
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
	b := NewBinder(testNamespace, testBaseDomain)
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
	b := NewBinder(testNamespace, testBaseDomain)
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
	b := NewBinder(testNamespace, testBaseDomain)
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
	b := NewBinder(testNamespace, testBaseDomain)
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
	b := NewBinder(testNamespace, testBaseDomain)

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

func TestBinder_BothNamespaces(t *testing.T) {
	b := NewBinder(testNamespace, testBaseDomain)
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

// TestBinderRelayOriginForDerivesNamespacePair pins the agent-side §6
// derivation: the relay origin for a control-allocated direct origin is the
// same origin label under the .relay.<namespace>.<base> namespace — the same
// rule the control plane applies, so both sides compute the same hostname.
// Already-relay and foreign-namespace origins are rejected.
func TestBinderRelayOriginForDerivesNamespacePair(t *testing.T) {
	b := NewBinder(testNamespace, testBaseDomain)

	got, err := b.RelayOriginFor(directOrigin)
	if err != nil {
		t.Fatalf("RelayOriginFor: %v", err)
	}
	if got != relayOrigin {
		t.Fatalf("RelayOriginFor = %q, want %q", got, relayOrigin)
	}

	// An origin that is already a relay origin must never be paired again.
	if _, err := b.RelayOriginFor(relayOrigin); err == nil {
		t.Fatalf("RelayOriginFor of a relay origin must be rejected (no relay-of-relay)")
	}
	// Foreign-namespace origins have no pair in this binder.
	if _, err := b.RelayOriginFor("r7k2m9p4x6.other.example.com"); err == nil {
		t.Fatalf("RelayOriginFor of a foreign origin must be rejected")
	}
	// A bare origin with no label before the namespace is malformed.
	if _, err := b.RelayOriginFor(testNamespace + "." + testBaseDomain); err == nil {
		t.Fatalf("RelayOriginFor of a label-less origin must be rejected")
	}
}

// TestBinderAllowShareAdmitsBothRouteKinds pins §13.1: one operation admits
// BOTH §6 origins of a content session — direct under RouteDirect and relay
// under RouteRelay — bound to the same share code. A validation failure on
// either origin admits neither (atomic pair).
func TestBinderAllowShareAdmitsBothRouteKinds(t *testing.T) {
	b := NewBinder(testNamespace, testBaseDomain)
	if err := b.AllowShare(directOrigin, relayOrigin, "code-pair"); err != nil {
		t.Fatalf("AllowShare: %v", err)
	}

	db, err := b.AdmitSNI(directOrigin)
	if err != nil {
		t.Fatalf("direct SNI not admitted after AllowShare: %v", err)
	}
	rb, err := b.AdmitSNI(relayOrigin)
	if err != nil {
		t.Fatalf("relay SNI not admitted after AllowShare: %v", err)
	}
	if db.RouteKind != RouteDirect || rb.RouteKind != RouteRelay {
		t.Fatalf("route kinds = %s/%s, want direct/relay", db.RouteKind, rb.RouteKind)
	}
	if db.ShareCode != "code-pair" || rb.ShareCode != "code-pair" {
		t.Fatalf("share codes = %q/%q, want both code-pair (one content session)", db.ShareCode, rb.ShareCode)
	}

	// Atomicity: a mismatched relay origin (foreign namespace) leaves the
	// previous state completely untouched — neither origin is rebound.
	if err := b.AllowShare(directOrigin, "other.relay.other.example.com", "code-pair"); err == nil {
		t.Fatalf("AllowShare with a foreign relay origin must fail")
	}
	rb2, err := b.AdmitSNI(relayOrigin)
	if err != nil || rb2.ShareCode != "code-pair" || rb2.RouteKind != RouteRelay {
		t.Fatalf("failed AllowShare mutated existing relay binding: %+v err=%v", rb2, err)
	}
}

// TestBinderRevokeShareRemovesBoth pins §6/§13.1: revocation removes BOTH
// bindings of the pair in one operation.
func TestBinderRevokeShareRemovesBoth(t *testing.T) {
	b := NewBinder(testNamespace, testBaseDomain)
	if err := b.AllowShare(directOrigin, relayOrigin, "code-pair"); err != nil {
		t.Fatalf("AllowShare: %v", err)
	}

	b.RevokeShare(directOrigin, relayOrigin)
	if _, err := b.AdmitSNI(directOrigin); err == nil {
		t.Fatalf("direct origin still admitted after RevokeShare")
	}
	if _, err := b.AdmitSNI(relayOrigin); err == nil {
		t.Fatalf("relay origin still admitted after RevokeShare")
	}
}

// TestSignalGateStillRejectsRouteRelay pins §13.1: even though the Binder now
// admits both route kinds for a share, the SignalGate stays direct-only — a
// RouteRelay open signal can never open the direct public port, regardless of
// what the source-authorization callback would say.
func TestSignalGateStillRejectsRouteRelay(t *testing.T) {
	b := NewBinder(testNamespace, testBaseDomain)
	if err := b.AllowShare(directOrigin, relayOrigin, "code-1"); err != nil {
		t.Fatalf("AllowShare: %v", err)
	}

	// The authorization callback reflects the share being bound for either
	// route (the Binder admits both) — the gate must STILL refuse relay.
	gate := NewSignalGate("agent-1", func(shareID string, kind RouteKind) bool {
		return shareID == "code-1"
	})

	base := OpenSignal{
		Version:   1,
		AgentID:   "agent-1",
		ShareID:   "code-1",
		Nonce:     "nonce-relay",
		Seq:       1,
		ExpiresAt: time.Now().Add(time.Minute),
		Lease:     30 * time.Second,
	}

	relay := base
	relay.RouteKind = RouteRelay
	if err := gate.Admit(relay); !errors.Is(err, ErrWrongRouteKind) {
		t.Fatalf("RouteRelay open signal: want ErrWrongRouteKind, got %v", err)
	}

	// Contrast: the same signal under RouteDirect is admissible, proving the
	// rejection is the direct-only policy and not a broken gate.
	direct := base
	direct.RouteKind = RouteDirect
	if err := gate.Admit(direct); err != nil {
		t.Fatalf("RouteDirect open signal: %v", err)
	}
}

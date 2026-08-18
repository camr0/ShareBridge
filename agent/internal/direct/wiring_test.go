// agent/internal/direct/wiring_test.go
package direct

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeResolver resolves exactly the codes present in sessions and returns
// ErrUnknown for every other code.
type fakeResolver struct {
	sessions map[string]*ContentSession
}

func (f *fakeResolver) Resolve(code string) (*ContentSession, error) {
	if s, ok := f.sessions[code]; ok {
		return s, nil
	}
	return nil, ErrUnknown
}

// stubResolver resolves every code to a minimal session. It keeps the
// placeholder page/download tests green now that route() fails closed without
// a resolver.
type stubResolver struct{}

func (stubResolver) Resolve(code string) (*ContentSession, error) {
	return &ContentSession{Membership: map[string]struct{}{}}, nil
}

// admittedRequest builds a request carrying the SNI and Host the Binder's HTTP
// authorization expects, so it reaches route() without a live TLS server.
func admittedRequest(method, target, origin string) *http.Request {
	req := httptest.NewRequest(method, "http://"+origin+target, nil)
	req.TLS = &tls.ConnectionState{ServerName: origin}
	return req
}

func TestWiringSetResolverConsultsResolverOnContentRoutes(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	knownOrigin := "known." + ns + "." + base
	unknownOrigin := "unknown." + ns + "." + base
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, nil, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow(knownOrigin, RouteDirect, "known")
	_ = srv.Binder().Allow(unknownOrigin, RouteDirect, "unknown")

	srv.SetResolver(&fakeResolver{sessions: map[string]*ContentSession{
		"known": {Membership: map[string]struct{}{}},
	}})

	handler := srv.Handler()

	// A resolved code reaches the handler (placeholder page).
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, admittedRequest(http.MethodGet, "/s/known", knownOrigin))
	if rr.Code != http.StatusOK {
		t.Fatalf("resolved code: status = %d, want 200", rr.Code)
	}

	// An unresolvable code → 404.
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, admittedRequest(http.MethodGet, "/s/unknown", unknownOrigin))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unresolvable code: status = %d, want 404", rr.Code)
	}
}

func TestWiringRouteFailsClosedWithoutResolver(t *testing.T) {
	ns, base := "sbdeadbeef", "example.com"
	origin := "demo." + ns + "." + base
	cert := testServerCert(t, ns, base)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, nil, &rotatableCerts{cert}, gate, 1<<20)
	_ = srv.Binder().Allow(origin, RouteDirect, "abc")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, admittedRequest(http.MethodGet, "/s/abc", origin))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("nil resolver: status = %d, want 404", rr.Code)
	}
}

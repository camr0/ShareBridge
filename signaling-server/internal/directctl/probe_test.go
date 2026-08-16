// probe_test.go
package directctl

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func mustPort(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("parse port %q: %v", s, err)
	}
	return n
}

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
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotSNI = r.TLS.ServerName
		if r.URL.Query().Get("nonce") != "n1" {
			w.WriteHeader(403)
			return
		}
		if r.URL.Query().Get("redirect") == "1" {
			http.Redirect(w, r, "https://should-not-be-followed/", http.StatusFound)
			return
		}
		w.WriteHeader(200)
		io.WriteString(w, "n1")
	}))
	ts.StartTLS()
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	port := mustPort(t, u.Port())
	err := ctrl.Probe(context.Background(), "demo.sb1.example.com", "abc", "key-1", OpenAck{PublicIP: "127.0.0.1", GrantedPort: port, Nonce: "n1"})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gotHost != "demo.sb1.example.com" {
		t.Fatalf("Host = %q", gotHost)
	}
	if gotSNI != "demo.sb1.example.com" {
		t.Fatalf("SNI = %q", gotSNI)
	}
}

func TestProbeDoesNotFollowRedirect(t *testing.T) {
	_, ctrl := newTestController(t)
	ctrl.allowPrivate = true
	var targetHit bool
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/followed" {
			targetHit = true
			return
		}
		http.Redirect(w, r, "/followed", http.StatusFound)
	}))
	ts.StartTLS()
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	port := mustPort(t, u.Port())
	// A redirect means the nonce is not echoed → the probe must fail, and the
	// redirect target must NOT have been fetched.
	_ = ctrl.Probe(context.Background(), "demo.sb1.example.com", "abc", "key-1", OpenAck{PublicIP: "127.0.0.1", GrantedPort: port, Nonce: "n1"})
	if targetHit {
		t.Fatalf("probe followed a redirect")
	}
}

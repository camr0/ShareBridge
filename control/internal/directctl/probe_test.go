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

	// One representative IP from each of the 18 CIDRs in ssrfDeny.
	cases := []struct {
		cidr string
		ip   string
	}{
		{"0.0.0.0/8", "0.0.0.1"},
		{"10.0.0.0/8", "10.0.0.1"},
		{"100.64.0.0/10", "100.64.0.1"},
		{"127.0.0.0/8", "127.0.0.1"},
		{"169.254.0.0/16", "169.254.1.1"},
		{"172.16.0.0/12", "172.16.0.1"},
		{"192.0.0.0/24", "192.0.0.1"},
		{"192.0.2.0/24", "192.0.2.1"},
		{"192.88.99.0/24", "192.88.99.1"},
		{"192.168.0.0/16", "192.168.1.1"},
		{"198.18.0.0/15", "198.18.0.1"},
		{"198.51.100.0/24", "198.51.100.1"},
		{"203.0.113.0/24", "203.0.113.1"},
		{"224.0.0.0/4", "224.0.0.1"},
		{"240.0.0.0/4", "240.0.0.1"},
		{"192.31.196.0/24", "192.31.196.1"},
		{"192.52.193.0/24", "192.52.193.1"},
		{"192.175.48.0/24", "192.175.48.1"},
	}
	for _, tc := range cases {
		t.Run(tc.cidr, func(t *testing.T) {
			if err := ctrl.Probe(context.Background(), "o", "abc", "key-1", OpenAck{PublicIP: tc.ip, GrantedPort: 443}); err == nil {
				t.Fatalf("expected rejection for %s (%s)", tc.ip, tc.cidr)
			}
		})
	}
}

func TestProbeRejectsInvalidPort(t *testing.T) {
	_, ctrl := newTestController(t)
	ctrl.allowPrivate = true
	for _, port := range []int{0, 65536} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			if err := ctrl.Probe(context.Background(), "o", "abc", "key-1", OpenAck{PublicIP: "127.0.0.1", GrantedPort: port}); err == nil {
				t.Fatalf("expected rejection for port %d", port)
			}
		})
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

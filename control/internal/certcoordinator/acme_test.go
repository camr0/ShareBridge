package certcoordinator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"net/url"
	"testing"
)

var testWant = []string{
	"*.v7q4km2x9pz6dn3w.sbx.example.com",
	"*.relay.v7q4km2x9pz6dn3w.sbx.example.com",
}

// newCSR builds a self-signed CSR in memory (no network) and returns both the
// parsed request and its PEM encoding.
func newCSR(t *testing.T, cn string, dnsNames []string, ips []net.IP, emails []string, uris []*url.URL) (*x509.CertificateRequest, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.CertificateRequest{
		Subject:        pkix.Name{CommonName: cn},
		DNSNames:       dnsNames,
		IPAddresses:    ips,
		EmailAddresses: emails,
		URIs:           uris,
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatal(err)
	}
	return csr, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestExactDNSNames(t *testing.T) {
	direct := testWant[0]
	relay := testWant[1]
	ip := net.ParseIP("192.0.2.1")
	uri, _ := url.Parse("spiffe://example.com/spike")

	cases := []struct {
		name string
		csr  *x509.CertificateRequest
		want bool
	}{
		{"valid two-SAN", mustCSR(t, "", []string{direct, relay}, nil, nil, nil), true},
		{"duplicate SAN", mustCSR(t, "", []string{direct, relay, direct}, nil, nil, nil), false},
		{"missing SAN", mustCSR(t, "", []string{direct}, nil, nil, nil), false},
		{"extra SAN", mustCSR(t, "", []string{direct, relay, "*.evil.example.com"}, nil, nil, nil), false},
		{"IP SAN", mustCSR(t, "", []string{direct, relay}, []net.IP{ip}, nil, nil), false},
		{"email SAN", mustCSR(t, "", []string{direct, relay}, nil, []string{"a@example.com"}, nil), false},
		{"URI SAN", mustCSR(t, "", []string{direct, relay}, nil, nil, []*url.URL{uri}), false},
	}
	for _, tc := range cases {
		if got := exactDNSNames(tc.csr, testWant); got != tc.want {
			t.Errorf("%s: exactDNSNames = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestValidCommonName(t *testing.T) {
	cases := []struct {
		name string
		cn   string
		want bool
	}{
		{"empty", "", true},
		{"authorized direct", testWant[0], true},
		{"authorized relay", testWant[1], true},
		{"unauthorized", "*.evil.example.com", false},
		{"bare host", "example.com", false},
	}
	for _, tc := range cases {
		if got := validCommonName(tc.cn, testWant); got != tc.want {
			t.Errorf("%s: validCommonName(%q) = %v, want %v", tc.name, tc.cn, got, tc.want)
		}
	}
}

// CompleteCSR must reject a CSR whose SANs are exactly right but whose
// CommonName would smuggle an extra order domain through lego's ObtainForCSR.
// This check runs before any network I/O, so it is safe to exercise directly.
func TestCompleteCSR_RejectsUnauthorizedCommonName(t *testing.T) {
	_, csrPEM := newCSR(t, "*.evil.example.com", testWant, nil, nil, nil)
	_, err := CompleteCSR(context.Background(), csrPEM, ACMEConfig{
		Namespace:  "v7q4km2x9pz6dn3w",
		BaseDomain: "sbx.example.com",
	})
	if err == nil {
		t.Fatalf("CompleteCSR accepted a CSR with an unauthorized CommonName")
	}
}

func mustCSR(t *testing.T, cn string, dnsNames []string, ips []net.IP, emails []string, uris []*url.URL) *x509.CertificateRequest {
	t.Helper()
	csr, _ := newCSR(t, cn, dnsNames, ips, emails, uris)
	return csr
}

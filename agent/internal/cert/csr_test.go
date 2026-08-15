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

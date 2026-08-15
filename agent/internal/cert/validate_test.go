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

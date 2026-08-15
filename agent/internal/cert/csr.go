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
//
//	*.      <namespace>.<baseDomain>   (direct namespace, default mode)
//	*.relay.<namespace>.<baseDomain>   (relay namespace, FRP fallback)
//
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

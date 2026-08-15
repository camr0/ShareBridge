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
//  1. every PEM block parses and the chain is non-empty;
//  2. the chain verifies against the configured trust root with the leaf as
//     the server cert (rejects self-signed, expired, and wrong-issuer chains);
//  3. the leaf's public key equals the locally-held key's public key
//     (rejects a cert minted for a different/attacker-controlled key);
//  4. the leaf's SAN set is EXACTLY the two expected wildcards, no extras;
//  5. the leaf is currently valid (NotBefore <= now <= NotAfter);
//  6. the leaf asserts serverAuth EKU and digital-signature key usage.
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

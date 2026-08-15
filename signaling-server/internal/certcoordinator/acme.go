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

// ACMEConfig carries operator-supplied ACME/DNS credentials and the
// enrollment's authorized identity. All values come from env/config at runtime
// and are NEVER committed to the repo.
type ACMEConfig struct {
	CA              string // lego.LEDirectoryStaging or lego.LEDirectoryProduction
	Email           string // ACME account contact
	CloudflareToken string // Cloudflare API token: single zone, DNS-edit only
	Namespace       string // the enrolling agent's namespace (authorized identity)
	BaseDomain      string // content domain, e.g. sharebridgeusercontent.com
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

// CompleteCSR takes an agent-submitted CSR PEM, verifies it authorizes ONLY the
// enrolling agent's two wildcard SANs, then runs an ACME DNS-01 order via lego's
// Cloudflare provider and returns ONLY the issued leaf-first chain. It never
// sees the agent's private key: lego finalizes the pre-made CSR via ObtainForCSR
// (the external-CSR API) instead of generating a key. lego derives the order's
// domains from the CSR's SANs, so a single call covers BOTH wildcards in one order.
func CompleteCSR(ctx context.Context, csrPEM []byte, cfg ACMEConfig) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("invalid CSR PEM block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}

	// Authorization: the CSR must be self-signed (proves the agent holds the
	// key) and request EXACTLY the enrolling agent's two wildcard SANs. This is
	// what stops an enrolled agent from obtaining a certificate for another
	// namespace or any other name in the zone.
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature check: %w", err)
	}
	want := []string{
		fmt.Sprintf("*.%s.%s", cfg.Namespace, cfg.BaseDomain),
		fmt.Sprintf("*.relay.%s.%s", cfg.Namespace, cfg.BaseDomain),
	}
	if !exactDNSNames(csr, want) {
		return nil, fmt.Errorf("CSR SANs = %v, want exactly %v", csr.DNSNames, want)
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

// exactDNSNames reports whether the CSR requests exactly the wanted DNS SANs
// and nothing else: no IP, email, or URI SANs, and the DNSNames set equals want.
func exactDNSNames(csr *x509.CertificateRequest, want []string) bool {
	if len(csr.IPAddresses) != 0 || len(csr.EmailAddresses) != 0 || len(csr.URIs) != 0 {
		return false
	}
	if len(csr.DNSNames) != len(want) {
		return false
	}
	wantSet := make(map[string]bool, len(want))
	for _, w := range want {
		wantSet[w] = true
	}
	seen := make(map[string]bool, len(csr.DNSNames))
	for _, n := range csr.DNSNames {
		if !wantSet[n] || seen[n] {
			return false // unauthorized, missing, or duplicate SAN
		}
		seen[n] = true
	}
	return true
}

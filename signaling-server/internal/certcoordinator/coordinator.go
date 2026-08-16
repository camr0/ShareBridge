package certcoordinator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const issuanceCooldown = 5 * time.Minute
const maxIssuanceConcurrency = 4

type CoordinatorConfig struct {
	CA, Email, CloudflareToken, BaseDomain string
	AccountKeyPath                         string
}

type cachedChain struct {
	chain    []byte
	notAfter time.Time
}

type Coordinator struct {
	cfg  CoordinatorConfig
	acct *acmeAccount

	mu       sync.Mutex
	sem      chan struct{}
	inflight map[string]bool
	csrIdx   map[string]cachedChain // apiKeyID + ":" + csrFP
	leafIdx  map[string]cachedChain // apiKeyID + ":" + leafFP
	latest   map[string]string      // apiKeyID -> leafFP of the latest issued chain
	last     map[string]time.Time
	issueFn  func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error)
}

func NewCoordinator(cfg CoordinatorConfig) (*Coordinator, error) {
	c := &Coordinator{
		cfg: cfg, sem: make(chan struct{}, maxIssuanceConcurrency),
		inflight: map[string]bool{}, csrIdx: map[string]cachedChain{},
		leafIdx: map[string]cachedChain{}, latest: map[string]string{}, last: map[string]time.Time{},
	}
	c.issueFn = c.completeCSR
	key, err := c.loadOrCreateAccountKey()
	if err != nil {
		return nil, err
	}
	c.acct = &acmeAccount{email: cfg.Email, key: key}
	return c, nil
}

// SetIssueFn replaces the issuance function. It is a test-only stub used by
// downstream packages (e.g. directctl) to inject a fake issuer.
func (c *Coordinator) SetIssueFn(fn func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.issueFn = fn
}

func (c *Coordinator) completeCSR(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) {
	return CompleteCSR(ctx, csrPEM, ACMEConfig{
		CA: c.cfg.CA, Email: c.cfg.Email, CloudflareToken: c.cfg.CloudflareToken,
		Namespace: namespace, BaseDomain: c.cfg.BaseDomain, AccountKey: c.acct.key,
	})
}

func (c *Coordinator) Issue(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) {
	csrFP := fingerprint(csrPEM)
	ck := apiKeyID + ":" + csrFP

	c.mu.Lock()
	if ch, ok := c.csrIdx[ck]; ok {
		c.mu.Unlock()
		return append([]byte(nil), ch.chain...), nil
	}
	if c.inflight[apiKeyID] {
		c.mu.Unlock()
		return nil, errors.New("issuance already in flight for this agent")
	}
	if last, ok := c.last[apiKeyID]; ok && time.Since(last) < issuanceCooldown {
		c.mu.Unlock()
		return nil, errors.New("issuance cooldown active")
	}
	c.inflight[apiKeyID] = true
	c.mu.Unlock()

	release := func() {
		c.mu.Lock()
		delete(c.inflight, apiKeyID)
		c.last[apiKeyID] = time.Now()
		c.mu.Unlock()
	}
	defer release()

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	chain, err := c.issueFn(ctx, csrPEM, namespace, apiKeyID)
	if err != nil {
		return nil, err
	}
	if len(chain) > 1<<20 {
		return nil, errors.New("chain too large")
	}
	na := leafNotAfter(chain)
	leafFP := leafFingerprint(chain)

	c.mu.Lock()
	c.csrIdx[ck] = cachedChain{chain: chain, notAfter: na}
	c.leafIdx[apiKeyID+":"+leafFP] = cachedChain{chain: chain, notAfter: na}
	c.latest[apiKeyID] = leafFP
	c.mu.Unlock()
	return append([]byte(nil), chain...), nil
}

func (c *Coordinator) HasLeafFingerprint(apiKeyID, leafFP string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.leafIdx[apiKeyID+":"+leafFP]
	return ok && ch.notAfter.After(time.Now())
}

func (c *Coordinator) ChainByLeaf(apiKeyID, leafFP string) ([]byte, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.leafIdx[apiKeyID+":"+leafFP]
	if !ok || !ch.notAfter.After(time.Now()) {
		return nil, time.Time{}, false
	}
	return append([]byte(nil), ch.chain...), ch.notAfter, true
}

// SeedLeaf re-seeds the in-memory leaf index from persisted state. After a
// control restart or API-key rotation the in-memory index is empty while the
// agent record still holds the installed cert fingerprint + expiry, so a
// tls_ready carrying that fingerprint must still be accepted without re-issuing.
// chain may be nil when the original chain bytes are not available (restart);
// notAfter is the persisted expiry.
func (c *Coordinator) SeedLeaf(apiKeyID, leafFP string, chain []byte, notAfter time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if chain == nil {
		chain = []byte{}
	}
	c.leafIdx[apiKeyID+":"+leafFP] = cachedChain{chain: chain, notAfter: notAfter}
	if _, ok := c.latest[apiKeyID]; !ok {
		c.latest[apiKeyID] = leafFP
	}
}

// LatestChain returns the most recently issued chain for an agent (for
// re-delivery when the agent reports an older, still-valid fingerprint).
func (c *Coordinator) LatestChain(apiKeyID string) ([]byte, string, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	leafFP, ok := c.latest[apiKeyID]
	if !ok {
		return nil, "", time.Time{}, false
	}
	ch, ok := c.leafIdx[apiKeyID+":"+leafFP]
	if !ok {
		return nil, "", time.Time{}, false
	}
	return append([]byte(nil), ch.chain...), leafFP, ch.notAfter, true
}

func (c *Coordinator) hasCSRFingerprint(apiKeyID, csrFP string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.csrIdx[apiKeyID+":"+csrFP]
	return ok
}

func (c *Coordinator) loadOrCreateAccountKey() (*ecdsa.PrivateKey, error) {
	if c.cfg.AccountKeyPath == "" {
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	b, err := os.ReadFile(c.cfg.AccountKeyPath)
	switch {
	case err == nil:
		block, _ := pem.Decode(b)
		if block == nil || block.Type != "EC PRIVATE KEY" {
			return nil, fmt.Errorf("ACME account key %s is malformed (not an EC PRIVATE KEY PEM block); refusing to silently regenerate — delete the file to recover", c.cfg.AccountKeyPath)
		}
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("ACME account key %s is malformed (%v); refusing to silently regenerate — delete the file to recover", c.cfg.AccountKeyPath, err)
		}
		return k, nil
	case errors.Is(err, os.ErrNotExist):
		// fall through to generate
	default:
		return nil, fmt.Errorf("read ACME account key %s: %w", c.cfg.AccountKeyPath, err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(c.cfg.AccountKeyPath), 0700); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(c.cfg.AccountKeyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0600); err != nil {
		return nil, err
	}
	return key, nil
}

// writeFileAtomic writes data to a temp file in the same directory, fsyncs it,
// and renames it over target so a crash cannot leave a truncated/partial key.
func writeFileAtomic(target string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".acct-key-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

func fingerprint(csrPEM []byte) string {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		sum := sha256.Sum256(csrPEM)
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

func leafFingerprint(chainPEM []byte) string {
	block, _ := pem.Decode(chainPEM)
	if block == nil {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

func leafNotAfter(chainPEM []byte) time.Time {
	block, _ := pem.Decode(chainPEM)
	if block == nil {
		return time.Time{}
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}
	}
	return cert.NotAfter
}

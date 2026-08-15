package certcoordinator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"sync/atomic"
	"testing"
	"time"
)

// newTestCoordinator builds a Coordinator with a fake issueFn and no account
// key, so tests never touch the network or the filesystem.
func newTestCoordinator() *Coordinator {
	c := &Coordinator{
		sem:      make(chan struct{}, maxIssuanceConcurrency),
		inflight: map[string]bool{},
		csrIdx:   map[string]cachedChain{},
		leafIdx:  map[string]cachedChain{},
		latest:   map[string]string{},
		last:     map[string]time.Time{},
	}
	c.issueFn = func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) {
		return nil, errors.New("unexpected issueFn call")
	}
	return c
}

// makeChain mints a single self-signed leaf certificate with the given label
// (embedded in the CommonName so distinct labels yield distinct fingerprints)
// and NotAfter. It returns a PEM chain that leafNotAfter/leafFingerprint can
// parse. It is not a t.Helper because the brief's tests call it without *T.
func makeChain(label string, notAfter time.Time) []byte {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: label},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// fingerprintBytes returns the raw SHA-256 digest of a CSR PEM (the DER body if
// PEM-decodable, else the raw input). Mirrors Coordinator.fingerprint.
func fingerprintBytes(csrPEM []byte) []byte {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		sum := sha256.Sum256(csrPEM)
		return sum[:]
	}
	sum := sha256.Sum256(block.Bytes)
	return sum[:]
}

// leafFingerprintBytes returns the raw SHA-256 digest of the leaf certificate's
// DER bytes (the first PEM block of the chain). Mirrors Coordinator.leafFingerprint.
func leafFingerprintBytes(chainPEM []byte) []byte {
	block, _ := pem.Decode(chainPEM)
	if block == nil {
		return nil
	}
	sum := sha256.Sum256(block.Bytes)
	return sum[:]
}

func TestCoordinatorIdempotencyAndLeafIndex(t *testing.T) {
	ctx := context.Background()
	c := newTestCoordinator()
	csr := []byte("csr-1")
	chain := makeChain("leaf-1", time.Now().Add(90*24*time.Hour))
	c.issueFn = func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) { return chain, nil }

	got, err := c.Issue(ctx, csr, "sbdeadbeef", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(chain) {
		t.Fatalf("chain mismatch")
	}

	csrFP := hex.EncodeToString(fingerprintBytes(csr))
	if !c.hasCSRFingerprint("key-1", csrFP) {
		t.Fatalf("csr index missing")
	}

	leafFP := hex.EncodeToString(leafFingerprintBytes(chain))
	if !c.HasLeafFingerprint("key-1", leafFP) {
		t.Fatalf("leaf index missing")
	}

	// Idempotent retry returns cached chain without re-issuing.
	c.issueFn = func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) {
		return nil, errors.New("should not re-issue")
	}
	got2, err := c.Issue(ctx, csr, "sbdeadbeef", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != string(chain) {
		t.Fatalf("idempotent retry changed chain")
	}

	// Cross-agent isolation: another agent's leaf fingerprint must not verify.
	if c.HasLeafFingerprint("key-2", leafFP) {
		t.Fatalf("cross-agent leak")
	}

	// A different CSR within cooldown is rejected.
	c.issueFn = func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) {
		return []byte("chain-2"), nil
	}
	if _, err := c.Issue(ctx, []byte("csr-2"), "sbdeadbeef", "key-1"); err == nil {
		t.Fatalf("expected cooldown rejection")
	}
}

func TestCoordinatorRejectsExpiredLeaf(t *testing.T) {
	ctx := context.Background()
	c := newTestCoordinator()
	csr := []byte("csr-expired")
	chain := makeChain("leaf-expired", time.Now().Add(-time.Hour))
	c.issueFn = func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) { return chain, nil }

	if _, err := c.Issue(ctx, csr, "sbdeadbeef", "key-1"); err != nil {
		t.Fatal(err)
	}

	leafFP := hex.EncodeToString(leafFingerprintBytes(chain))
	if c.HasLeafFingerprint("key-1", leafFP) {
		t.Fatalf("expired leaf must not verify")
	}
	if _, _, ok := c.ChainByLeaf("key-1", leafFP); ok {
		t.Fatalf("expired leaf must not be returned by ChainByLeaf")
	}
}

func TestCoordinatorLatestChain(t *testing.T) {
	ctx := context.Background()
	c := newTestCoordinator()
	csr := []byte("csr-latest")
	chain := makeChain("leaf-latest", time.Now().Add(90*24*time.Hour))
	c.issueFn = func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) { return chain, nil }

	if _, err := c.Issue(ctx, csr, "sbdeadbeef", "key-1"); err != nil {
		t.Fatal(err)
	}

	leafFP := hex.EncodeToString(leafFingerprintBytes(chain))
	gotChain, gotLeafFP, notAfter, ok := c.LatestChain("key-1")
	if !ok {
		t.Fatalf("LatestChain ok = false for known apiKeyID")
	}
	if string(gotChain) != string(chain) {
		t.Fatalf("LatestChain returned wrong chain")
	}
	if gotLeafFP != leafFP {
		t.Fatalf("LatestChain leaf fingerprint mismatch: got %s want %s", gotLeafFP, leafFP)
	}
	if notAfter.IsZero() {
		t.Fatalf("LatestChain notAfter is zero")
	}

	if _, _, _, ok := c.LatestChain("key-unknown"); ok {
		t.Fatalf("LatestChain ok = true for unknown apiKeyID")
	}
}

func TestCoordinatorGlobalSemaphore(t *testing.T) {
	c := newTestCoordinator()
	c.sem = make(chan struct{}, 1) // bound 1: issues must serialize

	var active, maxActive int32
	release := make(chan struct{})
	done := make(chan struct{}, 2)

	c.issueFn = func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) {
		cur := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxActive)
			if cur <= old || atomic.CompareAndSwapInt32(&maxActive, old, cur) {
				break
			}
		}
		<-release
		atomic.AddInt32(&active, -1)
		return makeChain("leaf-"+apiKeyID, time.Now().Add(90*24*time.Hour)), nil
	}

	go func() {
		_, _ = c.Issue(context.Background(), []byte("csr-a"), "sbdeadbeef", "key-1")
		done <- struct{}{}
	}()
	go func() {
		_, _ = c.Issue(context.Background(), []byte("csr-b"), "sbdeadbeef", "key-2")
		done <- struct{}{}
	}()

	// Wait until one issue is in flight, then give the second goroutine time to
	// reach the semaphore and confirm it is still blocked (active == 1).
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&active) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for first issue to start")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&active); got != 1 {
		t.Fatalf("expected 1 active issue with sem bound 1, got %d", got)
	}

	close(release)
	<-done
	<-done

	if got := atomic.LoadInt32(&maxActive); got != 1 {
		t.Fatalf("max concurrent issues = %d, want 1", got)
	}
}

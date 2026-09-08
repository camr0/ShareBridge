package directctl

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/control/internal/certcoordinator"
	"sharebridge/control/internal/hub"
	mig "sharebridge/control/migrations"
)

// newTestController boots a fresh in-memory PocketBase app with the base +
// agents schema, a stub coordinator (no network), and returns (app, ctrl).
func newTestController(t *testing.T) (core.App, *Controller) {
	t.Helper()
	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "pb_test_env",
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := mig.CreateCollections(app); err != nil {
		t.Fatalf("base schema: %v", err)
	}
	// Apply the full session-field migration chain so the sessions collection
	// carries relay_only/share_type/is_password_protected/inactive_reason for
	// the tombstone-aware resolver tests, plus migration 9 so relay
	// assignment/persistence (EnableRelay → EnsureAssignment) works.
	for _, fn := range []func(core.App) error{
		mig.AddRelayOnly,
		mig.AddSessionRelayStaticPub,
		mig.AddImmichSessionFields,
		mig.CreateAgents,
		mig.AddSessionsInactiveReason,
		mig.AddAgentsRelaySTUN,
	} {
		if err := fn(app); err != nil {
			t.Fatalf("session schema: %v", err)
		}
	}

	// A coordinator with a stub issuer returning a fixed chain, so tests can
	// Issue and then report a real leaf fingerprint.
	coord, err := certcoordinator.NewCoordinator(certcoordinator.CoordinatorConfig{AccountKeyPath: filepath.Join(t.TempDir(), "acct.pem")})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	coord.SetIssueFn(func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) {
		return mustTestChain(t), nil
	})
	ctrl := NewController(app, hub.New(), coord, nil, Config{
		BaseDomain: "example.com",
		// Task 22: the real shipped route-interstitial assets, so every
		// selection test exercises the production renderer (not a stub).
		InterstitialAssets: mustInterstitialAssets(t),
	})
	ctrl.allowPrivate = true // loopback probe allowed in tests
	ctrl.sendFn = func(ctx context.Context, conn *websocket.Conn, msg any) error { return nil }
	return app, ctrl
}

// captureSend swaps sendFn to record the next message map and returns it.
func (c *Controller) captureSend(fn func()) map[string]any {
	var got map[string]any
	old := c.sendFn
	c.sendFn = func(ctx context.Context, conn *websocket.Conn, msg any) error {
		got, _ = msg.(map[string]any)
		if got == nil {
			if b, err := json.Marshal(msg); err == nil {
				_ = json.Unmarshal(b, &got)
			}
		}
		return nil
	}
	fn()
	c.sendFn = old
	return got
}

// mustTestChain mints a valid, future-dated self-signed leaf so the
// coordinator's leafNotAfter parses to a future time (a hardcoded/garbage PEM
// would yield a zero NotAfter and ChainByLeaf would reject it).
func mustTestChain(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func leafFPOf(chainPEM []byte) string {
	block, _ := pem.Decode(chainPEM)
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

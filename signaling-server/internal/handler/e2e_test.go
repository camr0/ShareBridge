package handler

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"sharebridge/server/internal/certcoordinator"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/directctl"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/middleware"
	"sharebridge/server/internal/relay"
)

// TestEndToEndDirectFlow drives the full direct-mode control flow through the
// real HTTP + WebSocket stack: agent hello → enrolled → CSR → cert_issue →
// tls_ready → report_endpoint (DDNS) → enrollment_ready → register_share →
// share_registered{origin} → GET /s/<code> → open_signal → open_ack → loopback
// nonce probe → 302. The coordinator issues a fixed stub chain and the probe
// runs against a loopback TLS server that echoes the nonce, so no production
// certs, Cloudflare, or UPnP are touched.
func TestEndToEndDirectFlow(t *testing.T) {
	app, appCleanup := setupAgentTestApp(t)
	defer appCleanup()

	// Boot the control plane with a stub coordinator + stub issuer. A fixed
	// chain keeps the leaf fingerprint deterministic and lets the coordinator
	// cache it by leaf so tls_ready can look it up by reported fingerprint.
	coord, err := certcoordinator.NewCoordinator(certcoordinator.CoordinatorConfig{})
	require.NoError(t, err)
	chain := mustE2EChain(t)
	coord.SetIssueFn(func(ctx context.Context, csrPEM []byte, namespace, apiKeyID string) ([]byte, error) {
		return chain, nil
	})

	h := hub.New()
	cfg := config.Load()
	reg := relay.NewRegistry(2 * time.Second)
	ctrl := directctl.NewController(app, h, coord, nil, directctl.Config{BaseDomain: "example.com"})
	ctrl.AllowPrivateProbes() // loopback probe target in this test
	ctrl.SetDDNSFunc(func(ctx context.Context, name, ip string, ttl int) (string, error) {
		return "", nil // succeed DDNS without a live Cloudflare zone
	})

	// Wire the real HTTP + WS stack: /ws/agent (auth + AgentWS) and /s/{code}
	// (ctrl.Redirect), matching main.go's route shape.
	authMiddleware := middleware.APIKeyAuth(app)
	agentHandler := AgentWS(app, h, reg, cfg, ctrl)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent", authMiddleware(http.HandlerFunc(agentHandler)))
	mux.HandleFunc("/s/", func(w http.ResponseWriter, r *http.Request) {
		code := strings.TrimPrefix(r.URL.Path, "/s/")
		_ = ctrl.Redirect(w, r, code)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	// Loopback TLS probe server: echoes the nonce back verbatim, exactly what
	// directctl.Probe asserts (status 200 + body == nonce).
	probe := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, r.URL.Query().Get("nonce"))
	}))
	probe.StartTLS()
	defer probe.Close()
	probeURL, err := url.Parse(probe.URL)
	require.NoError(t, err)
	probePort := mustE2EAtoi(t, probeURL.Port())

	// Agent identity + API key, then dial the real agent WS.
	user, err := createTestUser(app, "e2e-agent@example.com")
	require.NoError(t, err)
	apiKey, err := createTestAPIKey(app, user.Id, "e2esecret")
	require.NoError(t, err)
	fullKey := apiKey.Id + ".e2esecret"
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/agent?api_key=" + fullKey

	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// 1. hello → welcome then enrolled (with namespace).
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"hello","version":"1.0","agent_id":"agent-e2e"}`)))
	require.Contains(t, string(readE2E(t, conn)), `"type":"welcome"`)
	var enrolled struct {
		Type      string `json:"type"`
		Namespace string `json:"namespace"`
	}
	require.NoError(t, json.Unmarshal(readE2E(t, conn), &enrolled))
	require.Equal(t, "enrolled", enrolled.Type)
	require.NotEmpty(t, enrolled.Namespace)

	// 2. csr_submit → cert_issue (stub chain); derive its leaf fingerprint.
	csrPEM := mustE2ECSR(t)
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText,
		[]byte(fmt.Sprintf(`{"type":"csr_submit","csr_pem":%q}`, csrPEM))))
	var certIssue struct {
		Type     string `json:"type"`
		ChainPEM string `json:"chain_pem"`
	}
	require.NoError(t, json.Unmarshal(readE2E(t, conn), &certIssue))
	require.Equal(t, "cert_issue", certIssue.Type)
	leafFP := e2eLeafFP(certIssue.ChainPEM)
	require.NotEmpty(t, leafFP)

	// 3. tls_ready (no immediate reply), then report_endpoint triggers DDNS and
	//    completes readiness → enrollment_ready.
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText,
		[]byte(fmt.Sprintf(`{"type":"tls_ready","fingerprint":%q}`, leafFP))))
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText,
		[]byte(fmt.Sprintf(`{"type":"report_endpoint","ip":"127.0.0.1","port":%d,"status":""}`, probePort))))
	require.Contains(t, string(readE2E(t, conn)), `"type":"enrollment_ready"`)

	// 4. register_share → share_registered {origin}.
	const code = "e2e-code-1234"
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText,
		[]byte(fmt.Sprintf(`{"type":"register_share","code":%q}`, code))))
	var shareReg struct {
		Type   string `json:"type"`
		Code   string `json:"code"`
		Origin string `json:"origin"`
	}
	require.NoError(t, json.Unmarshal(readE2E(t, conn), &shareReg))
	require.Equal(t, "share_registered", shareReg.Type)
	require.Equal(t, code, shareReg.Code)
	require.NotEmpty(t, shareReg.Origin)

	// 5. GET /s/<code> blocks on open_ack; drive it from a goroutine. The
	//    default http.Get would follow the 302 to the loopback server, so use a
	//    client that stops at the redirect.
	redirectResult := make(chan *http.Response, 1)
	redirectErr := make(chan error, 1)
	go func() {
		client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := client.Get(server.URL + "/s/" + code)
		if err != nil {
			redirectErr <- err
			return
		}
		redirectResult <- resp
	}()

	// Agent receives open_signal and acks with the loopback endpoint.
	var openSignal struct {
		Type    string `json:"type"`
		ShareID string `json:"share_id"`
		Nonce   string `json:"nonce"`
		Seq     uint64 `json:"seq"`
	}
	require.NoError(t, json.Unmarshal(readE2E(t, conn), &openSignal))
	require.Equal(t, "open_signal", openSignal.Type)
	require.Equal(t, code, openSignal.ShareID)
	require.NotEmpty(t, openSignal.Nonce)
	require.NotZero(t, openSignal.Seq)

	ack := fmt.Sprintf(`{"type":"open_ack","share_id":%q,"nonce":%q,"seq":%d,"granted_port":%d,"public_ip":"127.0.0.1","status":"ok"}`,
		openSignal.ShareID, openSignal.Nonce, openSignal.Seq, probePort)
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte(ack)))

	// Control probes the loopback nonce echo and 302s to the direct origin.
	select {
	case err := <-redirectErr:
		t.Fatalf("redirect request failed: %v", err)
	case resp := <-redirectResult:
		defer resp.Body.Close()
		require.Equal(t, http.StatusFound, resp.StatusCode)
		loc := resp.Header.Get("Location")
		require.Contains(t, loc, "https://"+shareReg.Origin)
		require.Contains(t, loc, "/s/"+code)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for redirect response")
	}
}

// readE2E reads one text frame with a bounded deadline so a missing message
// fails the test instead of hanging it.
func readE2E(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	require.NoError(t, err)
	return data
}

// mustE2EChain mints a valid, future-dated self-signed leaf (mirrors
// directctl.mustTestChain) so the coordinator's leafNotAfter parses to a future
// time and ChainByLeaf accepts it.
func mustE2EChain(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// mustE2ECSR generates a real, self-signed CSR PEM. The stub issuer ignores its
// contents, but a realistic CSR keeps the flow faithful.
func mustE2ECSR(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "agent-e2e"}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// e2eLeafFP computes the leaf fingerprint exactly as certcoordinator does:
// SHA-256 of the first PEM block in the chain.
func e2eLeafFP(chainPEM string) string {
	block, _ := pem.Decode([]byte(chainPEM))
	if block == nil {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

func mustE2EAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	require.NoError(t, err)
	return n
}

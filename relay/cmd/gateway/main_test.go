package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sharebridge/relay/internal/frpplugin"
	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/limits"
	"sharebridge/relay/internal/metrics"
	"sharebridge/relay/internal/presence"
	"sharebridge/relay/internal/routes"
)

// TestConfiguredLimitsReadsProductionEnvironment proves the gateway binary's
// actual configuration entry point reads the §14 operator environment,
// including a lowered global file-descriptor ceiling and the hello
// byte/timeout bounds the parser must honour.
func TestConfiguredLimitsReadsProductionEnvironment(t *testing.T) {
	t.Setenv(limits.EnvMaxStreamsGlobal, "123")
	t.Setenv(limits.EnvMaxStreamsPerSourceIP, "7")
	t.Setenv(limits.EnvMaxHelloBytes, "4096")
	t.Setenv(limits.EnvHelloTimeout, "750ms")

	config, err := configuredLimits()
	if err != nil {
		t.Fatalf("configuredLimits() = %v, want nil", err)
	}
	if config.MaxStreamsGlobal != 123 {
		t.Errorf("MaxStreamsGlobal = %d, want the environment value 123", config.MaxStreamsGlobal)
	}
	if config.MaxStreamsPerSourceIP != 7 {
		t.Errorf("MaxStreamsPerSourceIP = %d, want the environment value 7", config.MaxStreamsPerSourceIP)
	}
	if config.MaxHelloBytes != 4096 {
		t.Errorf("MaxHelloBytes = %d, want the environment value 4096", config.MaxHelloBytes)
	}
	if config.HelloTimeout.String() != "750ms" {
		t.Errorf("HelloTimeout = %v, want the environment value 750ms", config.HelloTimeout)
	}
}

// TestConfiguredLimitsFailsClosedOnInvalidEnvironment proves the binary
// refuses to start on a nonsensical operator value instead of running with a
// silently wrong bound.
func TestConfiguredLimitsFailsClosedOnInvalidEnvironment(t *testing.T) {
	t.Setenv(limits.EnvMaxStreamsGlobal, "0")
	if _, err := configuredLimits(); err == nil {
		t.Fatal("configuredLimits() with a zero global ceiling error = nil, want a fail-closed rejection")
	}
}

// TestNewPresenceRegistryBuildsWithoutError proves the gateway binary can
// construct its production presence registry (fresh boot identity plus the
// streams drain seam) without operator configuration.
func TestNewPresenceRegistryBuildsWithoutError(t *testing.T) {
	registry, err := newPresenceRegistry(gateway.NewStreams(), metrics.NewRegistry(metrics.Relay), newTunnelRestorationTracker(time.Now))
	if err != nil {
		t.Fatalf("newPresenceRegistry() error = %v, want nil", err)
	}
	if registry == nil {
		t.Fatal("newPresenceRegistry() returned a nil registry")
	}
	if registry.BootID() == "" {
		t.Fatal("newPresenceRegistry() produced an empty boot identity")
	}
}

// TestConfiguredSyncLoopFailsClosed proves the production control-sync build is
// fail-closed: no sync variables means sync is disabled (route readiness stays
// false), and a partial configuration refuses startup instead of running a
// half-configured control channel.
func TestConfiguredSyncLoopFailsClosed(t *testing.T) {
	for _, name := range []string{envControlSyncURL, envControlSyncSAN, envControlSyncCAFile, envGatewaySyncCertFile, envGatewaySyncKeyFile, envGatewayNamespace} {
		t.Setenv(name, "")
	}
	health := gateway.NewHealth()
	registry := metrics.NewRegistry(metrics.Relay)
	table := routes.NewTable(nil)
	streams := gateway.NewStreams()
	logger := slog.New(slog.DiscardHandler)

	loop, enabled, err := configuredSyncLoop(health, registry, table, streams, "gw-boot", logger)
	if err != nil || enabled || loop != nil {
		t.Fatalf("unconfigured sync = (%v, enabled=%v, %v), want (nil, false, nil)", loop, enabled, err)
	}

	t.Setenv(envControlSyncURL, "https://control.sync.internal:8443")
	if _, _, err := configuredSyncLoop(health, registry, table, streams, "gw-boot", logger); err == nil {
		t.Fatal("a partial control-sync configuration was accepted; it must refuse startup")
	}
}

// TestConfiguredNICSourceReportsUnavailableWithoutInterface proves the
// production NIC source never fabricates a ratio: without a configured
// interface it reports unavailable rather than a plausible 0.
func TestConfiguredNICSourceReportsUnavailableWithoutInterface(t *testing.T) {
	t.Setenv(envNICInterface, "")
	t.Setenv(envNICCapacityBytes, "1000")
	if _, ok := configuredNICSource()(); ok {
		t.Fatal("NIC source reported a ratio without a configured interface")
	}
}

// TestGatewayHealthReflectsRealFRPSPluginLifecycle proves the §17.1 frps
// process truth is driven by the real frps/plugin boundary: an authenticated
// Login from frps marks frps healthy, while route readiness stays false (the
// independent sync truth).
func TestGatewayHealthReflectsRealFRPSPluginLifecycle(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate control key: %v", err)
	}
	const sharedSecret = "gateway-health-test-secret"
	t.Setenv(envPluginSharedSecret, sharedSecret)
	t.Setenv(envControlPublicKey, hex.EncodeToString(publicKey))
	t.Setenv(envRelayPortMin, "21000")
	t.Setenv(envRelayPortMax, "21100")
	t.Setenv(envRelayDataDir, t.TempDir())

	health := gateway.NewHealth()
	recorder := &wiringPresenceRecorder{}
	events := frpsLifecycleEvents{next: recorder, health: health, restoration: newTunnelRestorationTracker(time.Now)}
	plugin, _, err := configuredPluginServer(events, metrics.NewRegistry(metrics.Relay))
	if err != nil {
		t.Fatalf("configuredPluginServer() error = %v, want nil", err)
	}

	if health.FRPSProcessHealthy() {
		t.Fatal("frps health defaults to false before any frps contact")
	}
	now := time.Now().UTC()
	claims := wiringClaims{
		Issuer: "sharebridge-control", Audience: "sharebridge-relay", APIKeyID: "api-health",
		AgentRecordID: "agent-health", Namespace: "sbdeadbeef", ProxyName: "sb-sbdeadbeef",
		RelayPort: 21042, Generation: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(9 * time.Minute), JTI: "jti-health-one",
	}
	token := signWiringCredential(t, privateKey, claims)
	if wiringRequest(t, plugin, sharedSecret, frpplugin.OperationLogin, wiringLoginContent(token, claims)).Reject {
		t.Fatal("the Login was rejected; the plugin was not wired")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !health.FRPSProcessHealthy() {
		time.Sleep(time.Millisecond)
	}
	if !health.FRPSProcessHealthy() {
		t.Fatal("a real frps Login did not mark frps process health true")
	}
	if health.RouteReady() {
		t.Fatal("frps health must never imply route readiness (snapshot/control sync are separate truths)")
	}
}

// TestTunnelRestorationMetricFromRealPresenceRestart drives the real presence
// registry through an frps SessionReset and a subsequent confirmed re-login,
// and observes the §17.3 restart→restored histogram move. No metric method is
// called directly.
func TestTunnelRestorationMetricFromRealPresenceRestart(t *testing.T) {
	registry := metrics.NewRegistry(metrics.Relay)
	current := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	clock := func() time.Time { return current }
	restoration := newTunnelRestorationTracker(clock)
	sink := &presenceMetricsSink{registry: registry, restoration: restoration, now: clock}
	presenceRegistry, err := presence.NewRegistry(presence.Config{
		BootID: "gw-restoration-boot",
		Now:    clock,
		Probe:  func(context.Context, int) (string, error) { return "127.0.0.1:4444", nil },
		Sink:   sink,
	})
	if err != nil {
		t.Fatalf("presence.NewRegistry: %v", err)
	}
	events := frpsLifecycleEvents{next: presenceRegistry, health: gateway.NewHealth(), restoration: restoration}

	const agent = "agent-restoration"
	const port = 10001
	login := func(jti string, issuedAt time.Time) {
		fact := frpplugin.PresenceFact{Operation: frpplugin.OperationLogin, AgentRecordID: agent, Namespace: "sb0123abcd",
			ProxyName: "sb-sb0123abcd", RelayPort: port, Generation: 1, RunID: "run-1", CredentialJTI: jti, CredentialIssuedAt: issuedAt}
		events.ObserveFRPEvent(fact)
		events.ObserveFRPEvent(frpplugin.PresenceFact{Operation: frpplugin.OperationNewProxy, AgentRecordID: agent, Namespace: "sb0123abcd",
			ProxyName: "sb-sb0123abcd", RelayPort: port, Generation: 1, RunID: "run-1", CredentialJTI: jti, CredentialIssuedAt: issuedAt})
		events.ObserveFRPEvent(frpplugin.PresenceFact{Operation: frpplugin.OperationNewUserConn, AgentRecordID: agent, Namespace: "sb0123abcd",
			ProxyName: "sb-sb0123abcd", RelayPort: port, Generation: 1, RunID: "run-1", RemoteAddr: "127.0.0.1:4444", CredentialJTI: jti, CredentialIssuedAt: issuedAt})
	}
	waitOnline := func() bool {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if presenceRegistry.Online(agent, port, 1) {
				return true
			}
			time.Sleep(time.Millisecond)
		}
		return false
	}

	firstLogin := current.Add(-time.Minute)
	login("jti-one", firstLogin)
	if !waitOnline() {
		t.Fatal("the initial tunnel never confirmed online")
	}

	// frps restarts: the plugin re-presents the burned credential (SessionReset).
	events.ObserveFRPEvent(frpplugin.PresenceFact{Operation: frpplugin.OperationSessionReset, AgentRecordID: agent, Namespace: "sb0123abcd",
		ProxyName: "sb-sb0123abcd", RelayPort: port, Generation: 1, RunID: "run-1", CredentialJTI: "jti-one", CredentialIssuedAt: firstLogin})
	if presenceRegistry.Online(agent, port, 1) {
		t.Fatal("the SessionReset did not clear the dead session's presence")
	}

	current = current.Add(3 * time.Second)
	login("jti-two", firstLogin.Add(time.Second))
	if !waitOnline() {
		t.Fatal("the tunnel never restored online after the frps restart")
	}

	if rendered := registry.Render(); !strings.Contains(rendered, "sharebridge_relay_tunnel_restoration_seconds_count{} 1") {
		t.Fatalf("the real restart→restored path recorded no restoration observation:\n%s", rendered)
	}
}

// TestConfiguredPluginServerWiresPresenceEvents proves the gateway binary's
// production plugin construction carries the presence-event seam end to end:
// an admitted Login reaches the injected presence sink, and a replayed
// (burned) Login drives the §15.2 SessionReset lifecycle fact into it. Without
// this wiring the plugin's frps-reset trigger would be discarded in production.
func TestConfiguredPluginServerWiresPresenceEvents(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate control key: %v", err)
	}
	const sharedSecret = "gateway-wiring-test-secret"
	t.Setenv(envPluginSharedSecret, sharedSecret)
	t.Setenv(envControlPublicKey, hex.EncodeToString(publicKey))
	t.Setenv(envRelayPortMin, "20000")
	t.Setenv(envRelayPortMax, "20100")
	t.Setenv(envRelayDataDir, t.TempDir())

	recorder := &wiringPresenceRecorder{}
	plugin, _, err := configuredPluginServer(recorder, metrics.NewRegistry(metrics.Relay))
	if err != nil {
		t.Fatalf("configuredPluginServer() error = %v, want nil", err)
	}

	now := time.Now().UTC()
	claims := wiringClaims{
		Issuer:        "sharebridge-control",
		Audience:      "sharebridge-relay",
		APIKeyID:      "api-wiring",
		AgentRecordID: "agent-wiring",
		Namespace:     "sbdeadbeef",
		ProxyName:     "sb-sbdeadbeef",
		RelayPort:     20042,
		Generation:    1,
		IssuedAt:      now.Add(-time.Minute),
		ExpiresAt:     now.Add(9 * time.Minute),
		JTI:           "jti-wiring-one",
	}
	token := signWiringCredential(t, privateKey, claims)
	login := wiringLoginContent(token, claims)

	if wiringRequest(t, plugin, sharedSecret, frpplugin.OperationLogin, login).Reject {
		t.Fatal("the first Login was rejected; the plugin was not wired for the credential")
	}
	// Facts are dispatched on the plugin's asynchronous bounded dispatcher.
	waitForWiringFacts(t, recorder, 1)
	if got := len(recorder.snapshot()); got != 1 {
		t.Fatalf("presence facts after Login = %d, want 1", got)
	}

	// A replayed (already-burned) one-use credential is the frps session-reset
	// signal and must be forwarded to the presence sink.
	if !wiringRequest(t, plugin, sharedSecret, frpplugin.OperationLogin, login).Reject {
		t.Fatal("the replayed Login was accepted; the replay trigger is broken")
	}
	waitForWiringFacts(t, recorder, 2)
	events := recorder.snapshot()
	if len(events) != 2 || events[1].Operation != frpplugin.OperationSessionReset {
		t.Fatalf("presence facts = %+v, want Login then SessionReset", events)
	}
	if events[1].AgentRecordID != claims.AgentRecordID {
		t.Fatalf("SessionReset fact agent = %q, want %q", events[1].AgentRecordID, claims.AgentRecordID)
	}
}

// wiringClaims mirrors the control-issued §7.2 credential claim set.
type wiringClaims struct {
	Issuer        string    `json:"iss"`
	Audience      string    `json:"aud"`
	APIKeyID      string    `json:"api_key_id"`
	AgentRecordID string    `json:"agent_record_id"`
	Namespace     string    `json:"namespace"`
	ProxyName     string    `json:"proxy_name"`
	RelayPort     int       `json:"relay_port"`
	Generation    int       `json:"generation"`
	IssuedAt      time.Time `json:"issued_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	JTI           string    `json:"jti"`
}

type wiringPresenceRecorder struct {
	mu     sync.Mutex
	events []frpplugin.PresenceFact
}

func (r *wiringPresenceRecorder) ObserveFRPEvent(fact frpplugin.PresenceFact) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fact)
}

func (r *wiringPresenceRecorder) snapshot() []frpplugin.PresenceFact {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]frpplugin.PresenceFact(nil), r.events...)
}

func waitForWiringFacts(t *testing.T, recorder *wiringPresenceRecorder, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(recorder.snapshot()) < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

func signWiringCredential(t *testing.T, privateKey ed25519.PrivateKey, claims wiringClaims) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal credential: %v", err)
	}
	signature := ed25519.Sign(privateKey, payload)
	return "sbrelay1." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func wiringLoginContent(token string, claims wiringClaims) map[string]any {
	return map[string]any{
		"version":    "0.71.0",
		"hostname":   "sharebridge-agent",
		"os":         "linux",
		"arch":       "amd64",
		"user":       "",
		"timestamp":  time.Now().Unix(),
		"run_id":     "run-wiring",
		"pool_count": 0,
		"metas": map[string]string{
			frpplugin.CredentialMetadataKey: token,
			frpplugin.GenerationMetadataKey: strconv.Itoa(claims.Generation),
		},
	}
}

type wiringPluginResponse struct {
	Reject bool `json:"reject"`
}

func wiringRequest(t *testing.T, plugin http.Handler, sharedSecret, operation string, content any) wiringPluginResponse {
	t.Helper()
	envelope := map[string]any{"version": frpplugin.FRPPluginAPIVersion, "op": operation, "content": content}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal plugin request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost,
		frpplugin.APIPath+"?version="+frpplugin.FRPPluginAPIVersion+"&op="+operation, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "127.0.0.1:43210"
	request.SetBasicAuth(frpplugin.PluginAuthUsername, sharedSecret)
	recorder := httptest.NewRecorder()
	plugin.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("plugin status = %d, want 200", recorder.Code)
	}
	var response wiringPluginResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode plugin response: %v", err)
	}
	return response
}

package frpplugin

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testPluginSecret = "local-frps-plugin-secret"

var testNow = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

type testCredentialClaims struct {
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

type eventRecorder struct {
	mu     sync.Mutex
	events []PresenceFact
}

type blockingEventRecorder struct {
	mu          sync.Mutex
	events      []PresenceFact
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func (recorder *blockingEventRecorder) ObserveFRPEvent(fact PresenceFact) {
	recorder.mu.Lock()
	recorder.events = append(recorder.events, fact)
	recorder.mu.Unlock()
	recorder.enteredOnce.Do(func() {
		close(recorder.entered)
		<-recorder.release
	})
}

func (recorder *blockingEventRecorder) unblock() {
	recorder.releaseOnce.Do(func() { close(recorder.release) })
}

func (recorder *blockingEventRecorder) waitForCount(t *testing.T, count int) []PresenceFact {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		recorder.mu.Lock()
		events := append([]PresenceFact(nil), recorder.events...)
		recorder.mu.Unlock()
		if len(events) >= count {
			return events
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d events", count)
	return nil
}

func (recorder *eventRecorder) ObserveFRPEvent(fact PresenceFact) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.events = append(recorder.events, fact)
}

func (recorder *eventRecorder) snapshot() []PresenceFact {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]PresenceFact(nil), recorder.events...)
}

func (recorder *eventRecorder) waitForCount(t *testing.T, count int) []PresenceFact {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		events := recorder.snapshot()
		if len(events) >= count {
			return events
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d events", count)
	return nil
}

type pluginFixture struct {
	t          *testing.T
	handler    http.Handler
	privateKey ed25519.PrivateKey
	recorder   *eventRecorder
	claims     testCredentialClaims
	token      string
	runID      string
}

func newPluginFixture(t *testing.T, mutateConfig ...func(*Config)) *pluginFixture {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	recorder := &eventRecorder{}
	config := Config{
		ControlPublicKey:   publicKey,
		PluginSharedSecret: testPluginSecret,
		RelayPortMin:       11000,
		RelayPortMax:       11099,
		Now:                func() time.Time { return testNow },
		PresenceEvents:     recorder,
	}
	for _, mutate := range mutateConfig {
		mutate(&config)
	}
	handler, err := NewServer(config)
	if err != nil {
		t.Fatalf("new plugin server: %v", err)
	}
	claims := validTestClaims(1, "jti-one")
	token := signTestCredential(t, privateKey, claims)
	return &pluginFixture{
		t:          t,
		handler:    handler,
		privateKey: privateKey,
		recorder:   recorder,
		claims:     claims,
		token:      token,
		runID:      "run-one",
	}
}

func validTestClaims(generation int, jti string) testCredentialClaims {
	return testCredentialClaims{
		Issuer:        "sharebridge-control",
		Audience:      "sharebridge-relay",
		APIKeyID:      "api-key-record-one",
		AgentRecordID: "agent-record-one",
		Namespace:     "sbdeadbeef",
		ProxyName:     "sb-sbdeadbeef",
		RelayPort:     11042,
		Generation:    generation,
		IssuedAt:      testNow.Add(-time.Minute),
		ExpiresAt:     testNow.Add(9 * time.Minute),
		JTI:           jti,
	}
}

func signTestCredential(t *testing.T, privateKey ed25519.PrivateKey, claims testCredentialClaims) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signature := ed25519.Sign(privateKey, payload)
	return "sbrelay1." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (fixture *pluginFixture) user(token string, generation int) map[string]any {
	return map[string]any{
		"user": "",
		"metas": map[string]string{
			CredentialMetadataKey: token,
			GenerationMetadataKey: strconv.Itoa(generation),
		},
		"run_id": fixture.runID,
	}
}

func (fixture *pluginFixture) loginContent(token string, claims testCredentialClaims) map[string]any {
	return map[string]any{
		"version":    "0.71.0",
		"hostname":   "sharebridge-agent",
		"os":         "linux",
		"arch":       "amd64",
		"user":       "",
		"timestamp":  testNow.Unix(),
		"run_id":     fixture.runID,
		"pool_count": 0,
		"metas": map[string]string{
			CredentialMetadataKey: token,
			GenerationMetadataKey: strconv.Itoa(claims.Generation),
		},
		"client_address": "203.0.113.9:54321",
	}
}

func (fixture *pluginFixture) proxyContent(token string, generation int) map[string]any {
	return map[string]any{
		"user":            fixture.user(token, generation),
		"proxy_name":      fixture.claims.ProxyName,
		"proxy_type":      "tcp",
		"use_encryption":  false,
		"use_compression": false,
		"remote_port":     fixture.claims.RelayPort,
	}
}

func (fixture *pluginFixture) pingContent(token string, generation int) map[string]any {
	return map[string]any{
		"user":          fixture.user(token, generation),
		"timestamp":     testNow.Unix(),
		"privilege_key": "frp-auth-value",
	}
}

func (fixture *pluginFixture) closeContent(token string, generation int) map[string]any {
	return map[string]any{
		"user":       fixture.user(token, generation),
		"proxy_name": fixture.claims.ProxyName,
	}
}

type testPluginResponse struct {
	Reject       bool            `json:"reject"`
	RejectReason string          `json:"reject_reason"`
	Unchange     bool            `json:"unchange"`
	Content      json.RawMessage `json:"content"`
}

func (fixture *pluginFixture) request(operation string, content any) testPluginResponse {
	fixture.t.Helper()
	return fixture.requestWithOptions(operation, content, func(request *http.Request) {
		request.RemoteAddr = "127.0.0.1:43210"
		request.SetBasicAuth(PluginAuthUsername, testPluginSecret)
	})
}

func (fixture *pluginFixture) requestWithOptions(operation string, content any, configure func(*http.Request)) testPluginResponse {
	fixture.t.Helper()
	envelope := map[string]any{"version": FRPPluginAPIVersion, "op": operation, "content": content}
	body, err := json.Marshal(envelope)
	if err != nil {
		fixture.t.Fatalf("marshal request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, APIPath+"?version="+FRPPluginAPIVersion+"&op="+operation, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if configure != nil {
		configure(request)
	}
	responseRecorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(responseRecorder, request)
	if responseRecorder.Code != http.StatusOK {
		fixture.t.Fatalf("status = %d, body = %s", responseRecorder.Code, responseRecorder.Body.String())
	}
	var response testPluginResponse
	if err := json.Unmarshal(responseRecorder.Body.Bytes(), &response); err != nil {
		fixture.t.Fatalf("decode response: %v; body=%q", err, responseRecorder.Body.String())
	}
	return response
}

func requireAllowed(t *testing.T, response testPluginResponse) {
	t.Helper()
	if response.Reject || !response.Unchange {
		t.Fatalf("expected unchanged allow response, got %+v", response)
	}
}

func requireRejected(t *testing.T, response testPluginResponse) {
	t.Helper()
	if !response.Reject || response.Unchange || response.RejectReason == "" {
		t.Fatalf("expected fail-closed reject response, got %+v", response)
	}
}

func loginFixture(t *testing.T, fixture *pluginFixture) {
	t.Helper()
	requireAllowed(t, fixture.request(OperationLogin, fixture.loginContent(fixture.token, fixture.claims)))
}

func loginAndProxyFixture(t *testing.T, fixture *pluginFixture) {
	t.Helper()
	loginFixture(t, fixture)
	requireAllowed(t, fixture.request(OperationNewProxy, fixture.proxyContent(fixture.token, fixture.claims.Generation)))
}

func TestPluginAcceptsExactLoginAndSingleTCPProxy(t *testing.T) {
	fixture := newPluginFixture(t)
	loginAndProxyFixture(t, fixture)
	requireAllowed(t, fixture.request(OperationPing, fixture.pingContent(fixture.token, fixture.claims.Generation)))
	requireAllowed(t, fixture.request(OperationCloseProxy, fixture.closeContent(fixture.token, fixture.claims.Generation)))

	events := fixture.recorder.waitForCount(t, 4)
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4: %+v", len(events), events)
	}
	for index, operation := range []string{OperationLogin, OperationNewProxy, OperationPing, OperationCloseProxy} {
		fact := events[index]
		if fact.Operation != operation || fact.AgentRecordID != fixture.claims.AgentRecordID ||
			fact.Namespace != fixture.claims.Namespace || fact.ProxyName != fixture.claims.ProxyName ||
			fact.RelayPort != fixture.claims.RelayPort || fact.Generation != fixture.claims.Generation ||
			fact.RunID != fixture.runID {
			t.Fatalf("event %d mismatch: %+v", index, fact)
		}
		encoded, err := json.Marshal(fact)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		if bytes.Contains(encoded, []byte(fixture.token)) {
			t.Fatal("presence fact exposed credential material")
		}
	}
}

func TestPluginAcceptsFirstLoginWithoutRunIDAndBindsServerRunID(t *testing.T) {
	fixture := newPluginFixture(t)
	login := fixture.loginContent(fixture.token, fixture.claims)
	login["run_id"] = ""
	requireAllowed(t, fixture.request(OperationLogin, login))
	requireAllowed(t, fixture.request(OperationNewProxy, fixture.proxyContent(fixture.token, fixture.claims.Generation)))

	events := fixture.recorder.waitForCount(t, 2)
	if len(events) != 2 || events[0].RunID != "" || events[1].RunID != fixture.runID {
		t.Fatalf("unexpected first-login run IDs: %+v", events)
	}
}

func TestPluginRejectsInvalidCredential(t *testing.T) {
	fixture := newPluginFixture(t)
	parts := strings.Split(fixture.token, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	signature[0] ^= 0xff
	invalid := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(signature)
	requireRejected(t, fixture.request(OperationLogin, fixture.loginContent(invalid, fixture.claims)))

	invalidClaims := []struct {
		name   string
		mutate func(*testCredentialClaims)
	}{
		{name: "issuer", mutate: func(claims *testCredentialClaims) { claims.Issuer = "other-control" }},
		{name: "audience", mutate: func(claims *testCredentialClaims) { claims.Audience = "other-service" }},
		{name: "api key", mutate: func(claims *testCredentialClaims) { claims.APIKeyID = "" }},
		{name: "agent", mutate: func(claims *testCredentialClaims) { claims.AgentRecordID = "" }},
		{name: "namespace", mutate: func(claims *testCredentialClaims) { claims.Namespace = "invalid" }},
		{name: "proxy name", mutate: func(claims *testCredentialClaims) { claims.ProxyName = "arbitrary" }},
		{name: "generation", mutate: func(claims *testCredentialClaims) { claims.Generation = -1 }},
		{name: "jti", mutate: func(claims *testCredentialClaims) { claims.JTI = "" }},
		{name: "future issue", mutate: func(claims *testCredentialClaims) { claims.IssuedAt = testNow.Add(time.Second) }},
		{name: "overlong lifetime", mutate: func(claims *testCredentialClaims) {
			claims.ExpiresAt = claims.IssuedAt.Add(10*time.Minute + time.Second)
		}},
	}
	for index, testCase := range invalidClaims {
		t.Run(testCase.name, func(t *testing.T) {
			claims := validTestClaims(1, fmt.Sprintf("invalid-claims-%d", index))
			testCase.mutate(&claims)
			token := signTestCredential(t, fixture.privateKey, claims)
			requireRejected(t, fixture.request(OperationLogin, fixture.loginContent(token, claims)))
		})
	}
}

func TestPluginRejectsCredentialWithUnknownClaim(t *testing.T) {
	fixture := newPluginFixture(t)
	payload, err := json.Marshal(map[string]any{
		"iss":             fixture.claims.Issuer,
		"aud":             fixture.claims.Audience,
		"api_key_id":      fixture.claims.APIKeyID,
		"agent_record_id": fixture.claims.AgentRecordID,
		"namespace":       fixture.claims.Namespace,
		"proxy_name":      fixture.claims.ProxyName,
		"relay_port":      fixture.claims.RelayPort,
		"generation":      fixture.claims.Generation,
		"issued_at":       fixture.claims.IssuedAt,
		"expires_at":      fixture.claims.ExpiresAt,
		"jti":             "unknown-claim-jti",
		"admin":           true,
	})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signature := ed25519.Sign(fixture.privateKey, payload)
	token := "sbrelay1." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
	requireRejected(t, fixture.request(OperationLogin, fixture.loginContent(token, fixture.claims)))
}

func TestPluginRejectsExpiredCredential(t *testing.T) {
	fixture := newPluginFixture(t)
	claims := validTestClaims(1, "expired-jti")
	claims.IssuedAt = testNow.Add(-11 * time.Minute)
	claims.ExpiresAt = testNow
	token := signTestCredential(t, fixture.privateKey, claims)
	requireRejected(t, fixture.request(OperationLogin, fixture.loginContent(token, claims)))
}

func TestAcceptedTunnelMayOutliveCredentialExpiry(t *testing.T) {
	clock := testNow
	fixture := newPluginFixture(t, func(config *Config) {
		config.Now = func() time.Time { return clock }
	})
	loginAndProxyFixture(t, fixture)
	clock = testNow.Add(20 * time.Minute)
	requireAllowed(t, fixture.request(OperationPing, fixture.pingContent(fixture.token, fixture.claims.Generation)))
}

func TestPluginRejectsCredentialThatExpiresWhileWaitingForAdmissionLock(t *testing.T) {
	clock := testNow
	var clockMu sync.Mutex
	prevalidated := make(chan struct{})
	var clockOnce sync.Once
	fixture := newPluginFixture(t, func(config *Config) {
		config.Now = func() time.Time {
			clockMu.Lock()
			now := clock
			clockMu.Unlock()
			clockOnce.Do(func() { close(prevalidated) })
			return now
		}
	})
	fixture.claims.ExpiresAt = testNow.Add(time.Second)
	fixture.token = signTestCredential(t, fixture.privateKey, fixture.claims)
	server := fixture.handler.(*Server)
	server.mu.Lock()
	responses := make(chan testPluginResponse, 1)
	go func() {
		responses <- fixture.request(OperationLogin, fixture.loginContent(fixture.token, fixture.claims))
	}()
	<-prevalidated
	clockMu.Lock()
	clock = fixture.claims.ExpiresAt
	clockMu.Unlock()
	server.mu.Unlock()
	requireRejected(t, <-responses)
}

func TestPluginDispatchesEventsInOrderWithoutCallbackControllingAdmissionLatency(t *testing.T) {
	recorder := &blockingEventRecorder{entered: make(chan struct{}), release: make(chan struct{})}
	defer recorder.unblock()
	fixture := newPluginFixture(t, func(config *Config) { config.PresenceEvents = recorder })
	responses := make(chan testPluginResponse, 1)
	go func() {
		responses <- fixture.request(OperationLogin, fixture.loginContent(fixture.token, fixture.claims))
	}()
	<-recorder.entered
	select {
	case response := <-responses:
		requireAllowed(t, response)
	case <-time.After(time.Second):
		recorder.unblock()
		<-responses
		t.Fatal("presence callback controlled Login admission latency")
	}
	requireAllowed(t, fixture.request(OperationNewProxy, fixture.proxyContent(fixture.token, fixture.claims.Generation)))
	requireAllowed(t, fixture.request(OperationPing, fixture.pingContent(fixture.token, fixture.claims.Generation)))
	recorder.unblock()
	events := recorder.waitForCount(t, 3)
	for index, operation := range []string{OperationLogin, OperationNewProxy, OperationPing} {
		if events[index].Operation != operation {
			t.Fatalf("event %d = %q, want %q", index, events[index].Operation, operation)
		}
	}
}

func TestPluginRejectsAdmissionWhenOrderedEventQueueIsFull(t *testing.T) {
	recorder := &blockingEventRecorder{entered: make(chan struct{}), release: make(chan struct{})}
	defer recorder.unblock()
	fixture := newPluginFixture(t, func(config *Config) {
		config.PresenceEvents = recorder
		config.MaxPendingEvents = 2
	})

	loginResponses := make(chan testPluginResponse, 1)
	go func() {
		loginResponses <- fixture.request(OperationLogin, fixture.loginContent(fixture.token, fixture.claims))
	}()
	<-recorder.entered
	select {
	case response := <-loginResponses:
		requireAllowed(t, response)
	case <-time.After(time.Second):
		recorder.unblock()
		<-loginResponses
		t.Fatal("presence callback controlled Login admission latency")
	}

	requireAllowed(t, fixture.request(OperationNewProxy, fixture.proxyContent(fixture.token, fixture.claims.Generation)))
	requireRejected(t, fixture.request(OperationPing, fixture.pingContent(fixture.token, fixture.claims.Generation)))

	recorder.unblock()
	events := recorder.waitForCount(t, 2)
	if events[0].Operation != OperationLogin || events[1].Operation != OperationNewProxy {
		t.Fatalf("events after overload = %+v, want ordered Login and NewProxy authorization facts", events)
	}
	requireAllowed(t, fixture.request(OperationPing, fixture.pingContent(fixture.token, fixture.claims.Generation)))
}

func TestPluginRejectsReplayedCredential(t *testing.T) {
	fixture := newPluginFixture(t)
	loginFixture(t, fixture)
	fixture.runID = "run-replay"
	requireRejected(t, fixture.request(OperationLogin, fixture.loginContent(fixture.token, fixture.claims)))
}

func TestPluginRejectsSupersededCredential(t *testing.T) {
	fixture := newPluginFixture(t)
	loginFixture(t, fixture)

	newClaims := validTestClaims(2, "generation-two")
	newToken := signTestCredential(t, fixture.privateKey, newClaims)
	fixture.runID = "run-two"
	requireAllowed(t, fixture.request(OperationLogin, fixture.loginContent(newToken, newClaims)))

	oldClaims := validTestClaims(1, "late-generation-one")
	oldToken := signTestCredential(t, fixture.privateKey, oldClaims)
	fixture.runID = "run-old"
	requireRejected(t, fixture.request(OperationLogin, fixture.loginContent(oldToken, oldClaims)))

	fixture.runID = "run-one"
	requireRejected(t, fixture.request(OperationPing, fixture.pingContent(fixture.token, 1)))
}

func TestPluginRejectsOlderUnusedCredentialAtSameGeneration(t *testing.T) {
	fixture := newPluginFixture(t)
	loginFixture(t, fixture)
	older := validTestClaims(1, "older-unused-jti")
	older.IssuedAt = fixture.claims.IssuedAt.Add(-time.Second)
	older.ExpiresAt = older.IssuedAt.Add(10 * time.Minute)
	token := signTestCredential(t, fixture.privateKey, older)
	fixture.runID = "older-run"
	requireRejected(t, fixture.request(OperationLogin, fixture.loginContent(token, older)))
}

func TestPluginAcceptsStrictlyNewerCredentialAtSameGeneration(t *testing.T) {
	fixture := newPluginFixture(t)
	loginFixture(t, fixture)
	newer := validTestClaims(1, "newer-unused-jti")
	newer.IssuedAt = fixture.claims.IssuedAt.Add(time.Second)
	newer.ExpiresAt = newer.IssuedAt.Add(10 * time.Minute)
	token := signTestCredential(t, fixture.privateKey, newer)
	fixture.runID = "newer-run"
	requireAllowed(t, fixture.request(OperationLogin, fixture.loginContent(token, newer)))

	fixture.runID = "run-one"
	requireRejected(t, fixture.request(OperationPing, fixture.pingContent(fixture.token, fixture.claims.Generation)))
}

func TestPluginRejectsDistinctCredentialWithEqualIssuanceAtSameGeneration(t *testing.T) {
	fixture := newPluginFixture(t)
	loginFixture(t, fixture)
	equalTime := validTestClaims(1, "equal-time-distinct-jti")
	equalTime.IssuedAt = fixture.claims.IssuedAt
	equalTime.ExpiresAt = fixture.claims.ExpiresAt
	token := signTestCredential(t, fixture.privateKey, equalTime)
	fixture.runID = "equal-time-run"
	requireRejected(t, fixture.request(OperationLogin, fixture.loginContent(token, equalTime)))
}

func TestPluginRetainsBoundedGenerationFenceAfterClose(t *testing.T) {
	clock := testNow
	fixture := newPluginFixture(t, func(config *Config) {
		config.Now = func() time.Time { return clock }
	})
	generationTwo := validTestClaims(2, "generation-two-current")
	generationTwoToken := signTestCredential(t, fixture.privateKey, generationTwo)
	fixture.claims = generationTwo
	fixture.token = generationTwoToken
	loginAndProxyFixture(t, fixture)
	requireAllowed(t, fixture.request(OperationCloseProxy, fixture.closeContent(generationTwoToken, 2)))

	clock = testNow.Add(20 * time.Minute)
	oldGeneration := validTestClaims(1, "generation-one-fresh-token")
	oldGeneration.IssuedAt = clock.Add(-time.Minute)
	oldGeneration.ExpiresAt = clock.Add(9 * time.Minute)
	oldToken := signTestCredential(t, fixture.privateKey, oldGeneration)
	fixture.runID = "run-old-generation"
	requireRejected(t, fixture.request(OperationLogin, fixture.loginContent(oldToken, oldGeneration)))
}

func TestPluginValidatesLoginPingAndCloseIdentity(t *testing.T) {
	t.Run("login identity", func(t *testing.T) {
		fixture := newPluginFixture(t)
		content := fixture.loginContent(fixture.token, fixture.claims)
		content["user"] = "other-agent"
		requireRejected(t, fixture.request(OperationLogin, content))
	})

	t.Run("ping generation", func(t *testing.T) {
		fixture := newPluginFixture(t)
		loginAndProxyFixture(t, fixture)
		content := fixture.pingContent(fixture.token, fixture.claims.Generation)
		content["user"].(map[string]any)["metas"].(map[string]string)[GenerationMetadataKey] = "2"
		requireRejected(t, fixture.request(OperationPing, content))
	})

	t.Run("ping run ID", func(t *testing.T) {
		fixture := newPluginFixture(t)
		loginAndProxyFixture(t, fixture)
		content := fixture.pingContent(fixture.token, fixture.claims.Generation)
		content["user"].(map[string]any)["run_id"] = "other-run"
		requireRejected(t, fixture.request(OperationPing, content))
	})

	t.Run("close proxy name", func(t *testing.T) {
		fixture := newPluginFixture(t)
		loginAndProxyFixture(t, fixture)
		content := fixture.closeContent(fixture.token, fixture.claims.Generation)
		content["proxy_name"] = "other-proxy"
		requireRejected(t, fixture.request(OperationCloseProxy, content))
	})
}

func TestPluginRejectsIdentityNamePortAndGenerationMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*pluginFixture, map[string]any)
	}{
		{name: "identity", mutate: func(_ *pluginFixture, content map[string]any) {
			content["user"].(map[string]any)["user"] = "other-agent"
		}},
		{name: "name", mutate: func(_ *pluginFixture, content map[string]any) { content["proxy_name"] = "other-proxy" }},
		{name: "port", mutate: func(_ *pluginFixture, content map[string]any) { content["remote_port"] = 11043 }},
		{name: "generation", mutate: func(_ *pluginFixture, content map[string]any) {
			content["user"].(map[string]any)["metas"].(map[string]string)[GenerationMetadataKey] = "2"
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newPluginFixture(t)
			loginFixture(t, fixture)
			content := fixture.proxyContent(fixture.token, fixture.claims.Generation)
			testCase.mutate(fixture, content)
			requireRejected(t, fixture.request(OperationNewProxy, content))
		})
	}
}

func TestPluginRejectsNonTCPProxy(t *testing.T) {
	fixture := newPluginFixture(t)
	loginFixture(t, fixture)
	content := fixture.proxyContent(fixture.token, fixture.claims.Generation)
	content["proxy_type"] = "udp"
	requireRejected(t, fixture.request(OperationNewProxy, content))
}

func TestPluginAllowsIdenticalNewProxyAuthorizationRetry(t *testing.T) {
	fixture := newPluginFixture(t)
	loginAndProxyFixture(t, fixture)
	requireAllowed(t, fixture.request(OperationNewProxy, fixture.proxyContent(fixture.token, fixture.claims.Generation)))
	requireAllowed(t, fixture.request(OperationPing, fixture.pingContent(fixture.token, fixture.claims.Generation)))

	events := fixture.recorder.waitForCount(t, 3)
	for index, operation := range []string{OperationLogin, OperationNewProxy, OperationPing} {
		if events[index].Operation != operation {
			t.Fatalf("event %d = %q, want %q; identical retry emitted a duplicate authorization fact", index, events[index].Operation, operation)
		}
	}
}

func TestPluginRejectsSecondProxy(t *testing.T) {
	fixture := newPluginFixture(t)
	loginAndProxyFixture(t, fixture)
	second := fixture.proxyContent(fixture.token, fixture.claims.Generation)
	second["proxy_name"] = "second-proxy"
	requireRejected(t, fixture.request(OperationNewProxy, second))
}

func TestPluginRejectsOutOfRangePort(t *testing.T) {
	fixture := newPluginFixture(t)
	claims := validTestClaims(1, "out-of-range")
	claims.RelayPort = 12000
	token := signTestCredential(t, fixture.privateKey, claims)
	requireRejected(t, fixture.request(OperationLogin, fixture.loginContent(token, claims)))
}

func TestPluginRejectsCompression(t *testing.T) {
	fixture := newPluginFixture(t)
	loginFixture(t, fixture)
	content := fixture.proxyContent(fixture.token, fixture.claims.Generation)
	content["use_compression"] = true
	requireRejected(t, fixture.request(OperationNewProxy, content))
}

func TestPluginRejectsUnapprovedProxyOptions(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value any
	}{
		{name: "encryption", field: "use_encryption", value: true},
		{name: "bandwidth limit", field: "bandwidth_limit", value: "1MB"},
		{name: "bandwidth mode", field: "bandwidth_limit_mode", value: "server"},
		{name: "group", field: "group", value: "group"},
		{name: "group key", field: "group_key", value: "key"},
		{name: "proxy metadata", field: "metas", value: map[string]string{"key": "value"}},
		{name: "annotations", field: "annotations", value: map[string]string{"key": "value"}},
		{name: "custom domains", field: "custom_domains", value: []string{"example.com"}},
		{name: "subdomain", field: "subdomain", value: "sub"},
		{name: "locations", field: "locations", value: []string{"/"}},
		{name: "http user", field: "http_user", value: "user"},
		{name: "http password", field: "http_pwd", value: "password"},
		{name: "host rewrite", field: "host_header_rewrite", value: "example.com"},
		{name: "headers", field: "headers", value: map[string]string{"x": "y"}},
		{name: "response headers", field: "response_headers", value: map[string]string{"x": "y"}},
		{name: "route by http user", field: "route_by_http_user", value: "user"},
		{name: "secret key", field: "sk", value: "key"},
		{name: "allow users", field: "allow_users", value: []string{"user"}},
		{name: "multiplexer", field: "multiplexer", value: "httpconnect"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newPluginFixture(t)
			loginFixture(t, fixture)
			content := fixture.proxyContent(fixture.token, fixture.claims.Generation)
			content[testCase.field] = testCase.value
			requireRejected(t, fixture.request(OperationNewProxy, content))
		})
	}
}

func TestPluginRejectsUnapprovedLoginOptions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "oversized connection pool", mutate: func(content map[string]any) { content["pool_count"] = 2 }},
		{name: "client identity override", mutate: func(content map[string]any) { content["client_id"] = "custom-client" }},
		{name: "client spec", mutate: func(content map[string]any) { content["client_spec"] = map[string]any{"always_auth_pass": true} }},
		{name: "extra metadata", mutate: func(content map[string]any) {
			content["metas"].(map[string]string)["unapproved"] = "value"
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newPluginFixture(t)
			content := fixture.loginContent(fixture.token, fixture.claims)
			testCase.mutate(content)
			requireRejected(t, fixture.request(OperationLogin, content))
		})
	}
}

func TestPluginRejectsUnknownOperation(t *testing.T) {
	fixture := newPluginFixture(t)
	requireRejected(t, fixture.request("Bogus", map[string]any{}))
}

// Task 7 amendment (2026-09-04): NewUserConn is readiness-only. It always
// returns FRP's accept response — never an authorization input, never a
// rejection — and records the correlation tuple for the presence registry.
func TestNewUserConnIsReadinessOnlyAndNeverAuthorizes(t *testing.T) {
	t.Run("accepts without a session and creates none", func(t *testing.T) {
		fixture := newPluginFixture(t)
		content := map[string]any{
			"user":        fixture.user(fixture.token, fixture.claims.Generation),
			"proxy_name":  fixture.claims.ProxyName,
			"proxy_type":  "tcp",
			"remote_addr": "127.0.0.1:54321",
		}
		requireAllowed(t, fixture.request(OperationNewUserConn, content))
		requireAllowed(t, fixture.request(OperationNewUserConn, map[string]any{}))
		if events := fixture.recorder.snapshot(); len(events) != 0 {
			t.Fatalf("unattributable NewUserConn emitted facts: %+v", events)
		}
		// Readiness handling must not have created admission state.
		requireRejected(t, fixture.request(OperationPing, fixture.pingContent(fixture.token, fixture.claims.Generation)))
	})

	t.Run("never flips proxy authorization", func(t *testing.T) {
		fixture := newPluginFixture(t)
		loginFixture(t, fixture)
		content := map[string]any{
			"user":        fixture.user(fixture.token, fixture.claims.Generation),
			"proxy_name":  fixture.claims.ProxyName,
			"proxy_type":  "tcp",
			"remote_addr": "127.0.0.1:54321",
		}
		requireAllowed(t, fixture.request(OperationNewUserConn, content))
		requireAllowed(t, fixture.request(OperationNewUserConn, content))
		// CloseProxy is only accepted for an authorized proxy; if NewUserConn
		// had flipped proxyAuthorized it would now be accepted.
		requireRejected(t, fixture.request(OperationCloseProxy, fixture.closeContent(fixture.token, fixture.claims.Generation)))
		// The only emitted facts are the authorization facts, not user conns.
		events := fixture.recorder.waitForCount(t, 1)
		if events[0].Operation != OperationLogin {
			t.Fatalf("first event = %+v, want only the Login fact", events[0])
		}
	})

	t.Run("accepts malformed and unattributable callbacks on a live session", func(t *testing.T) {
		fixture := newPluginFixture(t)
		loginAndProxyFixture(t, fixture)
		foreign := validTestClaims(9, "foreign-jti")
		foreignToken := signTestCredential(t, fixture.privateKey, foreign)
		baseTuple := func() map[string]any {
			return map[string]any{
				"user":        fixture.user(fixture.token, fixture.claims.Generation),
				"proxy_name":  fixture.claims.ProxyName,
				"proxy_type":  "tcp",
				"remote_addr": "127.0.0.1:54321",
			}
		}
		emptyRunID := baseTuple()
		emptyRunID["user"] = map[string]any{
			"user":   "",
			"metas":  fixture.user(fixture.token, fixture.claims.Generation)["metas"],
			"run_id": "",
		}
		variants := []struct {
			name    string
			content map[string]any
		}{
			{name: "empty content", content: map[string]any{}},
			{name: "wrong proxy name", content: func() map[string]any {
				content := baseTuple()
				content["proxy_name"] = "sb-other"
				return content
			}()},
			{name: "unknown credential", content: func() map[string]any {
				content := baseTuple()
				content["user"] = fixture.user(foreignToken, 9)
				return content
			}()},
			{name: "missing remote address", content: func() map[string]any {
				content := baseTuple()
				delete(content, "remote_addr")
				return content
			}()},
			{name: "empty run id", content: emptyRunID},
			{name: "oversized remote address", content: func() map[string]any {
				content := baseTuple()
				content["remote_addr"] = strings.Repeat("a", 80)
				return content
			}()},
		}
		for _, variant := range variants {
			t.Run(variant.name, func(t *testing.T) {
				requireAllowed(t, fixture.request(OperationNewUserConn, variant.content))
			})
		}
		// The session and its authorization state are untouched.
		requireAllowed(t, fixture.request(OperationPing, fixture.pingContent(fixture.token, fixture.claims.Generation)))
	})
}

func TestNewUserConnCorrelationTupleRecordedBounded(t *testing.T) {
	t.Run("records the correlation tuple once per user connection", func(t *testing.T) {
		fixture := newPluginFixture(t)
		loginAndProxyFixture(t, fixture)
		first := map[string]any{
			"user":        fixture.user(fixture.token, fixture.claims.Generation),
			"proxy_name":  fixture.claims.ProxyName,
			"proxy_type":  "tcp",
			"remote_addr": "127.0.0.1:54321",
		}
		requireAllowed(t, fixture.request(OperationNewUserConn, first))
		requireAllowed(t, fixture.request(OperationNewUserConn, first)) // duplicate callback
		second := map[string]any{
			"user":        fixture.user(fixture.token, fixture.claims.Generation),
			"proxy_name":  fixture.claims.ProxyName,
			"proxy_type":  "tcp",
			"remote_addr": "127.0.0.1:54322",
		}
		requireAllowed(t, fixture.request(OperationNewUserConn, second))

		events := fixture.recorder.waitForCount(t, 4)
		wantSources := []string{"127.0.0.1:54321", "127.0.0.1:54322"}
		for index, fact := range events[2:] {
			if fact.Operation != OperationNewUserConn ||
				fact.ProxyName != fixture.claims.ProxyName ||
				fact.RunID != fixture.runID ||
				fact.Generation != fixture.claims.Generation ||
				fact.AgentRecordID != fixture.claims.AgentRecordID ||
				fact.Namespace != fixture.claims.Namespace ||
				fact.RelayPort != fixture.claims.RelayPort ||
				fact.RemoteAddr != wantSources[index] {
				t.Fatalf("correlation fact %d = %+v", index, fact)
			}
			encoded, err := json.Marshal(fact)
			if err != nil {
				t.Fatalf("marshal fact: %v", err)
			}
			if bytes.Contains(encoded, []byte(fixture.token)) {
				t.Fatal("NewUserConn fact exposed credential material")
			}
		}
	})

	t.Run("drops facts when the ordered queue is full but still accepts", func(t *testing.T) {
		recorder := &blockingEventRecorder{entered: make(chan struct{}), release: make(chan struct{})}
		defer recorder.unblock()
		fixture := newPluginFixture(t, func(config *Config) {
			config.PresenceEvents = recorder
			config.MaxPendingEvents = 2
		})
		loginResponses := make(chan testPluginResponse, 1)
		go func() {
			loginResponses <- fixture.request(OperationLogin, fixture.loginContent(fixture.token, fixture.claims))
		}()
		<-recorder.entered // dispatcher blocked with the Login fact in flight
		requireAllowed(t, fixture.request(OperationNewProxy, fixture.proxyContent(fixture.token, fixture.claims.Generation)))

		tuple := map[string]any{
			"user":        fixture.user(fixture.token, fixture.claims.Generation),
			"proxy_name":  fixture.claims.ProxyName,
			"proxy_type":  "tcp",
			"remote_addr": "127.0.0.1:54321",
		}
		requireAllowed(t, fixture.request(OperationNewUserConn, tuple)) // queue full: fact dropped, never rejected

		recorder.unblock()
		events := recorder.waitForCount(t, 2)
		if events[0].Operation != OperationLogin || events[1].Operation != OperationNewProxy {
			t.Fatalf("events after overload = %+v", events)
		}
		requireAllowed(t, fixture.request(OperationNewUserConn, tuple)) // slot free again: recorded
		events = recorder.waitForCount(t, 3)
		if events[2].Operation != OperationNewUserConn || events[2].RemoteAddr != "127.0.0.1:54321" {
			t.Fatalf("third event = %+v, want the NewUserConn correlation fact", events[2])
		}
	})

	t.Run("bounds the deduplication state", func(t *testing.T) {
		fixture := newPluginFixture(t)
		loginAndProxyFixture(t, fixture)
		for index := 0; index < maxUserConnDedupEntries+80; index++ {
			tuple := map[string]any{
				"user":        fixture.user(fixture.token, fixture.claims.Generation),
				"proxy_name":  fixture.claims.ProxyName,
				"proxy_type":  "tcp",
				"remote_addr": fmt.Sprintf("127.0.0.1:%d", 20000+index),
			}
			requireAllowed(t, fixture.request(OperationNewUserConn, tuple))
		}
		server := fixture.handler.(*Server)
		server.mu.Lock()
		size := len(server.userConnSeen)
		server.mu.Unlock()
		if size > maxUserConnDedupEntries {
			t.Fatalf("dedup map size = %d, bound %d", size, maxUserConnDedupEntries)
		}
	})
}

func TestPluginRejectsMalformedOversizeAndProtocolMismatch(t *testing.T) {
	fixture := newPluginFixture(t)
	tests := []struct {
		name      string
		target    string
		body      string
		mediaType string
	}{
		{name: "bad JSON", target: APIPath + "?version=" + FRPPluginAPIVersion + "&op=Login", body: "{", mediaType: "application/json"},
		{name: "oversize", target: APIPath + "?version=" + FRPPluginAPIVersion + "&op=Login", body: strings.Repeat("x", MaxRequestBodyBytes+1), mediaType: "application/json"},
		{name: "query operation mismatch", target: APIPath + "?version=" + FRPPluginAPIVersion + "&op=Ping", body: `{"version":"0.1.0","op":"Login","content":{}}`, mediaType: "application/json"},
		{name: "query version mismatch", target: APIPath + "?version=9.9.9&op=Login", body: `{"version":"0.1.0","op":"Login","content":{}}`, mediaType: "application/json"},
		{name: "body version mismatch", target: APIPath + "?version=" + FRPPluginAPIVersion + "&op=Login", body: `{"version":"9.9.9","op":"Login","content":{}}`, mediaType: "application/json"},
		{name: "wrong content type", target: APIPath + "?version=" + FRPPluginAPIVersion + "&op=Login", body: `{}`, mediaType: "text/plain"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, testCase.target, strings.NewReader(testCase.body))
			request.RemoteAddr = "127.0.0.1:43210"
			request.Header.Set("Content-Type", testCase.mediaType)
			request.SetBasicAuth(PluginAuthUsername, testPluginSecret)
			responseRecorder := httptest.NewRecorder()
			fixture.handler.ServeHTTP(responseRecorder, request)
			if responseRecorder.Code != http.StatusOK {
				t.Fatalf("status = %d", responseRecorder.Code)
			}
			var response testPluginResponse
			if err := json.Unmarshal(responseRecorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			requireRejected(t, response)
		})
	}
}

func TestPluginRejectsNonLoopbackAndMissingSharedAuthentication(t *testing.T) {
	fixture := newPluginFixture(t)
	content := fixture.loginContent(fixture.token, fixture.claims)

	nonLoopback := fixture.requestWithOptions(OperationLogin, content, func(request *http.Request) {
		request.RemoteAddr = "198.51.100.20:43210"
		request.SetBasicAuth(PluginAuthUsername, testPluginSecret)
	})
	requireRejected(t, nonLoopback)

	missingAuthentication := fixture.requestWithOptions(OperationLogin, content, func(request *http.Request) {
		request.RemoteAddr = "127.0.0.1:43210"
	})
	requireRejected(t, missingAuthentication)

	wrongAuthentication := fixture.requestWithOptions(OperationLogin, content, func(request *http.Request) {
		request.RemoteAddr = "127.0.0.1:43210"
		request.SetBasicAuth(PluginAuthUsername, "wrong")
	})
	requireRejected(t, wrongAuthentication)
}

func TestPluginBoundsReplayAndSessionState(t *testing.T) {
	fixture := newPluginFixture(t, func(config *Config) {
		config.MaxReplayEntries = 1
		config.MaxSessions = 1
	})
	loginFixture(t, fixture)

	secondClaims := validTestClaims(1, "second-agent-jti")
	secondClaims.AgentRecordID = "agent-record-two"
	secondClaims.Namespace = "sbcafebabe"
	secondClaims.ProxyName = "sb-sbcafebabe"
	secondClaims.RelayPort = 11043
	secondToken := signTestCredential(t, fixture.privateKey, secondClaims)
	fixture.runID = "run-two"
	requireRejected(t, fixture.request(OperationLogin, fixture.loginContent(secondToken, secondClaims)))
}

func TestPluginConcurrentPingStateIsRaceSafe(t *testing.T) {
	fixture := newPluginFixture(t)
	loginAndProxyFixture(t, fixture)

	var waitGroup sync.WaitGroup
	failures := make(chan string, 32)
	for index := 0; index < 32; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			response := fixture.request(OperationPing, fixture.pingContent(fixture.token, fixture.claims.Generation))
			if response.Reject || !response.Unchange {
				failures <- fmt.Sprintf("unexpected response: %+v", response)
			}
		}()
	}
	waitGroup.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}

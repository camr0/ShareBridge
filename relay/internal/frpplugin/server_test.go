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

	events := fixture.recorder.snapshot()
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

	events := fixture.recorder.snapshot()
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

func TestPluginRejectsSecondProxy(t *testing.T) {
	fixture := newPluginFixture(t)
	loginAndProxyFixture(t, fixture)
	requireRejected(t, fixture.request(OperationNewProxy, fixture.proxyContent(fixture.token, fixture.claims.Generation)))
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
		{name: "connection pool", mutate: func(content map[string]any) { content["pool_count"] = 1 }},
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
	requireRejected(t, fixture.request("NewUserConn", map[string]any{}))
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

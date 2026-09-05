package tunnel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validTestConfig returns a relay_config payload shaped like control's
// §11.1 message for a settled assignment.
func validTestConfig() Config {
	return Config{
		Version:     RelayConfigVersion,
		Generation:  7,
		GatewayAddr: "relay.example.org",
		GatewayPort: 7000,
		ProxyName:   "sb-docs",
		RelayPort:   10042,
		Credential:  "header.payload.signature",
		ExpiresAt:   time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second),
	}
}

// testRenderSettings returns renderer settings pointing into a test
// temporary directory.
func testRenderSettings(t *testing.T) RenderSettings {
	t.Helper()
	dataDirectory := t.TempDir()
	return RenderSettings{
		TrustedCAFile: filepath.Join(dataDirectory, "relay-ca.pem"),
		LocalTarget:   LocalTarget,
	}
}

// renderedLineValue returns the value assigned to an exact top-level TOML
// key, failing the test when the key is absent or duplicated.
func renderedLineValue(t *testing.T, rendered []byte, key string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(string(rendered), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if assignment, ok := strings.CutPrefix(trimmed, key+" = "); ok {
			found = append(found, assignment)
		}
	}
	if len(found) != 1 {
		t.Fatalf("rendered config has %d assignments for key %q, want exactly 1:\n%s", len(found), key, rendered)
	}
	return found[0]
}

// proxyBlockKeys returns the key set of the single [[proxies]] block so the
// test can assert the renderer emits only the assigned identity keys.
func proxyBlockKeys(t *testing.T, rendered []byte) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	inProxyBlock := false
	proxyBlockCount := 0
	for _, line := range strings.Split(string(rendered), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "[[proxies]]" {
			proxyBlockCount++
			inProxyBlock = true
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			inProxyBlock = false
			continue
		}
		if inProxyBlock {
			key, _, _ := strings.Cut(trimmed, " = ")
			keys[key] = true
		}
	}
	if proxyBlockCount != 1 {
		t.Fatalf("rendered config declares %d [[proxies]] blocks, want exactly 1:\n%s", proxyBlockCount, rendered)
	}
	return keys
}

func TestRenderConfigHasOneFixedLoopbackProxy(t *testing.T) {
	config := validTestConfig()
	settings := testRenderSettings(t)

	rendered, err := Render(config, settings)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// Exactly one proxy, addressed only by its assigned name and relay port.
	proxyKeys := proxyBlockKeys(t, rendered)
	wantProxyKeys := map[string]bool{
		"name":                     true,
		"type":                     true,
		"localIP":                  true,
		"localPort":                true,
		"remotePort":               true,
		"transport.useCompression": true,
	}
	if len(proxyKeys) != len(wantProxyKeys) {
		t.Fatalf("proxy block keys = %v, want exactly %v", proxyKeys, wantProxyKeys)
	}
	for key := range wantProxyKeys {
		if !proxyKeys[key] {
			t.Errorf("proxy block missing required key %q", key)
		}
	}
	if got := renderedLineValue(t, rendered, "type"); got != `"tcp"` {
		t.Errorf("proxy type = %s, want \"tcp\" (TCP is the only approved proxy type)", got)
	}
	if got := renderedLineValue(t, rendered, "name"); got != fmt.Sprintf("%q", config.ProxyName) {
		t.Errorf("proxy name = %s, want the assigned %q", got, config.ProxyName)
	}
	if got := renderedLineValue(t, rendered, "remotePort"); got != fmt.Sprintf("%d", config.RelayPort) {
		t.Errorf("proxy remotePort = %s, want the assigned %d", got, config.RelayPort)
	}
	if got := renderedLineValue(t, rendered, "transport.useCompression"); got != "false" {
		t.Errorf("proxy useCompression = %s, want false (compression disabled, spec §14)", got)
	}

	// The local target is fixed to the agent HTTPS listener on loopback
	// (spec §4.5): callers cannot redirect the tunnel.
	if got := renderedLineValue(t, rendered, "localIP"); got != `"127.0.0.1"` {
		t.Errorf("proxy localIP = %s, want \"127.0.0.1\"", got)
	}
	if got := renderedLineValue(t, rendered, "localPort"); got != "8443" {
		t.Errorf("proxy localPort = %s, want 8443 (the fixed HTTPS listener)", got)
	}

	// Transport hard requirements from the 2026-09-04 amendment (proven by
	// the Task 8 gates): verified TLS with explicit CA and server name,
	// poolCount pinned to 1, TCP multiplexing off so authenticated
	// application Pings reach frps, and the explicit 10-second Ping.
	if got := renderedLineValue(t, rendered, "transport.tls.trustedCaFile"); got != fmt.Sprintf("%q", settings.TrustedCAFile) {
		t.Errorf("transport.tls.trustedCaFile = %s, want %q (verification must fail closed)", got, settings.TrustedCAFile)
	}
	if got := renderedLineValue(t, rendered, "transport.tls.serverName"); got != fmt.Sprintf("%q", config.GatewayAddr) {
		t.Errorf("transport.tls.serverName = %s, want the relay_config gateway address %q", got, config.GatewayAddr)
	}
	if got := renderedLineValue(t, rendered, "transport.tls.enable"); got != "true" {
		t.Errorf("transport.tls.enable = %s, want true", got)
	}
	if got := renderedLineValue(t, rendered, "transport.poolCount"); got != "1" {
		t.Errorf("transport.poolCount = %s, want 1", got)
	}
	if got := renderedLineValue(t, rendered, "transport.tcpMux"); got != "false" {
		t.Errorf("transport.tcpMux = %s, want false (mux suppresses application Pings)", got)
	}
	if got := renderedLineValue(t, rendered, "transport.heartbeatInterval"); got != "10" {
		t.Errorf("transport.heartbeatInterval = %s, want 10 (explicit 10-second Ping)", got)
	}
	if got := renderedLineValue(t, rendered, "transport.heartbeatTimeout"); got != "45" {
		t.Errorf("transport.heartbeatTimeout = %s, want 45 (the presence lease margin)", got)
	}

	// Credential and assignment travel as FRP login metadata for the
	// fail-closed relay plugin.
	if got := renderedLineValue(t, rendered, "metadatas.sharebridge_credential"); got != fmt.Sprintf("%q", config.Credential) {
		t.Errorf("metadatas.sharebridge_credential = %s, want the relay_config credential", got)
	}
	if got := renderedLineValue(t, rendered, "metadatas.sharebridge_generation"); got != fmt.Sprintf("%q", fmt.Sprintf("%d", config.Generation)) {
		t.Errorf("metadatas.sharebridge_generation = %s, want %q", got, fmt.Sprintf("%d", config.Generation))
	}
	if got := renderedLineValue(t, rendered, "serverAddr"); got != fmt.Sprintf("%q", config.GatewayAddr) {
		t.Errorf("serverAddr = %s, want %q", got, config.GatewayAddr)
	}
	if got := renderedLineValue(t, rendered, "serverPort"); got != fmt.Sprintf("%d", config.GatewayPort) {
		t.Errorf("serverPort = %s, want %d", got, config.GatewayPort)
	}

	// Fail-closed checks: no verification bypass, no admin listener, and no
	// bandwidth-limit fields (the cap is deferred to phase 4b, spec §14).
	renderedLower := strings.ToLower(string(rendered))
	for _, forbidden := range []string{"skipverify", "insecure", "webserver", "bandwidth"} {
		if strings.Contains(renderedLower, forbidden) {
			t.Errorf("rendered config contains forbidden %q:\n%s", forbidden, rendered)
		}
	}

	// Renderer must reject a divergent local target and a missing CA file:
	// both would silently weaken the fixed-target and fail-closed-TLS
	// guarantees.
	divergentSettings := settings
	divergentSettings.LocalTarget = "127.0.0.1:9999"
	if _, err := Render(config, divergentSettings); err == nil {
		t.Error("Render() with a non-fixed local target succeeded, want error")
	}
	emptyCASettings := settings
	emptyCASettings.TrustedCAFile = ""
	if _, err := Render(config, emptyCASettings); err == nil {
		t.Error("Render() without a trusted CA file succeeded, want error (verification must fail closed)")
	}
}

func TestConfigWrittenAtomicallyMode0600(t *testing.T) {
	dataDirectory := t.TempDir()
	configPath := filepath.Join(dataDirectory, "frpc.toml")
	rendered, err := Render(validTestConfig(), testRenderSettings(t))
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if err := WriteConfigFile(configPath, rendered); err != nil {
		t.Fatalf("WriteConfigFile() error = %v", err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat written config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("config file mode = %v, want 0600 (the file carries the admission credential)", got)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	if string(written) != string(rendered) {
		t.Error("config file content does not match the rendered payload")
	}
	entries, err := os.ReadDir(dataDirectory)
	if err != nil {
		t.Fatalf("read data directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "frpc.toml" {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("data directory contains %v, want only frpc.toml (no temporary file may survive the atomic write)", names)
	}

	// Replacing an existing configuration must go through the same
	// temp+rename path: the old mode must not survive, and no temporary
	// file may be left behind.
	if err := os.WriteFile(configPath, []byte("# stale previous generation\n"), 0644); err != nil {
		t.Fatalf("seed stale config: %v", err)
	}
	if err := WriteConfigFile(configPath, rendered); err != nil {
		t.Fatalf("second WriteConfigFile() error = %v", err)
	}
	info, err = os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat replaced config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("replaced config file mode = %v, want 0600", got)
	}
	entries, err = os.ReadDir(dataDirectory)
	if err != nil {
		t.Fatalf("read data directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("data directory contains %d entries after replacement, want 1 (no temporary leftovers)", len(entries))
	}

	// A path whose parent is a file cannot hold the config: the atomic
	// write must fail closed instead of clobbering anything.
	blockerPath := filepath.Join(dataDirectory, "blocker")
	if err := os.WriteFile(blockerPath, []byte("not a directory"), 0644); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	if err := WriteConfigFile(filepath.Join(blockerPath, "frpc.toml"), rendered); err == nil {
		t.Error("WriteConfigFile() to a path with a file parent succeeded, want error")
	}
}

func TestParseRelayConfigStrictShape(t *testing.T) {
	config := validTestConfig()
	validMessage := map[string]any{
		"type":         "relay_config",
		"version":      RelayConfigVersion,
		"generation":   config.Generation,
		"gateway_addr": config.GatewayAddr,
		"gateway_port": config.GatewayPort,
		"proxy_name":   config.ProxyName,
		"relay_port":   config.RelayPort,
		"credential":   config.Credential,
		"expires_at":   config.ExpiresAt.Format(time.RFC3339),
	}
	rawMessage, err := json.Marshal(validMessage)
	if err != nil {
		t.Fatalf("marshal valid relay_config: %v", err)
	}

	parsed, err := ParseRelayConfig(rawMessage)
	if err != nil {
		t.Fatalf("ParseRelayConfig() error = %v", err)
	}
	if parsed != config {
		t.Errorf("ParseRelayConfig() = %+v, want %+v", parsed, config)
	}

	invalidCases := []struct {
		name     string
		mutation func(message map[string]any)
	}{
		{"unknown field", func(message map[string]any) { message["unknown_field"] = "rejected" }},
		{"wrong message type", func(message map[string]any) { message["type"] = "other" }},
		{"unknown version", func(message map[string]any) { message["version"] = RelayConfigVersion + 1 }},
		{"missing version", func(message map[string]any) { delete(message, "version") }},
		{"negative generation", func(message map[string]any) { message["generation"] = -1 }},
		{"missing gateway address", func(message map[string]any) { message["gateway_addr"] = "" }},
		{"gateway address with whitespace", func(message map[string]any) { message["gateway_addr"] = "relay example org" }},
		{"gateway address with quote", func(message map[string]any) { message["gateway_addr"] = `relay"example` }},
		{"oversize gateway address", func(message map[string]any) { message["gateway_addr"] = strings.Repeat("a", 254) }},
		{"zero gateway port", func(message map[string]any) { message["gateway_port"] = 0 }},
		{"oversize gateway port", func(message map[string]any) { message["gateway_port"] = 65536 }},
		{"missing proxy name", func(message map[string]any) { message["proxy_name"] = "" }},
		{"proxy name with space", func(message map[string]any) { message["proxy_name"] = "sb docs" }},
		{"zero relay port", func(message map[string]any) { message["relay_port"] = 0 }},
		{"oversize relay port", func(message map[string]any) { message["relay_port"] = 65536 }},
		{"empty credential", func(message map[string]any) { message["credential"] = "" }},
		{"oversize credential", func(message map[string]any) { message["credential"] = strings.Repeat("a", 4097) }},
		{"credential with whitespace", func(message map[string]any) { message["credential"] = "header payload signature" }},
		{"unparseable expiry", func(message map[string]any) { message["expires_at"] = "not-a-time" }},
		{"missing expiry", func(message map[string]any) { delete(message, "expires_at") }},
	}
	for _, testCase := range invalidCases {
		t.Run(testCase.name, func(t *testing.T) {
			mutated := map[string]any{}
			for key, value := range validMessage {
				mutated[key] = value
			}
			testCase.mutation(mutated)
			rawMutated, err := json.Marshal(mutated)
			if err != nil {
				t.Fatalf("marshal mutated relay_config: %v", err)
			}
			if _, err := ParseRelayConfig(rawMutated); err == nil {
				t.Fatalf("ParseRelayConfig() accepted invalid payload (%s)", testCase.name)
			}
		})
	}

	if _, err := ParseRelayConfig([]byte(`{"type":"relay_config"}` + `{"type":"relay_config"}`)); err == nil {
		t.Error("ParseRelayConfig() accepted trailing data after the JSON value")
	}
	if _, err := ParseRelayConfig([]byte("not json")); err == nil {
		t.Error("ParseRelayConfig() accepted non-JSON input")
	}
}

// Package tunnel supervises the pinned FRP client (frpc) on behalf of the
// agent daemon (phase 4a spec §§4.5, 7.2, 7.4): it validates the control
// plane's relay_config message, renders the hardened generated frpc
// configuration, writes it atomically with mode 0600, and owns the child
// process lifecycle (no shell, bounded restart backoff, generation-fenced
// replacement, fresh credentials before every re-login).
package tunnel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LocalTarget is the only permitted frpc local target: the agent's persistent
// HTTPS listener on loopback (spec §4.5). The renderer refuses any other
// target, so neither control messages nor operator settings can redirect the
// tunnel at a different local service.
const LocalTarget = "127.0.0.1:8443"

// RelayConfigVersion is the exact wire version of the §11.1 relay_config
// message this renderer accepts; unknown versions are rejected.
const RelayConfigVersion = 1

// Bounded identifier sizes for the validated relay_config fields. The
// admission credential is a compact JWT-sized envelope; everything else is a
// short assignment identifier.
const (
	maxGatewayAddrLength = 253
	maxProxyNameLength   = 128
	maxCredentialLength  = 4096
)

// Config is the validated assignment carried by one relay_config message
// (§11.1): generation-fenced tunnel coordinates plus the one-use admission
// credential. Callers cannot add proxies or change the local target; the
// renderer derives both from this struct and fixed settings.
type Config struct {
	Version     int
	Generation  int
	GatewayAddr string
	GatewayPort int
	ProxyName   string
	RelayPort   int
	Credential  string
	ExpiresAt   time.Time
}

// Validate rejects a relay_config value outside the bounded, versioned shape
// control is allowed to send (§7.4 step 1). Validation errors name fields and
// sizes only; the credential value is never included.
func (config Config) Validate() error {
	if config.Version != RelayConfigVersion {
		return fmt.Errorf("unsupported relay_config version %d, want %d", config.Version, RelayConfigVersion)
	}
	if config.Generation < 0 {
		return fmt.Errorf("relay_config generation %d is negative", config.Generation)
	}
	if !validGatewayAddr(config.GatewayAddr) {
		return fmt.Errorf("relay_config gateway_addr must be 1-%d characters of letters, digits, '.', ':' or '-'", maxGatewayAddrLength)
	}
	if config.GatewayPort <= 0 || config.GatewayPort > 65535 {
		return fmt.Errorf("relay_config gateway_port %d out of range", config.GatewayPort)
	}
	if !validProxyName(config.ProxyName) {
		return fmt.Errorf("relay_config proxy_name must be 1-%d characters of letters, digits, '.', '_' or '-'", maxProxyNameLength)
	}
	if config.RelayPort <= 0 || config.RelayPort > 65535 {
		return fmt.Errorf("relay_config relay_port %d out of range", config.RelayPort)
	}
	if !validCredential(config.Credential) {
		return fmt.Errorf("relay_config credential must be 1-%d characters of letters, digits, '.', '_' or '-'", maxCredentialLength)
	}
	if config.ExpiresAt.IsZero() {
		return errors.New("relay_config expires_at is missing")
	}
	return nil
}

// RenderSettings are the fixed, agent-owned inputs the renderer needs beyond
// the control assignment: the operator-provisioned relay transport CA bundle
// and the fixed local HTTPS target.
type RenderSettings struct {
	// TrustedCAFile is the PEM bundle used to verify the frps transport
	// certificate. Without it transport verification would silently degrade,
	// so the renderer refuses to render without it (spec §14).
	TrustedCAFile string
	// LocalTarget must equal the fixed loopback HTTPS listener address.
	LocalTarget string
}

// Render produces the generated frpc configuration for one validated
// assignment. The output carries the hard security requirements proven by the
// Task 8 gates (spec §§7.2, 14 and the 2026-09-04 amendment):
//
//   - verified transport TLS with an explicit trustedCaFile and serverName
//     taken from relay_config — verification fails closed, there is no
//     skip-verify escape hatch;
//   - transport.poolCount = 1;
//   - TCP multiplexing disabled, so authenticated application Pings reach
//     frps every 10 seconds (mux would suppress them);
//   - an explicit 10-second heartbeat interval with the 45-second lease
//     timeout (the pinned release sends its first Ping immediately after
//     login);
//   - exactly one TCP proxy, addressed only by the assigned name and relay
//     port, targeting the fixed loopback HTTPS listener;
//   - compression disabled and no bandwidth-limit fields (the cap is
//     deferred to phase 4b, spec §14).
//
// loginFailExit stays enabled: a rejected Login (including a replay-rejected
// burned one-use jti) makes frpc exit, which is the supervisor-visible signal
// that a fresh credential must be requested before any re-login (§15.2).
func Render(config Config, settings RenderSettings) ([]byte, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if settings.LocalTarget != LocalTarget {
		return nil, fmt.Errorf("tunnel local target must stay fixed at %s, got %q", LocalTarget, settings.LocalTarget)
	}
	if strings.TrimSpace(settings.TrustedCAFile) == "" {
		return nil, errors.New("trusted CA file is required so transport verification fails closed")
	}
	if !validFilePath(settings.TrustedCAFile) {
		return nil, errors.New("trusted CA file must be a plain path without quotes, backslashes or control characters")
	}

	localHost, localPortText, err := splitHostPort(settings.LocalTarget)
	if err != nil {
		return nil, fmt.Errorf("split fixed local target: %w", err)
	}

	rendered := fmt.Sprintf(`# Generated by the ShareBridge agent tunnel manager (phase 4a spec §7.4).
# Carries a short-lived one-use relay admission credential: the file is
# written mode 0600 and must never be logged, edited, or committed. Control
# sends a fresh relay_config whenever the contents must change.
serverAddr = %[1]q
serverPort = %[2]d
loginFailExit = true

# One-use admission credential and assignment generation as FRP login
# metadata; the fail-closed relay authorization plugin accepts Login only
# from these (spec §7.2).
metadatas.sharebridge_credential = %[3]q
metadatas.sharebridge_generation = %[4]q

# Transport hard requirements (spec §14, proven by the Task 8 pinned-release
# gates): verified TLS with an explicit CA and server name, one pooled
# connection, and TCP multiplexing disabled so authenticated application
# Pings reach frps every 10 seconds for the 45-second presence lease.
transport.tcpMux = false
transport.poolCount = 1
transport.tls.enable = true
transport.tls.trustedCaFile = %[5]q
transport.tls.serverName = %[1]q
transport.heartbeatInterval = 10
transport.heartbeatTimeout = 45

[[proxies]]
name = %[6]q
type = "tcp"
localIP = %[7]q
localPort = %[8]s
remotePort = %[9]d
transport.useCompression = false
`,
		config.GatewayAddr,
		config.GatewayPort,
		config.Credential,
		strconv.Itoa(config.Generation),
		settings.TrustedCAFile,
		config.ProxyName,
		localHost,
		localPortText,
		config.RelayPort,
	)
	return []byte(rendered), nil
}

// ParseRelayConfig decodes and validates the exact §11.1 relay_config wire
// message: versioned, bounded, strict (unknown fields and trailing data are
// rejected, matching the repo's credential-parsing convention), with the
// expires_at timestamp parsed as RFC3339.
func ParseRelayConfig(data []byte) (Config, error) {
	var message struct {
		Type        string `json:"type"`
		Version     int    `json:"version"`
		Generation  int    `json:"generation"`
		GatewayAddr string `json:"gateway_addr"`
		GatewayPort int    `json:"gateway_port"`
		ProxyName   string `json:"proxy_name"`
		RelayPort   int    `json:"relay_port"`
		Credential  string `json:"credential"`
		ExpiresAt   string `json:"expires_at"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		return Config{}, fmt.Errorf("decode relay_config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("trailing data after relay_config value")
	}
	if message.Type != "relay_config" {
		return Config{}, fmt.Errorf("unexpected relay_config message type %q", message.Type)
	}
	expiresAt, err := time.Parse(time.RFC3339, message.ExpiresAt)
	if err != nil {
		return Config{}, fmt.Errorf("parse relay_config expires_at: %w", err)
	}
	config := Config{
		Version:     message.Version,
		Generation:  message.Generation,
		GatewayAddr: message.GatewayAddr,
		GatewayPort: message.GatewayPort,
		ProxyName:   message.ProxyName,
		RelayPort:   message.RelayPort,
		Credential:  message.Credential,
		ExpiresAt:   expiresAt,
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// WriteConfigFile installs the generated configuration at configPath
// atomically: the payload is written to a temporary file in the same
// directory with mode 0600 and then renamed over the destination, so readers
// never observe partial content and no chmod-in-place race exists. The
// temporary file never survives a failed write.
func WriteConfigFile(configPath string, data []byte) error {
	directory := filepath.Dir(configPath)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create tunnel data directory: %w", err)
	}
	temporaryFile, err := os.CreateTemp(directory, ".frpc-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary tunnel config file: %w", err)
	}
	temporaryPath := temporaryFile.Name()
	defer os.Remove(temporaryPath) // no-op once the rename has succeeded

	// CreateTemp already opens 0600; the explicit chmod keeps the guarantee
	// independent of any future constructor change.
	if err := temporaryFile.Chmod(0600); err != nil {
		temporaryFile.Close()
		return fmt.Errorf("set tunnel config mode 0600: %w", err)
	}
	if _, err := temporaryFile.Write(data); err != nil {
		temporaryFile.Close()
		return fmt.Errorf("write tunnel config: %w", err)
	}
	if err := temporaryFile.Sync(); err != nil {
		temporaryFile.Close()
		return fmt.Errorf("sync tunnel config: %w", err)
	}
	if err := temporaryFile.Close(); err != nil {
		return fmt.Errorf("close tunnel config: %w", err)
	}
	if err := os.Rename(temporaryPath, configPath); err != nil {
		return fmt.Errorf("install tunnel config: %w", err)
	}
	return nil
}

// validGatewayAddr accepts a bounded hostname or IP literal: letters, digits,
// dots, colons (IPv6 literals) and hyphens only, so the value is safe to
// embed in the TOML output unescaped.
func validGatewayAddr(gatewayAddr string) bool {
	if gatewayAddr == "" || len(gatewayAddr) > maxGatewayAddrLength {
		return false
	}
	return strings.IndexFunc(gatewayAddr, func(character rune) bool {
		isAllowed := character == '.' || character == ':' || character == '-' ||
			(character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z')
		return !isAllowed
	}) < 0
}

// validProxyName accepts a bounded assignment identifier; the value must be
// safe to embed in TOML unescaped.
func validProxyName(proxyName string) bool {
	if proxyName == "" || len(proxyName) > maxProxyNameLength {
		return false
	}
	return strings.IndexFunc(proxyName, func(character rune) bool {
		isAllowed := character == '.' || character == '_' || character == '-' ||
			(character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z')
		return !isAllowed
	}) < 0
}

// validCredential accepts the bounded credential envelope charset (JWT-style
// base64url segments joined by dots) and nothing else.
func validCredential(credential string) bool {
	if credential == "" || len(credential) > maxCredentialLength {
		return false
	}
	return strings.IndexFunc(credential, func(character rune) bool {
		isAllowed := character == '.' || character == '_' || character == '-' ||
			(character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z')
		return !isAllowed
	}) < 0
}

// validFilePath rejects path values that would need escaping inside the TOML
// output: quotes, backslashes and control characters.
func validFilePath(filePath string) bool {
	if filePath == "" {
		return false
	}
	return strings.IndexFunc(filePath, func(character rune) bool {
		return character == '"' || character == '\\' || character < 0x20 || character == 0x7f
	}) < 0
}

// splitHostPort splits the fixed local target into its host and numeric port
// text for the proxy's localIP/localPort keys.
func splitHostPort(localTarget string) (string, string, error) {
	host, portText, err := net.SplitHostPort(localTarget)
	if err != nil {
		return "", "", err
	}
	if _, err := strconv.Atoi(portText); err != nil {
		return "", "", fmt.Errorf("local target port %q is not numeric", portText)
	}
	return host, portText, nil
}

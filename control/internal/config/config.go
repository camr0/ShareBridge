package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/go-acme/lego/v4/lego"
)

type Config struct {
	Port    string
	DataDir string

	// SMTP (optional - if absent, email verification is disabled and login is allowed immediately)
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string

	// Bandwidth quota — tracked server-side for future tier enforcement
	DefaultQuotaGB float64

	// Direct-mode control plane (operator-supplied; empty disables direct mode)
	CloudflareToken string // CLOUDFLARE_TOKEN (single zone, DNS-edit only)
	BaseDomain      string // CONTENT_BASE_DOMAIN
	ACMEEmail       string // ACME_EMAIL
	ACMECADir       string // ACME_CA_DIR (lego production by default)

	// Relay tunnel policy (§4.5, operator-supplied; an empty gateway host or
	// auth seed disables relay assignment and relay_config emission entirely).
	RelayGatewayHost string // RELAY_GATEWAY_HOST (public relay gateway hostname)
	RelayGatewayPort int    // RELAY_GATEWAY_PORT (public FRP transport port, default 7000)
	RelayPortMin     int    // RELAY_PORT_MIN (inclusive, default 10000)
	RelayPortMax     int    // RELAY_PORT_MAX (inclusive, default 10099)
	RelayAuthKeySeed string // RELAY_AUTH_KEY_SEED (64-char hex Ed25519 seed; the gateway holds only the public key)

	// RelayGatewayIPv4 is the public IPv4 of the relay gateway (§6). Baseline
	// enrollment points the per-namespace content wildcard
	// *.relay.<namespace>.<base-domain> at it; absent or invalid values keep
	// baseline readiness unreachable (§7.1 makes relay DNS an enrollment gate).
	RelayGatewayIPv4 string // RELAY_GATEWAY_IPV4 (public relay gateway IPv4)

	// STUN observation listener (§10.1, plan Task 16). STUNBindAddr selects
	// the UDP bind address; UDP 3478 is the only new public control listener
	// and any other port fails closed at startup (stun.ParseBindAddr). The
	// value "off" disables the listener entirely (direct mode then has no
	// observations and always falls back to relay, §10.3).
	STUNBindAddr string // STUN_BIND_ADDR (default "0.0.0.0:3478", "off" disables)

	// STUNAdvertiseAddr is the public "host:port" advertised to agents in the
	// stun_challenge `server` field (§10.2), separate from STUN_BIND_ADDR:
	// the bind may be 0.0.0.0:3478 while agents must target a publicly
	// resolvable address. Unset ⇒ the STUN_BIND_ADDR value is advertised
	// verbatim — today's behavior, and DEV-ONLY whenever that bind is a
	// loopback or unspecified (0.0.0.0) address, since agents behind NAT can
	// never reach it (fail closed: no observation ⇒ relay fallback, §10.3).
	// Set ⇒ must validate as a public host:port (see STUNAdvertise) and is
	// advertised verbatim; an invalid value fails the process closed at
	// startup. Task 25's real-NAT gate exercises a publicly resolvable
	// advertise address.
	STUNAdvertiseAddr string // STUN_ADVERTISE_ADDR (default: STUN_BIND_ADDR verbatim)

	// RelaySelectionEnabled is the Task 20 operator flag (§9.1, §20 rollout)
	// gating every live relay route selection. Default FALSE: with the flag
	// off, control runs in rollback mode — canonical resolution keeps the
	// Phase 3 lifecycle statuses (relay_only stays 410/unsupported), direct
	// candidates still receive the Phase 4a interstitial, and relay is never
	// selected (relay-dependent cases return 503). The legacy Phase 3 direct
	// 302 is never restored in either state. Set RELAY_SELECTION_ENABLED=true
	// only after the relay gateway, route distribution, and presence sync are
	// verified (the release gate blocks on it).
	RelaySelectionEnabled bool // RELAY_SELECTION_ENABLED (default false = rollback mode)
}

func Load() *Config {
	return &Config{
		Port:    getEnv("PORT", "8080"),
		DataDir: getEnv("DATA_DIR", "./pb_data"),

		SMTPHost:     getEnv("SMTP_HOST", ""),
		SMTPPort:     getEnv("SMTP_PORT", "587"),
		SMTPUser:     getEnv("SMTP_USER", ""),
		SMTPPassword: getEnv("SMTP_PASSWORD", ""),

		DefaultQuotaGB: getEnvFloat("DEFAULT_QUOTA_GB", 50.0),

		CloudflareToken: getEnv("CLOUDFLARE_TOKEN", ""),
		BaseDomain:      getEnv("CONTENT_BASE_DOMAIN", "sharebridgeusercontent.com"),
		ACMEEmail:       getEnv("ACME_EMAIL", ""),
		ACMECADir:       getEnv("ACME_CA_DIR", lego.LEDirectoryProduction),

		RelayGatewayHost: getEnv("RELAY_GATEWAY_HOST", ""),
		RelayGatewayPort: getEnvInt("RELAY_GATEWAY_PORT", 7000),
		RelayPortMin:     getEnvInt("RELAY_PORT_MIN", 10000),
		RelayPortMax:     getEnvInt("RELAY_PORT_MAX", 10099),
		RelayAuthKeySeed: getEnv("RELAY_AUTH_KEY_SEED", ""),
		RelayGatewayIPv4: getEnv("RELAY_GATEWAY_IPV4", ""),
		STUNBindAddr:     getEnv("STUN_BIND_ADDR", "0.0.0.0:3478"),

		STUNAdvertiseAddr: getEnv("STUN_ADVERTISE_ADDR", ""),

		RelaySelectionEnabled: getEnvBool("RELAY_SELECTION_ENABLED", false),
	}
}

// STUNEnabled reports whether the §10.1 STUN observation listener should run.
// The explicit "off" value disables it; everything else is treated as a bind
// address that must parse (with port 3478) at startup or the server refuses
// to start (fail closed).
func (c *Config) STUNEnabled() bool {
	return c.STUNBindAddr != "" && c.STUNBindAddr != "off"
}

// cgnatAddrPrefix is RFC 6598 CGNAT (100.64.0.0/10), which Addr.IsPrivate
// does not cover but the §10.3 public-class policy never qualifies.
var cgnatAddrPrefix = netip.MustParsePrefix("100.64.0.0/10")

// STUNAdvertise returns the address advertised to agents in the
// stun_challenge `server` field (§10.2): the "host:port" their STUN Binding
// requests must reach. When STUN_ADVERTISE_ADDR is unset the STUN_BIND_ADDR
// value is returned verbatim (today's behavior — dev-only whenever that bind
// is loopback/0.0.0.0; agents behind NAT can never reach such an address and
// direct mode fails closed to relay, §10.3). When it is set it must be a
// valid host:port whose host is either a hostname (conservative RFC 1123
// charset) or a PUBLIC-CLASS IPv4 literal — loopback, unspecified, private,
// CGNAT, link-local and multicast literals fail closed, as does any IPv6
// literal (the direct path is IPv4-only, §10.3) — and the value is advertised
// verbatim. Any invalid value returns an error so startup fails closed; the
// listener bind itself stays governed by stun.ParseBindAddr (UDP 3478 only).
func (c *Config) STUNAdvertise() (string, error) {
	if c.STUNAdvertiseAddr == "" {
		return c.STUNBindAddr, nil
	}
	v := c.STUNAdvertiseAddr
	host, portStr, err := net.SplitHostPort(v)
	if err != nil {
		return "", fmt.Errorf("stun advertise address %q: %w", v, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("stun advertise address %q: port must be an integer in 1-65535", v)
	}
	if host == "" {
		return "", fmt.Errorf("stun advertise address %q: empty host", v)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		// Literal address: must be public-class IPv4.
		if !ip.Is4() {
			return "", fmt.Errorf("stun advertise address %q: the direct path is IPv4-only", v)
		}
		if ip.IsLoopback() || ip.IsUnspecified() || ip.IsPrivate() ||
			ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsMulticast() || cgnatAddrPrefix.Contains(ip) {
			return "", fmt.Errorf("stun advertise address %q: %s is not a public address", v, host)
		}
		return v, nil
	}
	// Hostname: conservative RFC 1123 charset, non-empty labels, bounded
	// length — the address rides the §11.1 wire and must stay well-formed.
	if len(host) > 253 {
		return "", fmt.Errorf("stun advertise address %q: hostname too long", v)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return "", fmt.Errorf("stun advertise address %q: invalid hostname label %q", v, label)
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			default:
				return "", fmt.Errorf("stun advertise address %q: invalid hostname rune %q", v, r)
			}
		}
	}
	return v, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			return f
		}
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return def
}

// getEnvBool parses Go bool literals ("1"/"t"/"T"/"TRUE"/"true"/"True" and
// the false forms); any other value — including unparseable input — fails
// closed to def. Task 20's flag uses it so a typo like "ture" can never
// silently enable relay selection.
func getEnvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

// HasSMTP returns true if SMTP is configured.
func (c *Config) HasSMTP() bool {
	return c.SMTPHost != ""
}

// RelayPolicyEnabled reports whether the operator configured enough relay
// policy for control to assign tunnels and sign credentials.
func (c *Config) RelayPolicyEnabled() bool {
	return c.RelayGatewayHost != "" && c.RelayAuthKeySeed != ""
}

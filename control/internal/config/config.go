package config

import (
	"os"
	"strconv"

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
	}
}

// STUNEnabled reports whether the §10.1 STUN observation listener should run.
// The explicit "off" value disables it; everything else is treated as a bind
// address that must parse (with port 3478) at startup or the server refuses
// to start (fail closed).
func (c *Config) STUNEnabled() bool {
	return c.STUNBindAddr != "" && c.STUNBindAddr != "off"
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

// HasSMTP returns true if SMTP is configured.
func (c *Config) HasSMTP() bool {
	return c.SMTPHost != ""
}

// RelayPolicyEnabled reports whether the operator configured enough relay
// policy for control to assign tunnels and sign credentials.
func (c *Config) RelayPolicyEnabled() bool {
	return c.RelayGatewayHost != "" && c.RelayAuthKeySeed != ""
}

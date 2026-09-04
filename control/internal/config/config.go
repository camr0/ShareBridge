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
	}
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

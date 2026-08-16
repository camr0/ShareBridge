package config

import (
	"os"
	"strconv"
	"time"

	"github.com/go-acme/lego/v4/lego"
)

type Config struct {
	Port    string
	STUNURL string
	DataDir string

	// Relay configuration
	RelayJWTSecret         string
	RelayPendingWaitWindow time.Duration

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
}

func Load() *Config {
	return &Config{
		Port:    getEnv("PORT", "8080"),
		STUNURL: getEnv("STUN_URL", "stun:stun.cloudflare.com:3478"),
		DataDir: getEnv("DATA_DIR", "./pb_data"),

		RelayJWTSecret:         getEnv("RELAY_JWT_SECRET", ""),
		RelayPendingWaitWindow: getEnvDuration("RELAY_PENDING_WAIT_WINDOW", 7*time.Second),

		SMTPHost:     getEnv("SMTP_HOST", ""),
		SMTPPort:     getEnv("SMTP_PORT", "587"),
		SMTPUser:     getEnv("SMTP_USER", ""),
		SMTPPassword: getEnv("SMTP_PASSWORD", ""),

		DefaultQuotaGB: getEnvFloat("DEFAULT_QUOTA_GB", 50.0),

		CloudflareToken: getEnv("CLOUDFLARE_TOKEN", ""),
		BaseDomain:      getEnv("CONTENT_BASE_DOMAIN", "sharebridgeusercontent.com"),
		ACMEEmail:       getEnv("ACME_EMAIL", ""),
		ACMECADir:       getEnv("ACME_CA_DIR", lego.LEDirectoryProduction),
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

func getEnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	}
	return def
}

// HasSMTP returns true if SMTP is configured.
func (c *Config) HasSMTP() bool {
	return c.SMTPHost != ""
}

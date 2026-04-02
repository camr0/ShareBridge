package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port       string
	AuthToken  string // Deprecated: use API keys instead
	STUNURL    string
	DBPath     string // NEW
	AdminToken string // NEW: for admin endpoints

	// TURN configuration
	TurnHost   string // Coturn hostname (PUBLIC IP or domain, not Docker hostname)
	TurnPort   string // Coturn port (default 3478)
	TurnSecret string // HMAC shared secret
}

func Load() *Config {
	return &Config{
		Port:       getEnv("PORT", "8080"),
		AuthToken:  getEnv("AUTH_TOKEN", "dev-token"),
		STUNURL:    getEnv("STUN_URL", "stun:stun.cloudflare.com:3478"),
		DBPath:     getEnv("DATABASE_PATH", "./signaling.db"),
		AdminToken: getEnv("ADMIN_TOKEN", ""), // Empty = admin endpoints disabled
		TurnHost:   getEnv("TURN_HOST", ""),
		TurnPort:   getEnv("TURN_PORT", "3478"),
		TurnSecret: getEnv("TURN_SECRET", ""),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// HasTurn returns true if TURN is configured.
func (c *Config) HasTurn() bool {
	return c.TurnHost != "" && c.TurnSecret != ""
}

// TurnURL returns the TURN URL (e.g., "turn:yourdomain.com:3478").
func (c *Config) TurnURL() string {
	return fmt.Sprintf("turn:%s:%s", c.TurnHost, c.TurnPort)
}

package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port    string
	STUNURL string
	DBPath  string

	// TURN configuration
	TurnHost   string
	TurnPort   string
	TurnSecret string

	// SMTP (optional - if absent, email verification is disabled and login is allowed immediately)
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string
}

func Load() *Config {
	return &Config{
		Port:    getEnv("PORT", "8080"),
		STUNURL: getEnv("STUN_URL", "stun:stun.cloudflare.com:3478"),
		DBPath:  getEnv("DATABASE_PATH", "./signaling.db"),

		TurnHost:   getEnv("TURN_HOST", ""),
		TurnPort:   getEnv("TURN_PORT", "3478"),
		TurnSecret: getEnv("TURN_SECRET", ""),

		SMTPHost:     getEnv("SMTP_HOST", ""),
		SMTPPort:     getEnv("SMTP_PORT", "587"),
		SMTPUser:     getEnv("SMTP_USER", ""),
		SMTPPassword: getEnv("SMTP_PASSWORD", ""),
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

// HasSMTP returns true if SMTP is configured.
func (c *Config) HasSMTP() bool {
	return c.SMTPHost != ""
}

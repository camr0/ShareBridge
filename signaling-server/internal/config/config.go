package config

import "os"

type Config struct {
	Port       string
	AuthToken  string // Deprecated: use API keys instead
	STUNURL    string
	DBPath     string // NEW
	AdminToken string // NEW: for admin endpoints
}

func Load() *Config {
	return &Config{
		Port:       getEnv("PORT", "8080"),
		AuthToken:  getEnv("AUTH_TOKEN", "dev-token"),
		STUNURL:    getEnv("STUN_URL", "stun:stun.cloudflare.com:3478"),
		DBPath:     getEnv("DATABASE_PATH", "./signaling.db"),
		AdminToken: getEnv("ADMIN_TOKEN", ""), // Empty = admin endpoints disabled
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

package config

import "os"

type Config struct {
	Port      string
	AuthToken string
	STUNURL   string
}

func Load() *Config {
	return &Config{
		Port:      getEnv("PORT", "8080"),
		AuthToken: getEnv("AUTH_TOKEN", "dev-token"),
		STUNURL:   getEnv("STUN_URL", "stun:stun.cloudflare.com:3478"),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

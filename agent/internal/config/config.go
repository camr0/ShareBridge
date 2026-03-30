package config

import "os"

type Config struct {
	SignalingServer string
	AuthToken       string
	AllowedHost     string
	// Password and MaxDownloads are set from CLI flags, not env vars
	Password     string
	MaxDownloads int
}

func Load() *Config {
	return &Config{
		SignalingServer: getEnv("SIGNALING_SERVER", "ws://localhost:8080"),
		AuthToken:       getEnv("AUTH_TOKEN", "dev-token"),
		AllowedHost:     getEnv("ALLOWED_OPENCLOUD_HOST", ""),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

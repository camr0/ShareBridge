package config

import "os"

type Config struct {
	SignalingServer string // e.g. ws://localhost:8080
	AuthToken       string
	ShareURL        string
}

func Load() *Config {
	return &Config{
		SignalingServer: getEnv("SIGNALING_SERVER", "ws://localhost:8080"),
		AuthToken:       getEnv("AUTH_TOKEN", "dev-token"),
		ShareURL:        getEnv("SHARE_URL", ""),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

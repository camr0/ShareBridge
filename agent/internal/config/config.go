package config

import "os"

type Config struct {
	SignalingServer string
	APIKey          string
	AllowedHost     string
	// Password and MaxDownloads are set from CLI flags
	Password     string
	MaxDownloads int
}

func Load() *Config {
	return &Config{
		SignalingServer: getEnv("SIGNALING_SERVER", "ws://localhost:8080"),
		APIKey:          getEnv("OPENCLOUDSHARE_API_KEY", ""),
		AllowedHost:     getEnv("ALLOWED_OPENCLOUD_HOST", ""),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

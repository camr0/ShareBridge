package config

import (
	"testing"
	"time"
)

func TestLoad_relayDefaults(t *testing.T) {
	t.Setenv("RELAY_LISTEN_ADDR", "")
	cfg := Load()
	if cfg.RelayListenAddr != "/ip4/127.0.0.1/tcp/9001/ws" {
		t.Fatalf("default RelayListenAddr: %q", cfg.RelayListenAddr)
	}
	if cfg.JWTTTL != 5*time.Minute {
		t.Fatalf("default JWTTTL: %v", cfg.JWTTTL)
	}
}

func TestLoad_jwtSecretFromEnv(t *testing.T) {
	t.Setenv("JWT_SECRET", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	cfg := Load()
	if len(cfg.JWTSecret) < 32 {
		t.Fatalf("JWTSecret len: %d", len(cfg.JWTSecret))
	}
}

func TestLoad_jwtSecretDefaultsForDev(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	cfg := Load()
	if len(cfg.JWTSecret) < 32 {
		t.Fatalf("default JWTSecret len: %d", len(cfg.JWTSecret))
	}
}

func TestLoad_smtpAbsentByDefault(t *testing.T) {
	t.Setenv("SMTP_HOST", "")
	cfg := Load()
	if cfg.HasSMTP() {
		t.Fatal("HasSMTP() should be false with no SMTP_HOST")
	}
}

package bulkfan

import "testing"

func envFromMap(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestFromEnvDefaultsWhenUnset(t *testing.T) {
	cfg := FromEnv(envFromMap(nil))
	if cfg != Default() {
		t.Fatalf("FromEnv(empty) = %+v, want %+v", cfg, Default())
	}
	if cfg.Enabled() {
		t.Fatalf("empty env reported Enabled()=true, want false")
	}
}

func TestFromEnvParsesEveryLever(t *testing.T) {
	cfg := FromEnv(envFromMap(map[string]string{
		EnvConns:    "4",
		EnvLanes:    "8",
		EnvPaceMbps: "180.5",
	}))
	if cfg.Conns != 4 || cfg.Lanes != 8 || cfg.PaceMbps != 180.5 {
		t.Fatalf("FromEnv = %+v, want Conns=4 Lanes=8 PaceMbps=180.5", cfg)
	}
	if !cfg.FanoutEnabled() || !cfg.StripeEnabled() || !cfg.PacingEnabled() || !cfg.Enabled() {
		t.Fatalf("FromEnv(4,8,180.5) did not enable all levers: %+v", cfg)
	}
	if got, want := cfg.PaceBytesPerSecond(), 180.5*1_000_000/8; got != want {
		t.Fatalf("PaceBytesPerSecond() = %v, want %v", got, want)
	}
}

func TestFromEnvInvalidValuesFallBackToDefault(t *testing.T) {
	cases := []struct {
		name  string
		value string
		key   string
	}{
		{"conns not a number", "many", EnvConns},
		{"conns below minimum", "0", EnvConns},
		{"conns negative", "-2", EnvConns},
		{"lanes zero", "0", EnvLanes},
		{"pace garbage", "fast", EnvPaceMbps},
		{"pace negative", "-100", EnvPaceMbps},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := FromEnv(envFromMap(map[string]string{tc.key: tc.value}))
			if cfg != Default() {
				t.Fatalf("FromEnv(%s=%q) = %+v, want default %+v", tc.key, tc.value, cfg, Default())
			}
		})
	}
}

func TestFromEnvClampsToMaximum(t *testing.T) {
	cfg := FromEnv(envFromMap(map[string]string{
		EnvConns:    "64",
		EnvLanes:    "64",
		EnvPaceMbps: "1000000",
	}))
	if cfg.Conns != MaxConns {
		t.Fatalf("Conns = %d, want clamp to %d", cfg.Conns, MaxConns)
	}
	if cfg.Lanes != MaxLanes {
		t.Fatalf("Lanes = %d, want clamp to %d", cfg.Lanes, MaxLanes)
	}
	if cfg.PaceMbps != MaxPaceMbps {
		t.Fatalf("PaceMbps = %v, want clamp to %v", cfg.PaceMbps, MaxPaceMbps)
	}
}

func TestFromEnvSingleValueLeavesOthersOff(t *testing.T) {
	cfg := FromEnv(envFromMap(map[string]string{EnvLanes: "4"}))
	if cfg.Lanes != 4 {
		t.Fatalf("Lanes = %d, want 4", cfg.Lanes)
	}
	if cfg.FanoutEnabled() || cfg.PacingEnabled() {
		t.Fatalf("single lever enabled others: %+v", cfg)
	}
}

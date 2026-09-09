package config

import (
	"os"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	// Unset any env vars that might interfere
	os.Unsetenv("DEFAULT_QUOTA_GB")

	cfg := Load()

	if cfg.DefaultQuotaGB != 50.0 {
		t.Errorf("DefaultQuotaGB = %v, want 50.0", cfg.DefaultQuotaGB)
	}
}

func TestLoad_QuotaOverride(t *testing.T) {
	os.Setenv("DEFAULT_QUOTA_GB", "100")
	defer func() {
		os.Unsetenv("DEFAULT_QUOTA_GB")
	}()

	cfg := Load()

	if cfg.DefaultQuotaGB != 100.0 {
		t.Errorf("DefaultQuotaGB = %v, want 100.0", cfg.DefaultQuotaGB)
	}
}

// TestRelaySelectionEnabledDefaultsFalse pins the Task 20 operator flag
// contract (plan Task 20; spec §20 rollout): RELAY_SELECTION_ENABLED defaults
// to FALSE when unset — the safe rollback posture — parses Go bool literals,
// and fails closed (false) on any unparseable value.
func TestRelaySelectionEnabledDefaultsFalse(t *testing.T) {
	t.Helper()

	cases := []struct {
		env  string
		want bool
	}{
		{env: "", want: false}, // unset
		{env: "false", want: false},
		{env: "true", want: true},
		{env: "1", want: true},
		{env: "0", want: false},
		{env: "TRUE", want: true},
		{env: "bogus", want: false}, // unparseable fails closed
	}
	for _, tc := range cases {
		if tc.env == "" {
			os.Unsetenv("RELAY_SELECTION_ENABLED")
		} else {
			os.Setenv("RELAY_SELECTION_ENABLED", tc.env)
		}
		cfg := Load()
		if cfg.RelaySelectionEnabled != tc.want {
			t.Errorf("RELAY_SELECTION_ENABLED=%q: RelaySelectionEnabled = %v, want %v", tc.env, cfg.RelaySelectionEnabled, tc.want)
		}
	}
	os.Unsetenv("RELAY_SELECTION_ENABLED")
}

func TestLoad_NoLegacyTransportConfig(t *testing.T) {
	// The v1 relay/TURN/ICE transport config fields (STUNURL, RelayJWTSecret,
	// RelayPendingWaitWindow) were removed along with the transport itself.
	// This test compiles only if those fields no longer exist on Config.
	cfg := Load()

	if cfg.DefaultQuotaGB != 50.0 {
		t.Errorf("DefaultQuotaGB = %v, want 50.0", cfg.DefaultQuotaGB)
	}
}

// TestSTUNAdvertise pins the T18-m1 knob contract (§10.2): unset ⇒ the
// STUN_BIND_ADDR value is advertised verbatim (dev-only when loopback/
// 0.0.0.0); set ⇒ a valid public host:port, advertised verbatim; any invalid
// value — bad port, non-public or IPv6 literal, malformed hostname — fails
// closed with an error.
func TestSTUNAdvertise(t *testing.T) {
	cases := []struct {
		name      string
		bind      string
		advertise string
		want      string
		wantErr   bool
	}{
		{name: "unset advertises bind verbatim", bind: "0.0.0.0:3478", advertise: "", want: "0.0.0.0:3478"},
		{name: "unset advertises loopback bind verbatim (dev-only)", bind: "127.0.0.1:3478", advertise: "", want: "127.0.0.1:3478"},
		{name: "public IPv4 literal advertised verbatim", bind: "0.0.0.0:3478", advertise: "203.0.113.7:3478", want: "203.0.113.7:3478"},
		{name: "public IPv4 literal nonstandard port verbatim", bind: "0.0.0.0:3478", advertise: "198.51.100.4:53478", want: "198.51.100.4:53478"},
		{name: "public hostname advertised verbatim", bind: "0.0.0.0:3478", advertise: "stun.example.com:3478", want: "stun.example.com:3478"},

		// Fail-closed: invalid or non-public advertise values.
		{name: "unspecified literal rejected", bind: "0.0.0.0:3478", advertise: "0.0.0.0:3478", wantErr: true},
		{name: "loopback literal rejected", bind: "0.0.0.0:3478", advertise: "127.0.0.1:3478", wantErr: true},
		{name: "private literal rejected", bind: "0.0.0.0:3478", advertise: "10.1.2.3:3478", wantErr: true},
		{name: "link-local literal rejected", bind: "0.0.0.0:3478", advertise: "169.254.9.9:3478", wantErr: true},
		{name: "CGNAT literal rejected", bind: "0.0.0.0:3478", advertise: "100.64.0.1:3478", wantErr: true},
		{name: "multicast literal rejected", bind: "0.0.0.0:3478", advertise: "224.0.0.1:3478", wantErr: true},
		{name: "IPv6 literal rejected (direct path is IPv4-only)", bind: "0.0.0.0:3478", advertise: "[2001:db8::1]:3478", wantErr: true},
		{name: "missing port rejected", bind: "0.0.0.0:3478", advertise: "stun.example.com", wantErr: true},
		{name: "port zero rejected", bind: "0.0.0.0:3478", advertise: "stun.example.com:0", wantErr: true},
		{name: "port too large rejected", bind: "0.0.0.0:3478", advertise: "stun.example.com:65536", wantErr: true},
		{name: "non-numeric port rejected", bind: "0.0.0.0:3478", advertise: "stun.example.com:stun", wantErr: true},
		{name: "empty host rejected", bind: "0.0.0.0:3478", advertise: ":3478", wantErr: true},
		{name: "hostname with invalid rune rejected", bind: "0.0.0.0:3478", advertise: "stun ex.ample.com:3478", wantErr: true},
		{name: "hostname with empty label rejected", bind: "0.0.0.0:3478", advertise: "stun..example.com:3478", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{STUNBindAddr: tc.bind, STUNAdvertiseAddr: tc.advertise}
			got, err := cfg.STUNAdvertise()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("STUNAdvertise(%q) = %q, want error", tc.advertise, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("STUNAdvertise(%q): %v", tc.advertise, err)
			}
			if got != tc.want {
				t.Fatalf("STUNAdvertise(%q) = %q, want %q advertised verbatim", tc.advertise, got, tc.want)
			}
		})
	}
}

// TestLoad_STUNAdvertiseAddrEnv pins the env var name: STUN_ADVERTISE_ADDR is
// loaded into Config.STUNAdvertiseAddr and flows through STUNAdvertise.
func TestLoad_STUNAdvertiseAddrEnv(t *testing.T) {
	t.Setenv("STUN_ADVERTISE_ADDR", "stun.example.com:3478")
	cfg := Load()
	if cfg.STUNAdvertiseAddr != "stun.example.com:3478" {
		t.Fatalf("STUNAdvertiseAddr = %q, want the loaded env value", cfg.STUNAdvertiseAddr)
	}
	got, err := cfg.STUNAdvertise()
	if err != nil {
		t.Fatalf("STUNAdvertise: %v", err)
	}
	if got != "stun.example.com:3478" {
		t.Fatalf("STUNAdvertise = %q, want the env value verbatim", got)
	}
}

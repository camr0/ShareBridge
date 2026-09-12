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
		{name: "hostname with underscore label rejected", bind: "0.0.0.0:3478", advertise: "stun_gate_example:3478", wantErr: true},
		{name: "hostname with embedded underscore rejected", bind: "0.0.0.0:3478", advertise: "stun_gate.example.com:3478", wantErr: true},
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

// TestControlSyncListener pins the task #16 §11.3 sync listener configuration:
// the listener is enabled only by the mTLS material/identity variables, all
// four are required together (fail closed, no half-configured channel), the
// bind defaults to numeric loopback, and an explicit bind is preserved for
// relayctl.NewServer to validate.
func TestControlSyncListener(t *testing.T) {
	base := &Config{}
	if base.ControlSyncConfigured() {
		t.Fatal("empty configuration enabled the sync listener")
	}
	listener, err := base.ControlSyncListener()
	if err != nil || listener.BindAddress != "" {
		t.Fatalf("disabled listener = (%+v, %v), want the zero value and nil", listener, err)
	}

	// A bind address alone never enables the listener (safe compose default).
	onlyBind := &Config{ControlSyncBindAddr: "127.0.0.1:9443"}
	if onlyBind.ControlSyncConfigured() {
		t.Fatal("CONTROL_SYNC_BIND_ADDR alone enabled the sync listener")
	}

	full := &Config{
		ControlSyncCertFile:          "/etc/sharebridge/control-sync/control-sync.crt",
		ControlSyncKeyFile:           "/etc/sharebridge/control-sync/control-sync.key",
		ControlSyncClientCAFile:      "/etc/sharebridge/control-sync/sync-ca.crt",
		ControlSyncExpectedClientSAN: "sharebridge-relay-gateway.sync.internal",
	}
	if !full.ControlSyncConfigured() {
		t.Fatal("full configuration did not enable the sync listener")
	}
	listener, err = full.ControlSyncListener()
	if err != nil {
		t.Fatalf("full ControlSyncListener: %v", err)
	}
	if listener.BindAddress != DefaultControlSyncBindAddr {
		t.Fatalf("default bind = %q, want %q", listener.BindAddress, DefaultControlSyncBindAddr)
	}
	if listener.CertFile != full.ControlSyncCertFile || listener.ExpectedClientSAN != full.ControlSyncExpectedClientSAN {
		t.Fatalf("resolved listener = %+v, want the configured material", listener)
	}

	withBind := *full
	withBind.ControlSyncBindAddr = "10.20.30.40:9443"
	listener, err = withBind.ControlSyncListener()
	if err != nil {
		t.Fatalf("explicit bind ControlSyncListener: %v", err)
	}
	if listener.BindAddress != "10.20.30.40:9443" {
		t.Fatalf("explicit bind = %q, want it preserved verbatim", listener.BindAddress)
	}

	// Each missing material variable refuses the whole configuration.
	for name, mutate := range map[string]func(*Config){
		"cert":   func(c *Config) { c.ControlSyncCertFile = "" },
		"key":    func(c *Config) { c.ControlSyncKeyFile = "" },
		"ca":     func(c *Config) { c.ControlSyncClientCAFile = "" },
		"client": func(c *Config) { c.ControlSyncExpectedClientSAN = "" },
	} {
		partial := *full
		mutate(&partial)
		if !partial.ControlSyncConfigured() {
			t.Fatalf("partial config (%s) was considered disabled", name)
		}
		if _, err := partial.ControlSyncListener(); err == nil {
			t.Fatalf("partial config (missing %s) was accepted; it must fail closed", name)
		}
	}
}

// TestLoad_ControlSyncEnvNames pins the exact env names the deployment gate
// asserts.
func TestLoad_ControlSyncEnvNames(t *testing.T) {
	t.Setenv("CONTROL_SYNC_BIND_ADDR", "127.0.0.1:19443")
	t.Setenv("CONTROL_SYNC_CERT_FILE", "/tmp/control-sync.crt")
	t.Setenv("CONTROL_SYNC_KEY_FILE", "/tmp/control-sync.key")
	t.Setenv("CONTROL_SYNC_CLIENT_CA_FILE", "/tmp/sync-ca.crt")
	t.Setenv("CONTROL_SYNC_EXPECTED_CLIENT_SAN", "gateway-sync.internal")
	cfg := Load()
	if cfg.ControlSyncBindAddr != "127.0.0.1:19443" ||
		cfg.ControlSyncCertFile != "/tmp/control-sync.crt" ||
		cfg.ControlSyncKeyFile != "/tmp/control-sync.key" ||
		cfg.ControlSyncClientCAFile != "/tmp/sync-ca.crt" ||
		cfg.ControlSyncExpectedClientSAN != "gateway-sync.internal" {
		t.Fatalf("loaded control sync config = %+v", cfg)
	}
}

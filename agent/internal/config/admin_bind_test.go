package config

import (
	"strings"
	"testing"
)

// TestUIAddrDefaultsToLoopback pins the secure-by-default bind: a fresh
// deployment with no UI_ADDR configured must not be reachable off-host.
func TestUIAddrDefaultsToLoopback(t *testing.T) {
	t.Setenv("UI_ADDR", "")
	t.Setenv("UI_PASSWORD", "")

	mgr := newTempConfigManager(t)
	if got := mgr.Get().UIAddr; got != "127.0.0.1" {
		t.Fatalf("default UIAddr = %q, want %q (a default deployment must bind loopback only)", got, "127.0.0.1")
	}
}

// TestExplicitNonLoopbackAddrWithoutPasswordFailsValidation proves the
// fail-closed rule for an operator who explicitly opts in to a non-loopback
// bind but forgets the credential.
func TestExplicitNonLoopbackAddrWithoutPasswordFailsValidation(t *testing.T) {
	t.Setenv("UI_ADDR", "0.0.0.0")
	t.Setenv("UI_PASSWORD", "")

	mgr := newTempConfigManager(t)
	cfg := mgr.Get()
	if cfg.UIAddr != "0.0.0.0" {
		t.Fatalf("UIAddr = %q, want explicit env value 0.0.0.0", cfg.UIAddr)
	}
	err := cfg.ValidateAdminBind()
	if err == nil {
		t.Fatalf("explicit non-loopback UI_ADDR with empty UI_PASSWORD must fail validation")
	}
	for _, want := range []string{"UI_ADDR", "UI_PASSWORD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %s", err, want)
		}
	}
}

func TestValidateAdminBind(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		password string
		wantErr  bool
	}{
		{name: "default loopback v4 without password", addr: "127.0.0.1", password: "", wantErr: false},
		{name: "loopback v6 without password", addr: "::1", password: "", wantErr: false},
		{name: "loopback hostname without password", addr: "localhost", password: "", wantErr: false},
		{name: "loopback with port without password", addr: "127.0.0.1:7878", password: "", wantErr: false},
		{name: "bracketed loopback v6 without password", addr: "[::1]", password: "", wantErr: false},
		{name: "unset addr normalizes to loopback", addr: "", password: "", wantErr: false},
		{name: "case-insensitive loopback hostname", addr: "LocalHost", password: "", wantErr: false},
		{name: "wildcard v4 without password", addr: "0.0.0.0", password: "", wantErr: true},
		{name: "wildcard v6 without password", addr: "::", password: "", wantErr: true},
		{name: "specific non-loopback ip without password", addr: "192.168.1.10", password: "", wantErr: true},
		{name: "routable hostname without password", addr: "sharebridge.example.com", password: "", wantErr: true},
		{name: "non-loopback ip with port without password", addr: "192.168.1.10:7878", password: "", wantErr: true},
		{name: "wildcard v4 with password", addr: "0.0.0.0", password: "s3cret", wantErr: false},
		{name: "specific non-loopback ip with password", addr: "192.168.1.10", password: "s3cret", wantErr: false},
		{name: "loopback with password", addr: "127.0.0.1", password: "s3cret", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAdminBind(tc.addr, tc.password)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateAdminBind(%q, %q) = nil, want error", tc.addr, tc.password)
				}
				for _, want := range []string{"UI_ADDR", "UI_PASSWORD"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q must mention %s", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateAdminBind(%q, %q) = %v, want nil", tc.addr, tc.password, err)
			}
		})
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.2", true},
		{"::1", true},
		{"[::1]", true},
		{"localhost", true},
		{"LOCALHOST", true},
		{"", true}, // normalized to the loopback default
		{"0.0.0.0", false},
		{"::", false},
		{"192.168.0.1", false},
		{"10.0.0.5", false},
		{"example.com", false},
	}
	for _, tc := range cases {
		if got := IsLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("IsLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

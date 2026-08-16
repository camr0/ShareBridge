package daemon

import (
	"testing"

	"sharebridge/agent/internal/direct"
)

const (
	testDirectNS   = "v7q4km2x9pz6dn3w"
	testDirectBase = "sharebridgeusercontent.com"
)

func testOriginFor(label string) string {
	return label + "." + testDirectNS + "." + testDirectBase
}

func TestRegistrationGatedOnReadiness(t *testing.T) {
	d := &Daemon{direct: &directState{ready: false}}
	if d.canRegisterDirect() {
		t.Fatalf("must gate before readiness")
	}
	d.direct.ready = true
	if !d.canRegisterDirect() {
		t.Fatalf("must allow after readiness")
	}
}

func TestBinderBoundOnOrigin(t *testing.T) {
	d := &Daemon{direct: &directState{
		binder: direct.NewBinder(testDirectNS, testDirectBase),
		origin: map[string]string{},
	}}
	const code = "SHARE123"
	origin := testOriginFor("sbabc123")
	d.bindOrigin(code, origin)

	bd, err := d.direct.binder.AdmitSNI(origin)
	if err != nil {
		t.Fatalf("Allow was not recorded: %v", err)
	}
	if bd.ShareCode != code {
		t.Fatalf("binding share code = %q, want %q", bd.ShareCode, code)
	}
	if got := d.direct.origin[code]; got != origin {
		t.Fatalf("origin[%q] = %q, want %q", code, got, origin)
	}
}

func TestBinderRevokedOnDelete(t *testing.T) {
	d := &Daemon{direct: &directState{
		binder: direct.NewBinder(testDirectNS, testDirectBase),
		origin: map[string]string{},
	}}
	const code = "SHARE123"
	origin := testOriginFor("sbabc123")
	d.bindOrigin(code, origin)

	d.revokeOrigin(code)

	if _, err := d.direct.binder.AdmitSNI(origin); err == nil {
		t.Fatalf("Revoke was not recorded: origin still admitted")
	}
	if _, ok := d.direct.origin[code]; ok {
		t.Fatalf("origin[%q] should be cleared after revoke", code)
	}
}

package directctl

// ResolveForRedirect tests (§8): the single tombstone-aware canonical-route
// status owner. The LEGACY Phase 3 Redirect flow that once shared this file
// was deleted with remediation R4 (item 1): canonical GET navigation
// dispatches through ResolveForRedirect + SelectRoute (routes.go) and never a
// direct 302; the direct-URL form is pinned by TestDirectURLOmitsDefaultPort
// in prepare_test.go plus the prepare/e2e suites.

import (
	"net/http"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// seedResolveSession seeds a session with code and the given mutation applied on
// top of a default active, direct, gallery (immich) session. The mutation may
// flip is_active/share_type/relay_only/is_password_protected/expires_at or set
// inactive_reason. This is the ResolveForRedirect test harness, so it does NOT
// install an epoch or a connected hub agent (status logic must not depend on
// live transport readiness).
func seedResolveSession(t *testing.T, app core.App, code string, mutate func(*core.Record)) {
	t.Helper()
	apiKeyID := mustAPIKey(t, app, "key-"+code).Id

	sessions, _ := app.FindCollectionByNameOrId("sessions")
	sess := core.NewRecord(sessions)
	sess.Set("code", code)
	sess.Set("api_key_id", apiKeyID)
	sess.Set("agent_id", "agent-1")
	sess.Set("share_type", "immich")
	sess.Set("is_active", true)
	if mutate != nil {
		mutate(sess)
	}
	if err := app.Save(sess); err != nil {
		t.Fatalf("save session %s: %v", code, err)
	}
}

// TestResolveForRedirect is the canonical-route status matrix (§8): active
// gallery → 302; relay-only/WebDAV/protected/expired → 410; revoked/unknown →
// 404; inactive rows with a missing/unrecognized inactive_reason fail safe as
// 404.
func TestResolveForRedirect(t *testing.T) {
	app, ctrl := newTestController(t)

	cases := []struct {
		name   string
		code   string
		seed   bool
		mutate func(*core.Record)
		status int
	}{
		{name: "active gallery", code: "active", seed: true, status: http.StatusFound},
		{name: "relay-only active", code: "relay", seed: true, mutate: func(r *core.Record) { r.Set("relay_only", true) }, status: http.StatusGone},
		{name: "webdav active", code: "webdav", seed: true, mutate: func(r *core.Record) { r.Set("share_type", "opencloud") }, status: http.StatusGone},
		{name: "protected active", code: "prot", seed: true, mutate: func(r *core.Record) { r.Set("is_password_protected", true) }, status: http.StatusGone},
		{name: "expired active", code: "expactive", seed: true, mutate: func(r *core.Record) { r.Set("expires_at", time.Now().Add(-time.Hour)) }, status: http.StatusGone},
		{name: "revoked tombstone", code: "revoked", seed: true, mutate: func(r *core.Record) { r.Set("is_active", false); r.Set("inactive_reason", "revoked") }, status: http.StatusNotFound},
		{name: "expired tombstone", code: "exptomb", seed: true, mutate: func(r *core.Record) { r.Set("is_active", false); r.Set("inactive_reason", "expired") }, status: http.StatusGone},
		{name: "unsupported tombstone", code: "unsup", seed: true, mutate: func(r *core.Record) { r.Set("is_active", false); r.Set("inactive_reason", "unsupported") }, status: http.StatusGone},
		{name: "missing reason", code: "missing", seed: true, mutate: func(r *core.Record) { r.Set("is_active", false) }, status: http.StatusNotFound},
		{name: "invalid reason", code: "invalid", seed: true, mutate: func(r *core.Record) { r.Set("is_active", false); r.Set("inactive_reason", "bogus") }, status: http.StatusNotFound},
		{name: "unknown code", code: "unknown", status: http.StatusNotFound},
	}

	for _, tc := range cases {
		if tc.seed {
			seedResolveSession(t, app, tc.code, tc.mutate)
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, status := ctrl.ResolveForRedirect(tc.code)
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
			if tc.status == http.StatusFound && rec == nil {
				t.Fatalf("active gallery must return its session record")
			}
		})
	}
}

// TestResolveForRedirectRelayOnlyLifecycleFlagGated pins the Task 20
// lifecycle ruling: with RelaySelectionEnabled the active public unprotected
// Immich relay_only share is a servable type (Found — SelectRoute then owns
// the relay-or-503 choice, §9.1); without the flag it stays 410 (Phase 3
// classification). Password-protected stays 410 in both states (deferred
// share type, Task 27), and selection-time gating never changes Task 6's
// share_registered message shape.
func TestResolveForRedirectRelayOnlyLifecycleFlagGated(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		mutate  func(*core.Record)
		want    int
	}{
		{name: "flag off relayOnly stays 410", enabled: false, mutate: func(r *core.Record) { r.Set("relay_only", true) }, want: http.StatusGone},
		{name: "flag on public relayOnly servable", enabled: true, mutate: func(r *core.Record) { r.Set("relay_only", true) }, want: http.StatusFound},
		{name: "flag on protected relayOnly still 410", enabled: true, mutate: func(r *core.Record) { r.Set("relay_only", true); r.Set("is_password_protected", true) }, want: http.StatusGone},
		{name: "flag on non-immich relayOnly still 410", enabled: true, mutate: func(r *core.Record) { r.Set("relay_only", true); r.Set("share_type", "opencloud") }, want: http.StatusGone},
		{name: "flag on expired relayOnly 410", enabled: true, mutate: func(r *core.Record) { r.Set("relay_only", true); r.Set("expires_at", time.Now().Add(-time.Hour)) }, want: http.StatusGone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, base := newTestController(t)
			base.cfg.RelaySelectionEnabled = tc.enabled
			code := "flagrelay"
			seedResolveSession(t, app, code, tc.mutate)
			_, got := base.ResolveForRedirect(code)
			if got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

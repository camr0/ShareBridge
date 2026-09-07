package directctl

// Task 20 route-selection matrix tests (plan Task 20; spec §§6.1, 9.1–9.4,
// 19). The matrix drives the exact production dispatch pair —
// ResolveForRedirect (lifecycle owner) then SelectRoute (§9.3 canonical
// resolution for lifecycle-active sessions) — over lifecycle × relayOnly ×
// direct eligibility (STUN missing/stale/mismatch/not-public/no-endpoint) ×
// relay presence lease × WS state × RelaySelectionEnabled.
//
// No test asserts or measures performance: §9.1 forbids measure-and-prefer,
// and the spy tests below prove selection never invokes the only measurement
// primitive (the reachability probe), never signals the agent, and never
// mutates direct state.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/control/internal/relayctl"
	"sharebridge/control/internal/stun"
)

const (
	routeDirectIP   = "203.0.113.7"
	routeGeneration = 7
)

// routePortSeq hands out unique relay ports: the agents.relay_port column is
// uniquely indexed (nonzero), and some tests seed several agents into one
// app.
var routePortSeq atomic.Uint64

func nextRelayPort() int { return int(10000 + routePortSeq.Add(1)) }

// routeOriginFor derives the per-session persisted direct origin from the
// share code (the sessions.origin unique index forbids sharing one origin
// across seeds). All test codes are valid lowercase hostname labels.
func routeOriginFor(code string) string { return code + ".sbdeadbeef.example.com" }

// routeRelayOriginFor is the §6 relay derivation of routeOriginFor.
func routeRelayOriginFor(code string) string { return code + ".relay.sbdeadbeef.example.com" }

// stubRevisionSource is a fixed RouteRevisionSource shared by the controller
// and the presence view so the Available route-revision join agrees in tests.
type stubRevisionSource struct {
	mu  sync.Mutex
	rev uint64
}

func (s *stubRevisionSource) CurrentRevision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rev
}

func (s *stubRevisionSource) bump(to uint64) {
	s.mu.Lock()
	s.rev = to
	s.mu.Unlock()
}

// selectionSpy records every side effect selection must never produce
// (§9.2: relay selection emits no open signal, runs no probe, and mutates no
// direct state).
type selectionSpy struct {
	mu          sync.Mutex
	emitOpens   int
	probes      int
	agentSends  int
	stunIssues  int
	ddnsUpdates int
	relayDNS    int
}

func (s *selectionSpy) counts() (int, int, int, int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emitOpens, s.probes, s.agentSends, s.stunIssues, s.ddnsUpdates, s.relayDNS
}

// newSelectionController wires a controller with constructor-time presence
// injection (the Task 20 carry-forward-c posture: the view and the route
// revision source ride on directctl.Config, not the unsynchronized setter),
// a shared fake revision source, and full side-effect spies. The agent's
// relay assignment in tests is always (port 12345, generation 7), matching
// grantPresence.
func newSelectionController(t *testing.T, enabled bool) (core.App, *Controller, *stubRevisionSource, *relayctl.PresenceView, *selectionSpy) {
	t.Helper()
	app, ctrl := newTestController(t)
	revision := &stubRevisionSource{rev: 42}
	view, err := relayctl.NewPresenceView(relayctl.PresenceViewConfig{
		Routes: revision, // nil App: selection tests assert availability, not diagnostics
	})
	if err != nil {
		t.Fatalf("new presence view: %v", err)
	}
	spy := &selectionSpy{}
	ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
		spy.mu.Lock()
		spy.emitOpens++
		spy.mu.Unlock()
		return OpenAck{}, errors.New("selection must never emit open signals")
	}
	ctrl.probeFn = func(ctx context.Context, origin, code, apiKeyID string, ack OpenAck) error {
		spy.mu.Lock()
		spy.probes++
		spy.mu.Unlock()
		return errors.New("selection must never probe")
	}
	ctrl.sendToAgentFn = func(ctx context.Context, apiKeyID string, msg any) error {
		spy.mu.Lock()
		spy.agentSends++
		spy.mu.Unlock()
		return errors.New("selection must never message the agent")
	}
	ctrl.stunIssueFn = func(agentID string, epoch stun.Epoch) (stun.Challenge, error) {
		spy.mu.Lock()
		spy.stunIssues++
		spy.mu.Unlock()
		return stun.Challenge{}, errors.New("selection must never issue STUN challenges")
	}
	ctrl.ddnsFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		spy.mu.Lock()
		spy.ddnsUpdates++
		spy.mu.Unlock()
		return "", errors.New("selection must never touch DDNS")
	}
	ctrl.relayDNSFn = func(ctx context.Context, name, ip string, ttl int) (string, error) {
		spy.mu.Lock()
		spy.relayDNS++
		spy.mu.Unlock()
		return "", errors.New("selection must never touch relay DNS")
	}
	ctrl.cfg.RelaySelectionEnabled = enabled
	// Same-package test wiring of the constructor-injected fields: the
	// production path (Config.RelayPresence/Config.Routes → NewController) is
	// exercised by TestBothCanonicalRoutesPreserve404410Interstitial302And503
	// in cmd/server, which builds the controller entirely through Config.
	ctrl.relayPresence = view
	ctrl.routes = revision
	return app, ctrl, revision, view, spy
}

// routeSession seeds one active supported public Immich session (origin
// a1b2c3d4e5f6.sbdeadbeef.example.com) owned by a fresh API key, with test
// mutations applied. Returns the apiKeyID.
func routeSession(t *testing.T, app core.App, code string, mutate func(*core.Record)) string {
	t.Helper()
	apiKeyID := mustAPIKey(t, app, "key-"+code).Id
	sessions, _ := app.FindCollectionByNameOrId("sessions")
	sess := core.NewRecord(sessions)
	sess.Set("code", code)
	sess.Set("api_key_id", apiKeyID)
	sess.Set("agent_id", "agent-1")
	sess.Set("share_type", "immich")
	sess.Set("origin", routeOriginFor(code))
	sess.Set("is_active", true)
	sess.Set("expires_at", time.Now().Add(time.Hour))
	if mutate != nil {
		mutate(sess)
	}
	if err := app.Save(sess); err != nil {
		t.Fatalf("save session %s: %v", code, err)
	}
	return apiKeyID
}

// grantPresence applies a gateway presence snapshot granting (port, gen 7)
// for agentID with a 45 s lease (inside the 60 s control bound) under the
// real clock the view uses in these tests.
func grantPresence(t *testing.T, view *relayctl.PresenceView, agentID string, port int) {
	t.Helper()
	env := relayctl.PresenceEnvelope{
		Version: relayctl.ProtocolVersion,
		Events: []relayctl.PresenceEvent{{
			GatewayBootID:  "boot-selection",
			Revision:       1,
			AgentRecordID:  agentID,
			RelayPort:      port,
			Generation:     routeGeneration,
			State:          relayctl.PresenceStateOnline,
			LeaseExpiresAt: time.Now().Add(45 * time.Second).Format(time.RFC3339),
		}},
	}
	if err := view.ApplyPresenceSnapshot(env); err != nil {
		t.Fatalf("apply presence snapshot: %v", err)
	}
}

// agentRecordID returns the agents row id for apiKeyID.
func agentRecordID(t *testing.T, app core.App, apiKeyID string) string {
	t.Helper()
	recs, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": apiKeyID})
	if err != nil || len(recs) == 0 {
		t.Fatalf("agent row missing for %s: %v", apiKeyID, err)
	}
	return recs[0].Id
}

// seedAgentFacts creates the agent row for apiKeyID with a DDNS-verified
// endpoint (endpointIP, possibly empty) and a fresh unique relay assignment
// (generation fixed). Returns the assigned relay port for grantPresence.
func seedAgentFacts(t *testing.T, app core.App, apiKeyID, endpointIP string) int {
	t.Helper()
	rec, _, err := LoadOrCreateAgent(app, apiKeyID)
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	port := nextRelayPort()
	if endpointIP != "" {
		rec.Set("endpoint_ip", endpointIP)
	}
	rec.Set("endpoint_port", 8443)
	rec.Set("relay_port", port)
	rec.Set("relay_generation", routeGeneration)
	if err := app.Save(rec); err != nil {
		t.Fatalf("save agent facts: %v", err)
	}
	return port
}

// connectWS seeds the WS-state dimension: a ready current epoch + a hub
// registration (the WS half of the direct readiness term).
func connectWS(t *testing.T, ctrl *Controller, apiKeyID string) {
	t.Helper()
	ctrl.epochMu.Lock()
	ctrl.epochs[apiKeyID] = &epochState{agentID: "agent-1", namespace: "sbdeadbeef", ready: true}
	ctrl.epochMu.Unlock()
	ctrl.hub.RegisterAgent(apiKeyID, nil)
}

// resolveAndSelect drives the exact production dispatch: ResolveForRedirect
// (lifecycle owner) then — only on the active classification — SelectRoute.
// Returns the lifecycle status plus the selection response.
func resolveAndSelect(t *testing.T, ctrl *Controller, code string) (int, *httptest.ResponseRecorder) {
	t.Helper()
	rec, status := ctrl.ResolveForRedirect(code)
	resp := httptest.NewRecorder()
	if status != http.StatusFound {
		return status, resp
	}
	if err := ctrl.SelectRoute(resp, httptest.NewRequest("GET", "/s/"+code, nil), rec, code); err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	return status, resp
}

// assertSelection asserts one matrix row: the resolved status, the response
// status, the Location expectation ("" = must be empty), and no-store on
// every selection response (interstitial and redirect alike are never
// cacheable — §9.3). Lifecycle-only rows (wantLife != 302) must produce NO
// response at all: SelectRoute is never invoked for them.
func assertSelection(t *testing.T, ctrl *Controller, code string, wantLife, wantStatus int, wantLoc string) *httptest.ResponseRecorder {
	t.Helper()
	life, resp := resolveAndSelect(t, ctrl, code)
	if life != wantLife {
		t.Fatalf("lifecycle status = %d, want %d", life, wantLife)
	}
	if wantLife != http.StatusFound {
		// Lifecycle-only row: SelectRoute must not have been invoked. The
		// recorder is fresh, so no response assertions apply here.
		return resp
	}
	if resp.Code != wantStatus {
		t.Fatalf("selection status = %d, want %d (body %q)", resp.Code, wantStatus, resp.Body.String())
	}
	loc := resp.Header().Get("Location")
	if wantLoc == "" {
		if loc != "" {
			t.Fatalf("Location = %q, want empty", loc)
		}
	} else {
		if !strings.Contains(loc, wantLoc) {
			t.Fatalf("Location = %q, want it to contain %q", loc, wantLoc)
		}
		if !strings.HasPrefix(loc, "https://") {
			t.Fatalf("Location = %q, must be https", loc)
		}
		if resp.Code != http.StatusFound {
			t.Fatalf("Location set on non-302 status %d", resp.Code)
		}
	}
	if cc := resp.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	return resp
}

// seedMatrixCase builds the full per-case world: session, agent facts,
// presence lease, WS state, and STUN observation. Direct eligibility inputs
// (stun/endpoint/ws) only matter for non-relayOnly sessions; relayOnly never
// touches them (§9.2), so those cases leave them unset.
func seedMatrixCase(t *testing.T, app core.App, ctrl *Controller, view *relayctl.PresenceView, code string, relayOnly bool, stunState string, endpoint, lease, ws bool) {
	t.Helper()
	var apiKeyID string
	var port int
	if relayOnly {
		apiKeyID = routeSession(t, app, code, func(r *core.Record) { r.Set("relay_only", true) })
	} else {
		apiKeyID = routeSession(t, app, code, nil)
	}
	if endpoint || lease || ws {
		port = seedAgentFacts(t, app, apiKeyID, map[bool]string{true: routeDirectIP, false: ""}[endpoint])
	}
	if lease {
		grantPresence(t, view, agentRecordID(t, app, apiKeyID), port)
	}
	if ws {
		connectWS(t, ctrl, apiKeyID)
	}
	switch stunState {
	case "eligible":
		installObservation(t, ctrl, apiKeyID, netip.MustParseAddr(routeDirectIP), true)
	case "mismatch":
		installObservation(t, ctrl, apiKeyID, netip.MustParseAddr("198.51.100.9"), true)
	case "notpublic":
		installObservation(t, ctrl, apiKeyID, netip.MustParseAddr("10.0.0.5"), false)
	case "stale":
		installObservation(t, ctrl, apiKeyID, netip.MustParseAddr(routeDirectIP), true)
		// Age the controller's clock past the 5-minute observation TTL: the
		// observation is now stale for every later freshness check.
		agedNow := ctrl.nowFn()
		ctrl.nowFn = func() time.Time { return agedNow.Add(6 * time.Minute) }
	}
}

// TestRouteSelectionMatrix is the plan-mandated table-driven matrix over
// lifecycle × relayOnly × direct eligibility (STUN missing/stale/mismatch/
// not-public/no-endpoint) × relay lease × WS state × RelaySelectionEnabled.
func TestRouteSelectionMatrix(t *testing.T) {
	cases := []struct {
		name       string
		enabled    bool
		relayOnly  bool
		lifecycle  string // "", "revoked", "expired"
		stun       string // "eligible", "missing", "stale", "mismatch", "notpublic"
		endpoint   bool
		lease      bool
		ws         bool
		wantLife   int
		wantStatus int
		wantLoc    string
	}{
		// ---- RelaySelectionEnabled = true: the §9.1 algorithm ----
		{name: "on relayOnly+lease 302 relay", enabled: true, relayOnly: true, lease: true, wantLife: http.StatusFound, wantStatus: http.StatusFound, wantLoc: routeRelayOriginFor("m00")},
		{name: "on relayOnly no lease 503", enabled: true, relayOnly: true, wantLife: http.StatusFound, wantStatus: http.StatusServiceUnavailable},
		{name: "on relayOnly+lease WS down 302 relay (WS irrelevant to relay)", enabled: true, relayOnly: true, lease: true, wantLife: http.StatusFound, wantStatus: http.StatusFound, wantLoc: routeRelayOriginFor("m02")},
		{name: "on eligible+lease interstitial never relay", enabled: true, stun: "eligible", endpoint: true, lease: true, ws: true, wantLife: http.StatusFound, wantStatus: http.StatusOK},
		{name: "on eligible no lease interstitial", enabled: true, stun: "eligible", endpoint: true, ws: true, wantLife: http.StatusFound, wantStatus: http.StatusOK},
		{name: "on STUN missing+lease interstitial (only freshness missing)", enabled: true, endpoint: true, lease: true, ws: true, wantLife: http.StatusFound, wantStatus: http.StatusOK},
		{name: "on STUN stale+lease interstitial (only freshness stale)", enabled: true, stun: "stale", endpoint: true, lease: true, ws: true, wantLife: http.StatusFound, wantStatus: http.StatusOK},
		{name: "on STUN mismatch+lease 302 relay", enabled: true, stun: "mismatch", endpoint: true, lease: true, ws: true, wantLife: http.StatusFound, wantStatus: http.StatusFound, wantLoc: routeRelayOriginFor("m07")},
		{name: "on STUN mismatch no lease 503", enabled: true, stun: "mismatch", endpoint: true, ws: true, wantLife: http.StatusFound, wantStatus: http.StatusServiceUnavailable},
		{name: "on STUN not-public+lease 302 relay", enabled: true, stun: "notpublic", endpoint: true, lease: true, ws: true, wantLife: http.StatusFound, wantStatus: http.StatusFound, wantLoc: routeRelayOriginFor("m09")},
		{name: "on no endpoint+no observation interstitial (preparable unknown: Task 21 may still map)", enabled: true, lease: true, ws: true, wantLife: http.StatusFound, wantStatus: http.StatusOK},
		{name: "on WS down+lease 302 relay (direct unpreparable, tunnel alive)", enabled: true, endpoint: true, lease: true, wantLife: http.StatusFound, wantStatus: http.StatusFound, wantLoc: routeRelayOriginFor("m11")},
		{name: "on WS down no lease 503 offline", enabled: true, endpoint: true, wantLife: http.StatusFound, wantStatus: http.StatusServiceUnavailable},

		// ---- RelaySelectionEnabled = false: rollback mode ----
		{name: "off eligible+lease interstitial NEVER relay", enabled: false, stun: "eligible", endpoint: true, lease: true, ws: true, wantLife: http.StatusFound, wantStatus: http.StatusOK},
		{name: "off STUN missing+lease interstitial NEVER relay", enabled: false, endpoint: true, lease: true, ws: true, wantLife: http.StatusFound, wantStatus: http.StatusOK},
		{name: "off STUN mismatch+lease 503 NEVER relay", enabled: false, stun: "mismatch", endpoint: true, lease: true, ws: true, wantLife: http.StatusFound, wantStatus: http.StatusServiceUnavailable},
		{name: "off WS down+lease 503 NEVER relay", enabled: false, endpoint: true, lease: true, wantLife: http.StatusFound, wantStatus: http.StatusServiceUnavailable},
		{name: "off relayOnly+lease lifecycle 410 selection never runs", enabled: false, relayOnly: true, lease: true, wantLife: http.StatusGone},

		// ---- Lifecycle ownership is flag-invariant ----
		{name: "on revoked 404", enabled: true, lifecycle: "revoked", wantLife: http.StatusNotFound},
		{name: "on expired 410", enabled: true, lifecycle: "expired", wantLife: http.StatusGone},
		{name: "off revoked 404", enabled: false, lifecycle: "revoked", wantLife: http.StatusNotFound},
		{name: "off expired 410", enabled: false, lifecycle: "expired", wantLife: http.StatusGone},
		{name: "on protected 410 even with flag", enabled: true, lifecycle: "protected", endpoint: true, lease: true, ws: true, wantLife: http.StatusGone},
		{name: "off protected 410", enabled: false, lifecycle: "protected", wantLife: http.StatusGone},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, ctrl, _, view, _ := newSelectionController(t, tc.enabled)
			code := fmt.Sprintf("m%02d", i)

			switch tc.lifecycle {
			case "":
				seedMatrixCase(t, app, ctrl, view, code, tc.relayOnly, tc.stun, tc.endpoint, tc.lease, tc.ws)
			case "revoked":
				routeSession(t, app, code, func(r *core.Record) {
					r.Set("is_active", false)
					r.Set("inactive_reason", "revoked")
				})
			case "expired":
				routeSession(t, app, code, func(r *core.Record) {
					r.Set("expires_at", time.Now().Add(-time.Hour))
				})
			case "protected":
				routeSession(t, app, code, func(r *core.Record) { r.Set("is_password_protected", true) })
			}

			assertSelection(t, ctrl, code, tc.wantLife, tc.wantStatus, tc.wantLoc)
		})
	}
}

// TestRelaySelectionNeverSignalsProbesOrMutatesDirectState is the §9.2
// integration invariant, spy-proven like Task 19: a relay-selected navigation
// (relayOnly, and the hard direct-ineligible fallback) emits no open_signal,
// runs no probe, sends no agent message, issues no STUN challenge, touches no
// DNS, and mutates no direct state (no diagnostics write, no verified tuple,
// no endpoint/port change). The probe is the ONLY measurement primitive in
// the system and it is never invoked; the selection decision carries no
// performance scores or timing measurements at all (§9.1).
func TestRelaySelectionNeverSignalsProbesOrMutatesDirectState(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		seed func(t *testing.T, app core.App, ctrl *Controller, view *relayctl.PresenceView, code string) string
	}{
		{name: "relayOnly", code: "spyrelay", seed: func(t *testing.T, app core.App, ctrl *Controller, view *relayctl.PresenceView, code string) string {
			apiKeyID := routeSession(t, app, code, func(r *core.Record) { r.Set("relay_only", true) })
			port := seedAgentFacts(t, app, apiKeyID, routeDirectIP)
			grantPresence(t, view, agentRecordID(t, app, apiKeyID), port)
			return apiKeyID
		}},
		{name: "direct-ineligible mismatch", code: "spymismatch", seed: func(t *testing.T, app core.App, ctrl *Controller, view *relayctl.PresenceView, code string) string {
			apiKeyID := routeSession(t, app, code, nil)
			port := seedAgentFacts(t, app, apiKeyID, routeDirectIP)
			grantPresence(t, view, agentRecordID(t, app, apiKeyID), port)
			connectWS(t, ctrl, apiKeyID)
			installObservation(t, ctrl, apiKeyID, netip.MustParseAddr("198.51.100.9"), true)
			return apiKeyID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, ctrl, _, view, spy := newSelectionController(t, true)
			apiKeyID := tc.seed(t, app, ctrl, view, tc.code)
			agentsBefore := snapshotAgentRow(t, app, tc.code)

			resp := assertSelection(t, ctrl, tc.code, http.StatusFound, http.StatusFound, routeRelayOriginFor(tc.code))

			emitOpens, probes, agentSends, stunIssues, ddns, relayDNS := spy.counts()
			if emitOpens != 0 || probes != 0 || agentSends != 0 || stunIssues != 0 || ddns != 0 || relayDNS != 0 {
				t.Fatalf("selection had side effects: emitOpens=%d probes=%d agentSends=%d stunIssues=%d ddns=%d relayDNS=%d",
					emitOpens, probes, agentSends, stunIssues, ddns, relayDNS)
			}
			_ = apiKeyID
			// No direct-state mutation: the agents row is byte-identical
			// (endpoint fields, diagnostics fields untouched) and the probe
			// verified-tuple cache is still empty.
			agentsAfter := snapshotAgentRow(t, app, tc.code)
			if agentsBefore != agentsAfter {
				t.Fatalf("agent row mutated by selection:\nbefore %q\nafter  %q", agentsBefore, agentsAfter)
			}
			ctrl.verifiedMu.Lock()
			n := len(ctrl.verified)
			ctrl.verifiedMu.Unlock()
			if n != 0 {
				t.Fatalf("verified-tuple cache grew to %d", n)
			}
			if resp.Header().Get("Location") != "https://"+routeRelayOriginFor(tc.code)+"/s/"+tc.code {
				t.Fatalf("relay Location = %q", resp.Header().Get("Location"))
			}
		})
	}
}

// snapshotAgentRow renders every §12/endpoint field of the agent row for the
// session's API key as a comparable string.
func snapshotAgentRow(t *testing.T, app core.App, code string) string {
	t.Helper()
	recs, err := app.FindRecordsByFilter("sessions", "code = {:c}", "", 1, 0, map[string]any{"c": code})
	if err != nil || len(recs) == 0 {
		t.Fatalf("session %s missing: %v", code, err)
	}
	agentRecs, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": recs[0].GetString("api_key_id")})
	if err != nil || len(agentRecs) == 0 {
		t.Fatalf("agent row missing: %v", err)
	}
	a := agentRecs[0]
	return strings.Join([]string{
		a.GetString("endpoint_ip"), fmt.Sprint(a.GetInt("endpoint_port")),
		a.GetString("direct_status"), a.GetString("direct_status_reason"),
		a.GetString("stun_observed_ip"), a.GetDateTime("stun_observed_at").String(),
		a.GetString("relay_port"), a.GetString("relay_generation"),
	}, "|")
}

// TestRelayOnlyPrepareCannotTouchDirect proves the §6.1 guarantee at the
// selection layer: a relayOnly navigation never selects, prepares, probes,
// or navigates to the direct origin — no direct DNS work, no STUN challenge,
// no open-signal machinery, no epoch or hub mutation, and the recipient's
// TCP destination is the relay hostname only.
func TestRelayOnlyPrepareCannotTouchDirect(t *testing.T) {
	app, ctrl, _, view, spy := newSelectionController(t, true)
	apiKeyID := routeSession(t, app, "relaytouch", func(r *core.Record) { r.Set("relay_only", true) })
	port := seedAgentFacts(t, app, apiKeyID, routeDirectIP)
	grantPresence(t, view, agentRecordID(t, app, apiKeyID), port)

	resp := httptest.NewRecorder()
	rec, status := ctrl.ResolveForRedirect("relaytouch")
	if status != http.StatusFound {
		t.Fatalf("lifecycle status = %d, want 302", status)
	}
	req := httptest.NewRequest("GET", "/s/relaytouch?origin=https://evil.example&direct=1", nil)
	req.Header.Set("X-Forwarded-Host", "evil.example")
	if err := ctrl.SelectRoute(resp, req, rec, "relaytouch"); err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if resp.Code != http.StatusFound || !strings.Contains(resp.Header().Get("Location"), routeRelayOriginFor("relaytouch")) {
		t.Fatalf("relayOnly selection = %d %q", resp.Code, resp.Header().Get("Location"))
	}

	// Direct state completely untouched: no epoch was ever created for the
	// agent, no STUN challenge issued, no DNS, no open signal, no probe.
	if ctrl.epochReady(apiKeyID) {
		t.Fatalf("relayOnly selection created a ready epoch")
	}
	emitOpens, probes, agentSends, stunIssues, ddns, relayDNS := spy.counts()
	if emitOpens != 0 || probes != 0 || agentSends != 0 || stunIssues != 0 || ddns != 0 || relayDNS != 0 {
		t.Fatalf("relayOnly selection touched direct machinery: %d %d %d %d %d %d",
			emitOpens, probes, agentSends, stunIssues, ddns, relayDNS)
	}
	if resp.Header().Get("Location") != "https://"+routeRelayOriginFor("relaytouch")+"/s/relaytouch" {
		t.Fatalf("Location = %q", resp.Header().Get("Location"))
	}
}

// TestOriginsAlwaysDerivedFromPersistedSession proves both origins are
// control-constructed from persisted session state and that no request or
// agent input can influence them (§9.3): poisoned request headers, query
// parameters, and URL paths leave the relay Location unchanged; a malformed
// persisted origin fails closed to 503 rather than echoing anything; the
// interstitial body is a constant with no request reflection.
func TestOriginsAlwaysDerivedFromPersistedSession(t *testing.T) {
	app, ctrl, _, view, _ := newSelectionController(t, true)

	apiKeyID := routeSession(t, app, "origins", func(r *core.Record) { r.Set("relay_only", true) })
	port := seedAgentFacts(t, app, apiKeyID, routeDirectIP)
	grantPresence(t, view, agentRecordID(t, app, apiKeyID), port)

	poisonings := []func(*http.Request){
		func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "evil.example") },
		func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") },
		func(r *http.Request) { r.Host = "evil.example" },
		func(r *http.Request) {
			r.URL.RawQuery = "origin=evil.example&relay=evil.example&url=https://evil.example"
		},
		func(r *http.Request) { r.URL.Path = "/s/origins/../../../evil" },
	}
	for i, poison := range poisonings {
		rec, status := ctrl.ResolveForRedirect("origins")
		if status != http.StatusFound {
			t.Fatalf("poison %d: lifecycle status %d", i, status)
		}
		resp := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/s/origins", nil)
		poison(req) // the attack input rides on the REQUEST reaching selection
		if err := ctrl.SelectRoute(resp, req, rec, "origins"); err != nil {
			t.Fatalf("poison %d: SelectRoute: %v", i, err)
		}
		if got := resp.Header().Get("Location"); got != "https://"+routeRelayOriginFor("origins")+"/s/origins" {
			t.Fatalf("poison %d: Location = %q, want the persisted-session derivation", i, got)
		}
	}

	// The agent row cannot influence the origin either: mutating the agent's
	// namespace/endpoint fields does not move the relay URL.
	agentRecs, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": apiKeyID})
	if err != nil || len(agentRecs) == 0 {
		t.Fatalf("agent row: %v", err)
	}
	agentRecs[0].Set("namespace", "sbevil000")
	agentRecs[0].Set("endpoint_ip", "192.0.2.99")
	if err := app.Save(agentRecs[0]); err != nil {
		t.Fatalf("mutate agent: %v", err)
	}
	rec, status := ctrl.ResolveForRedirect("origins")
	resp := httptest.NewRecorder()
	if err := ctrl.SelectRoute(resp, httptest.NewRequest("GET", "/s/origins", nil), rec, "origins"); err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if got := resp.Header().Get("Location"); got != "https://"+routeRelayOriginFor("origins")+"/s/origins" {
		t.Fatalf("agent-influenced Location = %q", got)
	}

	// A persisted origin that is not a direct origin fails closed: 503, and
	// never a malformed or attacker-shaped Location.
	app2, ctrl2, _, view2, _ := newSelectionController(t, true)
	badKey := routeSession(t, app2, "badorigin", func(r *core.Record) {
		r.Set("relay_only", true)
		r.Set("origin", "nodot")
	})
	badPort := seedAgentFacts(t, app2, badKey, routeDirectIP)
	grantPresence(t, view2, agentRecordID(t, app2, badKey), badPort)
	badRec, status := ctrl2.ResolveForRedirect("badorigin")
	if status != http.StatusFound {
		t.Fatalf("bad origin lifecycle status %d", status)
	}
	badResp := httptest.NewRecorder()
	if err := ctrl2.SelectRoute(badResp, httptest.NewRequest("GET", "/s/badorigin", nil), badRec, "badorigin"); err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if badResp.Code != http.StatusServiceUnavailable {
		t.Fatalf("bad origin status = %d, want 503", badResp.Code)
	}
	if loc := badResp.Header().Get("Location"); loc != "" {
		t.Fatalf("bad origin Location = %q, want empty", loc)
	}

	// The interstitial reflects no request input either.
	app3, ctrl3, _, _, _ := newSelectionController(t, true)
	directKey := routeSession(t, app3, "constbody", nil)
	seedAgentFacts(t, app3, directKey, routeDirectIP)
	connectWS(t, ctrl3, directKey)
	drec, dstatus := ctrl3.ResolveForRedirect("constbody")
	if dstatus != http.StatusFound {
		t.Fatalf("direct candidate lifecycle status %d", dstatus)
	}
	dresp := httptest.NewRecorder()
	dreq := httptest.NewRequest("GET", "/s/constbody?q=<script>alert(1)</script>", nil)
	dreq.Header.Set("X-Forwarded-Host", "evil.example")
	if err := ctrl3.SelectRoute(dresp, dreq, drec, "constbody"); err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if dresp.Code != http.StatusOK {
		t.Fatalf("interstitial status = %d", dresp.Code)
	}
	body := dresp.Body.String()
	if strings.Contains(body, "script") || strings.Contains(body, "evil.example") {
		t.Fatalf("interstitial body reflects request input: %q", body)
	}
}

// TestRollbackModeDisablesRelaySelectionButKeepsNewDirectFlow is the plan's
// exact four-scenario rollback proof with RELAY_SELECTION_ENABLED=false:
//
//  1. a non-relayOnly direct candidate still gets the Phase 4a interstitial;
//  2. a relay-ineligible, direct-ineligible case gets the 503 offline page;
//  3. a present relay with the flag off still gets 503 rather than relay;
//  4. no case revives the legacy Phase 3 direct 302.
func TestRollbackModeDisablesRelaySelectionButKeepsNewDirectFlow(t *testing.T) {
	app, ctrl, _, view, spy := newSelectionController(t, false)

	// Scenario 1: direct candidate (endpoint published, WS ready, fresh STUN
	// match) with a relay lease present → interstitial, never relay, never
	// the legacy direct 302. The same shape with STUN entirely unwired (no
	// observation at all — the rollback topology) stays an interstitial too:
	// STUN-off is rollback mode, and "only STUN freshness missing" is still
	// the interstitial category (§9.1).
	rollKey := routeSession(t, app, "roll0001", nil)
	port := seedAgentFacts(t, app, rollKey, routeDirectIP)
	grantPresence(t, view, agentRecordID(t, app, rollKey), port)
	connectWS(t, ctrl, rollKey)
	installObservation(t, ctrl, rollKey, netip.MustParseAddr(routeDirectIP), true)
	resp := assertSelection(t, ctrl, "roll0001", http.StatusFound, http.StatusOK, "")
	if resp.Header().Get("Location") != "" {
		t.Fatalf("scenario 1: rollback interstitial carried Location %q", resp.Header().Get("Location"))
	}

	rollKey2 := routeSession(t, app, "roll0002", nil)
	seedAgentFacts(t, app, rollKey2, routeDirectIP)
	connectWS(t, ctrl, rollKey2)
	assertSelection(t, ctrl, "roll0002", http.StatusFound, http.StatusOK, "")

	// Scenario 2: relay-ineligible (no lease) + direct-ineligible (fresh
	// STUN mismatch vs the published endpoint) → 503 offline page.
	rollKey3 := routeSession(t, app, "roll0003", nil)
	seedAgentFacts(t, app, rollKey3, routeDirectIP)
	connectWS(t, ctrl, rollKey3)
	installObservation(t, ctrl, rollKey3, netip.MustParseAddr("198.51.100.9"), true)
	assertSelection(t, ctrl, "roll0003", http.StatusFound, http.StatusServiceUnavailable, "")

	// Scenario 3: same direct-ineligible shape WITH a present relay lease —
	// the flag off still yields 503 rather than a relay 302.
	rollKey4 := routeSession(t, app, "roll0004", nil)
	port4 := seedAgentFacts(t, app, rollKey4, routeDirectIP)
	grantPresence(t, view, agentRecordID(t, app, rollKey4), port4)
	connectWS(t, ctrl, rollKey4)
	installObservation(t, ctrl, rollKey4, netip.MustParseAddr("198.51.100.9"), true)
	assertSelection(t, ctrl, "roll0004", http.StatusFound, http.StatusServiceUnavailable, "")

	// relayOnly under rollback stays a lifecycle 410 (Phase 3 classification
	// preserved); selection never runs for it.
	rollKey5 := routeSession(t, app, "roll0005", func(r *core.Record) { r.Set("relay_only", true) })
	port5 := seedAgentFacts(t, app, rollKey5, routeDirectIP)
	grantPresence(t, view, agentRecordID(t, app, rollKey5), port5)
	assertSelection(t, ctrl, "roll0005", http.StatusGone, 0, "")

	// Scenario 4: sweep — no rollback case produced ANY 302, so the legacy
	// Phase 3 direct 302 ("https://<origin>/s/<code>") is never revived. The
	// only direct machinery that could have served it (open signal, probe)
	// was never invoked either.
	for _, code := range []string{"roll0001", "roll0002", "roll0003", "roll0004", "roll0005"} {
		rec, status := ctrl.ResolveForRedirect(code)
		if status == http.StatusFound {
			resp := httptest.NewRecorder()
			if err := ctrl.SelectRoute(resp, httptest.NewRequest("GET", "/s/"+code, nil), rec, code); err != nil {
				t.Fatalf("%s: SelectRoute: %v", code, err)
			}
			if resp.Code == http.StatusFound {
				t.Fatalf("%s: rollback mode produced a 302 (Location %q)", code, resp.Header().Get("Location"))
			}
			if loc := resp.Header().Get("Location"); strings.Contains(loc, routeOriginFor(code)) {
				t.Fatalf("%s: rollback Location %q revives the legacy direct 302", code, loc)
			}
		}
	}
	emitOpens, probes, agentSends, stunIssues, ddns, relayDNS := spy.counts()
	if emitOpens != 0 || probes != 0 || agentSends != 0 || stunIssues != 0 || ddns != 0 || relayDNS != 0 {
		t.Fatalf("rollback selection had side effects: %d %d %d %d %d %d",
			emitOpens, probes, agentSends, stunIssues, ddns, relayDNS)
	}
}

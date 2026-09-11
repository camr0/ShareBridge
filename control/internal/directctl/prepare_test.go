package directctl

// Task 21 bounded prepare-route tests (plan Task 21; spec §§4.4, 9.3, 6.1,
// 9.2, 12). POST /api/shares/<code>/prepare-route re-resolves lifecycle and
// the live direct predicate, then runs the ONE four-second preparation
// context: inline STUN refresh ∥ open_signal/open_ack (mapping), with the
// public verified-tuple probe strictly sequenced AFTER the STUN match (the
// real Task 19 gate inside Probe). A budget miss answers from relay presence
// and persists NOTHING (§9.1 — cold-open behavior, never a durable direct
// classification). Every response is bounded no-store JSON whose URLs are
// derived from the persisted session row only (§6 forms); internal agent
// identifiers never appear.
//
// No test measures or compares direct/relay performance (§9.1): the probe is
// exercised only to prove the orchestration ordering (who may run when),
// never to score routes.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

const prepareTestNonce = "nonce-prepare"

// prepareOKAck is the canonical "ok" open ack the emitOpenFn stubs return:
// the agent granted the direct open on port 8443 at the routeDirectIP the
// STUN observation will confirm.
func prepareOKAck() OpenAck {
	return OpenAck{
		Status:      "ok",
		GrantedPort: 8443,
		PublicIP:    routeDirectIP,
		Nonce:       prepareTestNonce,
	}
}

// prepareCall drives the handler exactly as the router would (the router
// registration itself is exercised by cmd/server tests / main.go wiring).
func prepareCall(t *testing.T, ctrl *Controller, code string, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/shares/"+code+"/prepare-route", reader)
	if mutate != nil {
		mutate(req)
	}
	resp := httptest.NewRecorder()
	if err := ctrl.PrepareRoute(resp, req, code); err != nil {
		t.Fatalf("PrepareRoute: %v", err)
	}
	return resp
}

// decodePrepareBody parses the bounded JSON response.
func decodePrepareBody(t *testing.T, resp *httptest.ResponseRecorder) prepareResponse {
	t.Helper()
	var out prepareResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("response body is not bounded JSON: %v (%q)", err, resp.Body.String())
	}
	return out
}

// requireNoStore asserts the §9.3 non-cacheability contract on any
// preparation response.
func requireNoStore(t *testing.T, resp *httptest.ResponseRecorder) {
	t.Helper()
	if cc := resp.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	if ct := resp.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

// requireNoAgentIdentifiers proves the response never exposes internal agent
// identifiers (the API-key id and the agents-row record id are the two
// internal identities a recipient must never learn).
func requireNoAgentIdentifiers(t *testing.T, apiKeyID, agentRecordID, body string) {
	t.Helper()
	if apiKeyID != "" && strings.Contains(body, apiKeyID) {
		t.Fatalf("response leaks the internal api key id")
	}
	if agentRecordID != "" && strings.Contains(body, agentRecordID) {
		t.Fatalf("response leaks the internal agent record id")
	}
}

// TestPrepareRouteSuccessReturnsDerivedURLsAnd4000ms drives the full real
// preparation path for a warm, direct-eligible share: lifecycle re-resolved,
// live predicate matched (fresh public matching observation over the REAL
// wired STUN gate), real EmitOpen-contract ack, real Probe through the Task 19
// gate (nonce-echo transport — no network). Success returns bounded no-store
// JSON with the persisted-session-derived direct URL, the optional relay URL
// (flag on + presence lease), and directTimeoutMs=4000 — and never an
// internal agent identifier.
func TestPrepareRouteSuccessReturnsDerivedURLsAnd4000ms(t *testing.T) {
	app, ctrl, _, view, _ := newSelectionController(t, true)
	// Wire the §10.3 gate exactly as the Phase 4a topology does: a real STUN
	// listener is installed so Probe's gate is ACTIVE (a fresh exact match is
	// the only thing that authorizes the probe).
	attachSTUNListener(t, ctrl, newSTUNTestClock())

	code := "prep0001"
	key := routeSession(t, app, code, nil)
	port := seedAgentFacts(t, app, key, routeDirectIP)
	grantPresence(t, view, agentRecordID(t, app, key), port)
	connectWS(t, ctrl, key)
	t.Cleanup(func() { ctrl.AgentDisconnected(key, nil) })
	// Warm current-epoch observation: fresh, public, exactly matching the
	// published endpoint AND the ack's public IP.
	installObservation(t, ctrl, key, netip.MustParseAddr(routeDirectIP), true)

	// Real probe machinery against a nonce-echo transport (no network).
	counter := &probeTransportCounter{nonce: prepareTestNonce}
	ctrl.probeClient = &http.Client{Transport: counter}
	ctrl.probeFn = ctrl.Probe
	ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
		return prepareOKAck(), nil
	}

	resp := prepareCall(t, ctrl, code, "", nil)
	requireNoStore(t, resp)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	out := decodePrepareBody(t, resp)
	if out.Status != "direct" {
		t.Fatalf("status = %q, want %q", out.Status, "direct")
	}
	wantDirect := "https://" + routeOriginFor(code) + ":8443/s/" + code
	if out.DirectURL != wantDirect {
		t.Fatalf("direct_url = %q, want the persisted-session derivation %q", out.DirectURL, wantDirect)
	}
	wantRelay := "https://" + routeRelayOriginFor(code) + "/s/" + code
	if out.RelayURL != wantRelay {
		t.Fatalf("relay_url = %q, want the §6 derivation %q", out.RelayURL, wantRelay)
	}
	if out.DirectTimeoutMs != 4000 {
		t.Fatalf("direct_timeout_ms = %d, want 4000", out.DirectTimeoutMs)
	}
	if !strings.Contains(body, `"direct_timeout_ms":4000`) {
		t.Fatalf("body %q must carry the literal 4000 ms budget", body)
	}
	if counter.count() != 1 {
		t.Fatalf("real probe ran %d times, want exactly 1", counter.count())
	}
	requireNoAgentIdentifiers(t, key, agentRecordID(t, app, key), body)
}

// TestPrepareRouteOverallBudgetNeverExceedsFourSeconds pins §4.4: ONE
// four-second context encompasses the inline STUN refresh, the open_signal/
// open_ack round trip, and the probe. Proven structurally (every stage sees
// the same deadline, at most start+4s) and behaviorally (a stage that hangs
// until the context fires cannot hold the response past the bound; the miss
// is answered by the available relay for that navigation).
func TestPrepareRouteOverallBudgetNeverExceedsFourSeconds(t *testing.T) {
	app, ctrl, _, view, _ := newSelectionController(t, true)
	code := "budget01"
	key := routeSession(t, app, code, nil)
	port := seedAgentFacts(t, app, key, routeDirectIP)
	grantPresence(t, view, agentRecordID(t, app, key), port)
	connectWS(t, ctrl, key)
	t.Cleanup(func() { ctrl.AgentDisconnected(key, nil) })
	// No observation: the inline await runs inside the same budget context
	// (it fails closed fast — the issue fn refuses — keeping this subtest
	// focused on the open-signal hang).

	var mu sync.Mutex
	var start, deadlineSeen time.Time
	hang := make(chan struct{}) // closed once when the hanging stage observes ctx.Done()
	ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
		deadline, has := ctx.Deadline()
		mu.Lock()
		deadlineSeen = deadline
		start = time.Now()
		mu.Unlock()
		if !has {
			t.Error("open stage: context carries no deadline — the budget is not a deadline")
		}
		<-ctx.Done() // hang until the overall budget fires
		select {
		case <-hang:
		default:
			close(hang)
		}
		return OpenAck{}, ctx.Err()
	}
	probes := 0
	ctrl.probeFn = func(ctx context.Context, origin, code, apiKeyID string, ack OpenAck) error {
		mu.Lock()
		probes++
		mu.Unlock()
		return errors.New("probe must not run after a budget miss")
	}

	began := time.Now()
	resp := prepareCall(t, ctrl, code, "", nil)
	elapsed := time.Since(began)

	// (a) Structural: the single shared deadline is at most start+4s (and
	// meaningfully close to it — it is ONE four-second budget, not a per-stage
	// accumulation).
	mu.Lock()
	d := deadlineSeen.Sub(start)
	mu.Unlock()
	if d > prepareRouteBudget {
		t.Fatalf("shared stage deadline = %v after the 4 s mark", d)
	}
	if d < 3500*time.Millisecond {
		t.Fatalf("shared stage deadline = %v, want one full ~4 s budget", d)
	}

	// (b) Behavioral: the response cannot be held past the bound.
	if elapsed > 5500*time.Millisecond {
		t.Fatalf("preparation returned after %v — the overall budget is not enforced", elapsed)
	}
	select {
	case <-hang:
	default:
		t.Fatalf("the hanging stage was never released by the budget context")
	}

	// The miss is answered by the available relay for this navigation (§9.1)
	// — and the probe never ran.
	requireNoStore(t, resp)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", resp.Code, resp.Body.String())
	}
	out := decodePrepareBody(t, resp)
	if out.Status != "relay" || out.RelayURL != "https://"+routeRelayOriginFor(code)+"/s/"+code {
		t.Fatalf("budget miss response = %+v, want the derived relay URL", out)
	}
	if out.DirectURL != "" {
		t.Fatalf("budget miss leaked a direct URL %q", out.DirectURL)
	}
	mu.Lock()
	defer mu.Unlock()
	if probes != 0 {
		t.Fatalf("probe ran %d times after the budget miss", probes)
	}
}

// TestPrepareRunsMappingAndSTUNConcurrentlyButProbeWaitsForMatch pins the
// §4.4 sequencing: the open_signal (mapping) is already in flight while the
// inline STUN refresh is still pending (overlap), and the public probe fires
// only AFTER the STUN match succeeds — against the REAL Task 19 gate, so no
// probe packet can precede the match.
func TestPrepareRunsMappingAndSTUNConcurrentlyButProbeWaitsForMatch(t *testing.T) {
	app, ctrl, _, _, _ := newSelectionController(t, true)
	clock := newSTUNTestClock()
	ctrl.nowFn = clock.Now // one fake clock for controller + listener freshness
	advertise := attachSTUNListener(t, ctrl, clock)
	rec := newSTUNSendRecorder(t, ctrl)

	code := "concurre"
	key := routeSession(t, app, code, nil)
	seedAgentFacts(t, app, key, routeDirectIP)
	connectWS(t, ctrl, key)
	t.Cleanup(func() { ctrl.AgentDisconnected(key, nil) })
	// No observation yet: the await must challenge inline (once).

	counter := &probeTransportCounter{nonce: prepareTestNonce}
	ctrl.probeClient = &http.Client{Transport: counter}
	ctrl.probeFn = ctrl.Probe // the real §10.3 gate + probe

	var mu sync.Mutex
	openStarted := false
	matchReady := make(chan struct{})
	ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
		mu.Lock()
		openStarted = true
		mu.Unlock()
		select {
		case <-matchReady: // released only after the STUN match below
			return prepareOKAck(), nil
		case <-ctx.Done():
			return OpenAck{}, ctx.Err()
		}
	}

	resp := httptest.NewRecorder()
	done := make(chan error, 1)
	go func() {
		done <- ctrl.PrepareRoute(resp, httptest.NewRequest(http.MethodPost, "/api/shares/"+code+"/prepare-route", nil), code)
	}()

	// The mapping stage is in flight while STUN is still unresolved — the
	// two overlap inside the one preparation context.
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return openStarted
	}, "open stage to start")
	challenges := waitForChallenges(t, rec, 1) // the await issued ONE inline challenge
	mu.Lock()
	started := openStarted
	mu.Unlock()
	if !started {
		t.Fatalf("open stage must already be in flight while STUN is pending")
	}

	// While the observation is missing, the probe must not have fired.
	if n := counter.count(); n != 0 {
		t.Fatalf("probe ran %d times before any STUN match", n)
	}

	// Answer the inline challenge with a REAL STUN exchange (the observation
	// lands, but loopback-classified — the gate would refuse it), then install
	// the matching public observation: the "match succeeded" event.
	packed, _ := challenges[0]["challenge"].(string)
	id, txnHex, receiptHex, _ := stunExchange(t, advertise, packed)
	deliverSTUNResult(t, ctrl, key, id, txnHex, receiptHex)
	waitFor(t, 2*time.Second, func() bool {
		_, ok := ctrl.CurrentSTUNObservation(key, clock.Now())
		return ok
	}, "delivered observation to land")
	if n := counter.count(); n != 0 {
		t.Fatalf("probe ran %d times before the STUN match succeeded", n)
	}
	var matchAt time.Time
	installObservation(t, ctrl, key, netip.MustParseAddr(routeDirectIP), true)
	matchAt = time.Now()
	close(matchReady) // release the mapping stage

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PrepareRoute: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("preparation did not complete after the match released it")
	}

	// Probe strictly sequenced after the match, and it ran exactly once
	// through the real gate.
	if n := counter.count(); n != 1 {
		t.Fatalf("probe ran %d times, want exactly 1 after the match", n)
	}
	mu.Lock()
	defer mu.Unlock()
	_ = matchAt

	requireNoStore(t, resp)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", resp.Code, resp.Body.String())
	}
	out := decodePrepareBody(t, resp)
	if out.Status != "direct" || out.DirectURL != "https://"+routeOriginFor(code)+":8443/s/"+code {
		t.Fatalf("response = %+v, want the direct success derivation", out)
	}
	if len(rec.challenges()) != 1 {
		t.Fatalf("inline refresh issued %d challenges, want exactly 1", len(rec.challenges()))
	}
}

// TestPrepareFailureSelectsAvailableRelay proves every preparation failure
// arm answers with the available relay (flag on + presence lease) as bounded
// JSON — never a direct URL, never a topology detail — and that a relayOnly
// share is NEVER prepared at all (§6.1: no selection, preparation, probe, or
// direct navigation for it).
func TestPrepareFailureSelectsAvailableRelay(t *testing.T) {
	t.Run("open_signal failure selects relay", func(t *testing.T) {
		app, ctrl, _, view, spy := newSelectionController(t, true)
		code := "failop01"
		key := routeSession(t, app, code, nil)
		port := seedAgentFacts(t, app, key, routeDirectIP)
		grantPresence(t, view, agentRecordID(t, app, key), port)
		connectWS(t, ctrl, key)
		t.Cleanup(func() { ctrl.AgentDisconnected(key, nil) })
		installObservation(t, ctrl, key, netip.MustParseAddr(routeDirectIP), true)

		ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
			return OpenAck{}, errors.New("open_signal send failed")
		}
		resp := prepareCall(t, ctrl, code, "", nil)
		assertRelayFallback(t, app, resp, code, key)
		emitOpens, probes, _, _, _, _ := spy.counts()
		_ = emitOpens // our own stub replaced the spy's; probe counting is what matters
		if probes != 0 {
			t.Fatalf("probe ran %d times after an open failure", probes)
		}
	})

	t.Run("open_ack rejection selects relay", func(t *testing.T) {
		app, ctrl, _, view, _ := newSelectionController(t, true)
		code := "failack02"
		key := routeSession(t, app, code, nil)
		port := seedAgentFacts(t, app, key, routeDirectIP)
		grantPresence(t, view, agentRecordID(t, app, key), port)
		connectWS(t, ctrl, key)
		t.Cleanup(func() { ctrl.AgentDisconnected(key, nil) })

		ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
			return OpenAck{Status: "error", Error: "port busy"}, nil
		}
		resp := prepareCall(t, ctrl, code, "", nil)
		assertRelayFallback(t, app, resp, code, key)
	})

	t.Run("probe failure selects relay", func(t *testing.T) {
		app, ctrl, _, view, _ := newSelectionController(t, true)
		code := "failpr03"
		key := routeSession(t, app, code, nil)
		port := seedAgentFacts(t, app, key, routeDirectIP)
		grantPresence(t, view, agentRecordID(t, app, key), port)
		connectWS(t, ctrl, key)
		t.Cleanup(func() { ctrl.AgentDisconnected(key, nil) })
		installObservation(t, ctrl, key, netip.MustParseAddr(routeDirectIP), true)

		ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
			return prepareOKAck(), nil
		}
		ctrl.probeFn = func(ctx context.Context, origin, code, apiKeyID string, ack OpenAck) error {
			return errors.New("probe nonce mismatch")
		}
		resp := prepareCall(t, ctrl, code, "", nil)
		assertRelayFallback(t, app, resp, code, key)
	})

	t.Run("hard direct-ineligible selects relay without preparing", func(t *testing.T) {
		app, ctrl, _, view, spy := newSelectionController(t, true)
		code := "failmm04"
		key := routeSession(t, app, code, nil)
		port := seedAgentFacts(t, app, key, routeDirectIP)
		grantPresence(t, view, agentRecordID(t, app, key), port)
		connectWS(t, ctrl, key)
		t.Cleanup(func() { ctrl.AgentDisconnected(key, nil) })
		// Fresh observation that CONTRADICTS the published endpoint: not
		// preparable (§9.1 hard-ineligible arm) — relay without any
		// preparation attempt.
		installObservation(t, ctrl, key, netip.MustParseAddr("198.51.100.9"), true)

		resp := prepareCall(t, ctrl, code, "", nil)
		assertRelayFallback(t, app, resp, code, key)
		emitOpens, probes, agentSends, stunIssues, ddns, relayDNS := spy.counts()
		if emitOpens != 0 || probes != 0 || agentSends != 0 || stunIssues != 0 || ddns != 0 || relayDNS != 0 {
			t.Fatalf("hard-ineligible share was prepared: %d %d %d %d %d %d", emitOpens, probes, agentSends, stunIssues, ddns, relayDNS)
		}
	})

	t.Run("relayOnly never prepares (§6.1)", func(t *testing.T) {
		app, ctrl, _, view, spy := newSelectionController(t, true)
		code := "relayonly"
		key := routeSession(t, app, code, func(r *core.Record) { r.Set("relay_only", true) })
		port := seedAgentFacts(t, app, key, routeDirectIP)
		grantPresence(t, view, agentRecordID(t, app, key), port)
		connectWS(t, ctrl, key)
		t.Cleanup(func() { ctrl.AgentDisconnected(key, nil) })
		installObservation(t, ctrl, key, netip.MustParseAddr(routeDirectIP), true)

		resp := prepareCall(t, ctrl, code, "", nil)
		assertRelayFallback(t, app, resp, code, key)
		if out := decodePrepareBody(t, resp); out.DirectURL != "" {
			t.Fatalf("relayOnly preparation leaked a direct URL %q", out.DirectURL)
		}
		emitOpens, probes, agentSends, stunIssues, ddns, relayDNS := spy.counts()
		if emitOpens != 0 || probes != 0 || agentSends != 0 || stunIssues != 0 || ddns != 0 || relayDNS != 0 {
			t.Fatalf("relayOnly share touched direct machinery: %d %d %d %d %d %d", emitOpens, probes, agentSends, stunIssues, ddns, relayDNS)
		}
	})
}

// assertRelayFallback asserts a 200 bounded-JSON relay fallback answer with
// only the §6-derived relay URL (and no-store, and no internal identifiers).
func assertRelayFallback(t *testing.T, app core.App, resp *httptest.ResponseRecorder, code, key string) {
	t.Helper()
	requireNoStore(t, resp)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", resp.Code, resp.Body.String())
	}
	out := decodePrepareBody(t, resp)
	if out.Status != "relay" {
		t.Fatalf("status = %q, want %q", out.Status, "relay")
	}
	if want := "https://" + routeRelayOriginFor(code) + "/s/" + code; out.RelayURL != want {
		t.Fatalf("relay_url = %q, want %q", out.RelayURL, want)
	}
	if out.DirectURL != "" {
		t.Fatalf("failure arm leaked a direct URL %q", out.DirectURL)
	}
	if out.DirectTimeoutMs != 0 {
		t.Fatalf("failure arm carries direct_timeout_ms %d", out.DirectTimeoutMs)
	}
	requireNoAgentIdentifiers(t, key, agentRecordID(t, app, key), resp.Body.String())
}

// TestBudgetMissDoesNotPersistStickyFailure proves §9.1: a four-second
// preparation budget miss uses the available relay for that navigation and
// does NOT persist a lasting direct-ineligible state — the agents row is
// byte-identical across the miss, the probe verified-tuple cache stays empty,
// and no probe ever fires.
func TestBudgetMissDoesNotPersistStickyFailure(t *testing.T) {
	app, ctrl, _, view, _ := newSelectionController(t, true)
	code := "miss0001"
	key := routeSession(t, app, code, nil)
	port := seedAgentFacts(t, app, key, routeDirectIP)
	grantPresence(t, view, agentRecordID(t, app, key), port)
	connectWS(t, ctrl, key)
	t.Cleanup(func() { ctrl.AgentDisconnected(key, nil) })
	installObservation(t, ctrl, key, netip.MustParseAddr(routeDirectIP), true)

	before := snapshotAgentRow(t, app, code)

	ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
		<-ctx.Done() // the mapping stage consumes the whole budget
		return OpenAck{}, ctx.Err()
	}
	probes := 0
	ctrl.probeFn = func(ctx context.Context, origin, code, apiKeyID string, ack OpenAck) error {
		probes++
		return errors.New("must not run")
	}

	resp := prepareCall(t, ctrl, code, "", nil)

	requireNoStore(t, resp)
	out := decodePrepareBody(t, resp)
	if out.Status != "relay" {
		t.Fatalf("budget miss response = %+v, want the relay fallback", out)
	}
	if probes != 0 {
		t.Fatalf("probe ran %d times on the budget-miss path", probes)
	}
	// Nothing sticky was persisted: the agents row (diagnostics included) is
	// untouched and the verified-tuple cache did not grow.
	if after := snapshotAgentRow(t, app, code); after != before {
		t.Fatalf("budget miss mutated the agents row:\nbefore %q\nafter  %q", before, after)
	}
	ctrl.verifiedMu.Lock()
	n := len(ctrl.verified)
	ctrl.verifiedMu.Unlock()
	if n != 0 {
		t.Fatalf("verified-tuple cache grew to %d on the budget-miss path", n)
	}
}

// TestPrepareRouteLifecycleRejections re-resolves lifecycle per request
// through ResolveForRedirect: unknown/revoked → 404, expired/unsupported →
// 410 (relayOnly with the flag off stays a lifecycle 410). No machinery is
// ever touched for a lifecycle-rejected code, and every rejection is no-store.
func TestPrepareRouteLifecycleRejections(t *testing.T) {
	t.Run("unknown code 404", func(t *testing.T) {
		_, ctrl, _, _, spy := newSelectionController(t, true)
		resp := prepareCall(t, ctrl, "nosuch00", "", nil)
		assertLifecycleRejection(t, resp, http.StatusNotFound)
		assertNoMachinery(t, spy)
	})
	t.Run("revoked 404", func(t *testing.T) {
		app, ctrl, _, _, spy := newSelectionController(t, true)
		routeSession(t, app, "revoked0", func(r *core.Record) {
			r.Set("is_active", false)
			r.Set("inactive_reason", "revoked")
		})
		resp := prepareCall(t, ctrl, "revoked0", "", nil)
		assertLifecycleRejection(t, resp, http.StatusNotFound)
		assertNoMachinery(t, spy)
	})
	t.Run("expired 410", func(t *testing.T) {
		app, ctrl, _, _, spy := newSelectionController(t, true)
		routeSession(t, app, "expired0", func(r *core.Record) {
			r.Set("expires_at", time.Now().Add(-time.Hour))
		})
		resp := prepareCall(t, ctrl, "expired0", "", nil)
		assertLifecycleRejection(t, resp, http.StatusGone)
		assertNoMachinery(t, spy)
	})
	t.Run("unsupported (password-protected) 410", func(t *testing.T) {
		app, ctrl, _, _, spy := newSelectionController(t, true)
		routeSession(t, app, "locked000", func(r *core.Record) {
			r.Set("is_password_protected", true)
		})
		resp := prepareCall(t, ctrl, "locked000", "", nil)
		assertLifecycleRejection(t, resp, http.StatusGone)
		assertNoMachinery(t, spy)
	})
	t.Run("relayOnly without the flag is a lifecycle 410", func(t *testing.T) {
		app, ctrl, _, _, spy := newSelectionController(t, false)
		routeSession(t, app, "roflagoff", func(r *core.Record) { r.Set("relay_only", true) })
		resp := prepareCall(t, ctrl, "roflagoff", "", nil)
		assertLifecycleRejection(t, resp, http.StatusGone)
		assertNoMachinery(t, spy)
	})
}

// TestPrepareRouteMethodAndBodyRejections pins the surface contract: POST
// only (405 with Allow), bounded body accepting empty or one JSON object,
// 400 for malformed JSON, 413 for oversized bodies — and no request input
// (origin/redirect/url query, header, or body fields) is ever accepted or
// reflected: response URLs stay derived from the persisted session.
func TestPrepareRouteMethodAndBodyRejections(t *testing.T) {
	t.Run("GET is 405 POST-only", func(t *testing.T) {
		_, ctrl, _, _, _ := newSelectionController(t, true)
		req := httptest.NewRequest(http.MethodGet, "/api/shares/prep0002/prepare-route", nil)
		resp := httptest.NewRecorder()
		if err := ctrl.PrepareRoute(resp, req, "prep0002"); err != nil {
			t.Fatalf("PrepareRoute: %v", err)
		}
		if resp.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET status = %d, want 405", resp.Code)
		}
		if allow := resp.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("Allow = %q, want POST", allow)
		}
		requireNoStore(t, resp)
	})
	t.Run("PUT is 405", func(t *testing.T) {
		_, ctrl, _, _, _ := newSelectionController(t, true)
		req := httptest.NewRequest(http.MethodPut, "/api/shares/prep0002/prepare-route", nil)
		resp := httptest.NewRecorder()
		if err := ctrl.PrepareRoute(resp, req, "prep0002"); err != nil {
			t.Fatalf("PrepareRoute: %v", err)
		}
		if resp.Code != http.StatusMethodNotAllowed {
			t.Fatalf("PUT status = %d, want 405", resp.Code)
		}
	})
	t.Run("empty body is accepted", func(t *testing.T) {
		app, ctrl, _, view, _ := newSelectionController(t, true)
		code := "prep0002"
		key := routeSession(t, app, code, nil)
		port := seedAgentFacts(t, app, key, routeDirectIP)
		grantPresence(t, view, agentRecordID(t, app, key), port)
		connectWS(t, ctrl, key)
		t.Cleanup(func() { ctrl.AgentDisconnected(key, nil) })
		installObservation(t, ctrl, key, netip.MustParseAddr(routeDirectIP), true)
		counter := &probeTransportCounter{nonce: prepareTestNonce}
		ctrl.probeClient = &http.Client{Transport: counter}
		ctrl.probeFn = ctrl.Probe
		ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
			return prepareOKAck(), nil
		}
		resp := prepareCall(t, ctrl, code, "", nil)
		if resp.Code != http.StatusOK {
			t.Fatalf("empty body status = %d, want 200 (body %q)", resp.Code, resp.Body.String())
		}
	})
	t.Run("JSON object body is accepted", func(t *testing.T) {
		app, ctrl, _, view, _ := newSelectionController(t, true)
		code := "prep0003"
		key := routeSession(t, app, code, nil)
		port := seedAgentFacts(t, app, key, routeDirectIP)
		grantPresence(t, view, agentRecordID(t, app, key), port)
		connectWS(t, ctrl, key)
		t.Cleanup(func() { ctrl.AgentDisconnected(key, nil) })
		installObservation(t, ctrl, key, netip.MustParseAddr(routeDirectIP), true)
		counter := &probeTransportCounter{nonce: prepareTestNonce}
		ctrl.probeClient = &http.Client{Transport: counter}
		ctrl.probeFn = ctrl.Probe
		ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
			return prepareOKAck(), nil
		}
		resp := prepareCall(t, ctrl, code, `{"client_hint":"anything"}`, nil)
		if resp.Code != http.StatusOK {
			t.Fatalf("JSON body status = %d, want 200 (body %q)", resp.Code, resp.Body.String())
		}
	})
	t.Run("malformed JSON body is 400", func(t *testing.T) {
		app, ctrl, _, _, _ := newSelectionController(t, true)
		code := "prep0004"
		routeSession(t, app, code, nil)
		resp := prepareCall(t, ctrl, code, "{not json", nil)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("malformed body status = %d, want 400", resp.Code)
		}
		requireNoStore(t, resp)
	})
	t.Run("JSON array body is 400", func(t *testing.T) {
		app, ctrl, _, _, _ := newSelectionController(t, true)
		code := "prep0005"
		routeSession(t, app, code, nil)
		resp := prepareCall(t, ctrl, code, `[1,2,3]`, nil)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("array body status = %d, want 400", resp.Code)
		}
	})
	t.Run("oversized body is 413", func(t *testing.T) {
		app, ctrl, _, _, _ := newSelectionController(t, true)
		code := "prep0006"
		routeSession(t, app, code, nil)
		resp := prepareCall(t, ctrl, code, `{"pad":"`+strings.Repeat("x", 9<<10)+`"}`, nil)
		if resp.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized body status = %d, want 413", resp.Code)
		}
		requireNoStore(t, resp)
	})
	t.Run("redirect and origin inputs are never accepted", func(t *testing.T) {
		app, ctrl, _, view, _ := newSelectionController(t, true)
		code := "poison07"
		key := routeSession(t, app, code, nil)
		port := seedAgentFacts(t, app, key, routeDirectIP)
		grantPresence(t, view, agentRecordID(t, app, key), port)
		connectWS(t, ctrl, key)
		t.Cleanup(func() { ctrl.AgentDisconnected(key, nil) })
		installObservation(t, ctrl, key, netip.MustParseAddr(routeDirectIP), true)
		counter := &probeTransportCounter{nonce: prepareTestNonce}
		ctrl.probeClient = &http.Client{Transport: counter}
		ctrl.probeFn = ctrl.Probe
		ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
			return prepareOKAck(), nil
		}

		body := `{"origin":"https://evil.example","redirect":"https://evil.example","url":"https://evil.example","direct":"1"}`
		resp := prepareCall(t, ctrl, code, body, func(req *http.Request) {
			req.URL.RawQuery = "origin=evil.example&redirect=evil.example&url=https://evil.example"
			req.Header.Set("X-Forwarded-Host", "evil.example")
			req.Host = "evil.example"
		})
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", resp.Code, resp.Body.String())
		}
		out := decodePrepareBody(t, resp)
		if out.DirectURL != "https://"+routeOriginFor(code)+":8443/s/"+code {
			t.Fatalf("direct_url = %q, want the persisted-session derivation only", out.DirectURL)
		}
		if out.RelayURL != "https://"+routeRelayOriginFor(code)+"/s/"+code {
			t.Fatalf("relay_url = %q, want the §6 derivation only", out.RelayURL)
		}
		if strings.Contains(resp.Body.String(), "evil.example") {
			t.Fatalf("response reflected request input: %q", resp.Body.String())
		}
	})
}

// ---------- small shared helpers ----------

// waitFor polls cond until it holds or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func assertLifecycleRejection(t *testing.T, resp *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	requireNoStore(t, resp)
	if resp.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %q)", resp.Code, wantStatus, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "https://") {
		t.Fatalf("lifecycle rejection carried a URL: %q", resp.Body.String())
	}
}

func assertNoMachinery(t *testing.T, spy *selectionSpy) {
	t.Helper()
	emitOpens, probes, agentSends, stunIssues, ddns, relayDNS := spy.counts()
	if emitOpens != 0 || probes != 0 || agentSends != 0 || stunIssues != 0 || ddns != 0 || relayDNS != 0 {
		t.Fatalf("lifecycle rejection touched direct machinery: %d %d %d %d %d %d", emitOpens, probes, agentSends, stunIssues, ddns, relayDNS)
	}
}

// TestDirectURLOmitsDefaultPort pins the shared §6 direct-URL builder used by
// every direct-URL construction (R4 item 1 hoist; the 443-omitted form was
// previously covered only by the deleted legacy-Redirect test): port 443 is
// the TLS default and omitted, every other granted port is spelled out.
func TestDirectURLOmitsDefaultPort(t *testing.T) {
	if got := directURL("demo.sb1.example.com", 443, "code1"); got != "https://demo.sb1.example.com/s/code1" {
		t.Fatalf("directURL(443) = %q, want the port-omitted form", got)
	}
	if got := directURL("demo.sb1.example.com", 8443, "code1"); got != "https://demo.sb1.example.com:8443/s/code1" {
		t.Fatalf("directURL(8443) = %q, want the explicit-port form", got)
	}
	if got := directURL("demo.sb1.example.com", 1, "code1"); got != "https://demo.sb1.example.com:1/s/code1" {
		t.Fatalf("directURL(1) = %q, want the explicit-port form", got)
	}
}

// TestPrepareRouteRequiresCurrentEndpointReport proves the defense-in-depth
// gate for audit Important #3 (round C, finding A2): a successful open_ack
// alone is NOT enough to produce a direct origin. The agent's PERSISTED
// endpoint report must correspond to THIS open — at minimum the granted port
// AND the ack's public IP — so a missing, dropped, out-of-order, or stale
// report can never yield a direct URL. The live predicate and STUN observation
// are fully satisfied in every failure subtest, and the probe would succeed:
// only the persisted report is wrong.
//
// The IP case is the reviewer's scenario: a failed DDNS update for a changed
// public IP leaves the previous row (old endpoint_ip) intact while the ack
// carries the fresh public IP. The port may coincide, so a port-only gate
// passes and the raw-IP probe of the ack's public IP succeeds — yet the
// recipient's DDNS hostname still resolves to the OLD IP, a direct URL that
// cannot serve. The matching-report subtest is the no-false-rejection control.
func TestPrepareRouteRequiresCurrentEndpointReport(t *testing.T) {
	cases := []struct {
		name         string
		reportedPort int
		reportedIP   string
		ackIP        string
		wantDirect   bool
	}{
		{name: "stale previous-open port", reportedPort: 9999, reportedIP: routeDirectIP, ackIP: routeDirectIP},
		{name: "missing report (port 0)", reportedPort: 0, reportedIP: routeDirectIP, ackIP: routeDirectIP},
		{name: "stale previous-open IP (same port)", reportedPort: 8443, reportedIP: routeDirectIP, ackIP: "198.51.100.9"},
		{name: "missing report (empty IP)", reportedPort: 8443, reportedIP: "", ackIP: routeDirectIP},
		{name: "current report (matching port and IP)", reportedPort: 8443, reportedIP: routeDirectIP, ackIP: routeDirectIP, wantDirect: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			app, ctrl, _, view, _ := newSelectionController(t, true)
			attachSTUNListener(t, ctrl, newSTUNTestClock())
			code := "preprep01"
			key := routeSession(t, app, code, nil)
			port := seedAgentFacts(t, app, key, routeDirectIP)
			grantPresence(t, view, agentRecordID(t, app, key), port)
			connectWS(t, ctrl, key)
			t.Cleanup(func() { ctrl.AgentDisconnected(key, nil) })
			installObservation(t, ctrl, key, netip.MustParseAddr(routeDirectIP), true)

			// Overwrite the seeded report with the stale/missing value: the
			// persisted endpoint report no longer corresponds to the ack. The
			// observation still matches the persisted endpoint, so the LIVE
			// selection term is preparable — only the prepare gate can reject.
			rec := agentRow(t, app, key)
			rec.Set("endpoint_port", tc.reportedPort)
			rec.Set("endpoint_ip", tc.reportedIP)
			if err := app.Save(rec); err != nil {
				t.Fatalf("save agent row: %v", err)
			}

			ctrl.emitOpenFn = func(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
				ack := prepareOKAck() // grants 8443 at routeDirectIP
				ack.PublicIP = tc.ackIP
				return ack, nil
			}
			probes := 0
			ctrl.probeFn = func(ctx context.Context, origin, code, apiKeyID string, ack OpenAck) error {
				probes++
				return nil
			}

			resp := prepareCall(t, ctrl, code, "", nil)
			if tc.wantDirect {
				requireNoStore(t, resp)
				if resp.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200 (body %q)", resp.Code, resp.Body.String())
				}
				out := decodePrepareBody(t, resp)
				if out.Status != "direct" {
					t.Fatalf("status = %q, want %q: a report matching the ack port AND IP must yield direct", out.Status, "direct")
				}
				if probes != 1 {
					t.Fatalf("the probe ran %d time(s), want exactly 1 for the matching report", probes)
				}
				return
			}
			assertRelayFallback(t, app, resp, code, key)
			if probes != 0 {
				t.Fatalf("the probe ran %d time(s) without a current endpoint report", probes)
			}
		})
	}
}

package directctl

// Task 22 no-store interstitial tests (plan Task 22; spec §§4.3, 4.4, 6,
// 9.3, 9.4, 18.1). The interstitial is CONTROL's page: it prepares direct via
// the same-origin Task 21 endpoint, asks the browser to CORS-check the direct
// agent origin (GET /s/<code>/connect, four-second AbortController budget),
// navigates by the exact navigation rules (direct success → direct origin
// without touching relay; check miss → relay if available; unavailable →
// 503-style UI + canonical retry), and never loads or embeds content through
// control.
//
// The per-response CSP is constructed from the AUTHENTICATED session record
// (the persisted origin encodes the namespace) — never request input:
// default-src 'none', a unique-per-response nonce bound identically in the
// header and the single external <script>, same-origin preparation, and a
// namespace-scoped HTTPS any-port connect-src for the direct check ONLY. No
// frame/object destinations, no http: destinations, and no HTML URL outside
// the control-derived allowlist ever appears.
//
// The noscript path is the exact control-derived relay fallback (meta
// refresh + visible link) when relay is available under
// RelaySelectionEnabled=true — and unavailable wording + canonical retry
// otherwise (including flag OFF: a flag-off page never contains a relay URL).

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/control/internal/relayctl"
)

// mustInterstitialAssets loads the REAL shipped route-interstitial assets
// (control/web/route-interstitial.{html,js,css}) — the same files
// cmd/server loads at startup and deploy-testing.sh copies. One source of
// truth: the tests assert the exact assets the deployment serves.
func mustInterstitialAssets(t *testing.T) InterstitialAssets {
	t.Helper()
	assets, err := LoadInterstitialAssets("../../web")
	if err != nil {
		t.Fatalf("load route-interstitial assets: %v", err)
	}
	return assets
}

// newInterstitialController is newSelectionController with the real
// interstitial assets injected and the wanted RelaySelectionEnabled state.
func newInterstitialController(t *testing.T, enabled bool) (core.App, *Controller, *stubRevisionSource, *relayctl.PresenceView, *selectionSpy) {
	t.Helper()
	app, ctrl, revision, view, spy := newSelectionController(t, enabled)
	ctrl.cfg.InterstitialAssets = mustInterstitialAssets(t)
	return app, ctrl, revision, view, spy
}

// seedInterstitialDirect seeds the minimal direct-candidate world that lands
// in the §9.1 interstitial arm: an active supported session plus a ready
// current-epoch WS (no STUN observation ⇒ only freshness missing ⇒
// preparable). relayView optional: non-nil grants a relay presence lease so
// the flag-on page embeds the derived relay URL.
func seedInterstitialDirect(t *testing.T, app core.App, ctrl *Controller, relayView *relayctl.PresenceView, code string) string {
	t.Helper()
	apiKeyID := routeSession(t, app, code, nil)
	if relayView != nil {
		port := seedAgentFacts(t, app, apiKeyID, routeDirectIP)
		grantPresence(t, relayView, agentRecordID(t, app, apiKeyID), port)
	}
	connectWS(t, ctrl, apiKeyID)
	return apiKeyID
}

// renderInterstitial drives the exact production dispatch pair for a GET
// canonical navigation: ResolveForRedirect then SelectRoute. Optional
// poisoning is applied to the REQUEST reaching selection (§9.3: the page and
// its CSP must not reflect it).
func renderInterstitial(t *testing.T, ctrl *Controller, code string, poison func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	rec, status := ctrl.ResolveForRedirect(code)
	if status != http.StatusFound {
		t.Fatalf("lifecycle status = %d, want 302 Found (code %s)", status, code)
	}
	req := httptest.NewRequest(http.MethodGet, "/s/"+code, nil)
	if poison != nil {
		poison(req)
	}
	resp := httptest.NewRecorder()
	if err := ctrl.SelectRoute(resp, req, rec, code); err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	return resp
}

var (
	cspNonceRe     = regexp.MustCompile(`script-src 'nonce-([A-Za-z0-9+/_-]+)'`)
	scriptNonceRe  = regexp.MustCompile(`<script[^>]*nonce="([A-Za-z0-9+/_-]+)"[^>]*>`)
	attrSrcRe      = regexp.MustCompile(`\ssrc="([^"]*)"`)
	attrHrefRe     = regexp.MustCompile(`\shref="([^"]*)"`)
	attrDataURLsRe = regexp.MustCompile(`content="0; url=([^"]*)"`)
)

// extractCSPNonce returns the single nonce bound in the CSP header.
func extractCSPNonce(t *testing.T, csp string) string {
	t.Helper()
	m := cspNonceRe.FindStringSubmatch(csp)
	if m == nil {
		t.Fatalf("CSP %q carries no script nonce", csp)
	}
	return m[1]
}

// TestInterstitialNoStoreAndNonceScript pins the response contract: 200
// no-store text/html, exactly one CSP header whose nonce is bound to the ONE
// external script tag (header == tag, no inline script, no DOM event-handler
// attributes), a bounded body, a fresh nonce per response, and lifecycle
// rejection (revoked/unknown → 404) BEFORE any rendering.
func TestInterstitialNoStoreAndNonceScript(t *testing.T) {
	app, ctrl, _, view, _ := newInterstitialController(t, true)
	seedInterstitialDirect(t, app, ctrl, view, "nostr001")

	resp := renderInterstitial(t, ctrl, "nostr001", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", resp.Code, resp.Body.String())
	}
	if cc := resp.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store (§9.3: the page is never cacheable)", cc)
	}
	if ct := resp.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	body := resp.Body.String()
	if len(body) > 32<<10 {
		t.Fatalf("interstitial body is %d bytes, bounded pages must stay under 32 KiB", len(body))
	}

	cspVals := resp.Header().Values("Content-Security-Policy")
	if len(cspVals) != 1 {
		t.Fatalf("Content-Security-Policy headers = %d, want exactly 1 (%q)", len(cspVals), cspVals)
	}
	headerNonce := extractCSPNonce(t, cspVals[0])

	scripts := scriptNonceRe.FindAllStringSubmatch(body, -1)
	if len(scripts) != 1 {
		t.Fatalf("body carries %d nonce'd <script> tags, want exactly 1 (body %q)", len(scripts), body)
	}
	if scripts[0][1] != headerNonce {
		t.Fatalf("script nonce %q != header nonce %q — header and tag must match", scripts[0][1], headerNonce)
	}
	if !strings.Contains(scripts[0][0], `src="/web/route-interstitial.js"`) {
		t.Fatalf("the only script must be the external same-origin asset, got %q", scripts[0][0])
	}
	// No inline script without the nonce and no script content outside the
	// external src: the body must not carry inline JS between script tags.
	if strings.Count(body, "<script") != 1 || strings.Count(body, "</script>") != 1 {
		t.Fatalf("body must carry exactly one script element (found %d/%d)", strings.Count(body, "<script"), strings.Count(body, "</script>"))
	}
	for _, handler := range []string{"onclick=", "onload=", "onerror=", "onmouseover=", "javascript:"} {
		if strings.Contains(body, handler) {
			t.Fatalf("body contains forbidden inline scripting vector %q", handler)
		}
	}

	// Unique per response: a second navigation gets a DIFFERENT nonce.
	resp2 := renderInterstitial(t, ctrl, "nostr001", nil)
	nonce2 := extractCSPNonce(t, resp2.Header().Get("Content-Security-Policy"))
	if nonce2 == headerNonce {
		t.Fatalf("nonce reused across responses (%q) — must be unique per response", nonce2)
	}

	// Lifecycle rejection before rendering: revoked and unknown codes answer
	// 404 through the canonical resolver and never produce a page.
	routeSession(t, app, "revok002", func(r *core.Record) {
		r.Set("is_active", false)
		r.Set("inactive_reason", "revoked")
	})
	if _, status := ctrl.ResolveForRedirect("revok002"); status != http.StatusNotFound {
		t.Fatalf("revoked lifecycle status = %d, want 404", status)
	}
	if rec2, s2 := ctrl.ResolveForRedirect("nosuchcode"); s2 != http.StatusNotFound || rec2 != nil {
		t.Fatalf("unknown code status = %d, want 404", s2)
	}
}

// TestInterstitialCSPAllowsOnlySameOriginAndCurrentNamespaceHTTPSAnyPort pins
// the FULL policy: default-src 'none'; nonce-bound script and style;
// connect-src 'self' plus the CURRENT session namespace's HTTPS any-port
// wildcard (for the direct check ONLY); frame-ancestors/base-uri/form-action
// 'none'; no frame/object/img/media/font/worker destinations, no http:
// anywhere. The namespace scope comes from the persisted session origin —
// never request input, never the agent row.
func TestInterstitialCSPAllowsOnlySameOriginAndCurrentNamespaceHTTPSAnyPort(t *testing.T) {
	app, ctrl, _, _, _ := newInterstitialController(t, true)
	seedInterstitialDirect(t, app, ctrl, nil, "csp00001")

	resp := renderInterstitial(t, ctrl, "csp00001", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}
	csp := resp.Header().Get("Content-Security-Policy")
	nonce := extractCSPNonce(t, csp)

	want := "default-src 'none'; script-src 'nonce-" + nonce +
		"'; style-src 'nonce-" + nonce +
		"'; connect-src 'self' https://*.sbdeadbeef.example.com:*" +
		"; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	if csp != want {
		t.Fatalf("CSP = %q\nwant    %q", csp, want)
	}

	// No other resource destinations, no http: destinations, no bare
	// cross-namespace wildcard.
	for _, forbidden := range []string{"frame-src", "object-src", "child-src", "worker-src",
		"manifest-src", "media-src", "font-src", "img-src", "http:", "sbevil"} {
		if strings.Contains(csp, forbidden) {
			t.Fatalf("CSP %q must not contain %q", csp, forbidden)
		}
	}

	// A session in a DIFFERENT namespace scopes the wildcard to ITS namespace.
	otherKey := routeSession(t, app, "csp00002", func(r *core.Record) {
		r.Set("origin", "csp00002.sbfeed000.example.com")
	})
	connectWS(t, ctrl, otherKey)
	resp2 := renderInterstitial(t, ctrl, "csp00002", nil)
	csp2 := resp2.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp2, "connect-src 'self' https://*.sbfeed000.example.com:*") {
		t.Fatalf("second-namespace CSP = %q, want the sbfeed000 scope", csp2)
	}
	if strings.Contains(csp2, "sbdeadbeef") {
		t.Fatalf("second-namespace CSP %q leaks the other namespace", csp2)
	}

	// Request input never moves the scope: poisoned headers/query/host keep
	// the session-derived wildcard. Nonces are unique per response (pinned
	// above), so scope invariance is compared with the per-response nonce
	// normalized out of both policies.
	poison := func(r *http.Request) {
		r.Header.Set("X-Forwarded-Host", "evil.example")
		r.Header.Set("X-Forwarded-Proto", "http")
		r.Host = "evil.example"
		r.URL.RawQuery = "origin=evil.example&namespace=sbevil000&connect=ws://evil.example"
	}
	normalizeNonce := func(policy string) string {
		return strings.ReplaceAll(policy, extractCSPNonce(t, policy), "NONCE")
	}
	resp3 := renderInterstitial(t, ctrl, "csp00001", poison)
	if got := normalizeNonce(resp3.Header().Get("Content-Security-Policy")); got != normalizeNonce(csp) {
		t.Fatalf("poisoned-request CSP = %q, want the unchanged session-derived policy", got)
	}

	// The agent row is not the namespace authority either (§6: persisted
	// session origin is): mutating the agent namespace does not move the CSP.
	// Seed the agent row first (the shared fixtures create it lazily) — the
	// published endpoint keeps the term preparable (only STUN freshness
	// missing), so the re-render still reaches the interstitial arm.
	cspKey := routeSessionAPIKey(t, app, "csp00001")
	seedAgentFacts(t, app, cspKey, routeDirectIP)
	agentRecs, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0,
		map[string]any{"k": cspKey})
	if err != nil || len(agentRecs) == 0 {
		t.Fatalf("agent row: %v", err)
	}
	agentRecs[0].Set("namespace", "sbcsp9999")
	if err := app.Save(agentRecs[0]); err != nil {
		t.Fatalf("mutate agent namespace: %v", err)
	}
	resp4 := renderInterstitial(t, ctrl, "csp00001", nil)
	if got := resp4.Header().Get("Content-Security-Policy"); !strings.Contains(got, "https://*.sbdeadbeef.example.com:*") {
		t.Fatalf("agent-mutated CSP = %q, want the persisted-session namespace scope", got)
	}
}

// routeSessionAPIKey looks up the session's api_key_id by code (test helper).
func routeSessionAPIKey(t *testing.T, app core.App, code string) string {
	t.Helper()
	recs, err := app.FindRecordsByFilter("sessions", "code = {:c}", "", 1, 0, map[string]any{"c": code})
	if err != nil || len(recs) == 0 {
		t.Fatalf("session %s missing: %v", code, err)
	}
	return recs[0].GetString("api_key_id")
}

// TestInterstitialNeverEmbedsContentIframe proves §4.3/§9.3: the page never
// embeds either content origin in an iframe (or object/embed), carries NO
// frame/object destinations in its CSP, and its HTML contains ONLY
// control-derived URLs — the direct origin is never pre-embedded, and
// poisoned request input is never reflected.
func TestInterstitialNeverEmbedsContentIframe(t *testing.T) {
	app, ctrl, _, view, _ := newInterstitialController(t, true)
	seedInterstitialDirect(t, app, ctrl, view, "iframe01")

	poison := func(r *http.Request) {
		r.Header.Set("X-Forwarded-Host", "evil.example")
		r.URL.RawQuery = "url=https://evil.example&iframe=1&origin=" + routeOriginFor("iframe01")
	}
	resp := renderInterstitial(t, ctrl, "iframe01", poison)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}
	body := resp.Body.String()
	lower := strings.ToLower(body)

	for _, forbidden := range []string{"<iframe", "<object", "<embed", "srcdoc", "javascript:"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("interstitial body embeds forbidden element/vector %q", forbidden)
		}
	}
	csp := resp.Header().Get("Content-Security-Policy")
	for _, forbidden := range []string{"frame-src", "object-src"} {
		if strings.Contains(csp, forbidden) {
			t.Fatalf("CSP %q declares a %s destination", csp, forbidden)
		}
	}

	// The direct origin (content origin) is never embedded in the HTML at
	// all: content is only ever reached by navigation or the browser's own
	// connect check, never pre-loaded by control.
	if strings.Contains(body, routeOriginFor("iframe01")) {
		t.Fatalf("body embeds the direct origin %q — content must never load through control", routeOriginFor("iframe01"))
	}

	// Every URL in the page is control-derived. After removing the derived
	// relay URL occurrences, no other https:// or evil.example reference may
	// remain, and no request echo is present.
	relayWant := "https://" + routeRelayOriginFor("iframe01") + "/s/iframe01"
	if !strings.Contains(body, relayWant) {
		t.Fatalf("relay-available page must embed the exact derived relay URL %q", relayWant)
	}
	withoutRelay := strings.ReplaceAll(body, relayWant, "")
	if strings.Contains(withoutRelay, "https://") || strings.Contains(withoutRelay, "evil.example") {
		t.Fatalf("body carries a non-control-derived URL: %q", withoutRelay)
	}

	// src allowlist: only the same-origin interstitial script. href
	// allowlist: stylesheet, the derived relay URL, the canonical retry path.
	for _, m := range attrSrcRe.FindAllStringSubmatch(body, -1) {
		if m[1] != "/web/route-interstitial.js" {
			t.Fatalf("unexpected src destination %q", m[1])
		}
	}
	hrefOK := map[string]bool{
		"/web/route-interstitial.css": true,
		relayWant:                     true,
		"/s/iframe01":                 true,
	}
	for _, m := range attrHrefRe.FindAllStringSubmatch(body, -1) {
		if !hrefOK[m[1]] {
			t.Fatalf("unexpected href destination %q (allowlist: stylesheet, derived relay, canonical retry)", m[1])
		}
	}
	for _, m := range attrDataURLsRe.FindAllStringSubmatch(body, -1) {
		if m[1] != relayWant {
			t.Fatalf("meta-refresh target %q != the exact derived relay URL", m[1])
		}
	}
}

// TestInterstitialNoscriptUsesExactDerivedRelay proves the §9.3 noscript
// contract under RelaySelectionEnabled=true with a live presence lease: the
// page carries the EXACT control-derived relay URL as a <noscript> meta
// refresh plus a visible relay link, and the immediate "Use relay now" action
// wired to that already-returned URL (AbortController cancellation — never a
// new fetch).
func TestInterstitialNoscriptUsesExactDerivedRelay(t *testing.T) {
	app, ctrl, _, view, _ := newInterstitialController(t, true)
	seedInterstitialDirect(t, app, ctrl, view, "noscr001")

	resp := renderInterstitial(t, ctrl, "noscr001", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}
	body := resp.Body.String()

	// The EXACT §6-derived relay URL (persisted session origin only).
	want := "https://" + routeRelayOriginFor("noscr001") + "/s/noscr001"

	headMeta := `<noscript><meta http-equiv="refresh" content="0; url=` + want + `"></noscript>`
	if !strings.Contains(body, headMeta) {
		t.Fatalf("body lacks the exact derived-relay noscript meta refresh %q (body %q)", headMeta, body)
	}
	visibleLink := `<noscript><p><a id="sb-relay-link" href="` + want + `">Open your share via relay</a></p></noscript>`
	if !strings.Contains(body, visibleLink) {
		t.Fatalf("body lacks the visible noscript relay link %q", visibleLink)
	}
	if !strings.Contains(body, `data-relay-url="`+want+`"`) {
		t.Fatalf("body lacks the embedded already-returned relay URL for the manual action")
	}

	// The immediate manual action is present and labeled "Use relay now".
	buttonIdx := strings.Index(body, `id="sb-use-relay"`)
	if buttonIdx < 0 {
		t.Fatalf("relay-available interstitial must offer the immediate \"Use relay now\" action")
	}
	if !strings.Contains(body[buttonIdx:], "Use relay now</button>") {
		t.Fatalf("manual action must be labeled \"Use relay now\" (body %q)", body)
	}

	// The served script aborts the in-flight preparation/direct fetch via
	// AbortController and navigates to the already-embedded relay URL; it
	// contains exactly ONE POST (the preparation) — the manual action never
	// fetches a new route.
	js := string(ctrl.cfg.InterstitialAssets.JS)
	if !strings.Contains(js, "AbortController") {
		t.Fatalf("interstitial JS must cancel preparation/direct fetch via AbortController")
	}
	if !strings.Contains(js, "data-relay-url") {
		t.Fatalf("interstitial JS must navigate to the already-returned relay URL")
	}
	if got := strings.Count(js, `'POST'`); got != 1 {
		t.Fatalf("interstitial JS issues %d POSTs, want exactly 1 (manual relay never fetches a new route)", got)
	}
}

// TestInterstitialWithoutRelayShowsUnavailableAndRetry proves the negative
// arms of the noscript contract: with no selectable relay the page NEVER
// contains a relay URL — the noscript path renders the unavailable wording
// with the canonical retry link, and the "Use relay now" action is absent.
// Both causes are covered: flag ON without a presence lease, and flag OFF
// even WITH a lease (§20 rollback: flag-off pages never carry a relay URL).
func TestInterstitialWithoutRelayShowsUnavailableAndRetry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		lease   bool
	}{
		{name: "flag on, no presence lease", enabled: true, lease: false},
		{name: "flag off, lease present", enabled: false, lease: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, ctrl, _, view, _ := newInterstitialController(t, tc.enabled)
			if tc.lease {
				seedInterstitialDirect(t, app, ctrl, view, "unavail1")
			} else {
				seedInterstitialDirect(t, app, ctrl, nil, "unavail1")
			}

			resp := renderInterstitial(t, ctrl, "unavail1", nil)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (the direct-candidate interstitial still serves)", resp.Code)
			}
			body := resp.Body.String()

			if strings.Contains(body, ".relay.") || strings.Contains(body, "data-relay-url") {
				t.Fatalf("relay-less interstitial must never contain a relay URL: %q", body)
			}
			if strings.Contains(body, "sb-use-relay") || strings.Contains(body, "Use relay now") {
				t.Fatalf("relay-less interstitial must not offer the manual relay action")
			}
			// Noscript: unavailable wording + canonical retry (no-JS direct
			// preparation is deliberately unsupported in the MVP).
			wantRetry := `<noscript><p id="sb-noscript-unavailable">Your share can’t be opened right now. <a id="sb-noscript-retry" href="/s/unavail1">Retry</a></p></noscript>`
			if !strings.Contains(body, wantRetry) {
				t.Fatalf("noscript path must show unavailable wording + exact canonical retry %q (body %q)", wantRetry, body)
			}
			// The JS-driven terminal UI offers the same canonical retry
			// (§9.4: the UI offers/retries through the canonical URL).
			if !strings.Contains(body, `id="sb-unavailable"`) || !strings.Contains(body, `id="sb-retry" href="/s/unavail1"`) {
				t.Fatalf("JS unavailable UI with canonical retry missing (body %q)", body)
			}
		})
	}
}

// TestInterstitialZeroValueAssetsFailClosed pins the T22-m1 guard: a controller
// constructed with the zero-value InterstitialAssets (no LoadInterstitialAssets
// wiring) answers the §9.3 interstitial arm with the 503 unavailable response —
// never a 200 with an empty body. Production fails earlier at startup
// (main.go log.Fatalf); this guard makes the documented fail-closed 503 claim
// true for every construction of the controller.
func TestInterstitialZeroValueAssetsFailClosed(t *testing.T) {
	// The shared harness injects the real assets; clear them to the ZERO
	// value to model a controller constructed without any wiring.
	app, ctrl, _, _, _ := newSelectionController(t, true)
	ctrl.cfg.InterstitialAssets = InterstitialAssets{}
	seedInterstitialDirect(t, app, ctrl, nil, "zeroasset")

	resp := renderInterstitial(t, ctrl, "zeroasset", nil)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %q)", resp.Code, resp.Body.String())
	}
	if cc := resp.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	if body := resp.Body.String(); strings.Contains(body, "<html") || len(body) == 0 {
		t.Fatalf("unavailable response must be the bounded JSON body, got %q", body)
	}
	if loc := resp.Header().Get("Location"); loc != "" {
		t.Fatalf("unavailable response carried Location %q", loc)
	}
}

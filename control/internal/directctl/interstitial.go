package directctl

// Task 22 no-store interstitial renderer (plan Task 22; spec §§4.3, 4.4, 6,
// 9.3, 9.4).
//
// The interstitial is CONTROL's page (§4.3): it renders the preparation UI,
// embeds the already-returned relay URL when relay is selectable, and lets
// the browser-side script do the bounded direct check. It never loads or
// proxies content through control, never embeds either content origin in a
// frame, and never reflects request input: every URL on the page is
// control-derived from the PERSISTED session row.
//
// Security contract pinned here:
//
//   - no-store everywhere (the page is never cacheable, §9.3);
//   - one per-response nonce bound identically in the CSP header and the
//     single external <script> tag (no inline script, no event handlers);
//   - the CSP is constructed from the AUTHENTICATED session namespace — the
//     persisted sessions.origin encodes it — never from request input and
//     never from the agent row: default-src 'none', nonce-bound script and
//     style, same-origin preparation (connect-src 'self') plus the
//     namespace-scoped HTTPS any-port wildcard for the direct check ONLY,
//     frame-ancestors/base-uri/form-action 'none', and no other destination;
//   - relay links and the noscript relay fallback appear ONLY when
//     RelaySelectionEnabled=true AND the §9.2 presence lease backs it;
//     otherwise the page shows the unavailable wording with the canonical
//     retry link (a flag-off page never contains a relay URL);
//   - a persisted origin that is not a derivable direct origin fails closed
//     to 503 (the caller's lifecycle resolution already answered 404/410 —
//     invalid codes never reach rendering, Task 21 conventions).

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

const (
	interstitialHTMLName = "route-interstitial.html"
	interstitialJSName   = "route-interstitial.js"
	interstitialCSSName  = "route-interstitial.css"

	// Asset size bounds: the shipped control-authored assets are a few KiB;
	// anything larger is not ours and fails the load (bounded startup, §14).
	maxInterstitialHTMLBytes = 16 << 10
	maxInterstitialJSBytes   = 32 << 10
	maxInterstitialCSSBytes  = 16 << 10

	// maxInterstitialBodyBytes is the rendered-page bound the page contract
	// is pinned against (§9.3 bounded response); a larger render fails
	// closed to 503 instead of shipping an unbounded page.
	maxInterstitialBodyBytes = 32 << 10
)

// InterstitialAssets holds the three control-owned route-interstitial assets
// loaded once at startup and shared by the renderer and the /web asset
// routes. The tests load the same files (one source of truth: the tests
// assert the exact assets the deployment serves).
type InterstitialAssets struct {
	HTML []byte
	JS   []byte
	CSS  []byte
}

// LoadInterstitialAssets reads and bounds-checks the three route-interstitial
// assets from dir (the control web directory). The HTML template must carry
// every marker the renderer substitutes; a missing marker fails the load so
// a broken deployment is rejected at startup, not per navigation.
func LoadInterstitialAssets(dir string) (InterstitialAssets, error) {
	var assets InterstitialAssets
	var err error
	if assets.HTML, err = loadBoundedFile(filepath.Join(dir, interstitialHTMLName), maxInterstitialHTMLBytes); err != nil {
		return assets, err
	}
	if assets.JS, err = loadBoundedFile(filepath.Join(dir, interstitialJSName), maxInterstitialJSBytes); err != nil {
		return assets, err
	}
	if assets.CSS, err = loadBoundedFile(filepath.Join(dir, interstitialCSSName), maxInterstitialCSSBytes); err != nil {
		return assets, err
	}
	tpl := string(assets.HTML)
	for _, marker := range []string{"{{NONCE}}", "{{CODE}}", "{{RELAY_HEAD_NOSCRIPT}}", "{{RELAY_BODY_NOSCRIPT}}", "{{USE_RELAY_BUTTON}}"} {
		if strings.Count(tpl, marker) == 0 {
			return assets, fmt.Errorf("interstitial template %s missing marker %s", interstitialHTMLName, marker)
		}
	}
	return assets, nil
}

// loadBoundedFile reads path, refusing files above limit (a size cap on the
// read itself, so an oversized asset is rejected rather than truncated).
func loadBoundedFile(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("interstitial asset %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("read interstitial asset %s: %w", filepath.Base(path), err)
	}
	if len(data) > limit {
		return nil, fmt.Errorf("interstitial asset %s is %d bytes, limit %d", filepath.Base(path), len(data), limit)
	}
	return data, nil
}

// newInterstitialNonce generates the per-response CSP nonce: 24 random bytes,
// base64 (RawStdEncoding — the [A-Za-z0-9+/] alphabet CSP nonces expect, no
// padding). Fresh for every response; never derived from request state.
func newInterstitialNonce() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(buf), nil
}

// namespaceDomainFromOrigin derives the namespace scope for the CSP
// connect-src wildcard from the PERSISTED session origin (§6:
// "<label>.<namespace>.<base>" → "<namespace>.<base>"). It never consults
// the agent row (whose namespace column is advisory) or request input. The
// result is validated to a strict hostname charset so it can never inject
// into the CSP header. A persisted origin outside the configured base
// domain fails closed.
func namespaceDomainFromOrigin(origin, baseDomain string) (string, error) {
	dot := strings.Index(origin, ".")
	if dot <= 0 || dot == len(origin)-1 {
		return "", fmt.Errorf("not a direct origin")
	}
	nsDomain := origin[dot+1:]
	if baseDomain == "" || !strings.HasSuffix(nsDomain, "."+baseDomain) {
		return "", fmt.Errorf("origin not under base domain")
	}
	for _, r := range nsDomain {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
		default:
			return "", fmt.Errorf("namespace domain has invalid rune %q", r)
		}
	}
	return nsDomain, nil
}

// buildInterstitialCSP is the §9.3 policy, verbatim:
// default-src 'none'; nonce-bound script and style; connect-src limited to
// same-origin (the preparation call) plus the current session namespace's
// HTTPS any-port wildcard (the direct check ONLY — the mapped port is
// learned after the headers are sent); no frames, objects, base, or form
// destinations; nothing else inherits from default-src 'none'.
func buildInterstitialCSP(nonce, namespaceDomain string) string {
	return "default-src 'none'" +
		"; script-src 'nonce-" + nonce + "'" +
		"; style-src 'nonce-" + nonce + "'" +
		"; connect-src 'self' https://*." + namespaceDomain + ":*" +
		"; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
}

// renderInterstitialPage substitutes the control-derived values into the
// shipped template. code and the relay URL are HTML-escaped (identity for
// well-formed control-derived values, defense for anything unexpected).
// When relayLoc is empty (selection disabled or no presence lease) no relay
// URL, relay button, or relay noscript survives: the page carries only the
// unavailable wording and the canonical retry.
func renderInterstitialPage(assets InterstitialAssets, nonce, code, relayLoc string) (string, error) {
	// relayLoc is HTML-escaped before every attribute embedding: identity for
	// a well-formed control-derived URL, an attribute-boundary defense for
	// anything unexpected in the persisted row.
	var headNoScript, bodyNoScript, useRelay string
	if relayLoc != "" {
		relayAttr := html.EscapeString(relayLoc)
		headNoScript = `<noscript><meta http-equiv="refresh" content="0; url=` + relayAttr + `"></noscript>`
		bodyNoScript = `<noscript><p><a id="sb-relay-link" href="` + relayAttr + `">Open your share via relay</a></p></noscript>`
		useRelay = `<button type="button" id="sb-use-relay" data-relay-url="` + relayAttr + `">Use relay now</button>`
	} else {
		bodyNoScript = `<noscript><p id="sb-noscript-unavailable">Your share can’t be opened right now. <a id="sb-noscript-retry" href="/s/` + html.EscapeString(code) + `">Retry</a></p></noscript>`
	}
	page := strings.NewReplacer(
		"{{NONCE}}", nonce,
		"{{CODE}}", html.EscapeString(code),
		"{{RELAY_HEAD_NOSCRIPT}}", headNoScript,
		"{{RELAY_BODY_NOSCRIPT}}", bodyNoScript,
		"{{USE_RELAY_BUTTON}}", useRelay,
	).Replace(string(assets.HTML))
	if strings.Contains(page, "{{") {
		return "", fmt.Errorf("interstitial render left an unsubstituted marker")
	}
	return page, nil
}

// serveInterstitial answers the §9.3 200 no-store interstitial for a
// lifecycle-active direct candidate. sess is the persisted session record
// (the only origin/namespace authority); the caller's lifecycle resolution
// already rejected invalid codes with 404/410 before rendering. A persisted
// origin that is not a derivable direct origin fails closed to 503.
func (c *Controller) serveInterstitial(w http.ResponseWriter, r *http.Request, sess *core.Record, code string) error {
	origin := sess.GetString("origin")
	namespaceDomain, err := namespaceDomainFromOrigin(origin, c.cfg.BaseDomain)
	if err != nil {
		// Never echo the malformed origin; no usable transport (§9.3).
		return c.unavailable(w)
	}
	nonce, err := newInterstitialNonce()
	if err != nil {
		return fmt.Errorf("interstitial nonce: %w", err)
	}

	// The relay URL on the page is the SAME §9.3 derivation the 302 path and
	// the prepare endpoint use (persisted session origin only), offered only
	// when selection is enabled AND the presence lease currently backs it.
	// relaySelectableFor re-reads the assignment facts with a fresh clock so
	// the embedded URL is as current as the prepare response's would be.
	var relayLoc string
	if c.cfg.RelaySelectionEnabled {
		if loc, err := relayURL(origin, code); err == nil && c.relaySelectableFor(sess.GetString("api_key_id")) {
			relayLoc = loc
		}
	}

	page, err := renderInterstitialPage(c.cfg.InterstitialAssets, nonce, code, relayLoc)
	if err != nil || len(page) > maxInterstitialBodyBytes {
		return c.unavailable(w)
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", buildInterstitialCSP(nonce, namespaceDomain))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, page)
	return nil
}

// ServeInterstitialAsset writes one loaded interstitial asset (the JS or CSS
// the rendered page references by exact same-origin path) as a bounded
// no-store response. The raw HTML template is deliberately never served: the
// page exists only as rendered by serveInterstitial.
func ServeInterstitialAsset(w http.ResponseWriter, data []byte, contentType string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

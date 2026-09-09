package directctl

// Task 24 Phase A drift guard (review follow-up): the §9.3 interstitial CSP
// is hand-ported into e2e/browser/fixture.mjs so the browser gate serves the
// byte-for-byte page the deployed control serves. The port was verbatim-
// identical to buildInterstitialCSP when it landed, but nothing failed
// loudly on drift. This test pins the CSP header string per interstitial
// page variant — rendered through the REAL renderInterstitialPage/
// buildInterstitialCSP path — into control/web/testdata/route-interstitial-
// csp.golden, and e2e/browser/fixture.mjs asserts its port reproduces every
// block at fixture startup. An edit on either side now fails a Go test or
// the fixture startup instead of silently diverging.
//
// Golden updates are deliberate (the repo's golden convention: hand-
// committed testdata, cf. testdata/relay-sync). After an INTENTIONAL policy
// change run
//
//	go test ./internal/directctl -run TestInterstitialCSPGolden -update
//
// review the diff, update the fixture port to match, and commit both
// together. Until the port is updated the browser fixture refuses to start
// with "control golden drifted from fixture port".

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden is the deliberate-regeneration flag: go test ./internal/
// directctl -run TestInterstitialCSPGolden -update. Registered at package
// level so the test binary's flag parse sees it.
var updateGolden = flag.Bool("update", false, "rewrite the interstitial CSP golden (deliberate regeneration)")

const (
	// cspGoldenPath is the golden file, repo-relative from this package:
	// control/web/testdata/route-interstitial-csp.golden.
	cspGoldenPath = "../../web/testdata/route-interstitial-csp.golden"

	// cspGoldenNonce is the fixed nonce pinned into the golden (valid
	// RawStdEncoding base64 — the exact per-response nonce shape production
	// issues). Shared verbatim with e2e/browser/fixture.mjs; a nonce change
	// must land on both sides in one commit.
	cspGoldenNonce = "Z29sZGVuLWNzcC1waW4tbm9uY2UtMDAw"

	// cspGoldenNamespaceDomain is the fixture's namespace scope (NS_DOMAIN
	// in e2e/browser/fixture.mjs): the golden is the shared contract, so it
	// pins the exact namespace the browser gate serves.
	cspGoldenNamespaceDomain = "sb0a1b2c3.example.com"
)

// TestInterstitialCSPGolden renders each interstitial page variant through
// the REAL renderInterstitialPage/buildInterstitialCSP path and compares the
// resulting §9.3 CSP header string against the golden shared with
// e2e/browser/fixture.mjs, failing with a line diff on drift.
//
// The CSP construction is variant-independent today (it never reflects the
// page's relay state or the share code); pinning one block PER VARIANT keeps
// the golden honest if that ever changes and gives the fixture one block per
// variant it serves.
func TestInterstitialCSPGolden(t *testing.T) {
	assets := mustInterstitialAssets(t)

	variants := []struct {
		label    string
		relayLoc string
		code     string
	}{
		// Relay elements present (§9.2 presence lease backs the §9.3 page).
		{label: "relay-available", relayLoc: "https://goldenpin01.relay." + cspGoldenNamespaceDomain + "/s/goldenpin01", code: "goldenpin01"},
		// No selectable relay: the unavailable wording + canonical retry page.
		{label: "relay-unavailable", relayLoc: "", code: "goldenpin02"},
	}

	var b strings.Builder
	b.WriteString("# ShareBridge interstitial CSP golden — Task 24 Phase A drift guard.\n")
	b.WriteString("#\n")
	b.WriteString("# Pins the §9.3 Content-Security-Policy header string that control's\n")
	b.WriteString("# control/internal/directctl buildInterstitialCSP produces for each\n")
	b.WriteString("# interstitial page variant, generated through the real\n")
	b.WriteString("# renderInterstitialPage/buildInterstitialCSP path by\n")
	b.WriteString("# TestInterstitialCSPGolden. e2e/browser/fixture.mjs asserts its\n")
	b.WriteString("# hand-ported buildInterstitialCSP reproduces every block at fixture\n")
	b.WriteString("# startup; drift on either side now fails loudly.\n")
	b.WriteString("#\n")
	b.WriteString("# Shared constants (keep both sides in sync):\n")
	b.WriteString("#   nonce:            " + cspGoldenNonce + "\n")
	b.WriteString("#   namespace domain: " + cspGoldenNamespaceDomain + "  (fixture NS_DOMAIN)\n")
	b.WriteString("#\n")
	b.WriteString("# Regenerate deliberately after an intentional policy change:\n")
	b.WriteString("#   cd control && go test ./internal/directctl -run TestInterstitialCSPGolden -update\n")
	b.WriteString("# then update e2e/browser/fixture.mjs to match and commit both together.\n")

	for _, v := range variants {
		// Drive the real render path so a template/renderer break also fails
		// here; only the CSP header string is pinned.
		page, err := renderInterstitialPage(assets, cspGoldenNonce, v.code, v.relayLoc)
		if err != nil {
			t.Fatalf("render interstitial variant %s: %v", v.label, err)
		}
		if len(page) == 0 {
			t.Fatalf("interstitial variant %s rendered empty", v.label)
		}
		b.WriteString("\n# variant: " + v.label + "\n")
		b.WriteString(buildInterstitialCSP(cspGoldenNonce, cspGoldenNamespaceDomain) + "\n")
	}
	got := b.String()

	path := filepath.Join(".", cspGoldenPath)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create golden directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden updated: %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to write it): %v", path, err)
	}
	if got != string(want) {
		t.Fatalf("interstitial CSP drifted from the golden shared with e2e/browser/fixture.mjs\n"+
			"golden: %s\n"+
			"regenerate only after an intentional policy change (-update), then update the fixture port to match\n"+
			"diff (- golden, + rendered):\n%s",
			path, diffLines(string(want), got))
	}
}

// diffLines renders a compact line diff for the golden failure message. The
// golden is tiny and generated in a fixed block order, so line-by-line
// pairing pinpoints the drifted policy without diff machinery.
func diffLines(want, got string) string {
	wantLines := strings.Split(strings.TrimSuffix(want, "\n"), "\n")
	gotLines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	n := len(wantLines)
	if len(gotLines) > n {
		n = len(gotLines)
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		switch {
		case w == g:
			if w != "" {
				fmt.Fprintf(&b, "  %s\n", w)
			}
		default:
			if i < len(wantLines) {
				fmt.Fprintf(&b, "- %s\n", w)
			}
			if i < len(gotLines) {
				fmt.Fprintf(&b, "+ %s\n", g)
			}
		}
	}
	return b.String()
}

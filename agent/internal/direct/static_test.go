// agent/internal/direct/static_test.go
package direct

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

// TestEmbeddedStaticFSContainsGalleryAssets asserts the go:embed FS carries the
// relocated recipient gallery UI: the HTML page, the rewritten gallery data
// layer, the entry script, the video-buffer warning monitor, the extracted
// stylesheet, the CSP-safe placeholder image, and the lightGallery vendor tree
// (JS, CSS, fonts, loading image).
func TestEmbeddedStaticFSContainsGalleryAssets(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub(staticFS, static): %v", err)
	}

	required := []string{
		"index.html",
		"gallery.js",
		"app.js",
		"videoBufferWarning.js",
		"gallery.css",
		"placeholder.gif",
		"lightgallery/lightgallery.css",
		"lightgallery/lightgallery.es5.min.js",
		"lightgallery/fonts/lg.woff2",
		"lightgallery/images/loading.gif",
	}
	for _, name := range required {
		if _, err := fs.Stat(sub, name); err != nil {
			t.Errorf("embedded FS missing %q: %v", name, err)
		}
	}

	// The rewritten gallery.js must be the URL-factory data layer, not the old
	// binary-frame transport consumer.
	data, err := fs.ReadFile(sub, "gallery.js")
	if err != nil {
		t.Fatalf("read gallery.js: %v", err)
	}
	for _, factory := range []string{"thumbUrl", "previewUrl", "playbackUrl", "assetUrl", "archiveManifestUrl", "archivePartUrl"} {
		if !strings.Contains(string(data), factory) {
			t.Errorf("gallery.js does not reference URL factory %q", factory)
		}
	}

	// The page must not carry inline scripts or onclick handlers (CSP is
	// script-src 'self'; inline scripts and handlers are forbidden).
	page, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if strings.Contains(string(page), "onclick=") {
		t.Errorf("index.html contains inline onclick handler (forbidden by CSP)")
	}
	if strings.Contains(string(page), "<script>") {
		t.Errorf("index.html contains inline <script> block (forbidden by CSP)")
	}
	if !strings.Contains(string(page), `id="gallery-root"`) {
		t.Errorf("index.html missing gallery-root container")
	}
}

// TestStaticGalleryJSContentType asserts the static dispatch serves
// /s/{code}/static/gallery.js under the code-prefixed path (which the Phase-2
// Binder admits) with Content-Type text/javascript.
func TestStaticGalleryJSContentType(t *testing.T) {
	handler := newHandlerServerResolve(t, &fakeResolver{sessions: map[string]*ContentSession{
		"abc": {Membership: map[string]struct{}{}},
	}})

	rr := doRequest(handler, http.MethodGet, "/s/abc/static/gallery.js")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("Content-Type = %q, want text/javascript", ct)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rr.Header().Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'self'") {
		t.Fatalf("Content-Security-Policy = %q, want script-src 'self'", got)
	}
	if !strings.Contains(rr.Body.String(), "thumbUrl") {
		t.Fatalf("gallery.js body does not contain the rewritten data layer")
	}
}

// TestStaticRejectsTraversal asserts the static dispatch fails closed on path
// traversal and directory requests.
func TestStaticRejectsTraversal(t *testing.T) {
	handler := newHandlerServerResolve(t, &fakeResolver{sessions: map[string]*ContentSession{
		"abc": {Membership: map[string]struct{}{}},
	}})

	for _, path := range []string{
		"/s/abc/static/../server.go",
		"/s/abc/static/",
		"/s/abc/static/lightgallery",
	} {
		rr := doRequest(handler, http.MethodGet, path)
		if rr.Code == http.StatusOK {
			t.Fatalf("%s: status = %d, want non-200", path, rr.Code)
		}
	}
}

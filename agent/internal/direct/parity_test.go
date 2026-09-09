// agent/internal/direct/parity_test.go
//
// Task 13 — the browser-level parity suite that gates the v1-transport
// deletion (milestone 3c). It proves the whole 3a direct path end-to-end with
// a test CA and a deterministic fake ContentBackend:
//
//	enroll (namespace + CSR) → cert (issue + install) → register gallery share
//	(snapshot hydration) → open (on-demand port + origin bind) → probe (nonce
//	echo) → /s/{code} (HTML) → /items → /thumb/{id} → /preview/{id} →
//	/asset/{id} → /asset/{id}/playback (multiple 206 seeks) → /archive
//	(manifest) → every /archive/{token}/{part}.
//
// The control-plane "redirect" step (GET /s/{code} → 302 to the agent origin)
// lives in the control directctl package and is covered by that
// suite's redirect/probe tests, which this milestone's gate also runs.
//
// The headless-browser test asserts zero CSP violations (and zero uncaught JS
// exceptions) across gallery load, lightbox open, video slide, and slide
// navigation. It skips cleanly when Chrome/Chromium is unavailable or under
// -short, so the unit gate stays green on machines without a browser.
package direct

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/big"
	"math/bits"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"sharebridge/agent/internal/cert"
	"sharebridge/agent/internal/immich"
)

// ---------------------------------------------------------------------------
// Deterministic fake ContentBackend
// ---------------------------------------------------------------------------

// parityVideoLen is the authoritative transcoded length the playback handler
// reports and streams (honoring byte ranges).
const parityVideoLen = 1024

// parityVideoBytes is a deterministic video body: byte i is (i % 251), so a
// range seek can be verified byte-for-byte against the offset.
var parityVideoBytes = func() []byte {
	b := make([]byte, parityVideoLen)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}()

// parityThumbPNG / parityPreviewPNG are tiny but VALID PNGs so the browser can
// decode them. A valid image is required for lightGallery's zoom-from-origin
// open animation to measure the thumbnail and complete (an invalid image would
// leave the lightbox stuck pre-open and its toolbar/next at opacity 0).
var (
	parityThumbPNG   = solidPNG(8, 8, color.RGBA{R: 0xd4, G: 0x33, B: 0x33, A: 0xff})
	parityPreviewPNG = solidPNG(8, 8, color.RGBA{R: 0x2a, G: 0x5a, B: 0x9a, A: 0xff})
)

func solidPNG(w, h int, c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err) // never fails for an in-memory RGBA image
	}
	return buf.Bytes()
}

// parityGallery returns the three deterministic gallery items: two images and
// one video (so the browser test can open a video slide).
func parityGallery() immich.Gallery {
	dur := 12.5
	return immich.Gallery{
		AlbumName:        "Parity Album",
		AlbumDescription: "parity suite fixture",
		Items: []immich.GalleryItem{
			{ID: "img-1", Name: "one.jpg", MimeType: "image/jpeg", Width: 640, Height: 480, Size: 7, SHA1: "1111111111111111111111111111111111111111"},
			{ID: "vid-1", Name: "movie.mp4", MimeType: "video/mp4", Width: 1280, Height: 720, Size: int64(parityVideoLen), Duration: &dur, SHA1: "2222222222222222222222222222222222222222"},
			{ID: "img-2", Name: "two.png", MimeType: "image/png", Width: 320, Height: 240, Size: 7, SHA1: "3333333333333333333333333333333333333333"},
		},
	}
}

// parityBackend is a fully deterministic ContentBackend: thumbnails/previews/
// originals stream a fixed per-id body, the video streams the parityVideoBytes
// pattern (honoring a start offset), and the album download exposes two
// archive parts over the gallery's asset IDs.
type parityBackend struct{}

func (parityBackend) ListGallery(context.Context) (immich.Gallery, error) {
	return parityGallery(), nil
}

func (parityBackend) ThumbnailInfo(_ context.Context, id string) (string, int64, bool, error) {
	return "image/png", int64(len(parityThumbPNG)), true, nil
}
func (parityBackend) GetThumbnail(_ context.Context, id string, w io.Writer) (int64, error) {
	return writeAll(w, parityThumbPNG)
}

func (parityBackend) PreviewInfo(_ context.Context, id string) (string, int64, bool, error) {
	return "image/png", int64(len(parityPreviewPNG)), true, nil
}
func (parityBackend) GetPreview(_ context.Context, id string, w io.Writer) (int64, error) {
	return writeAll(w, parityPreviewPNG)
}

func (parityBackend) GetAssetInfo(_ context.Context, id string) (immich.Asset, error) {
	switch id {
	case "img-1":
		return immich.Asset{OriginalFileName: "one.jpg", OriginalMimeType: "image/jpeg", ExifInfo: &immich.ExifInfo{FileSizeInByte: int64(len("asset-" + id))}}, nil
	case "img-2":
		return immich.Asset{OriginalFileName: "two.png", OriginalMimeType: "image/png", ExifInfo: &immich.ExifInfo{FileSizeInByte: int64(len("asset-" + id))}}, nil
	case "vid-1":
		return immich.Asset{OriginalFileName: "movie.mp4", OriginalMimeType: "video/mp4", ExifInfo: &immich.ExifInfo{FileSizeInByte: int64(parityVideoLen)}}, nil
	default:
		return immich.Asset{}, &immich.NotFoundError{Detail: "asset " + id}
	}
}
func (parityBackend) GetFile(_ context.Context, id string, w io.Writer) (int64, error) {
	return writeAll(w, []byte("asset-"+id))
}

func (parityBackend) PlaybackInfo(_ context.Context, id string) (int64, bool, error) {
	return int64(parityVideoLen), true, nil
}
func (parityBackend) GetVideoPlayback(_ context.Context, id string, w io.Writer) (int64, error) {
	return writeAll(w, parityVideoBytes)
}
func (parityBackend) GetVideoPlaybackRange(_ context.Context, id string, startOffset int64, w io.Writer) (int64, error) {
	if startOffset < 0 || startOffset > int64(len(parityVideoBytes)) {
		return 0, &immich.UpstreamError{Status: http.StatusRequestedRangeNotSatisfiable}
	}
	return writeAll(w, parityVideoBytes[startOffset:])
}
func (parityBackend) HeadVideoPlayback(_ context.Context, id string) (int64, error) {
	return int64(parityVideoLen), nil
}

func (parityBackend) GetAlbumDownloadInfo(context.Context) (immich.AlbumDownload, error) {
	return immich.AlbumDownload{
		AlbumName: "Parity Album",
		Archives: []immich.DownloadArchive{
			{AssetIDs: []string{"img-1", "vid-1"}, Size: 100},
			{AssetIDs: []string{"img-2"}, Size: 50},
		},
	}, nil
}
func (parityBackend) DownloadArchive(_ context.Context, assetIDs []string, w io.Writer) (int64, error) {
	return writeAll(w, []byte("zip:"+strings.Join(assetIDs, ",")))
}

var _ ContentBackend = (*parityBackend)(nil)

func writeAll(w io.Writer, b []byte) (int64, error) {
	n, err := w.Write(b)
	return int64(n), err
}

// ---------------------------------------------------------------------------
// Test CA + enrollment helpers (mirrors the daemon's e2e harness, in-package)
// ---------------------------------------------------------------------------

// parityTestCA mints a self-signed CA root and returns it (with its key) plus
// a trust pool containing only that root.
func parityTestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, *x509.CertPool) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "parity-test-root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return root, rootKey, roots
}

// parityCSRPublicKey recovers the agent-held public key from a CSR PEM so a
// short-lived leaf can be minted for it (the private key never leaves the
// cert.Manager).
func parityCSRPublicKey(t *testing.T, csrPEM []byte) *ecdsa.PublicKey {
	t.Helper()
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("bad CSR PEM block: %+v", block)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("CSR public key is %T", csr.PublicKey)
	}
	return pub
}

// parityMintLeaf issues a leaf signed by the test CA carrying the exact two
// wildcard SANs ValidateChain requires (no extras).
func parityMintLeaf(t *testing.T, root *x509.Certificate, rootKey *ecdsa.PrivateKey, pub *ecdsa.PublicKey, ns, base string) []byte {
	t.Helper()
	sans := []string{
		fmt.Sprintf("*.%s.%s", ns, base),
		fmt.Sprintf("*.relay.%s.%s", ns, base),
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: sans[0]},
		DNSNames:     sans,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leafTmpl, root, pub, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// parityMapper is an in-memory PortMapper so the on-demand port can be opened
// without a router.
type parityMapper struct {
	mu    sync.Mutex
	ip    string
	added int
}

func (m *parityMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	m.mu.Lock()
	m.added++
	m.mu.Unlock()
	return ext, nil
}
func (m *parityMapper) DeletePortMapping(int) error              { return nil }
func (m *parityMapper) ExternalIP() (string, error)              { return m.ip, nil }
func (m *parityMapper) ListPortMappings() ([]PortMapping, error) { return nil, nil }
func (m *parityMapper) InternalIP() string                       { return "192.168.1.20" }

// ---------------------------------------------------------------------------
// Full-stack harness: enroll → cert → register → open, then a live TLS server
// ---------------------------------------------------------------------------

type parityHarness struct {
	ns       string
	base     string
	code     string
	origin   string
	gate     *SignalGate
	sm       *SnapshotManager
	registry *ResolverRegistry
	srv      *DirectServer
	port     *OnDemandPort
	ts       *httptest.Server
	client   *http.Client
	roots    *x509.CertPool
}

func newParityHarness(t *testing.T) *parityHarness {
	t.Helper()
	return newParityHarnessWithBackend(t, &parityBackend{})
}

// newParityHarnessWithBackend is newParityHarness with a caller-chosen
// ContentBackend (Task 24: the video-backed backend for the relay-origin
// seek case). Everything else — enroll → cert → register → open over the
// real cert manager, snapshot manager, on-demand port and direct server —
// is identical to the Phase 3 harness.
func newParityHarnessWithBackend(t *testing.T, backend ContentBackend) *parityHarness {
	t.Helper()
	const ns = "parityns"
	const base = "example.com"
	const code = "gallery1"
	dir := t.TempDir()

	// enroll → cert: namespace assignment, CSR, issue a leaf under the test CA,
	// and install (exercising the real ValidateChain path).
	root, rootKey, roots := parityTestCA(t)
	cm := cert.NewManager(dir, base, roots)
	if err := cm.SetNamespace(ns); err != nil {
		t.Fatal(err)
	}
	csrPEM, err := cm.GenerateCSR()
	if err != nil {
		t.Fatal(err)
	}
	leafDER := parityMintLeaf(t, root, rootKey, parityCSRPublicKey(t, csrPEM), ns, base)
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})...)
	if err := cm.Install(chain); err != nil {
		t.Fatalf("install chain: %v", err)
	}

	// register gallery share: hydrate the snapshot (ListGallery → membership +
	// ledger + archive registry) and register the code.
	sm := NewSnapshotManager(backend, 5, time.Minute)
	if err := sm.Build(context.Background()); err != nil {
		t.Fatalf("build snapshot: %v", err)
	}
	registry := NewResolverRegistry()
	registry.Put(code, sm)

	// open: on-demand port over a fake mapper + origin bind (the daemon's
	// bindOrigin) + the open-signal gate.
	mapper := &parityMapper{ip: "203.0.113.7"}
	port := NewOnDemandPortOwned(mapper, 443, 8443, time.Minute, "parity-test", "192.168.1.20")
	gate := NewSignalGate("parity-agent", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(ns, base, port, cm, gate, 1<<20)
	srv.SetResolver(registry)

	origin := "demo." + ns + "." + base
	if err := srv.Binder().Allow(origin, RouteDirect, code); err != nil {
		t.Fatal(err)
	}
	if err := port.OpenFor(code, time.Minute); err != nil {
		t.Fatalf("open port: %v", err)
	}

	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)

	return &parityHarness{
		ns: ns, base: base, code: code, origin: origin,
		gate: gate, sm: sm, registry: registry, srv: srv, port: port,
		ts: ts, roots: roots, client: parityTLSClient(roots, origin, ts.Listener.Addr().String()),
	}
}

// parityTLSClient dials the in-process listener regardless of the request
// host, presenting SNI = origin and verifying against the test CA.
func parityTLSClient(roots *x509.CertPool, origin, addr string) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName: origin,
			RootCAs:    roots,
			NextProtos: []string{"http/1.1"},
		},
		ForceAttemptHTTP2: false,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	return &http.Client{Transport: transport}
}

// get issues GET for path over the direct origin and returns the response
// (callers own the body).
func (h *parityHarness) get(t *testing.T, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "https://"+h.origin+path, nil)
	req.Host = h.origin
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// getRange is get with a Range header.
func (h *parityHarness) getRange(t *testing.T, path, rng string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "https://"+h.origin+path, nil)
	req.Host = h.origin
	req.Header.Set("Range", rng)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s (Range %s): %v", path, rng, err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Test 1: the full direct path (always runs — deterministic, no browser)
// ---------------------------------------------------------------------------

func TestParityFullDirectPath(t *testing.T) {
	h := newParityHarness(t)

	// ---- open → probe (the control plane's pre-redirect liveness check) ----
	if err := h.gate.Admit(OpenSignal{
		Version:   signalVersion,
		AgentID:   "parity-agent",
		ShareID:   h.code,
		RouteKind: RouteDirect,
		Nonce:     "nonce-1",
		Seq:       1,
		ExpiresAt: time.Now().Add(time.Minute),
		Lease:     30 * time.Second,
	}); err != nil {
		t.Fatalf("admit nonce: %v", err)
	}
	probe := h.get(t, "/s/"+h.code+"/probe?nonce=nonce-1")
	if probe.StatusCode != http.StatusOK {
		t.Fatalf("probe status = %d, want 200", probe.StatusCode)
	}
	if body := readBody(t, probe); body != "nonce-1" {
		t.Fatalf("probe echo = %q, want nonce-1", body)
	}
	if !h.port.Open() {
		t.Fatal("on-demand port is not open after OpenFor")
	}

	// ---- /s/{code} (HTML page) ----
	page := h.get(t, "/s/"+h.code)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d, want 200", page.StatusCode)
	}
	if ct := page.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("page Content-Type = %q, want text/html", ct)
	}
	if csp := page.Header.Get("Content-Security-Policy"); csp != contentSecurityPolicy {
		t.Fatalf("page CSP = %q, want %q", csp, contentSecurityPolicy)
	}
	pageBody := readBody(t, page)
	if !strings.Contains(pageBody, `id="gallery-root"`) {
		t.Fatal("page missing gallery root")
	}
	if !strings.Contains(pageBody, `<base href="/s/`+h.code+`/">`) {
		t.Fatalf("page missing code-substituted base:\n%s", pageBody)
	}

	// ---- /items ----
	itemsResp := h.get(t, "/s/"+h.code+"/items")
	if itemsResp.StatusCode != http.StatusOK {
		t.Fatalf("items status = %d, want 200", itemsResp.StatusCode)
	}
	var items itemsResponse
	if err := json.NewDecoder(itemsResp.Body).Decode(&items); err != nil {
		itemsResp.Body.Close()
		t.Fatalf("decode items: %v", err)
	}
	itemsResp.Body.Close()
	if items.AlbumName != "Parity Album" || items.AlbumDescription != "parity suite fixture" {
		t.Fatalf("album = %q/%q", items.AlbumName, items.AlbumDescription)
	}
	if len(items.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(items.Items))
	}
	for i, wantID := range []string{"img-1", "vid-1", "img-2"} {
		if items.Items[i].ID != wantID {
			t.Fatalf("items[%d].ID = %q, want %q", i, items.Items[i].ID, wantID)
		}
	}
	if items.Items[1].MimeType != "video/mp4" || items.Items[1].Duration == nil {
		t.Fatalf("video item = %+v, want video/mp4 with duration", items.Items[1])
	}

	// ---- /thumb, /preview, /asset for each image ----
	for _, id := range []string{"img-1", "img-2"} {
		th := h.get(t, "/s/"+h.code+"/thumb/"+id)
		thBody := readBody(t, th)
		if th.StatusCode != http.StatusOK || thBody != string(parityThumbPNG) {
			t.Fatalf("thumb %s: status=%d, body=%d bytes", id, th.StatusCode, len(thBody))
		}
		if ct := th.Header.Get("Content-Type"); ct != "image/png" {
			t.Fatalf("thumb %s Content-Type = %q, want image/png", id, ct)
		}

		pv := h.get(t, "/s/"+h.code+"/preview/"+id)
		pvBody := readBody(t, pv)
		if pv.StatusCode != http.StatusOK || pvBody != string(parityPreviewPNG) {
			t.Fatalf("preview %s: status=%d, body=%d bytes", id, pv.StatusCode, len(pvBody))
		}

		as := h.get(t, "/s/"+h.code+"/asset/"+id)
		if as.StatusCode != http.StatusOK || readBody(t, as) != "asset-"+id {
			t.Fatalf("asset %s: status=%d", id, as.StatusCode)
		}
		if cd := as.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
			t.Fatalf("asset %s Content-Disposition = %q, want attachment", id, cd)
		}
	}

	// ---- /asset/{id}/playback: multiple 206 seeks, byte-exact ----
	seeks := []struct {
		rng   string
		start int
		n     int
	}{
		{"bytes=0-99", 0, 100},
		{"bytes=100-199", 100, 100},
		{"bytes=500-599", 500, 100},
		{"bytes=1023-", 1023, 1},
		{"bytes=900-", 900, parityVideoLen - 900},
	}
	for _, s := range seeks {
		r := h.getRange(t, "/s/"+h.code+"/asset/vid-1/playback", s.rng)
		if r.StatusCode != http.StatusPartialContent {
			r.Body.Close()
			t.Fatalf("playback %s status = %d, want 206", s.rng, r.StatusCode)
		}
		wantRange := fmt.Sprintf("bytes %d-%d/%d", s.start, s.start+s.n-1, parityVideoLen)
		if cr := r.Header.Get("Content-Range"); cr != wantRange {
			r.Body.Close()
			t.Fatalf("playback %s Content-Range = %q, want %q", s.rng, cr, wantRange)
		}
		if cl := r.Header.Get("Content-Length"); cl != fmt.Sprintf("%d", s.n) {
			r.Body.Close()
			t.Fatalf("playback %s Content-Length = %q, want %d", s.rng, cl, s.n)
		}
		if ar := r.Header.Get("Accept-Ranges"); ar != "bytes" {
			r.Body.Close()
			t.Fatalf("playback %s Accept-Ranges = %q, want bytes", s.rng, ar)
		}
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			t.Fatalf("playback %s read: %v", s.rng, err)
		}
		if !bytes.Equal(b, parityVideoBytes[s.start:s.start+s.n]) {
			t.Fatalf("playback %s bytes mismatch (len=%d, want %d)", s.rng, len(b), s.n)
		}
	}

	// ---- /archive (manifest) ----
	manifestResp := h.get(t, "/s/"+h.code+"/archive")
	if manifestResp.StatusCode != http.StatusOK {
		t.Fatalf("archive manifest status = %d, want 200", manifestResp.StatusCode)
	}
	var manifest archiveManifestResponse
	if err := json.NewDecoder(manifestResp.Body).Decode(&manifest); err != nil {
		manifestResp.Body.Close()
		t.Fatalf("decode manifest: %v", err)
	}
	manifestResp.Body.Close()
	if manifest.Token == "" {
		t.Fatal("manifest token is empty")
	}
	if len(manifest.Parts) != 2 {
		t.Fatalf("manifest parts = %d, want 2", len(manifest.Parts))
	}
	if manifest.Parts[0].Index != 0 || manifest.Parts[0].Name != "Parity Album-part-1.zip" {
		t.Fatalf("part 0 = %+v", manifest.Parts[0])
	}
	if got := strings.Join(manifest.Parts[0].AssetIDs, ","); got != "img-1,vid-1" {
		t.Fatalf("part 0 assetIds = %q, want img-1,vid-1", got)
	}
	if manifest.Parts[1].Index != 1 || manifest.Parts[1].Name != "Parity Album-part-2.zip" {
		t.Fatalf("part 1 = %+v", manifest.Parts[1])
	}

	// ---- every /archive/{token}/{part} ----
	for _, part := range manifest.Parts {
		path := fmt.Sprintf("/s/%s/archive/%s/%d", h.code, manifest.Token, part.Index)
		r := h.get(t, path)
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			t.Fatalf("archive part %d status = %d, want 200", part.Index, r.StatusCode)
		}
		want := "zip:" + strings.Join(part.AssetIDs, ",")
		if body := readBody(t, r); body != want {
			t.Fatalf("archive part %d body = %q, want %q", part.Index, body, want)
		}
	}

	// ---- download accounting: original assets + archive both count (§11.1) ----
	// Two original assets (img-1, img-2) were downloaded above and the archive
	// parts commit exactly once, so the ledger must show 3 committed downloads
	// and no dangling reservations.
	session, err := h.sm.Resolve()
	if err != nil {
		t.Fatalf("resolve after parts: %v", err)
	}
	if got := session.Ledger.Downloads(); got != 3 {
		t.Fatalf("committed downloads = %d, want 3 (2 assets + 1 archive)", got)
	}
	if got := session.Ledger.Reservations(); got != 0 {
		t.Fatalf("reservations after commit = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Test 2: deterministic CSP gate (always runs — no browser required)
// ---------------------------------------------------------------------------

func TestParityCSPStatic(t *testing.T) {
	h := newParityHarness(t)

	// The served page must carry the exact §4.8 CSP and no inline constructs
	// that script-src 'self' forbids.
	page := h.get(t, "/s/"+h.code)
	if csp := page.Header.Get("Content-Security-Policy"); csp != contentSecurityPolicy {
		page.Body.Close()
		t.Fatalf("CSP = %q, want %q", csp, contentSecurityPolicy)
	}
	pageBody := readBody(t, page)
	for _, forbidden := range []string{"onclick=", "onload=", "onerror=", "<script>", "javascript:", "data:"} {
		if strings.Contains(pageBody, forbidden) {
			t.Fatalf("served page contains forbidden %q (CSP script-src 'self')", forbidden)
		}
	}

	// The JS entry points are served same-origin and must not reference
	// non-'self' URI schemes (inline *attribute* handlers are an HTML concern;
	// the scripts use addEventListener, not inline handlers, and may mention
	// "onerror=" etc. in comments without violating CSP).
	for _, name := range []string{"app.js", "gallery.js", "videoBufferWarning.js"} {
		r := h.get(t, "/s/"+h.code+"/static/"+name)
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			t.Fatalf("static %s status = %d, want 200", name, r.StatusCode)
		}
		body := readBody(t, r)
		for _, forbidden := range []string{"javascript:", "data:", "https://", "http://"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("static %s contains forbidden %q", name, forbidden)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Test 3: headless-browser zero-CSP-violation check (skips without a browser)
// ---------------------------------------------------------------------------

func parityChromePath() string {
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
		"google-chrome",
		"chromium",
		"chromium-browser",
	}
	for _, candidate := range candidates {
		p, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		// Validate the candidate actually launches: a homebrew "chromium"
		// symlink can point at a missing .app (the wrapper script execs a
		// nonexistent binary), which would fail only at browser start.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = exec.CommandContext(ctx, p, "--version").Run()
		cancel()
		if err == nil {
			return p
		}
	}
	return ""
}

// waitUntil polls cond (which may drive chromedp over ctx) until it returns
// true, the timeout elapses, or ctx is cancelled.
func waitUntil(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}

func TestParityBrowserZeroCSPViolations(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser parity test in -short mode")
	}
	chromePath := parityChromePath()
	if chromePath == "" {
		t.Skip("headless Chrome/Chromium not available")
	}

	h := newParityHarness(t)

	// The browser connects to the test CA over the ephemeral listener: the
	// origin is resolved to loopback and the CA is trusted via flags (no
	// system trust-store mutation).
	port := h.ts.Listener.Addr().(*net.TCPAddr).Port
	galleryURL := fmt.Sprintf("https://%s:%d/s/%s", h.origin, port, h.code)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("ignore-certificate-errors", true),
		chromedp.Flag("host-resolver-rules", "MAP "+h.origin+" 127.0.0.1"),
		chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	var (
		mu         sync.Mutex
		violations []string
		exceptions []string
	)
	chromedp.ListenTarget(browserCtx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *log.EventEntryAdded:
			text := strings.ToLower(ev.Entry.Text)
			if strings.Contains(text, "content security policy") ||
				strings.Contains(text, "refused to") ||
				strings.Contains(text, "violates the following") {
				mu.Lock()
				violations = append(violations, ev.Entry.Text)
				mu.Unlock()
			}
		case *runtime.EventExceptionThrown:
			mu.Lock()
			exceptions = append(exceptions, ev.ExceptionDetails.Text)
			mu.Unlock()
		}
	})

	ctx, cancel := context.WithTimeout(browserCtx, 90*time.Second)
	defer cancel()

	// Enable the Log + Runtime domains BEFORE navigation so violations during
	// initial load are captured.
	enable := chromedp.ActionFunc(func(ctx context.Context) error {
		if err := log.Enable().Do(ctx); err != nil {
			return err
		}
		return runtime.Enable().Do(ctx)
	})
	if err := chromedp.Run(ctx, enable, chromedp.Navigate(galleryURL)); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// gallery load: all 3 thumbnails render (proves app.js ran, /items was
	// fetched, and the grid was injected).
	if !waitUntil(ctx, 30*time.Second, func() bool {
		var n int
		_ = chromedp.Run(ctx, chromedp.Evaluate(`document.querySelectorAll('#gallery-root .gallery-item').length`, &n))
		return n == 3
	}) {
		t.Fatal("gallery did not render 3 items")
	}

	// lightbox open on the video slide.
	if err := chromedp.Run(ctx, chromedp.Click(`#gallery-root .gallery-item[data-gallery-id="vid-1"]`, chromedp.ByQuery)); err != nil {
		t.Fatalf("open lightbox: %v", err)
	}

	// video slide: the lightbox must inject a <video> streaming the playback
	// endpoint.
	var videoSrc string
	if !waitUntil(ctx, 15*time.Second, func() bool {
		var src string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(function(){var v=document.querySelector('.lg-current video.lg-video');return v?v.src:'';})()`, &src))
		if src == "" {
			return false
		}
		videoSrc = src
		return true
	}) {
		t.Fatal("video slide did not open in the lightbox")
	}
	if !strings.Contains(videoSrc, "/asset/vid-1/playback") {
		t.Fatalf("video src = %q, want .../asset/vid-1/playback", videoSrc)
	}

	// video visibility: the injected <video> must render inside the viewport.
	// Regression: a `position:relative` override on .lg-img-wrap downgraded
	// lightGallery's `position:absolute` slide wrapper, pushing the video
	// ~1359px off-screen while audio still played (the element existed and
	// streamed, but had a non-visible bounding rect).
	var videoRect struct {
		Top    float64
		Width  float64
		Height float64
	}
	var viewportHeight float64
	_ = chromedp.Run(ctx,
		chromedp.Evaluate(`(function(){var v=document.querySelector('.lg-current video.lg-video');if(!v)return null;var r=v.getBoundingClientRect();return {Top:r.top,Width:r.width,Height:r.height};})()`, &videoRect),
		chromedp.Evaluate(`window.innerHeight`, &viewportHeight),
	)
	if videoRect.Width <= 0 || videoRect.Height <= 0 {
		t.Fatalf("video has zero rendered size: %+v", videoRect)
	}
	if videoRect.Top < 0 || videoRect.Top >= viewportHeight {
		t.Fatalf("video rendered off-screen: top=%v viewportHeight=%v", videoRect.Top, viewportHeight)
	}

	// Let lightGallery finish its open animation (the first slide's busy flag
	// resets ~500ms after open) before navigating.
	time.Sleep(1500 * time.Millisecond)

	// navigation: the video slide is index 1 (lightGallery's 1-based counter
	// shows "2"); advance to the next slide (index 2 → counter "3").
	var counter string
	if !waitUntil(ctx, 15*time.Second, func() bool {
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(function(){var c=document.querySelector('.lg-counter-current');return c?c.textContent:'';})()`, &counter))
		return counter == "2"
	}) {
		t.Fatalf("lightbox counter = %q, want 2 (video slide) after open", counter)
	}

	// navigation: advance to the next slide with a keyboard arrow (a real user
	// interaction, independent of the toolbar's opacity).
	if err := chromedp.Run(ctx, chromedp.KeyEvent(kb.ArrowRight)); err != nil {
		t.Fatalf("next slide: %v", err)
	}
	if !waitUntil(ctx, 15*time.Second, func() bool {
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(function(){var c=document.querySelector('.lg-counter-current');return c?c.textContent:'';})()`, &counter))
		return counter == "3"
	}) {
		t.Fatalf("next slide did not advance: counter = %q, want 3", counter)
	}

	// zoom: no interactive zoom plugin is bundled; the zoom-from-origin open
	// animation and lightGallery's layout rely on inline style attributes,
	// which style-src 'unsafe-inline' permits. Assert inline styles were
	// applied (a blocked style would have surfaced as a CSP violation below).
	var inlineStyles int
	_ = chromedp.Run(ctx, chromedp.Evaluate(`document.querySelectorAll('.lg-outer [style]').length`, &inlineStyles))
	if inlineStyles == 0 {
		t.Fatal("lightbox applied no inline styles (lightGallery layout did not run)")
	}

	// Final assertion: zero CSP violations and zero uncaught JS exceptions
	// across gallery load, lightbox open, video slide, and navigation.
	mu.Lock()
	v := append([]string(nil), violations...)
	e := append([]string(nil), exceptions...)
	mu.Unlock()
	if len(v) > 0 {
		t.Fatalf("CSP violations detected (%d):\n%s", len(v), strings.Join(v, "\n"))
	}
	if len(e) > 0 {
		t.Fatalf("uncaught JS exceptions (%d):\n%s", len(e), strings.Join(e, "\n"))
	}
}

// ---------------------------------------------------------------------------
// Task 24 — seekable test MP4 (pure Go, no fixtures/deps): a structurally
// valid H.264 baseline file with I_PCM macroblocks, so a real browser parses
// duration + seeks and issues live Range requests against the shipped
// playback handler. Everything above the box writer is plain ISO BMFF.
// ---------------------------------------------------------------------------

// h264Bits is a minimal RBSP bit writer: raw big-endian bits plus the
// Exp-Golomb codewords the H.264 syntax uses.
type h264Bits struct {
	buf []byte
	n   uint
}

func (w *h264Bits) bit(b uint) {
	if int(w.n/8) == len(w.buf) {
		w.buf = append(w.buf, 0)
	}
	if b&1 == 1 {
		w.buf[w.n/8] |= 1 << (7 - w.n%8)
	}
	w.n++
}

// u writes the low n bits of val, most-significant first.
func (w *h264Bits) u(val uint32, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		w.bit(uint(val>>uint(i)) & 1)
	}
}

// ue writes an unsigned Exp-Golomb codeword.
func (w *h264Bits) ue(val uint32) {
	x := val + 1
	n := uint(bits.Len32(x)) - 1
	w.u(0, n)
	w.u(x, n+1)
}

// se writes a signed Exp-Golomb codeword.
func (w *h264Bits) se(val int32) {
	var m uint32
	if val <= 0 {
		m = uint32(-val) * 2
	} else {
		m = uint32(val)*2 - 1
	}
	w.ue(m)
}

// trailing writes rbsp_trailing_bits: the stop bit then zero-bit alignment.
// On return the writer is byte-aligned with no partial tail byte.
func (w *h264Bits) trailing() {
	w.bit(1)
	for w.n%8 != 0 {
		w.bit(0)
	}
}

// h264SPS returns a baseline SPS for a 64×64 4:2:0 picture (ISO 14496-10
// 7.3.2.1 — every field below is mandatory in baseline).
func h264SPS() []byte {
	w := &h264Bits{}
	w.u(66, 8)   // profile_idc: baseline
	w.u(0xC0, 8) // constraint_set0/1 flags
	w.u(30, 8)   // level_idc: level 3.0
	w.ue(0)      // seq_parameter_set_id
	w.ue(0)      // log2_max_frame_num_minus4 → 4-bit frame_num
	w.ue(2)      // pic_order_cnt_type 2 (POC derived from frame_num)
	w.ue(1)      // max_num_ref_frames
	w.u(0, 1)    // gaps_in_frame_num_value_allowed_flag
	w.ue(3)      // pic_width_in_mbs_minus1 → 4 MBs = 64 px
	w.ue(3)      // pic_height_in_map_units_minus1 → 4 MBs = 64 px
	w.u(1, 1)    // frame_mbs_only_flag
	w.u(1, 1)    // direct_8x8_inference_flag
	w.u(0, 1)    // frame_cropping_flag
	w.u(0, 1)    // vui_parameters_present_flag
	w.trailing()
	return w.buf
}

// h264PPS returns the matching picture parameter set (7.3.2.2 syntax).
func h264PPS() []byte {
	w := &h264Bits{}
	w.ue(0)   // pic_parameter_set_id
	w.ue(0)   // seq_parameter_set_id
	w.u(0, 1) // entropy_coding_mode_flag: CAVLC (baseline)
	w.u(0, 1) // bottom_field_pic_order_in_frame_present_flag
	w.ue(0)   // num_slice_groups_minus1
	w.ue(0)   // num_ref_idx_l0_default_active_minus1
	w.ue(0)   // num_ref_idx_l1_default_active_minus1
	w.u(0, 1) // weighted_pred_flag
	w.u(0, 2) // weighted_bipred_idc
	w.se(0)   // pic_init_qp_minus26
	w.se(0)   // pic_init_qs_minus26
	w.se(0)   // chroma_qp_index_offset
	w.u(0, 1) // deblocking_filter_control_present_flag
	w.u(0, 1) // constrained_intra_pred_flag
	w.u(0, 1) // redundant_pic_cnt_present_flag
	w.trailing()
	return w.buf
}

// h264IDRFrame encodes one gray I-frame: a single IDR slice of I_PCM
// macroblocks (raw samples — no DCT/CABAC machinery required). All frames
// are byte-identical in length: the constant idr_pic_id keeps the slice
// header a fixed bit count.
func h264IDRFrame(gray byte) []byte {
	w := &h264Bits{}
	w.ue(0)   // first_mb_in_slice
	w.ue(7)   // slice_type: I (all slices)
	w.ue(0)   // pic_parameter_set_id
	w.u(0, 4) // frame_num (log2_max_frame_num = 4)
	w.ue(0)   // idr_pic_id
	// pic_order_cnt_type 2 with frame_mbs_only ⇒ no pic_order_cnt_lsb syntax.
	w.u(0, 1) // dec_ref_pic_marking: no_output_of_prior_pics_flag
	w.u(0, 1) // long_term_reference_flag
	w.se(0)   // slice_qp_delta
	// deblocking_filter_control_present_flag = 0 ⇒ no filter syntax.
	const (
		mbsPerPic    = 16        // 4×4 MBs at 64×64
		samplesPerMB = 256 + 128 // I_PCM 4:2:0: 16×16 luma + 2×8×8 chroma
	)
	for mb := 0; mb < mbsPerPic; mb++ {
		w.ue(25) // mb_type: I_PCM (I-slice)
		for w.n%8 != 0 {
			w.bit(0) // pcm_alignment_zero_bit
		}
		for i := 0; i < samplesPerMB; i++ {
			w.buf = append(w.buf, gray) // raw PCM samples, one byte each
		}
		w.n += samplesPerMB * 8
	}
	w.trailing()
	// IDR slice NAL: nal_ref_idc=3, nal_unit_type=5, then RBSP → EBSP.
	return append([]byte{0x65}, emulationPrevent(w.buf)...)
}

// emulationPrevent inserts emulation_prevention_three_bytes (RBSP → EBSP).
func emulationPrevent(rbsp []byte) []byte {
	var out []byte
	zeros := 0
	for _, b := range rbsp {
		if zeros == 2 && b <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// --- minimal ISO BMFF box writers (all big-endian, version-0 full boxes) ---

func mp4Box(boxType string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], boxType)
	return append(out, payload...)
}

func mp4FullBox(boxType string, version byte, flags uint32, payload []byte) []byte {
	full := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return mp4Box(boxType, append(full, payload...))
}

var mp4UnityMatrix = []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}

func appendU16(dst []byte, v uint16) []byte { return append(dst, byte(v>>8), byte(v)) }
func appendU32(dst []byte, v uint32) []byte {
	return append(dst, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// seekableMP4 is the generated file plus the chunk geometry the stall backend
// needs to serve one chunk per ranged request.
type seekableMP4 struct {
	data      []byte
	dataStart int64 // file offset of chunk 0 (the mdat payload start)
	stride    int64 // bytes between consecutive chunk starts
	chunkLen  int64 // uniform encoded frame size
	frames    int
}

// minWindowBytes is the minimum number of file bytes any served window
// carries. Measured headless-Chromium behavior: the media demuxer only starts
// parsing once its buffer holds roughly one multibuffer block (~32 KiB) — a
// smaller bytes=0- response (header + frame 0 is ~7 KiB) leaves the element at
// HAVE_NOTHING forever (duration never parses), and a smaller seek response
// is likewise never consumed. 64 KiB clears the threshold with margin; the
// extra bytes are mdat padding the sample tables never decode.
const minWindowBytes = 64 << 10

// windowEnd returns the exclusive end of the ONE served window containing
// file offset off: at least minWindowBytes from off (clamped to EOF), and —
// for offsets below the first frame slot (the ftyp+moov header region) —
// through the end of frame slot 0, so a bytes=0- response always carries the
// container metadata the browser parses duration from.
func (m *seekableMP4) windowEnd(off int64) int64 {
	end := off + minWindowBytes
	if off < m.dataStart && m.dataStart+m.chunkLen > end {
		end = m.dataStart + m.chunkLen
	}
	if end > int64(len(m.data)) {
		end = int64(len(m.data))
	}
	return end
}

// servedChunk returns the exact bytes to write for a ranged request whose
// start offset is off: file[off:windowEnd(off)] — the response's first byte
// is the requested offset, and the single bounded window plus the stall
// below keep the browser's buffered region deterministic (never past the
// served window), so any later position forces a fresh ranged request.
func (m *seekableMP4) servedChunk(off int64) []byte {
	end := m.windowEnd(off)
	if end <= off {
		return nil
	}
	return m.data[off:end]
}

// seekableTestMP4 synthesizes a fast-start MP4: `frames` gray I_PCM frames,
// each in its own chunk spaced `stride` bytes apart, so a deep seek is never
// within an already-delivered range and forces a fresh ranged request.
// Layout: ftyp + moov + mdat (8-byte header + frames×stride payload). Chunk
// offsets are absolute file positions; the moov is built twice — first with
// zero offsets to learn the fixed header length (the stco box size is a pure
// function of the frame count), then with the real offsets.
func seekableTestMP4(frames int, stride int) *seekableMP4 {
	if frames <= 0 || stride <= 0 {
		panic("seekableTestMP4: non-positive frames or stride")
	}
	timescale := uint32(1000)
	delta := uint32(125) // 8 fps → (frames×125)/1000 s duration
	duration := uint64(frames) * uint64(delta)
	// avcC carries full NAL units: one header byte (nal_ref_idc=3, unit
	// type 7/8) in front of the SPS/PPS RBSP.
	sps := append([]byte{0x67}, h264SPS()...)
	pps := append([]byte{0x68}, h264PPS()...)
	framesData := make([][]byte, frames)
	for i := range framesData {
		// Each sample is stored avcC-style: 4-byte big-endian NALU length
		// (lengthSizeMinusOne = 3) followed by the full NAL unit.
		nal := h264IDRFrame(byte(40 + i*8))
		sample := appendU32(nil, uint32(len(nal)))
		framesData[i] = append(sample, nal...)
	}

	// stsd → avc1 → avcC (AVCDecoderConfigurationRecord with the SPS/PPS).
	avcC := []byte{1, 66, 0xC0, 30, 0xFF, 0xE1} // v1, base, compat, L3.0, 4-byte NALU lengths, 1 SPS
	avcC = appendU16(avcC, uint16(len(sps)))
	avcC = append(avcC, sps...)
	avcC = append(avcC, 1) // 1 PPS
	avcC = appendU16(avcC, uint16(len(pps)))
	avcC = append(avcC, pps...)
	avc1 := make([]byte, 6)                  // reserved
	avc1 = appendU16(avc1, 1)                // data_reference_index
	avc1 = append(avc1, make([]byte, 16)...) // pre_defined + reserved
	avc1 = appendU16(avc1, 64)               // width
	avc1 = appendU16(avc1, 64)               // height
	avc1 = appendU32(avc1, 0x00480000)       // horizresolution 72 dpi
	avc1 = appendU32(avc1, 0x00480000)       // vertresolution 72 dpi
	avc1 = appendU32(avc1, 0)                // reserved
	avc1 = appendU16(avc1, 1)                // frame_count
	avc1 = append(avc1, make([]byte, 32)...) // compressorname
	avc1 = appendU16(avc1, 0x0018)           // depth
	avc1 = appendU16(avc1, 0xFFFF)           // pre_defined (-1)
	avc1 = append(avc1, mp4Box("avcC", avcC)...)
	stsd := mp4FullBox("stsd", 0, 0, appendU32(nil, 1))
	stsd = append(stsd, mp4Box("avc1", avc1)...)

	stts := mp4FullBox("stts", 0, 0, func() []byte {
		b := appendU32(nil, 1)           // one entry
		b = appendU32(b, uint32(frames)) // sample_count
		return appendU32(b, delta)       // sample_delta
	}())
	stsc := mp4FullBox("stsc", 0, 0, func() []byte {
		b := appendU32(nil, 1) // one entry
		b = appendU32(b, 1)    // first_chunk
		b = appendU32(b, 1)    // samples_per_chunk: one frame per chunk
		return appendU32(b, 1) // sample_description_index
	}())
	stsz := mp4FullBox("stsz", 0, 0, func() []byte {
		b := appendU32(nil, 0) // sample_size 0 = per-sample table
		b = appendU32(b, uint32(frames))
		for _, f := range framesData {
			b = appendU32(b, uint32(len(f)))
		}
		return b
	}())

	buildHeader := func(chunkOffsets []uint32) []byte {
		stco := mp4FullBox("stco", 0, 0, func() []byte {
			b := appendU32(nil, uint32(frames))
			for _, off := range chunkOffsets {
				b = appendU32(b, off)
			}
			return b
		}())
		stbl := mp4Box("stbl", concat(stsd, stts, stsc, stsz, stco))
		minf := mp4Box("minf", concat(
			mp4FullBox("vmhd", 0, 1, func() []byte {
				b := appendU16(nil, 0) // graphicsmode
				b = appendU16(b, 0)   // opcolor r
				b = appendU16(b, 0)   // opcolor g
				return appendU16(b, 0) // opcolor b
			}()),
			mp4Box("dinf", mp4FullBox("dref", 0, 0, func() []byte {
				b := appendU32(nil, 1)                             // entry_count
				return append(b, mp4FullBox("url ", 0, 1, nil)...) // self-contained
			}())),
			stbl,
		))
		trak := mp4Box("trak", concat(
			mp4FullBox("tkhd", 0, 3, func() []byte {
				b := appendU32(nil, 0) // creation
				b = appendU32(b, 0)    // modification
				b = appendU32(b, 1)    // track_ID
				b = appendU32(b, 0)    // reserved
				b = appendU32(b, uint32(duration))
				b = append(b, make([]byte, 8)...) // reserved 2×u32
				b = appendU16(b, 0)               // layer
				b = appendU16(b, 0)               // alternate_group
				b = appendU16(b, 0)               // volume (video track)
				b = appendU16(b, 0)               // reserved
				for _, m := range mp4UnityMatrix {
					b = appendU32(b, m)
				}
				b = appendU32(b, 64<<16)    // width 16.16
				return appendU32(b, 64<<16) // height 16.16
			}()),
			mp4Box("mdia", concat(
				mp4FullBox("mdhd", 0, 0, func() []byte {
					b := appendU32(nil, 0)
					b = appendU32(b, 0)
					b = appendU32(b, timescale)
					b = appendU32(b, uint32(duration))
					b = appendU16(b, 0x55C4) // language 'und'
					return appendU16(b, 0)   // pre_defined
				}()),
				mp4FullBox("hdlr", 0, 0, func() []byte {
					b := appendU32(nil, 0) // pre_defined
					b = append(b, []byte("vide")...)
					b = append(b, make([]byte, 12)...) // reserved 3×u32
					return append(b, []byte("VideoHandler\x00")...)
				}()),
				minf,
			)),
		))
		mvhd := mp4FullBox("mvhd", 0, 0, func() []byte {
			b := appendU32(nil, 0) // creation
			b = appendU32(b, 0)    // modification
			b = appendU32(b, timescale)
			b = appendU32(b, uint32(duration))
			b = appendU32(b, 0x00010000)      // rate 1.0
			b = appendU16(b, 0x0100)          // volume 1.0
			b = append(b, make([]byte, 2)...) // reserved
			b = append(b, make([]byte, 8)...) // reserved 2×u32
			for _, m := range mp4UnityMatrix {
				b = appendU32(b, m)
			}
			b = append(b, make([]byte, 24)...) // pre_defined 6×u32
			return appendU32(b, 2)             // next_track_ID
		}())
		moov := mp4Box("moov", concat(mvhd, trak))
		ftyp := mp4Box("ftyp", func() []byte {
			b := append([]byte(nil), "isom"...)
			b = appendU32(b, 0x200) // minor version
			b = append(b, []byte("isom")...)
			b = append(b, []byte("iso2")...)
			b = append(b, []byte("avc1")...)
			return b
		}())
		return concat(ftyp, moov)
	}

	// First pass with zero offsets to learn the header length (the stco box
	// size is frame-count-fixed, so the length is stable across passes).
	headerLen := len(buildHeader(make([]uint32, frames)))
	mdatStart := int64(headerLen)
	header := buildHeader(rangeU32(frames, func(i int) uint32 {
		return uint32(mdatStart + 8 + int64(i)*int64(stride))
	}))
	if len(header) != headerLen {
		panic("seekableTestMP4: header length moved between passes")
	}

	total := mdatStart + 8 + int64(frames*stride)
	out := make([]byte, total)
	copy(out, header)
	binary.BigEndian.PutUint32(out[mdatStart:], uint32(8+frames*stride))
	copy(out[mdatStart+4:], []byte("mdat"))
	for i, f := range framesData {
		copy(out[mdatStart+8+int64(i*stride):], f)
	}
	return &seekableMP4{
		data: out, dataStart: mdatStart + 8, stride: int64(stride),
		chunkLen: int64(len(framesData[0])), frames: frames,
	}
}

func rangeU32(n int, f func(int) uint32) []uint32 {
	out := make([]uint32, n)
	for i := range out {
		out[i] = f(i)
	}
	return out
}

const (
	relayVideoFrames = 20 // 20 × 125 ms = 2.5 s timeline at 8 fps
	relayVideoStride = 512 << 10
)

// relayVideoMP4 lazily builds the seekable test video (≈10 MB once per test
// binary; tests that never touch playback never pay for it).
var relayVideoMP4 = sync.OnceValue(func() *seekableMP4 {
	return seekableTestMP4(relayVideoFrames, relayVideoStride)
})

// TestSeekableTestMP4Structure pins the generated file's box geometry: the
// top-level boxes parse, every stco offset lands inside the mdat payload on
// its own stride slot, and the encoded frame fits its chunk. Set
// SB_DUMP_SEEKABLE_MP4=<path> to also write the file out for external
// inspection (e.g. ffprobe) — never set by the ordinary gate.
func TestSeekableTestMP4Structure(t *testing.T) {
	if testing.Short() {
		t.Skip("synthesizes a ~10 MB file")
	}
	m := relayVideoMP4()
	if len(m.data) < 8 || string(m.data[4:8]) != "ftyp" {
		t.Fatalf("file does not start with an ftyp box (starts %q)", m.data[:8])
	}
	if size := binary.BigEndian.Uint32(m.data); size != 28 {
		t.Fatalf("ftyp size = %d, want 28", size)
	}
	// Walk the top-level boxes: ftyp + moov must cover the header, then mdat.
	off := int64(0)
	seen := map[string]bool{}
	for off < int64(len(m.data)) {
		if off+8 > int64(len(m.data)) {
			t.Fatalf("truncated box header at %d", off)
		}
		size := int64(binary.BigEndian.Uint32(m.data[off:]))
		typ := string(m.data[off+4 : off+8])
		if size < 8 || off+size > int64(len(m.data)) {
			t.Fatalf("box %q at %d has bad size %d", typ, off, size)
		}
		seen[typ] = true
		if typ == "mdat" {
			if off != m.dataStart-8 {
				t.Fatalf("mdat at %d, want %d (dataStart-8)", off, m.dataStart-8)
			}
		}
		off += size
	}
	for _, want := range []string{"ftyp", "moov", "mdat"} {
		if !seen[want] {
			t.Fatalf("missing top-level box %q (seen: %v)", want, seen)
		}
	}
	// Every chunk offset must be the exact start of its stride slot inside
	// the mdat payload, and the frame must fit with room to spare. The stco
	// offsets are located by walking the moov tree.
	offsets, err := mp4StcoOffsets(m.data)
	if err != nil {
		t.Fatalf("parse stco: %v", err)
	}
	if len(offsets) != m.frames {
		t.Fatalf("stco has %d entries, want %d", len(offsets), m.frames)
	}
	mdatPayloadLen := int64(len(m.data)) - m.dataStart
	for i, off := range offsets {
		want := m.dataStart + int64(i)*m.stride
		if off != want {
			t.Fatalf("stco[%d] = %d, want %d", i, off, want)
		}
		if off+m.chunkLen > m.dataStart+mdatPayloadLen {
			t.Fatalf("chunk %d overruns the mdat payload", i)
		}
	}
	if p := os.Getenv("SB_DUMP_SEEKABLE_MP4"); p != "" {
		if err := os.WriteFile(p, m.data, 0o600); err != nil {
			t.Fatalf("dump: %v", err)
		}
		t.Logf("wrote %d bytes to %s", len(m.data), p)
	}
}

// mp4StcoOffsets walks the box tree (top level + moov/trak/mdia/minf/stbl)
// and returns the chunk offset table from the first stco found.
func mp4StcoOffsets(data []byte) ([]int64, error) {
	var walk func(b []byte, depth int) ([]int64, bool)
	walk = func(b []byte, depth int) ([]int64, bool) {
		if depth > 6 {
			return nil, false
		}
		off := int64(0)
		for off+8 <= int64(len(b)) {
			size := int64(binary.BigEndian.Uint32(b[off:]))
			if size < 8 || off+size > int64(len(b)) {
				return nil, false
			}
			typ := string(b[off+4 : off+8])
			if typ == "stco" {
				// full box header (4) + entry_count (4) + entries.
				n := int(binary.BigEndian.Uint32(b[off+12:]))
				if off+16+4*int64(n) > off+size {
					return nil, false
				}
				out := make([]int64, n)
				for i := 0; i < n; i++ {
					out[i] = int64(binary.BigEndian.Uint32(b[off+16+4*int64(i):]))
				}
				return out, true
			}
			switch typ {
			case "moov", "trak", "mdia", "minf", "stbl":
				if got, ok := walk(b[off+8:off+size], depth+1); ok {
					return got, true
				}
			}
			off += size
		}
		return nil, false
	}
	got, ok := walk(data, 0)
	if !ok {
		return nil, fmt.Errorf("no stco found")
	}
	return got, nil
}

// parityVideoBackend is the Phase 3 deterministic backend with REAL range
// semantics backed by the seekable test MP4. Its range handler serves a
// bounded window per request and then stalls until the request context ends:
// the browser parses duration from the header window but can never buffer
// past it (continuations are stalled empty), so a later currentTime seek is
// guaranteed to issue a fresh ranged request at the deep chunk offset —
// deterministic under any localhost buffering policy or machine load.
type parityVideoBackend struct{ parityBackend }

func (parityVideoBackend) PlaybackInfo(context.Context, string) (int64, bool, error) {
	return int64(len(relayVideoMP4().data)), true, nil
}

func (parityVideoBackend) GetVideoPlayback(_ context.Context, _ string, w io.Writer) (int64, error) {
	return writeAll(w, relayVideoMP4().data)
}

func (parityVideoBackend) GetVideoPlaybackRange(ctx context.Context, _ string, startOffset int64, w io.Writer) (int64, error) {
	vid := relayVideoMP4()
	if startOffset < 0 || startOffset >= int64(len(vid.data)) {
		return 0, &immich.UpstreamError{Status: http.StatusRequestedRangeNotSatisfiable}
	}
	// Deterministic one-window stall (see the type comment): every ranged
	// request — bytes=0- included — receives exactly one minWindowBytes window
	// from its own start offset and then stalls until the request context
	// ends, so the browser can never pull more than one window per request
	// (every further window is a fresh ranged request) and the bytes=0-
	// response carries the container metadata the browser parses duration
	// from.
	chunk := vid.servedChunk(startOffset)
	n, err := w.Write(chunk)
	if err != nil {
		return int64(n), err
	}
	// Deterministic stall: the response stays open (its declared Content-
	// Length is the open range size) until the browser goes away.
	select {
	case <-ctx.Done():
	case <-time.After(120 * time.Second):
	}
	return int64(n), nil
}

var _ ContentBackend = (*parityVideoBackend)(nil)

// newRelayParityHarness is newParityHarnessWithBackend with BOTH origin kinds
// bound for the gallery code: the RouteDirect origin (unchanged) plus the
// RouteRelay hostname the Task 24 case navigates. Returns the harness and the
// relay origin hostname.
func newRelayParityHarness(t *testing.T) (*parityHarness, string) {
	t.Helper()
	h := newParityHarnessWithBackend(t, &parityVideoBackend{})
	relayOrigin := "gallery1.relay." + h.ns + "." + h.base
	if err := h.srv.Binder().Allow(relayOrigin, RouteRelay, h.code); err != nil {
		t.Fatal(err)
	}
	return h, relayOrigin
}

// TestRelayParityBrowserGalleryLightboxVideoSeek206 proves Phase 3 content
// parity over the RELAY origin (plan Task 24 chromedp case): the gallery is
// loaded via its RouteRelay hostname, the video lightbox opens, the browser
// issues an initial bytes=0- playback request answered 206 with the full-file
// Content-Range, later windows of the file are drawn exclusively through
// fresh ranged requests deep in the file — each answered 206 with the exact
// Content-Range for its start — and a later currentTime seek takes effect on
// the element through the relay origin.
func TestRelayParityBrowserGalleryLightboxVideoSeek206(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser parity test in -short mode")
	}
	chromePath := parityChromePath()
	if chromePath == "" {
		t.Skip("headless Chrome/Chromium not available")
	}

	h, relayOrigin := newRelayParityHarness(t)
	port := h.ts.Listener.Addr().(*net.TCPAddr).Port
	galleryURL := fmt.Sprintf("https://%s:%d/s/%s", relayOrigin, port, h.code)
	total := int64(len(relayVideoMP4().data))

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("ignore-certificate-errors", true),
		chromedp.Flag("host-resolver-rules",
			fmt.Sprintf("MAP %s 127.0.0.1, MAP %s 127.0.0.1", relayOrigin, h.origin)),
		chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	// Observe playback requests + responses via the Network domain. Sequence
	// and timestamps are taken at event-delivery time in the single chromedp
	// event goroutine, so the recorded order is consistent.
	var (
		mu    sync.Mutex
		reqs  []playbackReq               // in send order
		resps = map[string]playbackResp{} // requestID → response
	)
	chromedp.ListenTarget(browserCtx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *network.EventRequestWillBeSent:
			if !strings.Contains(ev.Request.URL, "/asset/vid-1/playback") {
				return
			}
			start := int64(0)
			for k, v := range ev.Request.Headers {
				if strings.EqualFold(k, "Range") {
					if s, ok := parseRangeStart(fmt.Sprintf("%v", v)); ok {
						start = s
					}
				}
			}
			mu.Lock()
			reqs = append(reqs, playbackReq{id: ev.RequestID.String(), start: start, at: time.Now()})
			mu.Unlock()
		case *network.EventResponseReceived:
			if !strings.Contains(ev.Response.URL, "/asset/vid-1/playback") {
				return
			}
			r := playbackResp{status: ev.Response.Status}
			for k, v := range ev.Response.Headers {
				if strings.EqualFold(k, "Content-Range") {
					r.contentRange = fmt.Sprintf("%v", v)
				}
			}
			mu.Lock()
			resps[ev.RequestID.String()] = r
			mu.Unlock()
		}
	})

	ctx, cancel := context.WithTimeout(browserCtx, 120*time.Second)
	defer cancel()
	if err := chromedp.Run(ctx, network.Enable()); err != nil {
		t.Fatalf("enable network domain: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Navigate(galleryURL)); err != nil {
		t.Fatalf("navigate relay gallery: %v", err)
	}
	if !waitUntil(ctx, 30*time.Second, func() bool {
		var n int
		_ = chromedp.Run(ctx, chromedp.Evaluate(`document.querySelectorAll('#gallery-root .gallery-item').length`, &n))
		return n == 3
	}) {
		t.Fatal("relay gallery did not render 3 items")
	}
	if err := chromedp.Run(ctx, chromedp.Click(`#gallery-root .gallery-item[data-gallery-id="vid-1"]`, chromedp.ByQuery)); err != nil {
		t.Fatalf("open lightbox: %v", err)
	}

	// The video element must have parsed the container metadata through the
	// RELAY origin (duration comes from the moov — the stream itself stalls
	// at chunk 0, which is exactly what forces the fresh ranged request
	// below).
	if !waitUntil(ctx, 30*time.Second, func() bool {
		var d float64
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(function(){var v=document.querySelector('.lg-current video.lg-video');return v?(v.duration||0):0;})()`, &d))
		return d > 2.0 && d < 3.0
	}) {
		t.Fatal("video metadata never loaded through the relay origin (duration not ~2.5s)")
	}

	// Initial playback request: bytes=0- answered 206 with the full-file
	// Content-Range.
	mu.Lock()
	firstReq, firstResp := firstPlayback(reqs, resps)
	mu.Unlock()
	if firstReq == nil {
		t.Fatal("no initial playback request observed")
	}
	if firstReq.start != 0 {
		t.Fatalf("initial playback range start = %d, want 0", firstReq.start)
	}
	if firstResp.status != http.StatusPartialContent || firstResp.contentRange != fmt.Sprintf("bytes 0-%d/%d", total-1, total) {
		t.Fatalf("initial playback response = %d %q, want 206 with Content-Range \"bytes 0-%d/%d\"",
			firstResp.status, firstResp.contentRange, total-1, total)
	}

	// Deep-range proof: the one-window stall means the browser can never hold
	// more than one window per range request, so every window past the first
	// is a fresh ranged request at a deep offset, each answered 206 with the
	// exact open-ended Content-Range for its start. (Measured headless-
	// Chromium behavior: the media stack prefetches the container's chunk
	// positions as parallel ranged requests right after the bytes=0- window,
	// and a later currentTime seek is served from that prefetch rather than by
	// aborting a stalled covering request — so the deterministic observable of
	// ranged seeking through the relay origin is this deep-range request set,
	// not a request timestamped after the seek instant.)
	var deepReq *playbackReq
	if !waitUntil(ctx, 30*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for i := range reqs {
			if reqs[i].start >= 1<<20 {
				s := reqs[i]
				deepReq = &s
				return true
			}
		}
		return false
	}) {
		t.Fatal("no deep (>=1 MiB) ranged playback request observed")
	}
	mu.Lock()
	deepResp := resps[deepReq.id]
	mu.Unlock()
	wantCR := fmt.Sprintf("bytes %d-%d/%d", deepReq.start, total-1, total)
	if deepResp.status != http.StatusPartialContent || deepResp.contentRange != wantCR {
		t.Fatalf("deep playback response (start %d) = %d %q, want 206 with Content-Range %q",
			deepReq.start, deepResp.status, deepResp.contentRange, wantCR)
	}

	// The seek takes effect on the element: currentTime reads back past the
	// seek target (2.0 s of the 2.5 s timeline) through the relay origin.
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`(function(){var v=document.querySelector('.lg-current video.lg-video');v.currentTime=2.0;return v.currentTime;})()`, nil)); err != nil {
		t.Fatalf("set currentTime: %v", err)
	}
	var current float64
	if !waitUntil(ctx, 15*time.Second, func() bool {
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(function(){var v=document.querySelector('.lg-current video.lg-video');return v?v.currentTime:0;})()`, &current))
		return current > 1.5
	}) {
		t.Fatalf("currentTime did not advance past the seek (got %v)", current)
	}
}

// playbackReq / playbackResp are the browser-observed halves of one playback
// exchange over the Network domain.
type playbackReq struct {
	id    string
	start int64
	at    time.Time
}

type playbackResp struct {
	status       int64
	contentRange string
}

// firstPlayback returns the first playback request and its response.
func firstPlayback(reqs []playbackReq, resps map[string]playbackResp) (*playbackReq, playbackResp) {
	if len(reqs) == 0 {
		return nil, playbackResp{}
	}
	first := reqs[0]
	return &first, resps[first.id]
}

// parseRangeStart extracts the start offset from a "bytes=N-…" Range value.
func parseRangeStart(rangeHdr string) (int64, bool) {
	if rangeHdr == "" {
		return 0, false
	}
	var start int64
	if _, err := fmt.Sscanf(rangeHdr, "bytes=%d-", &start); err != nil {
		return 0, false
	}
	return start, true
}

// ---------------------------------------------------------------------------
// Task 24 — the long-running agent fixture behind e2e/browser (§23.4). The
// Playwright globalSetup runs this package with SB_E2E_BROWSER_FIXTURE set;
// the test then serves the REAL direct server (Binder SNI admission, the
// Task 23 connect endpoint, the Phase 3 gallery placeholder) for the fixture
// share codes bound on their real route kinds until the driver kills the
// process. Without the env var it skips, so the ordinary gate never enters
// server mode.
// ---------------------------------------------------------------------------

// e2eFixtureCodes is the share-code layout the Playwright spec drives:
// direct candidates (whose direct check the fixture proxy may blackhole), a
// relay-only share, and a noscript share.
var e2eFixtureCodes = []string{
	"direct01", "blackh02", "manual03", "non44305", "unrelat06", "relayon04", "noscrpt08",
}

// e2eFixtureNS / e2eFixtureBase mirror a real namespace allocation (the same
// ^sb[0-9a-f]{8}$ form control issues) over the fixture base domain.
const (
	e2eFixtureNS   = "sb0a1b2c3"
	e2eFixtureBase = "example.com"
)

// connectObservation is one observed /s/<code>/connect exchange, recorded by
// wrapping the real server handler (the endpoint itself is NOT re-implemented
// — the wrapper only watches).
type connectObservation struct {
	Code           string            `json:"code"`
	Method         string            `json:"method"`
	RequestHeaders map[string]string `json:"requestHeaders"`
	ResponseStatus int               `json:"responseStatus"`
	ResponseHeader map[string]string `json:"responseHeaders"`
	ResponseBytes  int               `json:"responseBytes"`
}

// observingHandler wraps the real direct-server handler and records every
// /s/<code>/connect exchange (method, headers, response status, response
// headers, response body bytes) for the fixture admin endpoint.
type observingHandler struct {
	inner http.Handler
	mu    sync.Mutex
	obs   []connectObservation
}

func (h *observingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	code, isConnect := connectCodeOf(r.URL.Path)
	if !isConnect {
		h.inner.ServeHTTP(w, r)
		return
	}
	reqHeaders := map[string]string{}
	for k, v := range r.Header {
		reqHeaders[strings.ToLower(k)] = strings.Join(v, ",")
	}
	rec := &recordingResponseWriter{ResponseWriter: w, header: w.Header()}
	h.inner.ServeHTTP(rec, r)
	h.mu.Lock()
	h.obs = append(h.obs, connectObservation{
		Code: code, Method: r.Method,
		RequestHeaders: reqHeaders,
		ResponseStatus: rec.status, ResponseHeader: rec.headers(), ResponseBytes: rec.bytes,
	})
	h.mu.Unlock()
}

// connectCodeOf reports whether path is /s/<code>/connect and the code.
func connectCodeOf(path string) (string, bool) {
	if !strings.HasPrefix(path, "/s/") || !strings.HasSuffix(path, "/connect") {
		return "", false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(path, "/s/"), "/connect")
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// recordingResponseWriter captures status, headers and body size.
type recordingResponseWriter struct {
	http.ResponseWriter
	header      http.Header
	wroteHeader bool
	status      int
	bytes       int
}

func (r *recordingResponseWriter) WriteHeader(status int) {
	if !r.wroteHeader {
		r.wroteHeader, r.status = true, status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recordingResponseWriter) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader, r.status = true, http.StatusOK
	}
	r.bytes += len(b)
	return r.ResponseWriter.Write(b)
}

func (r *recordingResponseWriter) Header() http.Header { return r.header }

// headers snapshots the recorded headers (set-cookie included verbatim).
func (r *recordingResponseWriter) headers() map[string]string {
	out := map[string]string{}
	for k, v := range r.header {
		out[strings.ToLower(k)] = strings.Join(v, ",")
	}
	return out
}

func TestE2EBrowserFixture(t *testing.T) {
	if os.Getenv("SB_E2E_BROWSER_FIXTURE") == "" {
		t.Skip("long-running e2e/browser fixture server; driven by e2e/browser/fixture.mjs")
	}

	backend := &parityBackend{}
	sm := NewSnapshotManager(backend, 5, time.Hour)
	if err := sm.Build(context.Background()); err != nil {
		t.Fatalf("build snapshot: %v", err)
	}
	registry := NewResolverRegistry()
	for _, code := range e2eFixtureCodes {
		registry.Put(code, sm)
	}

	dir := t.TempDir()
	root, rootKey, roots := parityTestCA(t)
	cm := cert.NewManager(dir, e2eFixtureBase, roots)
	if err := cm.SetNamespace(e2eFixtureNS); err != nil {
		t.Fatal(err)
	}
	csrPEM, err := cm.GenerateCSR()
	if err != nil {
		t.Fatal(err)
	}
	leafDER := parityMintLeaf(t, root, rootKey, parityCSRPublicKey(t, csrPEM), e2eFixtureNS, e2eFixtureBase)
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})...)
	if err := cm.Install(chain); err != nil {
		t.Fatalf("install chain: %v", err)
	}

	mapper := &parityMapper{ip: "203.0.113.7"}
	port := NewOnDemandPortOwned(mapper, 443, 8443, time.Hour, "e2e-fixture", "192.168.1.20")
	gate := NewSignalGate("e2e-fixture", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(e2eFixtureNS, e2eFixtureBase, port, cm, gate, 1<<20)
	srv.SetResolver(registry)

	// Bind each direct candidate's RouteDirect origin (relay-only never gets
	// one, §6.1) and EVERY code's RouteRelay origin — the dual binding the
	// fixture's fallback/302/noscript cases navigate.
	for _, code := range e2eFixtureCodes {
		if code == "relayon04" {
			continue
		}
		if err := srv.Binder().Allow(code+"."+e2eFixtureNS+"."+e2eFixtureBase, RouteDirect, code); err != nil {
			t.Fatal(err)
		}
	}
	for _, code := range e2eFixtureCodes {
		if err := srv.Binder().Allow(code+".relay."+e2eFixtureNS+"."+e2eFixtureBase, RouteRelay, code); err != nil {
			t.Fatal(err)
		}
	}
	for _, code := range e2eFixtureCodes {
		if err := port.OpenFor(code, time.Hour); err != nil {
			t.Fatalf("open port for %s: %v", code, err)
		}
	}

	observing := &observingHandler{inner: srv.Handler()}
	ts := httptest.NewUnstartedServer(observing)
	ts.TLS = srv.TLSConfig()
	ts.StartTLS()
	defer ts.Close()

	// Admin listener: the connect-observation feed the Playwright spec
	// asserts against (an agent-side view of the REAL connect endpoint).
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	adminMux.HandleFunc("/admin/connect-observations", func(w http.ResponseWriter, _ *http.Request) {
		observing.mu.Lock()
		snapshot := append([]connectObservation(nil), observing.obs...)
		observing.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"observations": snapshot})
	})
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("admin listen: %v", err)
	}
	adminSrv := &http.Server{Handler: adminMux}
	go func() { _ = adminSrv.Serve(adminLn) }()
	defer adminSrv.Close()

	agentPort := ts.Listener.Addr().(*net.TCPAddr).Port
	adminPort := adminLn.Addr().(*net.TCPAddr).Port
	fmt.Printf("SB_E2E_AGENT_HOST=127.0.0.1\n")
	fmt.Printf("SB_E2E_AGENT_PORT=%d\n", agentPort)
	fmt.Printf("SB_E2E_AGENT_ADMIN_PORT=%d\n", adminPort)
	fmt.Printf("SB_E2E_AGENT_NS=%s\n", e2eFixtureNS)
	fmt.Printf("SB_E2E_AGENT_BASE=%s\n", e2eFixtureBase)
	fmt.Printf("SB_E2E_AGENT_READY=1\n")

	// Serve until the fixture driver kills the process (bounded, so a
	// crashed driver can never leak the server past 30 minutes).
	select {
	case <-time.After(30 * time.Minute):
	}
}

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
	"encoding/json"
	"encoding/pem"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/log"
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

func (parityBackend) ListGallery(context.Context) (immich.Gallery, error) { return parityGallery(), nil }

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
	backend := &parityBackend{}
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

	// ---- download accounting: exactly one committed download across parts ----
	session, err := h.sm.Resolve()
	if err != nil {
		t.Fatalf("resolve after parts: %v", err)
	}
	if got := session.Ledger.Downloads(); got != 1 {
		t.Fatalf("committed downloads = %d, want 1", got)
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

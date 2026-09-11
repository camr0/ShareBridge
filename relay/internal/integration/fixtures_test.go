// Package integration is the hermetic real-FRP relay integration suite
// (spec §18.2) and the §23.3 byte-preservation gate.
//
// Every test in this package is gated behind SHAREBRIDGE_FRP_INTEGRATION=1
// because it starts real, checksum-verified pinned frps/frpc child processes
// plus the full gateway/presence/plugin data plane. The fixture is entirely
// local (loopback only): no network, no credentials, no committed binaries.
//
// The fixture composes the REAL relay components in one process:
//
//	frps (pinned binary) + real frpplugin.Server + real presence.Registry
//	  → routes.Table → gateway.Server (public L4 acceptor)
//	frpc (pinned binary) ── TLS transport ── frps ── loopback proxy port
//	  → agent-side HTTPS listener (test CA) → fake Phase 3 content backend
//
// The browser side and the agent side are wrapped in byte taps so the exact
// TLS stream can be captured before the gateway and after FRP decapsulation
// and compared byte-for-byte (spec §16.1, §23.3).
package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sharebridge/relay/internal/frpplugin"
	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/presence"
	"sharebridge/relay/internal/routes"
)

const (
	// integrationEnv gates the whole package: the suite starts real pinned
	// binaries and is not part of the default `go test ./...` gate.
	integrationEnv = "SHAREBRIDGE_FRP_INTEGRATION"

	// The fixture namespace is a real control-shaped allocation
	// (^sb[0-9a-f]{8}$); the credential must satisfy frpplugin's exact
	// namespace/proxy-name binding.
	fixtureNamespace  = "sbdeadbeef"
	fixtureNamespaceB = "sbdeadbee2"
	fixtureBase       = "example.com"
	fixtureCode       = "share01"

	pluginSharedSecret = "integration-only-plugin-secret"

	// Relay ports are drawn from this quiet range so the frps `allowPorts`
	// policy and the plugin's RelayPortMin/Max agree exactly.
	relayPortMin = 20000
	relayPortMax = 30000

	// Per-request budget for fixture setup and content operations.
	setupTimeout = 30 * time.Second
)

// requireIntegration skips unless the operator explicitly enabled the suite.
// In required gate mode (SHAREBRIDGE_FRP_GATE=required) an unset integration
// env is a FAILURE, never a skip: the §23.3 gate must be un-forgeable.
func requireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv(integrationEnv) != "1" {
		if gateRequired() {
			t.Fatalf("%s=%s requires %s=1; the required §23.3 gate must never skip",
				gateModeEnv, gateModeRequired, integrationEnv)
		}
		t.Skipf("set %s=1 to run the hermetic real-FRP integration suite", integrationEnv)
	}
}

// ---------------------------------------------------------------------------
// Pinned FRP binaries (shared resolution with the gate's integrity checks)
// ---------------------------------------------------------------------------

// pinnedFRPBinaries returns the pinned frps/frpc paths for the current
// platform, fetching through relay/scripts/fetch-frp.sh when the
// version+digest-keyed cache is cold. The binaries are never committed. In
// required gate mode a missing platform pin is fatal, never a skip; TestMain
// has already re-hashed the artifacts before any case runs.
func pinnedFRPBinaries(t *testing.T) (string, string) {
	t.Helper()
	root := relayRoot(t)
	manifest, _, err := loadPinnedManifest(root)
	if err != nil {
		t.Fatalf("%v", err)
	}
	pin, err := pinnedArtifact(manifest, pinnedPlatformKey)
	if err != nil {
		if gateRequired() {
			t.Fatalf("required gate mode: %v", err)
		}
		if errors.Is(err, errNoPinnedArtifact) {
			t.Skipf("%v", err)
		}
		t.Fatalf("resolve pinned FRP artifact: %v", err)
	}
	cacheRoot := resolveFRPCacheRootAt(root)
	keyDir := filepath.Join(cacheRoot, fmt.Sprintf("v%s-sha256-%s", manifest.Version, pin.SHA256))
	frps := filepath.Join(keyDir, "frps")
	frpc := filepath.Join(keyDir, "frpc")
	if _, statErr := os.Stat(frps); statErr != nil {
		script := filepath.Join(root, "scripts", "fetch-frp.sh")
		cmd := exec.Command(script)
		cmd.Env = append(os.Environ(), "SHAREBRIDGE_FRP_CACHE="+cacheRoot)
		out, fetchErr := cmd.CombinedOutput()
		if fetchErr != nil {
			t.Fatalf("fetch checksum-verified pinned FRP: %v\n%s", fetchErr, out)
		}
	}
	for _, binary := range []string{frps, frpc} {
		if _, statErr := os.Stat(binary); statErr != nil {
			t.Fatalf("pinned FRP binary %s is unavailable: %v", binary, statErr)
		}
	}
	return frps, frpc
}

func relayRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve relay root: %v", err)
	}
	return root
}

// ---------------------------------------------------------------------------
// Test PKI: one CA trust pool plus an agent leaf carrying the two §6 wildcard
// SANs (direct namespace origin + relay namespace origin).
// ---------------------------------------------------------------------------

type testPKI struct {
	pool     *x509.CertPool
	cert     tls.Certificate
	caPEM    []byte
	leafPEM  []byte
	leafKey  *ecdsa.PrivateKey
	leafCert *x509.Certificate
}

func newTestPKI(t *testing.T, sans ...string) *testPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "integration-test-root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
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
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parse leaf certificate: %v", err)
	}
	return &testPKI{
		pool:     pool,
		cert:     tls.Certificate{Certificate: [][]byte{leafDER, caCert.Raw}, PrivateKey: leafKey, Leaf: leafCert},
		caPEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		leafPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		leafKey:  leafKey,
		leafCert: leafCert,
	}
}

// writeTransportCert mints the self-signed frps transport certificate the
// pinned frpc verifies with trustedCaFile+serverName (spec §23.2).
func writeTransportCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate transport key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create transport certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal transport key: %v", err)
	}
	certPath = filepath.Join(dir, "transport.crt")
	keyPath = filepath.Join(dir, "transport.key")
	writeFile(t, certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPath, keyPath
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// Byte taps: capture the exact browser stream before the gateway and the
// exact post-FRP stream at the agent.
// ---------------------------------------------------------------------------

type byteRecorder struct {
	mu   sync.Mutex
	data []byte
}

func (r *byteRecorder) append(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = append(r.data, p...)
}

func (r *byteRecorder) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.data...)
}

func (r *byteRecorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.data)
}

// writeRecordingConn records every byte written by the browser side (the
// stream handed to the gateway), preserving the net.Conn contract.
type writeRecordingConn struct {
	net.Conn
	rec *byteRecorder
}

func (c *writeRecordingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.rec.append(p[:n])
	}
	return n, err
}

// readRecordingConn records every byte read by the agent side (after FRP
// decapsulation), preserving the net.Conn contract.
type readRecordingConn struct {
	net.Conn
	rec *byteRecorder
}

func (c *readRecordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.rec.append(p[:n])
	}
	return n, err
}

// tapListener wraps the agent's raw TCP listener so every accepted
// connection tees its inbound bytes into the shared recorder.
type tapListener struct {
	net.Listener
	rec *byteRecorder
}

func (l *tapListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &readRecordingConn{Conn: conn, rec: l.rec}, nil
}

// ---------------------------------------------------------------------------
// TLS record fragmenter: splits the first handshake record (the ClientHello)
// into N TLS RECORDS so the gateway's fragmented/multi-record parser and its
// byte-for-byte replay are exercised by a real crypto/tls handshake. TLS
// permits a handshake message to span records, so the server accepts it.
//
// Scope note: this is record-level fragmentation. Separate TCP Write calls do
// NOT prove distinct kernel TCP segments, so this suite makes no
// TCP-segmentation claim anywhere (see GATE-EVIDENCE.md §23.3). Proving
// segment boundaries would need packet capture or platform-specific TCP_INFO,
// neither of which this cross-platform stdlib-only harness has.
// ---------------------------------------------------------------------------

type recordFragmenter struct {
	conn  net.Conn
	parts int
	buf   []byte
	done  bool
}

func (f *recordFragmenter) Read(p []byte) (int, error)         { return f.conn.Read(p) }
func (f *recordFragmenter) Close() error                       { return f.conn.Close() }
func (f *recordFragmenter) LocalAddr() net.Addr                { return f.conn.LocalAddr() }
func (f *recordFragmenter) RemoteAddr() net.Addr               { return f.conn.RemoteAddr() }
func (f *recordFragmenter) SetDeadline(t time.Time) error      { return f.conn.SetDeadline(t) }
func (f *recordFragmenter) SetReadDeadline(t time.Time) error  { return f.conn.SetReadDeadline(t) }
func (f *recordFragmenter) SetWriteDeadline(t time.Time) error { return f.conn.SetWriteDeadline(t) }

func (f *recordFragmenter) Write(p []byte) (int, error) {
	if f.done {
		return f.conn.Write(p)
	}
	f.buf = append(f.buf, p...)
	for !f.done {
		if len(f.buf) < 5 {
			break
		}
		bodyLen := int(f.buf[3])<<8 | int(f.buf[4])
		if len(f.buf) < 5+bodyLen {
			break
		}
		if err := f.writeFragmented(f.buf[:5+bodyLen]); err != nil {
			return 0, err
		}
		f.buf = f.buf[5+bodyLen:]
		f.done = true
	}
	if f.done && len(f.buf) > 0 {
		if _, err := f.conn.Write(f.buf); err != nil {
			return 0, err
		}
		f.buf = f.buf[:0]
	}
	return len(p), nil
}

func (f *recordFragmenter) writeFragmented(record []byte) error {
	body := record[5:]
	parts := f.parts
	if parts < 2 {
		parts = 2
	}
	if parts > len(body) {
		parts = len(body)
	}
	if parts < 2 {
		_, err := f.conn.Write(record)
		return err
	}
	// Exact parts-way split. A naive fixed-stride loop can emit fewer than
	// `parts` chunks (e.g. 4 chunks for parts=5, bodyLen=11), which would make
	// the gate's exact-record-count assertion unsound.
	for i := 0; i < parts; i++ {
		start := len(body) * i / parts
		end := len(body) * (i + 1) / parts
		header := []byte{record[0], record[1], record[2], byte((end - start) >> 8), byte(end - start)}
		if _, err := f.conn.Write(header); err != nil {
			return err
		}
		if _, err := f.conn.Write(body[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// handshakeRecordCount returns how many TLS records the first handshake
// message spans, so the gate can assert an EXACT record count (>= is not a
// fragmentation proof).
func handshakeRecordCount(stream []byte) int {
	records := 0
	messageBytes := 0
	messageLength := -1
	off := 0
	for off+5 <= len(stream) {
		if stream[off] != 0x16 {
			break
		}
		length := int(stream[off+3])<<8 | int(stream[off+4])
		if off+5+length > len(stream) {
			break
		}
		records++
		body := stream[off+5 : off+5+length]
		if messageLength < 0 && len(body) >= 4 {
			messageLength = int(body[1])<<16 | int(body[2])<<8 | int(body[3])
			messageBytes += len(body) - 4
		} else {
			messageBytes += len(body)
		}
		if messageLength >= 0 && messageBytes >= messageLength {
			break
		}
		off += 5 + length
	}
	return records
}

// ---------------------------------------------------------------------------
// Fake Phase 3 content backend behind a real HTTPS listener.
// ---------------------------------------------------------------------------

const (
	contentVideoLen = 1024
	archiveToken    = "arctok01"
)

var (
	contentThumbPNG   = mustPNG()
	contentPreviewPNG = mustPNG()
	contentVideoBytes = func() []byte {
		b := make([]byte, contentVideoLen)
		for i := range b {
			b[i] = byte(i % 251)
		}
		return b
	}()
)

func mustPNG() []byte {
	// A minimal, structurally valid 1x1 PNG (transparent). Fidelity to the
	// agent's image bytes is irrelevant: direct and relay are compared against
	// the same handler.
	return []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A', 'T',
		0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05,
		0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00,
		0x00, 0x00, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60,
		0x82,
	}
}

// contentCounts records per-endpoint request accounting (acceptance #11).
type contentCounts struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *contentCounts) hit(endpoint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = make(map[string]int)
	}
	c.counts[endpoint]++
}

func (c *contentCounts) get(endpoint string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[endpoint]
}

// contentServer is the deterministic fake Phase 3 backend: page, items,
// thumb, preview, original asset, playback with Range, archive manifest and
// parts, plus /connect and a cancellable slow stream.
type contentServer struct {
	counts *contentCounts
	// connectHits counts /connect calls (must stay zero on the relay path).
	connectHits int64
	mu          sync.Mutex
	// slowCancelled is closed when the slow endpoint observes context
	// cancellation, letting the test prove end-to-end cancellation.
	slowCancelled chan struct{}
	slowOnce      sync.Once
}

func newContentServer(counts *contentCounts) *contentServer {
	return &contentServer{counts: counts, slowCancelled: make(chan struct{})}
}

// connectCount returns the number of /connect probe requests observed. It
// takes the same lock as the handler so the gate test can read it under -race.
func (s *contentServer) connectCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connectHits
}

func (s *contentServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	code, rest, ok := splitSharePath(r.URL.Path)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_ = code
	switch {
	case rest == "/connect":
		s.mu.Lock()
		s.connectHits++
		s.mu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	case rest == "" || rest == "/":
		s.counts.hit("page")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, fmt.Sprintf(`<!doctype html><html><head><base href="/s/%s/"></head><body><div id="gallery-root"></div></body></html>`, fixtureCode))
	case rest == "/items":
		s.counts.hit("items")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"albumName":"Parity Album","albumDescription":"integration fixture","items":[`+
			`{"id":"img-1","name":"one.jpg","mimeType":"image/jpeg","width":640,"height":480,"size":7},`+
			`{"id":"vid-1","name":"movie.mp4","mimeType":"video/mp4","width":1280,"height":720,"size":1024,"duration":12.5},`+
			`{"id":"img-2","name":"two.png","mimeType":"image/png","width":320,"height":240,"size":7}]}`)
	case strings.HasPrefix(rest, "/thumb/"):
		s.counts.hit("thumb")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(contentThumbPNG)
	case strings.HasPrefix(rest, "/preview/"):
		s.counts.hit("preview")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(contentPreviewPNG)
	case strings.HasPrefix(rest, "/asset/") && strings.HasSuffix(rest, "/playback"):
		s.counts.hit("playback")
		s.servePlayback(w, r)
	case strings.HasPrefix(rest, "/asset/"):
		s.counts.hit("original")
		id := strings.TrimPrefix(rest, "/asset/")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, id))
		_, _ = io.WriteString(w, "asset-"+id)
	case rest == "/archive":
		s.counts.hit("archive-manifest")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"`+archiveToken+`","parts":[`+
			`{"index":0,"name":"Parity Album-part-1.zip","assetIds":["img-1","vid-1"],"size":100},`+
			`{"index":1,"name":"Parity Album-part-2.zip","assetIds":["img-2"],"size":50}]}`)
	case strings.HasPrefix(rest, "/archive/"):
		s.counts.hit("archive-part")
		w.Header().Set("Content-Type", "application/zip")
		_, _ = io.WriteString(w, "zip:archive-part")
	case rest == "/slow":
		s.counts.hit("slow")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		s.slowOnce.Do(func() { close(s.slowCancelled) })
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *contentServer) servePlayback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Accept-Ranges", "bytes")
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		w.Header().Set("Content-Length", strconv.Itoa(contentVideoLen))
		_, _ = w.Write(contentVideoBytes)
		return
	}
	start, end, ok := parseByteRange(rangeHeader, contentVideoLen)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", contentVideoLen))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	body := contentVideoBytes[start : end+1]
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, contentVideoLen))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(body)
}

// parseByteRange parses the single-range forms the browser/parity harness
// uses: bytes=N-M, bytes=N-, bytes=-N.
func parseByteRange(header string, size int) (int, int, bool) {
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	startStr, endStr, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	if startStr == "" {
		// suffix range: bytes=-N
		suffix, err := strconv.Atoi(endStr)
		if err != nil || suffix <= 0 {
			return 0, 0, false
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, true
	}
	start, err := strconv.Atoi(startStr)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if endStr != "" {
		end, err = strconv.Atoi(endStr)
		if err != nil || end < start {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true
}

// splitSharePath parses /s/{code}{rest}. A fixed code is not required: the
// fixture serves any code so the parity harness can exercise it directly.
func splitSharePath(path string) (code, rest string, ok bool) {
	if !strings.HasPrefix(path, "/s/") {
		return "", "", false
	}
	trimmed := strings.TrimPrefix(path, "/s/")
	code, rest, found := strings.Cut(trimmed, "/")
	if code == "" {
		return "", "", false
	}
	if !found {
		return code, "", true
	}
	return code, "/" + rest, true
}

// ---------------------------------------------------------------------------
// The hermetic stack
// ---------------------------------------------------------------------------

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type tunnelSpec struct {
	label      string
	agentID    string
	namespace  string
	generation int
	proxyPort  int
	localPort  int
}

func (s tunnelSpec) proxyName() string { return "sb-" + s.namespace }

type tunnel struct {
	spec       tunnelSpec
	cmd        *exec.Cmd
	out        *syncBuffer
	configPath string
	// token is the exact one-use credential handed to this frpc. Kept so a
	// test can re-present the very same credential (a faithful replay).
	token string
}

type presenceEventRecorder struct {
	mu     sync.Mutex
	events []presence.Event
}

func (r *presenceEventRecorder) ObservePresenceEvent(event presence.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *presenceEventRecorder) snapshot() []presence.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]presence.Event(nil), r.events...)
}

type relayStack struct {
	t    *testing.T
	dir  string
	pki  *testPKI
	key  ed25519.PrivateKey
	jti  int
	mu   sync.Mutex
	logs *syncBuffer

	frpsPath string
	frpcPath string

	transportPort int
	certPath      string
	keyPath       string

	pluginLn  net.Listener
	pluginSrv *http.Server

	presence       *presence.Registry
	presenceEvents *presenceEventRecorder
	routesTable    *routes.Table
	streams        *gateway.Streams
	gatewaySrv     *gateway.Server
	gatewayLn      net.Listener
	gatewayAddr    string

	frpsCmd *exec.Cmd

	// Agent side.
	agentLn     net.Listener
	agentTap    *byteRecorder
	agentAddr   string
	agentSrv    *http.Server
	content     *contentServer
	counts      *contentCounts
	rawTargetLn net.Listener
	rawTarget   *byteRecorder

	// dialMu guards dialTargets, the exact addresses the real gateway dialed
	// (recorded through gateway.WithDialer). Every dial must target the
	// resolved route's loopback FRP port (acceptance #7).
	dialMu      sync.Mutex
	dialTargets []string

	// probeMu guards the harness readiness-probe diagnostics.
	probeMu      sync.Mutex
	probeDials   int
	probeLastErr string

	relayHost  string
	directHost string
	relayHostB string

	tunnels map[string]*tunnel
}

// newRelayStack builds the hermetic stack. Callers start tunnels explicitly.
func newRelayStack(t *testing.T) *relayStack {
	return newRelayStackWithClock(t, nil)
}

// newRelayStackWithClock builds the hermetic stack with an injected clock for
// the presence registry (and the route table, so both leases read the same
// time base). A nil clock selects real time, exactly like newRelayStack. The
// injection exists so a 45-second presence lease can be crossed deterministically
// on the REAL frps/frpc data plane without waiting in real time.
func newRelayStackWithClock(t *testing.T, now func() time.Time) *relayStack {
	t.Helper()
	requireIntegration(t)
	frpsPath, frpcPath := pinnedFRPBinaries(t)
	s := &relayStack{
		t:        t,
		dir:      t.TempDir(),
		logs:     &syncBuffer{},
		frpsPath: frpsPath,
		frpcPath: frpcPath,
		tunnels:  make(map[string]*tunnel),
	}
	s.relayHost = fmt.Sprintf("%s.relay.%s.%s", fixtureCode, fixtureNamespace, fixtureBase)
	s.directHost = fmt.Sprintf("%s.%s.%s", fixtureCode, fixtureNamespace, fixtureBase)
	s.relayHostB = fmt.Sprintf("share02.relay.%s.%s", fixtureNamespaceB, fixtureBase)

	// Agent content server certificate: the two §6 wildcard SANs cover both
	// the direct and relay namespace origins.
	s.pki = newTestPKI(t,
		"*."+fixtureNamespace+"."+fixtureBase,
		"*."+fixtureNamespaceB+"."+fixtureBase,
		"*.relay."+fixtureNamespace+"."+fixtureBase,
		"*.relay."+fixtureNamespaceB+"."+fixtureBase,
	)

	// Control signing key (the relay only ever holds the public half).
	controlPub, controlPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate control key: %v", err)
	}
	s.key = controlPriv

	// Presence registry fed by the real plugin fact stream. The gateway
	// streams registry is the §15.2 drain seam, so the frps-reset clear drains
	// established streams exactly as production does; the expiry path must NOT
	// drain.
	s.streams = gateway.NewStreams()
	s.presenceEvents = &presenceEventRecorder{}
	registryConfig := presence.Config{
		BootID:  "integration-boot",
		Sink:    s.presenceEvents,
		Drainer: s.streams,
		// The harness probe is the production probe shape (zero-byte loopback
		// connects) with a test-only generous deadline: under a heavily loaded
		// `-race -count=N` run, frps can be slow to bind the proxy listener
		// after the plugin returns allow, and the strict production budget
		// (§4.4: 2.5 s) then flakes readiness. The production bounds stay
		// pinned by the presence package's TestLoopbackProbeRespectsBudget.
		Probe:             s.fixtureReadinessProbe,
		ProbeHardDeadline: 30 * time.Second,
	}
	if now != nil {
		registryConfig.Now = now
	}
	registry, err := presence.NewRegistry(registryConfig)
	if err != nil {
		t.Fatalf("new presence registry: %v", err)
	}
	s.presence = registry

	// Real plugin on loopback, emitting facts to the registry.
	plugin, err := frpplugin.NewServer(frpplugin.Config{
		ControlPublicKey:   controlPub,
		PluginSharedSecret: pluginSharedSecret,
		RelayPortMin:       relayPortMin,
		RelayPortMax:       relayPortMax,
		PresenceEvents:     registry,
	})
	if err != nil {
		t.Fatalf("new frp plugin: %v", err)
	}
	s.pluginLn = listenLoopback(t)
	s.pluginSrv = &http.Server{Handler: plugin}
	go func() { _ = s.pluginSrv.Serve(s.pluginLn) }()

	// Agent-side HTTPS listener with a raw byte tap in front of TLS.
	s.agentTap = &byteRecorder{}
	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen agent: %v", err)
	}
	s.agentLn = rawLn
	s.agentAddr = rawLn.Addr().String()
	s.counts = &contentCounts{}
	s.content = newContentServer(s.counts)
	s.agentSrv = &http.Server{
		Handler: s.content,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{s.pki.cert},
			NextProtos:   []string{"h2", "http/1.1"},
			MinVersion:   tls.VersionTLS12,
		},
	}
	go func() {
		_ = s.agentSrv.ServeTLS(&tapListener{Listener: rawLn, rec: s.agentTap}, "", "")
	}()

	// Raw recording target for the exact-routing second tunnel (no TLS).
	s.rawTarget = &byteRecorder{}
	s.rawTargetLn = listenLoopback(t)
	go func() {
		for {
			conn, err := s.rawTargetLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						s.rawTarget.append(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()

	// frps transport TLS material.
	s.certPath, s.keyPath = writeTransportCert(t, s.dir)

	// Route table joined against the presence registry.
	if now != nil {
		s.routesTable = routes.NewTable(registry, routes.WithClock(now))
	} else {
		s.routesTable = routes.NewTable(registry)
	}
	s.gatewaySrv = gateway.NewServer(s.routesTable, s.streams, gateway.WithDialer(s.recordDial))
	s.gatewayLn = listenLoopback(t)
	s.gatewayAddr = s.gatewayLn.Addr().String()
	go func() { _ = s.gatewaySrv.Serve(s.gatewayLn) }()

	// Start frps.
	s.transportPort = s.allocPort(t)
	s.writeFrpsConfig()
	s.startFrps()

	t.Cleanup(s.close)
	return s
}

// fixtureReadinessProbe is the integration harness's readiness probe. It
// performs the production probe's zero-byte loopback TCP connect, retrying
// until the registry's (test-only, generous) hard deadline. It records dial
// attempts and the last error so a readiness failure can be diagnosed.
func (s *relayStack) fixtureReadinessProbe(ctx context.Context, relayPort int) (string, error) {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(relayPort))
	var lastErr error
	for {
		s.probeMu.Lock()
		s.probeDials++
		s.probeMu.Unlock()
		conn, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
		if err == nil {
			sourceAddress := conn.LocalAddr().String()
			_ = conn.Close()
			return sourceAddress, nil
		}
		lastErr = err
		s.probeMu.Lock()
		s.probeLastErr = err.Error()
		s.probeMu.Unlock()
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("fixture readiness probe for %s: %w (last dial error: %v)", address, ctx.Err(), lastErr)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *relayStack) probeDiagnostics() (int, string) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	return s.probeDials, s.probeLastErr
}

// recordDial is the real gateway dial seam (gateway.WithDialer). It records
// every address the relay data plane dials before delegating to the network,
// so the relay-path privacy test can prove the relay path dials only the
// resolved route's loopback FRP port and never a direct-path probe target.
func (s *relayStack) recordDial(ctx context.Context, network, address string) (net.Conn, error) {
	s.dialMu.Lock()
	s.dialTargets = append(s.dialTargets, address)
	s.dialMu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

// dialTargetsSnapshot returns a copy of every address the gateway has dialed.
func (s *relayStack) dialTargetsSnapshot() []string {
	s.dialMu.Lock()
	defer s.dialMu.Unlock()
	return append([]string(nil), s.dialTargets...)
}

func (s *relayStack) writeFrpsConfig() {
	pluginAddr := fmt.Sprintf("http://%s:%s@%s", frpplugin.PluginAuthUsername, pluginSharedSecret, s.pluginLn.Addr())
	config := fmt.Sprintf(`bindAddr = "127.0.0.1"
bindPort = %d
proxyBindAddr = "127.0.0.1"
allowPorts = [{ start = %d, end = %d }]
maxPortsPerClient = 1
userConnTimeout = 10
transport.tcpMux = false
transport.heartbeatTimeout = 45
transport.tls.force = true
transport.tls.certFile = %q
transport.tls.keyFile = %q
[[httpPlugins]]
name = "sharebridge-authorize-presence"
addr = %q
path = %q
ops = ["Login", "NewProxy", "CloseProxy", "Ping", "NewUserConn"]
`, s.transportPort, relayPortMin, relayPortMax, s.certPath, s.keyPath, pluginAddr, frpplugin.APIPath)
	writeFile(s.t, filepath.Join(s.dir, "frps.toml"), []byte(config))
}

func (s *relayStack) startFrps() {
	s.t.Helper()
	cmd := exec.Command(s.frpsPath, "-c", filepath.Join(s.dir, "frps.toml"))
	cmd.Stdout, cmd.Stderr = s.logs, s.logs
	if err := cmd.Start(); err != nil {
		s.t.Fatalf("start real frps: %v", err)
	}
	s.frpsCmd = cmd
	s.waitTCPOpen(net.JoinHostPort("127.0.0.1", strconv.Itoa(s.transportPort)), 5*time.Second)
}

// startTunnel registers one agent tunnel: it signs the exact credential,
// starts the real frpc, applies the route, and returns once presence is
// probe-confirmed online.
func (s *relayStack) startTunnel(spec tunnelSpec) *tunnel {
	s.t.Helper()
	s.mu.Lock()
	s.jti++
	jti := fmt.Sprintf("%s-%d", spec.label, s.jti)
	s.mu.Unlock()

	token := s.signCredential(spec.proxyPort, spec.agentID, spec.namespace, spec.generation, jti)
	adminPort := s.allocPort(s.t)
	configPath := filepath.Join(s.dir, "frpc-"+spec.label+".toml")
	config := fmt.Sprintf(`serverAddr = "127.0.0.1"
serverPort = %d
loginFailExit = false
metadatas.sharebridge_credential = %q
metadatas.sharebridge_generation = %q
transport.tcpMux = false
transport.poolCount = 1
transport.tls.enable = true
transport.tls.trustedCaFile = %q
transport.tls.serverName = "localhost"
transport.heartbeatInterval = 10
transport.heartbeatTimeout = 45
webServer.addr = "127.0.0.1"
webServer.port = %d
webServer.user = "it"
webServer.password = "it"
[[proxies]]
name = %q
type = "tcp"
localIP = "127.0.0.1"
localPort = %d
remotePort = %d
transport.useCompression = false
`, s.transportPort, token, strconv.Itoa(spec.generation), s.certPath, adminPort,
		spec.proxyName(), spec.localPort, spec.proxyPort)
	writeFile(s.t, configPath, []byte(config))

	out := &syncBuffer{}
	cmd := exec.Command(s.frpcPath, "-c", configPath)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		s.t.Fatalf("start real frpc %s: %v", spec.label, err)
	}
	tun := &tunnel{spec: spec, cmd: cmd, out: out, configPath: configPath, token: token}
	s.mu.Lock()
	s.tunnels[spec.label] = tun
	s.mu.Unlock()
	return tun
}

// applyRoute publishes the exact route for a tunnel. Revision must supersede
// prior state for the hostname.
func (s *relayStack) applyRoute(hostname string, spec tunnelSpec, revision uint64) {
	s.t.Helper()
	if err := s.routesTable.Apply(routes.Route{
		Hostname:      hostname,
		AgentRecordID: spec.agentID,
		RelayPort:     spec.proxyPort,
		Generation:    uint64(spec.generation),
		SessionID:     "session-" + spec.label,
		Revision:      revision,
		Active:        true,
	}); err != nil {
		s.t.Fatalf("apply route %s: %v", hostname, err)
	}
}

// waitOnline blocks until the real presence registry reports the tunnel
// online (Login + authorized NewProxy + probe-confirmed NewUserConn).
func (s *relayStack) waitOnline(spec tunnelSpec, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.presence.Online(spec.agentID, spec.proxyPort, uint64(spec.generation)) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	dials, lastProbeErr := s.probeDiagnostics()
	s.t.Fatalf("tunnel %s never became online; probe dials=%d lastProbeErr=%q; presence events: %+v; frps log:\n%s",
		spec.label, dials, lastProbeErr, s.presenceEvents.snapshot(), s.logs.String())
}

// waitRouteReady blocks until the gateway route table resolves the hostname.
func (s *relayStack) waitRouteReady(hostname string, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := s.routesTable.Lookup(hostname); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("route %s never became routable: %v", hostname, lastErr)
}

func (s *relayStack) signCredential(port int, agentID, namespace string, generation int, jti string) string {
	s.t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss":             "sharebridge-control",
		"aud":             "sharebridge-relay",
		"api_key_id":      "api-integration",
		"agent_record_id": agentID,
		"namespace":       namespace,
		"proxy_name":      "sb-" + namespace,
		"relay_port":      port,
		"generation":      generation,
		"issued_at":       now.Add(-time.Minute).UTC(),
		"expires_at":      now.Add(9 * time.Minute).UTC(),
		"jti":             jti,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		s.t.Fatalf("marshal credential: %v", err)
	}
	signature := ed25519.Sign(s.key, payload)
	return "sbrelay1." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// ---------------------------------------------------------------------------
// Clients
// ---------------------------------------------------------------------------

type relayClient struct {
	client *http.Client
	wire   *byteRecorder

	mu       sync.Mutex
	versions []uint16
	alpns    []string
}

func (c *relayClient) recordState(state tls.ConnectionState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.versions = append(c.versions, state.Version)
	c.alpns = append(c.alpns, state.NegotiatedProtocol)
}

func (c *relayClient) negotiatedVersion() uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.versions) == 0 {
		return 0
	}
	return c.versions[len(c.versions)-1]
}

func (c *relayClient) negotiatedALPN() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.alpns) == 0 {
		return ""
	}
	return c.alpns[len(c.alpns)-1]
}

// newRelayClient returns an HTTP client that dials the gateway and speaks the
// requested TLS version/ALPN. It optionally fragments the ClientHello across
// TLS records and records the exact browser-side byte stream.
func (s *relayStack) newRelayClient(minVersion, maxVersion uint16, alpn []string, fragmentParts int) *relayClient {
	wire := &byteRecorder{}
	rc := &relayClient{wire: wire}
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	for _, proto := range alpn {
		if proto == "h2" {
			protocols.SetHTTP2(true)
		}
	}
	transport := &http.Transport{
		Protocols: protocols,
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.gatewayAddr)
			if err != nil {
				return nil, err
			}
			var conn net.Conn = &writeRecordingConn{Conn: raw, rec: wire}
			if fragmentParts > 0 {
				conn = &recordFragmenter{conn: conn, parts: fragmentParts}
			}
			tlsConn := tls.Client(conn, &tls.Config{
				ServerName: s.relayHost,
				RootCAs:    s.pki.pool,
				MinVersion: minVersion,
				MaxVersion: maxVersion,
				NextProtos: alpn,
			})
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
				return nil, err
			}
			rc.recordState(tlsConn.ConnectionState())
			return tlsConn, nil
		},
	}
	rc.client = &http.Client{Transport: transport}
	return rc
}

// newDirectClient talks straight to the agent HTTPS listener (bypassing the
// gateway/FRP) for the direct side of content parity.
func (s *relayStack) newDirectClient() *http.Client {
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	return &http.Client{Transport: &http.Transport{
		Protocols: protocols,
		TLSClientConfig: &tls.Config{
			ServerName: s.directHost,
			RootCAs:    s.pki.pool,
			MinVersion: tls.VersionTLS12,
		},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", s.agentAddr)
		},
	}}
}

func (s *relayStack) relayURL(path string) string  { return "https://" + s.relayHost + path }
func (s *relayStack) directURL(path string) string { return "https://" + s.directHost + path }

// doGet performs one GET against url (through whichever client) and returns
// the response. Headers other than Host are optional.
func doGet(t *testing.T, client *http.Client, url string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", url, err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func readResponseBytes(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return body
}

// ---------------------------------------------------------------------------
// Lifecycle helpers
// ---------------------------------------------------------------------------

func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	return ln
}

// globalPortRegistry hands out distinct quiet-range ports across every stack
// in the process, so parallel/subtest stacks can never collide on a port that
// was probed free but not yet bound.
var globalPortRegistry = struct {
	mu   sync.Mutex
	used map[int]bool
}{used: make(map[int]bool)}

// allocPort hands out a distinct quiet-range port per stack so the frps
// allowPorts policy and every assigned proxy/admin port stay deterministic.
func (s *relayStack) allocPort(t *testing.T) int {
	t.Helper()
	globalPortRegistry.mu.Lock()
	defer globalPortRegistry.mu.Unlock()
	for port := relayPortMin; port <= relayPortMax; port++ {
		if globalPortRegistry.used[port] {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			_ = ln.Close()
			globalPortRegistry.used[port] = true
			return port
		}
	}
	t.Fatalf("no free relay port in %d..%d", relayPortMin, relayPortMax)
	return 0
}

// portOf extracts the numeric port from a host:port address.
func portOf(t *testing.T, address string) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split address %s: %v", address, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port from %s: %v", address, err)
	}
	return port
}

func (s *relayStack) waitTCPOpen(address string, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for %s; frps log:\n%s", address, s.logs.String())
}

func (s *relayStack) stopTunnel(label string) {
	s.mu.Lock()
	tun := s.tunnels[label]
	delete(s.tunnels, label)
	s.mu.Unlock()
	if tun == nil || tun.cmd == nil || tun.cmd.Process == nil {
		return
	}
	_ = tun.cmd.Process.Kill()
	_, _ = tun.cmd.Process.Wait()
	tun.cmd = nil
}

func (s *relayStack) close() {
	s.mu.Lock()
	labels := make([]string, 0, len(s.tunnels))
	for label := range s.tunnels {
		labels = append(labels, label)
	}
	s.mu.Unlock()
	for _, label := range labels {
		s.stopTunnel(label)
	}
	if s.frpsCmd != nil && s.frpsCmd.Process != nil {
		_ = s.frpsCmd.Process.Kill()
		_, _ = s.frpsCmd.Process.Wait()
	}
	if s.gatewaySrv != nil {
		s.gatewaySrv.Close()
	}
	if s.agentSrv != nil {
		_ = s.agentSrv.Close()
	}
	if s.pluginSrv != nil {
		_ = s.pluginSrv.Close()
	}
	if s.rawTargetLn != nil {
		_ = s.rawTargetLn.Close()
	}
}

// ---------------------------------------------------------------------------
// Byte-parity assertion (§16.1 / §23.3)
// ---------------------------------------------------------------------------

type parityDigest struct {
	BrowserSHA256 string
	AgentSHA256   string
	Bytes         int
	AgentBytes    int
	ExtraAtAgent  int
	Records       int
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// compareParity is the §23.3 hard assertion: both captures must be non-empty,
// the post-quiescence agent stream must be EXACTLY as long as the browser
// stream (no injected trailing bytes and no truncation), and every byte must
// match. It returns the evidence digest even on failure so a caller can report
// the observed byte counts. ExtraAtAgent == 0 on success by construction.
func compareParity(browserBytes, agentBytes []byte) (parityDigest, error) {
	digest := parityDigest{
		Bytes:         len(browserBytes),
		AgentBytes:    len(agentBytes),
		ExtraAtAgent:  len(agentBytes) - len(browserBytes),
		BrowserSHA256: sha256Hex(browserBytes),
		AgentSHA256:   sha256Hex(agentBytes),
		Records:       handshakeRecordCount(browserBytes),
	}
	if len(browserBytes) == 0 {
		return digest, errors.New("browser-side tap captured zero bytes; a parity proof over an empty capture is vacuous")
	}
	if len(agentBytes) == 0 {
		return digest, errors.New("agent-side tap captured zero bytes after FRP decapsulation; an empty capture must FAIL, not read as zero extra bytes")
	}
	if len(agentBytes) != len(browserBytes) {
		if len(agentBytes) > len(browserBytes) {
			return digest, fmt.Errorf("agent tap holds %d byte(s) but the browser sent %d: %d injected/extra byte(s) arrived at the agent",
				len(agentBytes), len(browserBytes), len(agentBytes)-len(browserBytes))
		}
		return digest, fmt.Errorf("agent tap holds %d byte(s) but the browser sent %d: FRP truncated %d byte(s)",
			len(agentBytes), len(browserBytes), len(browserBytes)-len(agentBytes))
	}
	if !bytes.Equal(agentBytes, browserBytes) {
		for i := range browserBytes {
			if agentBytes[i] != browserBytes[i] {
				return digest, fmt.Errorf("byte mismatch at offset %d: browser %#02x vs agent %#02x (same length, %d bytes)",
					i, browserBytes[i], agentBytes[i], len(browserBytes))
			}
		}
	}
	return digest, nil
}

// checkFragmentation requires the exact expected TLS record count. `>=` is not
// a fragmentation proof: fewer records than requested makes the case vacuous.
func checkFragmentation(records, wantRecords int) error {
	if wantRecords < 2 {
		return fmt.Errorf("a %d-record ClientHello is not a fragmentation proof", wantRecords)
	}
	if records != wantRecords {
		return fmt.Errorf("ClientHello spans %d TLS record(s), want exactly %d (record-level fragmentation)", records, wantRecords)
	}
	return nil
}

// checkDialTargets requires at least one observed gateway dial (so the
// negative is not vacuous) and that every dial targeted exactly the resolved
// route's loopback FRP port.
func checkDialTargets(targets []string, relayPort int) error {
	if len(targets) == 0 {
		return errors.New("the gateway dial recorder observed no dials; the relay-path proof would be vacuous")
	}
	want := net.JoinHostPort("127.0.0.1", strconv.Itoa(relayPort))
	for _, target := range targets {
		if target != want {
			return fmt.Errorf("gateway dialed %q; the relay path must dial only the route's loopback FRP port %q", target, want)
		}
	}
	return nil
}

// assertByteParity quiesces the agent tap (bounded) so the comparison runs on a
// settled post-EOF capture, then applies compareParity as a hard assertion and
// emits the §23.3 per-case evidence line.
func assertByteParity(t *testing.T, browser, agent *byteRecorder) parityDigest {
	t.Helper()
	browserBytes := browser.Bytes()

	const quiet = 500 * time.Millisecond
	deadline := time.Now().Add(15 * time.Second)
	last := agent.Len()
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		current := agent.Len()
		if current != last {
			last = current
			stableSince = time.Now()
		} else if time.Since(stableSince) >= quiet {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	digest, err := compareParity(browserBytes, agent.Bytes())
	if err != nil {
		t.Fatalf("§23.3 byte parity FAILED (case %s): %v", t.Name(), err)
	}
	if digest.ExtraAtAgent != 0 {
		t.Fatalf("§23.3 byte parity FAILED (case %s): agent tap holds %d extra byte(s) after quiescence; want exactly 0",
			t.Name(), digest.ExtraAtAgent)
	}
	t.Logf("§23.3 evidence case=%s tlsRecords=%d browserBytes=%d agentBytes=%d extraAtAgent=%d browserSHA256=%s agentSHA256=%s",
		t.Name(), digest.Records, digest.Bytes, digest.AgentBytes, digest.ExtraAtAgent, digest.BrowserSHA256, digest.AgentSHA256)
	return digest
}

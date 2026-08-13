# Direct Transport Benchmark Harness — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a self-contained Go CLI (`agent/cmd/benchdirect`) that measures direct-mode WebRTC throughput under simulated latency/loss, isolating the browser-SCTP ceiling (mode A) from the production sender loop (mode B).

**Architecture:** A single Go binary launches headless Chrome via chromedp, serves an embedded receiver page over local HTTP, does the WebRTC SDP exchange in-process, and routes both peers' ICE traffic through an in-process UDP shim that injects delay/loss. Mode A pumps bytes through a bare pion `DataChannel`; mode B drives the real `peer` + `transfer.Manager` (`sendWithBackpressure`) against a fake in-memory `StorageBackend`. Results are reported as JSON.

**Tech Stack:** Go 1.26, `github.com/pion/webrtc/v4`, `github.com/chromedp/chromedp`, stdlib `flag`/`net`/`net/http`, `go:embed` for the receiver page.

## Global Constraints

- **Zero production-code changes.** Only `agent/cmd/benchdirect/**` is added; `agent/internal/**` is imported, never modified.
- **No API keys, signaling server, TURN, or PocketBase.** Loopback only, manual SDP exchange.
- **Single Go binary + installed Chrome.** No Node toolchain. chromedp is imported only by `cmd/benchdirect`, so `cmd/agent` is unaffected.
- **CLI-runnable:** `go run ./cmd/benchdirect --mode raw|prod --rtt 0|25|50|100 ...`.
- **Disposable:** benchmark code must not leak abstractions into production.
- Latency/loss are simulated in-process (UDP shim); no root, no kernel tools.
- Chrome is launched with `--disable-features=WebRtcHideLocalIpsWithMdns` so its SDP carries a plain `127.0.0.1` host candidate.

---

## File Structure

```
agent/cmd/benchdirect/
  main.go        flag parsing + orchestration (Task 6)
  shim.go        UDP latency/loss shim (Task 1)
  shim_test.go
  sdp.go         candidate port rewrite (Task 2)
  sdp_test.go
  browser.go     chromedp launch/navigate/eval (Task 3)
  server.go      local HTTP server + /start + /answer (Task 3)
  web/index.html receiver page (Task 3)
  web/bench.js   receiver: DataChannel byte counter (Task 3)
  rawbench.go    mode A: bare pion DataChannel pump (Task 4)
  rawbench_test.go
  fakebackend.go in-memory StorageBackend (Task 5)
  prodbench.go   mode B: peer + Manager sendWithBackpressure (Task 5)
  prodbench_test.go
  report.go      JSON + human result output (Task 6)
```

---

## Task 1: UDP latency/loss shim

**Files:**
- Create: `agent/cmd/benchdirect/shim.go`
- Test: `agent/cmd/benchdirect/shim_test.go`

**Interfaces:**
- Produces (consumed by Tasks 4, 5):

```go
type Shim struct{ /* internal */ }
func NewShim() *Shim
func (s *Shim) AddRoute(delay time.Duration, loss float64) (*Route, error)
func (s *Shim) Close() error

type Route struct{ /* internal */ }
func (r *Route) Addr() *net.UDPAddr            // listen address (127.0.0.1:0)
func (r *Route) SetForward(addr *net.UDPAddr)  // target datagrams are forwarded to
```

Semantics: a `Route` binds a UDP socket on `127.0.0.1:0`, reads datagrams, holds each for `delay` before forwarding to the target set by `SetForward`. `loss` is the probability (0..1) of dropping a datagram. Order is preserved. Forwarding is safe to enable lazily via `SetForward` (packets received before `SetForward` are dropped).

- [ ] **Step 1: Write the failing test**

`shim_test.go`:

```go
package main

import (
	"net"
	"testing"
	"time"
)

func echoServer(t *testing.T) (*net.UDPConn, *net.UDPAddr) {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	return pc, pc.LocalAddr().(*net.UDPAddr)
}

func TestRouteForwardsAfterDelay(t *testing.T) {
	target, targetAddr := echoServer(t)
	defer target.Close()

	s := NewShim()
	defer s.Close()
	r, err := s.AddRoute(200*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.SetForward(targetAddr)

	src, err := net.DialUDP("udp4", nil, r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	start := time.Now()
	if _, err := src.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 4)
	_ = target.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := target.ReadFromUDP(buf); err != nil {
		t.Fatalf("no packet received: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 200*time.Millisecond {
		t.Fatalf("packet arrived too early: %v", elapsed)
	}
	if string(buf) != "ping" {
		t.Fatalf("payload corrupted: %q", buf)
	}
}

func TestRouteDropsWithFullLoss(t *testing.T) {
	target, targetAddr := echoServer(t)
	defer target.Close()

	s := NewShim()
	defer s.Close()
	r, err := s.AddRoute(0, 1.0) // 100% loss
	if err != nil {
		t.Fatal(err)
	}
	r.SetForward(targetAddr)

	src, err := net.DialUDP("udp4", nil, r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 4)
	_ = target.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := target.ReadFromUDP(buf); err == nil {
		t.Fatal("expected packet to be dropped, but it arrived")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestRoute' -v`
Expected: FAIL — `undefined: NewShim`, `undefined: Shim`, `undefined: Route`.

- [ ] **Step 3: Write the implementation**

`shim.go`:

```go
package main

import (
	"math/rand"
	"net"
	"sync"
	"time"
)

type packet struct {
	data []byte
	addr *net.UDPAddr
	due  time.Time
}

type Route struct {
	mu     sync.Mutex
	conn   *net.UDPConn
	target *net.UDPAddr
	delay  time.Duration
	loss   float64
	queue  []packet
	notify chan struct{}
	closed bool
}

func (r *Route) Addr() *net.UDPAddr { return r.conn.LocalAddr().(*net.UDPAddr) }

func (r *Route) SetForward(addr *net.UDPAddr) {
	r.mu.Lock()
	r.target = addr
	r.mu.Unlock()
}

func (r *Route) readLoop() {
	buf := make([]byte, 64*1024)
	for {
		n, addr, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])

		r.mu.Lock()
		if r.closed || r.target == nil {
			r.mu.Unlock()
			continue
		}
		if rand.Float64() < r.loss {
			r.mu.Unlock()
			continue
		}
		headEmpty := len(r.queue) == 0
		r.queue = append(r.queue, packet{data: data, addr: addr, due: time.Now().Add(r.delay)})
		if headEmpty {
			select {
			case r.notify <- struct{}{}:
			default:
			}
		}
		r.mu.Unlock()
	}
}

func (r *Route) drainLoop() {
	for {
		<-r.notify
		for {
			r.mu.Lock()
			if len(r.queue) == 0 {
				r.mu.Unlock()
				break
			}
			head := r.queue[0]
			wait := time.Until(head.due)
			target := r.target
			r.mu.Unlock()

			if wait > 0 {
				time.Sleep(wait)
			}
			r.mu.Lock()
			if len(r.queue) == 0 || r.queue[0].due.After(head.due) {
				r.mu.Unlock()
				continue
			}
			r.queue = r.queue[1:]
			r.mu.Unlock()
			if target != nil {
				_, _ = r.conn.WriteToUDP(head.data, target)
			}
		}
	}
}

type Shim struct {
	mu     sync.Mutex
	routes []*Route
}

func NewShim() *Shim { return &Shim{} }

func (s *Shim) AddRoute(delay time.Duration, loss float64) (*Route, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	r := &Route{conn: conn, delay: delay, loss: loss, notify: make(chan struct{}, 1)}
	go r.readLoop()
	go r.drainLoop()
	s.mu.Lock()
	s.routes = append(s.routes, r)
	s.mu.Unlock()
	return r, nil
}

func (s *Shim) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.routes {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		_ = r.conn.Close()
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestRoute' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd agent && git add cmd/benchdirect/shim.go cmd/benchdirect/shim_test.go
git commit -m "bench: add UDP latency/loss shim"
```

---

## Task 2: SDP candidate port rewrite

**Files:**
- Create: `agent/cmd/benchdirect/sdp.go`
- Test: `agent/cmd/benchdirect/sdp_test.go`

**Interfaces:**
- Produces (consumed by Tasks 4, 5):

```go
// RewriteHostCandidatePort replaces the port of the first host candidate whose IP
// is 127.0.0.1 with newPort, returning the rewritten SDP and the original port.
func RewriteHostCandidatePort(sdp string, newPort int) (string, int, error)
```

- [ ] **Step 1: Write the failing test**

`sdp_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

const sampleSDP = "v=0\r\n" +
	"o=- 1 1 IN IP4 127.0.0.1\r\n" +
	"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=candidate:842163049 1 udp 2130706431 127.0.0.1 49439 typ host generation 0\r\n" +
	"a=ice-ufrag:abc\r\n" +
	"a=ice-pwd:def\r\n"

func TestRewriteHostCandidatePort(t *testing.T) {
	got, orig, err := RewriteHostCandidatePort(sampleSDP, 5001)
	if err != nil {
		t.Fatal(err)
	}
	if orig != 49439 {
		t.Fatalf("orig port = %d, want 49439", orig)
	}
	if !strings.Contains(got, "127.0.0.1 5001 typ host") {
		t.Fatalf("rewritten candidate missing:\n%s", got)
	}
	if strings.Contains(got, "127.0.0.1 49439 typ host") {
		t.Fatalf("original port still present:\n%s", got)
	}
	if !strings.Contains(got, "a=ice-ufrag:abc") {
		t.Fatal("unrelated line corrupted")
	}
}

func TestRewriteNoHostCandidate(t *testing.T) {
	_, _, err := RewriteHostCandidatePort("v=0\r\n", 5001)
	if err == nil {
		t.Fatal("expected error for missing host candidate")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestRewrite' -v`
Expected: FAIL — `undefined: RewriteHostCandidatePort`.

- [ ] **Step 3: Write the implementation**

`sdp.go`:

```go
package main

import (
	"fmt"
	"strconv"
	"strings"
)

func RewriteHostCandidatePort(sdp string, newPort int) (string, int, error) {
	lines := strings.Split(sdp, "\r\n")
	rewritten := false
	origPort := 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "a=candidate:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[4] != "127.0.0.1" {
			continue
		}
		port, err := strconv.Atoi(fields[5])
		if err != nil {
			return "", 0, fmt.Errorf("parse candidate port: %w", err)
		}
		fields[5] = strconv.Itoa(newPort)
		lines[i] = strings.Join(fields, " ")
		origPort = port
		rewritten = true
		break
	}
	if !rewritten {
		return "", 0, fmt.Errorf("no 127.0.0.1 host candidate found")
	}
	return strings.Join(lines, "\r\n"), origPort, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestRewrite' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd agent && git add cmd/benchdirect/sdp.go cmd/benchdirect/sdp_test.go
git commit -m "bench: add SDP candidate port rewrite"
```

---

## Task 3: Browser harness (chromedp + local server + receiver page)

**Files:**
- Create: `agent/cmd/benchdirect/browser.go`
- Create: `agent/cmd/benchdirect/server.go`
- Create: `agent/cmd/benchdirect/web/index.html`
- Create: `agent/cmd/benchdirect/web/bench.js`
- Test: `agent/cmd/benchdirect/browser_test.go` (smoke test)

**Interfaces:**
- Consumes: none (only chromedp, added here).
- Produces (consumed by Tasks 4, 5):

```go
type browserSession struct{ ctx context.Context; cancel context.CancelFunc }
func startBrowser(parent context.Context) (*browserSession, error)  // launches headless Chrome with mDNS disabled
func (b *browserSession) eval(expr string, res interface{}) error   // chromedp.Evaluate
func (b *browserSession) close()

type benchServer struct{ offerFn func() string; answerCh chan string; fs fs.FS }
func newBenchServer(offerFn func() string, answerCh chan string, fs fs.FS) *benchServer
func (s *benchServer) listen() (string, error)  // binds 127.0.0.1:0, returns "http://127.0.0.1:PORT"
```

Server contract (the receiver page depends on it):

- `POST /start` → `200 {"offer":"<sdp>","mode":"raw|prod","size":<int>,"chunk":<int>}`
- `POST /answer` (body `{"sdp":"<sdp>"}`) → `200 {}`; the SDP is written to `answerCh`.

Page contract (Go polls it):

- `window.__bench = { received: 0, firstByteTs: 0, lastByteTs: 0 }`
- `window.__benchSummary()` → `{ received, elapsedMs, mbps }` where `mbps` is `received*8/1e6 / (elapsedMs/1000)`.

- [ ] **Step 1: Add chromedp and write the failing smoke test**

Run first: `cd agent && go get github.com/chromedp/chromedp@latest`

`browser_test.go`:

```go
package main

import (
	"context"
	"testing"
	"time"
)

// Skip when Chrome is unavailable or under -short, so the unit suite stays
// runnable in CI without a browser.
func TestBrowserLaunchesAndEvaluates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser smoke test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := startBrowser(ctx)
	if err != nil {
		t.Skipf("chrome unavailable: %v", err)
	}
	defer b.close()

	var title string
	if err := b.eval("document.title", &title); err != nil {
		t.Fatal(err)
	}
	if title != "benchdirect" {
		t.Fatalf("title = %q, want %q", title, "benchdirect")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestBrowserLaunches' -v`
Expected: FAIL — `undefined: startBrowser` / `undefined: browserSession`.

- [ ] **Step 3: Write `browser.go`**

```go
package main

import (
	"context"
	"errors"
	"os/exec"

	"github.com/chromedp/chromedp"
)

type browserSession struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func chromeAvailable() bool {
	_, err := exec.LookPath("google-chrome")
	if err == nil {
		return true
	}
	_, err = exec.LookPath("chromium")
	if err == nil {
		return true
	}
	_, err = exec.LookPath("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
	return err == nil
}

func startBrowser(parent context.Context) (*browserSession, error) {
	if !chromeAvailable() {
		return nil, errors.New("chrome not found")
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("disable-features", "WebRtcHideLocalIpsWithMdns"),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("no-sandbox", true),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(parent, opts...)
	ctx, cancel := chromedp.NewContext(allocCtx)
	return &browserSession{ctx: ctx, cancel: func() { cancel(); cancelAlloc() }}, nil
}

func (b *browserSession) eval(expr string, res interface{}) error {
	return chromedp.Run(b.ctx, chromedp.Evaluate(expr, res))
}

func (b *browserSession) close() { b.cancel() }
```

- [ ] **Step 4: Write `server.go` and the receiver page**

`server.go`:

```go
package main

import (
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
)

type benchServer struct {
	offerFn  func() string
	answerCh chan string
	fs       fs.FS
	mode     string
	size     int64
	chunk    int
}

func newBenchServer(offerFn func() string, answerCh chan string, fs fs.FS) *benchServer {
	return &benchServer{offerFn: offerFn, answerCh: answerCh, fs: fs}
}

func (s *benchServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"offer": s.offerFn(), "mode": s.mode, "size": s.size, "chunk": s.chunk,
		})
	})
	mux.HandleFunc("/answer", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ SDP string `json:"sdp"` }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.answerCh <- body.SDP
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", http.FileServer(http.FS(s.fs)))
	return mux
}

func (s *benchServer) listen() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	go http.Serve(ln, s.handler())
	return "http://" + ln.Addr().String(), nil
}
```

`web/index.html`:

```html
<!doctype html><meta charset="utf-8"><title>benchdirect</title><script src="bench.js"></script>
```

`web/bench.js`:

```js
window.__bench = { received: 0, firstByteTs: 0, lastByteTs: 0 }

window.__benchSummary = () => {
  const ms = window.__bench.lastByteTs - window.__bench.firstByteTs
  return {
    received: window.__bench.received,
    elapsedMs: ms,
    mbps: ms > 0 ? (window.__bench.received * 8 / (ms / 1000)) / 1e6 : 0,
  }
}

function note(bytes) {
  const now = performance.now()
  if (window.__bench.firstByteTs === 0) window.__bench.firstByteTs = now
  window.__bench.lastByteTs = now
  window.__bench.received += bytes
}

async function run() {
  const res = await fetch('/start', { method: 'POST' })
  const cfg = await res.json()
  const pc = new RTCPeerConnection({ iceServers: [] })

  pc.ondatachannel = (e) => {
    const ch = e.channel
    ch.binaryType = 'arraybuffer'
    ch.onmessage = (m) => {
      if (cfg.mode === 'prod') note(m.data.byteLength - 14) // strip chunk envelope
      else note(m.data.byteLength)
    }
  }

  await pc.setRemoteDescription({ type: 'offer', sdp: cfg.offer })
  const answer = await pc.createAnswer()
  await pc.setLocalDescription(answer)
  await fetch('/answer', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sdp: pc.localDescription.sdp }),
  })
}

run()
```

- [ ] **Step 5: Run the smoke test to verify it passes**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestBrowserLaunches' -v`
Expected: PASS (or SKIP with a clear message if Chrome is absent).

- [ ] **Step 6: Commit**

```bash
cd agent && git add cmd/benchdirect/browser.go cmd/benchdirect/server.go \
  cmd/benchdirect/web/index.html cmd/benchdirect/web/bench.js cmd/benchdirect/browser_test.go go.mod go.sum
git commit -m "bench: add chromedp browser harness and receiver page"
```

---

## Task 4: Mode A — raw pion DataChannel bench

**Files:**
- Create: `agent/cmd/benchdirect/rawbench.go`
- Test: `agent/cmd/benchdirect/rawbench_test.go` (smoke test)

**Interfaces:**
- Consumes: `NewShim`, `Route` (Task 1); `RewriteHostCandidatePort` (Task 2); `startBrowser`, `benchServer` (Task 3).
- Produces:

```go
type rawResult struct {
	Mode      string  `json:"mode"`
	RTT       int     `json:"rtt_ms"`
	SentBytes int64   `json:"sent_bytes"`
	Received  int64   `json:"received_bytes"`
	Mbps      float64 `json:"mbps"`
}

func runRaw(ctx context.Context, cfg runConfig) (rawResult, error)
```

`runConfig` is declared in `rawbench.go` in this task and reused verbatim by Tasks 5 and 6:

```go
type runConfig struct {
	mode          string
	rttMs         int
	loss          float64
	size          int64
	chunk         int
	backpressure  string // "event" | "poll"
}
```

- [ ] **Step 1: Write the failing smoke test**

`rawbench_test.go`:

```go
package main

import (
	"context"
	"testing"
	"time"
)

func TestRunRawSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser smoke test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := runRaw(ctx, runConfig{
		mode: "raw", rttMs: 0, loss: 0, size: 8 << 20, chunk: 16 << 10, backpressure: "event",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Received < res.SentBytes {
		t.Fatalf("received %d < sent %d", res.Received, res.SentBytes)
	}
	if res.Mbps <= 0 {
		t.Fatalf("expected positive throughput, got %.2f Mbps", res.Mbps)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestRunRawSmoke' -v`
Expected: FAIL — `undefined: runRaw`.

- [ ] **Step 3: Write the implementation**

`rawbench.go`:

```go
package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/pion/webrtc/v4"
)

type runConfig struct {
	mode         string
	rttMs        int
	loss         float64
	size         int64
	chunk        int
	backpressure string
}

type rawResult struct {
	Mode      string  `json:"mode"`
	RTT       int     `json:"rtt_ms"`
	SentBytes int64   `json:"sent_bytes"`
	Received  int64   `json:"received_bytes"`
	Mbps      float64 `json:"mbps"`
}

func runRaw(ctx context.Context, cfg runConfig) (rawResult, error) {
	res := rawResult{Mode: "raw", RTT: cfg.rttMs}
	delay := time.Duration(cfg.rttMs/2) * time.Millisecond

	shim := NewShim()
	defer shim.Close()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return res, err
	}
	defer pc.Close()

	dc, err := pc.CreateDataChannel("bench", nil)
	if err != nil {
		return res, err
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return res, err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return res, err
	}

	// Route Chrome→Go through the shim.
	rA, err := shim.AddRoute(delay, cfg.loss)
	if err != nil {
		return res, err
	}
	rewrittenOffer, realGoPort, err := RewriteHostCandidatePort(offer.SDP, rA.Addr().Port)
	if err != nil {
		return res, err
	}
	rA.SetForward(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: realGoPort})

	answerCh := make(chan string, 1)
	srv := newBenchServer(func() string { return rewrittenOffer }, answerCh, webFS)
	srv.mode, srv.size, srv.chunk = cfg.mode, cfg.size, cfg.chunk
	baseURL, err := srv.listen()
	if err != nil {
		return res, err
	}

	b, err := startBrowser(ctx)
	if err != nil {
		return res, err
	}
	defer b.close()
	if err := chromedp.Run(b.ctx, chromedp.Navigate(baseURL)); err != nil {
		return res, err
	}

	select {
	case answer := <-answerCh:
		rB, err := shim.AddRoute(delay, cfg.loss)
		if err != nil {
			return res, err
		}
		rewrittenAnswer, realChromePort, err := RewriteHostCandidatePort(answer, rB.Addr().Port)
		if err != nil {
			return res, err
		}
		rB.SetForward(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: realChromePort})
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: rewrittenAnswer}); err != nil {
			return res, err
		}
	case <-ctx.Done():
		return res, ctx.Err()
	}

	// Pump bytes until the channel is open and all data is sent.
	buf := make([]byte, cfg.chunk)
	var sent int64
	dc.SetBufferedAmountLowThreshold(128 * 1024)
	lowCh := make(chan struct{}, 1)
	dc.OnBufferedAmountLow(func() {
		select {
		case lowCh <- struct{}{}:
		default:
		}
	})
	for sent < cfg.size {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		switch cfg.backpressure {
		case "poll":
			for dc.BufferedAmount() > 5*1024*1024 {
				select {
				case <-ctx.Done():
					return res, ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
		default: // "event"
			for dc.BufferedAmount() > 5*1024*1024 {
				select {
				case <-ctx.Done():
					return res, ctx.Err()
				case <-lowCh:
				}
			}
		}
		n := int64(cfg.chunk)
		if rem := cfg.size - sent; rem < n {
			n = rem
		}
		if err := dc.Send(buf[:n]); err != nil {
			return res, err
		}
		sent += n
		res.SentBytes = sent
	}

	// Wait for the browser to receive everything.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var received int64
		if err := b.eval("window.__bench.received", &received); err != nil {
			return res, err
		}
		if received >= cfg.size {
			break
		}
		if time.Now().After(deadline) {
			return res, fmt.Errorf("timed out waiting for receiver: %d/%d", received, cfg.size)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var summary struct {
		Received  int64   `json:"received"`
		ElapsedMs float64 `json:"elapsedMs"`
		Mbps      float64 `json:"mbps"`
	}
	if err := b.eval("window.__benchSummary()", &summary); err != nil {
		return res, err
	}
	res.Received = summary.Received
	res.Mbps = summary.Mbps
	return res, nil
}
```

Note: `webFS` is declared in `server.go` as `//go:embed web` and used here — add the embed to `server.go` in this task:

```go
import "embed"

//go:embed web
var webFS embed.FS
```

- [ ] **Step 4: Run the smoke test to verify it passes**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestRunRawSmoke' -v`
Expected: PASS. First run may take ~10s (Chrome launch + ICE).

- [ ] **Step 5: Commit**

```bash
cd agent && git add cmd/benchdirect/rawbench.go cmd/benchdirect/rawbench_test.go cmd/benchdirect/server.go
git commit -m "bench: add mode A raw pion DataChannel bench"
```

---

## Task 5: Mode B — production sender loop

**Files:**
- Create: `agent/cmd/benchdirect/fakebackend.go`
- Create: `agent/cmd/benchdirect/prodbench.go`
- Test: `agent/cmd/benchdirect/prodbench_test.go` (smoke test)

**Interfaces:**
- Consumes: `NewShim` (Task 1); `RewriteHostCandidatePort` (Task 2); `startBrowser`, `benchServer` (Task 3); `runConfig`, `rawResult` (Task 4).
- Produces:

```go
func runProd(ctx context.Context, cfg runConfig) (rawResult, error)
```

Mode B triggers the real sender via `manager.HandleMessage` with:

```go
[]byte(`{"type":"file_request","path":"bench.bin","request_id":"bench"}`)
```

- [ ] **Step 1: Write the failing smoke test**

`prodbench_test.go`:

```go
package main

import (
	"context"
	"testing"
	"time"
)

func TestRunProdSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser smoke test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := runProd(ctx, runConfig{
		mode: "prod", rttMs: 0, loss: 0, size: 8 << 20, chunk: 64 << 10, backpressure: "poll",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Received < res.SentBytes {
		t.Fatalf("received %d < sent %d", res.Received, res.SentBytes)
	}
	if res.Mbps <= 0 {
		t.Fatalf("expected positive throughput, got %.2f Mbps", res.Mbps)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestRunProdSmoke' -v`
Expected: FAIL — `undefined: runProd`.

- [ ] **Step 3: Write `fakebackend.go`**

```go
package main

import (
	"io"

	"sharebridge/agent/internal/cloudwebdav"
)

type benchStorage struct{ size int64 }

func (b benchStorage) ListFiles(subpath string) ([]cloudwebdav.FileInfo, error) {
	return []cloudwebdav.FileInfo{{Name: "bench.bin", Size: b.size}}, nil
}

func (b benchStorage) GetFile(filePath string, w io.Writer) (int64, error) {
	buf := make([]byte, 64*1024)
	var written int64
	for written < b.size {
		n := int64(len(buf))
		if rem := b.size - written; rem < n {
			n = rem
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

func (b benchStorage) GetSHA1(subpath string) string { return "" }
```

- [ ] **Step 4: Write `prodbench.go`**

```go
package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/chromedp/chromedp"
	"sharebridge/agent/internal/peer"
	"sharebridge/agent/internal/transfer"
)

func runProd(ctx context.Context, cfg runConfig) (rawResult, error) {
	res := rawResult{Mode: "prod", RTT: cfg.rttMs}
	delay := time.Duration(cfg.rttMs/2) * time.Millisecond

	shim := NewShim()
	defer shim.Close()

	p, err := peer.New(nil, false) // no ICE servers; direct, host candidates only
	if err != nil {
		return res, err
	}
	defer p.Close()

	offer, err := p.CreateOffer()
	if err != nil {
		return res, err
	}
	rA, err := shim.AddRoute(delay, cfg.loss)
	if err != nil {
		return res, err
	}
	rewrittenOffer, realGoPort, err := RewriteHostCandidatePort(offer, rA.Addr().Port)
	if err != nil {
		return res, err
	}
	rA.SetForward(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: realGoPort})

	answerCh := make(chan string, 1)
	srv := newBenchServer(func() string { return rewrittenOffer }, answerCh, webFS)
	srv.mode, srv.size, srv.chunk = cfg.mode, cfg.size, cfg.chunk
	baseURL, err := srv.listen()
	if err != nil {
		return res, err
	}

	b, err := startBrowser(ctx)
	if err != nil {
		return res, err
	}
	defer b.close()
	if err := chromedp.Run(b.ctx, chromedp.Navigate(baseURL)); err != nil {
		return res, err
	}

	select {
	case answer := <-answerCh:
		rB, err := shim.AddRoute(delay, cfg.loss)
		if err != nil {
			return res, err
		}
		rewrittenAnswer, realChromePort, err := RewriteHostCandidatePort(answer, rB.Addr().Port)
		if err != nil {
			return res, err
		}
		rB.SetForward(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: realChromePort})
		if err := p.SetAnswer(rewrittenAnswer); err != nil {
			return res, err
		}
	case <-ctx.Done():
		return res, ctx.Err()
	}

	mgr := transfer.NewManager(p, benchStorage{size: cfg.size}, 0)
	go mgr.HandleMessage([]byte(`{"type":"file_request","path":"bench.bin","request_id":"bench"}`))

	deadline := time.Now().Add(30 * time.Second)
	for {
		var received int64
		if err := b.eval("window.__bench.received", &received); err != nil {
			return res, err
		}
		if received >= cfg.size {
			break
		}
		if time.Now().After(deadline) {
			return res, fmt.Errorf("timed out waiting for receiver: %d/%d", received, cfg.size)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var summary struct {
		Received  int64   `json:"received"`
		ElapsedMs float64 `json:"elapsedMs"`
		Mbps      float64 `json:"mbps"`
	}
	if err := b.eval("window.__benchSummary()", &summary); err != nil {
		return res, err
	}
	res.Received = summary.Received
	res.Mbps = summary.Mbps
	res.SentBytes = cfg.size
	return res, nil
}
```

- [ ] **Step 5: Run the smoke test to verify it passes**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestRunProdSmoke' -v`
Expected: PASS. The Manager streams `cfg.size` bytes through `sendWithBackpressure`; the page strips the 14-byte chunk envelope and counts payload bytes.

- [ ] **Step 6: Commit**

```bash
cd agent && git add cmd/benchdirect/fakebackend.go cmd/benchdirect/prodbench.go cmd/benchdirect/prodbench_test.go
git commit -m "bench: add mode B production sender bench"
```

---

## Task 6: CLI wiring + report + matrix runner

**Files:**
- Create: `agent/cmd/benchdirect/main.go`
- Create: `agent/cmd/benchdirect/report.go`
- Test: `agent/cmd/benchdirect/report_test.go`

**Interfaces:**
- Consumes: `runRaw`, `runProd`, `rawResult`, `runConfig` (Tasks 4, 5).
- Produces: the `benchdirect` binary.

- [ ] **Step 1: Write the failing test**

`report_test.go`:

```go
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReportJSONRoundTrip(t *testing.T) {
	r := rawResult{Mode: "raw", RTT: 25, SentBytes: 1024, Received: 1024, Mbps: 12.5}
	var sb strings.Builder
	if err := writeJSON(&sb, r); err != nil {
		t.Fatal(err)
	}
	var back rawResult
	if err := json.Unmarshal([]byte(sb.String()), &back); err != nil {
		t.Fatal(err)
	}
	if back != r {
		t.Fatalf("round trip mismatch: %+v != %+v", back, r)
	}
}

func TestHumanSummary(t *testing.T) {
	r := rawResult{Mode: "prod", RTT: 50, SentBytes: 1024, Received: 1024, Mbps: 3.2}
	s := humanSummary(r)
	if !strings.Contains(s, "prod") || !strings.Contains(s, "3.2") {
		t.Fatalf("unexpected summary: %s", s)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestReportJSON|TestHumanSummary' -v`
Expected: FAIL — `undefined: writeJSON`, `undefined: humanSummary`.

- [ ] **Step 3: Write `report.go`**

```go
package main

import (
	"encoding/json"
	"fmt"
	"io"
)

func writeJSON(w io.Writer, r rawResult) error {
	return json.NewEncoder(w).Encode(r)
}

func humanSummary(r rawResult) string {
	return fmt.Sprintf("mode=%s rtt=%dms sent=%d received=%d throughput=%.2f Mbps",
		r.Mode, r.RTT, r.SentBytes, r.Received, r.Mbps)
}
```

- [ ] **Step 4: Write `main.go`**

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	mode := flag.String("mode", "raw", "raw|prod")
	rtt := flag.Int("rtt", 0, "added RTT in ms")
	loss := flag.Float64("loss", 0, "packet loss fraction 0..1")
	size := flag.Int64("size", 512<<20, "total bytes to send")
	chunk := flag.Int("chunk", 16<<10, "bytes per send")
	backpressure := flag.String("backpressure", "event", "event|poll (mode A only)")
	out := flag.String("out", "-", "JSON output path (default stdout)")
	flag.Parse()

	cfg := runConfig{
		mode:         *mode,
		rttMs:        *rtt,
		loss:         *loss,
		size:         *size,
		chunk:        *chunk,
		backpressure: *backpressure,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var (
		res rawResult
		err error
	)
	switch cfg.mode {
	case "raw":
		res, err = runRaw(ctx, cfg)
	case "prod":
		res, err = runProd(ctx, cfg)
	default:
		fmt.Fprintln(os.Stderr, "invalid -mode:", cfg.mode)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench failed:", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, humanSummary(res))
	if *out == "-" {
		_ = writeJSON(os.Stdout, res)
		return
	}
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open output:", err)
		os.Exit(1)
	}
	defer f.Close()
	_ = writeJSON(f, res)
}
```

- [ ] **Step 5: Run tests and a manual matrix check**

Run: `cd agent && go test ./cmd/benchdirect/... -v`
Expected: all unit + smoke tests PASS (smoke tests skip in `-short`).

Run: `cd agent && go run ./cmd/benchdirect -mode raw -rtt 0 -size 64MiB -chunk 16KiB`
Expected: one-line summary on stderr + a JSON object on stdout with `mbps > 0`.

Run: `cd agent && go run ./cmd/benchdirect -mode prod -rtt 50 -size 64MiB -chunk 64KiB`
Expected: same, with `rtt_ms=50` and `mode=prod`.

- [ ] **Step 6: Commit**

```bash
cd agent && git add cmd/benchdirect/main.go cmd/benchdirect/report.go cmd/benchdirect/report_test.go
git commit -m "bench: add CLI, JSON report, and matrix runner"
```

---

## Self-Review Notes

- **Spec coverage:** shim (Task 1), candidate rewrite (Task 2), chromedp + local server + receiver page (Task 3), mode A (Task 4), mode B with fake backend (Task 5), CLI + JSON output + flags (Task 6). Loss is supported in the shim from Task 1 and surfaced via `--loss`. Multi-`PeerConnection` (`--peer-conns`) is intentionally deferred (phase 2) per spec.
- **Placeholder scan:** none — every code step carries full code; every test carries a real assertion.
- **Type consistency:** `runConfig` and `rawResult` are defined in Task 4 and reused verbatim by Tasks 5 and 6; `webFS` is declared in `server.go` (Task 3, extended in Task 4) and used by both modes; `benchServer` field contract (`mode`, `size`, `chunk`) is used consistently.

---

## Execution Handoff

Plan complete. Two execution options:

1. **Subagent-Driven (recommended)** — dispatch a fresh subagent per task, review between tasks.
2. **Inline Execution** — execute tasks in this session using executing-plans, with checkpoints.

Which approach?

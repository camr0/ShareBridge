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
func NewShim(delay time.Duration, loss float64) (*Shim, error)
func (s *Shim) Addr() *net.UDPAddr
func (s *Shim) SetPeerA(a *net.UDPAddr)
func (s *Shim) Close() error
```

Semantics: `Shim` is an in-process reflexive NAT between two peers. It binds one UDP socket on `127.0.0.1:0`, holds each datagram for `delay`, then forwards it. Peer A is configured via `SetPeerA`; peer B is learned automatically from the first datagram whose source address is not A. Datagrams from A are forwarded to B and vice versa. `loss` is the probability (0..1) of dropping a datagram. Order is preserved. A datagram from A received before B is learned is dropped (B is unknown).

- [ ] **Step 1: Write the failing test**

`shim_test.go`:

```go
package main

import (
	"net"
	"testing"
	"time"
)

func listenUDP(t *testing.T) (*net.UDPConn, *net.UDPAddr) {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	return pc, pc.LocalAddr().(*net.UDPAddr)
}

func TestShimForwardsBidirectionallyWithDelay(t *testing.T) {
	peerA, aAddr := listenUDP(t) // "Go"
	defer peerA.Close()
	peerB, _ := listenUDP(t) // "Chrome"
	defer peerB.Close()

	s, err := NewShim(200*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetPeerA(aAddr)

	// Chrome → Go: first non-A source learns B, forwards to A after delay.
	start := time.Now()
	if _, err := peerB.WriteToUDP([]byte("ping"), s.Addr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = peerA.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := peerA.ReadFromUDP(buf); err != nil {
		t.Fatalf("peer A did not receive: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("corrupted: %q", buf)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatalf("arrived too early: %v", time.Since(start))
	}

	// Go → Chrome: source A forwards to learned B after delay.
	if _, err := peerA.WriteToUDP([]byte("pong"), s.Addr()); err != nil {
		t.Fatal(err)
	}
	buf2 := make([]byte, 4)
	_ = peerB.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := peerB.ReadFromUDP(buf2); err != nil {
		t.Fatalf("peer B did not receive: %v", err)
	}
	if string(buf2) != "pong" {
		t.Fatalf("corrupted: %q", buf2)
	}
}

func TestShimDropsWithFullLoss(t *testing.T) {
	peerA, aAddr := listenUDP(t)
	defer peerA.Close()
	peerB, _ := listenUDP(t)
	defer peerB.Close()

	s, err := NewShim(0, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetPeerA(aAddr)

	if _, err := peerB.WriteToUDP([]byte("ping"), s.Addr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = peerA.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := peerA.ReadFromUDP(buf); err == nil {
		t.Fatal("expected packet to be dropped, but it arrived")
	}
}

func TestShimDropsFromABeforeBLearned(t *testing.T) {
	peerA, aAddr := listenUDP(t)
	defer peerA.Close()
	peerB, _ := listenUDP(t)
	defer peerB.Close()

	s, err := NewShim(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetPeerA(aAddr)

	if _, err := peerA.WriteToUDP([]byte("early"), s.Addr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	_ = peerB.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := peerB.ReadFromUDP(buf); err == nil {
		t.Fatal("expected packet from A to be dropped before B is learned")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestShim' -v`
Expected: FAIL — `undefined: NewShim`, `undefined: Shim`.

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

// Shim is an in-process reflexive NAT that injects latency and loss between two
// peers. Peer A is configured via SetPeerA; peer B is learned from the first
// datagram whose source address is not A. Datagrams from A are forwarded to B
// and vice versa, each after a fixed delay.
type Shim struct {
	mu     sync.Mutex
	conn   *net.UDPConn
	delay  time.Duration
	loss   float64
	a      *net.UDPAddr
	b      *net.UDPAddr
	queue  []packet
	notify chan struct{}
	closed bool
}

func NewShim(delay time.Duration, loss float64) (*Shim, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	s := &Shim{conn: conn, delay: delay, loss: loss, notify: make(chan struct{}, 1)}
	go s.readLoop()
	go s.drainLoop()
	return s, nil
}

func (s *Shim) Addr() *net.UDPAddr { return s.conn.LocalAddr().(*net.UDPAddr) }

func (s *Shim) SetPeerA(a *net.UDPAddr) {
	s.mu.Lock()
	s.a = a
	s.mu.Unlock()
}

func (s *Shim) readLoop() {
	buf := make([]byte, 64*1024)
	for {
		n, src, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			continue
		}
		if rand.Float64() < s.loss {
			s.mu.Unlock()
			continue
		}
		var target *net.UDPAddr
		if s.a != nil && src.Equal(*s.a) {
			target = s.b // nil until B is learned → drop
		} else {
			if s.b == nil {
				s.b = src
			}
			target = s.a
		}
		if target == nil {
			s.mu.Unlock()
			continue
		}
		headEmpty := len(s.queue) == 0
		s.queue = append(s.queue, packet{data: data, addr: target, due: time.Now().Add(s.delay)})
		if headEmpty {
			select {
			case s.notify <- struct{}{}:
			default:
			}
		}
		s.mu.Unlock()
	}
}

func (s *Shim) drainLoop() {
	for {
		<-s.notify
		for {
			s.mu.Lock()
			if len(s.queue) == 0 {
				s.mu.Unlock()
				break
			}
			head := s.queue[0]
			wait := time.Until(head.due)
			s.queue = s.queue[1:]
			s.mu.Unlock()

			if wait > 0 {
				time.Sleep(wait)
			}
			_, _ = s.conn.WriteToUDP(head.data, head.addr)
		}
	}
}

func (s *Shim) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.conn.Close()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestShim' -v`
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
// RewriteHostCandidate rewrites the first IPv4 host candidate to a loopback
// candidate at 127.0.0.1:newPort, removes all other candidates, and returns the
// original IP and port (for use as a shim forward target).
func RewriteHostCandidate(sdp string, newPort int) (string, string, int, error)
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

func TestRewriteHostCandidate(t *testing.T) {
	got, ip, port, err := RewriteHostCandidate(sampleSDP, 5001)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "127.0.0.1" || port != 49439 {
		t.Fatalf("orig = %s:%d, want 127.0.0.1:49439", ip, port)
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

func TestRewriteHostCandidateRewritesNonLoopbackIP(t *testing.T) {
	sdp := "v=0\r\n" +
		"a=candidate:2935132940 1 udp 2113937151 192.168.1.224 51357 typ host generation 0\r\n"
	got, ip, port, err := RewriteHostCandidate(sdp, 5001)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.168.1.224" || port != 51357 {
		t.Fatalf("orig = %s:%d, want 192.168.1.224:51357", ip, port)
	}
	if !strings.Contains(got, "127.0.0.1 5001 typ host") {
		t.Fatalf("IP+port not rewritten to loopback:\n%s", got)
	}
}

func TestRewriteHostCandidateStripsOtherCandidates(t *testing.T) {
	sdp := "v=0\r\n" +
		"a=candidate:111 1 udp 2113937151 192.168.1.224 51357 typ host generation 0\r\n" +
		"a=candidate:222 1 udp 2113939711 2600:4040::1 63045 typ host generation 0\r\n" +
		"a=candidate:333 1 udp 2113937151 10.0.0.5 7000 typ host generation 0\r\n"
	got, _, _, err := RewriteHostCandidate(sdp, 5001)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, "a=candidate:") != 1 {
		t.Fatalf("expected exactly one candidate, got:\n%s", got)
	}
	if !strings.Contains(got, "127.0.0.1 5001 typ host") {
		t.Fatalf("missing rewritten candidate:\n%s", got)
	}
}

func TestRewriteNoHostCandidate(t *testing.T) {
	_, _, _, err := RewriteHostCandidate("v=0\r\n", 5001)
	if err == nil {
		t.Fatal("expected error for missing host candidate")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestRewrite' -v`
Expected: FAIL — `undefined: RewriteHostCandidate`.

- [ ] **Step 3: Write the implementation**

`sdp.go`:

```go
package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

func isIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}

func RewriteHostCandidate(sdp string, newPort int) (string, string, int, error) {
	lines := strings.Split(sdp, "\r\n")
	out := make([]string, 0, len(lines))
	origIP := ""
	origPort := 0
	rewritten := false
	for _, line := range lines {
		if strings.HasPrefix(line, "a=candidate:") {
			if !rewritten {
				fields := strings.Fields(line)
				if len(fields) >= 8 && fields[2] == "udp" && fields[7] == "host" && isIPv4(fields[4]) {
					port, err := strconv.Atoi(fields[5])
					if err != nil {
						return "", "", 0, fmt.Errorf("parse candidate port: %w", err)
					}
					origIP = fields[4]
					origPort = port
					fields[4] = "127.0.0.1"
					fields[5] = strconv.Itoa(newPort)
					out = append(out, strings.Join(fields, " "))
					rewritten = true
					continue
				}
			}
			continue // drop this candidate (non-IPv4-host or a later candidate)
		}
		out = append(out, line)
	}
	if !rewritten {
		return "", "", 0, fmt.Errorf("no IPv4 host candidate found")
	}
	return strings.Join(out, "\r\n"), origIP, origPort, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestRewrite' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd agent && git add cmd/benchdirect/sdp.go cmd/benchdirect/sdp_test.go
git commit -m "bench: add SDP candidate rewrite"
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

	"github.com/chromedp/chromedp"
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

	if err := chromedp.Run(b.ctx, chromedp.Navigate("data:text/html,<title>benchdirect</title>")); err != nil {
		t.Fatal(err)
	}
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
- Consumes: `NewShim`, `Route` (Task 1); `RewriteHostCandidate` (Task 2); `startBrowser`, `benchServer` (Task 3).
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
	"io/fs"
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

	shim, err := NewShim(delay, cfg.loss)
	if err != nil {
		return res, err
	}
	defer shim.Close()

	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return res, err
	}
	defer pc.Close()

	dc, err := pc.CreateDataChannel("bench", nil)
	if err != nil {
		return res, err
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return res, err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return res, err
	}
	<-gatherDone
	offerSDP := pc.LocalDescription().SDP

	// Rewrite Go's host candidate to the shim and register Go as peer A.
	rewrittenOffer, _, goPort, err := RewriteHostCandidate(offerSDP, shim.Addr().Port)
	if err != nil {
		return res, err
	}
	shim.SetPeerA(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: goPort})

	answerCh := make(chan string, 1)
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		return res, err
	}
	srv := newBenchServer(func() string { return rewrittenOffer }, answerCh, webRoot)
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
		// Rewrite Chrome's host candidate to the shim; the shim auto-learns
		// Chrome's real address from the first datagram it receives.
		rewrittenAnswer, _, _, err := RewriteHostCandidate(answer, shim.Addr().Port)
		if err != nil {
			return res, err
		}
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
- Modify: `agent/cmd/benchdirect/web/bench.js` (count only the `bulk` lane in prod mode)

**Interfaces:**
- Consumes: `NewShim` (Task 1); `RewriteHostCandidate` (Task 2); `startBrowser`, `benchServer` (Task 3); `runConfig`, `rawResult` (Task 4); `multilane.ChannelSet`/`Endpoint`/`LaneForClass` (production interfaces).
- Produces:

```go
func runProd(ctx context.Context, cfg runConfig) (rawResult, error)
```

Mode B triggers the real sender via `transfer.Manager.HandleMessage` with:

```go
[]byte(`{"type":"file_request","path":"bench.bin","request_id":"bench"}`)
```

Why not `peer.New`: production `peer.CreateOffer()` returns a candidate-less trickle SDP (it does not wait for ICE gathering and does not expose the gathered SDP), which the shim's non-trickle candidate rewrite cannot consume. So mode B builds a bench-local `ChannelSet` over three pion DataChannels (same loopback + gathering-wait setup as mode A) and passes it to the **real** `transfer.Manager`. The `Manager`'s `sendWithBackpressure` loop, 64 KiB chunk framing, and 5 MiB watermark are still exercised verbatim — `peer.Peer` is only thin glue.

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

- [ ] **Step 4: Fix `web/bench.js` to count only the `bulk` lane in prod mode**

The file stream is sent on the `bulk` DataChannel as 14-byte-framed chunks; the Manager also sends a `file_header` JSON message on `control` before streaming. The receiver must count only the `bulk` lane in prod mode (and ignore `control`/`media`), otherwise the control message corrupts the byte count. Replace the `pc.ondatachannel` handler with:

```js
  pc.ondatachannel = (e) => {
    const ch = e.channel
    ch.binaryType = 'arraybuffer'
    if (cfg.mode === 'prod' && ch.label !== 'bulk') return
    ch.onmessage = (m) => {
      if (cfg.mode === 'prod') note(m.data.byteLength - 14)
      else note(m.data.byteLength)
    }
  }
```

- [ ] **Step 5: Write `prodbench.go`**

```go
package main

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/pion/webrtc/v4"
	"sharebridge/agent/internal/multilane"
	"sharebridge/agent/internal/transfer"
)

// benchChannelSet wraps three pion DataChannels so the real transfer.Manager can
// run over a bench-controlled PeerConnection (peer.Peer cannot be used because
// its CreateOffer returns a candidate-less trickle SDP).
type benchChannelSet struct {
	mu        sync.Mutex
	endpoints map[multilane.Lane]*benchEndpoint
	onOpen    func()
	onClose   func()
}

func (c *benchChannelSet) Endpoint(lane multilane.Lane) multilane.Endpoint { return c.endpoints[lane] }
func (c *benchChannelSet) SetOnOpen(f func())                               { c.mu.Lock(); c.onOpen = f; c.mu.Unlock() }
func (c *benchChannelSet) SetOnClose(f func())                              { c.mu.Lock(); c.onClose = f; c.mu.Unlock() }
func (c *benchChannelSet) AddOnClose(f func()) {
	if f == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.onClose
	c.onClose = func() {
		if prev != nil {
			prev()
		}
		f()
	}
}
func (c *benchChannelSet) Close() error { return nil } // pc.Close handled by caller

type benchEndpoint struct {
	lane multilane.Lane
	dc   *webrtc.DataChannel
}

func (e *benchEndpoint) SendText(s string) error   { return e.dc.SendText(s) }
func (e *benchEndpoint) SendBinary(b []byte) error { return e.dc.Send(b) }
func (e *benchEndpoint) BufferedAmount() uint64    { return e.dc.BufferedAmount() }
func (e *benchEndpoint) SetOnMessage(f func([]byte)) {
	e.dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if f != nil {
			f(msg.Data)
		}
	})
}
func (e *benchEndpoint) SendBinaryClass(class multilane.TrafficClass, b []byte) error {
	lane, err := multilane.LaneForClass(class)
	if err != nil {
		return err
	}
	if lane != e.lane {
		return fmt.Errorf("traffic class %d maps to lane %d, not %d", class, lane, e.lane)
	}
	return e.dc.Send(b)
}

func runProd(ctx context.Context, cfg runConfig) (rawResult, error) {
	res := rawResult{Mode: "prod", RTT: cfg.rttMs}
	delay := time.Duration(cfg.rttMs/2) * time.Millisecond

	shim, err := NewShim(delay, cfg.loss)
	if err != nil {
		return res, err
	}
	defer shim.Close()

	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return res, err
	}
	defer pc.Close()

	labels := map[multilane.Lane]string{
		multilane.LaneControl: "control",
		multilane.LaneMedia:   "media",
		multilane.LaneBulk:    "bulk",
	}
	set := &benchChannelSet{endpoints: make(map[multilane.Lane]*benchEndpoint, 3)}
	openCh := make(chan struct{}, 3)
	for _, lane := range []multilane.Lane{multilane.LaneControl, multilane.LaneMedia, multilane.LaneBulk} {
		dc, err := pc.CreateDataChannel(labels[lane], nil)
		if err != nil {
			return res, err
		}
		set.endpoints[lane] = &benchEndpoint{lane: lane, dc: dc}
		dc.OnOpen(func() { select { case openCh <- struct{}{}: default: } })
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return res, err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return res, err
	}
	<-gatherDone
	offerSDP := pc.LocalDescription().SDP

	rewrittenOffer, _, goPort, err := RewriteHostCandidate(offerSDP, shim.Addr().Port)
	if err != nil {
		return res, err
	}
	shim.SetPeerA(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: goPort})

	answerCh := make(chan string, 1)
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		return res, err
	}
	srv := newBenchServer(func() string { return rewrittenOffer }, answerCh, webRoot)
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
		rewrittenAnswer, _, _, err := RewriteHostCandidate(answer, shim.Addr().Port)
		if err != nil {
			return res, err
		}
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: rewrittenAnswer}); err != nil {
			return res, err
		}
	case <-ctx.Done():
		return res, ctx.Err()
	}

	for i := 0; i < 3; i++ {
		select {
		case <-openCh:
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(10 * time.Second):
			return res, fmt.Errorf("timed out waiting for DataChannels to open")
		}
	}

	mgr := transfer.NewManager(set, benchStorage{size: cfg.size}, 0)
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

- [ ] **Step 6: Run the smoke test to verify it passes**

Run: `cd agent && go test ./cmd/benchdirect/ -run 'TestRunProdSmoke' -v`
Expected: PASS. The Manager streams `cfg.size` bytes through `sendWithBackpressure`; the page strips the 14-byte chunk envelope and counts payload bytes.

- [ ] **Step 7: Commit**

```bash
cd agent && git add cmd/benchdirect/fakebackend.go cmd/benchdirect/prodbench.go cmd/benchdirect/prodbench_test.go cmd/benchdirect/web/bench.js
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

package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"sharebridge/relay/internal/clienthello"
	"sharebridge/relay/internal/limits"
	"sharebridge/relay/internal/routes"
)

const (
	relayRouteAlpha   = "alpha.relay.ns1.sharebridgeusercontent.com"
	relayRouteUnknown = "unrouted.relay.ns1.sharebridgeusercontent.com"
	relayAgentRecord  = "agent-record-alpha"
	relayGeneration   = uint64(11)
	relayRevision     = uint64(3)
)

// staticPresence answers every presence query with a fixed verdict, standing
// in for the Task 14 leased presence registry.
type staticPresence struct {
	online bool
}

func (presence staticPresence) Online(agentRecordID string, relayPort int, generation uint64) bool {
	return presence.online
}

// gatedPresence answers the gateway's first presence query — the initial
// route lookup — with true, then parks every later query — the admission
// re-check at registration time — until the test releases it. That lets a
// test revoke the route inside the lookup→register window deterministically.
type gatedPresence struct {
	onlineQueries atomic.Int32
	lookupPassed  chan struct{}
	admitGate     chan struct{}
}

func newGatedPresence() *gatedPresence {
	return &gatedPresence{
		lookupPassed: make(chan struct{}),
		admitGate:    make(chan struct{}),
	}
}

func (presence *gatedPresence) Online(agentRecordID string, relayPort int, generation uint64) bool {
	if presence.onlineQueries.Add(1) == 1 {
		close(presence.lookupPassed)
		return true
	}
	<-presence.admitGate
	return false
}

// releaseAdmission unblocks the parked admission re-check.
func (presence *gatedPresence) releaseAdmission() {
	close(presence.admitGate)
}

// dialObservation is one recorded agent-side dial.
type dialObservation struct {
	network string
	address string
	// budget is the remaining connect budget observed at dial time.
	budget time.Duration
}

// recordingDialer stands in for the production loopback dialer. It records
// every dial's network, exact address, and remaining budget; with
// holdUntilDeadline set it never completes a connect and instead blocks
// until the gateway's budget cancels the dial context, like a black-holed
// loopback peer.
type recordingDialer struct {
	agentListener     net.Listener
	holdUntilDeadline atomic.Bool

	mu           sync.Mutex
	observations []dialObservation
}

func (dialer *recordingDialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	budget := time.Duration(0)
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		budget = time.Until(deadline)
	}
	dialer.mu.Lock()
	dialer.observations = append(dialer.observations, dialObservation{network: network, address: address, budget: budget})
	dialer.mu.Unlock()

	if dialer.holdUntilDeadline.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return net.Dial(network, address)
}

func (dialer *recordingDialer) recorded() []dialObservation {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	return append([]dialObservation(nil), dialer.observations...)
}

// gatewayHarness wires a real gateway server over real TCP listeners: an
// "agent" listener standing in for the loopback FRP proxy port and a public
// listener the server accepts browser connections on.
type gatewayHarness struct {
	server        *Server
	streams       *Streams
	table         *routes.Table
	dialer        *recordingDialer
	publicAddr    string
	agentListener net.Listener
	agentPort     int
}

func newGatewayHarness(t *testing.T, presence routes.Presence, options ...Option) *gatewayHarness {
	t.Helper()

	agentListener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "0"))
	if err != nil {
		t.Fatalf("listening for the fake agent loopback port: %v", err)
	}
	t.Cleanup(func() { agentListener.Close() })
	agentPort := agentListener.Addr().(*net.TCPAddr).Port

	publicListener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "0"))
	if err != nil {
		t.Fatalf("listening for the fake public interface: %v", err)
	}
	t.Cleanup(func() { publicListener.Close() })

	table := routes.NewTable(presence)
	streams := NewStreams()
	dialer := &recordingDialer{agentListener: agentListener}
	server := NewServer(table, streams, append([]Option{
		WithLogger(slog.New(slog.DiscardHandler)),
		WithDialer(dialer.dial),
	}, options...)...)

	go func() { _ = server.Serve(publicListener) }()
	t.Cleanup(func() {
		publicListener.Close()
		server.Close()
		server.Wait()
	})

	return &gatewayHarness{
		server:        server,
		streams:       streams,
		table:         table,
		dialer:        dialer,
		publicAddr:    publicListener.Addr().String(),
		agentListener: agentListener,
		agentPort:     agentPort,
	}
}

func applyRoute(t *testing.T, table *routes.Table, hostname string, relayPort int) {
	t.Helper()
	err := table.Apply(routes.Route{
		Hostname:      hostname,
		AgentRecordID: relayAgentRecord,
		RelayPort:     relayPort,
		Generation:    relayGeneration,
		SessionID:     "session-alpha",
		Revision:      relayRevision,
		Active:        true,
	})
	if err != nil {
		t.Fatalf("applying route %q: %v", hostname, err)
	}
}

// clientHelloRecord builds a single-record TLS ClientHello whose only
// extension is server_name carrying sni — structurally complete enough for
// the Task 2 parser to accept, ending exactly at the record boundary.
func clientHelloRecord(sni string) []byte {
	hostName := []byte{0x00, byte(len(sni) >> 8), byte(len(sni))}
	hostName = append(hostName, sni...)
	nameList := []byte{byte(len(hostName) >> 8), byte(len(hostName))}
	nameList = append(nameList, hostName...)

	extensions := []byte{0x00, 0x00, byte(len(nameList) >> 8), byte(len(nameList))}
	extensions = append(extensions, nameList...)

	body := []byte{0x03, 0x03}                             // client_version TLS 1.2
	body = append(body, bytes.Repeat([]byte{0xA5}, 32)...) // random
	body = append(body, 0x00)                              // empty session_id
	body = append(body, 0x00, 0x02, 0x13, 0x01)            // cipher_suites: TLS_AES_128_GCM_SHA256
	body = append(body, 0x01, 0x00)                        // compression_methods: null
	body = append(body, byte(len(extensions)>>8), byte(len(extensions)))
	body = append(body, extensions...)

	handshake := []byte{0x01, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	handshake = append(handshake, body...)

	record := []byte{0x16, 0x03, 0x01, byte(len(handshake) >> 8), byte(len(handshake))}
	return append(record, handshake...)
}

func connectBrowser(t *testing.T, address string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatalf("dialing the public gateway address %s: %v", address, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func acceptOne(t *testing.T, listener net.Listener) net.Conn {
	t.Helper()
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("arming the accept deadline: %v", err)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accepting the agent-side connection: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readFull(t *testing.T, conn net.Conn, length int) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("arming the read deadline: %v", err)
	}
	buffer := make([]byte, length)
	if _, err := io.ReadFull(conn, buffer); err != nil {
		t.Fatalf("reading %d bytes: %v", length, err)
	}
	return buffer
}

func readAllUntilClose(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("arming the read deadline: %v", err)
	}
	transcript, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("reading until close: %v", err)
	}
	return transcript
}

// assertClosedWithNoBytes reads the browser connection to EOF and enforces
// the generic-close contract: the gateway closed the connection and not one
// byte was written back to the browser.
func assertClosedWithNoBytes(t *testing.T, label string, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("%s: arming the read deadline: %v", label, err)
	}
	received := 0
	buffer := make([]byte, 1024)
	for {
		count, readErr := conn.Read(buffer)
		received += count
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed) {
				break
			}
			t.Fatalf("%s: unexpected read error: %v", label, readErr)
		}
	}
	if received != 0 {
		t.Fatalf("%s: browser received %d bytes; a generic close must return none", label, received)
	}
}

// assertPeerClosed requires the peer end of a TCP connection to be closed
// with no pending data.
func assertPeerClosed(t *testing.T, label string, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("%s: arming the read deadline: %v", label, err)
	}
	var buffer [1]byte
	count, err := conn.Read(buffer[:])
	if count != 0 {
		t.Fatalf("%s: received %d unexpected bytes", label, count)
	}
	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("%s: peer connection is still open", label)
	}
}

// assertRejectedWithoutBytes is assertClosedWithNoBytes for rejection paths
// that close the peer while its ClientHello is still unread. Closing a socket
// with unread data in the receive buffer makes the kernel send RST, which is
// still a generic close that returns zero bytes to the browser; the strict
// helper above is kept for the fully-read paths.
func assertRejectedWithoutBytes(t *testing.T, label string, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("%s: arming the read deadline: %v", label, err)
	}
	received := 0
	buffer := make([]byte, 1024)
	for {
		count, readErr := conn.Read(buffer)
		received += count
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed) || errors.Is(readErr, syscall.ECONNRESET) {
				break
			}
			t.Fatalf("%s: unexpected read error: %v", label, readErr)
		}
	}
	if received != 0 {
		t.Fatalf("%s: browser received %d bytes; a generic close must return none", label, received)
	}
}

func awaitStreamsDrained(t *testing.T, streams *Streams) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if streams.Len() == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("stream registry still holds %d streams after the connections ended", streams.Len())
}

// helloParserWithDeadline mimics clienthello.Peek with a test-shortened read
// budget: the real five-second budget lives in the Task 2 parser, so the
// deadline test substitutes this to prove the gateway closes the connection
// generically when the ClientHello does not arrive in time.
func helloParserWithDeadline(readTimeout time.Duration) helloParseFunc {
	return func(conn net.Conn) (*clienthello.Hello, error) {
		if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return nil, err
		}
		defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
		var firstByte [1]byte
		if _, err := io.ReadFull(conn, firstByte[:]); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil, fmt.Errorf("test hello parser: %w", clienthello.ErrTimeout)
			}
			return nil, err
		}
		return nil, errors.New("test hello parser: unexpected data before the deadline")
	}
}

func TestGatewayReplaysPrefixBeforeBidirectionalCopy(t *testing.T) {
	t.Run("client hello prefix reaches the agent first and byte-exact", func(t *testing.T) {
		harness := newGatewayHarness(t, staticPresence{online: true})
		applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

		browser := connectBrowser(t, harness.publicAddr)
		hello := clientHelloRecord(relayRouteAlpha)
		if _, err := browser.Write(hello); err != nil {
			t.Fatalf("writing the ClientHello: %v", err)
		}

		agentConn := acceptOne(t, harness.agentListener)

		replayed := readFull(t, agentConn, len(hello))
		if !bytes.Equal(replayed, hello) {
			t.Fatalf("agent received %d prefix bytes that differ from the %d-byte ClientHello", len(replayed), len(hello))
		}
		observations := harness.dialer.recorded()
		if len(observations) != 1 {
			t.Fatalf("gateway dialed %d times, want exactly 1", len(observations))
		}
		wantAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(harness.agentPort))
		if observations[0].address != wantAddress {
			t.Fatalf("dial target %q, want exactly %q", observations[0].address, wantAddress)
		}

		// Bytes after the hello must follow the prefix, never precede or
		// replace it.
		payload := bytes.Repeat([]byte{0xAB}, 128)
		if _, err := browser.Write(payload); err != nil {
			t.Fatalf("writing post-hello payload: %v", err)
		}
		if got := readFull(t, agentConn, len(payload)); !bytes.Equal(got, payload) {
			t.Fatalf("agent received post-hello bytes that differ from what the browser sent")
		}
	})

	t.Run("agent response streams back to the browser byte-exact", func(t *testing.T) {
		harness := newGatewayHarness(t, staticPresence{online: true})
		applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

		browser := connectBrowser(t, harness.publicAddr)
		hello := clientHelloRecord(relayRouteAlpha)
		if _, err := browser.Write(hello); err != nil {
			t.Fatalf("writing the ClientHello: %v", err)
		}

		agentConn := acceptOne(t, harness.agentListener)
		if got := readFull(t, agentConn, len(hello)); !bytes.Equal(got, hello) {
			t.Fatalf("agent received bytes that differ from the ClientHello")
		}

		response := bytes.Repeat([]byte{0xCD}, 96)
		if _, err := agentConn.Write(response); err != nil {
			t.Fatalf("agent writing its response: %v", err)
		}
		if got := readFull(t, browser, len(response)); !bytes.Equal(got, response) {
			t.Fatalf("browser received bytes that differ from the agent response")
		}
	})
}

func TestGatewayUnknownSNIClosesWithoutDial(t *testing.T) {
	harness := newGatewayHarness(t, staticPresence{online: true})
	applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

	browser := connectBrowser(t, harness.publicAddr)
	if _, err := browser.Write(clientHelloRecord(relayRouteUnknown)); err != nil {
		t.Fatalf("writing the ClientHello: %v", err)
	}

	assertClosedWithNoBytes(t, "unknown SNI", browser)

	if observations := harness.dialer.recorded(); len(observations) != 0 {
		t.Fatalf("gateway dialed the agent %d times for an unknown route, want 0", len(observations))
	}
	if got := harness.streams.Len(); got != 0 {
		t.Fatalf("stream registry holds %d streams after rejection, want 0", got)
	}
}

func TestGatewayAbsentTunnelClosesWithoutDial(t *testing.T) {
	harness := newGatewayHarness(t, staticPresence{online: false})
	applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

	browser := connectBrowser(t, harness.publicAddr)
	if _, err := browser.Write(clientHelloRecord(relayRouteAlpha)); err != nil {
		t.Fatalf("writing the ClientHello: %v", err)
	}

	assertClosedWithNoBytes(t, "absent tunnel", browser)

	if observations := harness.dialer.recorded(); len(observations) != 0 {
		t.Fatalf("gateway dialed the agent %d times without tunnel presence, want 0", len(observations))
	}
	if got := harness.streams.Len(); got != 0 {
		t.Fatalf("stream registry holds %d streams after rejection, want 0", got)
	}
}

func TestGatewayLoopbackConnectTimeout(t *testing.T) {
	harness := newGatewayHarness(t, staticPresence{online: true})
	harness.dialer.holdUntilDeadline.Store(true) // black-hole the loopback connect
	applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

	browser := connectBrowser(t, harness.publicAddr)
	if _, err := browser.Write(clientHelloRecord(relayRouteAlpha)); err != nil {
		t.Fatalf("writing the ClientHello: %v", err)
	}

	started := time.Now()
	assertClosedWithNoBytes(t, "loopback connect timeout", browser)
	elapsed := time.Since(started)

	observations := harness.dialer.recorded()
	if len(observations) != 1 {
		t.Fatalf("gateway dialed %d times, want exactly 1", len(observations))
	}
	observation := observations[0]
	if observation.network != "tcp" {
		t.Fatalf("dial network %q, want tcp", observation.network)
	}
	wantAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(harness.agentPort))
	if observation.address != wantAddress {
		t.Fatalf("dial target %q, want exactly %q (loopback host with the assigned relay port)", observation.address, wantAddress)
	}
	if observation.budget <= 1500*time.Millisecond || observation.budget > ConnectTimeout+100*time.Millisecond {
		t.Fatalf("observed connect budget %v, want the spec §14 two-second budget", observation.budget)
	}
	if elapsed < 1500*time.Millisecond || elapsed > 6*time.Second {
		t.Fatalf("connect timeout closed the browser connection after %v, want about %v", elapsed, ConnectTimeout)
	}
	if got := harness.streams.Len(); got != 0 {
		t.Fatalf("stream registry holds %d streams after the dial failed, want 0", got)
	}
}

func TestGatewayClientHelloDeadline(t *testing.T) {
	helloDeadline := 150 * time.Millisecond
	harness := newGatewayHarness(t, staticPresence{online: true}, WithHelloParser(helloParserWithDeadline(helloDeadline)))
	applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

	browser := connectBrowser(t, harness.publicAddr) // connects but never sends a hello

	started := time.Now()
	assertClosedWithNoBytes(t, "ClientHello deadline", browser)
	elapsed := time.Since(started)

	if elapsed < helloDeadline {
		t.Fatalf("gateway closed the connection after %v, before the %v hello budget had expired", elapsed, helloDeadline)
	}
	if observations := harness.dialer.recorded(); len(observations) != 0 {
		t.Fatalf("gateway dialed the agent %d times without a parsed hello, want 0", len(observations))
	}
	if got := harness.streams.Len(); got != 0 {
		t.Fatalf("stream registry holds %d streams after rejection, want 0", got)
	}
}

func TestGatewayInjectsNoBytes(t *testing.T) {
	harness := newGatewayHarness(t, staticPresence{online: true})
	applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

	hello := clientHelloRecord(relayRouteAlpha)
	payload := bytes.Repeat([]byte{0x42}, 512)

	browser := connectBrowser(t, harness.publicAddr)
	if _, err := browser.Write(hello); err != nil {
		t.Fatalf("writing the ClientHello: %v", err)
	}
	if _, err := browser.Write(payload); err != nil {
		t.Fatalf("writing the post-hello payload: %v", err)
	}
	// The browser is done sending; closing lets the agent side observe the
	// complete transcript once the gateway has relayed every byte.
	browser.Close()

	agentConn := acceptOne(t, harness.agentListener)
	transcript := readAllUntilClose(t, agentConn)

	want := append(append([]byte(nil), hello...), payload...)
	if !bytes.Equal(transcript, want) {
		t.Fatalf("agent received %d bytes; the browser stream is exactly %d bytes and must arrive untouched (no injected bytes)", len(transcript), len(want))
	}
	awaitStreamsDrained(t, harness.streams)
}

func TestGatewayStreamRegisteredDuringRevokeIsClosed(t *testing.T) {
	t.Run("admission rejects a stream whose presence vanished after lookup", func(t *testing.T) {
		presence := newGatedPresence()
		presence.releaseAdmission() // the admit-time lookup finds presence gone
		harness := newGatewayHarness(t, presence)
		applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

		browser := connectBrowser(t, harness.publicAddr)
		if _, err := browser.Write(clientHelloRecord(relayRouteAlpha)); err != nil {
			t.Fatalf("writing the ClientHello: %v", err)
		}

		// The dial happens inside the lookup→register window under test, so
		// the fake agent port accepted exactly one connection.
		agentConn := acceptOne(t, harness.agentListener)

		assertClosedWithNoBytes(t, "admission-rejected stream", browser)
		assertPeerClosed(t, "agent-side connection of the rejected stream", agentConn)

		if observations := harness.dialer.recorded(); len(observations) != 1 {
			t.Fatalf("gateway dialed %d times, want exactly 1 (the window under test)", len(observations))
		}
		if got := harness.streams.Len(); got != 0 {
			t.Fatalf("stream registry holds %d streams after admission rejected the stream, want 0", got)
		}
		if closed := harness.streams.CloseRoute(relayRouteAlpha); closed != 0 {
			t.Fatalf("CloseRoute after rejection closed %d streams, want 0", closed)
		}
	})

	t.Run("admission rejects a stream whose route was revoked after lookup", func(t *testing.T) {
		presence := newGatedPresence()
		harness := newGatewayHarness(t, presence)
		applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

		browser := connectBrowser(t, harness.publicAddr)
		if _, err := browser.Write(clientHelloRecord(relayRouteAlpha)); err != nil {
			t.Fatalf("writing the ClientHello: %v", err)
		}

		agentConn := acceptOne(t, harness.agentListener)
		<-presence.lookupPassed // initial lookup passed; the admission re-check is parked
		if err := harness.table.Revoke(relayRouteAlpha, relayRevision+1); err != nil {
			t.Fatalf("revoking inside the lookup→register window: %v", err)
		}
		presence.releaseAdmission()

		assertClosedWithNoBytes(t, "revoked-route stream", browser)
		assertPeerClosed(t, "agent-side connection of the revoked stream", agentConn)

		// The revoke's CloseRoute drained before the stream was ever
		// registered; the admission re-check must fence the stream anyway so
		// it can never outlive the revoke that preceded its registration.
		if closed := harness.streams.CloseRoute(relayRouteAlpha); closed != 0 {
			t.Fatalf("CloseRoute after the drained revoke closed %d streams, want 0", closed)
		}
		if got := harness.streams.Len(); got != 0 {
			t.Fatalf("stream registry holds %d streams after the revoke, want 0", got)
		}
	})

	t.Run("a revoke landing after registration closes the live stream", func(t *testing.T) {
		harness := newGatewayHarness(t, staticPresence{online: true})
		applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

		browser := connectBrowser(t, harness.publicAddr)
		hello := clientHelloRecord(relayRouteAlpha)
		if _, err := browser.Write(hello); err != nil {
			t.Fatalf("writing the ClientHello: %v", err)
		}

		agentConn := acceptOne(t, harness.agentListener)
		if got := readFull(t, agentConn, len(hello)); !bytes.Equal(got, hello) {
			t.Fatalf("agent received bytes that differ from the ClientHello")
		}

		if closed := harness.streams.CloseRoute(relayRouteAlpha); closed != 1 {
			t.Fatalf("revoke after registration closed %d streams, want 1", closed)
		}
		assertPeerClosed(t, "browser after revoke", browser)
		awaitStreamsDrained(t, harness.streams)
	})
}

// relayRouteBeta is a second exact route on the same agent as alpha, used to
// separate per-origin ceilings (different hostname) from per-agent ceilings
// (same agent record).
const relayRouteBeta = "beta.relay.ns1.sharebridgeusercontent.com"

// waitForCondition polls a predicate until it holds or the deadline expires.
// It is a deadline-bounded eventual assertion, never a sleep standing in for
// synchronization.
func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, description)
}

func awaitLimiterIdle(t *testing.T, limiter *limits.Limiter) {
	t.Helper()
	waitForCondition(t, 2*time.Second, func() bool {
		return limiter.ActiveConnections() == 0 && limiter.ActiveStreams() == 0
	}, "every connection and stream lease to be released")
}

// openStream connects a browser, delivers a complete ClientHello for the
// route, and returns both ends of a fully registered live stream.
func openStream(t *testing.T, harness *gatewayHarness, hostname string) (net.Conn, net.Conn) {
	t.Helper()
	browser := connectBrowser(t, harness.publicAddr)
	hello := clientHelloRecord(hostname)
	if _, err := browser.Write(hello); err != nil {
		t.Fatalf("writing the ClientHello for %s: %v", hostname, err)
	}
	agentConn := acceptOne(t, harness.agentListener)
	if got := readFull(t, agentConn, len(hello)); !bytes.Equal(got, hello) {
		t.Fatalf("agent received bytes that differ from the %s ClientHello", hostname)
	}
	return browser, agentConn
}

func TestGatewayLimitsRejectExcessPerSourceIP(t *testing.T) {
	config := limits.DefaultConfig()
	config.MaxStreamsPerSourceIP = 2
	config.MaxStreamsGlobal = 100
	limiter := limits.NewLimiter(config)
	harness := newGatewayHarness(t, staticPresence{online: true}, WithLimiter(limiter))
	applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

	firstBrowser, _ := openStream(t, harness, relayRouteAlpha)
	openStream(t, harness, relayRouteAlpha)
	if got := limiter.ActiveStreams(); got != 2 {
		t.Fatalf("ActiveStreams() = %d, want the two admitted streams", got)
	}

	// Every test connection shares 127.0.0.1, so the third is over the
	// per-source-IP ceiling and must close without a dial.
	third := connectBrowser(t, harness.publicAddr)
	_, _ = third.Write(clientHelloRecord(relayRouteAlpha))
	assertRejectedWithoutBytes(t, "per-source-IP over-limit connection", third)
	if observations := harness.dialer.recorded(); len(observations) != 2 {
		t.Fatalf("gateway dialed the agent %d times, want 2 (the over-limit connection must never dial)", len(observations))
	}
	if got := limiter.ActiveStreams(); got != 2 {
		t.Fatalf("ActiveStreams() = %d after the rejection, want 2", got)
	}

	// Releasing one stream frees exactly one slot.
	firstBrowser.Close()
	waitForCondition(t, 2*time.Second, func() bool { return limiter.ActiveStreams() == 1 }, "the per-IP slot to be released")
	openStream(t, harness, relayRouteAlpha)
	if got := limiter.ActiveStreams(); got != 2 {
		t.Fatalf("ActiveStreams() = %d after re-admission, want 2", got)
	}
}

func TestGatewayLimitsEnforcePerOriginAndPerAgent(t *testing.T) {
	t.Run("per exact origin", func(t *testing.T) {
		config := limits.DefaultConfig()
		config.MaxStreamsPerOrigin = 1
		config.MaxStreamsPerAgent = 100
		config.MaxStreamsGlobal = 100
		limiter := limits.NewLimiter(config)
		harness := newGatewayHarness(t, staticPresence{online: true}, WithLimiter(limiter))
		applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)
		applyRoute(t, harness.table, relayRouteBeta, harness.agentPort)

		openStream(t, harness, relayRouteAlpha)
		openStream(t, harness, relayRouteBeta) // a different exact origin still admits

		third := connectBrowser(t, harness.publicAddr)
		_, _ = third.Write(clientHelloRecord(relayRouteAlpha))
		assertRejectedWithoutBytes(t, "per-origin over-limit connection", third)
		if got := limiter.ActiveStreams(); got != 2 {
			t.Fatalf("ActiveStreams() = %d after the per-origin rejection, want 2", got)
		}
	})

	t.Run("per agent across distinct origins", func(t *testing.T) {
		config := limits.DefaultConfig()
		config.MaxStreamsPerOrigin = 100
		config.MaxStreamsPerAgent = 1
		config.MaxStreamsGlobal = 100
		limiter := limits.NewLimiter(config)
		harness := newGatewayHarness(t, staticPresence{online: true}, WithLimiter(limiter))
		applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)
		applyRoute(t, harness.table, relayRouteBeta, harness.agentPort)

		openStream(t, harness, relayRouteAlpha)

		second := connectBrowser(t, harness.publicAddr)
		_, _ = second.Write(clientHelloRecord(relayRouteBeta))
		assertRejectedWithoutBytes(t, "per-agent over-limit connection", second)
		if got := limiter.ActiveStreamsForAgent(relayAgentRecord); got != 1 {
			t.Fatalf("ActiveStreamsForAgent() = %d after the per-agent rejection, want 1", got)
		}
	})
}

// TestGatewayLimitsReleaseOnEveryRejectionPath walks every pre-pump exit
// path and asserts the held global/IP/agent/origin counters return to zero.
// The dispatch requires the release to be provably absent of leaks on error
// and close paths, not merely on the happy path.
func TestGatewayLimitsReleaseOnEveryRejectionPath(t *testing.T) {
	t.Run("clienthello failure", func(t *testing.T) {
		limiter := limits.NewLimiter(limits.DefaultConfig())
		harness := newGatewayHarness(t, staticPresence{online: true}, WithLimiter(limiter),
			WithHelloParser(func(net.Conn) (*clienthello.Hello, error) {
				return nil, clienthello.ErrMalformed
			}))
		applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

		browser := connectBrowser(t, harness.publicAddr)
		_, _ = browser.Write(clientHelloRecord(relayRouteAlpha))
		assertRejectedWithoutBytes(t, "clienthello failure", browser)
		awaitLimiterIdle(t, limiter)
	})

	t.Run("unknown route", func(t *testing.T) {
		limiter := limits.NewLimiter(limits.DefaultConfig())
		harness := newGatewayHarness(t, staticPresence{online: true}, WithLimiter(limiter))
		applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

		browser := connectBrowser(t, harness.publicAddr)
		_, _ = browser.Write(clientHelloRecord(relayRouteUnknown))
		assertClosedWithNoBytes(t, "unknown route", browser)
		awaitLimiterIdle(t, limiter)
	})

	t.Run("absent tunnel presence", func(t *testing.T) {
		limiter := limits.NewLimiter(limits.DefaultConfig())
		harness := newGatewayHarness(t, staticPresence{online: false}, WithLimiter(limiter))
		applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

		browser := connectBrowser(t, harness.publicAddr)
		_, _ = browser.Write(clientHelloRecord(relayRouteAlpha))
		assertClosedWithNoBytes(t, "absent tunnel presence", browser)
		awaitLimiterIdle(t, limiter)
	})

	t.Run("loopback dial failure", func(t *testing.T) {
		limiter := limits.NewLimiter(limits.DefaultConfig())
		harness := newGatewayHarness(t, staticPresence{online: true}, WithLimiter(limiter))
		harness.dialer.holdUntilDeadline.Store(true)
		applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

		browser := connectBrowser(t, harness.publicAddr)
		_, _ = browser.Write(clientHelloRecord(relayRouteAlpha))
		assertClosedWithNoBytes(t, "loopback dial failure", browser)
		awaitLimiterIdle(t, limiter)
	})

	t.Run("admission re-check rejection", func(t *testing.T) {
		presence := newGatedPresence()
		presence.releaseAdmission()
		limiter := limits.NewLimiter(limits.DefaultConfig())
		harness := newGatewayHarness(t, presence, WithLimiter(limiter))
		applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

		browser := connectBrowser(t, harness.publicAddr)
		_, _ = browser.Write(clientHelloRecord(relayRouteAlpha))
		_ = acceptOne(t, harness.agentListener)
		assertClosedWithNoBytes(t, "admission re-check rejection", browser)
		awaitLimiterIdle(t, limiter)
	})
}

// TestGatewayLimitsAdmitBeforeHelloParseAndBoundPreParseWork is the audit I7
// proof: a connection is admitted against the global and per-source-IP
// ceilings BEFORE any handler goroutine is spawned and before the 128 KiB
// ClientHello scratch buffer is allocated. Two admitted connections park
// inside the parser; every later connection is refused in the accept loop
// with no goroutine, no parse invocation, and no large allocation.
func TestGatewayLimitsAdmitBeforeHelloParseAndBoundPreParseWork(t *testing.T) {
	config := limits.DefaultConfig()
	config.MaxStreamsGlobal = 2
	config.MaxStreamsPerSourceIP = 1000
	limiter := limits.NewLimiter(config)

	var parseCalls atomic.Int32
	release := make(chan struct{})
	blockingParser := func(net.Conn) (*clienthello.Hello, error) {
		parseCalls.Add(1)
		<-release
		return nil, errors.New("test: ClientHello parser released")
	}

	preHarness := runtime.NumGoroutine()
	harness := newGatewayHarness(t, staticPresence{online: true}, WithLimiter(limiter), WithHelloParser(blockingParser))
	applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

	held := make([]net.Conn, 0, 2)
	for i := 0; i < 2; i++ {
		held = append(held, connectBrowser(t, harness.publicAddr))
	}
	waitForCondition(t, 3*time.Second, func() bool { return parseCalls.Load() == 2 }, "two admitted connections to reach the ClientHello parser")
	if got := limiter.ActiveConnections(); got != 2 {
		t.Fatalf("ActiveConnections() = %d, want the 2 admitted connections", got)
	}

	blockedBaseline := runtime.NumGoroutine()
	extra := make([]net.Conn, 0, 20)
	for i := 0; i < 20; i++ {
		extra = append(extra, connectBrowser(t, harness.publicAddr))
	}
	for _, conn := range extra {
		assertClosedWithNoBytes(t, "over-limit connection", conn)
	}

	if got := parseCalls.Load(); got != 2 {
		t.Fatalf("ClientHello parser invoked %d times; the %d unadmitted connections must never reach the parse or allocate its scratch buffer", got, len(extra))
	}
	waitForCondition(t, 2*time.Second, func() bool { return runtime.NumGoroutine() <= blockedBaseline+2 },
		"unadmitted connections to spawn no handler goroutines")

	close(release)
	for _, conn := range held {
		conn.Close()
	}
	waitForCondition(t, 3*time.Second, func() bool { return runtime.NumGoroutine() <= blockedBaseline },
		"the admitted handler goroutines to drain after release")
	awaitLimiterIdle(t, limiter)
	if got := runtime.NumGoroutine(); got > preHarness+8 {
		t.Fatalf("goroutines = %d after the flood drain, want no more than the pre-harness baseline %d plus slack", got, preHarness)
	}
}

// TestGatewayStreamRegistryBoundedByGlobalLimit proves the stream registry
// cannot grow past the global ceiling under a flood (audit I7 registry-state
// cap): the limiter refuses the extra connections before they can register.
func TestGatewayStreamRegistryBoundedByGlobalLimit(t *testing.T) {
	config := limits.DefaultConfig()
	config.MaxStreamsGlobal = 3
	config.MaxStreamsPerSourceIP = 1000
	limiter := limits.NewLimiter(config)
	harness := newGatewayHarness(t, staticPresence{online: true}, WithLimiter(limiter))
	applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

	held := make([]net.Conn, 0, 3)
	for i := 0; i < 3; i++ {
		browser, _ := openStream(t, harness, relayRouteAlpha)
		held = append(held, browser)
	}
	if got := harness.streams.Len(); got != 3 {
		t.Fatalf("stream registry length = %d, want the 3 admitted streams", got)
	}

	for i := 0; i < 5; i++ {
		conn := connectBrowser(t, harness.publicAddr)
		_, _ = conn.Write(clientHelloRecord(relayRouteAlpha))
		assertRejectedWithoutBytes(t, "over-global connection", conn)
	}
	waitForCondition(t, 2*time.Second, func() bool { return harness.streams.Len() <= 3 },
		"the stream registry to stay bounded by the global ceiling")
	if got := harness.streams.Len(); got > 3 {
		t.Fatalf("stream registry length = %d after the flood, want <= the global ceiling 3", got)
	}
	for _, conn := range held {
		conn.Close()
	}
	awaitStreamsDrained(t, harness.streams)
	awaitLimiterIdle(t, limiter)
}

// TestGatewayNoByteIdleTimeoutClosesStream proves the §14 five-minute no-byte
// idle timeout closes a live stream with no payload activity, and releases
// every counter on the close path.
func TestGatewayNoByteIdleTimeoutClosesStream(t *testing.T) {
	config := limits.DefaultConfig()
	config.IdleTimeout = 150 * time.Millisecond
	config.AbsoluteLifetime = 10 * time.Second
	limiter := limits.NewLimiter(config)
	harness := newGatewayHarness(t, staticPresence{online: true}, WithLimiter(limiter))
	applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

	browser, agentConn := openStream(t, harness, relayRouteAlpha)
	started := time.Now()
	assertPeerClosed(t, "no-byte idle browser", browser)
	assertPeerClosed(t, "no-byte idle agent", agentConn)
	elapsed := time.Since(started)
	if elapsed < config.IdleTimeout || elapsed > 3*time.Second {
		t.Fatalf("idle timeout closed the stream after %v, want about %v", elapsed, config.IdleTimeout)
	}
	awaitLimiterIdle(t, limiter)
}

// TestGatewayAbsoluteLifetimeHardClosesActiveStream proves the §14 24-hour
// absolute lifetime is a hard close that continuous payload activity cannot
// reset (the idle timeout is deliberately long here).
func TestGatewayAbsoluteLifetimeHardClosesActiveStream(t *testing.T) {
	config := limits.DefaultConfig()
	config.IdleTimeout = 10 * time.Second
	config.AbsoluteLifetime = 250 * time.Millisecond
	limiter := limits.NewLimiter(config)
	harness := newGatewayHarness(t, staticPresence{online: true}, WithLimiter(limiter))
	applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

	browser, agentConn := openStream(t, harness, relayRouteAlpha)

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_, _ = browser.Write([]byte{0x11})
			}
		}
	}()

	started := time.Now()
	assertPeerClosed(t, "absolute-lifetime browser", browser)
	elapsed := time.Since(started)
	if elapsed < 200*time.Millisecond {
		t.Fatalf("absolute lifetime closed the stream after %v, before the %v hard deadline", elapsed, config.AbsoluteLifetime)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("absolute lifetime closed the stream after %v, want about %v", elapsed, config.AbsoluteLifetime)
	}
	_ = agentConn
	awaitLimiterIdle(t, limiter)
}

// TestGatewayCountsPerAgentRelayedBytes proves the per-agent byte counter
// accounts for both relay directions (and the replayed ClientHello prefix)
// without the product-tier throttle (deferred to Phase 4b per §14).
func TestGatewayCountsPerAgentRelayedBytes(t *testing.T) {
	limiter := limits.NewLimiter(limits.DefaultConfig())
	harness := newGatewayHarness(t, staticPresence{online: true}, WithLimiter(limiter))
	applyRoute(t, harness.table, relayRouteAlpha, harness.agentPort)

	hello := clientHelloRecord(relayRouteAlpha)
	browser := connectBrowser(t, harness.publicAddr)
	if _, err := browser.Write(hello); err != nil {
		t.Fatalf("writing the ClientHello: %v", err)
	}
	agentConn := acceptOne(t, harness.agentListener)
	if got := readFull(t, agentConn, len(hello)); !bytes.Equal(got, hello) {
		t.Fatalf("agent received bytes that differ from the ClientHello")
	}

	toAgent := bytes.Repeat([]byte{0x5A}, 256)
	if _, err := browser.Write(toAgent); err != nil {
		t.Fatalf("writing browser payload: %v", err)
	}
	if got := readFull(t, agentConn, len(toAgent)); !bytes.Equal(got, toAgent) {
		t.Fatalf("agent received browser payload that differs")
	}

	toBrowser := bytes.Repeat([]byte{0xA5}, 128)
	if _, err := agentConn.Write(toBrowser); err != nil {
		t.Fatalf("writing agent payload: %v", err)
	}
	if got := readFull(t, browser, len(toBrowser)); !bytes.Equal(got, toBrowser) {
		t.Fatalf("browser received agent payload that differs")
	}

	wantBytes := uint64(len(hello) + len(toAgent) + len(toBrowser))
	waitForCondition(t, 2*time.Second, func() bool {
		return limiter.BytesForAgent(relayAgentRecord) == wantBytes
	}, "the per-agent byte counter to reach the relayed total")
}

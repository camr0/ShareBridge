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
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sharebridge/relay/internal/clienthello"
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

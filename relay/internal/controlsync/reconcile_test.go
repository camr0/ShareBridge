package controlsync

// Task 13 tests: the gateway-side snapshot/delta applier (plan Task 13, spec
// §§8, 14, 15.1, 15.4). The applier feeds routes.Table, must never guess
// across a revision gap, flips gateway readiness only after the first
// successful snapshot apply, enforces the finite 120-second route lease
// through the table, and distinguishes explicit revoke (closes established
// streams, §15.6) from lease expiry (blocks only new connections, §15.4).
//
// The gateway-facing test reuses the real gateway.Server and the real Task 2
// ClientHello parser over real TCP. The ClientHello builder and the
// generic-close assertion are deliberately duplicated from
// internal/gateway's tests (test assets may not be shared across packages).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/routes"
)

const (
	// reconcileNamespace mirrors control's GenerateNamespace shape: "sb" plus
	// eight lowercase hex characters (frpplugin.validNamespace).
	reconcileNamespace = "sb0123abcd"
	reconcileBootID    = "gateway-boot-task-13"
	reconcileAgentA    = "agent-record-alpha"
	reconcileAgentB    = "agent-record-beta"
	reconcileSession   = "session-record-1"
	reconcilePort      = 10001
	reconcileZone      = "sharebridgeusercontent.com"

	reconcileHostnameA = "app." + "relay." + reconcileNamespace + "." + reconcileZone
	reconcileHostnameB = "photos." + "relay." + reconcileNamespace + "." + reconcileZone
	reconcileHostnameC = "docs." + "relay." + reconcileNamespace + "." + reconcileZone
)

// reconcileJoin is the §8 presence join key used by the tests.
type reconcileJoin struct {
	agentRecordID string
	relayPort     int
	generation    uint64
}

// reconcilePresence is a configurable Presence standing in for the Task 14
// leased registry.
type reconcilePresence struct {
	mu     sync.Mutex
	online map[reconcileJoin]bool
}

func newReconcilePresence(online ...reconcileJoin) *reconcilePresence {
	presence := &reconcilePresence{online: make(map[reconcileJoin]bool)}
	for _, key := range online {
		presence.online[key] = true
	}
	return presence
}

func (presence *reconcilePresence) Online(agentRecordID string, relayPort int, generation uint64) bool {
	presence.mu.Lock()
	defer presence.mu.Unlock()
	return presence.online[reconcileJoin{agentRecordID, relayPort, generation}]
}

// wireRoute builds a wire Route the way control's publisher would.
func wireRoute(hostname string, revision uint64, agent string, port int) Route {
	return Route{
		Hostname:      hostname,
		AgentRecordID: agent,
		RelayPort:     port,
		Generation:    3,
		SessionID:     reconcileSession,
		Revision:      revision,
		Active:        true,
		Limits:        Limits{MaxStreamsPerOrigin: 32, MaxStreamsPerAgent: 64, MaxStreamsGlobal: 8192},
	}
}

// scriptedControl is a programmatic §11.3 control: it serves caller-set
// snapshot/delta payloads on the Task 11 endpoints, records the request
// paths in order, records the `since` of every delta fetch, and records the
// bodies of every status ack.
type scriptedControl struct {
	mu           sync.Mutex
	snapshotBody []byte
	deltaBody    []byte
	paths        []string
	deltaSinces  []uint64
	statusBodies [][]byte
}

func newScriptedControl(t *testing.T, certs syncTestCertificates) (*scriptedControl, *Client) {
	t.Helper()
	script := &scriptedControl{}
	stub := newStubControlServer(t, certs, func(t *testing.T, request *http.Request) (int, []byte) {
		return script.handle(t, request)
	})
	return script, stub.newTestClient(t, nil)
}

func (script *scriptedControl) handle(t *testing.T, request *http.Request) (int, []byte) {
	t.Helper()
	script.mu.Lock()
	defer script.mu.Unlock()
	script.paths = append(script.paths, request.URL.Path)
	switch request.URL.Path {
	case PathSnapshot:
		return http.StatusOK, script.snapshotBody
	case PathDeltas:
		since := uint64(0)
		for index := 0; index < len(request.URL.RawQuery); index++ {
			_ = index // since parsed below via ParseInt on the query value
		}
		values := request.URL.Query()
		if raw := values.Get("since"); raw != "" {
			parsed, err := parseUnsignedQueryValue(raw)
			if err != nil {
				t.Fatalf("scriptedControl: unparseable since query %q: %v", raw, err)
			}
			since = parsed
		}
		script.deltaSinces = append(script.deltaSinces, since)
		return http.StatusOK, script.deltaBody
	case PathStatus:
		body, err := io.ReadAll(io.LimitReader(request.Body, MaxRequestBodyBytes))
		if err != nil {
			t.Fatalf("scriptedControl: read status body: %v", err)
		}
		script.statusBodies = append(script.statusBodies, body)
		receipt, err := json.Marshal(SyncReceipt{Version: ProtocolVersion, Accepted: true})
		if err != nil {
			t.Fatalf("scriptedControl: marshal receipt: %v", err)
		}
		return http.StatusOK, receipt
	default:
		return http.StatusNotFound, nil
	}
}

func (script *scriptedControl) setSnapshot(t *testing.T, snapshot Snapshot) {
	t.Helper()
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	script.mu.Lock()
	defer script.mu.Unlock()
	script.snapshotBody = encoded
}

func (script *scriptedControl) setRawSnapshot(t *testing.T, raw []byte) {
	t.Helper()
	script.mu.Lock()
	defer script.mu.Unlock()
	script.snapshotBody = raw
}

func (script *scriptedControl) setDelta(t *testing.T, page DeltaPage) {
	t.Helper()
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("marshal delta page: %v", err)
	}
	script.mu.Lock()
	defer script.mu.Unlock()
	script.deltaBody = encoded
}

func (script *scriptedControl) recordedPaths() []string {
	script.mu.Lock()
	defer script.mu.Unlock()
	return append([]string(nil), script.paths...)
}

func (script *scriptedControl) recordedDeltaSinces() []uint64 {
	script.mu.Lock()
	defer script.mu.Unlock()
	return append([]uint64(nil), script.deltaSinces...)
}

func (script *scriptedControl) lastStatusAck(t *testing.T) StatusAck {
	t.Helper()
	script.mu.Lock()
	defer script.mu.Unlock()
	if len(script.statusBodies) == 0 {
		t.Fatalf("scriptedControl: no status ack was sent")
	}
	var ack StatusAck
	if err := json.Unmarshal(script.statusBodies[len(script.statusBodies)-1], &ack); err != nil {
		t.Fatalf("scriptedControl: decode last status ack: %v", err)
	}
	return ack
}

func parseUnsignedQueryValue(raw string) (uint64, error) {
	var value uint64
	for index := 0; index < len(raw); index++ {
		character := raw[index]
		if character < '0' || character > '9' {
			return 0, errors.New("not a decimal digit")
		}
		next := value*10 + uint64(character-'0')
		if next < value {
			return 0, errors.New("overflow")
		}
		value = next
	}
	return value, nil
}

// countingDrainer wraps a StreamDrainer and records every CloseRoute call so
// tests can prove drains happen exactly when the spec demands them.
type countingDrainer struct {
	inner StreamDrainer

	mu    sync.Mutex
	names []string
}

func (drainer *countingDrainer) CloseRoute(hostname string) int {
	drainer.mu.Lock()
	drainer.names = append(drainer.names, hostname)
	drainer.mu.Unlock()
	if drainer.inner == nil {
		return 0
	}
	return drainer.inner.CloseRoute(hostname)
}

func (drainer *countingDrainer) drained() []string {
	drainer.mu.Lock()
	defer drainer.mu.Unlock()
	return append([]string(nil), drainer.names...)
}

// countableConn counts Close calls so tests can distinguish a closed
// established stream from one that lease expiry must have left alone.
type countableConn struct {
	net.Conn

	mu         sync.Mutex
	closeCalls int
}

func newCountableConn() *countableConn {
	conn, other := net.Pipe()
	_ = other // the peer end stays open; nothing reads or writes either side
	return &countableConn{Conn: conn}
}

func (conn *countableConn) Close() error {
	conn.mu.Lock()
	conn.closeCalls++
	conn.mu.Unlock()
	return conn.Conn.Close()
}

func (conn *countableConn) closeCount() int {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.closeCalls
}

func mustApplier(t *testing.T, client *Client, table *routes.Table, streams StreamDrainer, clock func() time.Time) *Applier {
	t.Helper()
	applier, err := NewApplier(ApplierConfig{
		Client:    client,
		Table:     table,
		Streams:   streams,
		Namespace: reconcileNamespace,
		BootID:    reconcileBootID,
		Logger:    slog.New(slog.DiscardHandler),
		Clock:     clock,
	})
	if err != nil {
		t.Fatalf("NewApplier: %v", err)
	}
	return applier
}

func mustReconcileSnapshot(t *testing.T, applier *Applier) {
	t.Helper()
	if err := applier.ReconcileSnapshot(context.Background()); err != nil {
		t.Fatalf("ReconcileSnapshot: %v", err)
	}
	if !applier.Ready() {
		t.Fatalf("applier not ready after a successful snapshot apply")
	}
}

func mustApplyDeltas(t *testing.T, applier *Applier) {
	t.Helper()
	if err := applier.SyncDeltas(context.Background()); err != nil {
		t.Fatalf("SyncDeltas: %v", err)
	}
}

// lookupRevision returns the stored route's revision, requiring presence and
// a live lease (tests that need tombstone/expiry semantics call Lookup
// directly and assert the error class).
func lookupRevision(t *testing.T, table *routes.Table, hostname string) uint64 {
	t.Helper()
	route, err := table.Lookup(hostname)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", hostname, err)
	}
	return route.Revision
}

// --- named test 1: no public traffic before the initial snapshot ---

func TestGatewayRejectsPublicTrafficBeforeInitialSnapshot(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)

	agentListener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "0"))
	if err != nil {
		t.Fatalf("agent listener: %v", err)
	}
	t.Cleanup(func() { agentListener.Close() })
	agentPort := agentListener.Addr().(*net.TCPAddr).Port
	join := reconcileJoin{agentRecordID: reconcileAgentA, relayPort: agentPort, generation: 3}

	presence := newReconcilePresence(join)
	table := routes.NewTable(presence)
	streams := gateway.NewStreams()
	applier := mustApplier(t, client, table, streams, nil)

	publicListener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "0"))
	if err != nil {
		t.Fatalf("public listener: %v", err)
	}
	t.Cleanup(func() { publicListener.Close() })
	server := gateway.NewServer(table, streams, gateway.WithLogger(slog.New(slog.DiscardHandler)))
	go func() { _ = server.Serve(publicListener) }()
	t.Cleanup(func() {
		server.Close()
		server.Wait()
	})
	publicAddress := publicListener.Addr().String()

	// Before the initial snapshot the gateway must reject public traffic with
	// a generic close and never send a byte (spec §8).
	if applier.Ready() {
		t.Fatalf("a fresh applier must not be ready before its first snapshot")
	}
	browser := dialPublic(t, publicAddress)
	if _, err := browser.Write(reconcileClientHello(reconcileHostnameA)); err != nil {
		t.Fatalf("write pre-snapshot ClientHello: %v", err)
	}
	assertGenericClose(t, "pre-snapshot", browser)

	// Boot reconciliation: fetch + apply the snapshot; readiness follows.
	script.setSnapshot(t, Snapshot{
		Version:  ProtocolVersion,
		Revision: 7,
		Routes:   []Route{wireRoute(reconcileHostnameA, 7, reconcileAgentA, agentPort)},
	})
	mustReconcileSnapshot(t, applier)
	if applier.LastAppliedRevision() != 7 {
		t.Fatalf("LastAppliedRevision = %d, want 7", applier.LastAppliedRevision())
	}
	if _, err := table.Lookup(reconcileHostnameA); err != nil {
		t.Fatalf("Lookup after snapshot apply: %v", err)
	}

	// The identical public traffic now routes end to end: the agent side
	// receives the ClientHello bytes byte-for-byte before any other byte.
	hello := reconcileClientHello(reconcileHostnameA)
	accepted := make(chan net.Conn, 1)
	go func() {
		if err := agentListener.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Errorf("arm agent accept deadline: %v", err)
		}
		conn, acceptErr := agentListener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	browser = dialPublic(t, publicAddress)
	if _, err := browser.Write(hello); err != nil {
		t.Fatalf("write post-snapshot ClientHello: %v", err)
	}
	var agentConn net.Conn
	select {
	case agentConn = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatalf("agent loopback port was never dialed after the snapshot applied")
	}
	t.Cleanup(func() { agentConn.Close() })
	replayed := make([]byte, len(hello))
	if err := agentConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("arm agent read deadline: %v", err)
	}
	if _, err := io.ReadFull(agentConn, replayed); err != nil {
		t.Fatalf("read replayed ClientHello: %v", err)
	}
	if !bytes.Equal(replayed, hello) {
		t.Fatalf("replayed bytes differ from the inspected ClientHello")
	}
	if _, err := agentConn.Write([]byte("served-after-snapshot")); err != nil {
		t.Fatalf("agent write: %v", err)
	}
	served := make([]byte, len("served-after-snapshot"))
	if err := browser.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("arm browser read deadline: %v", err)
	}
	if _, err := io.ReadFull(browser, served); err != nil {
		t.Fatalf("read served bytes: %v", err)
	}
	if string(served) != "served-after-snapshot" {
		t.Fatalf("browser received %q", served)
	}
}

// --- named test 2: atomic snapshot replacement ---

func TestSnapshotAtomicallyReplacesRoutes(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)

	joinA := reconcileJoin{agentRecordID: reconcileAgentA, relayPort: reconcilePort, generation: 3}
	presence := newReconcilePresence(joinA)
	table := routes.NewTable(presence)
	streams := gateway.NewStreams()
	drainer := &countingDrainer{inner: streams}
	applier := mustApplier(t, client, table, drainer, nil)

	t.Run("replacement drops omitted routes and drains their established streams", func(t *testing.T) {
		script.setSnapshot(t, Snapshot{
			Version:  ProtocolVersion,
			Revision: 10,
			Routes: []Route{
				wireRoute(reconcileHostnameA, 5, reconcileAgentA, reconcilePort),
				wireRoute(reconcileHostnameB, 6, reconcileAgentA, reconcilePort),
			},
		})
		mustReconcileSnapshot(t, applier)
		lookupRevision(t, table, reconcileHostnameA)
		lookupRevision(t, table, reconcileHostnameB)

		streamConn := newCountableConn()
		streams.Register(reconcileHostnameA, reconcileAgentA, streamConn)
		if streams.Len() != 1 {
			t.Fatalf("streams.Len() = %d, want 1", streams.Len())
		}

		// The fresh snapshot omits A, supersedes B, adds C, and carries two
		// routes the applier must reject per-entry: one outside our namespace
		// and one with an RFC 1123-invalid label (§6 namespace binding).
		script.setSnapshot(t, Snapshot{
			Version:  ProtocolVersion,
			Revision: 20,
			Routes: []Route{
				wireRoute(reconcileHostnameB, 7, reconcileAgentA, reconcilePort),
				wireRoute(reconcileHostnameC, 8, reconcileAgentA, reconcilePort),
				wireRoute("app.relay.sbffffffff."+reconcileZone, 9, reconcileAgentA, reconcilePort),
				wireRoute("-bad.relay."+reconcileNamespace+"."+reconcileZone, 9, reconcileAgentA, reconcilePort),
			},
		})
		mustReconcileSnapshot(t, applier)

		if _, err := table.Lookup(reconcileHostnameA); !errors.Is(err, routes.ErrRouteNotFound) {
			t.Fatalf("Lookup(omitted route) error = %v, want %v", err, routes.ErrRouteNotFound)
		}
		if streamConn.closeCount() != 1 {
			t.Fatalf("dropped route's established stream was closed %d times, want exactly 1", streamConn.closeCount())
		}
		if streams.Len() != 0 {
			t.Fatalf("streams.Len() = %d, want 0 after the dropped route drained", streams.Len())
		}
		if drained := drainer.drained(); len(drained) != 1 || drained[0] != reconcileHostnameA {
			t.Fatalf("drained hostnames = %v, want [%s]", drained, reconcileHostnameA)
		}
		if revision := lookupRevision(t, table, reconcileHostnameB); revision != 7 {
			t.Fatalf("B revision = %d, want 7", revision)
		}
		lookupRevision(t, table, reconcileHostnameC)
		if _, err := table.Lookup("app.relay.sbffffffff." + reconcileZone); !errors.Is(err, routes.ErrRouteNotFound) {
			t.Fatalf("Lookup(wrong-namespace route) error = %v, want %v (foreign namespace must never be admitted)", err, routes.ErrRouteNotFound)
		}
		if _, err := table.Lookup("-bad.relay." + reconcileNamespace + "." + reconcileZone); err == nil {
			t.Fatalf("Lookup(RFC 1123-invalid label route) = nil error, want rejection")
		}
		if applier.LastAppliedRevision() != 20 {
			t.Fatalf("LastAppliedRevision = %d, want 20", applier.LastAppliedRevision())
		}
	})

	t.Run("snapshot re-push at equal revisions is idempotent success", func(t *testing.T) {
		// Identical re-push: every per-route revision equals the stored one,
		// which the table reports as stale; the applier must treat that as
		// success (SDD carry-forward) with no churn and no error.
		script.setSnapshot(t, Snapshot{
			Version:  ProtocolVersion,
			Revision: 20,
			Routes: []Route{
				wireRoute(reconcileHostnameB, 7, reconcileAgentA, reconcilePort),
				wireRoute(reconcileHostnameC, 8, reconcileAgentA, reconcilePort),
			},
		})
		mustReconcileSnapshot(t, applier)
		if revision := lookupRevision(t, table, reconcileHostnameB); revision != 7 {
			t.Fatalf("B revision = %d, want unchanged 7", revision)
		}
	})

	t.Run("regressed snapshot after a control restart keeps newer per-route state", func(t *testing.T) {
		// Task 12 concern c: after a control restart, never-tracked routes
		// enter the snapshot at the new process's revision, which can sit
		// BELOW revisions the gateway already applied. The applier must treat
		// the snapshot as success, keep the higher per-route state, and move
		// its delta cursor to the snapshot revision of the new control epoch.
		script.setSnapshot(t, Snapshot{
			Version:  ProtocolVersion,
			Revision: 15,
			Routes: []Route{
				wireRoute(reconcileHostnameB, 3, reconcileAgentA, reconcilePort),
				wireRoute(reconcileHostnameC, 4, reconcileAgentA, reconcilePort),
			},
		})
		mustReconcileSnapshot(t, applier)
		if revision := lookupRevision(t, table, reconcileHostnameB); revision != 7 {
			t.Fatalf("B revision = %d, want stored 7 (a regressing snapshot must never roll route state back)", revision)
		}
		if applier.LastAppliedRevision() != 15 {
			t.Fatalf("LastAppliedRevision = %d, want 15 (cursor follows the fresh control epoch)", applier.LastAppliedRevision())
		}
	})

	t.Run("concurrent lookups during replacement never observe partial state", func(t *testing.T) {
		stop := make(chan struct{})
		var waitGroup sync.WaitGroup
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// B exists in every replaced state, so a torn replacement
				// could surface as a transient miss.
				if _, err := table.Lookup(reconcileHostnameB); err != nil {
					t.Errorf("Lookup during replacement: %v", err)
					return
				}
			}
		}()
		for revision := uint64(30); revision < 50; revision++ {
			script.setSnapshot(t, Snapshot{
				Version:  ProtocolVersion,
				Revision: revision,
				Routes: []Route{
					wireRoute(reconcileHostnameB, revision, reconcileAgentA, reconcilePort),
				},
			})
			if err := applier.ReconcileSnapshot(context.Background()); err != nil {
				t.Fatalf("ReconcileSnapshot(revision %d): %v", revision, err)
			}
		}
		close(stop)
		waitGroup.Wait()
	})

	t.Run("limits widened after a control restart are accepted as authoritative", func(t *testing.T) {
		// SDD ruling for the Task 12 review minor: the gateway accepts the
		// publisher's limit values (config-epoch semantics). A restarted
		// control may carry wider limits; the applier must not invent
		// persistent widen-refusal state.
		widened := wireRoute(reconcileHostnameB, 60, reconcileAgentA, reconcilePort)
		widened.Limits = Limits{MaxStreamsPerOrigin: 64, MaxStreamsPerAgent: 128, MaxStreamsGlobal: 16384}
		script.setSnapshot(t, Snapshot{
			Version:  ProtocolVersion,
			Revision: 60,
			Routes:   []Route{widened},
		})
		mustReconcileSnapshot(t, applier)
		route, err := table.Lookup(reconcileHostnameB)
		if err != nil {
			t.Fatalf("Lookup after widened snapshot: %v", err)
		}
		if route.Limits.MaxStreamsPerOrigin != widened.Limits.MaxStreamsPerOrigin ||
			route.Limits.MaxStreamsPerAgent != widened.Limits.MaxStreamsPerAgent ||
			route.Limits.MaxStreamsGlobal != widened.Limits.MaxStreamsGlobal {
			t.Fatalf("stored limits = %+v, want the publisher's widened %+v", route.Limits, widened.Limits)
		}
	})
}

// --- named test 3: revision gaps block and force a snapshot refetch ---

func TestDeltaGapBlocksAndRefetchesSnapshot(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)

	joinA := reconcileJoin{agentRecordID: reconcileAgentA, relayPort: reconcilePort, generation: 3}
	presence := newReconcilePresence(joinA)
	table := routes.NewTable(presence)
	streams := gateway.NewStreams()
	applier := mustApplier(t, client, table, streams, nil)

	script.setSnapshot(t, Snapshot{
		Version:  ProtocolVersion,
		Revision: 10,
		Routes:   []Route{wireRoute(reconcileHostnameA, 10, reconcileAgentA, reconcilePort)},
	})
	mustReconcileSnapshot(t, applier)
	if applier.LastAppliedRevision() != 10 {
		t.Fatalf("LastAppliedRevision = %d, want 10", applier.LastAppliedRevision())
	}

	t.Run("a gap page blocks the applier without guessing", func(t *testing.T) {
		script.setDelta(t, DeltaPage{
			Version:        ProtocolVersion,
			Status:         DeltaStatusGap,
			Since:          10,
			LatestRevision: 15,
			Deltas:         nil,
		})
		err := applier.SyncDeltas(context.Background())
		if !errors.Is(err, ErrBadRevision) {
			t.Fatalf("SyncDeltas over a gap = %v, want %v", err, ErrBadRevision)
		}
		if applier.LastAppliedRevision() != 10 {
			t.Fatalf("LastAppliedRevision = %d, want still 10 (a gap must never advance the cursor)", applier.LastAppliedRevision())
		}
		if revision := lookupRevision(t, table, reconcileHostnameA); revision != 10 {
			t.Fatalf("A revision = %d, want unchanged 10", revision)
		}
		if !applier.Ready() {
			t.Fatalf("a gap must not withdraw readiness (§15.4: only lease expiry blocks new connections)")
		}
	})

	t.Run("reconcile answers a gap with a fresh snapshot and resyncs", func(t *testing.T) {
		script.setSnapshot(t, Snapshot{
			Version:  ProtocolVersion,
			Revision: 15,
			Routes:   []Route{wireRoute(reconcileHostnameA, 15, reconcileAgentA, reconcilePort)},
		})
		if err := applier.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile across a gap: %v", err)
		}
		// The recovery order must be deltas (the gap answer) then a fresh
		// snapshot; the non-fatal status ack POST may land in between.
		paths := script.recordedPaths()
		lastDeltas, lastSnapshot := -1, -1
		for index, path := range paths {
			switch path {
			case PathDeltas:
				lastDeltas = index
			case PathSnapshot:
				lastSnapshot = index
			}
		}
		if lastDeltas == -1 || lastSnapshot == -1 || lastDeltas > lastSnapshot {
			t.Fatalf("request paths = %v, want the gap delta fetch followed by a recovery snapshot", paths)
		}
		if applier.LastAppliedRevision() != 15 {
			t.Fatalf("LastAppliedRevision = %d, want 15 after the recovery snapshot", applier.LastAppliedRevision())
		}
		if revision := lookupRevision(t, table, reconcileHostnameA); revision != 15 {
			t.Fatalf("A revision = %d, want 15", revision)
		}
	})

	t.Run("an ok page resumes ordered application and explicit acks", func(t *testing.T) {
		script.setDelta(t, DeltaPage{
			Version:        ProtocolVersion,
			Status:         DeltaStatusOK,
			Since:          15,
			LatestRevision: 16,
			Deltas: []RouteDelta{{
				Revision:  16,
				Operation: RouteOperationAdd,
				Route:     wireRoute(reconcileHostnameA, 16, reconcileAgentA, reconcilePort),
			}},
		})
		if err := applier.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile with an ok page: %v", err)
		}
		if sinces := script.recordedDeltaSinces(); len(sinces) == 0 || sinces[len(sinces)-1] != 15 {
			t.Fatalf("delta fetches since = %v, want the last fetch at 15", sinces)
		}
		if revision := lookupRevision(t, table, reconcileHostnameA); revision != 16 {
			t.Fatalf("A revision = %d, want 16", revision)
		}
		ack := script.lastStatusAck(t)
		if ack.GatewayBootID != reconcileBootID || ack.LastAppliedRevision != 16 {
			t.Fatalf("last ack = boot %q revision %d, want boot %q revision 16", ack.GatewayBootID, ack.LastAppliedRevision, reconcileBootID)
		}
	})

	t.Run("per-entry rejections never drop an otherwise-valid page", func(t *testing.T) {
		script.setDelta(t, DeltaPage{
			Version:        ProtocolVersion,
			Status:         DeltaStatusOK,
			Since:          16,
			LatestRevision: 19,
			Deltas: []RouteDelta{
				{Revision: 17, Operation: RouteOperationAdd, Route: wireRoute("app.relay.sbffffffff."+reconcileZone, 17, reconcileAgentA, reconcilePort)},
				{Revision: 18, Operation: RouteOperationAdd, Route: wireRoute("-bad.relay."+reconcileNamespace+"."+reconcileZone, 18, reconcileAgentA, reconcilePort)},
				{Revision: 19, Operation: RouteOperationAdd, Route: wireRoute(reconcileHostnameC, 19, reconcileAgentA, reconcilePort)},
			},
		})
		mustApplyDeltas(t, applier)
		if _, err := table.Lookup("app.relay.sbffffffff." + reconcileZone); !errors.Is(err, routes.ErrRouteNotFound) {
			t.Fatalf("foreign-namespace delta route was admitted: %v", err)
		}
		if _, err := table.Lookup("-bad.relay." + reconcileNamespace + "." + reconcileZone); err == nil {
			t.Fatalf("RFC 1123-invalid delta route was admitted")
		}
		lookupRevision(t, table, reconcileHostnameC)
		if applier.LastAppliedRevision() != 19 {
			t.Fatalf("LastAppliedRevision = %d, want 19 (valid entries apply even when siblings are rejected)", applier.LastAppliedRevision())
		}
	})
}

// --- named test 4: lease expiry blocks new connections, preserves streams ---

func TestRouteLeaseExpiryBlocksNewButPreservesEstablishedStreams(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)

	current := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return current }

	joinA := reconcileJoin{agentRecordID: reconcileAgentA, relayPort: reconcilePort, generation: 3}
	presence := newReconcilePresence(joinA)
	table := routes.NewTable(presence, routes.WithClock(clock))
	streams := gateway.NewStreams()
	applier := mustApplier(t, client, table, streams, clock)

	script.setSnapshot(t, Snapshot{
		Version:  ProtocolVersion,
		Revision: 5,
		Routes:   []Route{wireRoute(reconcileHostnameA, 5, reconcileAgentA, reconcilePort)},
	})
	mustReconcileSnapshot(t, applier)
	if _, err := table.Lookup(reconcileHostnameA); err != nil {
		t.Fatalf("Lookup with a fresh lease: %v", err)
	}

	streamConn := newCountableConn()
	streams.Register(reconcileHostnameA, reconcileAgentA, streamConn)

	// 119 seconds in: one delayed refresh cycle must not expire anything.
	current = current.Add(119 * time.Second)
	if _, err := table.Lookup(reconcileHostnameA); err != nil {
		t.Fatalf("Lookup at +119s: %v", err)
	}

	// Past the §14 120-second lease with no applied refresh: new-connection
	// eligibility expires (generic close at lookup), established streams are
	// preserved.
	current = current.Add(2 * time.Second)
	if _, err := table.Lookup(reconcileHostnameA); !errors.Is(err, routes.ErrRouteLeaseExpired) {
		t.Fatalf("Lookup at +121s error = %v, want %v", err, routes.ErrRouteLeaseExpired)
	}
	if streamConn.closeCount() != 0 {
		t.Fatalf("lease expiry closed the established stream %d times, want 0 (§15.4)", streamConn.closeCount())
	}
	if streams.Len() != 1 {
		t.Fatalf("streams.Len() = %d, want 1 (expiry never drains)", streams.Len())
	}

	// The next applied delta (control's 30-second lease refresh) restarts the
	// lease: new connections are admitted again without any snapshot.
	script.setDelta(t, DeltaPage{
		Version:        ProtocolVersion,
		Status:         DeltaStatusOK,
		Since:          5,
		LatestRevision: 6,
		Deltas: []RouteDelta{{
			Revision:  6,
			Operation: RouteOperationLimit,
			Route:     wireRoute(reconcileHostnameA, 6, reconcileAgentA, reconcilePort),
		}},
	})
	mustApplyDeltas(t, applier)
	if _, err := table.Lookup(reconcileHostnameA); err != nil {
		t.Fatalf("Lookup after the lease-refresh delta: %v", err)
	}
	if streamConn.closeCount() != 0 {
		t.Fatalf("established stream was closed by a refresh: %d closes, want 0", streamConn.closeCount())
	}
}

// --- named test 5: explicit revoke closes established streams ---

func TestExplicitRevokeClosesEstablishedStreams(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)

	joinA := reconcileJoin{agentRecordID: reconcileAgentA, relayPort: reconcilePort, generation: 3}
	presence := newReconcilePresence(joinA)
	table := routes.NewTable(presence)
	streams := gateway.NewStreams()
	drainer := &countingDrainer{inner: streams}
	applier := mustApplier(t, client, table, drainer, nil)

	script.setSnapshot(t, Snapshot{
		Version:  ProtocolVersion,
		Revision: 5,
		Routes:   []Route{wireRoute(reconcileHostnameA, 5, reconcileAgentA, reconcilePort)},
	})
	mustReconcileSnapshot(t, applier)

	streamConn := newCountableConn()
	streams.Register(reconcileHostnameA, reconcileAgentA, streamConn)

	script.setDelta(t, DeltaPage{
		Version:        ProtocolVersion,
		Status:         DeltaStatusOK,
		Since:          5,
		LatestRevision: 6,
		Deltas: []RouteDelta{{
			Revision:  6,
			Operation: RouteOperationRevoke,
			Route:     wireRoute(reconcileHostnameA, 6, reconcileAgentA, reconcilePort),
		}},
	})
	// The wire carries revoked routes inactive; mirror control's publisher.
	script.mu.Lock()
	var page DeltaPage
	if err := json.Unmarshal(script.deltaBody, &page); err != nil {
		t.Fatalf("decode scripted delta page: %v", err)
	}
	page.Deltas[0].Route.Active = false
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("re-encode scripted delta page: %v", err)
	}
	script.deltaBody = encoded
	script.mu.Unlock()

	mustApplyDeltas(t, applier)

	if _, err := table.Lookup(reconcileHostnameA); !errors.Is(err, routes.ErrRouteInactive) {
		t.Fatalf("Lookup(revoked route) error = %v, want %v (revoked means tombstoned, not lease-expired)", err, routes.ErrRouteInactive)
	}
	if streamConn.closeCount() != 1 {
		t.Fatalf("explicit revoke closed the established stream %d times, want exactly 1 (§15.6)", streamConn.closeCount())
	}
	if streams.Len() != 0 {
		t.Fatalf("streams.Len() = %d, want 0 after an explicit revoke", streams.Len())
	}
	if drained := drainer.drained(); len(drained) != 1 || drained[0] != reconcileHostnameA {
		t.Fatalf("drained hostnames = %v, want exactly [%s]", drained, reconcileHostnameA)
	}

	t.Run("stale revoke after snapshot regression is an idempotent no-op without draining", func(t *testing.T) {
		// A control restart re-derived route state at lower revisions while
		// the gateway kept its higher per-route revision from the old
		// epoch. A subsequent revoke that is valid for the page cursor but
		// stale against the stored route must not touch or drain it.
		//
		// Step 1: epoch one carries C at revision 20 (stored, cursor 20).
		script.setSnapshot(t, Snapshot{
			Version:  ProtocolVersion,
			Revision: 20,
			Routes:   []Route{wireRoute(reconcileHostnameC, 20, reconcileAgentA, reconcilePort)},
		})
		mustReconcileSnapshot(t, applier)
		// Step 2: a restarted control (new epoch, revision 15) re-derives
		// C at revision 4; the applier keeps the stored revision 20 and
		// moves the cursor to 15.
		script.setSnapshot(t, Snapshot{
			Version:  ProtocolVersion,
			Revision: 15,
			Routes:   []Route{wireRoute(reconcileHostnameC, 4, reconcileAgentA, reconcilePort)},
		})
		mustReconcileSnapshot(t, applier)
		if revision := lookupRevision(t, table, reconcileHostnameC); revision != 20 {
			t.Fatalf("C revision = %d, want stored 20", revision)
		}

		// Step 3: a revoke at 16 supersedes the cursor (15) but not the
		// stored route (20): stale at the table, idempotent success at the
		// applier, and nothing drained.
		script.setDelta(t, func() DeltaPage {
			stale := wireRoute(reconcileHostnameC, 16, reconcileAgentA, reconcilePort)
			stale.Active = false
			return DeltaPage{
				Version:        ProtocolVersion,
				Status:         DeltaStatusOK,
				Since:          15,
				LatestRevision: 16,
				Deltas:         []RouteDelta{{Revision: 16, Operation: RouteOperationRevoke, Route: stale}},
			}
		}())
		mustApplyDeltas(t, applier)

		if revision := lookupRevision(t, table, reconcileHostnameC); revision != 20 {
			t.Fatalf("C revision = %d, want 20 (stale revoke must not touch the route)", revision)
		}
		if drained := drainer.drained(); len(drained) != 1 {
			t.Fatalf("drained hostnames = %v, want still just the earlier revoke", drained)
		}
	})
}

// --- carry-forward: uint64/int edge conversions are rejected on the wire ---

func TestWireCountsBeyondInt64RangeFailAtTheClient(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)

	// relay_port 2^63 fits no Go int; encoding/json must refuse the decode at
	// the wire boundary, before any applier conversion could exist.
	oversize := []byte(`{"version":1,"revision":2,"routes":[{"hostname":"` + reconcileHostnameA +
		`","agent_record_id":"agent-record-alpha","relay_port":9223372036854775808,` +
		`"generation":3,"session_id":"session-record-1","revision":2,"active":true,` +
		`"limits":{"max_streams_per_origin":32,"max_streams_per_agent":64,"max_streams_global":8192}}]}`)
	script.setRawSnapshot(t, oversize)

	_, err := client.FetchSnapshot(context.Background())
	if !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("FetchSnapshot with an out-of-int-range count = %v, want %v", err, ErrInvalidPayload)
	}
}

// --- applier construction fails closed ---

func TestApplierRejectsInvalidConfiguration(t *testing.T) {
	certs := newSyncTestCertificates(t)
	_, client := newScriptedControl(t, certs)
	table := routes.NewTable(newReconcilePresence())

	base := ApplierConfig{
		Client:    client,
		Table:     table,
		Namespace: reconcileNamespace,
		BootID:    reconcileBootID,
	}
	if applier, err := NewApplier(base); err != nil || applier == nil {
		t.Fatalf("NewApplier(valid config) = %v, %v", applier, err)
	}

	failures := []struct {
		name   string
		mutate func(*ApplierConfig)
	}{
		{"missing client", func(config *ApplierConfig) { config.Client = nil }},
		{"missing table", func(config *ApplierConfig) { config.Table = nil }},
		{"empty namespace", func(config *ApplierConfig) { config.Namespace = "" }},
		{"namespace outside control's shape", func(config *ApplierConfig) { config.Namespace = "my-namespace" }},
		{"uppercase namespace", func(config *ApplierConfig) { config.Namespace = "SB0123ABCD" }},
		{"empty boot id", func(config *ApplierConfig) { config.BootID = "" }},
		{"overlong boot id", func(config *ApplierConfig) { config.BootID = string(make([]byte, 65)) }},
	}
	for _, failure := range failures {
		config := base
		failure.mutate(&config)
		if applier, err := NewApplier(config); err == nil {
			t.Fatalf("NewApplier(%s) = %v, want an error", failure.name, applier)
		}
	}
}

// --- gateway-facing helpers (duplicated from internal/gateway's tests; test
// assets may not be shared across packages) ---

func dialPublic(t *testing.T, address string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatalf("dialing the public gateway address %s: %v", address, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// reconcileClientHello builds a single-record TLS ClientHello whose only
// extension is server_name carrying sni — structurally complete enough for
// the Task 2 parser to accept, ending exactly at the record boundary.
func reconcileClientHello(sni string) []byte {
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

// assertGenericClose reads the browser connection to EOF and enforces the
// §8 generic-close contract: the gateway closed the connection and not one
// byte was written back.
func assertGenericClose(t *testing.T, label string, conn net.Conn) {
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

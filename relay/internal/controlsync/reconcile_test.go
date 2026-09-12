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
	// statusFail makes PathStatus answer 503 while the snapshot/delta GETs
	// keep succeeding (M5 remediation round 2, Finding A).
	statusFail bool
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
		if script.statusFail {
			return http.StatusServiceUnavailable, []byte("status unavailable")
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, MaxRequestBodyBytes))
		if err != nil {
			t.Fatalf("scriptedControl: read status body: %v", err)
		}
		script.statusBodies = append(script.statusBodies, body)
		receipt, err := json.Marshal(StatusAckResponse{Version: ProtocolVersion, Acknowledged: true})
		if err != nil {
			t.Fatalf("scriptedControl: marshal status response: %v", err)
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

func (script *scriptedControl) setRawDelta(t *testing.T, raw []byte) {
	t.Helper()
	script.mu.Lock()
	defer script.mu.Unlock()
	script.deltaBody = raw
}

// rawSnapshotBody encodes a §11.3 snapshot payload carrying an explicit
// top-level control epoch (R2: the epoch is ALWAYS present, including on
// empty snapshots). It is built from an anonymous wire-shaped struct so the
// test pins the exact JSON on the wire, independent of the Go struct's field
// set — an applier that ignores the epoch fails these tests behaviorally.
func rawSnapshotBody(t *testing.T, epoch, revision uint64, routes []Route) []byte {
	t.Helper()
	payload := struct {
		Version  int     `json:"version"`
		Epoch    uint64  `json:"epoch"`
		Revision uint64  `json:"revision"`
		Routes   []Route `json:"routes"`
	}{Version: ProtocolVersion, Epoch: epoch, Revision: revision, Routes: routes}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal raw snapshot: %v", err)
	}
	return encoded
}

// rawDeltaBody encodes a §11.3 delta page carrying the control epoch that
// published it (R2: deltas carry the same epoch as snapshots and acks).
func rawDeltaBody(t *testing.T, epoch uint64, page DeltaPage) []byte {
	t.Helper()
	payload := struct {
		Epoch uint64 `json:"epoch"`
		DeltaPage
	}{Epoch: epoch, DeltaPage: page}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal raw delta page: %v", err)
	}
	return encoded
}

// failStatus toggles PathStatus rejection while the snapshot/delta GETs keep
// succeeding, so a test can prove the health truth follows the acknowledgement
// rather than the apply (Finding A).
func (script *scriptedControl) failStatus(fail bool) {
	script.mu.Lock()
	defer script.mu.Unlock()
	script.statusFail = fail
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
		Epoch:    100,
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
			Epoch:    100,
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
			Epoch:    100,
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
			Epoch:    100,
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

	t.Run("same-epoch regressed snapshot keeps newer per-route state", func(t *testing.T) {
		// Task 12 concern c, WITHIN one control epoch: a snapshot entry whose
		// revision sits below the stored one keeps the stored route (the
		// monotonic rule is unchanged inside a single control process
		// lifetime — R2 ruling 2). The cursor still follows the snapshot.
		script.setSnapshot(t, Snapshot{
			Version:  ProtocolVersion,
			Epoch:    100,
			Revision: 15,
			Routes: []Route{
				wireRoute(reconcileHostnameB, 3, reconcileAgentA, reconcilePort),
				wireRoute(reconcileHostnameC, 4, reconcileAgentA, reconcilePort),
			},
		})
		mustReconcileSnapshot(t, applier)
		if revision := lookupRevision(t, table, reconcileHostnameB); revision != 7 {
			t.Fatalf("B revision = %d, want stored 7 (a same-epoch regressing snapshot must never roll route state back)", revision)
		}
		if applier.LastAppliedRevision() != 15 {
			t.Fatalf("LastAppliedRevision = %d, want 15 (the cursor follows the snapshot)", applier.LastAppliedRevision())
		}
	})

	t.Run("new-epoch regressed snapshot is wholesale-authoritative", func(t *testing.T) {
		// R2 (Sol Critical-2): a control RESTART is a new epoch, and the new
		// epoch's snapshot replaces stored state wholesale — per-route
		// revisions restart within an epoch, so the old-epoch revision 7
		// carries no weight against the new epoch's authoritative 3.
		script.setRawSnapshot(t, rawSnapshotBody(t, 200, 15, []Route{
			wireRoute(reconcileHostnameB, 3, reconcileAgentA, reconcilePort),
			wireRoute(reconcileHostnameC, 4, reconcileAgentA, reconcilePort),
		}))
		mustReconcileSnapshot(t, applier)
		if revision := lookupRevision(t, table, reconcileHostnameB); revision != 3 {
			t.Fatalf("B revision = %d, want the new epoch's authoritative 3", revision)
		}
		if revision := lookupRevision(t, table, reconcileHostnameC); revision != 4 {
			t.Fatalf("C revision = %d, want the new epoch's authoritative 4", revision)
		}
		if applier.AppliedEpoch() != 200 {
			t.Fatalf("AppliedEpoch = %d, want 200", applier.AppliedEpoch())
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
				Epoch:    200,
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
			Epoch:    300,
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
		Epoch:    100,
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
			Epoch:          100,
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
			Epoch:    100,
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
			Epoch:          100,
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
		if ack.GatewayBootID != reconcileBootID || ack.LastAppliedRevision != 16 || ack.ControlEpoch != 100 {
			t.Fatalf("last ack = boot %q epoch %d revision %d, want boot %q epoch 100 revision 16", ack.GatewayBootID, ack.ControlEpoch, ack.LastAppliedRevision, reconcileBootID)
		}
	})

	t.Run("per-entry rejections never drop an otherwise-valid page", func(t *testing.T) {
		script.setDelta(t, DeltaPage{
			Version:        ProtocolVersion,
			Epoch:          100,
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
		Epoch:    100,
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
		Epoch:          100,
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
		Epoch:    100,
		Revision: 5,
		Routes:   []Route{wireRoute(reconcileHostnameA, 5, reconcileAgentA, reconcilePort)},
	})
	mustReconcileSnapshot(t, applier)

	streamConn := newCountableConn()
	streams.Register(reconcileHostnameA, reconcileAgentA, streamConn)

	script.setDelta(t, DeltaPage{
		Version:        ProtocolVersion,
		Epoch:          100,
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

	t.Run("revocation survives a control restart with regressed revisions", func(t *testing.T) {
		// INVERSION of the formerly-committed unsafe semantics (Sol mid-
		// project review Critical-2): a control restart re-derives route
		// state at lower revisions while the gateway holds higher per-route
		// revisions from the older control epoch. The new epoch's snapshot is
		// wholesale-authoritative (per-route revisions restart within the
		// epoch), so a subsequent valid revoke is EFFECTIVE — never an
		// idempotent no-op that leaves the route live and the streams up.
		//
		// Step 1: control epoch 100 carries C at revision 20 (stored 20,
		// cursor 20).
		script.setRawSnapshot(t, rawSnapshotBody(t, 100, 20,
			[]Route{wireRoute(reconcileHostnameC, 20, reconcileAgentA, reconcilePort)}))
		mustReconcileSnapshot(t, applier)
		streamConn := newCountableConn()
		streams.Register(reconcileHostnameC, reconcileAgentA, streamConn)
		if streams.Len() != 1 {
			t.Fatalf("streams.Len() = %d, want 1 after registering C's stream", streams.Len())
		}

		// Step 2: a restarted control (epoch 200, revision 15) re-derives C
		// at revision 4. The newer epoch is authoritative: the stored route
		// is replaced (revision 4, not the stale 20) and the cursor follows
		// the new epoch.
		script.setRawSnapshot(t, rawSnapshotBody(t, 200, 15,
			[]Route{wireRoute(reconcileHostnameC, 4, reconcileAgentA, reconcilePort)}))
		mustReconcileSnapshot(t, applier)
		if revision := lookupRevision(t, table, reconcileHostnameC); revision != 4 {
			t.Fatalf("C revision = %d, want the new epoch's authoritative 4", revision)
		}

		// Step 3: a revoke at 16 — valid for the new epoch's cursor (15) and
		// its stored revision (4) — must tombstone C and drain the
		// established stream. The formerly-committed behavior kept C live at
		// the stale revision 20, mapped the stale revoke to idempotent
		// success, and drained nothing.
		inactive := wireRoute(reconcileHostnameC, 16, reconcileAgentA, reconcilePort)
		inactive.Active = false
		script.setRawDelta(t, rawDeltaBody(t, 200, DeltaPage{
			Version:        ProtocolVersion,
			Status:         DeltaStatusOK,
			Since:          15,
			LatestRevision: 16,
			Deltas:         []RouteDelta{{Revision: 16, Operation: RouteOperationRevoke, Route: inactive}},
		}))
		mustApplyDeltas(t, applier)

		if _, err := table.Lookup(reconcileHostnameC); !errors.Is(err, routes.ErrRouteInactive) {
			t.Fatalf("Lookup(revoked route) error = %v, want %v (the revoke must be effective)", err, routes.ErrRouteInactive)
		}
		if streamConn.closeCount() != 1 {
			t.Fatalf("revoked route's established stream closed %d times, want exactly 1 (§15.6)", streamConn.closeCount())
		}
		if streams.Len() != 0 {
			t.Fatalf("streams.Len() = %d, want 0 after the effective revoke", streams.Len())
		}
		if drained := drainer.drained(); len(drained) != 2 || drained[1] != reconcileHostnameC {
			t.Fatalf("drained hostnames = %v, want the earlier revoke followed by %s", drained, reconcileHostnameC)
		}
	})
}

// TestRevocationSurvivesControlRestartWithRegressedRevisions is the named
// regression test for the Sol mid-project review Critical-2, reproducing its
// exact scenario end to end: an old epoch leaves a high stored per-route
// revision; control restarts and the new epoch serves regressed revisions; a
// revoke that is valid within the new epoch must make the route go ABSENT and
// drain its established streams. The formerly-committed behavior turned that
// revoke into an idempotent no-op (ErrStaleRevision mapped to success) and the
// route stayed live until its lease lapsed.
func TestRevocationSurvivesControlRestartWithRegressedRevisions(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)

	joinC := reconcileJoin{agentRecordID: reconcileAgentA, relayPort: reconcilePort, generation: 3}
	presence := newReconcilePresence(joinC)
	table := routes.NewTable(presence)
	streams := gateway.NewStreams()
	drainer := &countingDrainer{inner: streams}
	applier := mustApplier(t, client, table, drainer, nil)

	// Epoch one: the route is published at the old epoch's high revision
	// (time-seeded counters make cross-epoch revision comparisons
	// meaningless — exactly the hazard under review).
	script.setRawSnapshot(t, rawSnapshotBody(t, 100, 1750000100,
		[]Route{wireRoute(reconcileHostnameC, 1750000100, reconcileAgentA, reconcilePort)}))
	mustReconcileSnapshot(t, applier)
	lookupRevision(t, table, reconcileHostnameC)

	streamConn := newCountableConn()
	streams.Register(reconcileHostnameC, reconcileAgentA, streamConn)

	// Control restarts; the fresh epoch's publisher re-derives C at revision
	// 4 — far below the stored revision 1750000100. The newer epoch is
	// wholesale-authoritative: the stored route is replaced, not preserved.
	script.setRawSnapshot(t, rawSnapshotBody(t, 200, 1750000115,
		[]Route{wireRoute(reconcileHostnameC, 4, reconcileAgentA, reconcilePort)}))
	mustReconcileSnapshot(t, applier)
	if revision := lookupRevision(t, table, reconcileHostnameC); revision != 4 {
		t.Fatalf("C revision = %d, want the new epoch's 4 (epoch authority replaces per-route state)", revision)
	}

	// The new epoch revokes C. The revoke is valid within its own epoch and
	// must be effective: the route goes absent and the established stream is
	// drained (§15.6).
	inactive := wireRoute(reconcileHostnameC, 1750000116, reconcileAgentA, reconcilePort)
	inactive.Active = false
	script.setRawDelta(t, rawDeltaBody(t, 200, DeltaPage{
		Version:        ProtocolVersion,
		Status:         DeltaStatusOK,
		Since:          1750000115,
		LatestRevision: 1750000116,
		Deltas:         []RouteDelta{{Revision: 1750000116, Operation: RouteOperationRevoke, Route: inactive}},
	}))
	mustApplyDeltas(t, applier)

	if _, err := table.Lookup(reconcileHostnameC); !errors.Is(err, routes.ErrRouteInactive) {
		t.Fatalf("Lookup(revoked route) error = %v, want %v", err, routes.ErrRouteInactive)
	}
	if streamConn.closeCount() != 1 {
		t.Fatalf("revoked route's established stream closed %d times, want exactly 1", streamConn.closeCount())
	}
	if streams.Len() != 0 {
		t.Fatalf("streams.Len() = %d, want 0 after the effective revoke", streams.Len())
	}
	if drained := drainer.drained(); len(drained) != 1 || drained[0] != reconcileHostnameC {
		t.Fatalf("drained hostnames = %v, want exactly [%s]", drained, reconcileHostnameC)
	}
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

// --- R2: epoch authority (Sol Critical-2 remediation) ---

func TestSnapshotFromOlderEpochIsRejectedAndReconciles(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)

	joinC := reconcileJoin{agentRecordID: reconcileAgentA, relayPort: reconcilePort, generation: 3}
	presence := newReconcilePresence(joinC)
	table := routes.NewTable(presence)
	streams := gateway.NewStreams()
	applier := mustApplier(t, client, table, streams, nil)

	script.setRawSnapshot(t, rawSnapshotBody(t, 200, 30,
		[]Route{wireRoute(reconcileHostnameC, 30, reconcileAgentA, reconcilePort)}))
	mustReconcileSnapshot(t, applier)

	// An OLDER epoch's snapshot is rejected wholesale: no mutation, no
	// cursor movement, readiness untouched.
	script.setRawSnapshot(t, rawSnapshotBody(t, 100, 50,
		[]Route{wireRoute(reconcileHostnameC, 50, reconcileAgentA, reconcilePort)}))
	err := applier.ReconcileSnapshot(context.Background())
	if !errors.Is(err, ErrStaleControlEpoch) {
		t.Fatalf("ReconcileSnapshot(older epoch) = %v, want %v", err, ErrStaleControlEpoch)
	}
	if revision := lookupRevision(t, table, reconcileHostnameC); revision != 30 {
		t.Fatalf("C revision = %d, want unchanged 30 (a stale-epoch snapshot must never mutate state)", revision)
	}
	if applier.LastAppliedRevision() != 30 || applier.AppliedEpoch() != 200 {
		t.Fatalf("state moved: epoch %d revision %d, want epoch 200 revision 30", applier.AppliedEpoch(), applier.LastAppliedRevision())
	}
	if !applier.Ready() {
		t.Fatalf("a stale-epoch rejection must not withdraw readiness")
	}

	// The reconcile half: a fresh snapshot from a NEWER epoch converges.
	script.setRawSnapshot(t, rawSnapshotBody(t, 300, 35,
		[]Route{wireRoute(reconcileHostnameC, 31, reconcileAgentA, reconcilePort)}))
	mustReconcileSnapshot(t, applier)
	if revision := lookupRevision(t, table, reconcileHostnameC); revision != 31 {
		t.Fatalf("C revision = %d, want the newer epoch's 31", revision)
	}
	if applier.AppliedEpoch() != 300 {
		t.Fatalf("AppliedEpoch = %d, want 300", applier.AppliedEpoch())
	}
}

func TestDeltaPageFromWrongEpochIsNeverApplied(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)

	joinC := reconcileJoin{agentRecordID: reconcileAgentA, relayPort: reconcilePort, generation: 3}
	presence := newReconcilePresence(joinC)
	table := routes.NewTable(presence)
	streams := gateway.NewStreams()
	applier := mustApplier(t, client, table, streams, nil)

	script.setRawSnapshot(t, rawSnapshotBody(t, 200, 30,
		[]Route{wireRoute(reconcileHostnameC, 30, reconcileAgentA, reconcilePort)}))
	mustReconcileSnapshot(t, applier)

	touched := func(op string) RouteDelta {
		route := wireRoute(reconcileHostnameC, 31, reconcileAgentA, reconcilePort)
		if op == RouteOperationRevoke {
			route.Active = false
		}
		return RouteDelta{Revision: 31, Operation: op, Route: route}
	}

	// Older-epoch page: must never be applied against newer-epoch state.
	script.setRawDelta(t, rawDeltaBody(t, 100, DeltaPage{
		Version: ProtocolVersion, Status: DeltaStatusOK, Since: 30, LatestRevision: 31,
		Deltas: []RouteDelta{touched(RouteOperationLimit)},
	}))
	if err := applier.SyncDeltas(context.Background()); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("SyncDeltas(older-epoch page) = %v, want %v", err, ErrReconcileRequired)
	}
	// Newer-epoch page: meaningless before its announcing snapshot — also
	// refused, routing the caller to reconciliation.
	script.setRawDelta(t, rawDeltaBody(t, 300, DeltaPage{
		Version: ProtocolVersion, Status: DeltaStatusOK, Since: 30, LatestRevision: 31,
		Deltas: []RouteDelta{touched(RouteOperationRevoke)},
	}))
	if err := applier.SyncDeltas(context.Background()); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("SyncDeltas(newer-epoch page) = %v, want %v", err, ErrReconcileRequired)
	}
	if revision := lookupRevision(t, table, reconcileHostnameC); revision != 30 {
		t.Fatalf("C revision = %d, want unchanged 30 (foreign-epoch deltas must never apply)", revision)
	}
	if applier.LastAppliedRevision() != 30 {
		t.Fatalf("LastAppliedRevision = %d, want unchanged 30", applier.LastAppliedRevision())
	}

	// Reconcile converts the refusal into convergence: deltas fail, then the
	// newer epoch's announcing snapshot is fetched and adopted.
	script.setRawSnapshot(t, rawSnapshotBody(t, 300, 40,
		[]Route{wireRoute(reconcileHostnameC, 40, reconcileAgentA, reconcilePort)}))
	if err := applier.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile after a foreign-epoch page: %v", err)
	}
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
		t.Fatalf("request paths = %v, want the refused delta fetch followed by a recovery snapshot", paths)
	}
	if revision := lookupRevision(t, table, reconcileHostnameC); revision != 40 {
		t.Fatalf("C revision = %d, want 40 after the epoch-300 adoption", revision)
	}
}

func TestEmptySnapshotFromNewControlBootAdoptsAndWipes(t *testing.T) {
	// T15-m1 / Sol Important-4: a restarted control that serves an EMPTY
	// snapshot (no publishable routes yet) is exactly the payload the
	// always-present epoch exists for — the newer epoch makes it wholesale-
	// authoritative even at a revision below the gateway's stored state.
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)

	joinA := reconcileJoin{agentRecordID: reconcileAgentA, relayPort: reconcilePort, generation: 3}
	presence := newReconcilePresence(joinA)
	table := routes.NewTable(presence)
	streams := gateway.NewStreams()
	drainer := &countingDrainer{inner: streams}
	applier := mustApplier(t, client, table, drainer, nil)

	script.setRawSnapshot(t, rawSnapshotBody(t, 100, 20,
		[]Route{wireRoute(reconcileHostnameA, 20, reconcileAgentA, reconcilePort)}))
	mustReconcileSnapshot(t, applier)
	lookupRevision(t, table, reconcileHostnameA)

	streamConn := newCountableConn()
	streams.Register(reconcileHostnameA, reconcileAgentA, streamConn)

	// The new control boot's EMPTY snapshot: adopted wholesale, routes
	// wiped, established streams drained.
	script.setRawSnapshot(t, rawSnapshotBody(t, 200, 9, nil))
	mustReconcileSnapshot(t, applier)
	if _, err := table.Lookup(reconcileHostnameA); !errors.Is(err, routes.ErrRouteNotFound) {
		t.Fatalf("Lookup after the empty new-epoch snapshot = %v, want %v", err, routes.ErrRouteNotFound)
	}
	if streamConn.closeCount() != 1 {
		t.Fatalf("wiped route's established stream closed %d times, want exactly 1", streamConn.closeCount())
	}
	if applier.AppliedEpoch() != 200 || applier.LastAppliedRevision() != 9 {
		t.Fatalf("epoch/revision after adoption = %d/%d, want 200/9", applier.AppliedEpoch(), applier.LastAppliedRevision())
	}
	if !applier.Ready() {
		t.Fatalf("the gateway must be ready after adopting the new (empty) boot")
	}

	// The epoch was genuinely adopted: deltas of the new epoch apply.
	revived := wireRoute(reconcileHostnameA, 10, reconcileAgentA, reconcilePort)
	script.setRawDelta(t, rawDeltaBody(t, 200, DeltaPage{
		Version: ProtocolVersion, Status: DeltaStatusOK, Since: 9, LatestRevision: 10,
		Deltas: []RouteDelta{{Revision: 10, Operation: RouteOperationAdd, Route: revived}},
	}))
	mustApplyDeltas(t, applier)
	if revision := lookupRevision(t, table, reconcileHostnameA); revision != 10 {
		t.Fatalf("A revision = %d, want 10 after the new epoch's add", revision)
	}
}

func TestStaleRevokeAgainstActiveStoredRouteForcesReconciliation(t *testing.T) {
	// R2 ruling 3 hardening (defense in depth): within one epoch a
	// stale-revision revoke against an ACTIVE stored route is divergence —
	// never an idempotent success. Reaching it requires internal divergence
	// (a coherent same-epoch cursor can never produce it), so the test
	// simulates the divergence by applying an out-of-band higher revision
	// directly to the table, then proves the applier aborts the page and
	// reconciles from a full snapshot.
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)

	joinC := reconcileJoin{agentRecordID: reconcileAgentA, relayPort: reconcilePort, generation: 3}
	presence := newReconcilePresence(joinC)
	table := routes.NewTable(presence)
	streams := gateway.NewStreams()
	drainer := &countingDrainer{inner: streams}
	applier := mustApplier(t, client, table, drainer, nil)

	script.setSnapshot(t, Snapshot{
		Version:  ProtocolVersion,
		Epoch:    100,
		Revision: 50,
		Routes:   []Route{wireRoute(reconcileHostnameC, 50, reconcileAgentA, reconcilePort)},
	})
	mustReconcileSnapshot(t, applier)
	streamConn := newCountableConn()
	streams.Register(reconcileHostnameC, reconcileAgentA, streamConn)

	// Simulated divergence: stored state outruns the page cursor.
	divergent := wireRoute(reconcileHostnameC, 60, reconcileAgentA, reconcilePort)
	if err := table.Apply(routes.Route{
		Hostname:      divergent.Hostname,
		AgentRecordID: divergent.AgentRecordID,
		RelayPort:     divergent.RelayPort,
		Generation:    divergent.Generation,
		SessionID:     divergent.SessionID,
		Revision:      divergent.Revision,
		Active:        divergent.Active,
		Limits: routes.Limits{
			MaxStreamsPerOrigin: divergent.Limits.MaxStreamsPerOrigin,
			MaxStreamsPerAgent:  divergent.Limits.MaxStreamsPerAgent,
			MaxStreamsGlobal:    divergent.Limits.MaxStreamsGlobal,
		},
	}); err != nil {
		t.Fatalf("seed divergence: %v", err)
	}

	inactive := wireRoute(reconcileHostnameC, 51, reconcileAgentA, reconcilePort)
	inactive.Active = false
	stalePage := DeltaPage{
		Version: ProtocolVersion, Status: DeltaStatusOK, Since: 50, LatestRevision: 51,
		Deltas: []RouteDelta{{Revision: 51, Operation: RouteOperationRevoke, Route: inactive}},
	}

	// Direct application (white-box, bypassing the client's page validation
	// on purpose: the page is protocol-valid but divergent from stored state).
	if err := applier.applyDeltas(stalePage); !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("applyDeltas(stale revoke on active route) = %v, want %v", err, ErrReconcileRequired)
	}
	route, err := table.Lookup(reconcileHostnameC)
	if err != nil {
		t.Fatalf("Lookup after the aborted page = %v, want nil (the route must stay live)", err)
	}
	if route.Revision != 60 || !route.Active {
		t.Fatalf("stored route = revision %d active %v, want revision 60 active (the stale revoke must not touch it)", route.Revision, route.Active)
	}
	if applier.LastAppliedRevision() != 50 {
		t.Fatalf("LastAppliedRevision = %d, want unchanged 50 (the aborted page never advances the cursor)", applier.LastAppliedRevision())
	}
	if streamConn.closeCount() != 0 {
		t.Fatalf("streams closed %d times during the aborted page, want 0", streamConn.closeCount())
	}

	// Reconcile recovers: the delta refetch fails again, the snapshot wins.
	script.setRawDelta(t, rawDeltaBody(t, 100, stalePage))
	script.setSnapshot(t, Snapshot{
		Version:  ProtocolVersion,
		Epoch:    100,
		Revision: 70,
		Routes:   []Route{wireRoute(reconcileHostnameC, 65, reconcileAgentA, reconcilePort)},
	})
	if err := applier.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile after the divergent revoke: %v", err)
	}
	if revision := lookupRevision(t, table, reconcileHostnameC); revision != 65 {
		t.Fatalf("C revision = %d, want the snapshot's 65 after reconciliation", revision)
	}

	// Now the new epoch's revoke is genuinely effective: tombstone + drain.
	revoked := wireRoute(reconcileHostnameC, 71, reconcileAgentA, reconcilePort)
	revoked.Active = false
	script.setRawDelta(t, rawDeltaBody(t, 100, DeltaPage{
		Version: ProtocolVersion, Status: DeltaStatusOK, Since: 70, LatestRevision: 71,
		Deltas: []RouteDelta{{Revision: 71, Operation: RouteOperationRevoke, Route: revoked}},
	}))
	mustApplyDeltas(t, applier)
	if _, err := table.Lookup(reconcileHostnameC); !errors.Is(err, routes.ErrRouteInactive) {
		t.Fatalf("Lookup after the effective revoke = %v, want %v", err, routes.ErrRouteInactive)
	}
	if streamConn.closeCount() != 1 {
		t.Fatalf("revoked route's stream closed %d times, want exactly 1", streamConn.closeCount())
	}
	_ = drainer
}

// --- named case: contradictory state (§15.7) ---

// TestContradictoryStateFailsClosedAtTheGateway pins §15.7 end to end over a
// real public TCP listener: tunnel presence alone never makes an agent
// generic-routable, a route whose agent/port/generation does not match online
// presence never routes, and an unknown/missing route never routes — every
// case is a generic close. The control-side route state is still applied
// faithfully (the gateway never guesses or "repairs" it); only the presence
// join decides routability.
func TestContradictoryStateFailsClosedAtTheGateway(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)

	const (
		portA = 10001 // route A's port, matched by online presence
		portB = 10002 // route B's port, NOT matched by any presence
		portC = 10003 // presence for C exists, but no route is ever published
	)
	joinA := reconcileJoin{agentRecordID: reconcileAgentA, relayPort: portA, generation: 3}
	joinC := reconcileJoin{agentRecordID: reconcileAgentA, relayPort: portC, generation: 3}
	// Presence for A and for C; C has no route (tunnel presence alone).
	presence := newReconcilePresence(joinA, joinC)
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

	expectClosed := func(t *testing.T, hostname string) {
		t.Helper()
		browser := dialPublic(t, publicAddress)
		if _, err := browser.Write(reconcileClientHello(hostname)); err != nil {
			t.Fatalf("write ClientHello for %s: %v", hostname, err)
		}
		assertGenericClose(t, hostname, browser)
	}

	// Route A matches presence; route B does not (port mismatch — the exact
	// §15.7 "route exists, generation/port mismatch" contradiction).
	script.setSnapshot(t, Snapshot{
		Version:  ProtocolVersion,
		Epoch:    100,
		Revision: 5,
		Routes: []Route{
			wireRoute(reconcileHostnameA, 5, reconcileAgentA, portA),
			wireRoute(reconcileHostnameB, 5, reconcileAgentA, portB),
		},
	})
	mustReconcileSnapshot(t, applier)

	// A routes; B's route state is stored but never joins presence.
	if _, err := table.Lookup(reconcileHostnameA); err != nil {
		t.Fatalf("Lookup(A) with matching presence: %v", err)
	}
	if _, err := table.Lookup(reconcileHostnameB); !errors.Is(err, routes.ErrPresenceAbsent) {
		t.Fatalf("Lookup(B) with mismatched presence error = %v, want ErrPresenceAbsent", err)
	}
	expectClosed(t, reconcileHostnameB)

	// C: the gateway holds online tunnel presence for exact join C, but
	// control has published no route. Tunnel presence alone never makes an
	// agent generic-routable.
	if _, ok := table.Peek(reconcileHostnameC); ok {
		t.Fatalf("hostname C unexpectedly has route state")
	}
	if _, err := table.Lookup(reconcileHostnameC); !errors.Is(err, routes.ErrRouteNotFound) {
		t.Fatalf("Lookup(C) without a route error = %v, want ErrRouteNotFound", err)
	}
	expectClosed(t, reconcileHostnameC)

	// An entirely unknown hostname is closed identically.
	expectClosed(t, "unknown."+"relay."+reconcileNamespace+"."+reconcileZone)

	// Control publishes C's route at the next revision: now — and only now —
	// the route joins its already-online presence and routes.
	script.setDelta(t, DeltaPage{
		Version:        ProtocolVersion,
		Epoch:          100,
		Status:         DeltaStatusOK,
		Since:          5,
		LatestRevision: 6,
		Deltas: []RouteDelta{{
			Revision:  6,
			Operation: RouteOperationAdd,
			Route:     wireRoute(reconcileHostnameC, 6, reconcileAgentA, portC),
		}},
	})
	mustApplyDeltas(t, applier)
	if _, err := table.Lookup(reconcileHostnameC); err != nil {
		t.Fatalf("Lookup(C) after control published the route: %v", err)
	}
}

// --- named case: contradictory state end ---

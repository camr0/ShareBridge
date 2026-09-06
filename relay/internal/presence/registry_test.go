package presence

import (
	"context"
	"errors"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sharebridge/relay/internal/frpplugin"
)

const (
	testBootID    = "gwboot-test-0001"
	testAgent     = "agent-record-1"
	testNamespace = "sbdeadbeef"
	testProxy     = "sb-sbdeadbeef"
	testPort      = 10042
)

var testNow = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: testNow}
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(d time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(d)
}

type eventSink struct {
	mu     sync.Mutex
	events []Event
}

func (sink *eventSink) ObservePresenceEvent(event Event) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.events = append(sink.events, event)
}

func (sink *eventSink) snapshot() []Event {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]Event(nil), sink.events...)
}

type drainerFunc func(agentRecordID string) int

func (drainer drainerFunc) CloseAgent(agentRecordID string) int { return drainer(agentRecordID) }

type probeSpy struct {
	mu     sync.Mutex
	calls  int
	ports  []int
	source string
	fail   bool
}

func (spy *probeSpy) probe(_ context.Context, relayPort int) (string, error) {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	spy.calls++
	spy.ports = append(spy.ports, relayPort)
	if spy.fail || spy.source == "" {
		return "", errors.New("loopback connect refused")
	}
	return spy.source, nil
}

func (spy *probeSpy) callCount() int {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return spy.calls
}

type registryFixture struct {
	t        *testing.T
	registry *Registry
	clock    *fakeClock
	sink     *eventSink
	probe    *probeSpy
}

func newRegistryFixture(t *testing.T, mutate ...func(*Config)) *registryFixture {
	t.Helper()
	fixture := &registryFixture{
		t:     t,
		clock: newFakeClock(),
		sink:  &eventSink{},
		probe: &probeSpy{},
	}
	config := Config{
		BootID:  testBootID,
		Now:     fixture.clock.Now,
		Sink:    fixture.sink,
		Probe:   fixture.probe.probe,
		Drainer: drainerFunc(func(string) int { return 0 }),
	}
	for _, apply := range mutate {
		apply(&config)
	}
	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatalf("new presence registry: %v", err)
	}
	fixture.registry = registry
	return fixture
}

func testFact(operation string, generation int, runID string) frpplugin.PresenceFact {
	return frpplugin.PresenceFact{
		Operation:     operation,
		AgentRecordID: testAgent,
		Namespace:     testNamespace,
		ProxyName:     testProxy,
		RelayPort:     testPort,
		Generation:    generation,
		RunID:         runID,
	}
}

func (fixture *registryFixture) observe(fact frpplugin.PresenceFact) {
	fixture.t.Helper()
	fixture.registry.ObserveFRPEvent(fact)
}

func (fixture *registryFixture) online(generation uint64) bool {
	fixture.t.Helper()
	return fixture.registry.Online(testAgent, testPort, generation)
}

func (fixture *registryFixture) requireOffline(generation uint64) {
	fixture.t.Helper()
	if fixture.online(generation) {
		fixture.t.Fatalf("generation %d unexpectedly online", generation)
	}
}

func (fixture *registryFixture) requireOnline(generation uint64) {
	fixture.t.Helper()
	if !fixture.online(generation) {
		fixture.t.Fatalf("generation %d unexpectedly offline", generation)
	}
}

func (fixture *registryFixture) events() []Event {
	fixture.t.Helper()
	return fixture.sink.snapshot()
}

func (fixture *registryFixture) requireEventCount(want int) {
	fixture.t.Helper()
	if got := len(fixture.events()); got != want {
		fixture.t.Fatalf("presence events = %d, want %d: %+v", got, want, fixture.events())
	}
}

// registerOnline drives one tunnel through the full probe-confirmed readiness
// path: Login, authorized NewProxy (evoking the probe), then the correlated
// NewUserConn confirmation fact.
func (fixture *registryFixture) registerOnline(generation int, runID, probeSource string) {
	fixture.t.Helper()
	fixture.observe(testFact(frpplugin.OperationLogin, generation, runID))
	fixture.observe(testFact(frpplugin.OperationNewProxy, generation, runID))
	fixture.registry.waitIdle()
	fixture.observe(frpplugin.PresenceFact{
		Operation:     frpplugin.OperationNewUserConn,
		AgentRecordID: testAgent,
		Namespace:     testNamespace,
		ProxyName:     testProxy,
		RelayPort:     testPort,
		Generation:    generation,
		RunID:         runID,
		RemoteAddr:    probeSource,
	})
	fixture.requireOnline(uint64(generation))
}

func TestPresenceRequiresProbeConfirmedNewUserConn(t *testing.T) {
	fixture := newRegistryFixture(t)
	fixture.probe.source = "127.0.0.1:55555"

	// Login alone never online.
	fixture.observe(testFact(frpplugin.OperationLogin, 1, "run-1"))
	fixture.requireOffline(1)

	// A NewUserConn before any authorized NewProxy is an unprobed local
	// connection: it confirms nothing.
	fixture.observe(frpplugin.PresenceFact{
		Operation:     frpplugin.OperationNewUserConn,
		AgentRecordID: testAgent,
		Namespace:     testNamespace,
		ProxyName:     testProxy,
		RelayPort:     testPort,
		Generation:    1,
		RunID:         "run-1",
		RemoteAddr:    "127.0.0.1:55555",
	})
	fixture.requireOffline(1)

	// Authorized NewProxy evokes exactly one bounded readiness probe but does
	// not make the tunnel online: NewProxy is pre-registration authorization.
	fixture.observe(testFact(frpplugin.OperationNewProxy, 1, "run-1"))
	fixture.registry.waitIdle()
	fixture.requireOffline(1)
	if got := fixture.probe.callCount(); got != 1 {
		t.Fatalf("probe calls after NewProxy = %d, want 1", got)
	}

	// An authenticated Ping renews nothing into existence.
	fixture.observe(testFact(frpplugin.OperationPing, 1, "run-1"))
	fixture.requireOffline(1)

	// A NewUserConn from a foreign source address is not our probe socket.
	fixture.observe(frpplugin.PresenceFact{
		Operation:     frpplugin.OperationNewUserConn,
		AgentRecordID: testAgent,
		Namespace:     testNamespace,
		ProxyName:     testProxy,
		RelayPort:     testPort,
		Generation:    1,
		RunID:         "run-1",
		RemoteAddr:    "127.0.0.1:65432",
	})
	fixture.requireOffline(1)

	// The probe-confirmed, fully correlated current-generation fact is the
	// only absent -> online transition.
	fixture.observe(frpplugin.PresenceFact{
		Operation:     frpplugin.OperationNewUserConn,
		AgentRecordID: testAgent,
		Namespace:     testNamespace,
		ProxyName:     testProxy,
		RelayPort:     testPort,
		Generation:    1,
		RunID:         "run-1",
		RemoteAddr:    "127.0.0.1:55555",
	})
	fixture.requireOnline(1)
	if got := fixture.probe.callCount(); got != 1 {
		t.Fatalf("probe calls after confirmation = %d, want 1", got)
	}

	events := fixture.events()
	fixture.requireEventCount(1)
	event := events[0]
	if event.BootID != testBootID || event.Revision != 1 || event.AgentRecordID != testAgent ||
		event.RelayPort != testPort || event.Generation != 1 || event.State != StateOnline ||
		!event.LeaseExpiresAt.Equal(testNow.Add(DefaultLeaseTTL)) {
		t.Fatalf("online event = %+v", event)
	}

	// After confirmation the probe is closed: a duplicate callback changes
	// nothing and emits nothing.
	fixture.observe(frpplugin.PresenceFact{
		Operation:     frpplugin.OperationNewUserConn,
		AgentRecordID: testAgent,
		Namespace:     testNamespace,
		ProxyName:     testProxy,
		RelayPort:     testPort,
		Generation:    1,
		RunID:         "run-1",
		RemoteAddr:    "127.0.0.1:55555",
	})
	fixture.requireOnline(1)
	fixture.requireEventCount(1)
}

func TestRegistrationFailureNeverBecomesOnlineWhilePingContinues(t *testing.T) {
	fixture := newRegistryFixture(t)
	fixture.probe.fail = true

	fixture.observe(testFact(frpplugin.OperationLogin, 1, "run-1"))
	fixture.observe(testFact(frpplugin.OperationNewProxy, 1, "run-1"))
	fixture.registry.waitIdle()
	fixture.requireOffline(1)

	// The pinned release never retries a failed registration but authenticated
	// Pings continue: none of them may present the tunnel as online, and no
	// NewUserConn fact can exist for a proxy frps never bound.
	for elapsed := time.Duration(10); elapsed <= 120*time.Second; elapsed += 10 * time.Second {
		fixture.clock.Advance(10 * time.Second)
		fixture.observe(testFact(frpplugin.OperationPing, 1, "run-1"))
		fixture.requireOffline(1)
		fixture.observe(frpplugin.PresenceFact{
			Operation:     frpplugin.OperationNewUserConn,
			AgentRecordID: testAgent,
			Namespace:     testNamespace,
			ProxyName:     testProxy,
			RelayPort:     testPort,
			Generation:    1,
			RunID:         "run-1",
			RemoteAddr:    "127.0.0.1:55555",
		})
		fixture.requireOffline(1)
	}
	fixture.requireEventCount(0)
}

func TestStaleGenerationNewUserConnNeverConfirmsCurrentGeneration(t *testing.T) {
	fixture := newRegistryFixture(t)
	fixture.probe.source = "127.0.0.1:55555"
	fixture.registerOnline(1, "run-1", "127.0.0.1:55555")

	// A replacement generation fences the old one.
	fixture.observe(testFact(frpplugin.OperationLogin, 2, "run-2"))
	fixture.requireOffline(1)
	// The gen-2 probe reports the new source address; set it before NewProxy
	// because the spy reads it at call time.
	fixture.probe.source = "127.0.0.1:55556"
	fixture.observe(testFact(frpplugin.OperationNewProxy, 2, "run-2"))
	fixture.registry.waitIdle()

	stale := []frpplugin.PresenceFact{
		// The stale generation-1 listener answering the gen-2 probe socket.
		{Operation: frpplugin.OperationNewUserConn, AgentRecordID: testAgent, Namespace: testNamespace,
			ProxyName: testProxy, RelayPort: testPort, Generation: 1, RunID: "run-1", RemoteAddr: "127.0.0.1:55556"},
		// Current generation but the old session's run id.
		{Operation: frpplugin.OperationNewUserConn, AgentRecordID: testAgent, Namespace: testNamespace,
			ProxyName: testProxy, RelayPort: testPort, Generation: 2, RunID: "run-1", RemoteAddr: "127.0.0.1:55556"},
		// Current generation and run id but a foreign source address.
		{Operation: frpplugin.OperationNewUserConn, AgentRecordID: testAgent, Namespace: testNamespace,
			ProxyName: testProxy, RelayPort: testPort, Generation: 2, RunID: "run-2", RemoteAddr: "127.0.0.1:55555"},
		// Wrong proxy name.
		{Operation: frpplugin.OperationNewUserConn, AgentRecordID: testAgent, Namespace: testNamespace,
			ProxyName: "sb-other", RelayPort: testPort, Generation: 2, RunID: "run-2", RemoteAddr: "127.0.0.1:55556"},
	}
	for index, fact := range stale {
		fixture.observe(fact)
		fixture.requireOffline(1)
		fixture.requireOffline(2)
		if got := len(fixture.events()); got != 2 {
			t.Fatalf("stale fact %d changed presence state: %d events: %+v", index, got, fixture.events())
		}
	}

	fixture.observe(frpplugin.PresenceFact{
		Operation:     frpplugin.OperationNewUserConn,
		AgentRecordID: testAgent,
		Namespace:     testNamespace,
		ProxyName:     testProxy,
		RelayPort:     testPort,
		Generation:    2,
		RunID:         "run-2",
		RemoteAddr:    "127.0.0.1:55556",
	})
	fixture.requireOnline(2)
	fixture.requireOffline(1)
	fixture.requireEventCount(3)
	events := fixture.events()
	if events[2].Generation != 2 || events[2].State != StateOnline {
		t.Fatalf("third event = %+v, want gen-2 online", events[2])
	}
}

func TestCurrentPingRenews45SecondLease(t *testing.T) {
	fixture := newRegistryFixture(t)
	fixture.probe.source = "127.0.0.1:55555"
	fixture.registerOnline(1, "run-1", "127.0.0.1:55555")

	// Without renewal the lease would lapse at testNow+45s. A Ping at +40s
	// must push the lease to +85s.
	fixture.clock.Advance(40 * time.Second)
	fixture.requireOnline(1)
	fixture.observe(testFact(frpplugin.OperationPing, 1, "run-1"))

	fixture.clock.Advance(40 * time.Second) // +80s
	fixture.requireOnline(1)
	fixture.requireEventCount(1)

	// The renewed lease lapses at +85s: expired at first observation past it,
	// exactly once.
	fixture.clock.Advance(4 * time.Second) // +84s
	fixture.requireOnline(1)
	fixture.clock.Advance(2 * time.Second) // +86s
	fixture.requireOffline(1)
	fixture.requireEventCount(2)
	offline := fixture.events()[1]
	if offline.State != StateOffline || offline.Generation != 1 || offline.Revision != 2 {
		t.Fatalf("lease expiry event = %+v", offline)
	}
	// Once absent, further Pings never resurrect: only the readiness path does.
	fixture.observe(testFact(frpplugin.OperationPing, 1, "run-1"))
	fixture.requireOffline(1)
	fixture.requireEventCount(2)
}

func TestCloseLogoutAndExpiryBecomeAbsent(t *testing.T) {
	t.Run("CloseProxy", func(t *testing.T) {
		fixture := newRegistryFixture(t)
		fixture.probe.source = "127.0.0.1:55555"
		fixture.registerOnline(1, "run-1", "127.0.0.1:55555")

		// A CloseProxy naming a different proxy never makes the tunnel absent.
		wrongProxy := testFact(frpplugin.OperationCloseProxy, 1, "run-1")
		wrongProxy.ProxyName = "sb-other"
		fixture.observe(wrongProxy)
		fixture.requireOnline(1)

		fixture.observe(testFact(frpplugin.OperationCloseProxy, 1, "run-1"))
		fixture.requireOffline(1)
		fixture.requireEventCount(2)
		if state := fixture.events()[1].State; state != StateOffline {
			t.Fatalf("CloseProxy event state = %q", state)
		}
	})

	t.Run("frps reset clears all presence and drains after the mutation", func(t *testing.T) {
		clock := newFakeClock()
		sink := &eventSink{}
		probe := &probeSpy{source: "127.0.0.1:55555"}
		var (
			registry      *Registry
			drained       []string
			onlineAtDrain bool
		)
		var err error
		registry, err = NewRegistry(Config{
			BootID: testBootID,
			Now:    clock.Now,
			Sink:   sink,
			Probe:  probe.probe,
			Drainer: drainerFunc(func(agentRecordID string) int {
				drained = append(drained, agentRecordID)
				onlineAtDrain = registry.Online(testAgent, testPort, 1)
				return 0
			}),
		})
		if err != nil {
			t.Fatalf("new presence registry: %v", err)
		}
		registry.ObserveFRPEvent(testFact(frpplugin.OperationLogin, 1, "run-1"))
		registry.ObserveFRPEvent(testFact(frpplugin.OperationNewProxy, 1, "run-1"))
		registry.waitIdle()
		registry.ObserveFRPEvent(frpplugin.PresenceFact{
			Operation:     frpplugin.OperationNewUserConn,
			AgentRecordID: testAgent,
			Namespace:     testNamespace,
			ProxyName:     testProxy,
			RelayPort:     testPort,
			Generation:    1,
			RunID:         "run-1",
			RemoteAddr:    "127.0.0.1:55555",
		})
		if !registry.Online(testAgent, testPort, 1) {
			t.Fatal("tunnel not online before the frps reset")
		}

		registry.ClearAll()
		if registry.Online(testAgent, testPort, 1) {
			t.Fatal("presence survived the frps reset")
		}
		if len(sink.snapshot()) != 2 {
			t.Fatalf("presence events = %+v, want one online and one offline", sink.snapshot())
		}
		if strings.Join(drained, ",") != testAgent {
			t.Fatalf("drained agents = %v, want [%s]", drained, testAgent)
		}
		// Mutate-before-drain (Streams ordering invariant): the presence
		// mutation must already be visible when the drain runs.
		if onlineAtDrain {
			t.Fatal("drain ran before the presence mutation committed")
		}
	})

	t.Run("lease expiry", func(t *testing.T) {
		fixture := newRegistryFixture(t)
		fixture.probe.source = "127.0.0.1:55555"
		fixture.registerOnline(1, "run-1", "127.0.0.1:55555")

		fixture.clock.Advance(DefaultLeaseTTL - time.Second)
		fixture.requireOnline(1)
		fixture.clock.Advance(time.Second) // exactly at the lease boundary
		fixture.requireOffline(1)
		fixture.requireEventCount(2)
	})

	// A bare client logout sends no plugin callback in the pinned release; the
	// Pings stop, so the same lease expiry path makes the tunnel absent.
}

func TestReplacementGenerationFencesOldTunnel(t *testing.T) {
	fixture := newRegistryFixture(t)
	fixture.probe.source = "127.0.0.1:55555"
	fixture.registerOnline(1, "run-1", "127.0.0.1:55555")

	// The replacement Login fences the old tunnel immediately, before the new
	// generation is even confirmed.
	fixture.observe(testFact(frpplugin.OperationLogin, 2, "run-2"))
	fixture.requireOffline(1)
	fixture.requireEventCount(2)
	if state := fixture.events()[1].State; state != StateOffline {
		t.Fatalf("fence event state = %q", state)
	}

	// Stale old-generation facts after the fence: ignored, never resurrecting
	// gen 1 and never touching gen 2.
	fixture.observe(testFact(frpplugin.OperationPing, 1, "run-1"))
	fixture.observe(frpplugin.PresenceFact{
		Operation:     frpplugin.OperationNewUserConn,
		AgentRecordID: testAgent,
		Namespace:     testNamespace,
		ProxyName:     testProxy,
		RelayPort:     testPort,
		Generation:    1,
		RunID:         "run-1",
		RemoteAddr:    "127.0.0.1:55555",
	})
	fixture.observe(testFact(frpplugin.OperationNewProxy, 1, "run-1"))
	fixture.requireOffline(1)
	fixture.requireEventCount(2)
	if got := fixture.probe.callCount(); got != 1 {
		t.Fatalf("stale gen-1 facts re-armed a probe: %d calls", got)
	}

	// The new generation confirms with its own probe.
	fixture.observe(testFact(frpplugin.OperationNewProxy, 2, "run-2"))
	fixture.registry.waitIdle()
	fixture.observe(frpplugin.PresenceFact{
		Operation:     frpplugin.OperationNewUserConn,
		AgentRecordID: testAgent,
		Namespace:     testNamespace,
		ProxyName:     testProxy,
		RelayPort:     testPort,
		Generation:    2,
		RunID:         "run-2",
		RemoteAddr:    "127.0.0.1:55555",
	})
	fixture.requireOnline(2)
	fixture.requireOffline(1)
	fixture.requireEventCount(3)

	// A lower-generation Login is replayed history: ignored, gen 2 stays.
	fixture.observe(testFact(frpplugin.OperationLogin, 1, "run-old"))
	fixture.requireOnline(2)
	fixture.requireEventCount(3)

	// An equal-generation Login cannot exist from the plugin (issued-at fence);
	// if one ever arrived it must not reset or re-fence the live tunnel.
	fixture.observe(testFact(frpplugin.OperationLogin, 2, "run-2-forged"))
	fixture.requireOnline(2)
	fixture.requireEventCount(3)

	// A higher generation fences the live one again.
	fixture.observe(testFact(frpplugin.OperationLogin, 3, "run-3"))
	fixture.requireOffline(2)
	fixture.requireEventCount(4)
	fixture.requireOffline(3)
}

func TestBootIDAndRevisionMonotonic(t *testing.T) {
	fixture := newRegistryFixture(t)
	fixture.probe.source = "127.0.0.1:55555"

	fixture.registerOnline(1, "run-1", "127.0.0.1:55555")                // online
	fixture.observe(testFact(frpplugin.OperationCloseProxy, 1, "run-1")) // offline
	fixture.registerOnline(2, "run-2", "127.0.0.1:55555")                // online
	fixture.clock.Advance(DefaultLeaseTTL + time.Second)
	fixture.requireOffline(2) // expiry offline

	events := fixture.events()
	fixture.requireEventCount(4)
	wantState := []string{StateOnline, StateOffline, StateOnline, StateOffline}
	wantGeneration := []uint64{1, 1, 2, 2}
	for index, event := range events {
		if event.BootID != testBootID {
			t.Fatalf("event %d boot ID = %q, want %q", index, event.BootID, testBootID)
		}
		if event.Revision != uint64(index+1) {
			t.Fatalf("event %d revision = %d, want %d", index, event.Revision, index+1)
		}
		if event.State != wantState[index] || event.Generation != wantGeneration[index] {
			t.Fatalf("event %d = %+v, want state %q generation %d", index, event, wantState[index], wantGeneration[index])
		}
	}
}

func TestDelayedPingDoesNotFlapLease(t *testing.T) {
	fixture := newRegistryFixture(t)
	fixture.probe.source = "127.0.0.1:55555"
	fixture.registerOnline(1, "run-1", "127.0.0.1:55555")

	// Four nominal 10-second Ping opportunities per 45-second lease, so one
	// delayed or missed heartbeat must never flap presence (§14, GATE
	// EVIDENCE §23.7: a frozen real frpc produced a 14.01 s Ping gap). Each
	// step checks online immediately BEFORE the late Ping renews.
	delays := []time.Duration{14 * time.Second, 19 * time.Second, 11 * time.Second}
	for index, delay := range delays {
		fixture.clock.Advance(delay)
		fixture.requireOnline(1)
		if got := len(fixture.events()); got != 1 {
			t.Fatalf("delayed ping %d flapped presence: %d events: %+v", index, got, fixture.events())
		}
		fixture.observe(testFact(frpplugin.OperationPing, 1, "run-1"))
		fixture.requireOnline(1)
	}

	// Only the true expiry — no Ping for a full 45 seconds — transitions
	// offline, exactly once.
	fixture.clock.Advance(DefaultLeaseTTL + time.Second)
	fixture.requireOffline(1)
	fixture.requireEventCount(2)
	if state := fixture.events()[1].State; state != StateOffline {
		t.Fatalf("expiry event state = %q", state)
	}
}

func TestPresenceFactRangeChecksFailClosed(t *testing.T) {
	fixture := newRegistryFixture(t)
	fixture.probe.source = "127.0.0.1:55555"

	// Negative generation and out-of-domain ports never create state: the
	// uint64<->int edge is range-checked at the fact boundary (Task 13
	// carry-forward), never converted blindly.
	negative := testFact(frpplugin.OperationLogin, -1, "run-neg")
	fixture.observe(negative)
	fixture.requireOffline(uint64(math.MaxInt64) + 1)

	for _, port := range []int{0, -1, 65536, 99999} {
		badPort := testFact(frpplugin.OperationLogin, 1, "run-port")
		badPort.RelayPort = port
		fixture.observe(badPort)
		fixture.requireOffline(1)
	}

	// The int generation domain joins exactly into uint64 space, including
	// its maximum.
	fixture.registerOnline(math.MaxInt64, "run-max", "127.0.0.1:55555")
	// A route whose uint64 generation is outside the int credential domain can
	// never join: the registry only ever holds range-checked int generations.
	fixture.requireOffline(math.MaxUint64)
	fixture.requireOffline(uint64(math.MaxInt64) + 1)
}

func TestLoopbackProbeRespectsBudget(t *testing.T) {
	t.Run("connected listener confirms on the first attempt", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer listener.Close()
		port := listener.Addr().(*net.TCPAddr).Port

		accepted := make(chan net.Conn, 1)
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				accepted <- conn
				conn.Close()
			}
		}()

		probe := newLoopbackProbe(DefaultProbeMaxAttempts, DefaultProbeBackoff, DefaultProbeHardDeadline, DefaultProbeDialTimeout)
		started := time.Now()
		source, probeErr := probe(context.Background(), port)
		if probeErr != nil {
			t.Fatalf("probe: %v", probeErr)
		}
		host, portText, err := net.SplitHostPort(source)
		if err != nil || host != "127.0.0.1" {
			t.Fatalf("probe source = %q (%v), want a loopback host:port", source, err)
		}
		if sourcePort, err := strconv.Atoi(portText); err != nil || sourcePort <= 0 {
			t.Fatalf("probe source port = %q, want an ephemeral port", portText)
		}
		select {
		case conn := <-accepted:
			conn.Close()
		case <-time.After(time.Second):
			t.Fatal("probe connection never reached the proxy listener")
		}
		if elapsed := time.Since(started); elapsed > DefaultProbeHardDeadline {
			t.Fatalf("first-attempt probe took %s, hard deadline %s", elapsed, DefaultProbeHardDeadline)
		}
	})

	t.Run("refused proxy gives up within the hard deadline", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close() // reliably refused from here on

		probe := newLoopbackProbe(5, 25*time.Millisecond, 300*time.Millisecond, 100*time.Millisecond)
		started := time.Now()
		if _, probeErr := probe(context.Background(), port); probeErr == nil {
			t.Fatal("probe of a refused port succeeded")
		}
		if elapsed := time.Since(started); elapsed > 900*time.Millisecond {
			t.Fatalf("refused probe took %s, want within the 300ms hard deadline plus slop", elapsed)
		}
	})

	t.Run("deadline is re-checked before each wait", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close()

		// An absurd backoff must be capped at the remaining budget: the whole
		// loop has to finish at the 200ms hard deadline, not after one 5s wait.
		probe := newLoopbackProbe(5, 5*time.Second, 200*time.Millisecond, 50*time.Millisecond)
		started := time.Now()
		_, probeErr := probe(context.Background(), port)
		if probeErr == nil {
			t.Fatal("probe of a refused port succeeded")
		}
		if !strings.Contains(probeErr.Error(), "deadline") {
			t.Fatalf("probe error = %v, want the deadline-exceeded flavor", probeErr)
		}
		if elapsed := time.Since(started); elapsed > 900*time.Millisecond {
			t.Fatalf("probe waited past the hard deadline: %s", elapsed)
		}
	})
}

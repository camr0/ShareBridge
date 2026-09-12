package limits

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"sharebridge/relay/internal/clienthello"
)

// testSourceAddr builds a public peer address for one source IP. Tests share
// the IP value to exercise the per-source-IP ceiling and vary it to exercise
// distinct-key map growth.
func testSourceAddr(ip string) net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(ip), Port: 41234}
}

func mustAdmitConnection(t *testing.T, limiter *Limiter, ip string) *ConnectionLease {
	t.Helper()
	lease, err := limiter.AdmitConnection(testSourceAddr(ip))
	if err != nil {
		t.Fatalf("AdmitConnection(%s) = %v, want nil", ip, err)
	}
	return lease
}

func mustAdmitStream(t *testing.T, limiter *Limiter, conn *ConnectionLease, hostname, agentRecordID string) *StreamLease {
	t.Helper()
	return mustAdmitStreamWithRequest(t, limiter, conn, StreamRequest{Hostname: hostname, AgentRecordID: agentRecordID})
}

func mustAdmitStreamWithRequest(t *testing.T, limiter *Limiter, conn *ConnectionLease, request StreamRequest) *StreamLease {
	t.Helper()
	lease, err := limiter.AdmitStream(conn, request)
	if err != nil {
		t.Fatalf("AdmitStream(%s, %s) = %v, want nil", request.Hostname, request.AgentRecordID, err)
	}
	return lease
}

func assertIdleLimiter(t *testing.T, limiter *Limiter) {
	t.Helper()
	if got := limiter.ActiveConnections(); got != 0 {
		t.Fatalf("ActiveConnections() = %d, want 0 after every lease was released", got)
	}
	if got := limiter.ActiveStreams(); got != 0 {
		t.Fatalf("ActiveStreams() = %d, want 0 after every lease was released", got)
	}
	if got := limiter.TrackedSourceIPs(); got != 0 {
		t.Fatalf("TrackedSourceIPs() = %d, want 0 (the IP map is deleted at zero)", got)
	}
	if got := limiter.TrackedOrigins(); got != 0 {
		t.Fatalf("TrackedOrigins() = %d, want 0 (the origin map is deleted at zero)", got)
	}
}

// TestLimitsDefaultsMatchSpec14 pins every §14 MVP default so a relaxed
// production default is a test failure, never a silent drift.
func TestLimitsDefaultsMatchSpec14(t *testing.T) {
	config := DefaultConfig()

	integerDefaults := []struct {
		name string
		got  int
		want int
	}{
		{"MaxStreamsPerSourceIP", config.MaxStreamsPerSourceIP, 16},
		{"MaxStreamsPerOrigin", config.MaxStreamsPerOrigin, 32},
		{"MaxStreamsPerAgent", config.MaxStreamsPerAgent, 64},
		{"MaxStreamsGlobal", config.MaxStreamsGlobal, 8192},
		{"ProxiesPerAgent", config.ProxiesPerAgent, 1},
		{"MaxHelloBytes", config.MaxHelloBytes, 64 << 10},
		{"MaxTrackedAgents", config.MaxTrackedAgents, 4096},
	}
	for _, check := range integerDefaults {
		if check.got != check.want {
			t.Errorf("DefaultConfig().%s = %d, want the §14 default %d", check.name, check.got, check.want)
		}
	}

	durationDefaults := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"HelloTimeout", config.HelloTimeout, 5 * time.Second},
		{"DialTimeout", config.DialTimeout, 2 * time.Second},
		{"IdleTimeout", config.IdleTimeout, 5 * time.Minute},
		{"AbsoluteLifetime", config.AbsoluteLifetime, 24 * time.Hour},
	}
	for _, check := range durationDefaults {
		if check.got != check.want {
			t.Errorf("DefaultConfig().%s = %v, want the §14 default %v", check.name, check.got, check.want)
		}
	}
}

// TestLimitsHelloDefaultsMatchParserBounds keeps the limits config and the
// clienthello parser from drifting apart: the configured hello ceiling and
// deadline are exactly the values the parser enforces (spec §14).
func TestLimitsHelloDefaultsMatchParserBounds(t *testing.T) {
	config := DefaultConfig()
	if config.MaxHelloBytes != clienthello.MaxBufferedBytes {
		t.Errorf("MaxHelloBytes = %d, want the parser ceiling %d", config.MaxHelloBytes, clienthello.MaxBufferedBytes)
	}
	if config.HelloTimeout != clienthello.ReadTimeout {
		t.Errorf("HelloTimeout = %v, want the parser deadline %v", config.HelloTimeout, clienthello.ReadTimeout)
	}
}

func TestLimitsEnforcesPerSourceIPCeiling(t *testing.T) {
	config := DefaultConfig()
	config.MaxStreamsPerSourceIP = 3
	config.MaxStreamsGlobal = 100
	limiter := NewLimiter(config)

	leases := make([]*ConnectionLease, 0, 3)
	for i := 0; i < 3; i++ {
		leases = append(leases, mustAdmitConnection(t, limiter, "203.0.113.9"))
	}
	if _, err := limiter.AdmitConnection(testSourceAddr("203.0.113.9")); !errors.Is(err, ErrSourceIPLimit) {
		t.Fatalf("AdmitConnection over the per-IP ceiling error = %v, want %v", err, ErrSourceIPLimit)
	}

	// A different source IP is unaffected by another IP's saturation.
	other := mustAdmitConnection(t, limiter, "203.0.113.10")
	other.Release()

	for _, lease := range leases {
		lease.Release()
	}
	assertIdleLimiter(t, limiter)

	if _, err := limiter.AdmitConnection(testSourceAddr("203.0.113.9")); err != nil {
		t.Fatalf("AdmitConnection after releasing the ceiling = %v, want nil", err)
	}
}

func TestLimitsEnforcesPerOriginAndPerAgentCeilings(t *testing.T) {
	t.Run("per exact origin", func(t *testing.T) {
		config := DefaultConfig()
		config.MaxStreamsPerOrigin = 2
		config.MaxStreamsPerAgent = 64
		config.MaxStreamsGlobal = 100
		limiter := NewLimiter(config)

		conn := mustAdmitConnection(t, limiter, "198.51.100.1")
		first := mustAdmitStream(t, limiter, conn, "app.relay.ns1.example.com", "agent-a")
		second := mustAdmitStream(t, limiter, conn, "app.relay.ns1.example.com", "agent-a")
		if _, err := limiter.AdmitStream(conn, StreamRequest{Hostname: "app.relay.ns1.example.com", AgentRecordID: "agent-a"}); !errors.Is(err, ErrOriginLimit) {
			t.Fatalf("third stream on a saturated exact origin error = %v, want %v", err, ErrOriginLimit)
		}
		// A different exact origin on the same agent still admits.
		other := mustAdmitStream(t, limiter, conn, "photos.relay.ns1.example.com", "agent-a")

		second.Release()
		other.Release()
		first.Release()
		conn.Release()
		assertIdleLimiter(t, limiter)
	})

	t.Run("per agent across distinct origins", func(t *testing.T) {
		config := DefaultConfig()
		config.MaxStreamsPerOrigin = 32
		config.MaxStreamsPerAgent = 1
		config.MaxStreamsGlobal = 100
		limiter := NewLimiter(config)

		conn := mustAdmitConnection(t, limiter, "198.51.100.2")
		first := mustAdmitStream(t, limiter, conn, "app.relay.ns1.example.com", "agent-a")
		if _, err := limiter.AdmitStream(conn, StreamRequest{Hostname: "photos.relay.ns1.example.com", AgentRecordID: "agent-a"}); !errors.Is(err, ErrAgentLimit) {
			t.Fatalf("second stream on a saturated agent error = %v, want %v", err, ErrAgentLimit)
		}
		// A different agent still admits.
		other := mustAdmitStream(t, limiter, conn, "photos.relay.ns1.example.com", "agent-b")

		other.Release()
		first.Release()
		conn.Release()
		assertIdleLimiter(t, limiter)
	})
}

func TestLimitsEnforcesGlobalCeiling(t *testing.T) {
	config := DefaultConfig()
	config.MaxStreamsGlobal = 2
	config.MaxStreamsPerSourceIP = 100
	limiter := NewLimiter(config)

	first := mustAdmitConnection(t, limiter, "203.0.113.20")
	second := mustAdmitConnection(t, limiter, "203.0.113.21")
	if _, err := limiter.AdmitConnection(testSourceAddr("203.0.113.22")); !errors.Is(err, ErrGlobalLimit) {
		t.Fatalf("AdmitConnection over the global ceiling error = %v, want %v", err, ErrGlobalLimit)
	}

	first.Release()
	third := mustAdmitConnection(t, limiter, "203.0.113.22")
	if got := limiter.ActiveConnections(); got != 2 {
		t.Fatalf("ActiveConnections() = %d after re-admission, want 2", got)
	}
	third.Release()
	second.Release()
	assertIdleLimiter(t, limiter)
}

// TestLimitsAdmissionIsAtomicAcrossCeilings proves agent→origin acquisition
// is all-or-nothing: a rejected origin must not leak an agent slot.
func TestLimitsAdmissionIsAtomicAcrossCeilings(t *testing.T) {
	config := DefaultConfig()
	config.MaxStreamsPerOrigin = 1
	config.MaxStreamsPerAgent = 4
	config.MaxStreamsGlobal = 10
	limiter := NewLimiter(config)

	conn := mustAdmitConnection(t, limiter, "198.51.100.5")
	first := mustAdmitStream(t, limiter, conn, "app.relay.ns1.example.com", "agent-a")

	if _, err := limiter.AdmitStream(conn, StreamRequest{Hostname: "app.relay.ns1.example.com", AgentRecordID: "agent-b"}); !errors.Is(err, ErrOriginLimit) {
		t.Fatalf("origin-saturated admission error = %v, want %v", err, ErrOriginLimit)
	}
	if got := limiter.ActiveStreamsForAgent("agent-b"); got != 0 {
		t.Fatalf("ActiveStreamsForAgent(agent-b) = %d after a rejected origin admission, want 0 (no partial agent slot)", got)
	}
	if got := limiter.TrackedOrigins(); got != 1 {
		t.Fatalf("TrackedOrigins() = %d after a rejected admission, want 1", got)
	}

	first.Release()
	conn.Release()
	assertIdleLimiter(t, limiter)
}

// TestLimitsReleasesEveryCeilingIdempotently covers the counter-release
// contract for both lease phases: double release changes nothing, foreign or
// already-released connection leases are refused, and churn returns every
// counter to zero.
func TestLimitsReleasesEveryCeilingIdempotently(t *testing.T) {
	config := DefaultConfig()
	config.MaxStreamsGlobal = 4
	config.MaxStreamsPerSourceIP = 4
	config.MaxStreamsPerOrigin = 4
	config.MaxStreamsPerAgent = 4
	limiter := NewLimiter(config)

	for i := 0; i < 3; i++ {
		conn := mustAdmitConnection(t, limiter, "203.0.113.30")
		stream := mustAdmitStream(t, limiter, conn, "app.relay.ns1.example.com", "agent-a")
		stream.Release()
		stream.Release()
		conn.Release()
		conn.Release()
		assertIdleLimiter(t, limiter)
	}

	conn := mustAdmitConnection(t, limiter, "203.0.113.30")
	conn.Release()
	if _, err := limiter.AdmitStream(conn, StreamRequest{Hostname: "app.relay.ns1.example.com", AgentRecordID: "agent-a"}); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("AdmitStream with a released connection lease error = %v, want %v", err, ErrLeaseInvalid)
	}

	foreign := NewLimiter(DefaultConfig())
	foreignConn := mustAdmitConnection(t, foreign, "203.0.113.31")
	if _, err := limiter.AdmitStream(foreignConn, StreamRequest{Hostname: "app.relay.ns1.example.com", AgentRecordID: "agent-a"}); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("AdmitStream with a foreign limiter's lease error = %v, want %v", err, ErrLeaseInvalid)
	}
	foreignConn.Release()
	assertIdleLimiter(t, limiter)
}

// TestLimitsTrackedStateStaysBoundedByGlobalCeiling proves the transient
// per-IP/agent/origin maps can never grow past the global ceiling: every
// live entry holds at least one admitted connection, and entries are deleted
// at zero.
func TestLimitsTrackedStateStaysBoundedByGlobalCeiling(t *testing.T) {
	config := DefaultConfig()
	config.MaxStreamsGlobal = 4
	config.MaxStreamsPerSourceIP = 100
	config.MaxStreamsPerOrigin = 100
	config.MaxStreamsPerAgent = 100
	limiter := NewLimiter(config)

	connections := make([]*ConnectionLease, 0, 4)
	streams := make([]*StreamLease, 0, 4)
	for i := 0; i < 4; i++ {
		conn := mustAdmitConnection(t, limiter, fmt.Sprintf("203.0.113.%d", 40+i))
		stream := mustAdmitStream(t, limiter, conn,
			fmt.Sprintf("route-%d.relay.ns1.example.com", i),
			fmt.Sprintf("agent-%d", i))
		connections = append(connections, conn)
		streams = append(streams, stream)
	}

	if _, err := limiter.AdmitConnection(testSourceAddr("203.0.113.99")); !errors.Is(err, ErrGlobalLimit) {
		t.Fatalf("fifth distinct-IP connection error = %v, want %v", err, ErrGlobalLimit)
	}
	if got := limiter.TrackedSourceIPs(); got > config.MaxStreamsGlobal {
		t.Fatalf("TrackedSourceIPs() = %d, want <= the global ceiling %d", got, config.MaxStreamsGlobal)
	}
	if got := limiter.TrackedOrigins(); got > config.MaxStreamsGlobal {
		t.Fatalf("TrackedOrigins() = %d, want <= the global ceiling %d", got, config.MaxStreamsGlobal)
	}
	if got := limiter.TrackedAgents(); got > config.MaxStreamsGlobal {
		t.Fatalf("TrackedAgents() = %d, want <= the global ceiling %d", got, config.MaxStreamsGlobal)
	}

	for _, stream := range streams {
		stream.Release()
	}
	for _, conn := range connections {
		conn.Release()
	}
	assertIdleLimiter(t, limiter)
	// The per-agent byte map is a persistent operator gauge, so it stays at
	// its bounded size after every stream ends — it never grows further.
	if got := limiter.TrackedAgents(); got > config.MaxStreamsGlobal {
		t.Fatalf("TrackedAgents() = %d after all releases, want <= the global ceiling %d", got, config.MaxStreamsGlobal)
	}
}

// TestLimitsCapsTrackedAgentByteState proves the persistent per-agent byte
// map — the one map that outlives individual streams — is bounded and fails
// closed once its documented ceiling is reached. Eviction would silently
// corrupt long-lived operator accounting, so a new untracked agent is
// refused instead.
func TestLimitsCapsTrackedAgentByteState(t *testing.T) {
	config := DefaultConfig()
	config.MaxTrackedAgents = 2
	limiter := NewLimiter(config)

	for _, agent := range []string{"agent-a", "agent-b"} {
		conn := mustAdmitConnection(t, limiter, "203.0.113.50")
		stream := mustAdmitStream(t, limiter, conn, "app.relay.ns1.example.com", agent)
		stream.Release()
		conn.Release()
	}
	if got := limiter.TrackedAgents(); got != 2 {
		t.Fatalf("TrackedAgents() = %d, want 2 tracked agents", got)
	}

	conn := mustAdmitConnection(t, limiter, "203.0.113.50")
	if _, err := limiter.AdmitStream(conn, StreamRequest{Hostname: "app.relay.ns1.example.com", AgentRecordID: "agent-c"}); !errors.Is(err, ErrAgentCapacity) {
		t.Fatalf("AdmitStream for a new agent beyond the tracking ceiling error = %v, want %v", err, ErrAgentCapacity)
	}
	// An already-tracked agent keeps working at the ceiling.
	stream := mustAdmitStream(t, limiter, conn, "app.relay.ns1.example.com", "agent-a")
	stream.Release()
	conn.Release()
	assertIdleLimiter(t, limiter)
	if got := limiter.TrackedAgents(); got != 2 {
		t.Fatalf("TrackedAgents() = %d after churn, want the tracked agent set to stay at the 2-agent ceiling", got)
	}
}

func TestLimitsTracksPerAgentBytesAndAlertsAtThreshold(t *testing.T) {
	config := DefaultConfig()
	config.AgentBytesAlertThreshold = 100
	var alertsMu sync.Mutex
	var alerts []Saturation
	config.OnSaturation = func(saturation Saturation) {
		alertsMu.Lock()
		defer alertsMu.Unlock()
		alerts = append(alerts, saturation)
	}
	limiter := NewLimiter(config)

	conn := mustAdmitConnection(t, limiter, "203.0.113.60")
	stream := mustAdmitStream(t, limiter, conn, "app.relay.ns1.example.com", "agent-a")

	stream.AddBytes(60)
	stream.AddBytes(60)
	stream.AddBytes(0)
	stream.AddBytes(-5) // non-positive counts are ignored
	if got := limiter.BytesForAgent("agent-a"); got != 120 {
		t.Fatalf("BytesForAgent(agent-a) = %d, want 120 (positive counts only)", got)
	}

	alertsMu.Lock()
	firstAlerts := append([]Saturation(nil), alerts...)
	alertsMu.Unlock()
	if len(firstAlerts) != 1 {
		t.Fatalf("saturation alerts = %d, want exactly 1 at the byte threshold", len(firstAlerts))
	}
	alert := firstAlerts[0]
	if alert.Kind != SaturationAgentBytes || alert.Key != "agent-a" || alert.Bytes != 120 || alert.Threshold != 100 {
		t.Fatalf("saturation alert = %+v, want agent_bytes for agent-a at 120 bytes against threshold 100", alert)
	}

	// The alert fires once per crossing, never once per byte.
	stream.AddBytes(1000)
	alertsMu.Lock()
	if len(alerts) != 1 {
		t.Fatalf("saturation alerts = %d after the crossing, want 1 (no per-byte alert storm)", len(alerts))
	}
	alertsMu.Unlock()

	stream.Release()
	conn.Release()
	// Byte counters are operator gauges: they persist after the stream ends.
	if got := limiter.BytesForAgent("agent-a"); got != 1120 {
		t.Fatalf("BytesForAgent(agent-a) = %d after release, want the persistent counter 1120", got)
	}
	assertIdleLimiter(t, limiter)
}

func TestLimitsSaturationAlertsReportEveryCeiling(t *testing.T) {
	config := DefaultConfig()
	config.MaxStreamsGlobal = 1
	config.MaxStreamsPerSourceIP = 1
	config.MaxStreamsPerOrigin = 100
	config.MaxStreamsPerAgent = 1
	var alertsMu sync.Mutex
	var kinds []string
	config.OnSaturation = func(saturation Saturation) {
		alertsMu.Lock()
		defer alertsMu.Unlock()
		kinds = append(kinds, saturation.Kind)
	}
	limiter := NewLimiter(config)

	first := mustAdmitConnection(t, limiter, "203.0.113.70")
	if _, err := limiter.AdmitConnection(testSourceAddr("203.0.113.71")); !errors.Is(err, ErrGlobalLimit) {
		t.Fatalf("global rejection error = %v, want %v", err, ErrGlobalLimit)
	}
	stream := mustAdmitStream(t, limiter, first, "app.relay.ns1.example.com", "agent-a")
	// A different origin on the same saturated agent reaches the agent check.
	if _, err := limiter.AdmitStream(first, StreamRequest{Hostname: "photos.relay.ns1.example.com", AgentRecordID: "agent-a"}); !errors.Is(err, ErrAgentLimit) {
		t.Fatalf("agent rejection error = %v, want %v", err, ErrAgentLimit)
	}

	alertsMu.Lock()
	gotKinds := append([]string(nil), kinds...)
	alertsMu.Unlock()
	want := map[string]bool{SaturationGlobal: false, SaturationAgent: false}
	for _, kind := range gotKinds {
		if _, ok := want[kind]; ok {
			want[kind] = true
		}
	}
	for kind, seen := range want {
		if !seen {
			t.Fatalf("saturation alerts %v missing kind %q", gotKinds, kind)
		}
	}

	stream.Release()
	first.Release()
	assertIdleLimiter(t, limiter)
}

// TestLimitsRouteGlobalCeilingTightensOnly pins the route-distributed global
// stream ceiling (audit #2): it is enforced at the same mutex-protected
// admission point as the other ceilings, it may only tighten the process
// ceiling, and a zero route value means "no override" so the process ceiling
// stays authoritative.
func TestLimitsRouteGlobalCeilingTightensOnly(t *testing.T) {
	t.Run("a tighter route global limits concurrency at admission", func(t *testing.T) {
		config := DefaultConfig()
		config.MaxStreamsGlobal = 10
		config.MaxStreamsPerSourceIP = 100
		config.MaxStreamsPerOrigin = 100
		config.MaxStreamsPerAgent = 100
		var alertsMu sync.Mutex
		var alerts []Saturation
		config.OnSaturation = func(saturation Saturation) {
			alertsMu.Lock()
			defer alertsMu.Unlock()
			alerts = append(alerts, saturation)
		}
		limiter := NewLimiter(config)

		request := StreamRequest{
			Hostname:            "app.relay.ns1.example.com",
			AgentRecordID:       "agent-a",
			MaxStreamsPerOrigin: 100,
			MaxStreamsPerAgent:  100,
			MaxStreamsGlobal:    2,
		}
		connections := make([]*ConnectionLease, 0, 3)
		streams := make([]*StreamLease, 0, 2)
		for i := 0; i < 2; i++ {
			conn := mustAdmitConnection(t, limiter, fmt.Sprintf("198.51.100.%d", 20+i))
			stream, err := limiter.AdmitStream(conn, request)
			if err != nil {
				t.Fatalf("AdmitStream %d under the route global ceiling = %v, want nil", i, err)
			}
			connections = append(connections, conn)
			streams = append(streams, stream)
		}

		// The third public connection is admitted at the pre-parse process
		// ceiling (10), but the route-tightened global rejects the stream at
		// admission, before any agent slot or origin slot is taken.
		third := mustAdmitConnection(t, limiter, "198.51.100.30")
		connections = append(connections, third)
		if _, err := limiter.AdmitStream(third, request); !errors.Is(err, ErrGlobalLimit) {
			t.Fatalf("route-tightened global admission error = %v, want %v", err, ErrGlobalLimit)
		}
		if got := limiter.ActiveStreams(); got != 2 {
			t.Fatalf("ActiveStreams() = %d after the route-global rejection, want 2", got)
		}
		if got := limiter.ActiveStreamsForAgent("agent-a"); got != 2 {
			t.Fatalf("ActiveStreamsForAgent(agent-a) = %d after the route-global rejection, want 2 (the rejected stream must not leak an agent slot)", got)
		}

		alertsMu.Lock()
		var sawGlobal bool
		for _, alert := range alerts {
			if alert.Kind == SaturationGlobal && alert.Limit == 2 {
				sawGlobal = true
			}
		}
		alertsMu.Unlock()
		if !sawGlobal {
			t.Fatalf("route-tightened global rejection did not emit a %s saturation alert with limit 2: %+v", SaturationGlobal, alerts)
		}

		for _, stream := range streams {
			stream.Release()
		}
		for _, conn := range connections {
			conn.Release()
		}
		assertIdleLimiter(t, limiter)
	})

	t.Run("a looser route global never raises the process ceiling", func(t *testing.T) {
		config := DefaultConfig()
		config.MaxStreamsGlobal = 2
		config.MaxStreamsPerSourceIP = 100
		limiter := NewLimiter(config)

		if got := effectiveLimit(config.MaxStreamsGlobal, 999); got != config.MaxStreamsGlobal {
			t.Fatalf("effectiveLimit(process=%d, route=999) = %d, want the process ceiling %d", config.MaxStreamsGlobal, got, config.MaxStreamsGlobal)
		}
		loose := StreamRequest{Hostname: "app.relay.ns1.example.com", AgentRecordID: "agent-a", MaxStreamsGlobal: 999}
		first := mustAdmitConnection(t, limiter, "198.51.100.40")
		firstStream, err := limiter.AdmitStream(first, loose)
		if err != nil {
			t.Fatalf("AdmitStream under the process ceiling with a looser route value = %v, want nil", err)
		}
		second := mustAdmitConnection(t, limiter, "198.51.100.41")
		secondStream, err := limiter.AdmitStream(second, loose)
		if err != nil {
			t.Fatalf("second AdmitStream under the process ceiling = %v, want nil", err)
		}
		// The process ceiling still wins: the route value cannot admit a third.
		if _, err := limiter.AdmitConnection(testSourceAddr("198.51.100.42")); !errors.Is(err, ErrGlobalLimit) {
			t.Fatalf("process global ceiling with a looser route value error = %v, want %v", err, ErrGlobalLimit)
		}
		secondStream.Release()
		second.Release()
		firstStream.Release()
		first.Release()
		assertIdleLimiter(t, limiter)
	})

	t.Run("a zero route global means no override", func(t *testing.T) {
		config := DefaultConfig()
		config.MaxStreamsGlobal = 2
		config.MaxStreamsPerSourceIP = 100
		limiter := NewLimiter(config)

		if got := effectiveLimit(config.MaxStreamsGlobal, 0); got != config.MaxStreamsGlobal {
			t.Fatalf("effectiveLimit(process=%d, route=0) = %d, want the process ceiling %d (zero is unset)", config.MaxStreamsGlobal, got, config.MaxStreamsGlobal)
		}
		unset := StreamRequest{Hostname: "app.relay.ns1.example.com", AgentRecordID: "agent-a"}
		first := mustAdmitConnection(t, limiter, "198.51.100.50")
		firstStream := mustAdmitStreamWithRequest(t, limiter, first, unset)
		second := mustAdmitConnection(t, limiter, "198.51.100.51")
		secondStream := mustAdmitStreamWithRequest(t, limiter, second, unset)
		if _, err := limiter.AdmitConnection(testSourceAddr("198.51.100.52")); !errors.Is(err, ErrGlobalLimit) {
			t.Fatalf("process global ceiling with an unset route value error = %v, want %v", err, ErrGlobalLimit)
		}
		secondStream.Release()
		second.Release()
		firstStream.Release()
		first.Release()
		assertIdleLimiter(t, limiter)
	})

	t.Run("per-origin and per-agent still apply independently", func(t *testing.T) {
		config := DefaultConfig()
		config.MaxStreamsGlobal = 100
		config.MaxStreamsPerSourceIP = 100
		config.MaxStreamsPerOrigin = 100
		config.MaxStreamsPerAgent = 100
		limiter := NewLimiter(config)

		conn := mustAdmitConnection(t, limiter, "198.51.100.60")
		originRequest := StreamRequest{
			Hostname:            "app.relay.ns1.example.com",
			AgentRecordID:       "agent-a",
			MaxStreamsPerOrigin: 1,
			MaxStreamsGlobal:    50,
		}
		first := mustAdmitStreamWithRequest(t, limiter, conn, originRequest)
		if _, err := limiter.AdmitStream(conn, originRequest); !errors.Is(err, ErrOriginLimit) {
			t.Fatalf("per-origin rejection under an unsaturated route global error = %v, want %v", err, ErrOriginLimit)
		}

		agentRequest := StreamRequest{
			Hostname:           "photos.relay.ns1.example.com",
			AgentRecordID:      "agent-b",
			MaxStreamsPerAgent: 1,
			MaxStreamsGlobal:   50,
		}
		second := mustAdmitStreamWithRequest(t, limiter, conn, agentRequest)
		if _, err := limiter.AdmitStream(conn, StreamRequest{
			Hostname:           "videos.relay.ns1.example.com",
			AgentRecordID:      "agent-b",
			MaxStreamsPerAgent: 1,
			MaxStreamsGlobal:   50,
		}); !errors.Is(err, ErrAgentLimit) {
			t.Fatalf("per-agent rejection under an unsaturated route global error = %v, want %v", err, ErrAgentLimit)
		}

		second.Release()
		first.Release()
		conn.Release()
		assertIdleLimiter(t, limiter)
	})
}

// TestLimitsRouteGlobalRejectionReleasesEveryCeiling walks the route-global
// rejection path and proves the new admission check acquires nothing: the
// connection lease release still returns every counter to zero.
func TestLimitsRouteGlobalRejectionReleasesEveryCeiling(t *testing.T) {
	config := DefaultConfig()
	config.MaxStreamsGlobal = 4
	config.MaxStreamsPerSourceIP = 4
	config.MaxStreamsPerOrigin = 4
	config.MaxStreamsPerAgent = 4
	limiter := NewLimiter(config)
	request := StreamRequest{Hostname: "app.relay.ns1.example.com", AgentRecordID: "agent-a", MaxStreamsGlobal: 1}

	for i := 0; i < 3; i++ {
		first := mustAdmitConnection(t, limiter, "203.0.113.80")
		stream := mustAdmitStreamWithRequest(t, limiter, first, request)
		second := mustAdmitConnection(t, limiter, "203.0.113.81")
		if _, err := limiter.AdmitStream(second, request); !errors.Is(err, ErrGlobalLimit) {
			t.Fatalf("iteration %d route-global rejection error = %v, want %v", i, err, ErrGlobalLimit)
		}
		second.Release()
		stream.Release()
		stream.Release()
		first.Release()
		first.Release()
		assertIdleLimiter(t, limiter)
	}
}

// TestLimitsRouteCeilingsCanOnlyTightenProcessDefaults pins the safety
// direction: a control-distributed route ceiling may tighten the operator's
// process ceiling but can never loosen it.
func TestLimitsRouteCeilingsCanOnlyTightenProcessDefaults(t *testing.T) {
	t.Run("route ceiling tighter than the process default applies", func(t *testing.T) {
		config := DefaultConfig()
		config.MaxStreamsPerOrigin = 8
		config.MaxStreamsPerAgent = 64
		config.MaxStreamsGlobal = 100
		limiter := NewLimiter(config)

		conn := mustAdmitConnection(t, limiter, "198.51.100.7")
		first := mustAdmitStream(t, limiter, conn, "app.relay.ns1.example.com", "agent-a")
		request := StreamRequest{Hostname: "app.relay.ns1.example.com", AgentRecordID: "agent-a", MaxStreamsPerOrigin: 1}
		if _, err := limiter.AdmitStream(conn, request); !errors.Is(err, ErrOriginLimit) {
			t.Fatalf("route-tightened origin admission error = %v, want %v", err, ErrOriginLimit)
		}
		first.Release()
		conn.Release()
		assertIdleLimiter(t, limiter)
	})

	t.Run("route ceiling looser than the process default is ignored", func(t *testing.T) {
		config := DefaultConfig()
		config.MaxStreamsPerOrigin = 1
		config.MaxStreamsPerAgent = 64
		config.MaxStreamsGlobal = 100
		limiter := NewLimiter(config)

		conn := mustAdmitConnection(t, limiter, "198.51.100.8")
		first := mustAdmitStream(t, limiter, conn, "app.relay.ns1.example.com", "agent-a")
		request := StreamRequest{Hostname: "app.relay.ns1.example.com", AgentRecordID: "agent-a", MaxStreamsPerOrigin: 99}
		if _, err := limiter.AdmitStream(conn, request); !errors.Is(err, ErrOriginLimit) {
			t.Fatalf("loosened route ceiling admission error = %v, want the process ceiling to win with %v", err, ErrOriginLimit)
		}
		first.Release()
		conn.Release()
		assertIdleLimiter(t, limiter)
	})
}

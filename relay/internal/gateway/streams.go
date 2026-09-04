package gateway

import (
	"net"
	"sync"
)

// Stream is one live public connection indexed under both its exact route
// hostname and its agent record (spec §8). Close is the single exit path:
// revocation, lockdown, and normal connection end all funnel through it, and
// it is idempotent, so an already-closed stream is never closed twice.
type Stream struct {
	// Hostname is the exact route hostname the stream was accepted for.
	Hostname string
	// AgentRecordID is the agent record serving the stream.
	AgentRecordID string

	registry  *Streams
	conn      net.Conn
	closeOnce sync.Once
	closeErr  error
}

// Close closes the stream's connection and removes it from both registry
// indexes. Repeated calls change nothing beyond returning the first close
// result.
func (stream *Stream) Close() error {
	stream.closeOnce.Do(stream.performClose)
	return stream.closeErr
}

// closeIfOpen closes the stream and reports whether this call performed the
// close, letting revocation count exactly the streams it closed itself.
func (stream *Stream) closeIfOpen() bool {
	performed := false
	stream.closeOnce.Do(func() {
		performed = true
		stream.performClose()
	})
	return performed
}

func (stream *Stream) performClose() {
	stream.registry.remove(stream)
	stream.closeErr = stream.conn.Close()
}

// Streams is the active-stream registry: every live gateway connection
// indexed by exact route hostname and by agent record, so a route revoke or
// an agent lockdown closes matching connections immediately (spec §8, §15.6).
// Matching is exact — there is no wildcard matching anywhere. Safe for
// concurrent use; every registration must be paired with a Close on all exit
// paths so the registry never grows without bound (spec §14).
type Streams struct {
	mu      sync.Mutex
	byRoute map[string]map[*Stream]struct{}
	byAgent map[string]map[*Stream]struct{}
	live    int
}

// NewStreams returns an empty active-stream registry.
func NewStreams() *Streams {
	return &Streams{
		byRoute: make(map[string]map[*Stream]struct{}),
		byAgent: make(map[string]map[*Stream]struct{}),
	}
}

// Register indexes a live public stream under its exact route hostname and
// its agent record. The hostname is stored and matched verbatim; callers
// pass the already-normalized hostname from the route lookup.
func (registry *Streams) Register(hostname, agentRecordID string, conn net.Conn) *Stream {
	stream := &Stream{
		Hostname:      hostname,
		AgentRecordID: agentRecordID,
		registry:      registry,
		conn:          conn,
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.addTo(registry.byRoute, hostname, stream)
	registry.addTo(registry.byAgent, agentRecordID, stream)
	registry.live++
	return stream
}

// RegisterAdmitted registers a live public stream like Register and then —
// while still holding the registry lock, so no CloseRoute or CloseAgent can
// drain the indexes in between — invokes admit to re-verify route liveness
// at the exact moment of registration. This closes the lookup→register
// window: a stream whose route was revoked after the caller's lookup but
// before this call is un-indexed again as if never registered, so a stream
// can never outlive the revoke that preceded its registration (spec §8).
//
// The fence holds only under a control-side ordering invariant: every path
// that revokes, re-points, or expires routing state (table Revoke/Apply,
// presence expiry) must mutate that state BEFORE it calls CloseRoute or
// CloseAgent. The drain must follow, never precede, the mutation that
// motivates it — a control task that drains first and tombstones or expires
// presence afterwards would drain before a not-yet-registered stream and
// then flip the table, silently reopening this race. Tasks 10–14 (control
// sync, presence registry, FRP plugin) must preserve this order.
//
// admit runs under the registry lock: it must never call back into the
// registry, and nothing it calls may acquire the registry lock while holding
// another lock. Because admit re-routes through the presence check,
// presence.Online executes under the registry lock too — the Task 14
// presence implementation must answer from in-memory state without blocking
// or re-entering the registry.
//
// On success it returns the committed stream. When admit fails it returns a
// nil stream together with admit's error; the registry is left exactly as if
// the stream had never been registered, and the caller closes the connection
// generically like any other rejection.
func (registry *Streams) RegisterAdmitted(hostname, agentRecordID string, conn net.Conn, admit func() error) (*Stream, error) {
	stream := &Stream{
		Hostname:      hostname,
		AgentRecordID: agentRecordID,
		registry:      registry,
		conn:          conn,
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.addTo(registry.byRoute, hostname, stream)
	registry.addTo(registry.byAgent, agentRecordID, stream)
	if err := admit(); err != nil {
		registry.removeFrom(registry.byRoute, hostname, stream)
		registry.removeFrom(registry.byAgent, agentRecordID, stream)
		return nil, err
	}
	registry.live++
	return stream, nil
}

// Len reports how many streams are currently indexed.
func (registry *Streams) Len() int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.live
}

// CloseRoute closes and deregisters every stream bound to the exact relay
// hostname and returns how many open streams it closed. Streams already
// closed elsewhere are neither recounted nor re-closed.
//
// Ordering invariant (see RegisterAdmitted): the control path that motivates
// this drain — a route revoke or drop — must apply the table/presence
// mutation first and call CloseRoute only afterwards. Draining before the
// mutation lets a not-yet-registered stream slip past the fence and outlive
// the revoke.
func (registry *Streams) CloseRoute(hostname string) int {
	return registry.closeAll(registry.takeRoute(hostname))
}

// CloseAgent closes and deregisters every stream bound to the agent record —
// the lockdown path (spec §13.4, §15.6) — and returns how many open streams
// it closed.
//
// Ordering invariant (see RegisterAdmitted): the control path that motivates
// this drain — a lockdown or agent state change — must apply the
// table/presence mutation first and call CloseAgent only afterwards, for the
// same reason as CloseRoute.
func (registry *Streams) CloseAgent(agentRecordID string) int {
	return registry.closeAll(registry.takeAgent(agentRecordID))
}

func (registry *Streams) closeAll(streams []*Stream) int {
	closed := 0
	for _, stream := range streams {
		if stream.closeIfOpen() {
			closed++
		}
	}
	return closed
}

// takeRoute detaches the exact-hostname bucket under the lock, un-indexes
// its streams from the agent index, and returns them for closing without the
// lock held.
func (registry *Streams) takeRoute(hostname string) []*Stream {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	streams := registry.detach(registry.byRoute, hostname)
	for _, stream := range streams {
		registry.removeFrom(registry.byAgent, stream.AgentRecordID, stream)
	}
	return streams
}

// takeAgent is takeRoute for the lockdown path: it detaches the agent-record
// bucket and un-indexes its streams from the route index.
func (registry *Streams) takeAgent(agentRecordID string) []*Stream {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	streams := registry.detach(registry.byAgent, agentRecordID)
	for _, stream := range streams {
		registry.removeFrom(registry.byRoute, stream.Hostname, stream)
	}
	return streams
}

// detach removes and returns the whole bucket for key, decrementing the live
// count once per stream.
func (registry *Streams) detach(index map[string]map[*Stream]struct{}, key string) []*Stream {
	bucket := index[key]
	delete(index, key)
	streams := make([]*Stream, 0, len(bucket))
	for stream := range bucket {
		streams = append(streams, stream)
		registry.live--
	}
	return streams
}

// remove deregisters one stream from both indexes; a stream that is already
// gone is a no-op. The live count drops at most once per close.
func (registry *Streams) remove(stream *Stream) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	_, inRoute := registry.byRoute[stream.Hostname][stream]
	_, inAgent := registry.byAgent[stream.AgentRecordID][stream]
	if !inRoute && !inAgent {
		return
	}
	if inRoute {
		registry.removeFrom(registry.byRoute, stream.Hostname, stream)
	}
	if inAgent {
		registry.removeFrom(registry.byAgent, stream.AgentRecordID, stream)
	}
	registry.live--
}

func (registry *Streams) addTo(index map[string]map[*Stream]struct{}, key string, stream *Stream) {
	bucket := index[key]
	if bucket == nil {
		bucket = make(map[*Stream]struct{})
		index[key] = bucket
	}
	bucket[stream] = struct{}{}
}

func (registry *Streams) removeFrom(index map[string]map[*Stream]struct{}, key string, stream *Stream) {
	bucket := index[key]
	if bucket == nil {
		return
	}
	if _, ok := bucket[stream]; !ok {
		return
	}
	delete(bucket, stream)
	if len(bucket) == 0 {
		delete(index, key)
	}
}

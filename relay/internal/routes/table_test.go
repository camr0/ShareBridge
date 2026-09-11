package routes

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
)

const (
	testRelayHostname = "app.relay.ns1.sharebridgeusercontent.com"
	testAgentRecordID = "agent-record-1"
	testSessionID     = "session-record-1"
	testRelayPort     = 7101
	testGeneration    = 3
)

// presenceKey identifies the §8 presence join: one agent record, one relay
// port, one tunnel generation.
type presenceKey struct {
	agentRecordID string
	relayPort     int
	generation    uint64
}

// stubPresence is a configurable Presence whose online set tests mutate; it
// stands in for the Task 14 leased registry.
type stubPresence struct {
	mu     sync.Mutex
	online map[presenceKey]bool
}

func newStubPresence(online ...presenceKey) *stubPresence {
	stub := &stubPresence{online: make(map[presenceKey]bool)}
	for _, key := range online {
		stub.online[key] = true
	}
	return stub
}

func (stub *stubPresence) setOnline(key presenceKey, up bool) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.online[key] = up
}

func (stub *stubPresence) Online(agentRecordID string, relayPort int, generation uint64) bool {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.online[presenceKey{agentRecordID, relayPort, generation}]
}

// testRoute builds an active route carrying the §14 default concurrency
// ceilings.
func testRoute(hostname string, revision uint64) Route {
	return Route{
		Hostname:      hostname,
		AgentRecordID: testAgentRecordID,
		RelayPort:     testRelayPort,
		Generation:    testGeneration,
		SessionID:     testSessionID,
		Revision:      revision,
		Active:        true,
		Limits: Limits{
			MaxStreamsPerOrigin: 32,
			MaxStreamsPerAgent:  64,
			MaxStreamsGlobal:    8192,
		},
	}
}

// assertRouteEqual compares whole Route values; every field is comparable.
func assertRouteEqual(t *testing.T, got, want Route) {
	t.Helper()
	if got != want {
		t.Fatalf("route mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestRouteTableAcceptsOnlyExactActiveHostname(t *testing.T) {
	presence := newStubPresence(presenceKey{
		agentRecordID: testAgentRecordID,
		relayPort:     testRelayPort,
		generation:    testGeneration,
	})
	table := NewTable(presence)

	if err := table.Apply(testRoute(testRelayHostname, 7)); err != nil {
		t.Fatalf("Apply(active route) = %v, want nil", err)
	}
	route, err := table.Lookup(testRelayHostname)
	if err != nil {
		t.Fatalf("Lookup(exact hostname) = %v, want nil", err)
	}
	assertRouteEqual(t, route, testRoute(testRelayHostname, 7))

	// Normalization happens once, at insertion: a mixed-case delta with a
	// trailing dot lands on the same exact entry.
	if err := table.Apply(testRoute("App.Relay.NS1.Sharebridgeusercontent.COM.", 8)); err != nil {
		t.Fatalf("Apply(mixed-case trailing-dot route) = %v, want nil", err)
	}
	route, err = table.Lookup(testRelayHostname)
	if err != nil {
		t.Fatalf("Lookup after normalized re-apply = %v, want nil", err)
	}
	assertRouteEqual(t, route, testRoute(testRelayHostname, 8))

	// Lookup never normalizes: non-normalized spellings stay misses so the
	// hot path is a pure exact match.
	for _, spelling := range []string{
		"App.Relay.NS1.Sharebridgeusercontent.COM",
		testRelayHostname + ".",
	} {
		if _, err := table.Lookup(spelling); !errors.Is(err, ErrRouteNotFound) {
			t.Fatalf("Lookup(%q) error = %v, want %v (lookup must never normalize)", spelling, err, ErrRouteNotFound)
		}
	}

	if _, err := table.Lookup("other.relay.ns1.sharebridgeusercontent.com"); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("Lookup(unknown hostname) error = %v, want %v", err, ErrRouteNotFound)
	}
}

func TestRouteTableRejectsBareWildcardRandomAndTombstoned(t *testing.T) {
	presence := newStubPresence(presenceKey{
		agentRecordID: testAgentRecordID,
		relayPort:     testRelayPort,
		generation:    testGeneration,
	})
	table := NewTable(presence)

	if err := table.Apply(testRoute(testRelayHostname, 7)); err != nil {
		t.Fatalf("Apply(active route) = %v, want nil", err)
	}

	t.Run("wildcard route entries are refused", func(t *testing.T) {
		wildcard := testRoute("*.relay.ns1.sharebridgeusercontent.com", 9)
		if err := table.Apply(wildcard); !errors.Is(err, ErrInvalidHostname) {
			t.Fatalf("Apply(wildcard hostname) error = %v, want %v", err, ErrInvalidHostname)
		}
		// Defense in depth: even if a wildcard somehow arrived, no lookup
		// can ever match it.
		if _, err := table.Lookup(wildcard.Hostname); !errors.Is(err, ErrRouteNotFound) {
			t.Fatalf("Lookup(wildcard hostname) error = %v, want %v", err, ErrRouteNotFound)
		}
	})

	t.Run("bare namespace and random hostnames never match", func(t *testing.T) {
		misses := []string{
			"ns1.sharebridgeusercontent.com",                // the bare namespace
			"random99.relay.ns1.sharebridgeusercontent.com", // a random child
			testRelayHostname + ".extra.example.com",        // a suffix extension
		}
		for _, missed := range misses {
			if _, err := table.Lookup(missed); !errors.Is(err, ErrRouteNotFound) {
				t.Fatalf("Lookup(%q) error = %v, want %v (no wildcard or prefix matching exists)", missed, err, ErrRouteNotFound)
			}
		}
	})

	t.Run("revoked routes stop routing", func(t *testing.T) {
		if err := table.Revoke("never-added.relay.ns1.sharebridgeusercontent.com", 99); !errors.Is(err, ErrRouteNotFound) {
			t.Fatalf("Revoke(unknown hostname) error = %v, want %v", err, ErrRouteNotFound)
		}
		if err := table.Revoke(testRelayHostname, 8); err != nil {
			t.Fatalf("Revoke(active route) = %v, want nil", err)
		}
		if _, err := table.Lookup(testRelayHostname); !errors.Is(err, ErrRouteInactive) {
			t.Fatalf("Lookup(revoked hostname) error = %v, want %v", err, ErrRouteInactive)
		}
	})

	t.Run("snapshot tombstones never route", func(t *testing.T) {
		tombstone := testRoute("tombstoned.relay.ns1.sharebridgeusercontent.com", 4)
		tombstone.Active = false
		if err := table.Apply(tombstone); err != nil {
			t.Fatalf("Apply(tombstoned route) = %v, want nil", err)
		}
		if _, err := table.Lookup(tombstone.Hostname); !errors.Is(err, ErrRouteInactive) {
			t.Fatalf("Lookup(tombstoned hostname) error = %v, want %v", err, ErrRouteInactive)
		}
	})
}

func TestRouteTableRequiresMatchingAgentPortGeneration(t *testing.T) {
	exactKey := presenceKey{agentRecordID: testAgentRecordID, relayPort: testRelayPort, generation: testGeneration}
	wrongAgent := presenceKey{agentRecordID: "other-agent-record", relayPort: testRelayPort, generation: testGeneration}
	wrongPort := presenceKey{agentRecordID: testAgentRecordID, relayPort: testRelayPort + 1, generation: testGeneration}
	wrongGeneration := presenceKey{agentRecordID: testAgentRecordID, relayPort: testRelayPort, generation: testGeneration + 1}

	t.Run("exact agent, port and generation match routes", func(t *testing.T) {
		table := NewTable(newStubPresence(exactKey))
		if err := table.Apply(testRoute(testRelayHostname, 7)); err != nil {
			t.Fatalf("Apply(active route) = %v, want nil", err)
		}
		if _, err := table.Lookup(testRelayHostname); err != nil {
			t.Fatalf("Lookup with matching presence = %v, want nil", err)
		}
	})

	t.Run("any join mismatch blocks routing", func(t *testing.T) {
		// Every wrong join dimension is online while the exact key is not:
		// only the exact agent/port/generation triple may ever route.
		presence := newStubPresence(wrongAgent, wrongPort, wrongGeneration)
		table := NewTable(presence)
		if err := table.Apply(testRoute(testRelayHostname, 7)); err != nil {
			t.Fatalf("Apply(active route) = %v, want nil", err)
		}

		if _, err := table.Lookup(testRelayHostname); !errors.Is(err, ErrPresenceAbsent) {
			t.Fatalf("Lookup without any matching presence error = %v, want %v", err, ErrPresenceAbsent)
		}
		for _, key := range []presenceKey{wrongAgent, wrongPort, wrongGeneration} {
			if _, err := table.Lookup(testRelayHostname); !errors.Is(err, ErrPresenceAbsent) {
				t.Fatalf("Lookup despite wrong presence %+v error = %v, want %v", key, err, ErrPresenceAbsent)
			}
		}

		presence.setOnline(exactKey, true)
		if _, err := table.Lookup(testRelayHostname); err != nil {
			t.Fatalf("Lookup once the exact join key comes online = %v, want nil", err)
		}
		presence.setOnline(exactKey, false)
		if _, err := table.Lookup(testRelayHostname); !errors.Is(err, ErrPresenceAbsent) {
			t.Fatalf("Lookup after presence drops error = %v, want %v", err, ErrPresenceAbsent)
		}
	})

	t.Run("absent presence authority fails closed", func(t *testing.T) {
		table := NewTable(nil)
		if err := table.Apply(testRoute(testRelayHostname, 7)); err != nil {
			t.Fatalf("Apply(active route) = %v, want nil", err)
		}
		if _, err := table.Lookup(testRelayHostname); !errors.Is(err, ErrPresenceAbsent) {
			t.Fatalf("Lookup without presence authority error = %v, want %v", err, ErrPresenceAbsent)
		}
	})
}

func TestRouteRevisionNeverRegresses(t *testing.T) {
	t.Run("stale applies and revocations are ignored", func(t *testing.T) {
		table := NewTable(newStubPresence(presenceKey{
			agentRecordID: testAgentRecordID,
			relayPort:     testRelayPort,
			generation:    testGeneration,
		}))

		if err := table.Apply(testRoute(testRelayHostname, 5)); err != nil {
			t.Fatalf("Apply(revision 5) = %v, want nil", err)
		}

		stale := testRoute(testRelayHostname, 4)
		stale.AgentRecordID = "rogue-agent-record"
		stale.RelayPort = 7999
		if err := table.Apply(stale); !errors.Is(err, ErrStaleRevision) {
			t.Fatalf("Apply(revision 4 after 5) error = %v, want %v", err, ErrStaleRevision)
		}
		if err := table.Apply(testRoute(testRelayHostname, 5)); !errors.Is(err, ErrStaleRevision) {
			t.Fatalf("Apply(revision 5 again) error = %v, want %v (equal revisions never mutate)", err, ErrStaleRevision)
		}
		route, err := table.Lookup(testRelayHostname)
		if err != nil {
			t.Fatalf("Lookup after stale applies = %v, want nil", err)
		}
		assertRouteEqual(t, route, testRoute(testRelayHostname, 5))

		if err := table.Revoke(testRelayHostname, 4); !errors.Is(err, ErrStaleRevision) {
			t.Fatalf("Revoke(revision 4 after 5) error = %v, want %v", err, ErrStaleRevision)
		}
		if _, err := table.Lookup(testRelayHostname); err != nil {
			t.Fatalf("Lookup after stale revoke = %v, want nil", err)
		}

		if err := table.Apply(testRoute(testRelayHostname, 6)); err != nil {
			t.Fatalf("Apply(revision 6) = %v, want nil", err)
		}
		if err := table.Revoke(testRelayHostname, 7); err != nil {
			t.Fatalf("Revoke(revision 7) = %v, want nil", err)
		}
		if _, err := table.Lookup(testRelayHostname); !errors.Is(err, ErrRouteInactive) {
			t.Fatalf("Lookup after revoke = %v, want %v", err, ErrRouteInactive)
		}
		// The tombstone fences the hostname: a stale re-add cannot revive it.
		if err := table.Apply(testRoute(testRelayHostname, 6)); !errors.Is(err, ErrStaleRevision) {
			t.Fatalf("Apply(revision 6 after tombstone at 7) error = %v, want %v", err, ErrStaleRevision)
		}
		if _, err := table.Lookup(testRelayHostname); !errors.Is(err, ErrRouteInactive) {
			t.Fatalf("Lookup after stale re-add error = %v, want %v", err, ErrRouteInactive)
		}
		// A genuinely newer delta revives the route.
		revived := testRoute(testRelayHostname, 8)
		revived.RelayPort = 7202
		if err := table.Apply(revived); err != nil {
			t.Fatalf("Apply(revision 8) = %v, want nil", err)
		}
	})

	t.Run("concurrent out-of-order applies converge to the newest revision", func(t *testing.T) {
		table := NewTable(newStubPresence(presenceKey{
			agentRecordID: testAgentRecordID,
			relayPort:     testRelayPort,
			generation:    testGeneration,
		}))

		// Descending order is the strongest deterministic adversarial case:
		// a table that lets any write regress would land on revision 1.
		const newestRevision = 50
		var waitGroup sync.WaitGroup
		for revision := newestRevision; revision >= 1; revision-- {
			waitGroup.Add(1)
			go func(revision int) {
				defer waitGroup.Done()
				arrival := testRoute(testRelayHostname, uint64(revision))
				arrival.SessionID = fmt.Sprintf("session-record-%d", revision)
				_ = table.Apply(arrival)
			}(revision)
		}
		waitGroup.Wait()

		route, err := table.Lookup(testRelayHostname)
		if err != nil {
			t.Fatalf("Lookup after concurrent applies = %v, want nil", err)
		}
		if route.Revision != newestRevision {
			t.Fatalf("route revision = %d, want %d (revisions must never regress)", route.Revision, newestRevision)
		}
	})
}

// TestReplaceSnapshotFromNewEpochReplacesStoredRevisions pins the R2 epoch
// authority at the table level: a snapshot from a NEWER control epoch
// wholesale-replaces stored state — per-route revisions restart within an
// epoch, so a lower incoming revision replaces a higher stored one — while
// the same-epoch ReplaceSnapshot keeps the newer stored route (the monotonic
// rule is unchanged within one control process lifetime).
func TestReplaceSnapshotFromNewEpochReplacesStoredRevisions(t *testing.T) {
	table := NewTable(newStubPresence(presenceKey{
		agentRecordID: testAgentRecordID,
		relayPort:     testRelayPort,
		generation:    testGeneration,
	}))
	if err := table.Apply(testRoute(testRelayHostname, 20)); err != nil {
		t.Fatalf("Apply(revision 20) = %v, want nil", err)
	}

	// Same epoch: the regressed re-push keeps the newer stored route.
	if dropped := table.ReplaceSnapshot([]Route{testRoute(testRelayHostname, 4)}); len(dropped) != 0 {
		t.Fatalf("same-epoch ReplaceSnapshot dropped = %v, want none", dropped)
	}
	route, err := table.Lookup(testRelayHostname)
	if err != nil {
		t.Fatalf("Lookup after same-epoch re-push = %v, want nil", err)
	}
	if route.Revision != 20 {
		t.Fatalf("same-epoch re-push stored revision = %d, want 20 (monotonic within an epoch)", route.Revision)
	}

	// Newer epoch: the snapshot is wholesale-authoritative even at revision 4.
	if dropped := table.ReplaceSnapshotFromNewEpoch([]Route{testRoute(testRelayHostname, 4)}); len(dropped) != 0 {
		t.Fatalf("new-epoch ReplaceSnapshot dropped = %v, want none (the route stays live at its new revision)", dropped)
	}
	route, err = table.Lookup(testRelayHostname)
	if err != nil {
		t.Fatalf("Lookup after new-epoch replace = %v, want nil", err)
	}
	if route.Revision != 4 {
		t.Fatalf("new-epoch replace stored revision = %d, want 4 (per-route revisions restart within the epoch)", route.Revision)
	}

	// Omission from a newer-epoch snapshot is authoritative revocation
	// state: the live route is reported dropped for the post-mutation drain.
	dropped := table.ReplaceSnapshotFromNewEpoch(nil)
	if len(dropped) != 1 || dropped[0] != testRelayHostname {
		t.Fatalf("new-epoch omission dropped = %v, want [%s]", dropped, testRelayHostname)
	}
	if _, err := table.Lookup(testRelayHostname); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("Lookup after new-epoch omission = %v, want %v", err, ErrRouteNotFound)
	}
}

// TestRouteTableCapsEntriesUnderFlood proves the in-memory route map cannot
// grow without bound (audit I7). The documented policy is FAIL CLOSED with
// no eviction: once the ceiling is reached a NEW hostname is refused with
// ErrRouteCapacity and every already-admitted route stays present and
// routable. The ceiling counts active routes and tombstones alike, because a
// tombstone is retained state (spec §8) that still occupies the map.
func TestRouteTableCapsEntriesUnderFlood(t *testing.T) {
	const cap = 8
	presence := newStubPresence(presenceKey{
		agentRecordID: testAgentRecordID,
		relayPort:     testRelayPort,
		generation:    testGeneration,
	})
	table := NewTable(presence, WithMaxRoutes(cap))

	admitted := 0
	for i := 0; i < 100; i++ {
		hostname := fmt.Sprintf("flood-%03d.relay.ns1.sharebridgeusercontent.com", i)
		err := table.Apply(testRoute(hostname, 1))
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrRouteCapacity):
			// The documented fail-closed refusal for a new hostname.
		default:
			t.Fatalf("Apply(%q) error = %v, want nil or %v", hostname, err, ErrRouteCapacity)
		}
	}
	if admitted != cap {
		t.Fatalf("admitted %d routes under flood, want exactly the ceiling %d", admitted, cap)
	}
	if got := table.Len(); got != cap {
		t.Fatalf("table.Len() = %d, want the ceiling %d", got, cap)
	}

	// Fail closed means no eviction: every admitted route is still present.
	for i := 0; i < cap; i++ {
		hostname := fmt.Sprintf("flood-%03d.relay.ns1.sharebridgeusercontent.com", i)
		if _, ok := table.Peek(hostname); !ok {
			t.Fatalf("admitted route %q was evicted by the capacity refusal", hostname)
		}
	}

	// Updating an existing entry never trips the capacity check.
	if err := table.Apply(testRoute("flood-000.relay.ns1.sharebridgeusercontent.com", 2)); err != nil {
		t.Fatalf("Apply(update at the ceiling) = %v, want nil", err)
	}

	// A revoke tombstones in place (no growth) and does not free capacity,
	// because the tombstone is retained fence state.
	if err := table.Revoke("flood-001.relay.ns1.sharebridgeusercontent.com", 3); err != nil {
		t.Fatalf("Revoke(at the ceiling) = %v, want nil", err)
	}
	if err := table.Apply(testRoute("flood-next.relay.ns1.sharebridgeusercontent.com", 1)); !errors.Is(err, ErrRouteCapacity) {
		t.Fatalf("Apply(new hostname after a tombstone) error = %v, want %v (tombstones retain their map slot)", err, ErrRouteCapacity)
	}
}

// TestRouteTableSnapshotBoundsAllocationBeforeCap is the audit-I7 proof that
// ReplaceSnapshot enforces the entry ceiling BEFORE building any
// O(len(incoming)) map or slice: an oversized snapshot must not transiently
// grow gateway memory while it is being discarded. The test compares the
// bytes allocated for a 100k-entry snapshot against the bytes allocated for a
// ceiling-sized snapshot on the same machine (self-calibrating against the
// runtime and -race overhead). A pre-allocation bound keeps the ratio near 1;
// the pre-fix code allocates the whole oversized map first and scales with the
// input (≈18x at 100k, ~30 MiB vs ~1.6 MiB). Retention is checked too: the
// cap still holds the lexicographically smallest ceiling-sized set, so the fix
// changes only WHEN the cap applies, never the R4 snapshot semantics.
func TestRouteTableSnapshotBoundsAllocationBeforeCap(t *testing.T) {
	const ceiling = 4096

	buildIncoming := func(count int) []Route {
		incoming := make([]Route, count)
		for i := range incoming {
			incoming[i] = testRoute(fmt.Sprintf("snap-%06d.relay.ns1.sharebridgeusercontent.com", i), 1)
		}
		return incoming
	}

	measure := func(count int) (allocated uint64, table *Table, dropped []string) {
		incoming := buildIncoming(count)
		table = NewTable(newStubPresence(), WithMaxRoutes(ceiling))
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		dropped = table.ReplaceSnapshot(incoming)
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc, table, dropped
	}

	baselineBytes, baselineTable, _ := measure(ceiling)
	if got := baselineTable.Len(); got != ceiling {
		t.Fatalf("ceiling-sized snapshot retained %d entries, want %d", got, ceiling)
	}

	oversizeBytes, table, dropped := measure(100_000)
	if got := table.Len(); got != ceiling {
		t.Fatalf("oversized snapshot retained %d entries, want the independent table ceiling %d", got, ceiling)
	}
	// A freshly seeded table has no previously-live entries, so truncating an
	// oversized incoming snapshot drops nothing; the omitted entries were
	// simply never admitted.
	if len(dropped) != 0 {
		t.Fatalf("oversized snapshot on an empty table dropped %d hostnames, want none", len(dropped))
	}
	if oversizeBytes > 3*baselineBytes {
		t.Fatalf("oversized snapshot allocated %d bytes vs %d for a ceiling-sized snapshot: the cap must be applied before any O(len(incoming)) allocation",
			oversizeBytes, baselineBytes)
	}

	for i := 0; i < ceiling; i++ {
		hostname := fmt.Sprintf("snap-%06d.relay.ns1.sharebridgeusercontent.com", i)
		if _, ok := table.Peek(hostname); !ok {
			t.Fatalf("snapshot entry %q missing; the first %d lexicographic hostnames must be retained", hostname, ceiling)
		}
	}
	for _, index := range []int{ceiling, 50_000, 99_999} {
		hostname := fmt.Sprintf("snap-%06d.relay.ns1.sharebridgeusercontent.com", index)
		if _, ok := table.Peek(hostname); ok {
			t.Fatalf("snapshot entry %q was admitted past the %d-entry ceiling", hostname, ceiling)
		}
	}
}

// TestRouteTableSnapshotUnderCapIsUnchanged pins the other half of the
// pre-allocation bound: a snapshot at or below the ceiling keeps every entry
// and its exact route value, so tightening when the cap applies did not change
// the R4 snapshot contract for the normal (under-cap) case.
func TestRouteTableSnapshotUnderCapIsUnchanged(t *testing.T) {
	const ceiling = 16
	table := NewTable(newStubPresence(), WithMaxRoutes(ceiling))

	incoming := make([]Route, 0, ceiling)
	for i := 0; i < ceiling; i++ {
		incoming = append(incoming, testRoute(fmt.Sprintf("under-%02d.relay.ns1.sharebridgeusercontent.com", i), uint64(i+1)))
	}
	if dropped := table.ReplaceSnapshot(incoming); len(dropped) != 0 {
		t.Fatalf("under-cap snapshot dropped %v, want none", dropped)
	}
	if got := table.Len(); got != ceiling {
		t.Fatalf("under-cap snapshot retained %d entries, want all %d", got, ceiling)
	}
	for _, route := range incoming {
		stored, ok := table.Peek(route.Hostname)
		if !ok {
			t.Fatalf("under-cap snapshot lost %q", route.Hostname)
		}
		assertRouteEqual(t, stored, route)
	}
}

// TestRouteTableSnapshotCapIsFailClosedAndDeterministic proves the snapshot
// path obeys the same ceiling. Control's Task 11 validator already bounds a
// snapshot at 4096 routes; this is the table's independent defense in depth.
// Entries beyond the ceiling are omitted deterministically (lexicographic
// hostname order) so an overflow can never admit a random partial subset.
func TestRouteTableSnapshotCapIsFailClosedAndDeterministic(t *testing.T) {
	const cap = 4
	table := NewTable(newStubPresence(), WithMaxRoutes(cap))

	incoming := make([]Route, 0, 10)
	for i := 0; i < 10; i++ {
		incoming = append(incoming, testRoute(fmt.Sprintf("snap-%02d.relay.ns1.sharebridgeusercontent.com", i), 1))
	}
	table.ReplaceSnapshot(incoming)

	if got := table.Len(); got != cap {
		t.Fatalf("table.Len() after an oversize snapshot = %d, want the ceiling %d", got, cap)
	}
	for i := 0; i < cap; i++ {
		hostname := fmt.Sprintf("snap-%02d.relay.ns1.sharebridgeusercontent.com", i)
		if _, ok := table.Peek(hostname); !ok {
			t.Fatalf("snapshot entry %q missing; the first %d lexicographic hostnames must be admitted", hostname, cap)
		}
	}
	for i := cap; i < 10; i++ {
		hostname := fmt.Sprintf("snap-%02d.relay.ns1.sharebridgeusercontent.com", i)
		if _, ok := table.Peek(hostname); ok {
			t.Fatalf("snapshot entry %q was admitted past the %d-entry ceiling", hostname, cap)
		}
	}
}

package routes

import (
	"errors"
	"fmt"
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

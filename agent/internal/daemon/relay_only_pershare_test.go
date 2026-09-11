package daemon

// M4 closeout batch 4: per-share relay-only selection end to end.
//
// The post-remediation Sol audit found that CreateSession ignored its
// relay-only argument and registerImmichShare used only the persisted
// DefaultRelayOnly default, so the share form's visible relay-only control was
// silently dropped. These tests pin the wired chain: per-share selection wins
// over the global default, the agent installs the T27 relay-only single
// binding (zero direct-path setup), the registration carries relay_only so
// control never prepares a direct open, and the mode survives persistence and
// hydration.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/immich"
)

// newRelayOnlyTestDaemon is relayOnlyTestDaemon with a caller-supplied store,
// so a "restart" can rehydrate the same persisted sessions. The wiring is
// otherwise identical: baseline-ready direct state (TLS + relay DNS), a binder
// whose registrations return the lowercased code label as the control-allocated
// direct origin, and Immich config.
func newRelayOnlyTestDaemon(t *testing.T, defaultRelayOnly bool, st *mockStore) (*Daemon, *mockSignalingClient, *directState) {
	t.Helper()
	cm, _ := newBaselineCertFixture(t, t.TempDir())

	cfg := &config.Config{
		SignalingURL:      "ws://localhost:8080",
		APIKey:            "test-key",
		DefaultRelayOnly:  defaultRelayOnly,
		ImmichURL:         "http://immich.lan:2283",
		ImmichAllowedHost: "immich.lan:2283",
		ImmichAPIKey:      "api",
	}
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	sig.shareOrigin = func(code, shareURL string) string { return testOriginFor(strings.ToLower(code)) }

	ds := &directState{
		namespace:  testDirectNS,
		baseDomain: testDirectBase,
		cert:       cm,
		ready:      true, // baseline enrollment_ready for the current epoch
		gate:       direct.NewSignalGate(st.GetAgentID(), func(string, direct.RouteKind) bool { return true }),
		origins:    map[string]originPair{},
		listenAddr: "127.0.0.1:0",
	}
	d := &Daemon{
		config:    cfg,
		store:     st,
		signaling: sig,
		resolver:  direct.NewResolverRegistry(),
		sessions:  make(map[string]*Session),
		direct:    ds,
	}
	d.syncDirectServe()
	return d, sig, ds
}

// assertDirectPairBinding pins the T26 dual-origin pair on the binder state of
// one normal (non-relay-only) share: BOTH origins are admitted with their
// route kind, the originPair is recorded, and no relay-only single binding
// coexists.
func assertDirectPairBinding(t *testing.T, ds *directState, code, directOrigin, relayOrigin string) {
	t.Helper()
	db, err := ds.binder.AdmitSNI(directOrigin)
	require.NoError(t, err, "direct origin must be bound for a normal share")
	require.Equal(t, direct.RouteDirect, db.RouteKind)
	require.Equal(t, code, db.ShareCode)
	rb, err := ds.binder.AdmitSNI(relayOrigin)
	require.NoError(t, err, "relay origin must be bound for a normal share")
	require.Equal(t, direct.RouteRelay, rb.RouteKind)
	require.Equal(t, code, rb.ShareCode)

	ds.mu.Lock()
	pair, ok := ds.origins[code]
	_, relayOnlyBound := ds.relayOrigins[code]
	ds.mu.Unlock()
	require.True(t, ok, "normal share must record an originPair (T26)")
	require.Equal(t, directOrigin, pair.directOrigin)
	require.Equal(t, relayOrigin, pair.relayOrigin)
	require.False(t, relayOnlyBound, "normal share must not carry a relay-only single binding")
}

// TestPerShareRelayOnlyWinsOverGlobalDefault is the core audit fix: a share
// created with the per-share relay-only selection set is relay-only even when
// the persisted DefaultRelayOnly default is OFF. It pins the whole chain — the
// session/persistence flag, the T27 single relay binding with ZERO
// direct-path setup, the relay_only carried to control, and the agent's
// direct-open authorization refusing the share (no open_ack/open_signal path).
func TestPerShareRelayOnlyWinsOverGlobalDefault(t *testing.T) {
	d, sig, ds := newRelayOnlyTestDaemon(t, false, newMockStore())
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{{Key: "IMMICHPERSHARE", Type: "ALBUM"}}}, nil
	}

	code, err := d.CreateSession(context.Background(), "immich://IMMICHPERSHARE", "immich", "", 24*time.Hour, 10, true)
	require.NoError(t, err, "per-share relay-only creation must be accepted")
	require.Equal(t, "IMMICHPERSHARE", code)

	// (a) the session and its persistence record carry relay-only.
	session := d.GetSession(code)
	require.NotNil(t, session)
	require.True(t, session.RelayOnly, "per-share relay-only must win over DefaultRelayOnly=false")
	entry := d.store.GetSession(code)
	require.NotNil(t, entry)
	require.True(t, entry.RelayOnly, "relay-only must be persisted with the session")

	// (b) the agent installs the T27 relay-only single binding and no direct
	// origin/mapping.
	directOrigin := testOriginFor("immichpershare")
	relayOrigin := relayOriginForLabel("immichpershare")
	assertRelayOnlyBinding(t, ds, code, directOrigin, relayOrigin)
	require.False(t, d.directShareAuthorized(code, direct.RouteDirect),
		"relay-only share must never authorize a direct open at the agent")

	// (c) the control registration carries relay_only, so control never selects
	// or prepares a direct route for it.
	registered := sig.registeredSnapshot()
	require.Len(t, registered, 1)
	require.True(t, registered[0].RelayOnly, "registration must carry relay_only=true")

	// (e) no direct open ever happened: the agent sent no open_ack (it only
	// emits one in response to a control open_signal, which control never sends
	// for a relay_only session — see directctl's relayOnly suppression tests).
	require.False(t, sig.sentContains("open_ack"), "relay-only share must never produce an open_ack")
	require.False(t, sig.sentContains("open_signal"), "the agent must never emit open_signal")
}

// TestPerShareDirectWinsOverGlobalRelayDefault is the other precedence arm: an
// explicit per-share direct selection wins over DefaultRelayOnly=true and keeps
// the T26 dual-origin pair.
func TestPerShareDirectWinsOverGlobalRelayDefault(t *testing.T) {
	d, sig, ds := newRelayOnlyTestDaemon(t, true, newMockStore())
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{{Key: "IMMICHPERDIRECT", Type: "ALBUM"}}}, nil
	}

	code, err := d.CreateSession(context.Background(), "immich://IMMICHPERDIRECT", "immich", "", 24*time.Hour, 10, false)
	require.NoError(t, err)

	session := d.GetSession(code)
	require.NotNil(t, session)
	require.False(t, session.RelayOnly, "explicit per-share direct must win over DefaultRelayOnly=true")
	entry := d.store.GetSession(code)
	require.NotNil(t, entry)
	require.False(t, entry.RelayOnly)

	registered := sig.registeredSnapshot()
	require.Len(t, registered, 1)
	require.False(t, registered[0].RelayOnly, "registration must carry relay_only=false")

	assertDirectPairBinding(t, ds, code, testOriginFor("immichperdirect"), relayOriginForLabel("immichperdirect"))
	require.True(t, d.directShareAuthorized(code, direct.RouteDirect),
		"a normal share must still be authorized to open direct (T26 preserved)")
}

// TestRelayOnlyShareHydratesRelayOnly pins persistence-independent-of-restart:
// after hydration a relay-only share stays relay-only and re-registers as such
// even though the current DefaultRelayOnly default is OFF — no accidental
// upgrade to a T26 dual-origin share on reconnect — and still never produces a
// direct open.
func TestRelayOnlyShareHydratesRelayOnly(t *testing.T) {
	st := newMockStore()
	d1, _, _ := newRelayOnlyTestDaemon(t, false, st)
	d1.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{{Key: "IMMICHHYDRATE", Type: "ALBUM"}}}, nil
	}
	code, err := d1.CreateSession(context.Background(), "immich://IMMICHHYDRATE", "immich", "", 24*time.Hour, 10, true)
	require.NoError(t, err)
	require.True(t, d1.GetSession(code).RelayOnly)

	// "Restart": a fresh daemon over the same store, with the default OFF.
	d2, sig2, ds2 := newRelayOnlyTestDaemon(t, false, st)
	d2.loadSessionsFromStore(context.Background())

	session := d2.GetSession("IMMICHHYDRATE")
	require.NotNil(t, session, "relay-only share must be rehydrated")
	require.True(t, session.RelayOnly, "hydration must not upgrade a relay-only share to dual-origin")

	registered := sig2.registeredSnapshot()
	require.Len(t, registered, 1)
	require.True(t, registered[0].RelayOnly, "reconnect must re-register relay_only=true")

	directOrigin := testOriginFor("immichhydrate")
	relayOrigin := relayOriginForLabel("immichhydrate")
	assertRelayOnlyBinding(t, ds2, "IMMICHHYDRATE", directOrigin, relayOrigin)
	require.False(t, d2.directShareAuthorized("IMMICHHYDRATE", direct.RouteDirect),
		"rehydrated relay-only share must still refuse a direct open")
	require.False(t, sig2.sentContains("open_ack"), "hydration must not produce a direct open")
}

// TestGlobalRelayDefaultStillAppliesWhenNoPerShareSelection pins the unchanged
// fallback path: the poller (which has no per-share field) keeps using
// DefaultRelayOnly for both settings.
func TestGlobalRelayDefaultStillAppliesWhenNoPerShareSelection(t *testing.T) {
	for _, relayDefault := range []bool{true, false} {
		t.Run(map[bool]string{true: "default_relay_only", false: "default_direct"}[relayDefault], func(t *testing.T) {
			d, sig, ds := newRelayOnlyTestDaemon(t, relayDefault, newMockStore())
			d.newImmichPoller = func() (immichPoller, error) {
				return &fakeImmichPoller{shares: []immich.SharedLink{{Key: "IMMICHPOLL", Type: "ALBUM"}}}, nil
			}

			require.NoError(t, d.syncImmichShares(context.Background()))

			session := d.GetSession("IMMICHPOLL")
			require.NotNil(t, session)
			require.Equal(t, relayDefault, session.RelayOnly, "poller must follow DefaultRelayOnly unchanged")

			registered := sig.registeredSnapshot()
			require.Len(t, registered, 1)
			require.Equal(t, relayDefault, registered[0].RelayOnly)

			if relayDefault {
				assertRelayOnlyBinding(t, ds, "IMMICHPOLL", testOriginFor("immichpoll"), relayOriginForLabel("immichpoll"))
			} else {
				assertDirectPairBinding(t, ds, "IMMICHPOLL", testOriginFor("immichpoll"), relayOriginForLabel("immichpoll"))
			}
		})
	}
}

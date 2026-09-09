package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/immich"
	"sharebridge/agent/internal/store"
)

// immichTestConfig points the daemon at a fake Immich instance so manual and
// poller paths can construct a client without network access.
func immichTestConfig(d *Daemon) {
	d.config.ImmichURL = "http://immich.lan:2283"
	d.config.ImmichAllowedHost = "immich.lan:2283"
	d.config.ImmichAPIKey = "api"
}

func TestEnforceManualRejectsWebDAV(t *testing.T) {
	d, _ := newTestDaemon(t)
	_, err := d.CreateSession(context.Background(), "https://opencloud.example.com/s/abc", "opencloud", "", 24*time.Hour, 10, false)
	require.ErrorContains(t, err, "not supported")
}

// TestEnforceManualRejectsRelayOnly pins the retained §13.3 rejection for
// deferred share types: an opencloud/nextcloud share is still rejected even
// when marked relay-only — relay-only restoration is scoped to supported
// public Immich gallery shares only.
func TestEnforceManualRejectsRelayOnly(t *testing.T) {
	d, _ := newTestDaemon(t)
	_, err := d.CreateSession(context.Background(), "https://opencloud.example.com/s/abc", "opencloud", "", 24*time.Hour, 10, true)
	require.ErrorContains(t, err, "not supported")
}

func TestEnforceManualImmichRejectsProtected(t *testing.T) {
	d, sig := newTestDaemon(t)
	immichTestConfig(d)
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHPROT", Type: "ALBUM", Password: "secret"},
		}}, nil
	}

	_, err := d.CreateSession(context.Background(), "immich://IMMICHPROT", "immich", "", 24*time.Hour, 10, false)
	require.ErrorContains(t, err, "password-protected")
	require.Empty(t, sig.registeredSnapshot())
}

func TestEnforcePollerRejectsProtected(t *testing.T) {
	d, sig := newTestDaemon(t)
	immichTestConfig(d)
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHPROT2", Type: "ALBUM", Password: "secret"},
		}}, nil
	}

	require.NoError(t, d.syncImmichShares(context.Background()))
	require.False(t, sig.registeredCode("IMMICHPROT2"))
	require.Nil(t, d.GetSession("IMMICHPROT2"))
}

// relayOnlyTestDaemon builds a daemon wired with a BASELINE-READY direct
// state (§7.1: TLS + relay DNS — the state every registration waits for), a
// binder, and Immich config, so the polling/manual/restoration relay-only
// paths can be driven end to end without network access. Registrations get
// the control-allocated direct origin (lowercased code label) from the mock
// registrar; the relay origin is its deterministic §6 pair.
func relayOnlyTestDaemon(t *testing.T, defaultRelayOnly bool) (*Daemon, *mockSignalingClient, *directState) {
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
	st := newMockStore()
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

// assertRelayOnlyBinding pins §13.3 zero direct-path setup on the binder
// state of one relay-only share: the relay origin is admitted (RouteRelay,
// same content session), the direct origin is NOT admitted, and no originPair
// was recorded for the share.
func assertRelayOnlyBinding(t *testing.T, ds *directState, code, directOrigin, relayOrigin string) {
	t.Helper()
	rb, err := ds.binder.AdmitSNI(relayOrigin)
	require.NoError(t, err, "relay origin must be bound for a relay-only share")
	require.Equal(t, direct.RouteRelay, rb.RouteKind)
	require.Equal(t, code, rb.ShareCode)
	_, err = ds.binder.AdmitSNI(directOrigin)
	require.Error(t, err, "relay-only share must not bind its direct origin (zero direct-path setup)")
	_, ok := ds.origins[code]
	require.False(t, ok, "relay-only session must not create an originPair")
}

// TestRelayOnlyPollingRegistrationAccepted pins §13.3: a discovered public
// Immich album registers as a relay-only share when DefaultRelayOnly is set —
// the Phase 3 temporary rejection is gone at the polling entry point. The
// share is bound to its relay origin only (zero direct-path setup), and the
// binding survives a binder rebuild and is removed by revocation as one
// logical operation.
func TestRelayOnlyPollingRegistrationAccepted(t *testing.T) {
	d, sig, ds := relayOnlyTestDaemon(t, true)
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHRELAYOK", Type: "ALBUM"},
		}}, nil
	}

	require.NoError(t, d.syncImmichShares(context.Background()))

	session := d.GetSession("IMMICHRELAYOK")
	require.NotNil(t, session)
	require.True(t, session.RelayOnly)

	registered := sig.registeredSnapshot()
	require.Len(t, registered, 1)
	require.True(t, registered[0].RelayOnly, "polling must register the share relay-only")

	directOrigin := testOriginFor("immichrelayok")
	relayOrigin := relayOriginForLabel("immichrelayok")
	assertRelayOnlyBinding(t, ds, "IMMICHRELAYOK", directOrigin, relayOrigin)

	// Persisted with both origin facts; the mode is preserved for restoration.
	entry := d.store.GetSession("IMMICHRELAYOK")
	require.NotNil(t, entry)
	require.True(t, entry.RelayOnly)
	require.Equal(t, directOrigin, entry.Origin)
	require.Equal(t, relayOrigin, entry.RelayOrigin)

	// The single relay binding survives a binder/listener rebuild (§13.1).
	ds.mu.Lock()
	ds.binder = nil
	ds.mu.Unlock()
	d.syncDirectServe()
	assertRelayOnlyBinding(t, ds, "IMMICHRELAYOK", directOrigin, relayOrigin)

	// Revocation removes the relay binding (§6) and a later rebuild does not
	// resurrect it.
	d.revokeOrigin("IMMICHRELAYOK")
	_, err := ds.binder.AdmitSNI(relayOrigin)
	require.Error(t, err, "revocation must remove the relay-only binding")
	ds.mu.Lock()
	ds.binder = nil
	ds.mu.Unlock()
	d.syncDirectServe()
	_, err = ds.binder.AdmitSNI(relayOrigin)
	require.Error(t, err, "rebuilt binder must not resurrect a revoked relay-only binding")
}

// TestRelayOnlyManualCreationAccepted pins §13.3: manual creation of a
// supported public Immich share is accepted in relay-only mode — the Phase 3
// CreateSession rejection is gone.
func TestRelayOnlyManualCreationAccepted(t *testing.T) {
	d, sig, ds := relayOnlyTestDaemon(t, true)
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHMANRELAY", Type: "ALBUM"},
		}}, nil
	}

	code, err := d.CreateSession(context.Background(), "immich://IMMICHMANRELAY", "immich", "", 24*time.Hour, 10, true)
	require.NoError(t, err, "manual relay-only Immich creation must be accepted")
	require.Equal(t, "IMMICHMANRELAY", code)

	session := d.GetSession(code)
	require.NotNil(t, session)
	require.True(t, session.RelayOnly)

	registered := sig.registeredSnapshot()
	require.Len(t, registered, 1)
	require.True(t, registered[0].RelayOnly)

	assertRelayOnlyBinding(t, ds, code, testOriginFor("immichmanrelay"), relayOriginForLabel("immichmanrelay"))
}

// TestRelayOnlyRestorationAccepted pins §13.3: a persisted relay-only public
// Immich session is RESTORED (re-registered and re-bound to its relay origin
// only) instead of being tombstoned as unsupported. Restoration does not
// depend on the current DefaultRelayOnly setting — the persisted mode wins.
func TestRelayOnlyRestorationAccepted(t *testing.T) {
	d, sig, ds := relayOnlyTestDaemon(t, false)
	now := time.Now()
	directOrigin := testOriginFor("immichrestore")
	relayOrigin := relayOriginForLabel("immichrestore")
	require.NoError(t, d.store.SaveSession(store.SessionEntry{
		Code: "IMMICHRESTORE", ShareURL: "immich://IMMICHRESTORE", ShareType: "immich",
		RelayOnly: true, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		Origin: directOrigin, RelayOrigin: relayOrigin,
	}))

	d.loadSessionsFromStore(context.Background())

	session := d.GetSession("IMMICHRESTORE")
	require.NotNil(t, session, "relay-only public Immich session must be restored")
	require.True(t, session.RelayOnly)
	require.NotNil(t, d.store.GetSession("IMMICHRESTORE"), "restored session stays persisted")

	require.True(t, sig.registeredCode("IMMICHRESTORE"))
	require.False(t, sig.hasSentMessage("deregister", map[string]any{"code": "IMMICHRESTORE", "reason": "unsupported"}),
		"restored relay-only session must not be tombstoned as unsupported")

	registered := sig.registeredSnapshot()
	require.Len(t, registered, 1)
	require.True(t, registered[0].RelayOnly)

	assertRelayOnlyBinding(t, ds, "IMMICHRESTORE", directOrigin, relayOrigin)
}

// TestEnforceRestoreRejectsUnsupported pins the retained §13.3 restoration
// rejections: password-protected Immich and WebDAV/file (opencloud/nextcloud)
// persisted sessions are still removed and deregistered as unsupported.
// Relay-only public Immich rows are restored instead (§13.3) — see
// TestRelayOnlyRestorationAccepted.
func TestEnforceRestoreRejectsUnsupported(t *testing.T) {
	d, sig := newTestDaemon(t)
	now := time.Now()
	future := now.Add(time.Hour)

	require.NoError(t, d.store.SaveSession(store.SessionEntry{Code: "prot", ShareURL: "immich://prot", ShareType: "immich", IsPasswordProtected: true, CreatedAt: now, ExpiresAt: future}))
	require.NoError(t, d.store.SaveSession(store.SessionEntry{Code: "wd", ShareURL: "https://cloud.example.com/s/abc", ShareType: "opencloud", CreatedAt: now, ExpiresAt: future}))

	d.loadSessionsFromStore(context.Background())

	for _, code := range []string{"prot", "wd"} {
		require.Nil(t, d.GetSession(code), "session %q should not be restored", code)
		require.Nil(t, d.store.GetSession(code), "session %q should be deleted from the store", code)
		require.True(t, sig.hasSentMessage("deregister", map[string]any{"code": code, "reason": "unsupported"}),
			"session %q should be deregistered as unsupported (410)", code)
		require.False(t, sig.unregisteredCode(code),
			"session %q must not be unregistered via unregister_share (revoked/404)", code)
	}
}

func TestCleanupPersistedSessionSendsDeregisterUnsupported(t *testing.T) {
	d, sig := newTestDaemon(t)

	entry := store.SessionEntry{Code: "prot", ShareURL: "immich://prot", ShareType: "immich", IsPasswordProtected: true}
	require.NoError(t, d.store.SaveSession(entry))

	d.cleanupPersistedSession(context.Background(), entry)

	require.Nil(t, d.store.GetSession("prot"), "persisted session should be deleted")
	require.True(t, sig.hasSentMessage("deregister", map[string]any{"code": "prot", "reason": "unsupported"}),
		"cleanupPersistedSession should deregister unsupported sessions as unsupported")
	require.False(t, sig.unregisteredCode("prot"),
		"cleanupPersistedSession must not use unregister_share (revoked/404)")
}

func TestSyncImmichSharesProtectedRemovalDeregistersUnsupported(t *testing.T) {
	d, sig := newTestDaemon(t)
	immichTestConfig(d)
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHPROTOLD", Type: "ALBUM", Password: "secret"},
		}}, nil
	}
	d.sessions["IMMICHPROTOLD"] = &Session{Code: "IMMICHPROTOLD", ShareType: "immich"}

	require.NoError(t, d.syncImmichShares(context.Background()))

	require.Nil(t, d.GetSession("IMMICHPROTOLD"))
	require.True(t, sig.hasSentMessage("deregister", map[string]any{"code": "IMMICHPROTOLD", "reason": "unsupported"}),
		"protected Immich share skipped by the poller should be deregistered as unsupported")
	require.False(t, sig.unregisteredCode("IMMICHPROTOLD"),
		"protected Immich share must not be unregistered as revoked (404)")
}

func TestEnforceManualImmichPlumbsMaxDownloadsAndExpiry(t *testing.T) {
	d, _ := newTestDaemon(t)
	immichTestConfig(d)
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{{Key: "IMMICHMANUAL2", Type: "ALBUM"}}}, nil
	}

	code, err := d.CreateSession(context.Background(), "immich://IMMICHMANUAL2", "immich", "", 48*time.Hour, 17, false)
	require.NoError(t, err)
	require.Equal(t, "IMMICHMANUAL2", code)

	session := d.GetSession(code)
	require.NotNil(t, session)
	require.Equal(t, 17, session.MaxDownloads)
	require.False(t, session.ExpiresAt.IsZero())
	require.InDelta(t, 48*time.Hour.Seconds(), time.Until(session.ExpiresAt).Seconds(), 120)

	stored := d.store.GetSession(code)
	require.NotNil(t, stored)
	require.Equal(t, 17, stored.MaxDownloads)
	require.False(t, stored.ExpiresAt.IsZero())
}

func TestEnforceDiscoveredImmichGetsDefaultMaxDownloads(t *testing.T) {
	d, _ := newTestDaemon(t)
	immichTestConfig(d)
	d.config.DefaultMaxDownloads = 7
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{{Key: "IMMICHDISC1", Type: "ALBUM"}}}, nil
	}

	require.NoError(t, d.syncImmichShares(context.Background()))

	session := d.GetSession("IMMICHDISC1")
	require.NotNil(t, session)
	require.Equal(t, 7, session.MaxDownloads)

	stored := d.store.GetSession("IMMICHDISC1")
	require.NotNil(t, stored)
	require.Equal(t, 7, stored.MaxDownloads)
}

package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

func TestEnforceManualRejectsRelayOnly(t *testing.T) {
	d, _ := newTestDaemon(t)
	_, err := d.CreateSession(context.Background(), "https://opencloud.example.com/s/abc", "opencloud", "", 24*time.Hour, 10, true)
	require.ErrorContains(t, err, "relay-only")
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

func TestEnforcePollerRejectsRelayOnly(t *testing.T) {
	d, sig := newTestDaemon(t)
	d.config.DefaultRelayOnly = true // simulate an un-migrated config
	immichTestConfig(d)
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHRELAY", Type: "ALBUM"},
		}}, nil
	}

	err := d.syncImmichShares(context.Background())
	require.ErrorContains(t, err, "relay-only")
	require.False(t, sig.registeredCode("IMMICHRELAY"))
}

func TestEnforceRestoreRejectsUnsupported(t *testing.T) {
	d, sig := newTestDaemon(t)
	now := time.Now()
	future := now.Add(time.Hour)

	require.NoError(t, d.store.SaveSession(store.SessionEntry{Code: "prot", ShareURL: "immich://prot", ShareType: "immich", IsPasswordProtected: true, CreatedAt: now, ExpiresAt: future}))
	require.NoError(t, d.store.SaveSession(store.SessionEntry{Code: "relay", ShareURL: "immich://relay", ShareType: "immich", RelayOnly: true, CreatedAt: now, ExpiresAt: future}))
	require.NoError(t, d.store.SaveSession(store.SessionEntry{Code: "wd", ShareURL: "https://cloud.example.com/s/abc", ShareType: "opencloud", CreatedAt: now, ExpiresAt: future}))

	d.loadSessionsFromStore(context.Background())

	for _, code := range []string{"prot", "relay", "wd"} {
		require.Nil(t, d.GetSession(code), "session %q should not be restored", code)
		require.Nil(t, d.store.GetSession(code), "session %q should be deleted from the store", code)
		require.True(t, sig.unregisteredCode(code), "session %q should be unregistered from the control plane", code)
	}
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

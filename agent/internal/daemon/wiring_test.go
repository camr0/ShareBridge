// agent/internal/daemon/wiring_test.go
package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/immich"
	"sharebridge/agent/internal/store"
)

// stubResolver resolves every code to a minimal content session. It keeps the
// placeholder download test green now that route() fails closed without a
// resolver.
type stubResolver struct{}

func (stubResolver) Resolve(code string) (*direct.ContentSession, error) {
	return &direct.ContentSession{Membership: map[string]struct{}{}}, nil
}

// newImmichTestServer returns an httptest server serving a one-asset album
// gallery for any shared link. The inline assets mean ListGallery never hits
// the version/album-assets endpoints.
func newImmichTestServer(t *testing.T) (baseURL, host string) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/shared-links/me" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"ALBUM","album":{"id":"album-1","albumName":"Summer"},"assets":[{"id":"asset-1","originalFileName":"one.jpg","originalMimeType":"image/jpeg","checksum":"abc"}]}`))
	}))
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	return ts.URL, u.Host
}

func TestWiringRegisterImmichShareCreatesSnapshotManagerAndBuilds(t *testing.T) {
	baseURL, host := newImmichTestServer(t)
	d, sig := newTestDaemon(t)
	d.config.ImmichURL = baseURL
	d.config.ImmichAllowedHost = host
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{{Key: "IMMICHWIRING1", Type: "ALBUM"}}}, nil
	}

	require.NoError(t, d.syncImmichShares(context.Background()))

	require.True(t, sig.registeredCode("IMMICHWIRING1"))
	mgr := d.resolver.Get("IMMICHWIRING1")
	require.NotNil(t, mgr, "registering a share must create a SnapshotManager in the registry")

	session, err := mgr.Resolve()
	require.NoError(t, err, "Build must have hydrated the snapshot")
	require.Equal(t, "Summer", session.Gallery.AlbumName)
	require.Len(t, session.Membership, 1)
	require.Contains(t, session.Membership, "asset-1")
}

func TestWiringLoadSessionsFromStoreHydratesRestoredSessions(t *testing.T) {
	baseURL, host := newImmichTestServer(t)
	st := newMockStore()
	require.NoError(t, st.SaveSession(store.SessionEntry{
		Code:                "IMMICHPERSIST2",
		ShareURL:            "immich://IMMICHPERSIST2",
		ShareType:           "immich",
		IsPasswordProtected: false,
		RelayOnly:           false,
		MaxDownloads:        3,
		CreatedAt:           time.Now().Add(-time.Hour),
		ExpiresAt:           time.Now().Add(time.Hour),
	}))

	cfg := &config.Config{
		SignalingURL:      "ws://localhost:8080",
		APIKey:            "test-key",
		ImmichURL:         baseURL,
		ImmichAllowedHost: host,
		ImmichAPIKey:      "api",
		DefaultRelayOnly:  false,
	}
	cfgMgr := &mockConfigManager{cfg: cfg}
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	d, err := NewWithSignaling(cfgMgr, st, sig)
	require.NoError(t, err)

	d.loadSessionsFromStore(context.Background())

	require.NotNil(t, d.GetSession("IMMICHPERSIST2"))
	mgr := d.resolver.Get("IMMICHPERSIST2")
	require.NotNil(t, mgr, "restoring an immich session must create a SnapshotManager")

	cs, err := mgr.Resolve()
	require.NoError(t, err, "restored session must be hydrated by Build")
	require.Equal(t, "Summer", cs.Gallery.AlbumName)
	require.Equal(t, 3, cs.MaxDownloads)
}

func TestWiringHydrateContentSessionPersistsDownloads(t *testing.T) {
	baseURL, host := newImmichTestServer(t)
	d, sig := newTestDaemon(t)
	d.config.ImmichURL = baseURL
	d.config.ImmichAllowedHost = host
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{{Key: "IMMICHPERSIST3", Type: "ALBUM"}}}, nil
	}

	require.NoError(t, d.syncImmichShares(context.Background()))
	require.True(t, sig.registeredCode("IMMICHPERSIST3"))

	mgr := d.resolver.Get("IMMICHPERSIST3")
	require.NotNil(t, mgr, "registering a share must create a SnapshotManager")
	cs, err := mgr.Resolve()
	require.NoError(t, err)
	require.NotNil(t, cs.Ledger)

	// A committed download must be durably recorded via store.IncrementDownloads
	// (the persist callback hydrateContentSession wires into the ledger).
	require.True(t, cs.Ledger.TryReserve())
	cs.Ledger.Commit()

	st := d.store.(*mockStore)
	st.mu.Lock()
	defer st.mu.Unlock()
	require.Equal(t, 1, st.downloads["IMMICHPERSIST3"],
		"committed download must be persisted via store.IncrementDownloads")
}

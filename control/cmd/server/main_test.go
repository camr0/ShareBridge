package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/types"
	"github.com/stretchr/testify/require"
	"sharebridge/control/internal/directctl"
	"sharebridge/control/internal/hub"
	"sharebridge/control/internal/relayctl"
	"sharebridge/control/migrations"
)

func setupServerTestApp(t *testing.T) (core.App, func()) {
	t.Helper()

	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, testApp.Bootstrap())
	require.NoError(t, testApp.RunSystemMigrations())
	require.NoError(t, migrations.CreateCollections(testApp))
	require.NoError(t, migrations.AddRelayOnly(testApp))
	require.NoError(t, migrations.AddSessionRelayStaticPub(testApp))
	require.NoError(t, migrations.AddImmichSessionFields(testApp))
	require.NoError(t, migrations.CreateAgents(testApp))
	require.NoError(t, migrations.AddSessionsInactiveReason(testApp))

	return testApp, func() { testApp.Cleanup() }
}

func createServerTestUser(t *testing.T, app core.App, email string) *core.Record {
	t.Helper()

	usersCol, err := app.FindCollectionByNameOrId("users")
	require.NoError(t, err)

	user := core.NewRecord(usersCol)
	user.SetEmail(email)
	user.SetPassword("testpassword123")
	require.NoError(t, app.Save(user))

	return user
}

func createServerTestAPIKey(t *testing.T, app core.App, userID string) *core.Record {
	t.Helper()

	apiKeysCol, err := app.FindCollectionByNameOrId("api_keys")
	require.NoError(t, err)

	record := core.NewRecord(apiKeysCol)
	record.Set("account_id", userID)
	record.Set("is_active", true)
	record.Set("label", "test key")
	require.NoError(t, app.Save(record))

	return record
}

func createServerTestSession(t *testing.T, app core.App, apiKeyID, code string, expiresAt *time.Time) *core.Record {
	t.Helper()

	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	require.NoError(t, err)

	record := core.NewRecord(sessionsCol)
	record.Set("code", code)
	record.Set("api_key_id", apiKeyID)
	record.Set("agent_id", "test-agent")
	record.Set("is_active", true)
	if expiresAt != nil {
		dt, err := types.ParseDateTime(*expiresAt)
		require.NoError(t, err)
		record.Set("expires_at", dt)
	}
	require.NoError(t, app.Save(record))

	return record
}

func testSessionExists(t *testing.T, app core.App, code string) bool {
	t.Helper()

	records, err := app.FindRecordsByFilter(
		"sessions",
		"code = {:code}",
		"",
		1,
		0,
		map[string]any{"code": code},
	)
	require.NoError(t, err)
	return len(records) == 1
}

func sessionIsActive(t *testing.T, app core.App, code string) bool {
	t.Helper()

	records, err := app.FindRecordsByFilter(
		"sessions",
		"code = {:code}",
		"",
		1,
		0,
		map[string]any{"code": code},
	)
	require.NoError(t, err)
	require.Len(t, records, 1)
	return records[0].GetBool("is_active")
}

func setupRouterForTest(t *testing.T) (core.App, http.Handler) {
	t.Helper()

	app, cleanup := setupServerTestApp(t)
	t.Cleanup(cleanup)

	pbRouter, err := apis.NewRouter(app)
	require.NoError(t, err)

	// Direct-mode controller for the /s/{code} dispatch. Coordinator and DDNS
	// are nil: Redirect's gates only need the hub + epoch state, which the
	// route-level tests exercise independently.
	ctrl := directctl.NewController(app, hub.New(), nil, nil, directctl.Config{BaseDomain: "example.com"})

	// Canonical routes: /share/{code} and /s/{code} both resolve through the
	// tombstone-aware resolver and then the §9.1 route-selection matrix
	// (matching main.go's dispatch). Lifecycle statuses are unchanged:
	// expired/unsupported 410, revoked/unknown 404; lifecycle-active shares
	// get the interstitial/relay-302/503 selection outcomes.
	pbRouter.GET("/share/{code}", serveCanonicalRoute(ctrl))
	pbRouter.GET("/s/{code}", serveCanonicalRoute(ctrl))

	mux, err := pbRouter.BuildMux()
	require.NoError(t, err)
	return app, mux
}

// fixedStubRevision is a relayctl.RouteRevisionSource with a constant
// revision, shared by the test controller and the test presence view so the
// Available route-revision join agrees.
type fixedStubRevision struct{ rev uint64 }

func (s *fixedStubRevision) CurrentRevision() uint64 { return s.rev }

// seedCanonicalAgent creates the agents row for apiKeyID with a DDNS-verified
// endpoint and a uniquely-assigned relay port (the column is uniquely
// indexed), returning (record id, relay port) for presence.
func seedCanonicalAgent(t *testing.T, app core.App, apiKeyID, endpointIP string) (string, int) {
	t.Helper()
	agentsCol, err := app.FindCollectionByNameOrId("agents")
	require.NoError(t, err)
	seq := canonicalAgentSeq.Add(1)
	port := int(10000 + seq)
	rec := core.NewRecord(agentsCol)
	rec.Set("api_key_id", apiKeyID)
	// Namespace is uniquely indexed: derive a unique one per seeded agent.
	rec.Set("namespace", fmt.Sprintf("sbdead%04x", seq))
	rec.Set("cert_status", "ready")
	rec.Set("endpoint_ip", endpointIP)
	rec.Set("endpoint_port", 8443)
	rec.Set("relay_port", port)
	rec.Set("relay_generation", 7)
	require.NoError(t, app.Save(rec))
	return rec.Id, port
}

// canonicalAgentSeq hands out unique relay ports across agents in one app.
var canonicalAgentSeq atomic.Uint64

// grantCanonicalPresence applies a gateway presence snapshot granting
// (port, generation 7) for agentID with a 45 s lease.
func grantCanonicalPresence(t *testing.T, view *relayctl.PresenceView, agentID string, port int) {
	t.Helper()
	env := relayctl.PresenceEnvelope{
		Version: relayctl.ProtocolVersion,
		Events: []relayctl.PresenceEvent{{
			GatewayBootID:  "boot-canonical",
			Revision:       1,
			AgentRecordID:  agentID,
			RelayPort:      port,
			Generation:     7,
			State:          relayctl.PresenceStateOnline,
			LeaseExpiresAt: time.Now().Add(45 * time.Second).Format(time.RFC3339),
		}},
	}
	require.NoError(t, view.ApplyPresenceSnapshot(env))
}

// TestBothCanonicalRoutesPreserve404410Interstitial302And503 walks the §9.3
// canonical-resolution outcomes over BOTH canonical routes (GET /share/{code}
// and GET /s/{code}) with RELAY_SELECTION_ENABLED=true:
// unknown/revoked → 404; expired/unsupported → 410; direct candidate (only
// STUN freshness missing) → 200 no-store interstitial; relayOnly with a live
// presence lease → 302 to the derived relay hostname; direct-ineligible with
// no relay → 503. The 404/410/interstitial/302/503 outcomes are identical on
// both paths.
func TestBothCanonicalRoutesPreserve404410Interstitial302And503(t *testing.T) {
	app, cleanup := setupServerTestApp(t)
	defer cleanup()
	require.NoError(t, migrations.AddAgentsRelaySTUN(app))

	revision := &fixedStubRevision{rev: 42}
	view, err := relayctl.NewPresenceView(relayctl.PresenceViewConfig{Routes: revision})
	require.NoError(t, err)

	ctrl := directctl.NewController(app, hub.New(), nil, nil, directctl.Config{
		BaseDomain:            "example.com",
		RelaySelectionEnabled: true,
		RelayPresence:         view,
		Routes:                revision,
		// Task 22: the real shipped route-interstitial assets so the 200
		// interstitial row exercises the production renderer.
		InterstitialAssets: mustServerTestInterstitialAssets(t),
	})

	pbRouter, err := apis.NewRouter(app)
	require.NoError(t, err)
	pbRouter.GET("/share/{code}", serveCanonicalRoute(ctrl))
	pbRouter.GET("/s/{code}", serveCanonicalRoute(ctrl))
	mux, err := pbRouter.BuildMux()
	require.NoError(t, err)

	mkSession := func(code string, relayOnly bool, expires *time.Time, mutate func(*core.Record)) string {
		user := createServerTestUser(t, app, code+"@canonical.example")
		apiKey := createServerTestAPIKey(t, app, user.Id)
		record := createServerTestSession(t, app, apiKey.Id, code, expires)
		record.Set("share_type", "immich")
		// Unique per-session origin (sessions.origin is uniquely indexed);
		// lowercased so the label is a valid hostname component.
		record.Set("origin", strings.ToLower(code)+".sbdeadbeef.example.com")
		if relayOnly {
			record.Set("relay_only", true)
		}
		if mutate != nil {
			mutate(record)
		}
		require.NoError(t, app.Save(record))
		return apiKey.Id
	}

	scenarios := []struct {
		name     string
		seed     func(t *testing.T) string
		wantCode int
		wantLoc  string
	}{
		{name: "unknown 404", seed: func(t *testing.T) string { return "UNKNOWN0000" }, wantCode: http.StatusNotFound},
		{name: "revoked 404", seed: func(t *testing.T) string {
			code := "REVOKED0001"
			mkSession(code, false, nil, func(r *core.Record) {
				r.Set("is_active", false)
				r.Set("inactive_reason", "revoked")
			})
			return code
		}, wantCode: http.StatusNotFound},
		{name: "expired 410", seed: func(t *testing.T) string {
			code := "EXPIRED0002"
			past := time.Now().Add(-time.Hour)
			mkSession(code, false, &past, nil)
			return code
		}, wantCode: http.StatusGone},
		{name: "interstitial 200 no-store only STUN freshness missing", seed: func(t *testing.T) string {
			code := "INTERST0003"
			apiKey := mkSession(code, false, nil, nil)
			seedCanonicalAgent(t, app, apiKey, "203.0.113.7")
			ctrl.InstallReadyEpochForTest(apiKey, "sbdeadbeef")
			return code
		}, wantCode: http.StatusOK},
		{name: "relayOnly lease 302 relay", seed: func(t *testing.T) string {
			code := "RELAY30204"
			apiKey := mkSession(code, true, nil, nil)
			agentID, port := seedCanonicalAgent(t, app, apiKey, "203.0.113.7")
			grantCanonicalPresence(t, view, agentID, port)
			return code
		}, wantCode: http.StatusFound, wantLoc: "https://relay30204.relay.sbdeadbeef.example.com/s/"},
		{name: "direct-ineligible WS-down no lease 503", seed: func(t *testing.T) string {
			code := "OFFLINE505"
			mkSession(code, false, nil, nil)
			return code
		}, wantCode: http.StatusServiceUnavailable},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			code := sc.seed(t)
			for _, path := range []string{"/s/", "/share/"} {
				req := httptest.NewRequest(http.MethodGet, path+code, nil)
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				require.Equal(t, sc.wantCode, rec.Code, "path %s body %q", path, rec.Body.String())
				if sc.wantLoc != "" {
					require.Equal(t, sc.wantLoc+code, rec.Header().Get("Location"))
					require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
				}
				if sc.wantCode == http.StatusOK {
					require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
				}
			}
		})
	}
}

func TestServerRoutesRelayOnlySessionGone(t *testing.T) {
	app, router := setupRouterForTest(t)
	user := createServerTestUser(t, app, "relayonly-route@example.com")
	apiKey := createServerTestAPIKey(t, app, user.Id)
	session := createServerTestSession(t, app, apiKey.Id, "RELAYONLYROUTE1", nil)
	session.Set("relay_only", true)
	require.NoError(t, app.Save(session))

	req := httptest.NewRequest(http.MethodGet, "/s/RELAYONLYROUTE1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusGone, rec.Code)
}

// TestServerRoutesGallerySessionRedirectsNotWebClient asserts an active gallery
// (immich) session dispatches to the controller gate (503 when the epoch is not
// ready) rather than serving a static page.
func TestServerRoutesGallerySessionRedirectsNotWebClient(t *testing.T) {
	app, router := setupRouterForTest(t)
	user := createServerTestUser(t, app, "direct-route@example.com")
	apiKey := createServerTestAPIKey(t, app, user.Id)
	session := createServerTestSession(t, app, apiKey.Id, "DIRECTROUTE1", nil)
	session.Set("share_type", "immich")
	require.NoError(t, app.Save(session))

	req := httptest.NewRequest(http.MethodGet, "/s/DIRECTROUTE1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "direct unavailable")
}

// newServerTestPublisher builds the Task 12 route publisher wired the same
// way main.go wires it, so deleteExpiredSessions exercises the real revoke
// path (sessions without an agent relay assignment simply publish nothing).
// mustServerTestInterstitialAssets loads the real shipped route-interstitial
// assets (control/web/route-interstitial.{html,js,css}) — the same files
// main.go loads at startup and deploy-testing.sh copies — so the canonical-
// route tests exercise the production §9.3 renderer rather than a stub.
func mustServerTestInterstitialAssets(t *testing.T) directctl.InterstitialAssets {
	t.Helper()
	assets, err := directctl.LoadInterstitialAssets("../../web")
	require.NoError(t, err)
	return assets
}

func newServerTestPublisher(t *testing.T, app core.App) *relayctl.Publisher {
	t.Helper()
	publisher, err := relayctl.NewPublisher(app, relayctl.PublisherConfig{})
	require.NoError(t, err)
	return publisher
}

func TestDeleteExpiredSessions_PreservesSessionsWithoutExpiry(t *testing.T) {
	app, cleanup := setupServerTestApp(t)
	defer cleanup()

	user := createServerTestUser(t, app, "noexpiry@example.com")
	apiKey := createServerTestAPIKey(t, app, user.Id)
	createServerTestSession(t, app, apiKey.Id, "NOEXPIRY1", nil)

	require.NoError(t, deleteExpiredSessions(app, newServerTestPublisher(t, app)))
	require.True(t, testSessionExists(t, app, "NOEXPIRY1"))
}

func TestDeleteExpiredSessions_DeletesOnlyExpiredSessions(t *testing.T) {
	app, cleanup := setupServerTestApp(t)
	defer cleanup()

	user := createServerTestUser(t, app, "expired@example.com")
	apiKey := createServerTestAPIKey(t, app, user.Id)

	expiredAt := time.Now().UTC().Add(-1 * time.Hour)
	futureAt := time.Now().UTC().Add(1 * time.Hour)

	createServerTestSession(t, app, apiKey.Id, "EXPIRED1", &expiredAt)
	createServerTestSession(t, app, apiKey.Id, "FUTURE01", &futureAt)

	require.NoError(t, deleteExpiredSessions(app, newServerTestPublisher(t, app)))
	// Soft-delete: the expired row still exists (so its origin is never reused)
	// but is marked inactive.
	require.True(t, testSessionExists(t, app, "EXPIRED1"))
	require.False(t, sessionIsActive(t, app, "EXPIRED1"))
	require.True(t, testSessionExists(t, app, "FUTURE01"))
	require.True(t, sessionIsActive(t, app, "FUTURE01"))
}

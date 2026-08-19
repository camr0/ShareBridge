package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/types"
	"github.com/stretchr/testify/require"
	"sharebridge/control/internal/directctl"
	"sharebridge/control/internal/hub"
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
	// tombstone-aware resolver (§8). Active gallery shares 302 to the direct
	// origin; expired/unsupported return 410 and revoked/unknown return 404.
	pbRouter.GET("/share/{code}", serveShareRedirect(ctrl))
	pbRouter.GET("/s/{code}", func(e *core.RequestEvent) error {
		code := e.Request.PathValue("code")
		_, status := ctrl.ResolveForRedirect(code)
		switch status {
		case http.StatusFound:
			return ctrl.Redirect(e.Response, e.Request, code)
		case http.StatusGone:
			return e.Error(http.StatusGone, "share expired or unsupported", nil)
		default:
			return e.NotFoundError("session not found", nil)
		}
	})

	mux, err := pbRouter.BuildMux()
	require.NoError(t, err)
	return app, mux
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

func TestDeleteExpiredSessions_PreservesSessionsWithoutExpiry(t *testing.T) {
	app, cleanup := setupServerTestApp(t)
	defer cleanup()

	user := createServerTestUser(t, app, "noexpiry@example.com")
	apiKey := createServerTestAPIKey(t, app, user.Id)
	createServerTestSession(t, app, apiKey.Id, "NOEXPIRY1", nil)

	require.NoError(t, deleteExpiredSessions(app))
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

	require.NoError(t, deleteExpiredSessions(app))
	// Soft-delete: the expired row still exists (so its origin is never reused)
	// but is marked inactive.
	require.True(t, testSessionExists(t, app, "EXPIRED1"))
	require.False(t, sessionIsActive(t, app, "EXPIRED1"))
	require.True(t, testSessionExists(t, app, "FUTURE01"))
	require.True(t, sessionIsActive(t, app, "FUTURE01"))
}

package relay_test

import (
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/require"
	"sharebridge/control/internal/relay"
	"sharebridge/control/migrations"
)

func setupRelayTestApp(t *testing.T) (core.App, func()) {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, testApp.Bootstrap())
	require.NoError(t, testApp.RunSystemMigrations())
	require.NoError(t, migrations.CreateCollections(testApp))
	require.NoError(t, migrations.AddAPIKeyTimestamps(testApp))
	require.NoError(t, migrations.AddQuotaFields(testApp))
	cleanup := func() { testApp.Cleanup() }
	return testApp, cleanup
}

func createRelayTestAccountWithQuota(app core.App, email string, quotaGB float64, usageGB float64) (*core.Record, error) {
	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return nil, err
	}
	user := core.NewRecord(usersCol)
	user.SetEmail(email)
	user.SetPassword("testpassword123")
	user.Set("relay_quota_gb", quotaGB)
	user.Set("current_period_usage_gb", usageGB)
	user.Set("quota_period_start", time.Now().UTC())
	user.Set("quota_period_end", time.Now().UTC().Add(30*24*time.Hour))
	user.Set("turn_baseline_bytes", 0.0)
	if err := app.Save(user); err != nil {
		return nil, err
	}
	return user, nil
}

func TestApplyRelayBytes_IncrementsCurrentPeriodUsage(t *testing.T) {
	app, cleanup := setupRelayTestApp(t)
	defer cleanup()

	user, err := createRelayTestAccountWithQuota(app, "acct@example.com", 50.0, 0.0)
	require.NoError(t, err)

	now := time.Now().UTC()
	if err := relay.ApplyRelayBytes(app, user.Id, 5_000_000, now); err != nil {
		t.Fatal(err)
	}

	updated, err := app.FindRecordById("users", user.Id)
	require.NoError(t, err)
	usage := updated.GetFloat("current_period_usage_gb")
	expected := 0.005 // 5MB = 0.005 GB
	if usage < expected-0.0001 || usage > expected+0.0001 {
		t.Errorf("usage = %v, expected ~%v", usage, expected)
	}
}

func TestApplyRelayBytes_RollsExpiredPeriodAndArchivesOldUsage(t *testing.T) {
	app, cleanup := setupRelayTestApp(t)
	defer cleanup()

	user, err := createRelayTestAccountWithQuota(app, "expired@example.com", 50.0, 3.25)
	require.NoError(t, err)
	// Set expired period
	user.Set("quota_period_start", time.Now().UTC().Add(-40*24*time.Hour))
	user.Set("quota_period_end", time.Now().UTC().Add(-24*time.Hour))
	require.NoError(t, app.Save(user))

	// Apply bytes - should trigger rollover
	if err := relay.ApplyRelayBytes(app, user.Id, 1_000_000_000, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// Check archived in bandwidth_usage
	bw, err := app.FindRecordsByFilter("bandwidth_usage", "account_id = {:id}", "", 10, 0, map[string]any{"id": user.Id})
	require.NoError(t, err)
	if len(bw) != 1 {
		t.Fatalf("expected 1 bandwidth_usage record, got %d", len(bw))
	}
	archivedBytes := int64(bw[0].GetFloat("bytes_transferred"))
	expected := int64(3_250_000_000) // 3.25 GB
	if archivedBytes != expected {
		t.Errorf("archived bytes = %v, expected %v", archivedBytes, expected)
	}

	// Check that current_period_usage_gb is reset and includes new bytes
	updated, err := app.FindRecordById("users", user.Id)
	require.NoError(t, err)
	usage := updated.GetFloat("current_period_usage_gb")
	expectedUsage := 1.0 // 1GB from the ApplyRelayBytes call
	if usage < expectedUsage-0.0001 || usage > expectedUsage+0.0001 {
		t.Errorf("usage = %v, expected ~%v", usage, expectedUsage)
	}
}

func TestApplyRelayBytes_NoRolloverWhenPeriodActive(t *testing.T) {
	app, cleanup := setupRelayTestApp(t)
	defer cleanup()

	user, err := createRelayTestAccountWithQuota(app, "active@example.com", 50.0, 2.0)
	require.NoError(t, err)

	now := time.Now().UTC()
	if err := relay.ApplyRelayBytes(app, user.Id, 500_000_000, now); err != nil {
		t.Fatal(err)
	}

	updated, err := app.FindRecordById("users", user.Id)
	require.NoError(t, err)
	usage := updated.GetFloat("current_period_usage_gb")
	expected := 2.5 // 2.0 + 0.5 GB
	if usage < expected-0.0001 || usage > expected+0.0001 {
		t.Errorf("usage = %v, expected ~%v", usage, expected)
	}

	// Should not have created bandwidth_usage record
	bw, err := app.FindRecordsByFilter("bandwidth_usage", "account_id = {:id}", "", 10, 0, map[string]any{"id": user.Id})
	require.NoError(t, err)
	if len(bw) != 0 {
		t.Errorf("expected 0 bandwidth_usage records, got %d", len(bw))
	}
}

func TestApplyRelayBytes_AccumulatesMultipleCalls(t *testing.T) {
	app, cleanup := setupRelayTestApp(t)
	defer cleanup()

	user, err := createRelayTestAccountWithQuota(app, "multi@example.com", 50.0, 0.0)
	require.NoError(t, err)

	now := time.Now().UTC()
	// Apply bytes multiple times
	if err := relay.ApplyRelayBytes(app, user.Id, 100_000_000, now); err != nil {
		t.Fatal(err)
	}
	if err := relay.ApplyRelayBytes(app, user.Id, 200_000_000, now); err != nil {
		t.Fatal(err)
	}
	if err := relay.ApplyRelayBytes(app, user.Id, 300_000_000, now); err != nil {
		t.Fatal(err)
	}

	updated, err := app.FindRecordById("users", user.Id)
	require.NoError(t, err)
	usage := updated.GetFloat("current_period_usage_gb")
	expected := 0.6 // 100MB + 200MB + 300MB = 600MB = 0.6 GB
	if usage < expected-0.0001 || usage > expected+0.0001 {
		t.Errorf("usage = %v, expected ~%v", usage, expected)
	}
}

func TestApplyRelayBytes_ConcurrentIncrements(t *testing.T) {
	app, cleanup := setupRelayTestApp(t)
	defer cleanup()

	user, err := createRelayTestAccountWithQuota(app, "concurrent@example.com", 50.0, 0.0)
	require.NoError(t, err)

	// Launch 100 concurrent goroutines, each incrementing by 10MB
	const numGoroutines = 100
	const bytesPerCall = 10_000_000 // 10MB

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	now := time.Now().UTC()
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			if err := relay.ApplyRelayBytes(app, user.Id, bytesPerCall, now); err != nil {
				t.Errorf("ApplyRelayBytes failed: %v", err)
			}
		}()
	}
	wg.Wait()

	// Verify all increments were applied atomically (no lost updates)
	updated, err := app.FindRecordById("users", user.Id)
	require.NoError(t, err)
	usage := updated.GetFloat("current_period_usage_gb")
	expected := float64(numGoroutines*bytesPerCall) / relay.GB
	// Allow small floating point tolerance
	if usage < expected-0.0001 || usage > expected+0.0001 {
		t.Errorf("usage = %v, expected ~%v (lost updates detected)", usage, expected)
	}
}
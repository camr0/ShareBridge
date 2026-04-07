package quota

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/types"
	"github.com/stretchr/testify/require"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/metrics"
	"sharebridge/server/migrations"
)

// setupTestApp creates a test PocketBase app with all migrations applied.
func setupTestApp(t *testing.T) *tests.TestApp {
	testApp, err := tests.NewTestApp(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { testApp.Cleanup() })

	// Bootstrap and run system migrations
	err = testApp.Bootstrap()
	require.NoError(t, err)
	err = testApp.RunSystemMigrations()
	require.NoError(t, err)

	// Run our migrations
	err = migrations.CreateCollections(testApp)
	require.NoError(t, err)
	err = migrations.AddAPIKeyTimestamps(testApp)
	require.NoError(t, err)
	err = migrations.AddQuotaFields(testApp)
	require.NoError(t, err)

	return testApp
}

// createTestUser creates a test user with quota fields set.
func createTestUser(t *testing.T, app core.App, relayQuotaGB float64, usageGB float64, periodEnd time.Time) *core.Record {
	usersCol, err := app.FindCollectionByNameOrId("users")
	require.NoError(t, err)

	record := core.NewRecord(usersCol)
	record.Set("email", fmt.Sprintf("test%d@example.com", time.Now().UnixNano()))
	record.Set("password", "testpassword123")
	record.Set("relay_quota_gb", relayQuotaGB)
	record.Set("current_period_usage_gb", usageGB)
	record.Set("quota_period_start", types.NowDateTime().Add(-24*time.Hour))
	record.Set("quota_period_end", types.NowDateTime().Add(periodEnd.Sub(time.Now().UTC())))
	record.Set("turn_baseline_bytes", 0.0)

	err = app.Save(record)
	require.NoError(t, err)

	return record
}

// createMockPrometheusServer creates a mock Prometheus server that returns the specified bytes.
func createMockPrometheusServer(bytes int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []map[string]any{
					{
						"metric": map[string]string{},
						"value":  []any{1700000000, string(rune('0' + bytes%10))},
					},
				},
			},
		})
	}))
}

// createMockPrometheusServerWithValue creates a mock Prometheus server that returns a specific value.
func createMockPrometheusServerWithValue(value int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []map[string]any{
					{
						"metric": map[string]string{},
						"value":  []any{1700000000, formatInt64(value)},
					},
				},
			},
		})
	}))
}

func formatInt64(v int64) string {
	var buf [20]byte
	i := len(buf) - 1
	if v == 0 {
		return "0"
	}
	for v > 0 {
		buf[i] = byte('0' + v%10)
		v /= 10
		i--
	}
	return string(buf[i+1:])
}

func TestPoller_UpdateUsage(t *testing.T) {
	// Create mock Prometheus server returning 1 GB worth of bytes
	mockPrometheus := createMockPrometheusServerWithValue(1073741824) // 1 GB in bytes
	defer mockPrometheus.Close()

	testApp := setupTestApp(t)

	// Create a user with a quota period in the future (not ending yet)
	futureEnd := time.Now().UTC().Add(24 * time.Hour)
	user := createTestUser(t, testApp, 100.0, 0.0, futureEnd)

	// Create config and poller
	cfg := &config.Config{
		DefaultQuotaGB:     100.0,
		QuotaCheckInterval: 1 * time.Minute,
	}

	metricsClient := metrics.NewPrometheusClient(mockPrometheus.URL)
	poller := NewPoller(testApp, metricsClient, cfg)

	// Manually trigger a poll
	err := poller.poll()
	require.NoError(t, err)

	// Fetch updated user
	updatedUser, err := testApp.FindRecordById("users", user.Id)
	require.NoError(t, err)

	// Verify usage was updated (should be ~1 GB)
	usage := updatedUser.GetFloat("current_period_usage_gb")
	require.Greater(t, usage, 0.9)
	require.Less(t, usage, 1.1)
}

func TestPoller_PeriodReset(t *testing.T) {
	// Create mock Prometheus server returning 2 GB worth of bytes
	mockPrometheus := createMockPrometheusServerWithValue(2147483648) // 2 GB in bytes
	defer mockPrometheus.Close()

	testApp := setupTestApp(t)

	// Create a user with a quota period that has already ended
	usersCol, err := testApp.FindCollectionByNameOrId("users")
	require.NoError(t, err)

	user := core.NewRecord(usersCol)
	user.Set("email", fmt.Sprintf("test%d@example.com", time.Now().UnixNano()))
	user.Set("password", "testpassword123")
	user.Set("relay_quota_gb", 100.0)
	user.Set("current_period_usage_gb", 1.5) // 1.5 GB usage in old period
	user.Set("quota_period_start", types.NowDateTime().Add(-54*24*time.Hour))
	user.Set("quota_period_end", types.NowDateTime().Add(-24*time.Hour))
	user.Set("turn_baseline_bytes", 0.0)

	err = testApp.Save(user)
	require.NoError(t, err)

	// Create config and poller
	cfg := &config.Config{
		DefaultQuotaGB:     100.0,
		QuotaCheckInterval: 1 * time.Minute,
	}

	metricsClient := metrics.NewPrometheusClient(mockPrometheus.URL)
	poller := NewPoller(testApp, metricsClient, cfg)

	// Manually trigger a poll
	err = poller.poll()
	require.NoError(t, err)

	// Fetch updated user
	updatedUser, err := testApp.FindRecordById("users", user.Id)
	require.NoError(t, err)

	// Verify usage was reset to 0 (new period)
	usage := updatedUser.GetFloat("current_period_usage_gb")
	// The new baseline will be set to the current Prometheus value, so net usage should be 0
	require.InDelta(t, 0.0, usage, 0.1)

	// Verify period was updated
	newPeriodEnd := updatedUser.GetDateTime("quota_period_end")
	require.True(t, newPeriodEnd.Time().After(time.Now().UTC()), "new period end should be in the future")
}

func TestPoller_ArchiveOnReset(t *testing.T) {
	// Create mock Prometheus server
	mockPrometheus := createMockPrometheusServerWithValue(2147483648) // 2 GB in bytes
	defer mockPrometheus.Close()

	testApp := setupTestApp(t)

	// Create a user with a quota period that has already ended
	usersCol, err := testApp.FindCollectionByNameOrId("users")
	require.NoError(t, err)

	user := core.NewRecord(usersCol)
	user.Set("email", fmt.Sprintf("test%d@example.com", time.Now().UnixNano()))
	user.Set("password", "testpassword123")
	user.Set("relay_quota_gb", 100.0)
	user.Set("current_period_usage_gb", 1.5) // 1.5 GB usage to archive
	user.Set("quota_period_start", types.NowDateTime().Add(-30*24*time.Hour))
	user.Set("quota_period_end", types.NowDateTime().Add(-24*time.Hour))
	user.Set("turn_baseline_bytes", 0.0)

	err = testApp.Save(user)
	require.NoError(t, err)

	// Create config and poller
	cfg := &config.Config{
		DefaultQuotaGB:     100.0,
		QuotaCheckInterval: 1 * time.Minute,
	}

	metricsClient := metrics.NewPrometheusClient(mockPrometheus.URL)
	poller := NewPoller(testApp, metricsClient, cfg)

	// Manually trigger a poll
	err = poller.poll()
	require.NoError(t, err)

	// Check bandwidth_usage collection for archived record
	bwRecords, err := testApp.FindRecordsByFilter(
		"bandwidth_usage",
		"account_id = {:accountID}",
		"-created",
		1,
		0,
		map[string]any{"accountID": user.Id},
	)
	require.NoError(t, err)
	require.Len(t, bwRecords, 1, "should have one bandwidth_usage record")

	archived := bwRecords[0]
	require.Equal(t, user.Id, archived.GetString("account_id"))
	require.InDelta(t, int64(1.5*1e9), archived.GetInt("bytes_transferred"), 1000000)
}

func TestPoller_StartStop(t *testing.T) {
	mockPrometheus := createMockPrometheusServerWithValue(1073741824)
	defer mockPrometheus.Close()

	testApp := setupTestApp(t)

	cfg := &config.Config{
		DefaultQuotaGB:     100.0,
		QuotaCheckInterval: 100 * time.Millisecond,
	}

	metricsClient := metrics.NewPrometheusClient(mockPrometheus.URL)
	poller := NewPoller(testApp, metricsClient, cfg)

	// Start should not panic
	poller.Start()
	require.True(t, poller.started.Load())

	// Stop should not panic
	poller.Stop()
	require.False(t, poller.started.Load())

	// Double stop should not panic
	poller.Stop()
}

func TestPoller_MultipleAccounts(t *testing.T) {
	// Create mock Prometheus server
	mockPrometheus := createMockPrometheusServerWithValue(536870912) // 0.5 GB in bytes
	defer mockPrometheus.Close()

	testApp := setupTestApp(t)

	// Create multiple users
	futureEnd := time.Now().UTC().Add(24 * time.Hour)
	user1 := createTestUser(t, testApp, 100.0, 0.0, futureEnd)
	user2 := createTestUser(t, testApp, 200.0, 0.0, futureEnd)

	// Create config and poller
	cfg := &config.Config{
		DefaultQuotaGB:     100.0,
		QuotaCheckInterval: 1 * time.Minute,
	}

	metricsClient := metrics.NewPrometheusClient(mockPrometheus.URL)
	poller := NewPoller(testApp, metricsClient, cfg)

	// Manually trigger a poll
	err := poller.poll()
	require.NoError(t, err)

	// Verify both users were updated
	updatedUser1, err := testApp.FindRecordById("users", user1.Id)
	require.NoError(t, err)
	usage1 := updatedUser1.GetFloat("current_period_usage_gb")
	require.Greater(t, usage1, 0.4)
	require.Less(t, usage1, 0.6)

	updatedUser2, err := testApp.FindRecordById("users", user2.Id)
	require.NoError(t, err)
	usage2 := updatedUser2.GetFloat("current_period_usage_gb")
	require.Greater(t, usage2, 0.4)
	require.Less(t, usage2, 0.6)
}

func TestPoller_CoturnRestart_PreservesPreRestartUsage(t *testing.T) {
	// Scenario: Coturn restarted mid-period.
	//   - turn_baseline_bytes = 10 GB  (set at last period reset — traffic before this period)
	//   - current_period_usage_gb = 5 GB  (accumulated during current period before restart)
	//   - Prometheus counter after restart = 3 GB  (counter reset to 0, then 3 GB of new traffic)
	//
	// Detection: totalBytes (3 GB) < baselineBytes (10 GB) → restart
	//
	// Fix: virtual negative baseline
	//   newBaseline = totalBytes - preRestartBytes = 3 GB - 5 GB = -2 GB
	//   netBytes    = totalBytes - newBaseline     = 3 GB - (-2 GB) = 5 GB  ← preserved ✓
	//
	// On subsequent polls (e.g., totalBytes = 4 GB):
	//   netBytes = 4 GB - (-2 GB) = 6 GB  ← 5 GB pre-restart + 1 GB post-restart ✓
	mockPrometheus := createMockPrometheusServerWithValue(3_000_000_000) // 3 GB post-restart
	defer mockPrometheus.Close()

	testApp := setupTestApp(t)

	usersCol, err := testApp.FindCollectionByNameOrId("users")
	require.NoError(t, err)

	user := core.NewRecord(usersCol)
	user.Set("email", fmt.Sprintf("test%d@example.com", time.Now().UnixNano()))
	user.Set("password", "testpassword123")
	user.Set("relay_quota_gb", 50.0)
	user.Set("current_period_usage_gb", 5.0)
	user.Set("quota_period_start", types.NowDateTime().Add(-15*24*time.Hour))
	user.Set("quota_period_end", types.NowDateTime().Add(15*24*time.Hour))
	user.Set("turn_baseline_bytes", 10_000_000_000.0) // 10 GB baseline from last period reset

	err = testApp.Save(user)
	require.NoError(t, err)

	cfg := &config.Config{
		DefaultQuotaGB:     50.0,
		QuotaCheckInterval: 1 * time.Minute,
	}
	metricsClient := metrics.NewPrometheusClient(mockPrometheus.URL)
	poller := NewPoller(testApp, metricsClient, cfg)

	err = poller.poll()
	require.NoError(t, err)

	updated, err := testApp.FindRecordById("users", user.Id)
	require.NoError(t, err)

	// Usage must remain ~5 GB — pre-restart traffic is preserved, not lost.
	usage := updated.GetFloat("current_period_usage_gb")
	require.InDelta(t, 5.0, usage, 0.01, "pre-restart usage should be preserved")

	// Baseline should be negative: 3 GB - 5 GB = -2 GB
	baseline := updated.GetFloat("turn_baseline_bytes")
	require.InDelta(t, -2_000_000_000.0, baseline, 1000.0, "baseline should be virtual negative to encode pre-restart offset")
}

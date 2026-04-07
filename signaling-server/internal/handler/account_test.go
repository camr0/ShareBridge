package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/stretchr/testify/require"
)

func TestBuildQuotaResponse(t *testing.T) {
	now := time.Now().UTC()
	periodEnd := now.Add(30 * 24 * time.Hour)

	tests := []struct {
		name           string
		limitGB        float64
		usedGB         float64
		periodStart    time.Time
		periodEnd      time.Time
		expectedLimit  float64
		expectedUsed   float64
		expectedRemain float64
		expectedPct    int
	}{
		{
			name:           "zero usage",
			limitGB:        100.0,
			usedGB:         0.0,
			periodStart:    now,
			periodEnd:      periodEnd,
			expectedLimit:  100.0,
			expectedUsed:   0.0,
			expectedRemain: 100.0,
			expectedPct:    0,
		},
		{
			name:           "50% usage",
			limitGB:        100.0,
			usedGB:         50.0,
			periodStart:    now,
			periodEnd:      periodEnd,
			expectedLimit:  100.0,
			expectedUsed:   50.0,
			expectedRemain: 50.0,
			expectedPct:    50,
		},
		{
			name:           "100% usage (at limit)",
			limitGB:        100.0,
			usedGB:         100.0,
			periodStart:    now,
			periodEnd:      periodEnd,
			expectedLimit:  100.0,
			expectedUsed:   100.0,
			expectedRemain: 0.0,
			expectedPct:    100,
		},
		{
			name:           "exceeded limit",
			limitGB:        100.0,
			usedGB:         150.0,
			periodStart:    now,
			periodEnd:      periodEnd,
			expectedLimit:  100.0,
			expectedUsed:   150.0,
			expectedRemain: 0.0,
			expectedPct:    100,
		},
		{
			name:           "unlimited quota (zero limit)",
			limitGB:        0.0,
			usedGB:         50.0,
			periodStart:    now,
			periodEnd:      periodEnd,
			expectedLimit:  0.0,
			expectedUsed:   50.0,
			expectedRemain: 0.0,
			expectedPct:    0,
		},
		{
			name:           "fractional usage",
			limitGB:        10.0,
			usedGB:         3.14159,
			periodStart:    now,
			periodEnd:      periodEnd,
			expectedLimit:  10.0,
			expectedUsed:   3.14159,
			expectedRemain: 6.85841,
			expectedPct:    31,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildQuotaResponse(tt.limitGB, tt.usedGB, tt.periodStart, tt.periodEnd)

			require.InDelta(t, tt.expectedLimit, result.LimitGB, 0.001)
			require.InDelta(t, tt.expectedUsed, result.UsedGB, 0.001)
			require.InDelta(t, tt.expectedRemain, result.RemainingGB, 0.001)
			require.Equal(t, tt.expectedPct, result.PercentageUsed)
			require.WithinDuration(t, tt.periodStart, result.PeriodStart, time.Second)
			require.WithinDuration(t, tt.periodEnd, result.PeriodEnd, time.Second)
		})
	}
}

func TestGetAccount(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	user, err := createTestUser(app, "account-test@example.com")
	require.NoError(t, err)

	// Set quota fields on user
	now := time.Now().UTC()
	periodEnd := now.Add(30 * 24 * time.Hour)
	user.Set("relay_quota_gb", 50.0)
	user.Set("current_period_usage_gb", 10.0)
	user.Set("quota_period_start", now)
	user.Set("quota_period_end", periodEnd)
	err = app.Save(user)
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodGet, "/api/account", nil)
	recorder := httptest.NewRecorder()

	requestEvent := new(core.RequestEvent)
	requestEvent.App = app
	requestEvent.Request = request
	requestEvent.Response = recorder
	requestEvent.Auth = user

	err = GetAccount(app)(requestEvent)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var response AccountResponse
	err = json.Unmarshal(recorder.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Equal(t, user.Id, response.ID)
	require.Equal(t, "account-test@example.com", response.Email)
	require.InDelta(t, 50.0, response.Quota.LimitGB, 0.001)
	require.InDelta(t, 10.0, response.Quota.UsedGB, 0.001)
}

func TestGetAccount_Unauthorized(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()
	defer cleanup()

	request := httptest.NewRequest(http.MethodGet, "/api/account", nil)
	recorder := httptest.NewRecorder()

	requestEvent := new(core.RequestEvent)
	requestEvent.App = app
	requestEvent.Request = request
	requestEvent.Response = recorder
	requestEvent.Auth = nil // Not authenticated

	err := GetAccount(app)(requestEvent)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestGetAccountQuota(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	user, err := createTestUser(app, "quota-test@example.com")
	require.NoError(t, err)

	// Set quota fields on user
	now := time.Now().UTC()
	periodEnd := now.Add(30 * 24 * time.Hour)
	user.Set("relay_quota_gb", 100.0)
	user.Set("current_period_usage_gb", 25.0)
	user.Set("quota_period_start", now)
	user.Set("quota_period_end", periodEnd)
	err = app.Save(user)
	require.NoError(t, err)

	secret := "quotatestsecret"
	apiKey, err := createTestAPIKey(app, user.Id, secret)
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodGet, "/api/account/quota", nil)
	recorder := httptest.NewRecorder()

	requestEvent := new(core.RequestEvent)
	requestEvent.App = app
	requestEvent.Request = request
	requestEvent.Response = recorder

	// Simulate API key auth by setting account_id in context
	ctx := request.Context()
	ctx = withAccountID(ctx, user.Id)
	requestEvent.Request = request.WithContext(ctx)

	err = GetAccountQuota(app)(requestEvent)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var response QuotaInfo
	err = json.Unmarshal(recorder.Body.Bytes(), &response)
	require.NoError(t, err)
	require.InDelta(t, 100.0, response.LimitGB, 0.001)
	require.InDelta(t, 25.0, response.UsedGB, 0.001)
	require.InDelta(t, 75.0, response.RemainingGB, 0.001)
	require.Equal(t, 25, response.PercentageUsed)

	// Verify API key was tracked
	_ = apiKey
}

func TestGetAccountQuota_MissingAuth(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	request := httptest.NewRequest(http.MethodGet, "/api/account/quota", nil)
	recorder := httptest.NewRecorder()

	requestEvent := new(core.RequestEvent)
	requestEvent.App = app
	requestEvent.Request = request
	requestEvent.Response = recorder

	// No account_id in context (no API key auth)
	err := GetAccountQuota(app)(requestEvent)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestGetAccountQuota_UserNotFound(t *testing.T) {
	app, cleanup := setupAgentTestApp(t)
	defer cleanup()

	request := httptest.NewRequest(http.MethodGet, "/api/account/quota", nil)
	recorder := httptest.NewRecorder()

	requestEvent := new(core.RequestEvent)
	requestEvent.App = app
	requestEvent.Request = request
	requestEvent.Response = recorder

	// Set non-existent account_id
	ctx := request.Context()
	ctx = withAccountID(ctx, "non-existent-user-id")
	requestEvent.Request = request.WithContext(ctx)

	err := GetAccountQuota(app)(requestEvent)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, recorder.Code)
}

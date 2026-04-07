package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// QuotaInfo represents quota information for an account
type QuotaInfo struct {
	LimitGB        float64   `json:"limit_gb"`
	UsedGB         float64   `json:"used_gb"`
	RemainingGB    float64   `json:"remaining_gb"`
	PeriodStart    time.Time `json:"period_start"`
	PeriodEnd      time.Time `json:"period_end"`
	PercentageUsed float64   `json:"percentage_used"`
}

// AccountResponse represents account information for the dashboard
type AccountResponse struct {
	ID    string    `json:"id"`
	Email string    `json:"email"`
	Quota QuotaInfo `json:"quota"`
}

// contextKey is a private type for context keys to avoid collisions
type accountContextKey string

const (
	accountIDContextKey accountContextKey = "account_id"
)

// withAccountID returns a new context with the account ID.
// This mirrors the same function in middleware package for test compatibility.
func withAccountID(ctx context.Context, accountID string) context.Context {
	return context.WithValue(ctx, accountIDContextKey, accountID)
}

// getAccountID retrieves the account ID from the request context.
// Returns empty string if not found.
func getAccountID(ctx context.Context) string {
	if id, ok := ctx.Value(accountIDContextKey).(string); ok {
		return id
	}
	return ""
}

// buildQuotaResponse constructs a QuotaInfo from raw quota data.
// Calculates remaining and percentage values.
func buildQuotaResponse(limitGB, usedGB float64, periodStart, periodEnd time.Time) QuotaInfo {
	remainingGB := limitGB - usedGB
	if remainingGB < 0 {
		remainingGB = 0
	}

	percentageUsed := 0.0
	if limitGB > 0 {
		percentageUsed = (usedGB / limitGB) * 100.0
		if percentageUsed > 100.0 {
			percentageUsed = 100.0
		}
	}

	return QuotaInfo{
		LimitGB:        limitGB,
		UsedGB:         usedGB,
		RemainingGB:    remainingGB,
		PeriodStart:    periodStart,
		PeriodEnd:      periodEnd,
		PercentageUsed: percentageUsed,
	}
}

// GetAccount returns account information for the authenticated user.
// Requires JWT authentication via apis.RequireAuth middleware.
// GET /api/account
func GetAccount(app core.App) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		authRecord := e.Auth
		if authRecord == nil {
			return e.JSON(http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		}

		// Get quota fields from user record
		relayQuotaGB := authRecord.GetFloat("relay_quota_gb")
		currentUsageGB := authRecord.GetFloat("current_period_usage_gb")

		var periodStart, periodEnd time.Time

		periodStartTime := authRecord.GetDateTime("quota_period_start")
		if !periodStartTime.IsZero() {
			periodStart = periodStartTime.Time()
		}

		periodEndTime := authRecord.GetDateTime("quota_period_end")
		if !periodEndTime.IsZero() {
			periodEnd = periodEndTime.Time()
		}

		quota := buildQuotaResponse(relayQuotaGB, currentUsageGB, periodStart, periodEnd)

		return e.JSON(http.StatusOK, AccountResponse{
			ID:    authRecord.Id,
			Email: authRecord.Email(),
			Quota: quota,
		})
	}
}

// GetAccountQuota returns quota information for an account.
// Requires API key authentication via middleware.APIKeyAuth.
// GET /api/account/quota
func GetAccountQuota(app core.App) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		// Extract account_id from context (set by APIKeyAuth middleware)
		accountID := getAccountID(e.Request.Context())
		if accountID == "" {
			return e.JSON(http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		}

		// Fetch the user record
		user, err := app.FindRecordById("users", accountID)
		if err != nil {
			return e.JSON(http.StatusNotFound, map[string]string{"error": "account not found"})
		}

		// Get quota fields from user record
		relayQuotaGB := user.GetFloat("relay_quota_gb")
		currentUsageGB := user.GetFloat("current_period_usage_gb")

		var periodStart, periodEnd time.Time

		periodStartTime := user.GetDateTime("quota_period_start")
		if !periodStartTime.IsZero() {
			periodStart = periodStartTime.Time()
		}

		periodEndTime := user.GetDateTime("quota_period_end")
		if !periodEndTime.IsZero() {
			periodEnd = periodEndTime.Time()
		}

		quota := buildQuotaResponse(relayQuotaGB, currentUsageGB, periodStart, periodEnd)

		return e.JSON(http.StatusOK, quota)
	}
}

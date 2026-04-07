package handler

import (
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/middleware"
)

// QuotaInfo represents quota information for an account
type QuotaInfo struct {
	LimitGB        float64   `json:"limit_gb"`
	UsedGB         float64   `json:"used_gb"`
	RemainingGB    float64   `json:"remaining_gb"`
	PeriodStart    time.Time `json:"period_start"`
	PeriodEnd      time.Time `json:"period_end"`
	PercentageUsed int       `json:"percentage_used"`
}

// AccountResponse represents account information for the dashboard
type AccountResponse struct {
	ID    string    `json:"id"`
	Email string    `json:"email"`
	Quota QuotaInfo `json:"quota"`
}

// buildQuotaResponse constructs a QuotaInfo from raw quota data.
// Calculates remaining and percentage values.
func buildQuotaResponse(limitGB, usedGB float64, periodStart, periodEnd time.Time) QuotaInfo {
	remainingGB := limitGB - usedGB
	if remainingGB < 0 {
		remainingGB = 0
	}

	percentageUsed := 0
	if limitGB > 0 {
		percentageUsed = int((usedGB / limitGB) * 100.0)
		if percentageUsed > 100 {
			percentageUsed = 100
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
		accountID := middleware.GetAccountID(e.Request.Context())
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

package handler

import (
	"fmt"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// checkRelayQuota checks if the account has exceeded their relay quota.
// Returns (exceeded, periodEnd) where exceeded is true if usage >= limit.
func checkRelayQuota(accountRecord *core.Record) (bool, time.Time) {
	limitGB := accountRecord.GetFloat("relay_quota_gb")
	if limitGB <= 0 {
		limitGB = 50.0 // Default quota
	}
	usedGB := accountRecord.GetFloat("current_period_usage_gb")
	periodEnd := accountRecord.GetDateTime("quota_period_end").Time()
	if time.Now().After(periodEnd) {
		return false, periodEnd
	}
	return usedGB >= limitGB, periodEnd
}

// lookupAccountForAPIKey retrieves the account record for a given API key ID.
func lookupAccountForAPIKey(app core.App, apiKeyID string) (*core.Record, error) {
	apiKeyRecord, err := app.FindRecordById("api_keys", apiKeyID)
	if err != nil {
		return nil, fmt.Errorf("api_keys %s: %w", apiKeyID, err)
	}
	accountID := apiKeyRecord.GetString("account_id")
	return app.FindRecordById("users", accountID)
}
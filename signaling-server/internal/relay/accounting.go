package relay

import (
	"fmt"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// ApplyRelayBytes adds relay bytes to the user's current_period_usage_gb.
// If the quota period has expired, it rolls over to a new period and archives
// the old usage to the bandwidth_usage table.
func ApplyRelayBytes(app core.App, accountID string, forwardedBytes int64, now time.Time) error {
	record, err := app.FindRecordById("users", accountID)
	if err != nil {
		return err
	}

	periodEnd := record.GetDateTime("quota_period_end").Time()
	if !periodEnd.IsZero() && now.After(periodEnd) {
		if err := rollQuotaPeriod(app, record, now); err != nil {
			return err
		}
		// Reload the record after rollover to get fresh values
		record, err = app.FindRecordById("users", accountID)
		if err != nil {
			return err
		}
	}

	current := record.GetFloat("current_period_usage_gb")
	record.Set("current_period_usage_gb", current+(float64(forwardedBytes)/1e9))
	return app.Save(record)
}

// rollQuotaPeriod archives the current period's usage to bandwidth_usage and
// resets the user's quota period.
func rollQuotaPeriod(app core.App, record *core.Record, now time.Time) error {
	bwCol, err := app.FindCollectionByNameOrId("bandwidth_usage")
	if err != nil {
		return err
	}

	archive := core.NewRecord(bwCol)
	archive.Set("account_id", record.Id)
	archive.Set("period_start", record.GetDateTime("quota_period_start").Time())
	archive.Set("period_end", record.GetDateTime("quota_period_end").Time())
	archive.Set("bytes_transferred", int64(record.GetFloat("current_period_usage_gb")*1e9))
	if err := app.Save(archive); err != nil {
		return fmt.Errorf("archive quota period: %w", err)
	}

	record.Set("current_period_usage_gb", 0.0)
	record.Set("quota_period_start", now.UTC())
	record.Set("quota_period_end", now.UTC().Add(30*24*time.Hour))
	return app.Save(record)
}
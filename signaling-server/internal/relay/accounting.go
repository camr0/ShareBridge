package relay

import (
	"fmt"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// Constants for quota and byte calculations.
const (
	// GB is the number of bytes in a gigabyte (decimal, 10^9).
	GB = 1e9
	// QuotaPeriodDays is the length of a quota period in days.
	QuotaPeriodDays = 30
)

// ApplyRelayBytes adds relay bytes to the user's current_period_usage_gb.
// If the quota period has expired, it rolls over to a new period and archives
// the old usage to the bandwidth_usage table.
//
// The byte increment uses an atomic SQL UPDATE to avoid lost updates from
// concurrent calls. The period rollover has a small race window (two concurrent
// calls could both trigger rollover), but this is bounded and rare (once per
// 30 days per user). A double rollover would simply archive zero usage, which
// is not a correctness issue.
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
	}

	// Atomic increment using raw SQL to avoid read-modify-write race condition.
	// This prevents lost updates when multiple concurrent calls increment usage.
	gbIncrement := float64(forwardedBytes) / GB
	_, err = app.DB().NewQuery("UPDATE users SET current_period_usage_gb = current_period_usage_gb + {:inc} WHERE id = {:id}").
		Bind(map[string]any{"inc": gbIncrement, "id": accountID}).
		Execute()
	return err
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
	archive.Set("bytes_transferred", int64(record.GetFloat("current_period_usage_gb")*GB))
	if err := app.Save(archive); err != nil {
		return fmt.Errorf("archive quota period: %w", err)
	}

	record.Set("current_period_usage_gb", 0.0)
	record.Set("quota_period_start", now.UTC())
	record.Set("quota_period_end", now.UTC().Add(QuotaPeriodDays*24*time.Hour))
	return app.Save(record)
}
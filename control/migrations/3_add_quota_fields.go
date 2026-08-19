package migrations

import (
	"fmt"
	"time"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(AddQuotaFields, nil)
}

// AddQuotaFields adds quota fields to the users collection and creates the
// bandwidth_usage audit collection for tracking bandwidth usage per account.
func AddQuotaFields(app core.App) error {
	if err := addQuotaFieldsToUsers(app); err != nil {
		return err
	}
	if err := createBandwidthUsageCollection(app); err != nil {
		return err
	}
	// Backfill existing users with default quota values
	if err := backfillExistingUsers(app); err != nil {
		return err
	}
	return nil
}

// addQuotaFieldsToUsers adds quota-related fields to the users collection.
func addQuotaFieldsToUsers(app core.App) error {
	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return fmt.Errorf("users collection not found: %w", err)
	}

	changed := false

	// relay_quota_gb: Maximum relay quota in GB for the user's tier
	if usersCol.Fields.GetByName("relay_quota_gb") == nil {
		usersCol.Fields.Add(&core.NumberField{
			Name:     "relay_quota_gb",
			Required: false,
		})
		changed = true
	}

	// current_period_usage_gb: Current period's bandwidth usage in GB
	if usersCol.Fields.GetByName("current_period_usage_gb") == nil {
		usersCol.Fields.Add(&core.NumberField{
			Name:     "current_period_usage_gb",
			Required: false,
		})
		changed = true
	}

	// quota_period_start: Start date of the current quota period
	if usersCol.Fields.GetByName("quota_period_start") == nil {
		usersCol.Fields.Add(&core.DateField{
			Name:     "quota_period_start",
			Required: false,
		})
		changed = true
	}

	// quota_period_end: End date of the current quota period
	if usersCol.Fields.GetByName("quota_period_end") == nil {
		usersCol.Fields.Add(&core.DateField{
			Name:     "quota_period_end",
			Required: false,
		})
		changed = true
	}

	// turn_baseline_bytes: Baseline TURN traffic at quota reset for net calculation
	if usersCol.Fields.GetByName("turn_baseline_bytes") == nil {
		usersCol.Fields.Add(&core.NumberField{
			Name:     "turn_baseline_bytes",
			Required: false,
		})
		changed = true
	}

	if !changed {
		return nil
	}

	return app.Save(usersCol)
}

// createBandwidthUsageCollection creates the bandwidth_usage audit collection.
func createBandwidthUsageCollection(app core.App) error {
	// Check if collection already exists (idempotent)
	if _, err := app.FindCollectionByNameOrId("bandwidth_usage"); err == nil {
		return nil
	}

	// Find the users collection to get its ID for the relation
	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return fmt.Errorf("users collection not found: %w", err)
	}

	// Create bandwidth_usage collection
	bwCol := core.NewBaseCollection("bandwidth_usage")
	bwCol.Fields.Add(
		&core.RelationField{
			Name:          "account_id",
			CollectionId:  usersCol.Id,
			Required:      true,
			CascadeDelete: true,
			MaxSelect:     1,
		},
		&core.DateField{
			Name:     "period_start",
			Required: true,
		},
		&core.DateField{
			Name:     "period_end",
			Required: true,
		},
		&core.NumberField{
			Name:     "bytes_transferred",
			Required: true,
		},
		&core.AutodateField{
			Name:     "created",
			System:   true,
			OnCreate: true,
		},
	)

	// bandwidth_usage is internal audit data - no API access
	bwCol.ListRule = nil
	bwCol.ViewRule = nil
	bwCol.CreateRule = nil
	bwCol.UpdateRule = nil
	bwCol.DeleteRule = nil

	if err := app.Save(bwCol); err != nil {
		return fmt.Errorf("failed to save bandwidth_usage collection: %w", err)
	}

	return nil
}

// backfillExistingUsers sets default quota values for existing users
// who don't have quota fields set (upgrades from 10a).
func backfillExistingUsers(app core.App) error {
	// Find users without relay_quota_gb set
	records, err := app.FindRecordsByFilter(
		"users",
		"relay_quota_gb = null || relay_quota_gb = 0",
		"",
		500,
		0,
	)
	if err != nil {
		return fmt.Errorf("failed to fetch users for backfill: %w", err)
	}

	now := time.Now().UTC()
	periodEnd := now.Add(30 * 24 * time.Hour)

	for _, record := range records {
		// Only backfill if relay_quota_gb is not set or is 0
		if record.GetFloat("relay_quota_gb") == 0 {
			record.Set("relay_quota_gb", 50.0) // Default 50GB
			record.Set("current_period_usage_gb", 0.0)
			record.Set("quota_period_start", now)
			record.Set("quota_period_end", periodEnd)
			record.Set("turn_baseline_bytes", 0.0)

			if err := app.Save(record); err != nil {
				return fmt.Errorf("failed to backfill user %s: %w", record.Id, err)
			}
		}
	}

	return nil
}

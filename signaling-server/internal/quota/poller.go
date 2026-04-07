package quota

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/metrics"
)

// Poller periodically fetches bandwidth usage from Prometheus and updates PocketBase.
type Poller struct {
	app            core.App
	metricsClient  *metrics.PrometheusClient
	checkInterval  time.Duration
	defaultQuotaGB float64
	stopChan       chan struct{}
	started        atomic.Bool
}

// NewPoller creates a new quota poller.
func NewPoller(app core.App, metricsClient *metrics.PrometheusClient, cfg *config.Config) *Poller {
	return &Poller{
		app:            app,
		metricsClient:  metricsClient,
		checkInterval:  cfg.QuotaCheckInterval,
		defaultQuotaGB: cfg.DefaultQuotaGB,
	}
}

// Start begins the polling loop in a background goroutine.
func (p *Poller) Start() {
	if p.started.Load() {
		return
	}
	p.stopChan = make(chan struct{})
	p.started.Store(true)

	go p.run()
}

// Stop halts the polling loop.
func (p *Poller) Stop() {
	if !p.started.Load() {
		return
	}
	close(p.stopChan)
	p.started.Store(false)
}

// run executes the main polling loop.
func (p *Poller) run() {
	ticker := time.NewTicker(p.checkInterval)
	defer ticker.Stop()

	// Run immediately on start
	if err := p.poll(); err != nil {
		log.Printf("quota poller initial poll error: %v", err)
	}

	for {
		select {
		case <-ticker.C:
			if err := p.poll(); err != nil {
				log.Printf("quota poller poll error: %v", err)
			}
		case <-p.stopChan:
			return
		}
	}
}

// poll fetches bandwidth metrics for all accounts and updates their quota usage.
func (p *Poller) poll() error {
	// Fetch all users with quota fields using pagination
	offset := 0
	const batchSize = 500

	for {
		records, err := p.app.FindRecordsByFilter(
			"users",
			"relay_quota_gb != null",
			"",
			batchSize,
			offset,
		)
		if err != nil {
			return fmt.Errorf("failed to fetch users: %w", err)
		}

		now := time.Now().UTC()

		for _, record := range records {
			if err := p.updateAccountQuota(record, now); err != nil {
				log.Printf("quota poller: failed to update account %s: %v", record.Id, err)
				// Continue with other accounts
			}
		}

		// If we got fewer records than batch size, we've processed all users
		if len(records) < batchSize {
			break
		}

		offset += batchSize
	}

	return nil
}

// updateAccountQuota updates the quota usage for a single account.
func (p *Poller) updateAccountQuota(record *core.Record, now time.Time) error {
	accountID := record.Id

	// Get current period dates
	periodEnd := record.GetDateTime("quota_period_end")

	// Check if period has ended and needs reset
	if !periodEnd.IsZero() && now.After(periodEnd.Time()) {
		if err := p.resetQuotaPeriod(record, now); err != nil {
			return fmt.Errorf("failed to reset quota period: %w", err)
		}
		// After reset, fetch the record again to get updated baseline
		var err error
		record, err = p.app.FindRecordById("users", accountID)
		if err != nil {
			return fmt.Errorf("failed to fetch updated record after reset: %w", err)
		}
	}

	// Query Prometheus for total TURN traffic
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	totalBytes, err := p.metricsClient.QueryAccountBytes(ctx, accountID)
	if err != nil {
		return fmt.Errorf("failed to query prometheus: %w", err)
	}

	// Calculate net usage (current total minus baseline at period start)
	// When Coturn restarts, Prometheus counters reset to 0, so totalBytes
	// may be less than baselineBytes. We use the "virtual negative baseline"
	// approach: update baseline to current total (resetting net to 0) so that
	// quota consumption before the restart is preserved.
	baselineBytes := record.GetFloat("turn_baseline_bytes")
	netBytes := totalBytes - int64(baselineBytes)
	if netBytes < 0 {
		// Coturn restart detected - update baseline to current total
		netBytes = 0
		record.Set("turn_baseline_bytes", float64(totalBytes))
	}

	// Convert to GB
	netGB := float64(netBytes) / (1024 * 1024 * 1024)

	// Update current period usage
	record.Set("current_period_usage_gb", netGB)

	if err := p.app.Save(record); err != nil {
		return fmt.Errorf("failed to save record: %w", err)
	}

	return nil
}

// resetQuotaPeriod archives current usage and starts a new quota period.
func (p *Poller) resetQuotaPeriod(record *core.Record, now time.Time) error {
	// Get current period data
	periodStart := record.GetDateTime("quota_period_start")
	periodEnd := record.GetDateTime("quota_period_end")
	currentUsage := record.GetFloat("current_period_usage_gb")

	// Only archive if we have valid period data
	if !periodStart.IsZero() && !periodEnd.IsZero() {
		// Convert GB to bytes for archival
		bytesTransferred := int64(currentUsage * 1024 * 1024 * 1024)

		// Create bandwidth_usage record
		bwCol, err := p.app.FindCollectionByNameOrId("bandwidth_usage")
		if err != nil {
			return fmt.Errorf("bandwidth_usage collection not found: %w", err)
		}

		bwRecord := core.NewRecord(bwCol)
		bwRecord.Set("account_id", record.Id)
		bwRecord.Set("period_start", periodStart.Time())
		bwRecord.Set("period_end", periodEnd.Time())
		bwRecord.Set("bytes_transferred", bytesTransferred)

		if err := p.app.Save(bwRecord); err != nil {
			return fmt.Errorf("failed to create bandwidth_usage record: %w", err)
		}
	}

	// Query current TURN traffic to set new baseline
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	newBaselineBytes, err := p.metricsClient.QueryAccountBytes(ctx, record.Id)
	if err != nil {
		log.Printf("quota poller: failed to query baseline for account %s: %v", record.Id, err)
		// Continue with zero baseline if query fails
		newBaselineBytes = 0
	}

	// Reset quota fields
	record.Set("turn_baseline_bytes", float64(newBaselineBytes))
	record.Set("current_period_usage_gb", 0.0)
	record.Set("quota_period_start", now)
	record.Set("quota_period_end", now.Add(30*24*time.Hour))

	if err := p.app.Save(record); err != nil {
		return fmt.Errorf("failed to save record with reset quota: %w", err)
	}

	return nil
}

package controlsync

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// DefaultSyncInterval is the gateway's route/control reconcile cadence. It is
// well inside the 120-second §14 route lease and the 45-second presence lease,
// so a single missed pass never expires live state.
const DefaultSyncInterval = 5 * time.Second

// HealthSink is the truthful §17.1 health surface the sync loop drives. It is
// the production write path for `gateway.Health`'s snapshot/control-sync
// truths: route readiness becomes true only after a snapshot has actually
// been applied AND control sync is healthy, and control sync is healthy only
// while the revision control last ACKNOWLEDGED covers the revision the
// gateway has applied (control's own lease watermark — see
// Applier.ControlSyncHealthy). Snapshot readiness NEVER regresses (a later gap
// must not withdraw routing for already-served routes — §15.4).
type HealthSink interface {
	SetSnapshotReady(ready bool)
	SetControlSynced(synced bool)
}

// LoopConfig is the fail-closed construction contract for the sync loop.
type LoopConfig struct {
	// Applier is the Task 13 snapshot/delta applier; required.
	Applier *Applier
	// Interval is the reconcile cadence; <= 0 selects DefaultSyncInterval.
	Interval time.Duration
	// Health receives the §17.1 readiness truths. Optional; a nil sink
	// records nothing.
	Health HealthSink
	// Logger receives bounded diagnostics. Optional; defaults to slog.Default.
	Logger *slog.Logger
}

// Loop is the production driver that keeps the gateway's route state in step
// with control and reports the resulting health truths. It is a thin,
// single-goroutine wrapper over Applier.Reconcile: it adds no policy of its
// own, so the R2 epoch authority and §15.4 recovery behaviour remain entirely
// the applier's.
type Loop struct {
	applier  *Applier
	interval time.Duration
	health   HealthSink
	logger   *slog.Logger
}

// NewLoop validates the configuration and returns the loop.
func NewLoop(config LoopConfig) (*Loop, error) {
	if config.Applier == nil {
		return nil, errors.New("controlsync: loop requires an applier")
	}
	if config.Interval < 0 {
		return nil, errors.New("controlsync: loop interval must not be negative")
	}
	interval := config.Interval
	if interval == 0 {
		interval = DefaultSyncInterval
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Loop{
		applier:  config.Applier,
		interval: interval,
		health:   config.Health,
		logger:   logger,
	}, nil
}

// ReconcileOnce performs one reconcile pass and reports the resulting health
// truths. Snapshot readiness is set from the applier's real state on every
// pass (true after the first applied snapshot) — it is independent of the
// acknowledgement outcome, because an apply that committed really did make
// the gateway snapshot-ready. Control-sync health mirrors control's own lease
// predicate (Applier.ControlSyncHealthy: the last ACKNOWLEDGED revision must
// cover the applied revision) whenever the pass actually reached control — a
// full success, or a successful fetch+apply whose only failure was the
// /status acknowledgement (ErrAckFailed). Any other failure means the gateway
// could not observe control's current revision, so the truth is withdrawn
// rather than asserted. The error is returned for the caller's own retry/log
// policy.
func (loop *Loop) ReconcileOnce(ctx context.Context) error {
	err := loop.applier.Reconcile(ctx)
	if loop.health != nil {
		loop.health.SetSnapshotReady(loop.applier.Ready())
		// An ack failure leaves the watermark exactly where control's own
		// predicate leaves it: a rejected ack for a no-op poll changes
		// nothing control has to renew (still healthy), while a rejected ack
		// after a NEW revision was applied leaves it uncovered (unhealthy)
		// until an ack catches up.
		if err == nil || errors.Is(err, ErrAckFailed) {
			loop.health.SetControlSynced(loop.applier.ControlSyncHealthy())
		} else {
			loop.health.SetControlSynced(false)
		}
	}
	return err
}

// Run reconciles immediately and then on the configured interval until ctx is
// cancelled. Reconcile failures are logged and retried on the next tick; the
// loop never exits on a transport error, because control sync recovery is the
// applier's job (a gap or a foreign epoch always recovers through a fresh
// snapshot).
func (loop *Loop) Run(ctx context.Context) {
	if err := loop.ReconcileOnce(ctx); err != nil {
		loop.logger.Warn("controlsync: initial reconcile failed", "error", err)
	}
	ticker := time.NewTicker(loop.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := loop.ReconcileOnce(ctx); err != nil {
				loop.logger.Warn("controlsync: reconcile failed", "error", err)
			}
		}
	}
}

// Presence transport (task #16, ledger I4-partial): the gateway-side loop that
// carries the §7.3 authoritative tunnel presence to control over the §11.3
// mTLS sync channel.
//
// Before this existed, control's presence view was never fed in production: the
// gateway's registry emitted presence transitions to the metrics sink only, so
// control's leases were always empty and every relay route failed closed. Two
// facts make a periodic FULL-STATE republish the correct transport:
//
//   - a presence lease is renewed by an authenticated Ping every 10 s but the
//     registry only EMITS a transition when online/offline changes, so a
//     healthy tunnel's renewal is invisible to control unless the gateway
//     re-states its current state; and
//   - control's stored lease expiry is the gateway-stated expiry (bounded by
//     receipt + 60 s), so a lease that is never re-stated expires even while
//     the tunnel keeps pinging.
//
// The loop is deliberately ONE goroutine with no payload buffering: a
// transition, a tick and a manual Notify all collapse into "read the current
// authoritative state and post it". Because a snapshot is a wholesale
// replacement, a dropped/failed publish needs no replay machinery — the next
// tick (or the next transition's Notify) republishes the complete state, and a
// lost POST can never leave control holding a superseded partial view.
package controlsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DefaultPresenceRepublishInterval is the full-state presence republish
// cadence. It is deliberately 15 s: one third of the gateway registry's 45 s
// lease (presence.DefaultLeaseTTL), so a republished lease is never more than
// ~15 s stale and even two consecutive missed republishes leave the previously
// stated lease unexpired. It is also four times inside control's 60 s
// receipt-bounded lease cap, so the republish — not control's cap — is what
// keeps the entry current.
const DefaultPresenceRepublishInterval = 15 * time.Second

// PresenceSnapshotEntry is one currently-online tunnel in the gateway's
// authoritative presence state (mirrors presence.SnapshotEntry; converted at
// the cmd/gateway seam so this package never depends on the presence package).
type PresenceSnapshotEntry struct {
	AgentRecordID  string
	RelayPort      int
	Generation     uint64
	LeaseExpiresAt time.Time
}

// PresenceSource supplies the gateway's current authoritative presence state
// for republish. The production implementation wraps presence.Registry.Snapshot.
type PresenceSource interface {
	// PresenceSnapshot returns the reporting boot identity, the current
	// presence revision, and one entry per online tunnel with its CURRENT
	// lease expiry. A source that cannot produce a boot identity reports "".
	PresenceSnapshot() (bootID string, revision uint64, entries []PresenceSnapshotEntry)
}

// PresencePublisherConfig is the construction contract for the publisher.
type PresencePublisherConfig struct {
	// Client is the §11.3 sync client; required.
	Client *Client
	// Source supplies the authoritative state; required.
	Source PresenceSource
	// Interval overrides the republish cadence. Zero selects
	// DefaultPresenceRepublishInterval; negative is invalid.
	Interval time.Duration
	// Logger receives bounded diagnostics (never credential material). nil
	// selects slog.Default.
	Logger *slog.Logger
}

// PresencePublisher republishes the gateway's full presence state to control on
// a bounded cadence and whenever the registry signals a transition. It is safe
// for concurrent Notify calls and owns exactly one loop goroutine (Run).
type PresencePublisher struct {
	client   *Client
	source   PresenceSource
	interval time.Duration
	logger   *slog.Logger
	// wake is a size-1 coalescing signal: any number of transitions between
	// iterations collapse into one republish, so a transition storm can never
	// grow a queue or spawn work.
	wake chan struct{}
}

// NewPresencePublisher validates the configuration and returns the publisher.
func NewPresencePublisher(config PresencePublisherConfig) (*PresencePublisher, error) {
	if config.Client == nil {
		return nil, errors.New("controlsync: presence publisher requires a sync client")
	}
	if config.Source == nil {
		return nil, errors.New("controlsync: presence publisher requires a presence source")
	}
	if config.Interval < 0 {
		return nil, errors.New("controlsync: presence republish interval must not be negative")
	}
	interval := config.Interval
	if interval == 0 {
		interval = DefaultPresenceRepublishInterval
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &PresencePublisher{
		client:   config.Client,
		source:   config.Source,
		interval: interval,
		logger:   logger,
		wake:     make(chan struct{}, 1),
	}, nil
}

// Notify requests an immediate republish. It is non-blocking by contract: the
// presence registry calls it while holding its state lock (and the gateway
// stream-admission path holds that lock in turn), so it may only signal a
// coalescing channel and return. A signal already pending makes this a no-op.
func (publisher *PresencePublisher) Notify() {
	if publisher == nil {
		return
	}
	select {
	case publisher.wake <- struct{}{}:
	default:
	}
}

// PublishOnce reads the current authoritative state and posts one full presence
// snapshot. A source that reports no boot identity yields an error (a snapshot
// without a boot is not adoptable and is never emitted). The payload is a full
// replacement, so a caller may retry it freely.
func (publisher *PresencePublisher) PublishOnce(ctx context.Context) error {
	bootID, revision, entries := publisher.source.PresenceSnapshot()
	if bootID == "" {
		return errors.New("controlsync: presence source produced no gateway boot identity")
	}
	events := make([]PresenceEvent, 0, len(entries))
	for _, entry := range entries {
		events = append(events, PresenceEvent{
			GatewayBootID:  bootID,
			Revision:       revision,
			AgentRecordID:  entry.AgentRecordID,
			RelayPort:      entry.RelayPort,
			Generation:     entry.Generation,
			State:          PresenceStateOnline,
			LeaseExpiresAt: entry.LeaseExpiresAt.UTC().Format(time.RFC3339),
		})
	}
	// Always a non-nil slice so the wire carries "events":[] rather than null.
	envelope := PresenceEnvelope{
		Version:       ProtocolVersion,
		GatewayBootID: bootID,
		Revision:      revision,
		Events:        events,
	}
	if err := publisher.client.PublishPresenceSnapshot(ctx, envelope); err != nil {
		return fmt.Errorf("controlsync: presence republish: %w", err)
	}
	return nil
}

// Run publishes immediately — the boot snapshot, empty or not, is what makes a
// restarted gateway adoptable at control — then republishes on the configured
// interval and whenever Notify signals a transition. Failures are logged and
// retried by the next tick; the loop never exits on a transport error, because
// a full snapshot is idempotent and self-healing. Exactly one goroutine runs
// this loop for the process lifetime.
func (publisher *PresencePublisher) Run(ctx context.Context) {
	if err := publisher.PublishOnce(ctx); err != nil {
		publisher.logger.Warn("controlsync: initial presence republish failed", "error", err)
	}
	ticker := time.NewTicker(publisher.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-publisher.wake:
			if err := publisher.PublishOnce(ctx); err != nil {
				publisher.logger.Warn("controlsync: presence republish failed", "error", err)
			}
		case <-ticker.C:
			if err := publisher.PublishOnce(ctx); err != nil {
				publisher.logger.Warn("controlsync: presence republish failed", "error", err)
			}
		}
	}
}

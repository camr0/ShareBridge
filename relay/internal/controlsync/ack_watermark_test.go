package controlsync

// M5 remediation round 2, Fix A1: control-sync health must mirror control's
// own lease predicate, which is a revision WATERMARK and not a boolean.
//
// control/internal/relayctl/publisher.go Healthy() is `hasAck &&
// ackedRevision >= revision`: control renews a gateway's route leases while
// the revision that gateway last acknowledged covers the revision control has
// published. A rejected acknowledgement is therefore only unhealthy when it
// leaves a NEW revision uncovered. The previous round made EVERY rejected ack
// withdraw route readiness, which is strictly more pessimistic than control:
// after a current ack, a rejected ack for a no-op poll changes nothing control
// has to renew, yet the gateway flipped route_ready=false and diverged from
// the lease truth.
//
// These tests drive the real sync loop over the real Task 11 client and a
// scripted control, and observe the §17.1 truth through gateway.Health — the
// same surface operators read at /healthz.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/routes"
)

// ackWatermarkLoop builds the production sync loop over a scripted control and
// a real gateway health surface.
func ackWatermarkLoop(t *testing.T, certs syncTestCertificates) (*scriptedControl, *Applier, *gateway.Health, *Loop) {
	t.Helper()
	script, client := newScriptedControl(t, certs)
	table := routes.NewTable(newReconcilePresence())
	applier := mustApplier(t, client, table, nil, time.Now)
	health := gateway.NewHealth()
	loop, err := NewLoop(LoopConfig{
		Applier:  applier,
		Interval: time.Hour,
		Health:   health,
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	return script, applier, health, loop
}

// ackWatermarkSnapshot is a control snapshot at the given revision.
func ackWatermarkSnapshot(revision uint64) Snapshot {
	return Snapshot{
		Version:  ProtocolVersion,
		Epoch:    7,
		Revision: revision,
		Routes:   []Route{wireRoute(reconcileHostnameA, revision, reconcileAgentA, reconcilePort)},
	}
}

// ackWatermarkNoopDelta is the up-to-date poll control answers when it has
// published nothing new: since == latest_revision == revision.
func ackWatermarkNoopDelta(revision uint64) DeltaPage {
	return DeltaPage{
		Version:        ProtocolVersion,
		Epoch:          7,
		Status:         DeltaStatusOK,
		Since:          revision,
		LatestRevision: revision,
	}
}

// TestNoOpAckFailureKeepsControlSyncHealthy pins the A1 behaviour: after a
// CURRENT ack, a rejected ack for a no-op poll must leave /healthz
// route_ready=true. Control's lease predicate is a watermark — its
// ackedRevision still covers its published revision — so control keeps
// renewing the lease and the gateway must not report a divergence that does
// not exist.
func TestNoOpAckFailureKeepsControlSyncHealthy(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, _, health, loop := ackWatermarkLoop(t, certs)

	// Control publishes revision 40; the gateway applies it and acks.
	script.setSnapshot(t, ackWatermarkSnapshot(40))
	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("initial ReconcileOnce: %v", err)
	}
	if !health.RouteReady() {
		t.Fatal("precondition: a current ack must leave route_ready=true")
	}

	// Control publishes nothing new. The gateway's no-op poll is applied and
	// its /status ack is rejected.
	script.setDelta(t, ackWatermarkNoopDelta(40))
	script.failStatus(true)
	if err := loop.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("a rejected /status acknowledgement must surface as a reconcile failure")
	}
	if !health.RouteReady() {
		t.Fatal("a rejected ack for a NO-OP poll must keep route_ready=true: the current ack still covers control's published revision, so control is still renewing the lease (control's predicate is hasAck && ackedRevision >= revision)")
	}
}

// TestNewRevisionAckFailureWithdrawsControlSync pins the other half of the
// watermark: once a NEW revision has been applied, the old ack no longer
// covers it, so route_ready must be withdrawn until an ack catches up. This
// is the direction that must never silently read healthy (the lease really is
// about to stop being refreshed).
func TestNewRevisionAckFailureWithdrawsControlSync(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, _, health, loop := ackWatermarkLoop(t, certs)

	script.setSnapshot(t, ackWatermarkSnapshot(40))
	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("initial ReconcileOnce: %v", err)
	}
	if !health.RouteReady() {
		t.Fatal("precondition: a current ack must leave route_ready=true")
	}

	// Control publishes revision 41; the gateway applies it but its ack is
	// rejected, so the acked watermark (40) no longer covers the applied
	// revision (41).
	script.setDelta(t, DeltaPage{
		Version:        ProtocolVersion,
		Epoch:          7,
		Status:         DeltaStatusOK,
		Since:          40,
		LatestRevision: 41,
		Deltas: []RouteDelta{{
			Revision:  41,
			Operation: RouteOperationAdd,
			Route:     wireRoute(reconcileHostnameB, 41, reconcileAgentB, reconcilePort),
		}},
	})
	script.failStatus(true)
	if err := loop.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("a rejected /status acknowledgement for a new revision must surface as a reconcile failure")
	}
	if health.RouteReady() {
		t.Fatal("a rejected ack after a NEW revision was applied must withdraw route_ready until an ack catches up")
	}
}

// TestAckWatermarkRecoveryRestoresControlSync pins that the watermark is not
// latched: a subsequent accepted ack of the current revision restores
// route_ready, exactly as control's own non-latching predicate does.
func TestAckWatermarkRecoveryRestoresControlSync(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, _, health, loop := ackWatermarkLoop(t, certs)

	script.setSnapshot(t, ackWatermarkSnapshot(40))
	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("initial ReconcileOnce: %v", err)
	}

	script.setDelta(t, DeltaPage{
		Version:        ProtocolVersion,
		Epoch:          7,
		Status:         DeltaStatusOK,
		Since:          40,
		LatestRevision: 41,
		Deltas: []RouteDelta{{
			Revision:  41,
			Operation: RouteOperationAdd,
			Route:     wireRoute(reconcileHostnameB, 41, reconcileAgentB, reconcilePort),
		}},
	})
	script.failStatus(true)
	if err := loop.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("a rejected /status acknowledgement must surface as a reconcile failure")
	}
	if health.RouteReady() {
		t.Fatal("precondition: a rejected ack for a new revision must withdraw route_ready")
	}

	// The same current revision, now acknowledged: control's watermark moves
	// to 41 and route readiness is restored.
	script.setDelta(t, ackWatermarkNoopDelta(41))
	script.failStatus(false)
	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce after /status recovers: %v", err)
	}
	if !health.RouteReady() {
		t.Fatal("an accepted ack of the current revision must restore route_ready")
	}
}

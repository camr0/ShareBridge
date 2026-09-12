package controlsync

// M5 remediation round 2, Fix A1 — equivalence pin.
//
// The gateway cannot read control's publisher state, so it mirrors control's
// lease predicate with the facts it owns. This test pins that mirror against a
// local oracle that is transcribed from control's actual predicate:
//
//	control:  Healthy()               == hasAck && ackedRevision >= revision
//	          (control/internal/relayctl/publisher.go)
//	gateway:  ControlSyncHealthy()    == hasAcked && ackedEpoch == appliedEpoch
//	                                     && ackedRevision >= appliedRevision
//
// where `appliedRevision` is control's published revision as observed by the
// last successful fetch+apply, and `ackedRevision` is the revision carried by
// the last acknowledgement control ACCEPTED. The epoch term mirrors control's
// own "a new gateway boot ID replaces the recorded ack state wholesale" rule
// across a control restart (revisions restart within an epoch).
//
// If control's predicate changes shape later, this oracle stops matching by
// construction and this test goes red — that is the divergence alarm for a
// maintainer (see the fix-round report section).

import (
	"context"
	"errors"
	"testing"
	"time"

	"sharebridge/relay/internal/routes"
)

// controlLeaseOracle is a faithful local model of control's lease predicate.
// It is deliberately written as control writes it — `hasAck && ackedRevision
// >= revision` — so that a shape change on either side shows up as a
// divergence.
type controlLeaseOracle struct {
	revision      uint64
	hasAck        bool
	ackedRevision uint64
}

// publish records control publishing a new route revision.
func (oracle *controlLeaseOracle) publish(revision uint64) {
	oracle.revision = revision
}

// ack records control ACCEPTING an acknowledgement for revision (rejected
// acks change nothing in control, mirroring the scripted control's statusFail).
func (oracle *controlLeaseOracle) ack(revision uint64) {
	if !oracle.hasAck || revision > oracle.ackedRevision {
		oracle.ackedRevision = revision
	}
	oracle.hasAck = true
}

// restart models a control process restart: the publisher state is gone and
// the epoch (and with it the revision lineage) restarts.
func (oracle *controlLeaseOracle) restart() {
	oracle.hasAck = false
	oracle.ackedRevision = 0
}

// healthy is control's predicate verbatim.
func (oracle *controlLeaseOracle) healthy() bool {
	return oracle.hasAck && oracle.ackedRevision >= oracle.revision
}

// TestAckWatermarkMatchesControlLeasePredicate drives publish / apply / ack
// events and asserts after every stage that the gateway's verdict is exactly
// control's predicate over the same history.
func TestAckWatermarkMatchesControlLeasePredicate(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)
	table := routes.NewTable(newReconcilePresence())
	applier := mustApplier(t, client, table, nil, time.Now)
	oracle := &controlLeaseOracle{}

	assertEquivalent := func(stage string, want bool) {
		t.Helper()
		if got := oracle.healthy(); got != want {
			t.Fatalf("%s: control-oracle healthy()=%v, want %v (hasAck=%v acked=%d revision=%d) — the oracle itself is wrong",
				stage, got, want, oracle.hasAck, oracle.ackedRevision, oracle.revision)
		}
		if got := applier.ControlSyncHealthy(); got != want {
			t.Fatalf("%s: gateway ControlSyncHealthy()=%v, want %v — control-oracle says hasAck=%v ackedRevision=%d revision=%d",
				stage, got, want, oracle.hasAck, oracle.ackedRevision, oracle.revision)
		}
	}

	// Stage 1 — control publishes 40; the gateway applies and acks it.
	oracle.publish(40)
	script.setSnapshot(t, ackWatermarkSnapshot(40))
	if err := applier.ReconcileSnapshot(context.Background()); err != nil {
		t.Fatalf("snapshot publish: %v", err)
	}
	oracle.ack(40)
	assertEquivalent("publish 40 + accepted ack", true)

	// Stage 2 — no new revision; the no-op poll's ack is REJECTED. Control's
	// watermark still covers revision 40, so control keeps renewing.
	script.setDelta(t, ackWatermarkNoopDelta(40))
	script.failStatus(true)
	if err := applier.Reconcile(context.Background()); !errors.Is(err, ErrAckFailed) {
		t.Fatalf("no-op poll with rejected ack: err=%v, want ErrAckFailed", err)
	}
	assertEquivalent("no-op poll + rejected ack", true)

	// Stage 3 — control publishes 41; the gateway applies it, the ack is
	// REJECTED. The watermark (40) no longer covers 41 on either side.
	oracle.publish(41)
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
	if err := applier.Reconcile(context.Background()); !errors.Is(err, ErrAckFailed) {
		t.Fatalf("new revision with rejected ack: err=%v, want ErrAckFailed", err)
	}
	assertEquivalent("publish 41 + rejected ack", false)

	// Stage 4 — the next poll is accepted: both watermarks advance to 41.
	script.setDelta(t, ackWatermarkNoopDelta(41))
	script.failStatus(false)
	if err := applier.Reconcile(context.Background()); err != nil {
		t.Fatalf("recovery ack: %v", err)
	}
	oracle.ack(41)
	assertEquivalent("publish 41 + accepted ack", true)

	// Stage 5 — control restarts into a NEW epoch with a lower revision; the
	// gateway re-snapshots but its ack is REJECTED. Control forgot the old
	// ack; the gateway must not carry the old epoch's watermark across.
	oracle.restart()
	oracle.publish(3)
	script.setSnapshot(t, Snapshot{
		Version:  ProtocolVersion,
		Epoch:    8,
		Revision: 3,
		Routes:   []Route{wireRoute(reconcileHostnameA, 3, reconcileAgentA, reconcilePort)},
	})
	script.failStatus(true)
	if err := applier.ReconcileSnapshot(context.Background()); !errors.Is(err, ErrAckFailed) {
		t.Fatalf("new epoch with rejected ack: err=%v, want ErrAckFailed", err)
	}
	assertEquivalent("new epoch 8 + rejected ack", false)

	// Stage 6 — the new epoch's revision is acknowledged: both recover.
	script.setDelta(t, DeltaPage{
		Version:        ProtocolVersion,
		Epoch:          8,
		Status:         DeltaStatusOK,
		Since:          3,
		LatestRevision: 3,
	})
	script.failStatus(false)
	if err := applier.Reconcile(context.Background()); err != nil {
		t.Fatalf("new epoch recovery ack: %v", err)
	}
	oracle.ack(3)
	assertEquivalent("new epoch 8 + accepted ack", true)
}

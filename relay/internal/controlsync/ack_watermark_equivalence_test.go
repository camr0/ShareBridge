package controlsync

// M5 remediation round 2, Fix A1 + A2 — equivalence pin.
//
// The gateway cannot read control's publisher state, so it mirrors control's
// lease predicate with the facts it owns. This test pins that mirror against a
// local oracle that is transcribed from control's actual predicate:
//
//	control:  Healthy()               == hasAck && ackedRevision >= revision
//	gateway:  ControlSyncHealthy()    == hasAcked && ackedEpoch == appliedEpoch
//	                                     && ackedRevision >= appliedRevision
//
// where `appliedRevision` is control's published revision as observed by the
// last successful fetch+apply, and `ackedRevision` is the revision carried by
// the last acknowledgement control ACCEPTED. The epoch term mirrors control's
// own "a new gateway boot ID replaces the recorded ack state wholesale" rule
// across a control restart (revisions restart within an epoch).
//
// "Accepted" is the crux (A2). Control's publisher refuses an ack carrying a
// control epoch other than its own — it records nothing and reports the
// explicit negative `acknowledged:false, reason:"foreign_epoch"`. The oracle
// models that acceptance decision (`ack` returns false and mutates nothing on
// an epoch mismatch), and the scripted control answers the same way. So the
// pin covers the restart boundary: after control restarts to a new epoch, an
// in-flight ack for the superseded epoch must leave BOTH sides agreeing that
// no ack exists (unhealthy), and the gateway must not keep claiming the
// superseded epoch's watermark.
//
// If control's predicate or acceptance shape changes later, this oracle stops
// matching by construction and this test goes red — that is the divergence
// alarm for a maintainer (see the fix-round report section).

import (
	"context"
	"errors"
	"testing"
	"time"

	"sharebridge/relay/internal/routes"
)

// controlLeaseOracle is a faithful local model of control's lease predicate
// AND its acknowledgement-acceptance rule. It is deliberately written as
// control writes it — `hasAck && ackedRevision >= revision`, with a foreign
// control epoch refused wholesale — so that a shape change on either side
// shows up as a divergence.
type controlLeaseOracle struct {
	epoch         uint64
	revision      uint64
	hasAck        bool
	ackedEpoch    uint64
	ackedRevision uint64
}

// publish records control publishing a new route revision.
func (oracle *controlLeaseOracle) publish(revision uint64) {
	oracle.revision = revision
}

// ack records an acknowledgement ARRIVING at control and reports whether
// control accepted it. Control refuses any ack carrying an epoch other than
// its own — it records nothing and reports acknowledged:false /
// foreign_epoch — exactly as relayctl.Publisher.Acknowledge does.
func (oracle *controlLeaseOracle) ack(epoch uint64, revision uint64) bool {
	if epoch != oracle.epoch {
		return false
	}
	if !oracle.hasAck || oracle.ackedEpoch != epoch {
		oracle.hasAck = true
		oracle.ackedEpoch = epoch
		oracle.ackedRevision = revision
		return true
	}
	if revision > oracle.ackedRevision {
		oracle.ackedRevision = revision
	}
	return true
}

// restart models a control process restart into a new epoch: the publisher's
// acknowledgement state is gone and the epoch changes. The following publish
// call sets the new epoch's revision.
func (oracle *controlLeaseOracle) restart(epoch uint64) {
	oracle.epoch = epoch
	oracle.hasAck = false
	oracle.ackedEpoch = 0
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
	oracle := &controlLeaseOracle{epoch: 7}
	script.setStatusEpoch(7)

	assertEquivalent := func(stage string, want bool) {
		t.Helper()
		if got := oracle.healthy(); got != want {
			t.Fatalf("%s: control-oracle healthy()=%v, want %v (hasAck=%v ackedEpoch=%d acked=%d revision=%d epoch=%d) — the oracle itself is wrong",
				stage, got, want, oracle.hasAck, oracle.ackedEpoch, oracle.ackedRevision, oracle.revision, oracle.epoch)
		}
		if got := applier.ControlSyncHealthy(); got != want {
			t.Fatalf("%s: gateway ControlSyncHealthy()=%v, want %v — control-oracle says hasAck=%v ackedEpoch=%d ackedRevision=%d revision=%d epoch=%d",
				stage, got, want, oracle.hasAck, oracle.ackedEpoch, oracle.ackedRevision, oracle.revision, oracle.epoch)
		}
	}

	// Stage 1 — control publishes 40; the gateway applies and acks it.
	oracle.publish(40)
	script.setSnapshot(t, ackWatermarkSnapshot(40))
	if err := applier.ReconcileSnapshot(context.Background()); err != nil {
		t.Fatalf("snapshot publish: %v", err)
	}
	if accepted := oracle.ack(7, 40); !accepted {
		t.Fatal("control must accept an ack for its own epoch")
	}
	assertEquivalent("publish 40 + accepted ack", true)

	// Stage 2 — no new revision; the no-op poll's ack is REJECTED at the
	// transport (503). Control's watermark still covers revision 40, so
	// control keeps renewing and the gateway must keep agreeing.
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
	if accepted := oracle.ack(7, 41); !accepted {
		t.Fatal("control must accept an ack for its own epoch")
	}
	assertEquivalent("publish 41 + accepted ack", true)

	// Stage 5 — control restarts into a NEW epoch with a lower revision; the
	// gateway re-snapshots but its ack fails at the transport. Control forgot
	// the old ack; the gateway must not carry the old epoch's watermark
	// across.
	oracle.restart(8)
	oracle.publish(3)
	script.setStatusEpoch(8)
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
	script.setDelta(t, ackWatermarkNoopDeltaAt(8, 3))
	script.failStatus(false)
	if err := applier.Reconcile(context.Background()); err != nil {
		t.Fatalf("new epoch recovery ack: %v", err)
	}
	if accepted := oracle.ack(8, 3); !accepted {
		t.Fatal("control must accept an ack for its own epoch")
	}
	assertEquivalent("new epoch 8 + accepted ack", true)

	// Stage 7 — THE RESTART BOUNDARY (A2). Control restarts from epoch 8 to
	// epoch 9 BETWEEN the gateway's apply and its ack: the gateway still holds
	// the still-current epoch-8 state and its in-flight poll (served by the
	// pre-restart control) applies cleanly, but the epoch-8 ack reaches the
	// NEW control 9, which refuses it (foreign epoch) and records NOTHING.
	// The gateway must treat the explicit refusal as "not acked" and withdraw,
	// rather than keep reporting healthy on the superseded epoch's watermark.
	oracle.restart(9)
	script.setStatusEpoch(9)
	script.setDelta(t, ackWatermarkNoopDeltaAt(8, 3))
	if err := applier.Reconcile(context.Background()); !errors.Is(err, ErrAckFailed) {
		t.Fatalf("control restart between apply and ack: err=%v, want ErrAckFailed", err)
	}
	if accepted := oracle.ack(8, 3); accepted {
		t.Fatal("control 9 must refuse an epoch-8 ack")
	}
	assertEquivalent("control restarts to 9; the in-flight epoch-8 ack is refused", false)

	// Stage 8 — the gateway learns the new epoch (its applied epoch no longer
	// matches the page, forcing a fresh snapshot), applies it, and re-acks:
	// both recover together.
	oracle.publish(2)
	script.setSnapshot(t, Snapshot{
		Version:  ProtocolVersion,
		Epoch:    9,
		Revision: 2,
		Routes:   []Route{wireRoute(reconcileHostnameA, 2, reconcileAgentA, reconcilePort)},
	})
	if err := applier.ReconcileSnapshot(context.Background()); err != nil {
		t.Fatalf("epoch 9 snapshot: %v", err)
	}
	if accepted := oracle.ack(9, 2); !accepted {
		t.Fatal("control must accept an ack for its own epoch")
	}
	assertEquivalent("epoch 9 snapshot applied and acked", true)

	// Stage 9 — the new epoch's watermark behaves exactly as the old one:
	// publishing 3 and applying it with a rejected (transport) ack withdraws
	// both, and an accepted re-ack restores both.
	oracle.publish(3)
	script.setDelta(t, DeltaPage{
		Version:        ProtocolVersion,
		Epoch:          9,
		Status:         DeltaStatusOK,
		Since:          2,
		LatestRevision: 3,
		Deltas: []RouteDelta{{
			Revision:  3,
			Operation: RouteOperationAdd,
			Route:     wireRoute(reconcileHostnameB, 3, reconcileAgentB, reconcilePort),
		}},
	})
	script.failStatus(true)
	if err := applier.Reconcile(context.Background()); !errors.Is(err, ErrAckFailed) {
		t.Fatalf("epoch 9 new revision with rejected ack: err=%v, want ErrAckFailed", err)
	}
	assertEquivalent("epoch 9 publishes 3 + rejected ack", false)

	script.setDelta(t, ackWatermarkNoopDeltaAt(9, 3))
	script.failStatus(false)
	if err := applier.Reconcile(context.Background()); err != nil {
		t.Fatalf("epoch 9 recovery ack: %v", err)
	}
	if accepted := oracle.ack(9, 3); !accepted {
		t.Fatal("control must accept an ack for its own epoch")
	}
	assertEquivalent("epoch 9 publishes 3 + accepted ack", true)

	// Stage 10 — a no-op poll at the new epoch whose ack fails at the
	// transport keeps both healthy (the watermark still covers the applied
	// revision on both sides), preserving the A1 ordinary case across a
	// restart.
	script.setDelta(t, ackWatermarkNoopDeltaAt(9, 3))
	script.failStatus(true)
	if err := applier.Reconcile(context.Background()); !errors.Is(err, ErrAckFailed) {
		t.Fatalf("epoch 9 no-op poll with rejected ack: err=%v, want ErrAckFailed", err)
	}
	assertEquivalent("epoch 9 no-op poll + rejected ack", true)
}

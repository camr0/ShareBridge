package controlsync

// M5 remediation round 2, Finding A: a failed /status acknowledgement must
// reach the control-sync health truth. Control renews route leases only while
// the CURRENT gateway boot has acknowledged the latest published revision
// (relayctl.Publisher.Healthy), so if snapshot/delta GETs keep succeeding but
// every /status POST is rejected, /healthz must stop claiming route readiness
// rather than stay true while control's leases silently expire. The failed ack
// is deliberately NOT latched: the next successful acknowledgement restores
// the truth (control's own Healthy() is exactly that non-latching predicate).

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/routes"
)

func TestStatusAckFailureMarksControlUnhealthy(t *testing.T) {
	certs := newSyncTestCertificates(t)
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

	script.setSnapshot(t, Snapshot{
		Version:  ProtocolVersion,
		Epoch:    7,
		Revision: 40,
		Routes:   []Route{wireRoute(reconcileHostnameA, 40, reconcileAgentA, reconcilePort)},
	})
	// /status is rejected while the snapshot GET and its apply both succeed.
	script.failStatus(true)
	if err := loop.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("a rejected /status acknowledgement must surface as a reconcile failure")
	}
	// The apply committed independently of the ack: readiness is the applier's
	// own state, so this is not a case of "nothing was applied".
	if !applier.Ready() {
		t.Fatal("the snapshot apply succeeded; applier readiness must not depend on the ack")
	}
	if health.RouteReady() {
		t.Fatal("/healthz reports route_ready=true while /status is rejected; control renews leases only on a current ack")
	}

	// A later no-op delta poll with the ack still failing keeps it false.
	script.setDelta(t, DeltaPage{
		Version:        ProtocolVersion,
		Epoch:          7,
		Status:         DeltaStatusOK,
		Since:          40,
		LatestRevision: 40,
	})
	if err := loop.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("a still-rejected /status must keep failing the reconcile")
	}
	if health.RouteReady() {
		t.Fatal("route_ready must stay false for as long as acknowledgements fail")
	}

	// A successful acknowledgement restores the truth on the next pass.
	script.failStatus(false)
	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce after /status recovers: %v", err)
	}
	if !health.RouteReady() {
		t.Fatal("route_ready must be restored by a successful acknowledgement")
	}
}

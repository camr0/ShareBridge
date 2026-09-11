package controlsync

// §17.1 production health wiring: the sync loop is the only non-test caller of
// the gateway health snapshot/control-sync setters. This test drives the loop
// against the real Task 11 client and stub control and observes the two
// truths move — route readiness only after a real snapshot applies, control
// sync withdrawn on a real reconcile failure, and snapshot readiness never
// regressing (R2/§15.4).

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"sharebridge/relay/internal/routes"
)

type recordingHealth struct {
	mu            sync.Mutex
	snapshotReady bool
	controlSynced bool
}

func (health *recordingHealth) SetSnapshotReady(ready bool) {
	health.mu.Lock()
	health.snapshotReady = ready
	health.mu.Unlock()
}

func (health *recordingHealth) SetControlSynced(synced bool) {
	health.mu.Lock()
	health.controlSynced = synced
	health.mu.Unlock()
}

func (health *recordingHealth) truths() (snapshotReady, controlSynced bool) {
	health.mu.Lock()
	defer health.mu.Unlock()
	return health.snapshotReady, health.controlSynced
}

func TestSyncLoopReportsHealthFromRealReconcile(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)
	table := routes.NewTable(newReconcilePresence())
	applier := mustApplier(t, client, table, nil, time.Now)

	health := &recordingHealth{}
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
	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce(success): %v", err)
	}
	snapshotReady, controlSynced := health.truths()
	if !snapshotReady || !controlSynced {
		t.Fatalf("after a successful reconcile snapshotReady=%v controlSynced=%v, want both true", snapshotReady, controlSynced)
	}

	// A real reconcile failure (control answers a malformed snapshot) must
	// withdraw control sync while leaving snapshot readiness untouched.
	script.setRawSnapshot(t, []byte("not-json"))
	if err := loop.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("ReconcileOnce(malformed snapshot) error = nil, want a failure")
	}
	snapshotReady, controlSynced = health.truths()
	if controlSynced {
		t.Fatal("control sync must be withdrawn after a failed reconcile")
	}
	if !snapshotReady {
		t.Fatal("snapshot readiness must never regress (R2/§15.4)")
	}

	// Recovery: a good snapshot again restores control sync without ever
	// having cleared snapshot readiness.
	script.setSnapshot(t, Snapshot{
		Version:  ProtocolVersion,
		Epoch:    7,
		Revision: 40,
		Routes:   []Route{wireRoute(reconcileHostnameA, 40, reconcileAgentA, reconcilePort)},
	})
	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce(recovery): %v", err)
	}
	snapshotReady, controlSynced = health.truths()
	if !snapshotReady || !controlSynced {
		t.Fatalf("after recovery snapshotReady=%v controlSynced=%v, want both true", snapshotReady, controlSynced)
	}
}

// TestSyncLoopRunDrivesHealthOnTicker proves the periodic production driver
// (Run, not just ReconcileOnce) wires the same truths on its first pass.
func TestSyncLoopRunDrivesHealthOnTicker(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)
	table := routes.NewTable(newReconcilePresence())
	applier := mustApplier(t, client, table, nil, time.Now)
	script.setSnapshot(t, Snapshot{
		Version:  ProtocolVersion,
		Epoch:    7,
		Revision: 40,
		Routes:   []Route{wireRoute(reconcileHostnameA, 40, reconcileAgentA, reconcilePort)},
	})

	health := &recordingHealth{}
	loop, err := NewLoop(LoopConfig{Applier: applier, Interval: time.Hour, Health: health, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if snapshotReady, controlSynced := health.truths(); snapshotReady && controlSynced {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	snapshotReady, controlSynced := health.truths()
	t.Fatalf("Run did not drive health: snapshotReady=%v controlSynced=%v", snapshotReady, controlSynced)
}

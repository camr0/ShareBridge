package controlsync

// Behavioural coverage for the two §17.3 emissions the applier owns: route
// propagation lag (from a control publish timestamp) and revocation→stream-
// close latency (from the real drain path). Each test drives the production
// applier against the real Task 11 client/stub-control stack and observes the
// metric change — never by calling the registry method directly.

import (
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/metrics"
	"sharebridge/relay/internal/routes"
)

func newEmissionsApplier(t *testing.T, client *Client, table *routes.Table, streams StreamDrainer, registry *metrics.Registry, clock func() time.Time) *Applier {
	t.Helper()
	applier, err := NewApplier(ApplierConfig{
		Client:    client,
		Table:     table,
		Streams:   streams,
		Namespace: reconcileNamespace,
		BootID:    reconcileBootID,
		Logger:    slog.New(slog.DiscardHandler),
		Metrics:   registry,
		Clock:     clock,
	})
	if err != nil {
		t.Fatalf("NewApplier: %v", err)
	}
	return applier
}

func TestApplierRecordsPropagationLagFromPublishedAt(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)
	registry := metrics.NewRegistry(metrics.Relay)
	now := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	table := routes.NewTable(newReconcilePresence())
	applier := newEmissionsApplier(t, client, table, nil, registry, clock)

	script.setSnapshot(t, Snapshot{
		Version:     ProtocolVersion,
		Epoch:       7,
		Revision:    40,
		Routes:      []Route{wireRoute(reconcileHostnameA, 40, reconcileAgentA, reconcilePort)},
		PublishedAt: now.Add(-2 * time.Second).UTC().Format(time.RFC3339),
	})
	if err := applier.ReconcileSnapshot(t.Context()); err != nil {
		t.Fatalf("ReconcileSnapshot: %v", err)
	}

	rendered := registry.Render()
	if !strings.Contains(rendered, "sharebridge_relay_route_propagation_lag_seconds_count{} 1") {
		t.Fatalf("a snapshot with published_at recorded no propagation-lag observation:\n%s", rendered)
	}
	if !strings.Contains(rendered, "sharebridge_relay_route_propagation_lag_seconds_sum{} 2") {
		t.Fatalf("propagation-lag sum is not the real 2-second age:\n%s", rendered)
	}
}

func TestApplierNeverFabricatesPropagationLag(t *testing.T) {
	for _, publishedAt := range []string{"", "not-a-time"} {
		t.Run("published_at="+publishedAt, func(t *testing.T) {
			certs := newSyncTestCertificates(t)
			script, client := newScriptedControl(t, certs)
			registry := metrics.NewRegistry(metrics.Relay)
			now := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			table := routes.NewTable(newReconcilePresence())
			applier := newEmissionsApplier(t, client, table, nil, registry, clock)

			script.setSnapshot(t, Snapshot{
				Version:     ProtocolVersion,
				Epoch:       7,
				Revision:    40,
				Routes:      []Route{wireRoute(reconcileHostnameA, 40, reconcileAgentA, reconcilePort)},
				PublishedAt: publishedAt,
			})
			if err := applier.ReconcileSnapshot(t.Context()); err != nil {
				t.Fatalf("ReconcileSnapshot: %v", err)
			}
			if !applier.Ready() {
				t.Fatal("applier not ready after a successful snapshot apply")
			}
			if rendered := registry.Render(); strings.Contains(rendered, "sharebridge_relay_route_propagation_lag_seconds_count{} 1") {
				t.Fatalf("an absent/malformed published_at fabricated a lag observation:\n%s", rendered)
			}
		})
	}
}

func TestApplierRecordsRevocationCloseLatencyOnRealDrain(t *testing.T) {
	certs := newSyncTestCertificates(t)
	script, client := newScriptedControl(t, certs)
	registry := metrics.NewRegistry(metrics.Relay)
	now := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	streams := gateway.NewStreams()
	table := routes.NewTable(newReconcilePresence())
	applier := newEmissionsApplier(t, client, table, streams, registry, clock)

	script.setSnapshot(t, Snapshot{
		Version:  ProtocolVersion,
		Epoch:    7,
		Revision: 40,
		Routes:   []Route{wireRoute(reconcileHostnameA, 40, reconcileAgentA, reconcilePort)},
	})
	if err := applier.ReconcileSnapshot(t.Context()); err != nil {
		t.Fatalf("ReconcileSnapshot: %v", err)
	}

	// A real established stream on the exact route the revoke names.
	streamServer, streamClient := net.Pipe()
	defer streamServer.Close()
	defer streamClient.Close()
	streams.Register(reconcileHostnameA, reconcileAgentA, streamServer)
	if streams.Len() != 1 {
		t.Fatalf("streams registered = %d, want 1", streams.Len())
	}

	script.setDelta(t, DeltaPage{
		Version:        ProtocolVersion,
		Epoch:          7,
		Status:         DeltaStatusOK,
		Since:          40,
		LatestRevision: 41,
		Deltas: []RouteDelta{{
			Revision:  41,
			Operation: RouteOperationRevoke,
			Route: Route{
				Hostname:      reconcileHostnameA,
				AgentRecordID: reconcileAgentA,
				RelayPort:     reconcilePort,
				Generation:    3,
				SessionID:     reconcileSession,
				Revision:      41,
				Active:        false,
			},
		}},
	})
	if err := applier.SyncDeltas(t.Context()); err != nil {
		t.Fatalf("SyncDeltas: %v", err)
	}
	if streams.Len() != 0 {
		t.Fatal("the revoked route did not close its established stream")
	}
	if rendered := registry.Render(); !strings.Contains(rendered, "sharebridge_relay_revocation_close_seconds_count{} 1") {
		t.Fatalf("the real drain path recorded no revocation-close observation:\n%s", rendered)
	}
}

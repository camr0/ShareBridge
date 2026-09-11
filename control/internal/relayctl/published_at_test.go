package relayctl

// §17.3 bullet 2 propagation-lag support: control stamps every snapshot and
// delta page with the RFC 3339 publish time of the revision it serves. This
// test pins the stamp's presence, format, and that it is OBSERVABILITY ONLY —
// the payload still validates under the normal protocol rules and the R2
// epoch remains the adoption authority.

import (
	"testing"
	"time"
)

func TestPublisherStampsPublishedAtForPropagationLag(t *testing.T) {
	app := newPublisherTestApp(t)
	apiKey, _ := createPublisherAgent(t, app, "published", "sb7a8b9c0d", 10055, 2)
	sessionID := createPublisherSession(t, app, apiKey, "publishedshare", "pub.sb7a8b9c0d.example.com", nil)
	publisher := newTestPublisher(t, app, PublisherConfig{RevisionSeed: publisherTestSeed})
	// newTestPublisher injects a fixed clock at publisherTestBase; every
	// publish in this test happens at that instant, so the stamp is exact.
	want := publisherTestBase.UTC().Format(time.RFC3339)

	snapshot, err := publisher.RouteSnapshot()
	if err != nil {
		t.Fatalf("route snapshot: %v", err)
	}
	if snapshot.PublishedAt != want {
		t.Fatalf("snapshot published_at = %q, want %q", snapshot.PublishedAt, want)
	}
	if _, err := time.Parse(time.RFC3339, snapshot.PublishedAt); err != nil {
		t.Fatalf("snapshot published_at %q is not RFC 3339: %v", snapshot.PublishedAt, err)
	}
	if err := ValidateSnapshot(snapshot, MaxRoutesPerSnapshot); err != nil {
		t.Fatalf("snapshot with published_at fails protocol validation: %v", err)
	}

	if err := publisher.PublishAdd(sessionID); err != nil {
		t.Fatalf("publish add: %v", err)
	}
	page, err := publisher.RouteDeltas(publisherTestSeed)
	if err != nil {
		t.Fatalf("route deltas: %v", err)
	}
	if len(page.Deltas) != 1 {
		t.Fatalf("delta page carries %d deltas, want 1", len(page.Deltas))
	}
	if page.PublishedAt != want {
		t.Fatalf("delta page published_at = %q, want %q", page.PublishedAt, want)
	}
	if err := ValidateDeltaPage(page, MaxDeltasPerPage); err != nil {
		t.Fatalf("delta page with published_at fails protocol validation: %v", err)
	}

	// An empty OK page (since == current revision) still carries the stamp.
	current, err := publisher.RouteDeltas(publisher.CurrentRevision())
	if err != nil {
		t.Fatalf("current deltas: %v", err)
	}
	if current.PublishedAt != want {
		t.Fatalf("empty delta page published_at = %q, want %q", current.PublishedAt, want)
	}
}

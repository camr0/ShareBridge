// agent/internal/direct/archive_test.go
package direct

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sharebridge/agent/internal/immich"
)

// archiveBackend is a ContentBackend whose archive-relevant methods are function
// fields. It embeds *handlerBackend (defined in handlers_test.go) for the inert
// thumb/preview/asset/playback stubs and overrides the two archive methods.
type archiveBackend struct {
	*handlerBackend
	info   func(ctx context.Context) (immich.AlbumDownload, error)
	stream func(ctx context.Context, assetIDs []string, w io.Writer) (int64, error)
}

func (b *archiveBackend) GetAlbumDownloadInfo(ctx context.Context) (immich.AlbumDownload, error) {
	if b.info == nil {
		return immich.AlbumDownload{}, nil
	}
	return b.info(ctx)
}

func (b *archiveBackend) DownloadArchive(ctx context.Context, assetIDs []string, w io.Writer) (int64, error) {
	if b.stream == nil {
		return 0, nil
	}
	return b.stream(ctx, assetIDs, w)
}

// mutResolver returns a session that tests can swap atomically to simulate the
// SnapshotManager advancing the content generation (new session, new membership,
// same ledger + archive registry).
type mutResolver struct {
	mu      sync.Mutex
	session *ContentSession
}

func (m *mutResolver) Resolve(string) (*ContentSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.session, nil
}

// newArchiveHandler builds a DirectServer wired with backend for share "abc"
// (membership, generation, ledger, and a fresh archive registry), returning the
// handler and the mutable resolver.
func newArchiveHandler(t *testing.T, backend ContentBackend, membership map[string]struct{}, ledger *Ledger, gen uint64, ttl time.Duration, now func() time.Time) (http.Handler, *mutResolver) {
	t.Helper()
	reg := newArchiveRegistry(ledger, ttl, now)
	t.Cleanup(reg.Close)
	session := &ContentSession{
		Backend:    backend,
		Membership: membership,
		ContentGen: gen,
		Ledger:     ledger,
		Archives:   reg,
	}
	res := &mutResolver{session: session}
	return newHandlerServerResolve(t, res), res
}

func getManifest(t *testing.T, handler http.Handler) archiveManifestResponse {
	t.Helper()
	rr := doRequest(handler, http.MethodGet, "/s/abc/archive")
	if rr.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, want 200 (body=%q)", rr.Code, rr.Body.String())
	}
	var resp archiveManifestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return resp
}

func TestArchiveManifestReturnsTokenAndParts(t *testing.T) {
	backend := &archiveBackend{
		handlerBackend: &handlerBackend{},
		info: func(context.Context) (immich.AlbumDownload, error) {
			return immich.AlbumDownload{
				AlbumName: "Summer-2025",
				Archives: []immich.DownloadArchive{
					{AssetIDs: []string{"a1", "a2"}, Size: 500},
					{AssetIDs: []string{"a3"}, Size: 250},
				},
			}, nil
		},
	}
	handler, _ := newArchiveHandler(t, backend, map[string]struct{}{"a1": {}, "a2": {}, "a3": {}}, NewLedger(5), 1, time.Hour, time.Now)

	resp := getManifest(t, handler)
	if resp.Token == "" {
		t.Fatal("token must be non-empty")
	}
	if len(resp.Parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(resp.Parts))
	}
	p0 := resp.Parts[0]
	if p0.Index != 0 || p0.Name != "Summer-2025-part-1.zip" || p0.EstimatedSize != 500 {
		t.Fatalf("part 0 = %+v", p0)
	}
	if len(p0.AssetIDs) != 2 || p0.AssetIDs[0] != "a1" || p0.AssetIDs[1] != "a2" {
		t.Fatalf("part 0 assetIds = %v, want [a1 a2]", p0.AssetIDs)
	}
	p1 := resp.Parts[1]
	if p1.Index != 1 || p1.Name != "Summer-2025-part-2.zip" || p1.EstimatedSize != 250 {
		t.Fatalf("part 1 = %+v", p1)
	}
}

func TestArchiveManifestRejectsNonMember(t *testing.T) {
	backend := &archiveBackend{
		handlerBackend: &handlerBackend{},
		info: func(context.Context) (immich.AlbumDownload, error) {
			return immich.AlbumDownload{
				AlbumName: "A",
				Archives:  []immich.DownloadArchive{{AssetIDs: []string{"a1", "ghost"}, Size: 10}},
			}, nil
		},
	}
	handler, _ := newArchiveHandler(t, backend, map[string]struct{}{"a1": {}}, NewLedger(5), 1, time.Hour, time.Now)

	rr := doRequest(handler, http.MethodGet, "/s/abc/archive")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestArchiveManifestRejectsDuplicateIDs(t *testing.T) {
	backend := &archiveBackend{
		handlerBackend: &handlerBackend{},
		info: func(context.Context) (immich.AlbumDownload, error) {
			return immich.AlbumDownload{
				AlbumName: "A",
				Archives: []immich.DownloadArchive{
					{AssetIDs: []string{"a1"}, Size: 1},
					{AssetIDs: []string{"a1"}, Size: 2},
				},
			}, nil
		},
	}
	handler, _ := newArchiveHandler(t, backend, map[string]struct{}{"a1": {}}, NewLedger(5), 1, time.Hour, time.Now)

	rr := doRequest(handler, http.MethodGet, "/s/abc/archive")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestArchiveManifestZeroArchivesNotFound(t *testing.T) {
	backend := &archiveBackend{
		handlerBackend: &handlerBackend{},
		info: func(context.Context) (immich.AlbumDownload, error) {
			return immich.AlbumDownload{AlbumName: "A", Archives: nil}, nil
		},
	}
	handler, _ := newArchiveHandler(t, backend, map[string]struct{}{"a1": {}}, NewLedger(5), 1, time.Hour, time.Now)

	rr := doRequest(handler, http.MethodGet, "/s/abc/archive")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestArchivePartStreamsAttachment(t *testing.T) {
	var gotIDs []string
	backend := &archiveBackend{
		handlerBackend: &handlerBackend{},
		info: func(context.Context) (immich.AlbumDownload, error) {
			return immich.AlbumDownload{
				AlbumName: "Summer-2025",
				Archives:  []immich.DownloadArchive{{AssetIDs: []string{"a1", "a2"}, Size: 100}},
			}, nil
		},
		stream: func(_ context.Context, assetIDs []string, w io.Writer) (int64, error) {
			gotIDs = assetIDs
			n, err := io.WriteString(w, "ZIPDATA")
			return int64(n), err
		},
	}
	handler, _ := newArchiveHandler(t, backend, map[string]struct{}{"a1": {}, "a2": {}}, NewLedger(5), 1, time.Hour, time.Now)

	resp := getManifest(t, handler)
	rr := doRequest(handler, http.MethodGet, "/s/abc/archive/"+resp.Token+"/0")
	if rr.Code != http.StatusOK {
		t.Fatalf("part status = %d, want 200", rr.Code)
	}
	cd := rr.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q, want attachment prefix", cd)
	}
	if !strings.Contains(cd, "Summer-2025-part-1.zip") {
		t.Fatalf("Content-Disposition = %q, want filename Summer-2025-part-1.zip", cd)
	}
	if rr.Body.String() != "ZIPDATA" {
		t.Fatalf("body = %q, want ZIPDATA", rr.Body.String())
	}
	if len(gotIDs) != 2 || gotIDs[0] != "a1" || gotIDs[1] != "a2" {
		t.Fatalf("DownloadArchive assetIds = %v, want [a1 a2]", gotIDs)
	}
}

func TestArchiveCommitExactlyOnceAndDuplicateIdempotent(t *testing.T) {
	backend := &archiveBackend{
		handlerBackend: &handlerBackend{},
		info: func(context.Context) (immich.AlbumDownload, error) {
			return immich.AlbumDownload{
				AlbumName: "A",
				Archives: []immich.DownloadArchive{
					{AssetIDs: []string{"a1"}, Size: 1},
					{AssetIDs: []string{"a2"}, Size: 1},
				},
			}, nil
		},
		stream: func(_ context.Context, _ []string, w io.Writer) (int64, error) {
			n, err := io.WriteString(w, "Z")
			return int64(n), err
		},
	}
	ledger := NewLedger(5)
	handler, _ := newArchiveHandler(t, backend, map[string]struct{}{"a1": {}, "a2": {}}, ledger, 1, time.Hour, time.Now)

	resp := getManifest(t, handler)

	// Duplicate fetch of part 0 is idempotent: re-streamed, no double count.
	for i := 0; i < 2; i++ {
		if rr := doRequest(handler, http.MethodGet, "/s/abc/archive/"+resp.Token+"/0"); rr.Code != http.StatusOK {
			t.Fatalf("part 0 fetch %d: status = %d", i, rr.Code)
		}
	}
	if got := ledger.Downloads(); got != 0 {
		t.Fatalf("downloads after part 0 only: want 0, got %d", got)
	}

	// The last part commits exactly once.
	if rr := doRequest(handler, http.MethodGet, "/s/abc/archive/"+resp.Token+"/1"); rr.Code != http.StatusOK {
		t.Fatalf("part 1 fetch: status = %d", rr.Code)
	}
	if got := ledger.Downloads(); got != 1 {
		t.Fatalf("downloads after all parts: want 1, got %d", got)
	}

	// The committed transaction is gone: further fetches are 404, no double count.
	if rr := doRequest(handler, http.MethodGet, "/s/abc/archive/"+resp.Token+"/0"); rr.Code != http.StatusNotFound {
		t.Fatalf("post-commit fetch: status = %d, want 404", rr.Code)
	}
	if got := ledger.Downloads(); got != 1 {
		t.Fatalf("downloads after post-commit fetch: want 1, got %d", got)
	}
}

func TestArchiveAbandonedTransactionTTLReleasesNoCommit(t *testing.T) {
	backend := &archiveBackend{
		handlerBackend: &handlerBackend{},
		info: func(context.Context) (immich.AlbumDownload, error) {
			return immich.AlbumDownload{
				AlbumName: "A",
				Archives:  []immich.DownloadArchive{{AssetIDs: []string{"a1"}, Size: 1}},
			}, nil
		},
	}
	ledger := NewLedger(5)
	now := time.Unix(1_000_000, 0)
	handler, res := newArchiveHandler(t, backend, map[string]struct{}{"a1": {}}, ledger, 1, time.Hour, func() time.Time { return now })

	resp := getManifest(t, handler)
	if got := ledger.Reservations(); got != 1 {
		t.Fatalf("reservations after manifest: want 1, got %d", got)
	}

	// Abandon the transaction: advance past the TTL and reap.
	now = now.Add(2 * time.Hour)
	res.session.Archives.reap()

	if got := ledger.Reservations(); got != 0 {
		t.Fatalf("reservations after reap: want 0, got %d", got)
	}
	if got := ledger.Downloads(); got != 0 {
		t.Fatalf("downloads after reap: want 0, got %d", got)
	}
	if rr := doRequest(handler, http.MethodGet, "/s/abc/archive/"+resp.Token+"/0"); rr.Code != http.StatusNotFound {
		t.Fatalf("part after expiry: status = %d, want 404", rr.Code)
	}
}

func TestArchiveActivePartPinsTransaction(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	backend := &archiveBackend{
		handlerBackend: &handlerBackend{},
		info: func(context.Context) (immich.AlbumDownload, error) {
			return immich.AlbumDownload{
				AlbumName: "A",
				Archives:  []immich.DownloadArchive{{AssetIDs: []string{"a1"}, Size: 1}},
			}, nil
		},
		stream: func(_ context.Context, _ []string, w io.Writer) (int64, error) {
			close(started)
			<-release
			n, err := io.WriteString(w, "ZIP")
			return int64(n), err
		},
	}
	ledger := NewLedger(5)
	now := time.Unix(1_000_000, 0)
	handler, res := newArchiveHandler(t, backend, map[string]struct{}{"a1": {}}, ledger, 1, time.Hour, func() time.Time { return now })

	resp := getManifest(t, handler)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doRequest(handler, http.MethodGet, "/s/abc/archive/"+resp.Token+"/0")
	}()
	<-started // the part stream is now in flight; the transaction is pinned

	// Advance past the TTL and reap: the pinned transaction must survive.
	now = now.Add(2 * time.Hour)
	res.session.Archives.reap()
	if got := ledger.Reservations(); got != 1 {
		t.Fatalf("reservations while pinned: want 1, got %d", got)
	}

	close(release)
	rr := <-done
	if rr.Code != http.StatusOK {
		t.Fatalf("part status = %d, want 200", rr.Code)
	}
	if got := ledger.Downloads(); got != 1 {
		t.Fatalf("downloads after commit: want 1, got %d", got)
	}
	if got := ledger.Reservations(); got != 0 {
		t.Fatalf("reservations after commit: want 0, got %d", got)
	}
}

func TestArchiveContentGenChangeInvalidates(t *testing.T) {
	backend := &archiveBackend{
		handlerBackend: &handlerBackend{},
		info: func(context.Context) (immich.AlbumDownload, error) {
			return immich.AlbumDownload{
				AlbumName: "A",
				Archives:  []immich.DownloadArchive{{AssetIDs: []string{"a1"}, Size: 1}},
			}, nil
		},
		stream: func(_ context.Context, _ []string, w io.Writer) (int64, error) {
			n, err := io.WriteString(w, "Z")
			return int64(n), err
		},
	}
	ledger := NewLedger(5)
	handler, res := newArchiveHandler(t, backend, map[string]struct{}{"a1": {}}, ledger, 1, time.Hour, time.Now)

	resp := getManifest(t, handler)
	if got := ledger.Reservations(); got != 1 {
		t.Fatalf("reservations after manifest: want 1, got %d", got)
	}

	// Membership change: swap to a new session (gen 2, a1 removed), keeping the
	// same ledger + archive registry (mirrors SnapshotManager.doBuild).
	res.mu.Lock()
	res.session = &ContentSession{
		Backend:    backend,
		Membership: map[string]struct{}{"a2": {}},
		ContentGen: 2,
		Ledger:     ledger,
		Archives:   res.session.Archives,
	}
	res.mu.Unlock()

	rr := doRequest(handler, http.MethodGet, "/s/abc/archive/"+resp.Token+"/0")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("part after gen change: status = %d, want 403", rr.Code)
	}
	if got := ledger.Reservations(); got != 0 {
		t.Fatalf("reservations after gen-change part: want 0, got %d", got)
	}
	if got := ledger.Downloads(); got != 0 {
		t.Fatalf("downloads after gen-change part: want 0, got %d", got)
	}
}

func TestArchiveInvalidateTombstoneReturnsForbidden(t *testing.T) {
	backend := &archiveBackend{
		handlerBackend: &handlerBackend{},
		info: func(context.Context) (immich.AlbumDownload, error) {
			return immich.AlbumDownload{
				AlbumName: "A",
				Archives:  []immich.DownloadArchive{{AssetIDs: []string{"a1"}, Size: 1}},
			}, nil
		},
	}
	ledger := NewLedger(5)
	handler, res := newArchiveHandler(t, backend, map[string]struct{}{"a1": {}}, ledger, 1, time.Hour, time.Now)

	resp := getManifest(t, handler)
	if got := ledger.Reservations(); got != 1 {
		t.Fatalf("reservations after manifest: want 1, got %d", got)
	}

	// Drive the real invalidate() path (as SnapshotManager.doBuild does on a
	// membership change), not a manual session swap.
	res.session.Archives.invalidate()

	if got := ledger.Reservations(); got != 0 {
		t.Fatalf("reservations after invalidate: want 0, got %d", got)
	}

	// A part fetch for an invalidated token must be 403 (not 404).
	rr := doRequest(handler, http.MethodGet, "/s/abc/archive/"+resp.Token+"/0")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("part after invalidate: status = %d, want 403", rr.Code)
	}
	if got := ledger.Downloads(); got != 0 {
		t.Fatalf("downloads after invalidate: want 0, got %d", got)
	}
}

func TestArchiveInvalidateCancelsInFlightStream(t *testing.T) {
	started := make(chan struct{})
	streamErr := make(chan error, 1)
	backend := &archiveBackend{
		handlerBackend: &handlerBackend{},
		info: func(context.Context) (immich.AlbumDownload, error) {
			return immich.AlbumDownload{
				AlbumName: "A",
				Archives:  []immich.DownloadArchive{{AssetIDs: []string{"a1"}, Size: 1}},
			}, nil
		},
		stream: func(ctx context.Context, _ []string, _ io.Writer) (int64, error) {
			close(started)
			select {
			case <-ctx.Done():
				streamErr <- ctx.Err()
				return 0, ctx.Err()
			case <-time.After(5 * time.Second):
				streamErr <- nil
				return 0, nil
			}
		},
	}
	ledger := NewLedger(5)
	handler, res := newArchiveHandler(t, backend, map[string]struct{}{"a1": {}}, ledger, 1, time.Hour, time.Now)

	resp := getManifest(t, handler)
	if got := ledger.Reservations(); got != 1 {
		t.Fatalf("reservations after manifest: want 1, got %d", got)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doRequest(handler, http.MethodGet, "/s/abc/archive/"+resp.Token+"/0")
	}()
	<-started // the part stream is in flight

	// invalidate() must atomically cancel the in-flight stream.
	res.session.Archives.invalidate()

	select {
	case err := <-streamErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("invalidate() did not cancel the in-flight stream")
	}

	rr := <-done
	if rr.Code == http.StatusOK {
		t.Fatalf("part status = 200, want non-200 after invalidate")
	}
	if got := ledger.Reservations(); got != 0 {
		t.Fatalf("reservations after invalidate: want 0, got %d", got)
	}
	if got := ledger.Downloads(); got != 0 {
		t.Fatalf("downloads after invalidate: want 0, got %d", got)
	}
}

func TestArchiveManifestGenerationChangeDuringFetch(t *testing.T) {
	firstFetchStarted := make(chan struct{})
	firstFetchRelease := make(chan struct{})
	calls := 0
	backend := &archiveBackend{
		handlerBackend: &handlerBackend{},
		info: func(context.Context) (immich.AlbumDownload, error) {
			calls++
			if calls == 1 {
				close(firstFetchStarted)
				<-firstFetchRelease
				return immich.AlbumDownload{
					AlbumName: "old",
					Archives:  []immich.DownloadArchive{{AssetIDs: []string{"a1"}, Size: 1}},
				}, nil
			}
			return immich.AlbumDownload{
				AlbumName: "new",
				Archives: []immich.DownloadArchive{
					{AssetIDs: []string{"a1"}, Size: 1},
					{AssetIDs: []string{"a2"}, Size: 2},
				},
			}, nil
		},
	}
	handler, res := newArchiveHandler(t, backend, map[string]struct{}{"a1": {}}, NewLedger(5), 1, time.Hour, time.Now)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doRequest(handler, http.MethodGet, "/s/abc/archive")
	}()
	<-firstFetchStarted // the first GetAlbumDownloadInfo is in flight

	// Advance the generation while the first fetch is in flight.
	res.mu.Lock()
	res.session = &ContentSession{
		Backend:    backend,
		Membership: map[string]struct{}{"a1": {}, "a2": {}},
		ContentGen: 2,
		Ledger:     res.session.Ledger,
		Archives:   res.session.Archives,
	}
	res.mu.Unlock()

	close(firstFetchRelease)
	rr := <-done

	if rr.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, want 200 (body=%q)", rr.Code, rr.Body.String())
	}
	if calls != 2 {
		t.Fatalf("GetAlbumDownloadInfo calls = %d, want 2 (discard + retry)", calls)
	}
	var resp archiveManifestResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Parts) != 2 {
		t.Fatalf("parts = %d, want 2 (from the retried fetch)", len(resp.Parts))
	}
	if resp.Parts[0].Name != "new-part-1.zip" {
		t.Fatalf("part 0 name = %q, want from the retried fetch", resp.Parts[0].Name)
	}
}

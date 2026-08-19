// agent/internal/direct/download_accounting_test.go
package direct

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"sharebridge/agent/internal/immich"
)

// assetLedgerServer builds a DirectServer wired with a session that carries the
// given ledger so download-accounting tests can assert reserve/commit/release
// behavior on the original-asset path.
func assetLedgerServer(t *testing.T, backend ContentBackend, ledger *Ledger) http.Handler {
	t.Helper()
	return newHandlerServerResolve(t, &fakeResolver{sessions: map[string]*ContentSession{
		"abc": {Backend: backend, Membership: map[string]struct{}{"asset-1": {}}, Ledger: ledger},
	}})
}

// assetBackend returns a handlerBackend that streams "DATA" for asset-1 with
// the given asset metadata, plus optional GetFile override behavior.
func assetBackend(stream func(ctx context.Context, id string, w io.Writer) (int64, error)) *handlerBackend {
	return &handlerBackend{
		assetInfo: func(context.Context, string) (immich.Asset, error) {
			return immich.Asset{OriginalFileName: "x.bin", OriginalMimeType: "application/octet-stream"}, nil
		},
		file: stream,
	}
}

func TestAssetDownloadReserveCommitOnSuccess(t *testing.T) {
	ledger := NewLedger(5)
	backend := assetBackend(func(_ context.Context, _ string, w io.Writer) (int64, error) {
		n, err := io.WriteString(w, "DATA")
		return int64(n), err
	})
	handler := assetLedgerServer(t, backend, ledger)

	rr := doRequest(handler, http.MethodGet, "/s/abc/asset/asset-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := ledger.Downloads(); got != 1 {
		t.Fatalf("downloads = %d, want 1 (successful asset download commits once)", got)
	}
	if got := ledger.Reservations(); got != 0 {
		t.Fatalf("reservations = %d, want 0 after commit", got)
	}
}

func TestAssetDownloadReleaseOnStreamFailure(t *testing.T) {
	ledger := NewLedger(5)
	backend := assetBackend(func(context.Context, string, io.Writer) (int64, error) {
		return 0, errors.New("upstream gone")
	})
	handler := assetLedgerServer(t, backend, ledger)

	rr := doRequest(handler, http.MethodGet, "/s/abc/asset/asset-1")
	if rr.Code == http.StatusOK {
		t.Fatalf("status = 200, want non-200 for a failing stream")
	}
	if got := ledger.Downloads(); got != 0 {
		t.Fatalf("downloads = %d, want 0 (failed download must not commit)", got)
	}
	if got := ledger.Reservations(); got != 0 {
		t.Fatalf("reservations = %d, want 0 (failed download must release)", got)
	}
}

func TestAssetDownloadReleaseOnCancel(t *testing.T) {
	started := make(chan struct{})
	aborted := make(chan struct{})
	ledger := NewLedger(5)
	backend := assetBackend(func(ctx context.Context, _ string, _ io.Writer) (int64, error) {
		close(started)
		<-ctx.Done()
		close(aborted)
		return 0, ctx.Err()
	})
	handler := assetLedgerServer(t, backend, ledger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := admittedRequest(http.MethodGet, "/s/abc/asset/asset-1", testOrigin).WithContext(ctx)

	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rr, req)
	}()
	<-started
	cancel()

	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream read was not aborted after request context cancellation")
	}
	<-done

	if got := ledger.Downloads(); got != 0 {
		t.Fatalf("downloads = %d, want 0 (cancelled download must not commit)", got)
	}
	if got := ledger.Reservations(); got != 0 {
		t.Fatalf("reservations = %d, want 0 (cancelled download must release)", got)
	}
}

func TestAssetDownloadRespectsMaxDownloads(t *testing.T) {
	ledger := NewLedger(1)
	backend := assetBackend(func(_ context.Context, _ string, w io.Writer) (int64, error) {
		n, err := io.WriteString(w, "DATA")
		return int64(n), err
	})
	handler := assetLedgerServer(t, backend, ledger)

	// First download commits and exhausts the limit.
	if rr := doRequest(handler, http.MethodGet, "/s/abc/asset/asset-1"); rr.Code != http.StatusOK {
		t.Fatalf("first download status = %d, want 200", rr.Code)
	}
	if got := ledger.Downloads(); got != 1 {
		t.Fatalf("downloads after first = %d, want 1", got)
	}

	// The N+1th download is blocked before streaming (403), not committed.
	streamed := false
	backend.file = func(context.Context, string, io.Writer) (int64, error) {
		streamed = true
		return 0, nil
	}
	rr := doRequest(handler, http.MethodGet, "/s/abc/asset/asset-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("second download status = %d, want 403 (limit exhausted)", rr.Code)
	}
	if streamed {
		t.Fatal("second download must be rejected before streaming begins")
	}
	if got := ledger.Downloads(); got != 1 {
		t.Fatalf("downloads after blocked download = %d, want 1 (no double count)", got)
	}
}

func TestAssetDownloadCommitPersists(t *testing.T) {
	var (
		mu       sync.Mutex
		gotCount int
	)
	ledger := NewLedger(5)
	ledger.SetPersist(func(count int) error {
		mu.Lock()
		gotCount = count
		mu.Unlock()
		return nil
	})
	backend := assetBackend(func(_ context.Context, _ string, w io.Writer) (int64, error) {
		n, err := io.WriteString(w, "DATA")
		return int64(n), err
	})
	handler := assetLedgerServer(t, backend, ledger)

	if rr := doRequest(handler, http.MethodGet, "/s/abc/asset/asset-1"); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotCount != 1 {
		t.Fatalf("persist count = %d, want 1 (committed asset download must persist)", gotCount)
	}
}

func TestArchiveCommitPersists(t *testing.T) {
	var (
		mu       sync.Mutex
		gotCount int
	)
	ledger := NewLedger(5)
	ledger.SetPersist(func(count int) error {
		mu.Lock()
		gotCount = count
		mu.Unlock()
		return nil
	})
	backend := &archiveBackend{
		handlerBackend: &handlerBackend{},
		info: func(context.Context) (immich.AlbumDownload, error) {
			return immich.AlbumDownload{
				AlbumName: "A",
				Archives:  []immich.DownloadArchive{{AssetIDs: []string{"asset-1"}, Size: 1}},
			}, nil
		},
		stream: func(_ context.Context, _ []string, w io.Writer) (int64, error) {
			n, err := io.WriteString(w, "ZIP")
			return int64(n), err
		},
	}
	handler, _ := newArchiveHandler(t, backend, map[string]struct{}{"asset-1": {}}, ledger, 1, time.Hour, time.Now)

	resp := getManifest(t, handler)
	if rr := doRequest(handler, http.MethodGet, "/s/abc/archive/"+resp.Token+"/0"); rr.Code != http.StatusOK {
		t.Fatalf("part status = %d, want 200", rr.Code)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotCount != 1 {
		t.Fatalf("persist count = %d, want 1 (committed archive download must persist)", gotCount)
	}
}

func TestLedgerCommitPersistErrorDoesNotAffectCount(t *testing.T) {
	l := NewLedger(2)
	l.SetPersist(func(int) error { return errors.New("disk full") })
	if !l.TryReserve() {
		t.Fatal("reserve should succeed")
	}
	if got := l.Commit(); got != 1 {
		t.Fatalf("Commit = %d, want 1 despite persistence failure", got)
	}
	if got := l.Downloads(); got != 1 {
		t.Fatalf("Downloads = %d, want 1 (in-memory count unaffected by persist error)", got)
	}
}

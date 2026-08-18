// agent/internal/direct/content_test.go
package direct

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"sharebridge/agent/internal/immich"
)

// fakeBackend is a minimal ContentBackend whose ListGallery result is
// test-controllable. All other methods are inert zero-value stubs.
type fakeBackend struct {
	mu      sync.Mutex
	gallery immich.Gallery
	err     error
}

func (f *fakeBackend) setGallery(g immich.Gallery) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gallery = g
}

func (f *fakeBackend) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeBackend) ListGallery(ctx context.Context) (immich.Gallery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gallery, f.err
}

func (f *fakeBackend) GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error) {
	return 0, nil
}
func (f *fakeBackend) GetPreview(ctx context.Context, id string, w io.Writer) (int64, error) {
	return 0, nil
}
func (f *fakeBackend) GetAssetInfo(ctx context.Context, id string) (immich.Asset, error) {
	return immich.Asset{}, nil
}
func (f *fakeBackend) GetFile(ctx context.Context, id string, w io.Writer) (int64, error) {
	return 0, nil
}
func (f *fakeBackend) GetVideoPlayback(ctx context.Context, id string, w io.Writer) (int64, error) {
	return 0, nil
}
func (f *fakeBackend) GetVideoPlaybackRange(ctx context.Context, id string, startOffset int64, w io.Writer) (int64, error) {
	return 0, nil
}
func (f *fakeBackend) HeadVideoPlayback(ctx context.Context, id string) (int64, error) {
	return 0, nil
}
func (f *fakeBackend) GetAlbumDownloadInfo(ctx context.Context) (immich.AlbumDownload, error) {
	return immich.AlbumDownload{}, nil
}
func (f *fakeBackend) DownloadArchive(ctx context.Context, assetIDs []string, w io.Writer) (int64, error) {
	return 0, nil
}
func (f *fakeBackend) ThumbnailInfo(ctx context.Context, id string) (string, int64, bool, error) {
	return "", 0, false, nil
}
func (f *fakeBackend) PreviewInfo(ctx context.Context, id string) (string, int64, bool, error) {
	return "", 0, false, nil
}
func (f *fakeBackend) PlaybackInfo(ctx context.Context, id string) (int64, bool, error) {
	return 0, false, nil
}

var _ ContentBackend = (*fakeBackend)(nil)

func TestLedgerTryReserveEnforcesLimit(t *testing.T) {
	l := NewLedger(2)
	if !l.TryReserve() {
		t.Fatal("first reserve should succeed")
	}
	if !l.TryReserve() {
		t.Fatal("second reserve should succeed (within limit)")
	}
	if l.TryReserve() {
		t.Fatal("third reserve should fail (limit exhausted)")
	}
}

func TestLedgerUnlimitedWhenMaxNonPositive(t *testing.T) {
	for _, max := range []int{0, -1} {
		l := NewLedger(max)
		for i := 0; i < 100; i++ {
			if !l.TryReserve() {
				t.Fatalf("max=%d: reserve %d should always succeed", max, i)
			}
		}
	}
}

func TestLedgerCommitIncrementsDownloads(t *testing.T) {
	l := NewLedger(2)
	l.TryReserve() // reservations=1
	if got := l.Commit(); got != 1 {
		t.Fatalf("Commit: want downloads=1, got %d", got)
	}
	if got := l.Downloads(); got != 1 {
		t.Fatalf("Downloads: want 1, got %d", got)
	}
}

func TestLedgerReleaseDecrementsReservations(t *testing.T) {
	l := NewLedger(1)
	l.TryReserve() // reservations=1
	if got := l.Reservations(); got != 1 {
		t.Fatalf("Reservations after reserve: want 1, got %d", got)
	}
	l.Release()
	if got := l.Reservations(); got != 0 {
		t.Fatalf("Reservations after release: want 0, got %d", got)
	}
	// The freed reservation must make admission possible again.
	if !l.TryReserve() {
		t.Fatal("reserve after release should succeed")
	}
}

func TestLedgerConcurrentTryReserveOnlyOneSucceeds(t *testing.T) {
	l := NewLedger(2)
	l.TryReserve()
	l.Commit() // downloads=1 == max-1

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- l.TryReserve()
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	for ok := range results {
		if ok {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("want exactly 1 successful TryReserve at downloads==max-1, got %d", successes)
	}
}

func TestSnapshotManagerResolveUnreadyBeforeBuild(t *testing.T) {
	m := NewSnapshotManager(&fakeBackend{}, 5, time.Minute)
	if _, err := m.Resolve(); !errors.Is(err, ErrUnready) {
		t.Fatalf("before Build: want ErrUnready, got %v", err)
	}
}

func TestSnapshotManagerResolveReturnsSessionAfterBuild(t *testing.T) {
	b := &fakeBackend{}
	b.setGallery(immich.Gallery{
		AlbumName: "Summer",
		Items:     []immich.GalleryItem{{ID: "a1", Name: "one.jpg"}},
	})
	m := NewSnapshotManager(b, 5, time.Minute)

	if err := m.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	s, err := m.Resolve()
	if err != nil {
		t.Fatalf("Resolve after Build: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil session")
	}
	if s.Gallery.AlbumName != "Summer" {
		t.Fatalf("album name: want Summer, got %q", s.Gallery.AlbumName)
	}
	if _, ok := s.Membership["a1"]; !ok {
		t.Fatal("membership missing asset a1")
	}
	if s.ContentGen != 1 {
		t.Fatalf("ContentGen: want 1, got %d", s.ContentGen)
	}
	if s.MaxDownloads != 5 {
		t.Fatalf("MaxDownloads: want 5, got %d", s.MaxDownloads)
	}
	if s.Ledger == nil {
		t.Fatal("session Ledger must not be nil")
	}
}

func TestSnapshotManagerFailClosedAfterRepeatedBuildFailures(t *testing.T) {
	b := &fakeBackend{}
	b.setGallery(immich.Gallery{AlbumName: "Summer"})
	m := NewSnapshotManager(b, 5, 10*time.Millisecond) // 2× poll = 20ms

	now := time.Unix(1_000_000, 0)
	m.now = func() time.Time { return now }

	if err := m.Build(context.Background()); err != nil {
		t.Fatalf("hydrate Build: %v", err)
	}
	if _, err := m.Resolve(); err != nil {
		t.Fatalf("Resolve after hydrate: %v", err)
	}

	b.setErr(errors.New("upstream down"))
	for i := 0; i < 3; i++ {
		if err := m.Build(context.Background()); err == nil {
			t.Fatalf("Build %d: want failure", i)
		}
	}

	// Within 2× poll of the last success: still serve the stale snapshot.
	if _, err := m.Resolve(); err != nil {
		t.Fatalf("Resolve within 2x poll: want stale serve, got %v", err)
	}

	// Advance past 2× poll (20ms): fail closed.
	now = now.Add(30 * time.Millisecond)
	if _, err := m.Resolve(); !errors.Is(err, ErrUnready) {
		t.Fatalf("Resolve after >2x poll failure: want ErrUnready, got %v", err)
	}
}

func TestSnapshotManagerBuildDoesNotAdvanceContentGenWhenUnchanged(t *testing.T) {
	b := &fakeBackend{}
	b.setGallery(immich.Gallery{
		AlbumName:        "Summer",
		AlbumDescription: "2025",
		Items: []immich.GalleryItem{
			{ID: "a1", Name: "one.jpg", MimeType: "image/jpeg", Width: 100, Height: 80, Size: 42, SHA1: "abc"},
		},
	})
	m := NewSnapshotManager(b, 5, time.Minute)

	if err := m.Build(context.Background()); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	if m.contentGen != 1 {
		t.Fatalf("contentGen after first Build: want 1, got %d", m.contentGen)
	}

	if err := m.Build(context.Background()); err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if m.contentGen != 1 {
		t.Fatalf("contentGen advanced on unchanged gallery: got %d", m.contentGen)
	}
}

func TestSnapshotManagerBuildAdvancesContentGenOnChange(t *testing.T) {
	b := &fakeBackend{}
	b.setGallery(immich.Gallery{AlbumName: "A", Items: []immich.GalleryItem{{ID: "a1"}}})
	m := NewSnapshotManager(b, 5, time.Minute)

	if err := m.Build(context.Background()); err != nil {
		t.Fatalf("Build 1: %v", err)
	}
	if m.contentGen != 1 {
		t.Fatalf("contentGen after Build 1: want 1, got %d", m.contentGen)
	}

	b.setGallery(immich.Gallery{AlbumName: "A", Items: []immich.GalleryItem{{ID: "a1"}, {ID: "a2"}}})
	if err := m.Build(context.Background()); err != nil {
		t.Fatalf("Build 2: %v", err)
	}
	if m.contentGen != 2 {
		t.Fatalf("contentGen after Build 2 (membership change): want 2, got %d", m.contentGen)
	}
}

func TestResolverRegistryRoundTrip(t *testing.T) {
	r := NewResolverRegistry()
	m := NewSnapshotManager(&fakeBackend{}, 5, time.Minute)

	if got := r.Get("code"); got != nil {
		t.Fatalf("Get before Put: want nil, got %v", got)
	}

	r.Put("code", m)
	if got := r.Get("code"); got != m {
		t.Fatal("Get after Put: want the same manager")
	}

	r.Delete("code")
	if got := r.Get("code"); got != nil {
		t.Fatalf("Get after Delete: want nil, got %v", got)
	}
}

func TestResolverRegistryResolveUnknownCode(t *testing.T) {
	r := NewResolverRegistry()
	if _, err := r.Resolve("missing"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("Resolve unknown code: want ErrUnknown, got %v", err)
	}
}

func TestItemsResponseMarshalsLowerCamel(t *testing.T) {
	dur := 12.5
	resp := itemsResponse{
		AlbumName:        "Summer",
		AlbumDescription: "vacation",
		Items: []itemDTO{
			{
				ID:       "a1",
				Name:     "one.jpg",
				MimeType: "image/jpeg",
				Width:    4032,
				Height:   3024,
				Size:     4200000,
				Duration: &dur,
				SHA1:     "abc123",
			},
		},
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"albumName", "albumDescription", "items"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("missing key %q in %s", key, data)
		}
	}
	if _, ok := m["album_name"]; ok {
		t.Fatalf("unexpected snake_case key in %s", data)
	}

	items, ok := m["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: want 1 element, got %v", m["items"])
	}
	first := items[0].(map[string]any)
	for _, key := range []string{"id", "name", "mimeType", "width", "height", "size", "duration", "sha1"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("missing item key %q in %s", key, data)
		}
	}
	if _, ok := first["mime_type"]; ok {
		t.Fatalf("unexpected snake_case item key in %s", data)
	}
}

func TestItemsResponseDurationNullWhenAbsent(t *testing.T) {
	resp := itemsResponse{AlbumName: "A", Items: []itemDTO{{ID: "a1", Duration: nil}}}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"duration":null`) {
		t.Fatalf("want \"duration\":null, got %s", data)
	}
}

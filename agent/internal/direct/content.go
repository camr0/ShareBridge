// agent/internal/direct/content.go
package direct

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"sharebridge/agent/internal/immich"
)

// ContentBackend is the narrow subset of the Immich client the HTTP content
// layer needs. It is implemented by *immich.Client (see the compile-time
// assertion below).
type ContentBackend interface {
	ListGallery(ctx context.Context) (immich.Gallery, error)
	GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error)
	GetPreview(ctx context.Context, id string, w io.Writer) (int64, error)
	GetAssetInfo(ctx context.Context, id string) (immich.Asset, error)
	GetFile(ctx context.Context, id string, w io.Writer) (int64, error)
	GetVideoPlayback(ctx context.Context, id string, w io.Writer) (int64, error)
	GetVideoPlaybackRange(ctx context.Context, id string, startOffset int64, w io.Writer) (int64, error)
	HeadVideoPlayback(ctx context.Context, id string) (int64, error)
	GetAlbumDownloadInfo(ctx context.Context) (immich.AlbumDownload, error)
	DownloadArchive(ctx context.Context, assetIDs []string, w io.Writer) (int64, error)
	ThumbnailInfo(ctx context.Context, id string) (string, int64, bool, error)
	PreviewInfo(ctx context.Context, id string) (string, int64, bool, error)
	PlaybackInfo(ctx context.Context, id string) (int64, bool, error)
}

var _ ContentBackend = (*immich.Client)(nil)

// ContentSession is an immutable per-share snapshot a request holds for its
// lifetime: the resolved backend, the complete gallery snapshot (DTO +
// membership index + generation), the immutable download limit, and the shared
// mutable accounting ledger. Lifecycle ("active, gallery-type, not revoked") is
// encoded by the resolver's sentinel errors, not by this value.
type ContentSession struct {
	Backend      ContentBackend
	Gallery      immich.Gallery
	Membership   map[string]struct{}
	ContentGen   uint64
	MaxDownloads int
	Ledger       *Ledger
}

// Ledger is the per-session download-accounting ledger. It tracks the committed
// download count plus active reservations, enforcing the immutable MaxDownloads
// limit. It is guarded by its own mutex (l.mu).
type Ledger struct {
	mu           sync.Mutex
	max          int
	downloads    int
	reservations int
}

// NewLedger returns a Ledger with the given immutable download limit. A limit
// <= 0 means unlimited admission (reservations are still tracked).
func NewLedger(max int) *Ledger {
	return &Ledger{max: max}
}

// TryReserve atomically grants admission when downloads+reservations < max, or
// unconditionally when max <= 0. On success it records a reservation.
func (l *Ledger) TryReserve() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max > 0 && l.downloads+l.reservations >= l.max {
		return false
	}
	l.reservations++
	return true
}

// Commit consumes one reservation and records a completed download, returning
// the new committed-download count.
func (l *Ledger) Commit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reservations > 0 {
		l.reservations--
	}
	l.downloads++
	return l.downloads
}

// Release consumes one reservation without counting a download.
func (l *Ledger) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reservations > 0 {
		l.reservations--
	}
}

// Downloads returns the committed-download count.
func (l *Ledger) Downloads() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.downloads
}

// Reservations returns the active-reservation count.
func (l *Ledger) Reservations() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reservations
}

// Resolver maps a share code to an immutable content session.
type Resolver interface {
	Resolve(code string) (*ContentSession, error)
}

var (
	// ErrUnknown reports an unknown share code (→ 404).
	ErrUnknown = errors.New("direct: unknown share code")
	// ErrForbidden reports a membership/limit/type failure (→ 403).
	ErrForbidden = errors.New("direct: forbidden")
	// ErrUnready reports content not yet hydrated or fail-closed (→ 503).
	ErrUnready = errors.New("direct: content not ready")
)

// SnapshotManager owns the per-share state lock (mu): the immutable gallery
// snapshot, its membership generation, and the refresh failure bookkeeping that
// drives the fail-closed policy.
type SnapshotManager struct {
	mu           sync.Mutex
	backend      ContentBackend
	session      *ContentSession
	contentGen   uint64
	refreshAt    time.Time // last successful refresh
	lastErrAt    time.Time // last failed refresh
	poll         time.Duration
	maxDownloads int
	ledger       *Ledger
	now          func() time.Time
	building     bool
	done         chan struct{}
	buildErr     error
}

// NewSnapshotManager returns a SnapshotManager for the given backend. The
// download limit is immutable per share; poll is the refresh interval used for
// the 2×poll fail-closed bound.
func NewSnapshotManager(backend ContentBackend, maxDownloads int, poll time.Duration) *SnapshotManager {
	return &SnapshotManager{
		backend:      backend,
		maxDownloads: maxDownloads,
		ledger:       NewLedger(maxDownloads),
		poll:         poll,
		now:          time.Now,
	}
}

// Build fetches the gallery outside the lock, then atomically swaps the snapshot
// under mu, advancing the content generation only when the gallery actually
// changed. Concurrent builds are singleflight-deduplicated.
func (m *SnapshotManager) Build(ctx context.Context) error {
	m.mu.Lock()
	if m.building {
		done := m.done
		m.mu.Unlock()
		select {
		case <-done:
			m.mu.Lock()
			err := m.buildErr
			m.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.building = true
	m.done = make(chan struct{})
	m.buildErr = nil
	m.mu.Unlock()

	err := m.doBuild(ctx)

	m.mu.Lock()
	m.building = false
	m.buildErr = err
	close(m.done)
	m.mu.Unlock()
	return err
}

func (m *SnapshotManager) doBuild(ctx context.Context) error {
	// Fetch OUTSIDE the lock: network I/O must not hold the per-share state lock.
	gallery, err := m.backend.ListGallery(ctx)
	if err != nil {
		m.mu.Lock()
		m.lastErrAt = m.now()
		m.mu.Unlock()
		return err
	}

	membership := make(map[string]struct{}, len(gallery.Items))
	for _, it := range gallery.Items {
		membership[it.ID] = struct{}{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.refreshAt = m.now()
	m.lastErrAt = time.Time{}

	if m.session != nil && galleryEqual(m.session.Gallery, gallery) {
		// Content unchanged: keep the snapshot and generation.
		return nil
	}

	m.contentGen++
	m.session = &ContentSession{
		Backend:      m.backend,
		Gallery:      gallery,
		Membership:   membership,
		ContentGen:   m.contentGen,
		MaxDownloads: m.maxDownloads,
		Ledger:       m.ledger,
	}
	return nil
}

// Resolve returns the current snapshot, ErrUnready before the first successful
// hydration, and ErrUnready (fail-closed) once refresh has failed for longer
// than 2× the poll interval since the last success.
func (m *SnapshotManager) Resolve() (*ContentSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session == nil {
		return nil, ErrUnready
	}
	if !m.lastErrAt.IsZero() && m.now().Sub(m.refreshAt) > 2*m.poll {
		return nil, ErrUnready
	}
	return m.session, nil
}

// galleryEqual reports whether two gallery snapshots are content-identical
// (album metadata plus every item field, deep-compared; membership is implied
// by the item ID set).
func galleryEqual(a, b immich.Gallery) bool {
	if a.AlbumName != b.AlbumName || a.AlbumDescription != b.AlbumDescription {
		return false
	}
	if len(a.Items) != len(b.Items) {
		return false
	}
	for i := range a.Items {
		if !galleryItemEqual(a.Items[i], b.Items[i]) {
			return false
		}
	}
	return true
}

func galleryItemEqual(a, b immich.GalleryItem) bool {
	if a.ID != b.ID || a.Name != b.Name || a.MimeType != b.MimeType ||
		a.Width != b.Width || a.Height != b.Height || a.Size != b.Size ||
		a.SHA1 != b.SHA1 {
		return false
	}
	if (a.Duration == nil) != (b.Duration == nil) {
		return false
	}
	if a.Duration != nil && *a.Duration != *b.Duration {
		return false
	}
	return true
}

// ResolverRegistry maps share codes to their per-share SnapshotManager. It
// implements Resolver.
type ResolverRegistry struct {
	mu     sync.Mutex
	byCode map[string]*SnapshotManager
}

// NewResolverRegistry returns an empty ResolverRegistry.
func NewResolverRegistry() *ResolverRegistry {
	return &ResolverRegistry{byCode: make(map[string]*SnapshotManager)}
}

// Get returns the SnapshotManager for code, or nil if unknown.
func (r *ResolverRegistry) Get(code string) *SnapshotManager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byCode[code]
}

// Put associates code with the given SnapshotManager.
func (r *ResolverRegistry) Put(code string, m *SnapshotManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byCode[code] = m
}

// Delete removes the mapping for code.
func (r *ResolverRegistry) Delete(code string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byCode, code)
}

// Resolve resolves code to its session, returning ErrUnknown for an unmapped
// code.
func (r *ResolverRegistry) Resolve(code string) (*ContentSession, error) {
	r.mu.Lock()
	m := r.byCode[code]
	r.mu.Unlock()
	if m == nil {
		return nil, ErrUnknown
	}
	return m.Resolve()
}

var _ Resolver = (*ResolverRegistry)(nil)

// itemsResponse is the lowerCamel wire DTO for /items. immich.Gallery has no
// JSON tags, so the handler marshals through this explicit schema.
type itemsResponse struct {
	AlbumName        string    `json:"albumName"`
	AlbumDescription string    `json:"albumDescription"`
	Items            []itemDTO `json:"items"`
}

type itemDTO struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	MimeType string   `json:"mimeType"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	Size     int64    `json:"size"`
	Duration *float64 `json:"duration"`
	SHA1     string   `json:"sha1"`
}

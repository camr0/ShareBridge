// agent/internal/direct/content.go
package direct

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
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
	Archives     *ArchiveRegistry

	// Streams is the per-share streaming semaphore (§11), stable across the
	// session's lifetime (like Ledger and Archives). A nil gate means
	// unlimited streaming admission.
	Streams *streamGate
}

// Ledger is the per-session download-accounting ledger. It tracks the committed
// download count plus active reservations, enforcing the immutable MaxDownloads
// limit. It is guarded by its own mutex (l.mu).
//
// persist is an optional callback invoked on every Commit with the new
// committed-download count, wired by the daemon to store.IncrementDownloads.
// A persistence failure is logged and does not affect the in-memory count,
// which still enforces the limit (§11.1).
type Ledger struct {
	mu           sync.Mutex
	max          int
	downloads    int
	reservations int
	persist      func(count int) error
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
// the new committed-download count. If a persist callback is installed it is
// invoked outside the lock with the new count; a persistence failure is logged
// and the in-memory count still enforces the limit (§11.1).
func (l *Ledger) Commit() int {
	l.mu.Lock()
	if l.reservations > 0 {
		l.reservations--
	}
	l.downloads++
	count := l.downloads
	persist := l.persist
	l.mu.Unlock()

	if persist != nil {
		if err := persist(count); err != nil {
			log.Printf("direct: persist download count: %v", err)
		}
	}
	return count
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

// SetPersist installs the optional persistence callback invoked on every
// Commit with the new committed-download count. It is intended to be set once
// by the daemon to store.IncrementDownloads; it may be re-set idempotently.
func (l *Ledger) SetPersist(fn func(count int) error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.persist = fn
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
	archives     *ArchiveRegistry
	streams      *streamGate
	now          func() time.Time
	building     bool
	done         chan struct{}
	buildErr     error
}

// NewSnapshotManager returns a SnapshotManager for the given backend. The
// download limit is immutable per share; poll is the refresh interval used for
// the 2×poll fail-closed bound.
func NewSnapshotManager(backend ContentBackend, maxDownloads int, poll time.Duration) *SnapshotManager {
	ledger := NewLedger(maxDownloads)
	archives := newArchiveRegistry(ledger, defaultArchiveTTL, time.Now)
	return &SnapshotManager{
		backend:      backend,
		maxDownloads: maxDownloads,
		ledger:       ledger,
		archives:     archives,
		streams:      newStreamGate(perShareStreamLimit),
		poll:         poll,
		now:          time.Now,
	}
}

// SetPersistDownload installs the ledger's persistence callback, wired by the
// daemon to store.IncrementDownloads so committed downloads (both original-
// asset and album-archive) are durable (§11.1).
func (m *SnapshotManager) SetPersistDownload(fn func(count int) error) {
	if m.ledger != nil {
		m.ledger.SetPersist(fn)
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
		Archives:     m.archives,
		Streams:      m.streams,
	}
	// Membership changed: roll back any open archive transactions (release their
	// reservations) atomically with the swap (§4.6/§5.2).
	if m.archives != nil {
		m.archives.invalidate()
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

// MintArchive mints an archive transaction for code atomically under the
// per-share state lock: it re-resolves the current session and, only if its
// generation still equals expectedGen, mints a fresh token + reservation bound
// to that session. A generation advance between the re-check and the mint is
// thus impossible (§4.6/§11). It implements the archiveMinter capability used
// by handleArchiveManifest.
func (r *ResolverRegistry) MintArchive(code string, expectedGen uint64, parts []ArchivePart) (string, error) {
	r.mu.Lock()
	m := r.byCode[code]
	r.mu.Unlock()
	if m == nil {
		return "", ErrUnknown
	}
	return m.mintArchive(expectedGen, parts)
}

// mintArchive holds the per-share state lock across the session re-resolution
// and the mint, closing the TOCTOU window that a separate re-check + mint would
// leave open.
func (m *SnapshotManager) mintArchive(expectedGen uint64, parts []ArchivePart) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session == nil {
		return "", ErrUnready
	}
	if !m.lastErrAt.IsZero() && m.now().Sub(m.refreshAt) > 2*m.poll {
		return "", ErrUnready
	}
	s := m.session
	if s.ContentGen != expectedGen {
		return "", errArchiveGenChanged
	}
	if s.Archives == nil {
		return "", errArchiveNotFound
	}
	return s.Archives.mint(s, parts)
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

// ---- Album archive (multi-part, transactional) ----

// ArchivePart is one immutable part of an album-download archive: its index, a
// display name, the asset IDs it packages (already validated against the
// snapshot membership), and its estimated size in bytes.
type ArchivePart struct {
	Index         int
	Name          string
	AssetIDs      []string
	EstimatedSize int64
}

// ArchiveTransaction is the in-memory state of one album-download transaction.
// It holds a reservation in the session ledger until it is committed (all parts
// fetched → one committed download) or released (abandoned/invalidated → no
// count). done/state/expiresAt/pinned are guarded by mu; the registry's lock
// guards map membership and the TTL reaper's scan.
type ArchiveTransaction struct {
	Token      string
	Parts      []ArchivePart
	ContentGen uint64

	mu        sync.Mutex
	done      map[int]bool
	state     string // open|committed|released
	expiresAt time.Time
	pinned    bool               // an in-flight part holds the pin
	cancel    context.CancelFunc // aborts the in-flight part stream (guarded by mu)
}

const (
	txnOpen      = "open"
	txnCommitted = "committed"
	txnReleased  = "released"
)

// defaultArchiveTTL is the idle-transaction TTL (§4.6): an abandoned
// transaction (no in-flight part) is released after this duration.
const defaultArchiveTTL = time.Hour

var (
	// errArchiveNotFound reports a missing archive/part/token (→ 404).
	errArchiveNotFound = errors.New("direct: archive not found")
	// errArchiveForbidden reports a membership/duplicate/generation failure or
	// an exhausted download limit (→ 403).
	errArchiveForbidden = errors.New("direct: archive forbidden")
	// errArchiveGenChanged reports that the content generation advanced between
	// a manifest fetch and its mint, so the caller must discard and retry.
	errArchiveGenChanged = errors.New("direct: archive generation changed")
)

// templateInflight is the singleflight slot for the upstream
// GetAlbumDownloadInfo fetch. The leader closes done after publishing its
// result (or error) into the registry; waiters re-enter template and observe
// the cache or become the leader themselves.
type templateInflight struct {
	done chan struct{}
}

// ArchiveRegistry owns the per-share album-archive state: the transaction map,
// the singleflight manifest-template cache keyed by content generation, and the
// idle-transaction reaper. It is guarded by mu; the ledger it accounts against
// is the same per-session ledger the asset handler uses.
type ArchiveRegistry struct {
	mu            sync.Mutex
	ledger        *Ledger
	txns          map[string]*ArchiveTransaction
	templateParts []ArchivePart
	templateGen   uint64
	inflight      *templateInflight
	ttl           time.Duration
	now           func() time.Time
	stop          chan struct{}
}

func newArchiveRegistry(ledger *Ledger, ttl time.Duration, now func() time.Time) *ArchiveRegistry {
	if now == nil {
		now = time.Now
	}
	return &ArchiveRegistry{
		ledger: ledger,
		txns:   make(map[string]*ArchiveTransaction),
		ttl:    ttl,
		now:    now,
	}
}

// Start launches the idle-transaction reaper. It is idempotent; Close stops it.
// mint calls Start lazily once the first transaction exists, so shares with no
// archive activity never spawn a reaper goroutine.
func (a *ArchiveRegistry) Start() {
	a.mu.Lock()
	if a.stop != nil {
		a.mu.Unlock()
		return
	}
	a.stop = make(chan struct{})
	stop := a.stop
	a.mu.Unlock()
	go a.reapLoop(stop)
}

// Close stops the reaper (if started). Safe to call on a never-started registry.
func (a *ArchiveRegistry) Close() {
	a.mu.Lock()
	stop := a.stop
	a.stop = nil
	a.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

func (a *ArchiveRegistry) reapLoop(stop chan struct{}) {
	if a.ttl <= 0 {
		return
	}
	interval := a.ttl / 4
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.reap()
		case <-stop:
			return
		}
	}
}

// template returns the validated manifest parts for the session's generation,
// singleflighting the upstream GetAlbumDownloadInfo fetch into a shared template
// keyed by the generation. The caller MUST re-resolve the session after this
// returns and retry if the generation advanced during the fetch (§11).
func (a *ArchiveRegistry) template(ctx context.Context, s *ContentSession) ([]ArchivePart, error) {
	for {
		a.mu.Lock()
		a.reapLocked()

		if a.templateParts != nil && a.templateGen == s.ContentGen {
			parts := cloneParts(a.templateParts)
			a.mu.Unlock()
			return parts, nil
		}

		if a.inflight != nil {
			f := a.inflight
			a.mu.Unlock()
			select {
			case <-f.done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		f := &templateInflight{done: make(chan struct{})}
		a.inflight = f
		gen := s.ContentGen
		a.mu.Unlock()

		parts, err := a.fetchAndValidate(ctx, s.Backend, s.Membership)

		a.mu.Lock()
		a.inflight = nil
		if err == nil {
			a.templateParts = parts
			a.templateGen = gen
		}
		a.mu.Unlock()
		close(f.done)

		if err != nil {
			return nil, err
		}
		return cloneParts(parts), nil
	}
}

// fetchAndValidate fetches the album download info outside the registry lock
// and validates it: every asset ID must be in the membership snapshot, duplicate
// IDs are rejected (→ 403 the whole manifest), and zero archives → 404.
func (a *ArchiveRegistry) fetchAndValidate(ctx context.Context, backend ContentBackend, membership map[string]struct{}) ([]ArchivePart, error) {
	info, err := backend.GetAlbumDownloadInfo(ctx)
	if err != nil {
		return nil, err
	}
	parts := make([]ArchivePart, 0, len(info.Archives))
	seen := make(map[string]struct{})
	for i, arch := range info.Archives {
		for _, id := range arch.AssetIDs {
			if _, ok := membership[id]; !ok {
				return nil, errArchiveForbidden
			}
			if _, dup := seen[id]; dup {
				return nil, errArchiveForbidden
			}
			seen[id] = struct{}{}
		}
		parts = append(parts, ArchivePart{
			Index:         i,
			Name:          archivePartName(info.AlbumName, i),
			AssetIDs:      append([]string(nil), arch.AssetIDs...),
			EstimatedSize: arch.Size,
		})
	}
	if len(parts) == 0 {
		return nil, errArchiveNotFound
	}
	return parts, nil
}

// mint reserves a download slot and creates a fresh transaction bound to the
// session's generation, returning the opaque token. It returns ErrForbidden when
// the download limit is exhausted.
func (a *ArchiveRegistry) mint(s *ContentSession, parts []ArchivePart) (string, error) {
	if a.ledger != nil && !a.ledger.TryReserve() {
		return "", ErrForbidden
	}
	token := newArchiveToken()
	txn := &ArchiveTransaction{
		Token:      token,
		Parts:      cloneParts(parts),
		ContentGen: s.ContentGen,
		done:       make(map[int]bool, len(parts)),
		state:      txnOpen,
		expiresAt:  a.now().Add(a.ttl),
	}
	a.mu.Lock()
	a.reapLocked()
	a.txns[token] = txn
	a.mu.Unlock()
	// Lazily ensure the idle-transaction reaper is running now that there is
	// at least one live transaction.
	a.Start()
	return token, nil
}

// beginPart validates a part fetch and pins the transaction. It derives a
// cancelable stream context from ctx and registers its CancelFunc on the
// transaction (under txn.mu) so invalidate() can atomically abort an in-flight
// part stream. On success it returns the deep-copied part, the pinned
// transaction, and the stream context to use for DownloadArchive.
func (a *ArchiveRegistry) beginPart(ctx context.Context, s *ContentSession, token string, part int) (ArchivePart, *ArchiveTransaction, context.Context, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reapLocked()

	txn, ok := a.txns[token]
	if !ok {
		return ArchivePart{}, nil, nil, errArchiveNotFound
	}

	txn.mu.Lock()
	defer txn.mu.Unlock()

	if txn.state == txnReleased {
		return ArchivePart{}, nil, nil, errArchiveForbidden
	}
	if part < 0 || part >= len(txn.Parts) {
		return ArchivePart{}, nil, nil, errArchiveNotFound
	}
	if txn.ContentGen != s.ContentGen {
		a.releaseLocked(txn)
		return ArchivePart{}, nil, nil, errArchiveForbidden
	}
	for _, id := range txn.Parts[part].AssetIDs {
		if _, ok := s.Membership[id]; !ok {
			a.releaseLocked(txn)
			return ArchivePart{}, nil, nil, errArchiveForbidden
		}
	}

	streamCtx, cancel := context.WithCancel(ctx)
	txn.cancel = cancel
	txn.pinned = true
	txn.expiresAt = a.now().Add(a.ttl)
	return clonePart(txn.Parts[part]), txn, streamCtx, nil
}

// endPart releases the pin taken by beginPart and, when the stream completed
// successfully, marks the part done; once every part is done the transaction
// commits exactly one download.
func (a *ArchiveRegistry) endPart(txn *ArchiveTransaction, part int, completed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	txn.mu.Lock()
	defer txn.mu.Unlock()

	txn.pinned = false
	txn.cancel = nil
	txn.expiresAt = a.now().Add(a.ttl)

	if completed && txn.state == txnOpen {
		txn.done[part] = true
		if len(txn.done) == len(txn.Parts) {
			txn.state = txnCommitted
			if a.ledger != nil {
				a.ledger.Commit()
			}
			delete(a.txns, txn.Token)
		}
	}
}

// reap releases idle (unpinned) transactions whose TTL has expired. It is
// invoked by the background reaper and opportunistically on every registry
// operation.
func (a *ArchiveRegistry) reap() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reapLocked()
}

func (a *ArchiveRegistry) reapLocked() {
	if a.ttl <= 0 {
		return
	}
	now := a.now()
	for _, txn := range a.txns {
		txn.mu.Lock()
		if txn.state == txnOpen && !txn.pinned && !now.Before(txn.expiresAt) {
			a.releaseLocked(txn)
		} else if txn.state == txnReleased && !now.Before(txn.expiresAt) {
			// Remove an expired invalidate() tombstone: it has 403'd long
			// enough; further fetches of the token are now 404.
			delete(a.txns, txn.Token)
		}
		txn.mu.Unlock()
	}
}

// invalidate releases every open transaction (no commit) and aborts any
// in-flight part stream — used when a membership change advances the generation
// and existing reservations must be rolled back (§4.6: "atomically cancel any
// active stream"). Each transaction is kept as a released tombstone so a
// subsequent part fetch returns 403 (not 404); the reaper removes the tombstone
// once its TTL expires. The stale generation-keyed template is discarded.
func (a *ArchiveRegistry) invalidate() {
	a.mu.Lock()
	var cancels []context.CancelFunc
	for _, txn := range a.txns {
		txn.mu.Lock()
		if txn.cancel != nil {
			cancels = append(cancels, txn.cancel)
		}
		a.invalidateLocked(txn)
		txn.mu.Unlock()
	}
	a.templateParts = nil
	a.templateGen = 0
	a.mu.Unlock()

	// Abort the in-flight streams after releasing the registry lock: cancel is
	// non-blocking and idempotent, and the aborted stream's endPart must be able
	// to re-acquire a.mu.
	for _, c := range cancels {
		c()
	}
}

// releaseLocked transitions an open transaction to released and releases its
// reservation (no download count), removing it from the map so subsequent
// fetches return 404. It is idempotent. The caller must hold both a.mu and
// txn.mu.
func (a *ArchiveRegistry) releaseLocked(txn *ArchiveTransaction) {
	if txn.state != txnOpen {
		return
	}
	txn.state = txnReleased
	txn.pinned = false
	txn.cancel = nil
	if a.ledger != nil {
		a.ledger.Release()
	}
	delete(a.txns, txn.Token)
}

// invalidateLocked transitions an open transaction to released and releases its
// reservation, but KEEPS it in the map as a tombstone so a subsequent part
// fetch for the invalidated token returns 403 (§4.6). It is idempotent. The
// caller must hold both a.mu and txn.mu.
func (a *ArchiveRegistry) invalidateLocked(txn *ArchiveTransaction) {
	if txn.state != txnOpen {
		return
	}
	txn.state = txnReleased
	txn.pinned = false
	txn.cancel = nil
	if a.ledger != nil {
		a.ledger.Release()
	}
}

func cloneParts(parts []ArchivePart) []ArchivePart {
	out := make([]ArchivePart, len(parts))
	for i, p := range parts {
		out[i] = clonePart(p)
	}
	return out
}

func clonePart(p ArchivePart) ArchivePart {
	p.AssetIDs = append([]string(nil), p.AssetIDs...)
	return p
}

func archivePartName(albumName string, index int) string {
	base := albumName
	if base == "" {
		base = "album"
	}
	return fmt.Sprintf("%s-part-%d.zip", base, index+1)
}

func newArchiveToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand is documented not to fail; fall back to a time-based token
		// so a request never fails on token generation.
		return fmt.Sprintf("archive-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

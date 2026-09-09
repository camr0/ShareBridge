// agent/internal/direct/handlers.go
package direct

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"sharebridge/agent/internal/immich"
)

// handleConnect serves GET /s/<code>/connect (§9.3): the interstitial's
// credential-free, content-free CORS reachability check of the direct agent
// origin. The Binder has already authorized SNI, Host, route namespace, and
// share code for this request; this handler then additionally requires the
// RouteDirect binding, the exact configured allowed origin (s.connectOrigin —
// the production interstitial origin by default, CONNECT_ALLOWED_ORIGIN for
// test deployments), and GET.
//
// Success is 204 with no-store and exactly one ACAO for the allowed origin. The
// check resolves no content, touches no backend, performs no session
// accounting, and never sets a cookie (§18.3: the connect endpoint exposes no
// content/cookie). Preflight (OPTIONS) is deliberately not implemented: the
// interstitial issues a simple CORS GET, so browsers never preflight, and an
// OPTIONS request fails as a plain 404 with no Access-Control-* headers —
// CORS capability is never broadened.
func (s *DirectServer) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Re-derive the admitted binding from the connection's SNI — the same
	// authorization the Binder performed moments ago — and require the direct
	// namespace. Failing closed on a missing TLS state keeps the direct-only
	// requirement airtight.
	sni := ""
	if r.TLS != nil {
		sni = r.TLS.ServerName
	}
	bd, err := s.binder.AdmitSNI(sni)
	if err != nil || bd.RouteKind != RouteDirect {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Header.Get("Origin") != s.connectOrigin {
		// Absent or foreign Origin: refuse without echoing the value.
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Access-Control-Allow-Origin", s.connectOrigin)
	w.WriteHeader(http.StatusNoContent)
}

// classifyErr maps a backend error to an HTTP status per §4.5: upstream
// NotFoundError → 404, AuthError → 403, any other UpstreamError → 502, and any
// non-HTTP error (timeout / unreachable / transport) → 503.
func classifyErr(err error) int {
	var nf *immich.NotFoundError
	if errors.As(err, &nf) {
		return http.StatusNotFound
	}
	var auth *immich.AuthError
	if errors.As(err, &auth) {
		return http.StatusForbidden
	}
	var up *immich.UpstreamError
	if errors.As(err, &up) {
		return http.StatusBadGateway
	}
	return http.StatusServiceUnavailable
}

// contentID extracts and validates the single {id} path segment under prefix.
// It rejects empty ids, dot segments, and any embedded path separator, so an
// encoded slash or multi-segment path never reaches the backend.
func contentID(rest, prefix string) (string, bool) {
	id := strings.TrimPrefix(rest, prefix)
	if id == "" || id == "." || id == ".." || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// contentSecurityPolicy is the §4.8 CSP: script-src stays strict (no inline
// scripts, no onclick handlers); style-src allows inline style attributes
// because the lightGallery runtime and gallery.js set style/style.cssText at
// runtime; img/media/connect are same-origin only.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self'; media-src 'self'; connect-src 'self'"

// setSecurityHeaders applies the shared security headers for content responses
// (§4.8): no sniffing, no caching (the share is ephemeral and revocable), no
// referrer, and the CSP.
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
}

// sanitizeFilename strips path separators and control characters from a
// filename so it can be embedded safely in a Content-Disposition header.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.Map(func(r rune) rune {
		if r == 0 || r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "asset"
	}
	return name
}

// contentDisposition returns an attachment Content-Disposition value for a
// sanitized filename, quoted/encoded by mime.FormatMediaType.
func contentDisposition(name string) string {
	return mime.FormatMediaType("attachment", map[string]string{"filename": name})
}

// statusWriter buffers the HTTP status until the first body write, so a
// streaming error that surfaces before any byte is written can still commit the
// correct mapped status instead of an already-committed 200 (§4.5).
type statusWriter struct {
	http.ResponseWriter
	status    int
	committed bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if sw.committed {
		return
	}
	sw.ResponseWriter.WriteHeader(code)
	sw.committed = true
}

func (sw *statusWriter) Write(p []byte) (int, error) {
	if !sw.committed {
		sw.ResponseWriter.WriteHeader(sw.status)
		sw.committed = true
	}
	return sw.ResponseWriter.Write(p)
}

// streamBody streams via the given function with a delayed status. A nil error
// commits the success status (even for an empty body). A pre-first-byte error
// commits the mapped status; a post-first-byte error logs and aborts the
// stream via panic(http.ErrAbortHandler) (§4.5).
func (s *DirectServer) streamBody(w http.ResponseWriter, status int, stream func(dst io.Writer) error) {
	sw := &statusWriter{ResponseWriter: w, status: status}
	dst, release := s.holdStream(sw)
	defer release()
	if err := stream(dst); err != nil {
		if !sw.committed {
			// The failure surfaced before the first body byte, so the success
			// status was never committed; map and commit the correct status.
			w.Header().Del("Content-Length")
			http.Error(w, http.StatusText(classifyErr(err)), classifyErr(err))
			return
		}
		log.Printf("direct: mid-stream failure: %v", err)
		panic(http.ErrAbortHandler)
	}
	if !sw.committed {
		sw.WriteHeader(status)
	}
}

// acquireStreams admits a streaming response under the per-share and global
// semaphores (§11). On success it returns a release func and ok=true. On
// saturation it returns the overload status — 429 (per-share, transient) or
// 503 (global) — and ok=false; the caller must write that response and not
// stream.
func (s *DirectServer) acquireStreams(session *ContentSession) (release func(), status int, ok bool) {
	var perShare *streamGate
	if session != nil {
		perShare = session.Streams
	}
	if !perShare.tryAcquire() {
		return nil, http.StatusTooManyRequests, false
	}
	if !s.globalStreams.tryAcquire() {
		perShare.release()
		return nil, http.StatusServiceUnavailable, false
	}
	return func() {
		s.globalStreams.release()
		perShare.release()
	}, 0, true
}

// writeOverloaded writes the overload response with a bounded-retry hint
// (§11). It clears streaming headers that a handler may have staged before the
// admission check so the short error body is not contradicted by a stale
// Content-Length.
func writeOverloaded(w http.ResponseWriter, status int) {
	h := w.Header()
	h.Del("Content-Length")
	h.Del("Content-Range")
	h.Del("Accept-Ranges")
	h.Del("Content-Disposition")
	h.Set("Retry-After", "1")
	http.Error(w, http.StatusText(status), status)
}

// streamBodyLimited is streamBody gated by the per-share + global streaming
// semaphores. When saturated it writes the overload response (429/503) and
// returns without streaming; otherwise the release is deferred so the slot is
// freed on success, pre-first-byte error, and the mid-stream abort panic alike.
func (s *DirectServer) streamBodyLimited(session *ContentSession, w http.ResponseWriter, status int, stream func(dst io.Writer) error) {
	release, overload, ok := s.acquireStreams(session)
	if !ok {
		writeOverloaded(w, overload)
		return
	}
	defer release()
	s.streamBody(w, status, stream)
}

// resolveAndMember resolves the share and verifies {id} ∈ the snapshot
// membership. It writes the mapped response (404/403/503) and returns ok=false
// when the request must not proceed.
func (s *DirectServer) resolveAndMember(w http.ResponseWriter, code, id string) (*ContentSession, bool) {
	session, ok := s.resolveContent(w, code)
	if !ok {
		return nil, false
	}
	if _, ok := session.Membership[id]; !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, false
	}
	return session, true
}

// writeImageHeaders sets the shared security headers plus Content-Type (falling
// back to application/octet-stream) and, when known, Content-Length.
func writeImageHeaders(w http.ResponseWriter, contentType string, length int64, known bool) {
	setSecurityHeaders(w)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	if known {
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	}
}

func (s *DirectServer) handleThumb(w http.ResponseWriter, r *http.Request, code, id string) {
	session, ok := s.resolveAndMember(w, code, id)
	if !ok {
		return
	}
	s.activity(w, r, code)
	contentType, length, known, err := session.Backend.ThumbnailInfo(r.Context(), id)
	if err != nil {
		http.Error(w, http.StatusText(classifyErr(err)), classifyErr(err))
		return
	}
	writeImageHeaders(w, contentType, length, known)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.streamBody(w, http.StatusOK, func(dst io.Writer) error {
		_, err := session.Backend.GetThumbnail(r.Context(), id, dst)
		return err
	})
}

func (s *DirectServer) handlePreview(w http.ResponseWriter, r *http.Request, code, id string) {
	session, ok := s.resolveAndMember(w, code, id)
	if !ok {
		return
	}
	s.activity(w, r, code)
	contentType, length, known, err := session.Backend.PreviewInfo(r.Context(), id)
	if err != nil {
		http.Error(w, http.StatusText(classifyErr(err)), classifyErr(err))
		return
	}
	writeImageHeaders(w, contentType, length, known)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.streamBody(w, http.StatusOK, func(dst io.Writer) error {
		_, err := session.Backend.GetPreview(r.Context(), id, dst)
		return err
	})
}

// handleItems serves the gallery metadata as the lowerCamel wire DTO. The
// snapshot's immich.Gallery has no JSON tags (it would marshal PascalCase
// keys), so it is mapped through itemsResponse/itemDTO explicitly.
func (s *DirectServer) handleItems(w http.ResponseWriter, r *http.Request, code string) {
	session, ok := s.resolveContent(w, code)
	if !ok {
		return
	}
	s.activity(w, r, code)
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	resp := itemsResponse{
		AlbumName:        session.Gallery.AlbumName,
		AlbumDescription: session.Gallery.AlbumDescription,
		Items:            make([]itemDTO, 0, len(session.Gallery.Items)),
	}
	for _, it := range session.Gallery.Items {
		resp.Items = append(resp.Items, itemDTO{
			ID:       it.ID,
			Name:     it.Name,
			MimeType: it.MimeType,
			Width:    it.Width,
			Height:   it.Height,
			Size:     it.Size,
			Duration: it.Duration,
			SHA1:     it.SHA1,
		})
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *DirectServer) handleAsset(w http.ResponseWriter, r *http.Request, code, id string) {
	session, ok := s.resolveAndMember(w, code, id)
	if !ok {
		return
	}
	s.activity(w, r, code)
	asset, err := session.Backend.GetAssetInfo(r.Context(), id)
	if err != nil {
		http.Error(w, http.StatusText(classifyErr(err)), classifyErr(err))
		return
	}
	setSecurityHeaders(w)
	contentType := asset.OriginalMimeType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", contentDisposition(sanitizeFilename(asset.OriginalFileName)))
	if size := asset.FileSize(); size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Reserve a download slot before streaming and commit on successful
	// completion / release on failure or cancel — mirroring the album-archive
	// path's reserve-before-stream atomicity (§11.1). A nil ledger (test
	// doubles) means unlimited admission with no accounting.
	ledger := session.Ledger
	if ledger != nil && !ledger.TryReserve() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	completed := false
	defer func() {
		if ledger == nil {
			return
		}
		if completed {
			ledger.Commit()
		} else {
			ledger.Release()
		}
	}()

	s.streamBodyLimited(session, w, http.StatusOK, func(dst io.Writer) error {
		streamed, err := session.Backend.GetFile(r.Context(), id, dst)
		// A positive asset metadata size is authoritative. A clean short
		// upstream stream must release, rather than consume, the reservation.
		completed = err == nil && (asset.FileSize() <= 0 || streamed == asset.FileSize())
		return err
	})
}

// errBoundReached is the sentinel a boundedWriter returns once it has forwarded
// its full bound of bytes. It is not an error: it is how the writer halts the
// upstream read exactly at the range bound. Callers translate it back to nil.
var errBoundReached = errors.New("direct: playback range bound reached")

// boundedWriter forwards at most limit bytes to w, then returns errBoundReached
// to stop the upstream reader at the bound. It never reports the bound sentinel
// as a failure of the bytes it actually forwarded.
type boundedWriter struct {
	w     io.Writer
	limit int64
}

func (bw *boundedWriter) Write(p []byte) (int, error) {
	if bw.limit <= 0 {
		return 0, errBoundReached
	}
	if int64(len(p)) > bw.limit {
		p = p[:bw.limit]
	}
	n, err := bw.w.Write(p)
	bw.limit -= int64(n)
	if err == nil && bw.limit <= 0 {
		return n, errBoundReached
	}
	return n, err
}

// writeRangeNotSatisfiable commits a 416 with the `bytes */<total>` form.
// http.Error supplies the status, text body, and Content-Type.
func writeRangeNotSatisfiable(w http.ResponseWriter, total int64) {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
	http.Error(w, http.StatusText(http.StatusRequestedRangeNotSatisfiable), http.StatusRequestedRangeNotSatisfiable)
}

// handlePlayback serves the transcoded video stream at /asset/{id}/playback
// (§4.2). It honors a single satisfiable byte range when the transcoded length
// is authoritatively known; otherwise it serves the full body (200) or, for an
// authoritative zero length or an unsatisfiable range, a 416.
func (s *DirectServer) handlePlayback(w http.ResponseWriter, r *http.Request, code, id string) {
	session, ok := s.resolveAndMember(w, code, id)
	if !ok {
		return
	}
	s.activity(w, r, code)
	length, known, err := session.Backend.PlaybackInfo(r.Context(), id)
	if err != nil {
		http.Error(w, http.StatusText(classifyErr(err)), classifyErr(err))
		return
	}

	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "video/mp4")

	if r.Method == http.MethodHead {
		// HEAD ignores Range (§4.4): status + headers, no body, no 206.
		if known {
			w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	if !known {
		// Unknown total: ignore Range, serve 200 full body, no Accept-Ranges.
		s.streamBodyLimited(session, w, http.StatusOK, func(dst io.Writer) error {
			_, err := session.Backend.GetVideoPlayback(r.Context(), id, dst)
			return err
		})
		return
	}

	if length <= 0 {
		// Authoritative zero length → 416 with the */0 form.
		writeRangeNotSatisfiable(w, 0)
		return
	}

	if r.Header.Get("Range") == "" {
		// No Range header: serve the full body.
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		s.streamBodyLimited(session, w, http.StatusOK, func(dst io.Writer) error {
			_, err := session.Backend.GetVideoPlayback(r.Context(), id, dst)
			return err
		})
		return
	}

	start, end, _, ok := ParseRange(r.Header.Get("Range"), length, true)
	if !ok {
		// Suffix / multiple / malformed / start>=total / start>end → 416.
		writeRangeNotSatisfiable(w, length)
		return
	}

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, length))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	limit := end - start + 1
	s.streamBodyLimited(session, w, http.StatusPartialContent, func(dst io.Writer) error {
		bw := &boundedWriter{w: dst, limit: limit}
		_, err := session.Backend.GetVideoPlaybackRange(r.Context(), id, start, bw)
		if errors.Is(err, errBoundReached) {
			return nil
		}
		return err
	})
}

// archiveManifestResponse is the lowerCamel wire DTO for the archive manifest
// (§4.6): the opaque transaction token plus its parts.
type archiveManifestResponse struct {
	Token string           `json:"token"`
	Parts []archivePartDTO `json:"parts"`
}

type archivePartDTO struct {
	Index         int      `json:"index"`
	Name          string   `json:"name"`
	EstimatedSize int64    `json:"estimatedSize"`
	AssetIDs      []string `json:"assetIds"`
}

func archivePartDTOs(parts []ArchivePart) []archivePartDTO {
	out := make([]archivePartDTO, len(parts))
	for i, p := range parts {
		out[i] = archivePartDTO{
			Index:         p.Index,
			Name:          p.Name,
			EstimatedSize: p.EstimatedSize,
			AssetIDs:      p.AssetIDs,
		}
	}
	return out
}

// archiveStatus maps an archive error to an HTTP status: missing
// archive/part/token → 404, membership/duplicate/generation/limit failure →
// 403, upstream errors via classifyErr.
func archiveStatus(err error) int {
	switch {
	case errors.Is(err, errArchiveNotFound):
		return http.StatusNotFound
	case errors.Is(err, errArchiveForbidden), errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	default:
		return classifyErr(err)
	}
}

// maxArchiveManifestRetries bounds the generation-recheck retry loop (§11).
const maxArchiveManifestRetries = 5

// handleArchiveManifest serves GET /s/{code}/archive (§4.6): it singleflights
// the upstream GetAlbumDownloadInfo into a template keyed by the content
// generation, re-checks the generation after the fetch (discard+retry if it
// advanced), then mints a fresh token + transaction + reservation. HEAD
// validates the share but mints nothing.
func (s *DirectServer) handleArchiveManifest(w http.ResponseWriter, r *http.Request, code string) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodHead {
		if _, ok := s.resolveContent(w, code); !ok {
			return
		}
		s.activity(w, r, code)
		w.WriteHeader(http.StatusOK)
		return
	}

	recorded := false
	for attempts := 0; ; attempts++ {
		session, ok := s.resolveContent(w, code)
		if !ok {
			return
		}
		if !recorded {
			s.activity(w, r, code)
			recorded = true
		}
		if session.Archives == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		parts, err := session.Archives.template(r.Context(), session)
		if err != nil {
			http.Error(w, http.StatusText(archiveStatus(err)), archiveStatus(err))
			return
		}

		// Re-check the generation and mint atomically under the per-share state
		// lock (§11): if the membership advanced while the upstream fetch was
		// in flight, discard the template and retry against the new generation;
		// otherwise mint a transaction bound to the generation that was current
		// at check time — a generation advance cannot interleave the two.
		token, err := mintArchiveTransaction(s.resolver, code, session, parts)
		if err != nil {
			if errors.Is(err, errArchiveGenChanged) {
				if attempts >= maxArchiveManifestRetries {
					http.Error(w, "content changed, retry", http.StatusServiceUnavailable)
					return
				}
				continue
			}
			http.Error(w, http.StatusText(archiveStatus(err)), archiveStatus(err))
			return
		}
		_ = json.NewEncoder(w).Encode(archiveManifestResponse{
			Token: token,
			Parts: archivePartDTOs(parts),
		})
		return
	}
}

// archiveMinter is an optional Resolver capability: it mints an archive
// transaction atomically under the per-share state lock. ResolverRegistry
// implements it; test doubles that do not fall back to a re-resolve + mint in
// mintArchiveTransaction.
type archiveMinter interface {
	MintArchive(code string, expectedGen uint64, parts []ArchivePart) (string, error)
}

// mintArchiveTransaction atomically re-checks the content generation and mints
// the transaction. A resolver implementing archiveMinter does both under the
// per-share state lock; otherwise it falls back to a re-resolve + mint (the
// test double path, which is not concurrent with a real SnapshotManager).
func mintArchiveTransaction(r Resolver, code string, session *ContentSession, parts []ArchivePart) (string, error) {
	if m, ok := r.(archiveMinter); ok {
		return m.MintArchive(code, session.ContentGen, parts)
	}
	re, err := r.Resolve(code)
	if err != nil {
		return "", err
	}
	if re == nil {
		return "", ErrUnknown
	}
	if re.Archives == nil {
		return "", errArchiveNotFound
	}
	if re.ContentGen != session.ContentGen {
		return "", errArchiveGenChanged
	}
	return re.Archives.mint(re, parts)
}

// handleArchivePart serves GET/HEAD /s/{code}/archive/{token}/{part} (§4.6): it
// revalidates the part's asset IDs against the current generation before
// streaming, pins the transaction for the stream's duration, and commits exactly
// one download once the last part completes. Duplicate part fetches are
// idempotent (re-stream, no double count).
func (s *DirectServer) handleArchivePart(w http.ResponseWriter, r *http.Request, code, token string, part int) {
	session, ok := s.resolveContent(w, code)
	if !ok {
		return
	}
	s.activity(w, r, code)
	if session.Archives == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	p, txn, streamCtx, err := session.Archives.beginPart(r.Context(), session, token, part)
	if err != nil {
		http.Error(w, http.StatusText(archiveStatus(err)), archiveStatus(err))
		return
	}

	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(sanitizeFilename(p.Name)))

	if r.Method == http.MethodHead {
		session.Archives.endPart(txn, part, false)
		w.WriteHeader(http.StatusOK)
		return
	}

	// completed is set by the stream closure and read by the deferred endPart,
	// which runs on every exit path — success, pre-first-byte error, the
	// mid-stream abort panic, and the invalidate()-triggered cancellation — so
	// the pin is always released exactly once.
	completed := false
	defer func() {
		session.Archives.endPart(txn, part, completed)
	}()
	s.streamBodyLimited(session, w, http.StatusOK, func(dst io.Writer) error {
		_, err := session.Backend.DownloadArchive(streamCtx, p.AssetIDs, dst)
		if err == nil {
			completed = true
		}
		return err
	})
}

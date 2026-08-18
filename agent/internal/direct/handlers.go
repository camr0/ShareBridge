// agent/internal/direct/handlers.go
package direct

import (
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"sharebridge/agent/internal/immich"
)

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

// setSecurityHeaders applies the shared security headers for content responses
// (§4.8): no sniffing, no caching (the share is ephemeral and revocable), no
// referrer.
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
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
	if err := stream(sw); err != nil {
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

func (s *DirectServer) handleAsset(w http.ResponseWriter, r *http.Request, code, id string) {
	session, ok := s.resolveAndMember(w, code, id)
	if !ok {
		return
	}
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
	s.streamBody(w, http.StatusOK, func(dst io.Writer) error {
		_, err := session.Backend.GetFile(r.Context(), id, dst)
		return err
	})
}

package immich

import "fmt"

// AuthError indicates the upstream Immich server rejected a request because the
// credentials (API key / share key / password) were missing or invalid
// (HTTP 401 Unauthorized or 403 Forbidden). Classified via errors.As.
type AuthError struct {
	Status int    // 401 or 403
	Detail string // human-readable failure detail (request context + trimmed upstream body)
}

func (e *AuthError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("immich: authentication required (HTTP %d)", e.Status)
	}
	return fmt.Sprintf("immich: authentication required (HTTP %d): %s", e.Status, e.Detail)
}

// NotFoundError indicates the upstream Immich server reported the requested
// resource does not exist (HTTP 404 Not Found). Classified via errors.As.
type NotFoundError struct {
	Detail string // human-readable failure detail (request context + trimmed upstream body)
}

func (e *NotFoundError) Error() string {
	if e.Detail == "" {
		return "immich: not found (HTTP 404)"
	}
	return "immich: not found (HTTP 404): " + e.Detail
}

// UpstreamError indicates the upstream Immich server returned an unexpected
// HTTP status — anything other than 2xx, 401/403, or 404. Classified via
// errors.As. A Status of 0 denotes a non-HTTP upstream failure.
type UpstreamError struct {
	Status int
	Detail string // human-readable failure detail (request context + trimmed upstream body)
}

func (e *UpstreamError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("immich: upstream returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("immich: upstream returned HTTP %d: %s", e.Status, e.Detail)
}

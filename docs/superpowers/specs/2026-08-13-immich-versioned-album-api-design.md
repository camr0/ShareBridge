# Immich Versioned Album API Design

## Problem

Immich v3 removed asset arrays from album detail responses. ShareBridge currently falls back from an empty shared-link asset array to `GET /api/albums/{id}`, receives another empty array, and sends a zero-item gallery. Large albums make the required replacement especially visible because the supported metadata search endpoint returns at most 1,000 assets per page.

## Design

Each Immich client lazily requests `GET /api/server/version` and caches the result. Version state belongs to the client rather than process-global state so clients connected to different Immich servers cannot contaminate one another.

- Immich major versions below 3 use the legacy `GET /api/albums/{id}` asset response.
- Immich major versions 3 and above use `POST /api/search/metadata`, filtered by album ID, with a page size of 1,000 and `withExif: true`.
- Search pagination follows the server-provided `nextPage` token until it is null.
- Version detection failure falls back to capability-based behavior: try the legacy endpoint, then paginated search if it returns no assets.

The client logs the detected server version, selected compatibility path, each search page count, and the final asset total. Logs describe the concrete API strategy as well as its compatible Immich generation.

## Compatibility and Safety

The existing dual-format duration decoder remains unchanged. Version detection chooses endpoints; tolerant decoding handles payload representations independently and safely supports backports, prereleases, and forks.

Pagination rejects invalid, repeated, or non-numeric next-page tokens rather than looping forever. Password and shared-link query parameters continue to be attached to every request.

## Testing

Tests cover cached version detection, v2 legacy selection, v3 pagination across 1,000/1,000/453 assets, query/password propagation, version-detection fallback, empty albums, and malformed pagination. The complete Go test and vet suites plus the agent build verify the change before publication.

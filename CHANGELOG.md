# Changelog

All notable changes to ShareBridge are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.8.0] - 2026-08-13

### Added

- Browser share links with accounts, API keys, session expiry, download limits, and secure-relay quotas.
- OpenCloud file and directory browsing with streamed downloads.
- Immich share discovery, password-protected albums, image previews, streamed video playback and seeking, and multi-part Download All archives.
- Direct WebRTC transfers and a secure-relay fallback with end-to-end Noise encryption.
- Independent control, interactive-media, thumbnail, and bulk lanes with operation correlation, scheduling, and backpressure.
- Agent container publication to GHCR for self-hosted deployments.

### Changed

- Large Immich galleries now load progressively in 120-item windows instead of transferring and rendering every thumbnail up front.
- Immich album discovery supports both the v2 and v3 API shapes through server-version detection and capability fallback.

### Fixed

- Accepted numeric and string Immich asset durations across supported server versions.
- Improved media seeking, range requests, transfer completion accounting, disconnect cleanup, and lifecycle race handling.
- Added byte-count and checksum validation so incomplete or corrupted streamed downloads fail visibly.

### Security

- Relay payloads are end-to-end encrypted between the browser and agent; the signaling server does not receive plaintext file or media contents.
- Protected Immich passwords are validated by the agent and are not persisted by ShareBridge.

[Unreleased]: https://github.com/camr0/ShareBridge/compare/v0.8.0...HEAD
[0.8.0]: https://github.com/camr0/ShareBridge/releases/tag/v0.8.0

# Immich Duration Compatibility Design

## Problem

ShareBridge models Immich asset `duration` values as strings. Current Immich releases return nullable integer milliseconds in `AssetResponseDto`, so decoding `/api/shared-links` fails when any asset contains a numeric duration. Because polling decodes the response as one unit, this prevents every shared link in that poll from synchronizing.

## Design

Introduce a focused JSON duration type in the Immich client package. It accepts legacy formatted duration strings, current numeric millisecond values, and `null`. The type normalizes both non-null representations to seconds for gallery items while keeping all downstream gallery and transfer interfaces unchanged.

Malformed duration values remain decoding errors so unexpected API data is visible instead of being silently discarded.

## Compatibility

- Legacy Immich duration strings continue through the existing timestamp parser.
- Current Immich integer durations are interpreted as milliseconds according to `AssetResponseDto`.
- `null` and empty legacy values produce no gallery duration.

## Testing

Add regression coverage proving that polling a shared-link response with numeric duration succeeds. Extend gallery conversion coverage to assert equivalent seconds for numeric milliseconds and legacy timestamp strings, plus nullable duration behavior. Run the Immich package tests and the complete Go test suite before publishing.

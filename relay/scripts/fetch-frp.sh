#!/usr/bin/env bash
# fetch-frp.sh — download and verify the pinned FRP release for this platform.
#
# Downloads the exact FRP release recorded in relay/frp/manifest.json over
# HTTPS, verifies its SHA-256 against the pinned digest, and stages frps and
# frpc into a version+digest-keyed cache directory. Fails closed on any
# mismatch or malformed manifest value; never falls back to an unpinned
# "latest" release URL. FRP binaries are never committed to the repository —
# release packaging bundles executables verified through this script.
#
# Usage:   relay/scripts/fetch-frp.sh
#          relay/scripts/fetch-frp.sh --print-frps-sha256
# Stdout:  the staging directory containing frps and frpc (single line);
#          progress and diagnostics go to stderr. With --print-frps-sha256 the
#          single line is instead the manifest-pinned SHA-256 of the frps
#          executable, and with --print-pins it is
#          `<version> <platform-key> <tarball-sha256> <frps-sha256>`; both are
#          read from the committed manifest only and never create, read, or
#          trust the cache.
# Cache:   ${SHAREBRIDGE_FRP_CACHE:-<relay-root>/.cache/frp}
#            .../downloads/frp_<version>_<platform>.tar.gz   verified tarball
#            .../v<version>-sha256-<digest>/frps             staged server
#            .../v<version>-sha256-<digest>/frpc             staged client
#            .../v<version>-sha256-<digest>/.verified        digest marker
#          A cache hit is only taken after the staged tarball is re-hashed
#          against the pinned digest AND frps/frpc are re-extracted from that
#          verified tarball and required to match byte-for-byte (frps also
#          against the manifest frps_sha256 pin). The `.verified` marker is
#          written for compatibility but is never proof on its own.
# Exit:    0 on success (cache hit or fresh verified download), 1 on any
#          verification failure.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
relay_root="$(cd "${script_dir}/.." && pwd)"
manifest_path="${relay_root}/frp/manifest.json"
cache_root="${SHAREBRIDGE_FRP_CACHE:-${relay_root}/.cache/frp}"

log() { printf '%s\n' "$*" >&2; }
fail() { log "ERROR: $*"; exit 1; }

# --print-frps-sha256 is a hermetic query over the committed manifest: it must
# never create, read, or trust the cache.
print_frps_sha256=0
print_pins=0
case "${1:-}" in
  "") : ;;
  --print-frps-sha256) print_frps_sha256=1 ;;
  --print-pins) print_pins=1 ;;
  *) fail "unknown argument: ${1} (supported: --print-frps-sha256, --print-pins)" ;;
esac

# Map the running platform to a manifest artifact key.
case "$(uname -s)/$(uname -m)" in
  Darwin/arm64)  platform_key="darwin_arm64" ;;
  Linux/x86_64)  platform_key="linux_amd64" ;;
  Linux/aarch64) platform_key="linux_arm64" ;;
  *) fail "unsupported platform $(uname -s)/$(uname -m); the manifest pins darwin_arm64, linux_amd64, and linux_arm64" ;;
esac

[[ -f "$manifest_path" ]] || fail "pinned FRP manifest not found at ${manifest_path}"

# Extract this platform's pinned values from the manifest. The committed
# manifest uses a fixed one-key-per-line layout; the Go test suite parses the
# same file with encoding/json and enforces its full structure.
frp_version="$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest_path" | head -n 1)"
pin_status="$(sed -n 's/^[[:space:]]*"pin_status":[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest_path" | head -n 1)"
artifact_url="$(sed -n "/^[[:space:]]*\"${platform_key}\":/,/^[[:space:]]*}/ s/^[[:space:]]*\"url\":[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$manifest_path" | head -n 1)"
expected_digest="$(sed -n "/^[[:space:]]*\"${platform_key}\":/,/^[[:space:]]*}/ s/^[[:space:]]*\"sha256\":[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$manifest_path" | head -n 1)"
expected_frps_digest="$(sed -n "/^[[:space:]]*\"${platform_key}\":/,/^[[:space:]]*}/ s/^[[:space:]]*\"frps_sha256\":[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$manifest_path" | head -n 1)"

# Fail closed unless every pinned value is present and well formed, and the
# URL names the exact immutable release asset (no "latest" aliasing).
[[ "$frp_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "manifest version '${frp_version}' is not an immutable x.y.z release tag"
[[ "$pin_status" == "provisional-pending-user-approval" || "$pin_status" == "approved" ]] || fail "manifest pin_status '${pin_status}' is not in the allowed vocabulary"
[[ "$artifact_url" == https://* ]] || fail "manifest URL for ${platform_key} is not HTTPS: ${artifact_url}"
expected_asset_path="/releases/download/v${frp_version}/frp_${frp_version}_${platform_key}.tar.gz"
[[ "$artifact_url" == *"$expected_asset_path" ]] || fail "manifest URL for ${platform_key} does not name the exact pinned release asset: ${artifact_url}"
[[ "$expected_digest" =~ ^[0-9a-f]{64}$ ]] || fail "manifest SHA-256 for ${platform_key} is not 64 lowercase hex characters: ${expected_digest}"
[[ "$expected_frps_digest" =~ ^[0-9a-f]{64}$ ]] || fail "manifest frps_sha256 for ${platform_key} is not 64 lowercase hex characters: ${expected_frps_digest}"

# The hermetic manifest query stops here: no cache, no download.
if [[ "$print_frps_sha256" == "1" ]]; then
  printf '%s\n' "$expected_frps_digest"
  exit 0
fi
if [[ "$print_pins" == "1" ]]; then
  printf '%s %s %s %s\n' "$frp_version" "$platform_key" "$expected_digest" "$expected_frps_digest"
  exit 0
fi

# Resolve (and create) an absolute cache root.
mkdir -p -- "$cache_root" || fail "cannot create FRP cache directory ${cache_root}"
cache_root="$(cd -- "$cache_root" && pwd)" || fail "cannot resolve FRP cache directory ${cache_root}"

key_dir="${cache_root}/v${frp_version}-sha256-${expected_digest}"
downloads_dir="${cache_root}/downloads"
tarball_path="${downloads_dir}/frp_${frp_version}_${platform_key}.tar.gz"

# Prefer sha256sum (Linux) and fall back to shasum (macOS). Both print
# "<digest>  <file>"; awk selects the first field so either format parses.
if command -v sha256sum >/dev/null 2>&1; then
  digest_tool=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  digest_tool=(shasum -a 256)
else
  fail "neither sha256sum nor shasum is available to verify the download"
fi

digest_of() {
  "${digest_tool[@]}" "$1" | awk '{print $1}'
}

# verify_staged_binaries <key-dir> <tarball> — re-extract frps/frpc from the
# digest-verified tarball and require the staged copies to be byte-identical
# and executable, with frps matching the committed manifest pin. A `.verified`
# marker or an executable bit is never proof on its own, so a poisoned cache
# cannot be mistaken for a verified one.
verify_staged_binaries() {
  local key_dir="$1" tarball="$2" check_dir extracted ok=0
  [[ -x "${key_dir}/frps" && -x "${key_dir}/frpc" ]] || return 1
  [[ "$(digest_of "${key_dir}/frps")" == "$expected_frps_digest" ]] || return 1
  check_dir="$(mktemp -d)"
  if ! tar -xzf "$tarball" -C "$check_dir" 2>/dev/null; then
    rm -rf -- "$check_dir"
    return 1
  fi
  extracted="${check_dir}/frp_${frp_version}_${platform_key}"
  if [[ -f "${extracted}/frps" && -f "${extracted}/frpc" ]] \
    && cmp -s "${extracted}/frps" "${key_dir}/frps" \
    && cmp -s "${extracted}/frpc" "${key_dir}/frpc"; then
    ok=1
  fi
  rm -rf -- "$check_dir"
  [[ "$ok" == "1" ]]
}

# Cache hit: a previous run staged the executables into the version+digest-
# keyed directory. It is only a hit when the cached tarball still hashes to the
# pinned digest AND frps/frpc re-extract from it byte-identically with frps
# matching the committed manifest pin. The `.verified` marker alone is never
# accepted as proof.
if [[ -f "${tarball_path}" ]] \
  && [[ "$(digest_of "$tarball_path")" == "$expected_digest" ]] \
  && verify_staged_binaries "$key_dir" "$tarball_path"; then
  log "cache hit: ${key_dir} (FRP v${frp_version}, pin ${pin_status}; staged binaries re-verified against the pinned tarball)"
  printf '%s\n' "$key_dir"
  exit 0
fi

mkdir -p -- "$downloads_dir"

# Download over HTTPS only. The partial-file suffix keeps an interrupted run
# from being mistaken for a complete artifact; the rename below is atomic.
if [[ ! -f "$tarball_path" ]]; then
  log "downloading ${artifact_url}"
  curl --proto '=https' --tlsv1.2 --fail --location --retry 3 \
    --silent --show-error \
    --output "${tarball_path}.part" "$artifact_url" \
    || fail "download failed for ${artifact_url}"
  mv -- "${tarball_path}.part" "$tarball_path"
fi

# Verify against the pinned digest; fail closed and remove the artifact so a
# tampered or corrupt file can never be staged or reused.
actual_digest="$(digest_of "$tarball_path")"
if [[ "$actual_digest" != "$expected_digest" ]]; then
  rm -f -- "$tarball_path"
  fail "SHA-256 mismatch for ${tarball_path}: expected ${expected_digest}, got ${actual_digest}; the artifact was removed"
fi
log "verified ${tarball_path} (sha256 ${actual_digest})"

# Extract into a private staging directory, then publish the executables and
# the digest marker. An interrupted run leaves no marker and retries cleanly.
staging_dir="${cache_root}/.staging.${platform_key}.$$"
cleanup_staging() {
  if [[ -n "${staging_dir:-}" ]]; then
    rm -rf -- "$staging_dir"
  fi
}
trap cleanup_staging EXIT
rm -rf -- "$staging_dir"
mkdir -p -- "$staging_dir"
tar -xzf "$tarball_path" -C "$staging_dir" || fail "extracting ${tarball_path} failed"

shopt -s nullglob
archive_root_dirs=("${staging_dir}"/*/)
shopt -u nullglob
[[ ${#archive_root_dirs[@]} -eq 1 ]] || fail "expected exactly one top-level directory inside the FRP archive"
archive_root_dir="${archive_root_dirs[0]}"
[[ "$(basename "${archive_root_dir}")" == "frp_${frp_version}_${platform_key}" ]] \
  || fail "unexpected archive layout: top-level directory is $(basename "${archive_root_dir}")"
[[ -f "${archive_root_dir}/frps" && -f "${archive_root_dir}/frpc" ]] \
  || fail "archive does not contain both frps and frpc"

mkdir -p -- "$key_dir"
mv -- "${archive_root_dir}/frps" "${archive_root_dir}/frpc" "$key_dir/"
chmod 0755 "${key_dir}/frps" "${key_dir}/frpc"
[[ "$(digest_of "${key_dir}/frps")" == "$expected_frps_digest" ]] \
  || fail "staged frps has SHA-256 $(digest_of "${key_dir}/frps"); the manifest pins ${expected_frps_digest}"
printf '%s\n' "$expected_digest" > "${key_dir}/.verified"

log "staged ${key_dir}/frps and ${key_dir}/frpc (FRP v${frp_version}, pin ${pin_status})"
printf '%s\n' "$key_dir"

#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VENDOR_DIR="$ROOT_DIR/signaling-server/web/src/vendor"

UPSTREAM_REPO="${UPSTREAM_REPO:-https://raw.githubusercontent.com/jimmywarting/StreamSaver.js}"
UPSTREAM_SHA="${UPSTREAM_SHA:-5b372f7ba5f1e82ef80e2466023930ee2c616a28}"
SAFARI_REPO="${SAFARI_REPO:-https://raw.githubusercontent.com/camr0/StreamSaver.js}"
SAFARI_SHA="${SAFARI_SHA:-cfecfdda5e45bc1eecb0ce2c2fb0c420107a0a2e}"
HASH_WASM_REPO="${HASH_WASM_REPO:-https://cdn.jsdelivr.net/npm/hash-wasm@4.12.0/dist}"
HASH_WASM_SOURCE="${HASH_WASM_SOURCE:-https://github.com/Daninet/hash-wasm}"
HASH_WASM_SHA="${HASH_WASM_SHA:-373b796205ab55fb4a657374dad6ea589bf75815}"
HASH_WASM_PATH="${HASH_WASM_PATH:-index.esm.js}"

mkdir -p "$VENDOR_DIR"

fetch() {
  curl -fsSL "$1/$2/$3" -o "$4"
}

fetch_direct() {
  curl -fsSL "$1" -o "$2"
}

patch_generic_streamsaver() {
  perl -0pi -e 's/\|\| !!global\.safari \|\| !!global\.WebKitPoint//g' "$1"
}

patch_streamsaver_for_esm() {
  perl -0pi -e 's/: this\[name\] = definition\(\)/: globalThis[name] = definition()/g' "$1"
  perl -0pi -e "s/const global = typeof window === 'object' \? window : this/const global = typeof window === 'object' ? window : globalThis/g" "$1"
}

assert_contains() {
  local target_path="$1"
  local pattern="$2"

  if ! rg -q "$pattern" "$target_path"; then
    echo "expected pattern '$pattern' in $target_path" >&2
    exit 1
  fi
}

assert_not_contains() {
  local target_path="$1"
  local pattern="$2"

  if rg -q "$pattern" "$target_path"; then
    echo "unexpected pattern '$pattern' in $target_path" >&2
    exit 1
  fi
}

prepend_header() {
  local source_repo="$1"
  local source_sha="$2"
  local source_path="$3"
  local target_path="$4"
  local tmp_path

  tmp_path="$(mktemp)"
  if [[ "$target_path" == *.html ]]; then
    {
      printf '<!-- Vendored from %s at %s (%s). Updated via scripts/update_streamsaver_vendor.sh. -->\n' "$source_repo" "$source_sha" "$source_path"
      cat "$target_path"
    } > "$tmp_path"
  else
    {
      printf '/* Vendored from %s at %s (%s). Updated via scripts/update_streamsaver_vendor.sh. */\n' "$source_repo" "$source_sha" "$source_path"
      cat "$target_path"
    } > "$tmp_path"
  fi
  mv "$tmp_path" "$target_path"
}

fetch "$UPSTREAM_REPO" "$UPSTREAM_SHA" "StreamSaver.js" "$VENDOR_DIR/streamsaver.js"
fetch "$UPSTREAM_REPO" "$UPSTREAM_SHA" "sw.js" "$VENDOR_DIR/streamsaver-sw.js"
fetch "$UPSTREAM_REPO" "$UPSTREAM_SHA" "mitm.html" "$VENDOR_DIR/streamsaver-mitm.html"
fetch "$SAFARI_REPO" "$SAFARI_SHA" "StreamSaver.js" "$VENDOR_DIR/streamsaver-safari.js"
fetch "$SAFARI_REPO" "$SAFARI_SHA" "sw.js" "$VENDOR_DIR/streamsaver-safari-sw.js"
fetch "$SAFARI_REPO" "$SAFARI_SHA" "mitm.html" "$VENDOR_DIR/streamsaver-safari-mitm.html"
fetch_direct "$HASH_WASM_REPO/$HASH_WASM_PATH" "$VENDOR_DIR/hash-wasm.js"

patch_generic_streamsaver "$VENDOR_DIR/streamsaver.js"
patch_streamsaver_for_esm "$VENDOR_DIR/streamsaver.js"
patch_streamsaver_for_esm "$VENDOR_DIR/streamsaver-safari.js"

assert_not_contains "$VENDOR_DIR/streamsaver.js" 'global\.safari|WebKitPoint'
assert_contains "$VENDOR_DIR/streamsaver.js" 'globalThis\[name\] = definition\(\)'
assert_contains "$VENDOR_DIR/streamsaver-safari.js" 'globalThis\[name\] = definition\(\)'

prepend_header "$UPSTREAM_REPO" "$UPSTREAM_SHA" "StreamSaver.js" "$VENDOR_DIR/streamsaver.js"
prepend_header "$UPSTREAM_REPO" "$UPSTREAM_SHA" "sw.js" "$VENDOR_DIR/streamsaver-sw.js"
prepend_header "$UPSTREAM_REPO" "$UPSTREAM_SHA" "mitm.html" "$VENDOR_DIR/streamsaver-mitm.html"
prepend_header "$SAFARI_REPO" "$SAFARI_SHA" "StreamSaver.js" "$VENDOR_DIR/streamsaver-safari.js"
prepend_header "$SAFARI_REPO" "$SAFARI_SHA" "sw.js" "$VENDOR_DIR/streamsaver-safari-sw.js"
prepend_header "$SAFARI_REPO" "$SAFARI_SHA" "mitm.html" "$VENDOR_DIR/streamsaver-safari-mitm.html"
prepend_header "$HASH_WASM_SOURCE" "$HASH_WASM_SHA" "dist/$HASH_WASM_PATH from hash-wasm@4.12.0" "$VENDOR_DIR/hash-wasm.js"

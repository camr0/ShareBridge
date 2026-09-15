#!/bin/bash
# Apply a pion/sctp patch variant to ./forks/sctp, always restoring from the
# pristine copy first so variants cannot contaminate each other.
set -eu
cd "$(dirname "$0")"
FORK=forks/sctp
PRISTINE=forks/sctp-pristine

# Bootstrap the pristine copy from the module cache on first use, so the fork is
# regenerable and does not need to be committed.
if [ ! -d "$PRISTINE" ]; then
  SRC="$(go env GOMODCACHE)/github.com/pion/sctp@v1.9.4"
  if [ ! -d "$SRC" ]; then
    echo "missing module cache copy at $SRC" >&2
    exit 1
  fi
  mkdir -p forks
  cp -R "$SRC" "$PRISTINE"
  chmod -R u+w "$PRISTINE"
  echo "bootstrapped $PRISTINE" >&2
fi

restore() {
  rm -rf "$FORK"
  cp -R "$PRISTINE" "$FORK"
}

# pion hardcodes rtoMin = 1000ms (rtx_timer.go). Linux TCP's floor is 200ms.
# lowering rtoMin alone keeps rtoMax at 60s, so exponential backoff still works
# -- unlike the rtoMax hack tested earlier, which capped backoff too.
patch_rtomin() {
  sed -i '' 's/^\trtoMin float64 = 1\.0 \* 1000$/\trtoMin float64 = 200.0/' "$FORK/rtx_timer.go"
}

# pion initialises ssthresh to the peer's rwnd (~5MB from Chrome), so slow start
# blows past the bottleneck buffer. Cap it instead. NOTE: after slow start,
# congestion avoidance only adds ~1 MTU per RTT, so too small a value is worse.
patch_ssthresh() {
  sed -i '' "s/^\ta\.ssthresh = a\.RWND()$/\ta.ssthresh = ${1:-256 * 1024}/" "$FORK/association.go"
}

case "${1:-none}" in
  none)     restore ;;
  rtomin)   restore; patch_rtomin ;;
  ssthresh) restore; patch_ssthresh ;;
  ssth768)  restore; patch_ssthresh "768 * 1024" ;;
  both)     restore; patch_rtomin; patch_ssthresh ;;
  *) echo "unknown variant: $1 (want none|rtomin|ssthresh|ssth768|both)" >&2; exit 2 ;;
esac

echo "variant=$1"
echo -n "  rtoMin:   "; grep -n 'rtoMin float64' "$FORK/rtx_timer.go" | head -1
echo -n "  rtoMax:   "; grep -n 'defaultRTOMax float64' "$FORK/rtx_timer.go" | head -1
echo    "  ssthresh: $(grep -cE 'a\.ssthresh = [0-9]+ \* 1024' "$FORK/association.go") line(s) patched"

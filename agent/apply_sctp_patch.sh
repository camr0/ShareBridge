#!/bin/bash
# Apply a pion/sctp patch variant to ./forks/sctp, always restoring from the
# pristine copy first so variants cannot contaminate each other.
set -eu
cd "$(dirname "$0")"
FORK=forks/sctp
PRISTINE=forks/sctp-pristine
# Underscore prefix so the Go tool ignores these directories: their files are
# package sctp and reference fork internals, so they only compile once copied
# into forks/sctp.
BBRDIR=forks/_bbr
TRACEDIR=forks/_trace

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

# BBR-lite: cap cwnd at a measured BDP instead of slow-starting toward rwnd.
patch_bbr() {
  cp "$BBRDIR/bbr.go" "$FORK/bbr.go"
  # Insert the sampler/clamp call as the first statement of the cumulative-ACK
  # handler (whose contract already requires the association lock).
  perl -0pi -e 's/(func \(a \*Association\) onCumulativeTSNAckPointAdvanced\(totalBytesAcked int\) \{\n)/$1\ta.bbrOnAck(totalBytesAcked)\n/' "$FORK/association.go"
}

# Send-side instrumentation (E35): ~1 Hz JSONL of cwnd/ssthresh/rwnd/outstanding
# plus the RTO-vs-fast-retransmit split. Inert unless SB_SCTP_TRACE is set --
# startTrace() returns after one env lookup when the switch is off.
patch_trace() {
  cp "$TRACEDIR/sctp_trace.go" "$FORK/sctp_trace.go"
  cp "$TRACEDIR/sctp_trace_test.go" "$FORK/sctp_trace_test.go"
  # Start the emitter at the end of association construction, after every
  # queue/timer exists but before any packet can be sent (no lock is held here).
  perl -0pi -e 's/(\tassoc\.ackTimer = newAckTimer\(assoc\)\n)\n(\treturn assoc\n)/$1\n\tassoc.startTrace()\n\n$2/' "$FORK/association.go"
  grep -q 'assoc.startTrace()' "$FORK/association.go" || {
    echo "trace hook insertion failed" >&2
    exit 1
  }
}

# Variants compose with '+', e.g. `rtomin+trace` or `bbr+trace`. The tree is
# restored from pristine first, so components always apply to a clean fork.
spec="${1:-none}"
restore
traced=no
IFS='+' read -r -a parts <<< "$spec" || true
for part in "${parts[@]}"; do
  case "$part" in
    none)     ;;
    rtomin)   patch_rtomin ;;
    ssthresh) patch_ssthresh ;;
    ssth768)  patch_ssthresh "768 * 1024" ;;
    bbr)      patch_bbr ;;
    both)     patch_rtomin; patch_ssthresh ;;  # legacy alias for rtomin+ssthresh
    trace)    patch_trace; traced=yes ;;
    *) echo "unknown variant component: $part (want none|rtomin|ssthresh|ssth768|bbr|both|trace, composed with +)" >&2; exit 2 ;;
  esac
done

# Wire the local fork into the module graph. This is deliberately NOT committed in
# go.mod: forks/sctp is generated (and gitignored), so a committed replace would
# break `go build` for anyone who has not run this script first.
if [ "$spec" = "none" ]; then
  go mod edit -dropreplace github.com/pion/sctp
else
  go mod edit -replace github.com/pion/sctp=./forks/sctp
fi

echo "variant=$spec"
echo -n "  rtoMin:   "; grep -n 'rtoMin float64' "$FORK/rtx_timer.go" | head -1
echo -n "  rtoMax:   "; grep -n 'defaultRTOMax float64' "$FORK/rtx_timer.go" | head -1
echo    "  ssthresh: $(grep -cE 'a\.ssthresh = [0-9]+ \* 1024' "$FORK/association.go") line(s) patched"
echo    "  bbr hook: $(grep -c 'a.bbrOnAck(totalBytesAcked)' "$FORK/association.go") line(s), bbr.go present: $([ -f "$FORK/bbr.go" ] && echo yes || echo no)"
echo    "  trace:    $traced (hook $(grep -c 'assoc.startTrace()' "$FORK/association.go") line(s), sctp_trace.go present: $([ -f "$FORK/sctp_trace.go" ] && echo yes || echo no))"

# Experiment 44 — client-side wiring: recover the EXP28 `?bare=1` receiver, and wire the browser for N5 (2026-10-13)

**Status: both tasks delivered as source + tests. Nothing was deployed. No field cell, no lab harness,
no `docker build`, no `git`, no contact with VERSA / TESTBOX / CLIENT-EAST / CLIENT-WEST.**

**Verdict.**
* **Task 1 (unconditional) — done.** The EXP28 bare receiver now exists in the repo as a guarded,
  permanently-inert code path (`?bare=1`), with a deploy/revert script that prints a
  `sha256/size/mode/owner` manifest before and after and refuses a revert that does not reproduce the
  recorded pre-deploy state. A local end-to-end self-test (deploy → verify → revert → diff) passes.
* **Task 2 (budget allowed) — done.** The client accepts `bulk-*` N5 lanes when the agent offers them,
  feeds v2-enveloped chunks through `bulkReorder.js` in global sequence order, and is unchanged when the
  agent offers nothing (proved by the untouched 274-test baseline plus targeted tests).
* **Not done:** no browser was driven and no field wiring was exercised against a real agent; the N5 path
  is proven only by unit/integration tests against synthetic frames. See §5.

---

## 1. Task 1 — the recovered EXP28 bare receiver

### 1.1 What was recovered

`2026-09-18-exp28-bare-receive-path.md` §Patch describes a temporary hand-patch to TESTBOX's test copy of
`/opt/sharebridge-test/web/src/app.js` that was reverted byte-for-byte and existed nowhere in the repo or
on the box. Its three jobs are now a permanent code path:

| EXP28 job | Where it lives now |
|---|---|
| 1. flag the mode; expose `globalThis.__bare = { bytes, frames, header, startedAt, endedAt }` | `signaling-server/web/src/bareReceive.js` (`installBareReceive()`), called once at module scope of `app.js` |
| 2. replace the bulk-lane `onmessage` with `bytes += event.data.byteLength; frames += 1` — no `decodeBinaryEnvelope`, no `routeOrBufferFrame`, no chunk assembly, no early-frame buffer, no sink, no SHA-1, no `concatBytes`, no DOM work | `app.js:attachTransferChannel` — `onBulkFrame()` counts and returns before any decode/route |
| 3. early-return from `handleControlMessage` for `file_header` / `chunk_end` so no download operation, pipeline or sink is ever created | `app.js:handleControlMessage` — the two types are recorded/dropped immediately after `JSON.parse` |

Everything else is the real client, unchanged: the signalling socket, join/knock, offer/answer, ICE/DTLS,
the `control`/`media`/`bulk` lane set, `file_list` → `renderFileList` → click → `file_request`, the relay
standby, the `Connected (Direct)` badge, and the control lane's real handler.

Two deliberate, documented extensions over E28's exact bytes:
* the bare counter is wired to **every** bulk lane (including any N5 `bulk-*` lane) through the same
  subscription hook the normal path uses — with a single lane this is identical to E28;
* the implied-envelope size is overridable (`?bare=1&bare_env=22`) because a striped lane emits 22-byte v2
  envelopes, where E28's gate constant `14` would be wrong. Default is 14.

### 1.2 Files

| path | state | sha256 (this build) | bytes |
|---|---|---:|---:|
| `signaling-server/web/src/bareReceive.js` | **new** | `83cb881e733a8ec3159adfd1151f16fcc4bea4dd07e9e2dee182a4e4bd25cf52` | 4,291 |
| `signaling-server/web/src/bareReceive.test.js` | **new** | — | — |
| `signaling-server/web/src/bareReceiveMode.test.js` | **new** | — | — |
| `signaling-server/web/src/app.js` | modified | `10b53ec90e2a3728556e5edce06130b95fd7d93582ef814e7414549983987706` | 94,811 |
| `agent/deploy_bare_client.sh` | **new** | — | 14 KB |
| `agent/deploy_bare_client.selftest.sh` | **new** | — | 4.5 KB |

`app.js` before this experiment was `407ee7032f90e4298ee6b0416ed69dbf94147d0c6ff0933e7c6803f129bdf3c1`
(91,308 B) — the exact hash E28 recorded pre-patch and post-restore. **It is no longer that hash**, because
the recovered path is now permanent source. A revert on TESTBOX restores whatever was there from the
backup, verified by hash, independent of the repo's current bytes.

### 1.3 Inertness — how it is proved

* `installBareReceive()` returns `null` when `location.search` has no truthy `bare` parameter, and then
  writes nothing to `globalThis`.
* Every other touch point is a single falsy test: `if (bareReceive) { … return }` in the bulk handler, and
  `if (bareReceive && (msg.type === 'file_header' || msg.type === 'chunk_end'))` in `handleControlMessage`.
* `bareReceiveMode.test.js` loads a second, cache-distinct instance of `app.js` with a stubbed
  `globalThis.location`, once **without** `?bare=1` (asserts `getBareReceive() === null` and no
  `globalThis.__bare`) and once **with** it (asserts the counter shape, that two synthetic bulk frames add
  up to `bytes=1614 frames=2`, that `file_header` sets `header` without creating a download, and that
  `chunk_end` completes nothing).
* The original 274 tests all still pass, and exactly one of them was edited in place (the
  `binaryEnvelope` version-rejection case, for the new v2 error ordering); none of them sets `?bare=1`.

### 1.4 Byte accounting — the real node test output

`node --test src/bareReceive.test.js src/bareReceiveMode.test.js` (from
`.worktrees/benchdirect/signaling-server/web`):

```
✔ bare mode is enabled only by an explicit truthy bare parameter (0.699625ms)
✔ an absent bare parameter produces no receiver and no global (0.084458ms)
✔ bare mode publishes the EXP28 counter shape and counts wire bytes only (0.384083ms)
✔ markHeader retains only the framing fields the driver reports (0.075459ms)
✔ the default envelope size agrees with the wire envelope module (0.041875ms)
✔ synthetic frames reproduce the field gate exactly (uniform 16 KiB chunking) (48.17075ms)
✔ synthetic frames reproduce E28 B-A1 exactly (mixed chunk sizes) (43.299458ms)
✔ a single missing frame fails the gate (the gate is not vacuous) (41.316584ms)
✔ a v2 (striped) envelope size is selectable and changes the implied payload (0.328208ms)
✔ without ?bare=1 the app installs no bare receiver at all (8.048667ms)
✔ ?bare=1 exposes the EXP28 counter, counts bulk frames, and ignores the framing control messages (5.76025ms)
```

The gate is exercised from synthetic frames only, in two shapes:

* **uniform** — 48,256 × (16,384 B payload + 14 B envelope) = `wire 791,301,888`,
  `wire − 14 × frames = 790,626,304` exactly;
* **E28 B-A1** — 48,255 full 16 KiB payloads + 8 × 2 KiB tails = `48,263` frames and
  `wire 791,301,986`, which is the **exact wire total E28 logged** (`BARE-ACCOUNTING B-A1 … wire=791301986`),
  and again `wire − 14 × frames = 790,626,304`;
* a **negative control** drops one frame and asserts the gate no longer reaches 790,626,304.

No agent, no browser, no field cell, no `timeout` (absent on macOS).

### 1.5 Deploy / revert

The script never hardcodes the host (runbook rule 5). Read the TESTBOX IP from
`.worktrees/e2e-v1/INFRA.local.md`:

```bash
cd .worktrees/benchdirect/agent

# what is there now
SB_TESTBOX_HOST=ubuntu@<TESTBOX-IP> ./deploy_bare_client.sh manifest bare

# deploy the bare receiver (default fileset), printing the manifest before/after
SB_TESTBOX_HOST=ubuntu@<TESTBOX-IP> ./deploy_bare_client.sh deploy bare

# land the client back exactly as it was (newest session, or pass the id the deploy printed)
SB_TESTBOX_HOST=ubuntu@<TESTBOX-IP> ./deploy_bare_client.sh revert
```

Filesets: `bare` = `src/app.js src/bareReceive.js src/bulkReorder.js`
(`bulkReorder.js` is included **only** because `app.js` imports it statically and TESTBOX's test web
root is not known to carry it (E42 never deployed it) — without it the module graph fails and the page
never loads);
`n5`/`all` = `bare` + `src/binaryEnvelope.js src/channelSet.js src/directChannel.js`.

Each `deploy`:
1. hashes the affected files on the box **before** anything is written and prints the manifest;
2. `cp -p`s them into `$SB_BACKUP_ROOT/<session>/` and writes `manifest.before` + `fileset` beside them;
3. `scp`s the repo files to `/tmp/sb-bare-stage/<session>/` and `sudo install -m 644 -o root -g root` them
   into `$SB_WEB_ROOT`;
4. re-hashes and prints the manifest **after**;
5. asserts every deployed file equals the repo byte-for-byte, and (unless `SB_SKIP_HTTP_VERIFY=1`)
   re-fetches each file over HTTP from `http://127.0.0.1:8080/src/…` on the box and hashes the served body;
6. prints `DEPLOY-OK session=…` plus the exact revert command. Any mismatch prints `DEPLOY-FAIL`.

Each `revert` restores `manifest.before` file-by-file (deleting any file that was `ABSENT` before the
deploy), re-hashes, and prints `REVERT-OK` only if the post-revert manifest sorts identically to the
recorded pre-deploy manifest — otherwise `REVERT-MISMATCH` and a non-zero exit.

**Nothing was deployed to TESTBOX by this experiment.** `SB_LOCAL_SIM=1` runs the same remote halves on
this machine, which is how the round trip is proven.

### 1.6 Self-test output (real)

`agent/deploy_bare_client.selftest.sh` builds a throwaway web root with sentinel "pristine" files and no
`bareReceive.js`, runs `deploy all`, compares against the repo, reverts, and diffs the whole tree:

```
=== deploy all (simulated, no network) ===
DEPLOY-OK session=20261013T025227Z-all
SELFTEST ok: all six deployed files are byte-identical to the repo
=== revert ===
REVERT-OK session=20261013T025227Z-all — post-revert manifest is identical to the recorded pre-deploy manifest
SELFTEST ok: post-revert manifest is byte-identical to the pre-deploy manifest
=== revert of the newest session (no explicit id) ===
SELFTEST ok: newest-session revert also round-trips
=== bare fileset deploys app.js + its modules, not the N5 extras ===
SELFTEST ok: bare fileset left binaryEnvelope.js untouched
SELFTEST ok: bare fileset left directChannel.js untouched
=== a module missing from the web root round-trips too ===
SELFTEST ok: a module absent before the deploy is removed again by the revert
SELFTEST PASS
```

(Full log: `/tmp/exp44-selftest.txt`, exit 0, 8 `SELFTEST ok` lines.)

---

## 2. Task 2 — wiring the browser for N5 (`SB_BULK_LANES`)

### 2.1 What the agent already does (E42, unchanged here)

With `SB_BULK_LANES=K` the agent creates `bulk`, `bulk-1`, … `bulk-(K-1)` ordered DataChannels
(`agent/internal/peer/peer.go:107`), round-robins chunks across them and rewrites each 14-byte v1 chunk
frame as a 22-byte **v2** frame carrying a global uint64 sequence
(`agent/internal/transfer/bulkstripe.go`, `agent/internal/bulkfan/envelope.go`). Those Go packages were
**not touched** — they are the previous build agent's and frozen.

### 2.2 Client changes

| file | change |
|---|---|
| `src/channelSet.js` | `attach()` accepts `bulk-<n>` (n ≥ 1) as an **extra** lane: never required, never blocking the handshake, never able to satisfy a missing required lane. New `bulkLanes` getter (primary first, extras sorted numerically) and an `onbulklane` hook. Extra lanes are closed with the rest of the set. |
| `src/directChannel.js` | extra lanes map to `LANE_BULK`; `subscribeExtraBulkLane(handler)` installs the handler as the `onmessage` of every current **and future** extra lane (closing the race where a lane opens after the set is already ready); `bulkLanes` / `extraBulkLanes` getters; `bufferedAmount` now sums control + media + all bulk lanes. The primary lane is never rewired. |
| `src/binaryEnvelope.js` | `decodeBinaryEnvelope` accepts envelope version `0x02` and returns `sequence` (BigInt) plus `envelopeVersion: 2`; v1 frames still have **no** `sequence` property at all, which is the discriminator. New `encodeChunkEnvelopeV2`, and exported `CHUNK_ENVELOPE_BYTES`/`CHUNK_ENVELOPE_V2_BYTES`. Length is still checked before the version byte, matching `bulkfan.DecodeChunkFrame`. |
| `src/app.js` | `attachTransferChannel` subscribes the bulk handler to extra lanes when the lane set offers the hook (`channel.subscribeExtraBulkLane?.(onBulkFrame)` — a relay channel or an un-updated set simply does not have it). `handleBulkMessage` now routes through `ingestBulkFrame`: a frame **without** `sequence` takes the original direct path; a frame **with** it goes into a `ReorderBuffer` and only the newly consecutive run is passed to `routeOrBufferFrame`. `bufferEarlyFrame` refuses to coalesce sequence-carrying frames (coalescing would destroy the chunk→sequence mapping); `retireOperation('bulk', …)` discards the buffer. |
| `src/bulkReorder.js` | unchanged (E42's module, now actually wired in). |

Ordering authority is a single point — the reorder buffer at arrival — so chunks that arrive before their
`file_header` land in the early-frame store already in sequence order and are replayed directly, with no
double-push.

### 2.3 "Exactly as today when the agent offers nothing" — how it is proved

1. With one `bulk` lane the agent stamps **v1** frames, which decode to `sequence === undefined`, so
   `ingestBulkFrame` calls `routeOrBufferFrame('bulk', frame)` exactly as before — asserted by
   `v1 frames never engage the striped reorder path (default client unchanged)`.
2. `subscribeExtraBulkLane` is never called on a lane set that lacks it
   (`a lane set without the extra-lane hook still uses the primary bulk lane`).
3. `bulkLanes` on a peer with no extras is exactly `[bulk]`, and the primary lane's `onmessage` is never
   reassigned (`channelSet.test.js`, `directChannel.test.js`).
4. **273 of the 274 pre-existing tests are byte-unchanged and all 274 pass** (one was edited in place
   for the new v2 error ordering), which is the strongest available statement that the default path did
   not move.

### 2.4 Test results (real output)

```
$ cd .worktrees/benchdirect/signaling-server/web
$ node --test src/*.test.js
…
ℹ tests 305
ℹ suites 0
ℹ pass 305
ℹ fail 0
ℹ cancelled 0
ℹ skipped 0
ℹ todo 0
ℹ duration_ms 636.252959
```

274 (E42's count) + 31 new = 305. The 31 new tests: 9 in `bareReceive.test.js`, 2 in
`bareReceiveMode.test.js`, 6 in `channelSet.test.js`, 2 in `directChannel.test.js`, 7 in `app.test.js`,
2 in `n5ClientWiring.test.js`, and 3 in `binaryEnvelope.test.js`. The two N5 end-to-end tests read:

```
✔ extra bulk lanes offered by the agent are wired to the same handler (3.971875ms)
✔ a lane set without the extra-lane hook still uses the primary bulk lane (0.571125ms)
```

The first delivers `bulk-1` chunks with sequences 1 then 0 and asserts the pipeline sees `[1,2]` then
`[9]`, then a v1 frame on the primary lane still appends directly — i.e. both paths coexist in one session.

Three E42-era tests had to change behaviour, and two of them changed expectation deliberately:
`binaryEnvelope.test.js`'s "rejects … unsupported file chunk envelopes" asserted that a 14-byte version-2
frame is rejected as an unsupported version; it is now rejected as **truncated** (the v2 length check), with
a new test covering a genuine unknown version byte (`0x03`). The two `app.test.js` failures that appeared
mid-build (`too short` vs `version` for a 3-byte frame) were resolved in the module, not the test, by
restoring the length-before-version check order.

---

## 3. What a field agent must do

**To run any `?bare=1` arm (E28 reproduction, E41 C1/C2 arms):**
1. Read the TESTBOX IP from `.worktrees/e2e-v1/INFRA.local.md`.
2. `SB_TESTBOX_HOST=ubuntu@<TESTBOX-IP> agent/deploy_bare_client.sh deploy bare` → require `DEPLOY-OK`.
3. Drive with `SHARE_URL="https://TESTBOX/s/<code>?bare=1"` (`drive_bare.js` / `drive_bare_ff.js` do
   **not** add the parameter). The gate is unchanged: `wire − 14 × frames == 790,626,304`.
4. `SB_TESTBOX_HOST=ubuntu@<TESTBOX-IP> agent/deploy_bare_client.sh revert` → require `REVERT-OK`.
   Do this before any non-bare cell; a bare client never downloads a file.

**To run an N5 arm (`SB_BULK_LANES=K`):**
1. Rebuild the agent from the `benchdirect` worktree with E42's flags (do **not** overwrite
   `sb-agent:pristine`), then recreate `sb-run` with `-e SB_BULK_LANES=4`.
2. `SB_TESTBOX_HOST=ubuntu@<TESTBOX-IP> agent/deploy_bare_client.sh deploy n5` → require `DEPLOY-OK`.
   The client now accepts the extra lanes, so the channel set no longer fails on `bulk-1`.
3. Run the cell normally (`drive.js` / `drive_click.js` — no query parameter needed) and revert afterwards.
4. Sanity check the agent log for `DataChannel lanes ready` after all `K` lanes are open; the browser side
   has no independent signal, so a mismatch would show up as an incomplete transfer rather than an error.

Both filesets are independently reversible; `deploy bare` twice and reverting the newest session restores
the state before the second deploy, so keep one session per change.

---

## 4. Caveats

1. **Nothing was measured.** No throughput number, no field cell, no browser run. Both tasks are
   build + unit-test results.
2. **The deploy script was never executed against TESTBOX** — that was out of scope by instruction. It is
   proven by `bash -n` plus a full `SB_LOCAL_SIM=1` deploy→verify→revert round trip on this machine. The
   only unexercised parts are `ssh`/`scp` transport, `sudo install -o root -g root`, and the HTTP
   served-bytes check.
3. **N5 correctness rests on synthetic frames.** No real `bulk-*` lane has ever delivered a chunk to this
   client. Specifically untested: SCTP lane-open ordering on a real association, behaviour when one lane
   stalls long enough to exceed `DEFAULT_REORDER_LIMIT` (16,384 pending chunks ≈ 256 MiB at 16 KiB), and the
   interaction with `shouldDeferEnd` when `chunk_end` arrives before the last buffered chunk (unit-reasoned
   via the existing deferral path, not exercised).
4. **Residual ordering window.** During `drainEarlyFrames` the control chain replays early-buffered frames
   with `await`s; a chunk arriving on another lane in that window is released and appended immediately. The
   same window exists in the unstriped path today (arrival order is the append order there); with striping
   it is bounded by the ≤128-frame early store. Not closed here.
5. **A1 (`SB_BULK_CONNS`) is still not client-workable** and was not attempted: it needs peer-creation
   fan-out, capability negotiation and browser fan-in over N `RTCPeerConnection`s (E42 §6a–d).
6. `deploy_bare_client.sh` assumes the TESTBOX ssh user has passwordless `sudo`; pass `SB_REMOTE_SUDO=` for
   a root login. The backup lives under `/tmp` on the box, so a reboot between deploy and revert loses it —
   prefer short-lived sessions, as the campaign does.
7. The repo's `app.js` is now permanently different from the 2026-09-19 pristine hash
   `407ee703…`. Any future script that asserts byte-identity with that hash must be updated; the
   deploy/revert path does not depend on it.

## 5. Artifacts

* This file.
* `signaling-server/web/src/{bareReceive,app,binaryEnvelope,channelSet,directChannel}.js` and the new
  `bareReceive.test.js`, `bareReceiveMode.test.js`, `n5ClientWiring.test.js` (+ additions to
  `binaryEnvelope.test.js`, `channelSet.test.js`, `directChannel.test.js`, `app.test.js`).
* `agent/deploy_bare_client.sh`, `agent/deploy_bare_client.selftest.sh`.
* `/tmp/exp44-client-tests.txt` (full `node --test src/*.test.js` output),
  `/tmp/exp44-selftest.txt` (deploy/revert self-test, exit 0).
* No repository state changed (no mutating git command); the orchestrator commits.

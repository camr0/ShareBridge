# Experiment 10b — v1 arms (salvaged) and the v2-phase failure — 2026-09-18

> **CONFOUND (found 2026-09-18, F18).** The v1 arms here are legitimate **v1** measurements (app JS receive
> path), but any comparison against E10/E10c's v2 arms compares **transport and client implementation
> together**: those v2 numbers came from the browser's **native HTTP download** (`Content-Disposition:
> attachment`, zero page-JS), not from a JS receive path. Do not quote v1-vs-v2 ratios from these files as a
> pure transport result. See `ACTIONABLE-FINDINGS-2026-09-18.md` F18.

**Status:** this agent died on a provider usage limit (`openai-codex`) after ~135 tool calls and ~11
minutes, and wrote **no results file**. Its four v1 cells are recovered from the v1 agent's own log
(`sb-run`, UTC timestamps) and are valid measurements. Its v2 phase produced **no measurement**. The
numbers below are transcribed by the orchestrator from the agent log, not from the (absent) agent
report.

## v1 arms — recovered and valid

Window = first `DataChannel lanes ready` → `download complete`. Each cell transferred
754 MiB = 790,626,304 B = 6,325.01 Mb.

| cell | window (UTC) | window s | Mbps |
|---|---|---|---|
| CLIENT-EAST v1 rep1 | 07:32:02 → 07:32:59 | 57 | **110.97** |
| CLIENT-EAST v1 rep2 | 07:33:53 → 07:34:55 | 62 | **102.02** |
| CLIENT-WEST v1 rep1 | 07:35:45 → 07:37:30 | 105 | **60.24** |
| CLIENT-WEST v1 rep2 | 07:38:12 → 07:39:57 | 105 | **60.24** |

Cross-checks: E10 measured CLIENT-EAST v1 = **110.97 Mbps** (identical to rep1 here), and Exp 9
measured CLIENT-WEST v1 = **61.4 Mbps**. So v1 is reproducible:

- **v1 CLIENT-EAST: 102–111 Mbps** (three cells across two independent runs)
- **v1 CLIENT-WEST: 60.2–61.4 Mbps** (three cells across two runs; two of them identical to 0.1%)

Against the ~246 Mbps UDP path capacity, that is **~45% and ~25%** of the path respectively. Neither
number is limited by the client (Exp 9/E8b showed the hosts are not the shared constraint) nor by the
path (E8c's UDP ladder).

## v2 phase — no measurement (failure recorded)

The v2 units were started at 07:40:58 UTC and applied their route snapshot (11 routes, 0 dropped) at
07:41:03. At 07:41:58 the gateway closed a public connection:

```
gateway: closed public connection reason=route-lookup remote=… sni=<v1 signalling host>
error="routes: lookup \"<v1 signalling host>\": routes: no such route"
```

i.e. the client was directed at the **v1 share URL** while the v2 gateway was serving, and the v2
gateway has no route for the v1 signalling SNI. The gateway was then stopped at 07:42:12 and the agent
died on its provider limit shortly afterwards. **No v2 byte counter moved in this experiment.**

The correct method (used successfully by E10 for its 233.285 Mbps cell) is to fetch from the **relay
origin URL returned by the control API's `prepare-route`**, not from the v1 share URL. This is now
spelled out in the brief for the follow-up run.

## Rig restoration (performed by the orchestrator after the agent died)

The agent died mid-switch with the rig stranded on v2. The orchestrator restored and verified the v1
rig:

- TESTBOX: `caddy=active`, `sharebridge-test=active`, `sharebridge-relay-gateway=inactive`,
  `sharebridge-relay-frps=inactive`, `sharebridge=inactive`; v1 signalling HTTP **200**.
- VERSA: `sb-run` **up** (`sb-agent:pristine`), `sharebridge-agent-test` stopped; production
  `sharebridge-agent` (port 7878) observed up and never touched.
- `sb-run` env verified after restoration: `UI_PORT=7879`, `UI_ADDR=127.0.0.1`,
  `SB_SCTP_CA_STEP=32768` — matches the documented rig spec, so its cells are on the intended tuning.

## Lesson (now runbook rule 13)

**Write the results file incrementally, after every cell.** ~135 tool calls of measurement were very
nearly lost because the file was written only at the end. Two agents died on provider rate limits in
this session; a third (this one) had to have its data reconstructed from a container log.

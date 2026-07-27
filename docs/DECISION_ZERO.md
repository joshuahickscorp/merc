# Decision Zero — realtime lane: keep or kill

**Status: OPEN. Owner's call.** Everything in the remediation plan that does not
depend on this decision is done. This record exists so the call can be made from
evidence rather than from momentum, and so either branch is cheap to execute.

## What was tested

Two independent arguments were commissioned, each briefed to make the strongest
possible case for one branch and to engage the other side's steelman. Neither
was told which way anyone else leaned.

| Branch | Confidence | Driving fact |
|---|---:|---|
| `[KILL-RT]` | **78%** | Production identity is Apple Silicon + Candle only; realtime has never executed on a real GPU. |
| `[KEEP-RT]` | **72%** | Zero customers plus a batch registry that admits only Apple Silicon means realtime is the only designed path to general supply and default buyer integration. |

**Six points of confidence is not a verdict.** Two careful readings of the same
codebase landed on opposite conclusions with overlapping intervals. That is the
signal: this is not an engineering question with a hidden right answer, it is a
business-intent question, and the evidence genuinely underdetermines it.

## What both sides agree on

Worth separating from the contested part, because these are not in dispute:

1. **The lane must stop being a ghost.** 3,390 lines across
   `control/realtime*.go` and `agent/src/vllm.rs` are untracked — never in any
   CI run, never reviewed, and unrecoverable if deleted. The KEEP brief calls
   this "a process failure"; the KILL brief says archive before any purge. Both
   want it out of this state before anything else happens.
2. **Stop dual-tracking.** Whichever lane loses should stop receiving
   engineering attention immediately. The expensive failure mode is maintaining
   both.
3. **Deleting the bytes is a separate decision from stopping the lane.** In the
   KILL brief's own words: *"the case for stop-the-lane is stronger than the case
   for destroy-the-bytes-without-archive."*

A checksummed snapshot of all eight files is preserved outside the repository at
`realtime-lane-snapshot/` with a `MANIFEST.sha256`, so `[KILL-RT]` is now a
reversible operation rather than permanent loss.

## The question that actually resolves it

Not *"which code is better"* — both briefs agree the realtime money and protocol
work is substantial and the batch lane is the only thing that has run on
admissible hardware.

The question is: **within 90 days, can you get a Linux/CUDA supplier to register
and serve real traffic?**

- **Yes** → `[KEEP-RT]`. The realtime lane is the only path to that supply, the
  hard money and protocol work already exists, and OpenAI compatibility is the
  default integration buyers already have.
- **No** → `[KILL-RT]`. Selling a latency SLA on machines the control plane
  returns HTTP 400 for is not a product, and the 8–12 day Batch API port buys a
  buyer on-ramp for supply that actually exists.

Nobody but the owner can answer that, which is why the plan assigned this
decision here and why it stays open.

## Cost of each branch

| | `[KILL-RT]` | `[KEEP-RT]` |
|---|---|---|
| Immediate | delete 3,390 lines (snapshot preserved) | `git add` the lane, put it in CI |
| Next spend | Batch API port, **8–12 days** — a port, not a revert: `api.go` moved 2,111 lines and `store.go` 4,681 since `89267633` | one Linux CUDA host, real vLLM parity evidence, ~2 days + rental |
| Then | demote streaming in `EXECUTION_NETWORK_BIBLE.md` and the site | widen `validHWClasses` / `validEngines` beyond Apple + `candle` |
| Residual risk | Apple-only supply may never form; canary still prohibits independent suppliers | commodity chat competes on price with hyperscalers; cash rails unproven |

## What is already true regardless

The money holes that made `[KEEP-RT]` dangerous are closed in the working tree:
buyers are charged for realtime usage, the saved-card free-inference
short-circuit is gone, every ledger write goes through one integer-micro writer,
and sequence reservation is atomic and proven under concurrency. Whichever way
this goes, those were worth doing — they are not sunk cost in the losing branch.

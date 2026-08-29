# Qualifying 24-hour soak (Level B/C)

`evidence/external/qualifying-soak-24h.json` is the Level B/C endurance
receipt. It is worth 3 readiness points only when
`scripts/validate-readiness.py::qualifying_24h_soak_proven` accepts it.
That checker requires a schema-v2 `go_closure_soak` `PASS` after a real
86400-second window, an immutable candidate image, and independent
re-validation of the raw sample stream. Until those conditions hold, the
receipt is refused and the 3 points stay unearned.

This file describes the HTTPS observer that is running against the
persistent staging plane at `https://mercmerc.net`. The observer is how
this lane starts the 24-hour clock honestly. It is not a substitute for
`scripts/go-closure-soak.sh` and it does not write `status: PASS` or
`qualifies_for_24h_gate: true`.

The derived one-hour backend-alpha soak
(`evidence/external/qualifying-soak-alpha.json`) is a different receipt
and is already closed. Do not overwrite it.

## What is running

The observer lives in `scripts/soak/soak24.py`. It:

- GETs `https://mercmerc.net/version` and `https://mercmerc.net/readyz`
  on a fixed 60-second cadence
- records the deployed commit and `payment_mode` on every sample
- writes raw rows to `evidence/external/qualifying-soak-24h-samples.jsonl`
- rewrites the receipt from those rows (elapsed = wall clock now minus
  the real `started_at`; never a fabricated duration)
- stays restartable: `start` / `resume` reuse the original `started_at`
  and append, they do not reset the clock
- treats a mid-window `/version` commit change as `CANDIDATE_CHANGED`
  (`candidate.changed: true`, `continuity: broken_redeploy`) rather than
  pretending one candidate ran the whole window
- treats `payment_mode != test` or `live_value_movement != false` as
  `POLICY_LEFT_TEST`

It does not inspect docker, Prometheus, or Metal agents, so it cannot
satisfy the go-closure assertions the 3-point gate demands. That is
intentional. An in-progress receipt is the correct document; a receipt
that claims the 24-hour gate before 86400 real seconds have elapsed is
the one unacceptable outcome.

## Commands

From the repository root. The sampler is detached in tmux session
`merc-qualifying-soak-24h` (nohup fallback if tmux is missing).

```bash
# start, or resume the same run if state already exists
python3 scripts/soak/soak24.py start

# progress: real start, elapsed seconds, sample count, candidate, pid
python3 scripts/soak/soak24.py status

# rewrite the receipt from the sample stream without taking a new probe
python3 scripts/soak/soak24.py stamp

# restart the sampler against the existing started_at / samples
python3 scripts/soak/soak24.py resume

# refused until elapsed >= 86400; never writes PASS
python3 scripts/soak/soak24.py finish
```

`finish` exits non-zero while the window is still open. Use `status` or
`stamp` for an in-progress receipt.

Offline checks (no network):

```bash
python3 scripts/soak/test_soak24.py
```

Confirm the Level B gate still refuses the in-progress receipt:

```bash
python3 scripts/validate-readiness.py
# expect: evidence/external/qualifying-soak-24h.json CHECK_FAILED → 0/3
# expect: Level B still 87/100, P1=5, backend alpha 85/91
```

## Receipt rules

- `status` is one of `IN_PROGRESS`, `CANDIDATE_CHANGED`,
  `POLICY_LEFT_TEST`, `OBSERVED_WINDOW_COMPLETE`, `FAILED`.
- `status` is never `PASS`.
- `qualification.qualifies_for_24h_gate` is always `false`.
- `finished_at` is omitted until the requested 86400 seconds have
  actually elapsed on the wall clock.
- `duration.elapsed_seconds` is derived from `now - started_at`.
- `duration.observed_window_seconds` is last sample minus first sample.
- A parallel-lane redeploy must show up as `candidate.changed: true`.

Until the 24-hour soak has passed the go-closure checker, readiness must
keep those 3 points at zero.

## Relationship to P1-RECOVERY-SOAK

`docs/ALPHA_GATE_PLAN.md` still names the supervisor start for the
alpha-gate soak:

```bash
export MERC_ALPHA_SOAK_I_AM_THE_SUPERVISOR=1
scripts/alpha/soak.sh --execute --duration 86400 --interval 60
```

and the isolated go-closure alternative
`scripts/go-closure-soak.sh --target ssh --duration 86400 --interval 60 --execute`.
Those remain the paths that can eventually produce a schema-v2
`go_closure_soak` document. This observer is the persistent-staging
HTTPS clock that can run now, from this Mac, without SSH and without
claiming that later bar.

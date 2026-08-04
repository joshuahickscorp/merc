# REFACTOR_PLAN.md — Aggressive reorganization and deep compression of `merc`

**Status:** executable plan only. No moves, merges, or deletions have been performed yet.

**Worktree:** `/Users/scammermike/Downloads/merc/.worktrees/teardown`  
**Branch:** `refactor/teardown`  
**Base commit:** `ff441d1e37a29cc83f571c36e7dc05cbecf295db` (exact copy of `main`)  
**Plan authoring date:** 2026-08-04  

**Hard rules for every later execution run:**

- Stay inside this worktree. Never touch `/Users/scammermike/Downloads/merc`.
- Do not push, merge to other branches, deploy, or modify remotes.
- Every phase ends with a known-good tree that a reviewer can ship or stop after.
- Proof-machinery honesty beats cosmetic directory counts.
- Documentation consolidation is **no-loss merge**, never summary.

---

## Corrections to the recon

Measurements re-derived in this worktree with a real shell. Where they differ from the task contract, **this section is authoritative**.

| Claim in contract | Re-derived value | Notes |
|---|---|---|
| Tracked top-level dirs **25** | **22** tracked (`.githooks`, `.github`, `.tools` + 19 product) | Recon's 25 almost certainly counted six names **not present** here: `docker/`, `pass3_bundle/`, `build/`, `dist/`, `render/`, `review/` (19+6=25). |
| Product target **9** dirs from **25** | **19 product → 9 product** | Dot-dirs stay and do not count against the under-10 product budget. |
| `control/*.go` total LOC **166435** | **166335** | Δ −100. Python line counts over `git ls-files 'control/*.go'`. |
| LOC histogram 300–499:**83**, 500–999:**75** | **84**, **74** | Adjacent-bin drift of 1; totals still 463 files. |
| `api.go` **4798** LOC | **4797** | |
| `pass3_bundle/` empty, delete | **Already absent** | No tracked files, no directory. Phase is a no-op. |
| `docker/` → `ops/docker/` | **No `docker/` directory exists** | Only root `Dockerfile.control` + `docker-compose*.yml` (stay at root). Phase absorbs **`monitoring/` only** into `ops/`. |
| `review/` ~13M untracked | **Absent in this worktree** | Procedure retained for when data appears. Do not invent or delete it. |
| `worktreeContentDigest` at `dev_checkpoint.go:132-156` | Function starts at **line 122** (body through 156) | Same function; line anchor drifted. |
| `sourceFingerprint` at `evidence.go:148-175` | Function starts at **line 106**; path framing loop **148–175** matches | |
| CODEOWNERS globs expand into the 94-set | **No** | Expanding `admin*.go`/`auth*.go`/`payout*.go`/`stripe*.go` yields **114** basenames. Contract arithmetic (71 named scripts + … → 94, free 370) matches the **non-expanded** union. |

**Stale basename (the one already dead):** `ledger.go`  
Present in `.github/CODEOWNERS` as `/control/ledger.go`. No `control/ledger.go` on disk. Live successor is `control/ledger_write.go` (453 LOC). The 94-set is **93 existing + 1 stale**.

**None** of the five path-into-proof bindings were found mis-described in substance.

---

## 0. Measurement evidence (commands and outputs)

All counts produced in this worktree on 2026-08-04.

### 0.1 Inventory

```
$ git rev-parse HEAD
ff441d1e37a29cc83f571c36e7dc05cbecf295db

$ git branch --show-current
refactor/teardown

$ git ls-files | wc -l
    1008

$ git ls-files | awk -F/ 'NF>1{print $1}' | sort -u | wc -l
      22

$ git ls-files | awk -F/ 'NF>1{print $1}' | sort -u
.githooks
.github
.tools
agent
artifacts
bench
benchmarks
census
cli
control
docs
evidence
logo
macapp
monitoring
ops
pricing
proof
proto
scripts
sdk
web

$ git ls-files 'control/*.go' | wc -l
     463
$ git ls-files 'control/*.go' | grep -v '_test\.go$' | wc -l
     204
$ git ls-files 'control/*.go' | grep '_test\.go$' | wc -l
     259
$ git ls-files '*.md' | wc -l
      67
```

### 0.2 `control/` LOC (Python recount)

```
total files: 463
total LOC: 166335
prod files: 204 LOC: 90990
test files: 259 LOC: 75345
  0-99: 82
  100-299: 196
  300-499: 84
  500-999: 74
  1000-1999: 24
  2000-3999: 2
  4000-+: 1
```

Largest production files:

| LOC | File |
|----:|---|
| 4797 | `control/api.go` |
| 2405 | `control/realtime_store.go` |
| 1973 | `control/gateway_parity_harness.go` |
| 1829 | `control/store.go` |
| 1825 | `control/pricing_decision.go` |
| 1659 | `control/service_leases.go` |
| 1659 | `control/realtime.go` |
| 1616 | `control/store_jobs.go` |
| 1389 | `control/store_payouts.go` |
| 1389 | `control/quote.go` |
| 1386 | `control/workers.go` |
| 1315 | `control/scheduler.go` |
| 1286 | `control/store_tasks.go` |
| 1247 | `control/gateway_parity_matrix.go` |
| 1226 | `control/enrollment.go` |

### 0.3 Special markers

```
//go:embed:
  control/store.go:60,62           schema.sql (duplicate directive, one var)
  control/runtime_authority.go:17  runtime-authority.json
  control/runtime_authority.go:913 evidence-manifest.json
  control/realtime_profiles.go:62  runtime-profiles/*.json

func init():
  control/planner.go:13
  control/pricing.go:226
  control/scheduler.go:32

TestMain:
  control/currency_test.go:18

build-tagged .go: 0
generated .go (Code generated / DO NOT EDIT): 0
```

### 0.4 Load-bearing basename union → 94

| Guard | Count | Source |
|---|---:|---|
| MUTATIONS basenames | 27 unique (84 entries) | `scripts/mutation-test.sh` |
| `moneyAndAdmissionAuthorityFiles` | 22 | `control/authority_callgraph_test.go:205-212` (mirrored in `plan_calibration_test.go`) |
| `os.ReadFile("<name>.go")` | 6 unique / 7 call sites | listed below |
| `control/<name>.go` in scripts + Makefile + `.github/` | 71 unique | `rg -o 'control/[A-Za-z0-9_]+\.go'` |
| CODEOWNERS **literal** `.go` paths (no glob expansion) | 9 | `.github/CODEOWNERS` |
| **Union** | **94** | 93 on disk + **`ledger.go` STALE** |

`os.ReadFile` call sites:

```
control/directed_enrollment_test.go:98       os.ReadFile("scheduler.go")
control/exact_reuse_authority_test.go:79     os.ReadFile("exact_reuse_batch.go")
control/prefix_routing_wiring_test.go:477    os.ReadFile("workers.go")
control/prefix_placement_test.go:86          os.ReadFile("prefix_placement.go")
control/reuse_not_physical_metric_test.go:23 os.ReadFile("realtime.go")
control/release_artifact_test.go:37          os.ReadFile("api.go")
control/workload_simulation_test.go:442      os.ReadFile("api.go")
```

```
prod  204 = 71 named + 133 free
test  259 = 22 named + 237 free
              93 named existing + 370 free
              + ledger.go stale = 94 load-bearing names
```

### 0.5 Directory move reference pressure

`rg -n -F '<prefix>'` excluding files under that top-level directory. **Caveat:**
substring false positives are common (`benchmarks/` matches `evidence/benchmarks/`
and `runtime-benchmarks/`; `artifacts/` matches `.artifacts/`). True by-hand lists in §3 filter these.

| Prefix | Tracked files | External files (raw) | External lines (raw) |
|---|---:|---:|---:|
| `sdk/` | 10 | 11 | 29 |
| `cli/` | 2 | 6 | 8 |
| `macapp/` | 2 | 13 | 16 |
| `proto/` | 3 | 8 | 11 |
| `proof/` | 1 | 4 | 4 |
| `census/` | 3 | 6 | 6 |
| `artifacts/` | 1 | 35 (mostly false+) | 52 (mostly false+) |
| `bench/` | 4 | 15 | 32 |
| `benchmarks/` | 1 | 46 (mostly false+) | 187 (mostly false+) |
| `monitoring/` | 7 | 17 | 46 |
| `logo/` | 5 | 6 | 14 |

### 0.6 Markdown inventory

67 tracked `.md`, **10303** total LOC. Per-file LOC + `git log -1 --format=%ad|%h|%s --date=short`:

```
43	CHANGELOG.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
27	EXECUTION_NETWORK_BIBLE.md	2026-07-29|47c369a3|read the eta calibration table back into the next quote
193	README.md	2026-08-03|38653d5d|docs: 84 of 100 is the ceiling this machine can reach, and say why
19	RELEASE_GATES.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
112	RELEASE_READINESS.md	2026-08-03|64d41d64|docs: say which cited receipts are unbound, and flag a cost authority that rests on a withdrawn one
51	REQUIREMENT_PROOF_MATRIX.md	2026-07-29|47c369a3|read the eta calibration table back into the next quote
50	ROADMAP.md	2026-07-28|26ff44fb|docs: drop roadmap items already shipped, correct two figures
45	RUNBOOK_ARTIFACTS.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
21	RUNBOOK_WORKER_FAILURE.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
49	census/CODEBASE_CENSUS.md	2026-08-02|a0b15db5|Advance network programme rendering and CAD economics proof
13	cli/README.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
135	docs/ACCEPTABLE_USE_AND_ABUSE_RESPONSE.md	2026-07-19|abac2461|release: close private canary code gates
67	docs/BENCH_PROFILES_2026-07-27.md	2026-08-03|ab4d2877|docs: say what the evidence supports, and stop quoting numbers nobody can reproduce
290	docs/CANARY_DRIVER_FINDINGS.md	2026-07-28|447ef1bc|record that the canary noop is correct behaviour, not a defect
136	docs/CANARY_SCENARIO_DRIVER.md	2026-07-28|1ae6bfa6|add the go-closure canary scenario driver (UNREVIEWED - do not merge)
192	docs/CANARY_TERMS.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
148	docs/CAPACITY_LEASES.md	2026-08-02|00364e20|feat: a bounded prepaid envelope, so admission stops re-proving funding per request
151	docs/CUDA_CHAIN_RUNBOOK.md	2026-08-03|64d41d64|docs: say which cited receipts are unbound, and flag a cost authority that rests on a withdrawn one
77	docs/DECISION_ZERO.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
47	docs/DECISION_ZERO_OUTCOME.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
112	docs/DECISION_ZERO_REVERSAL.md	2026-07-27|37ea6c22|multi-GPU: workers declare topology, merc does not trust it
19	docs/DEPENDENCY_REVIEW.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
272	docs/DEPLOY_MERC_DROPLET.md	2026-08-01|4223abf4|fix(economics): allocate fixed costs across charge batches
131	docs/DSAR_RUNBOOK.md	2026-08-03|64d41d64|docs: say which cited receipts are unbound, and flag a cost authority that rests on a withdrawn one
309	docs/EXECUTION_FRONTIER_LANE.md	2026-08-03|64d41d64|docs: say which cited receipts are unbound, and flag a cost authority that rests on a withdrawn one
128	docs/EXTERNAL_ACTION_QUEUE.md	2026-08-03|64d41d64|docs: say which cited receipts are unbound, and flag a cost authority that rests on a withdrawn one
36	docs/EXTERNAL_OPERATOR_HANDOFF.md	2026-07-28|2ec16884|bind release authority to exact evidence chain
481	docs/FACET_EXTERNAL_ACTION_PACK.md	2026-08-03|64d41d64|docs: say which cited receipts are unbound, and flag a cost authority that rests on a withdrawn one
26	docs/FRONTEND_CONTRACT.md	2026-07-28|8488f3ef|bind pricing authority end to end
211	docs/FRONTIER_300X.md	2026-08-03|ab4d2877|docs: say what the evidence supports, and stop quoting numbers nobody can reproduce
241	docs/HOT_PATH_DURABLE_ADMISSION.md	2026-08-03|fb88c24d|chore: stamp this wave's own receipts, and stop one from inventing a status
104	docs/LANE_RESEARCH.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
58	docs/LEVEL_B_OPERATOR_HANDOFF.md	2026-08-03|38653d5d|docs: 84 of 100 is the ceiling this machine can reach, and say why
540	docs/MASTER_PROGRAMME_LEDGER.md	2026-08-03|64d41d64|docs: say which cited receipts are unbound, and flag a cost authority that rests on a withdrawn one
20	docs/MEDIA_RENDERING_CONTRACT.md	2026-08-02|a0b15db5|Advance network programme rendering and CAD economics proof
17	docs/MEDIA_TRANSCODE_CONTRACT.md	2026-08-02|a0b15db5|Advance network programme rendering and CAD economics proof
959	docs/MERC_SHIPPABILITY_DIRECTIVE.md	2026-08-03|06f5d478|fix: two routes served buyer traffic without ever being reviewed
169	docs/MONEY_AUDIT_2026-07-27.md	2026-07-28|ab026140|make processor fee allocation deterministic and durable
120	docs/OFFER_MULTIPLICITY.md	2026-08-03|84795c25|docs: the single-hot-offer tail is a fixture artefact, not a production defect
188	docs/PATH_TO_TEN.md	2026-07-28|6877f795|refresh release evidence integrity
53	docs/POSTGRES_TRUST_BOUNDARY.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
95	docs/PRIVACY_DATA_GOVERNANCE.md	2026-07-19|abac2461|release: close private canary code gates
101	docs/PRIVACY_NOTICE_DRAFT.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
179	docs/QUICKSTART.md	2026-07-28|8488f3ef|bind pricing authority end to end
175	docs/RENAME_REGISTER.md	2026-07-28|8a9a6a92|rename every process, binary and service off cx
217	docs/RUNBOOKS.md	2026-07-28|8a9a6a92|rename every process, binary and service off cx
77	docs/RUNPOD_ESCALATION.md	2026-07-29|edb3b7bb|correct the pre-push overclaims and close a secret-ignore hole
236	docs/RUNTIME_CROSS_TEST_2026-07-30.md	2026-07-30|33b1b1da|perf: llama.cpp clears the embed cosine gate; next blocker is wire_kind
49	docs/RUNTIME_MATRIX.md	2026-08-02|fdc8eec1|fix: a cell stayed routable on a receipt nobody could reproduce
99	docs/SECURITY.md	2026-08-02|a2dbb86a|Expose bounded project compiler route
383	docs/SHIPPABILITY_STATUS.md	2026-08-03|64d41d64|docs: say which cited receipts are unbound, and flag a cost authority that rests on a withdrawn one
104	docs/SPEED_LANE_2026-07-27.md	2026-08-03|ab4d2877|docs: say what the evidence supports, and stop quoting numbers nobody can reproduce
272	docs/STAGING_DEPLOYMENT_PLAN.md	2026-07-29|9ec45e6d|warn that the verification sample secret is immutable too
143	docs/STRIPE_CONNECT.md	2026-07-28|bd7a44f6|bind catalogue pricing to atomic FX authority
80	docs/STRIPE_SANDBOX_SETUP.md	2026-07-28|677166d9|prove Stripe webhook application outcomes
25	docs/SUPPLY_CHAIN_POLICY.md	2026-07-20|87a81857|release: complete autonomous nonexternal closure
133	docs/SUPPORT_AND_INCIDENT_RUNBOOK.md	2026-07-27|ded18b54|rename: zero-residue audit, RESIDUE 0, gated in CI
95	docs/THIRD_PARTY_LICENSES.md	2026-08-02|a0b15db5|Advance network programme rendering and CAD economics proof
279	docs/WORKLOAD_IR_V1.md	2026-08-02|a0b15db5|Advance network programme rendering and CAD economics proof
259	docs/YOUR_10_ACTIONS.md	2026-07-28|6877f795|refresh release evidence integrity
425	docs/runtime/ADAPTIVE_EXECUTION.md	2026-08-03|64d41d64|docs: say which cited receipts are unbound, and flag a cost authority that rests on a withdrawn one
79	docs/runtime/RUNTIME_GGUF_CLOSURE_BASELINE.md	2026-07-30|b834b43e|docs: record the runtime GGUF closure baseline before modifying anything
74	docs/runtime/SECOND_RUNTIME_CENSUS.md	2026-07-30|c0cf0936|perf: bind resolved artifacts into the profile digest, and survive the bump
236	docs/runtime/TRANCHE_STATUS.md	2026-08-03|64d41d64|docs: say which cited receipts are unbound, and flag a cost authority that rests on a withdrawn one
103	monitoring/README.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
272	ops/staging/README.md	2026-07-30|9fe7e55b|add confirmed data-preserving release teardown
53	sdk/python/README.md	2026-07-27|8156ce1a|supplier console: earnings, payout rail and verification standing
```

---

## 1. The end state

### 1.1 Target tree (9 tracked product directories)

```
merc/
├── agent/      Rust worker agent                         (path unchanged)
├── clients/    NEW ← sdk/ + cli/ + macapp/ + proto/
├── control/    Go control plane                         (path unchanged; compressed within)
├── docs/       documentation corpus                     (compressed; + root mergeables absorbed)
├── evidence/   proof corpus ← proof/ + census/ + artifacts/
│               + bench/ + benchmarks/ under NON-colliding names
├── ops/        operations ← + monitoring/   (no docker/ exists to move)
├── pricing/    live price board                         (path unchanged)
├── scripts/    automation                               (path unchanged)
├── web/        site ← + logo/
└── .local/     GITIGNORED only ← intended home for build/ dist/ render/ review/
                when those trees exist locally (none present in this worktree now)
```

**Root files that stay at root** (do not count against the directory budget):

`README.md`, `CHANGELOG.md`, `LICENSE`, `NOTICE`, `Makefile`, `.gitignore`,
`.dockerignore`, `.env.example`, `.merc-launch.env.template`,
`Dockerfile.control`, `docker-compose.yml`, `docker-compose.prod.yml`,
`docker-compose.observability.yml`, `Caddyfile`, `RELEASE_READINESS.md`.

`RELEASE_READINESS.md` is pinned by `control/evidence.go:24-25`
(`isGeneratedReleaseEvidencePath`). Keep at root rather than moving.

**Deleted:** `pass3_bundle/` — already gone (no-op).

**Dot-dirs retained:** `.githooks/`, `.github/`, `.tools/`.

### 1.2 Arithmetic

| Metric | Before | After (recommended) | After (max / Wave B) |
|---|---:|---:|---:|
| Tracked product top-level dirs | 19 | **9** | 9 |
| All tracked top-level dirs (incl. dot) | 22 | 12 | 12 |
| Tracked files | 1008 | **~700–760** | **~650–720** |
| `control/*.go` | 463 | **~160** (free-only aggressive) | **~120–140** |
| Tracked `*.md` | 67 | **~18** (≈14 under `docs/` + ≈4 root) | same |

**Recommended path:** directory reorg + doc merge + **free-only** `control/` merges.
Named/load-bearing merges are optional Wave B with much higher review cost.

| Wave | Files removed (approx.) |
|---|---:|
| Directory moves | 0 (rename only) |
| Doc consolidation | ~45–50 |
| `control/` free-only aggressive pack | ~300 (133→~31 prod free, 237→~36 test free; named 93 stay) |
| **Total** | **~1008 → ~700–760** |

### 1.3 What each surviving directory is for

| Dir | Role |
|---|---|
| `agent/` | Rust worker agent, sandbox, bench subcommands |
| `clients/` | Buyer/operator clients: Python/TS SDKs, CLI, macOS app, wire schemas |
| `control/` | Go module root (`package main`), API, money, scheduling, evidence issuance |
| `docs/` | Human documentation + legal/policy instruments |
| `evidence/` | Proof artifacts, census, claims policy, former root bench fixtures |
| `ops/` | Governance JSON, systemd, staging compose, observability (`monitoring/`) |
| `pricing/` | Live `board.json` catalogue input for control boot |
| `scripts/` | CI, deploy, validation, mutation, canary drivers |
| `web/` | Public site + brand assets (from `logo/`) |

---

## 2. Phase list

**Commit granularity:** one phase = one commit (or a short stack sharing one
phase gate), always leaving local gates green or documenting deferred gates.
Phases are independently revertible via `git revert`.

| ID | Name | Depends | Parallel | Touches | Must NOT touch |
|---|---|---|---|---|---|
| P0 | Baseline inventory freeze | — | — | this plan only (done) | everything else |
| P1 | `.gitignore` → `.local/` mapping | P0 | — | `.gitignore` | tracked content paths |
| P2 | Create `clients/`; move sdk, cli, macapp, proto | P1 | P3, P4 | those trees + refs | money files; cosign workflow |
| P3 | `monitoring/` → `ops/monitoring/` | P1 | P2, P4 | monitoring + compose/scripts | `docs/RUNBOOKS.md` anchors |
| P4 | `logo/` → `web/logo/` | P1 | P2, P3 | logo + provenance | URL `/logo/*` in Caddyfile |
| P5 | `proof/`, `census/`, `artifacts/` → `evidence/` | P1 | after P2–P4 preferred | those dirs + hardcodes | historical receipt *meanings* |
| P6 | root `bench/` + `benchmarks/` non-colliding | P5 | — | bench fixtures + tests | `evidence/bench/`, `evidence/benchmarks/` |
| P7 | Documentation consolidation (no-loss) | path-stable | — | docs + root mergeables | 8 legal paths; SECURITY; SUPPORT; RUNBOOKS anchors |
| P8 | `control/` free-file compression | P7 optional | not with P9 | free `.go` only | 93 named; `main.go`; embed hosts; `api.go` |
| P9 | Optional named merges + guards | P8 | — | named files + guards | money×non-money merges |
| P10 | Proof regeneration | P2–P9 | — | census, checkpoint, bindings | do not rewrite signed history |
| P11 | Completeness proof | P10 | — | read-only checks | product behaviour |

### P1 — `.gitignore` rewrite

Map local/build/untracked product paths into `.local/` conventions; keep sacred
ignores (`models/`, `*.gguf`, etc.). Full rule map in §7.4.

**Gate:**

```bash
git check-ignore -v agent/target/ control/merc .artifacts/ evidence/checkpoint/ \
  docs/speed-lane-reports/ .serena/ .local/review/ || true
# each intended ignore must still match
```

**Commit:** one commit `chore(ignore): map local artifacts under .local/`.

### P2 — `clients/` assembly

```bash
mkdir -p clients
git mv sdk clients/sdk
git mv cli clients/cli
git mv macapp clients/macapp
git mv proto clients/proto
# then rewrite refs (§3, §4) in the SAME commit
```

**Gate:**

```bash
test ! -e sdk && test -d clients/sdk
bash scripts/verify-python-sdk-package.sh
python3 -m json.tool clients/proto/manifest.schema.json >/dev/null
python3 scripts/validate-claim-surfaces.py
```

**Commit:** one commit (move + all ref updates + claim-policy). Never split.

### P3 — `monitoring/` → `ops/monitoring/`

```bash
git mv monitoring ops/monitoring
# rewrite compose, scripts, claim-policy active_surfaces
```

**Gate:**

```bash
test -f ops/monitoring/alerts.yml
node scripts/validate-observability.mjs
rg -o 'docs/RUNBOOKS.md#[a-zA-Z0-9_-]+' ops/monitoring/alerts.yml | sort -u
# confirm each anchor heading still exists in docs/RUNBOOKS.md
```

**Commit:** one commit.

### P4 — `logo/` → `web/logo/`

```bash
git mv logo web/logo
# update ops/asset-provenance.json paths (same sha256)
# update scripts/rename-residue-audit.py FROZEN_PATHS
# Caddyfile @immutable path /logo/* is a URL — do NOT rewrite to /web/logo/*
```

**Gate:**

```bash
python3 scripts/validate-governance.py
python3 - <<'PY'
import json,hashlib,pathlib
p=json.load(open('ops/asset-provenance.json'))
for a in p['assets']:
    path=pathlib.Path(a['path'])
    if path.exists():
        h=hashlib.sha256(path.read_bytes()).hexdigest()
        assert h==a['sha256'], (a['path'], h, a['sha256'])
print('provenance ok')
PY
```

**Commit:** one commit.

### P5 — proof/census/artifacts → evidence/

| From | To |
|---|---|
| `proof/claims/claim-policy.json` | `evidence/proof/claims/claim-policy.json` |
| `census/CODEBASE_CENSUS.{json,md}` | `evidence/census/CODEBASE_CENSUS.{json,md}` |
| `artifacts/runtime/...` | `evidence/artifacts/runtime/...` |

**By-hand hardcodes (same commit):**

- `control/evidence.go:24-25` — `isGeneratedReleaseEvidencePath`
- `control/audit.go:508` — prose path in generated census md
- `scripts/validate-claim-surfaces.py:17` — `POLICY = ROOT / "proof" / ...`
- `scripts/validate-repo-boundary.py:37` — open census JSON
- `scripts/rename-residue-audit.py` — `FROZEN_PATHS` `census/` → `evidence/census/`

**Gate:**

```bash
test -f evidence/proof/claims/claim-policy.json
test -f evidence/census/CODEBASE_CENSUS.json
python3 scripts/validate-claim-surfaces.py
python3 scripts/validate-repo-boundary.py
```

**Commit:** one commit.

### P6 — non-colliding bench moves

| From | To | Why |
|---|---|---|
| `bench/immutable/` | `evidence/immutable-fixtures/` | Must not use `evidence/bench/` (exists: quality-suite/runs jsonl, different data) |
| `benchmarks/realtime-chat-v1.json` | `evidence/workload-catalog/realtime-chat-v1.json` | Must not use `evidence/benchmarks/` (live pricing authorities; `control/pricing.go:47-55`; `Dockerfile.control` COPY) |

**Gate:**

```bash
test ! -e bench && test ! -e benchmarks
test -f evidence/immutable-fixtures/shared-prefix-v1.json
test -f evidence/workload-catalog/realtime-chat-v1.json
python3 scripts/test-bench-accounting.py
rg -n 'evidence/benchmarks/2026-07-01-m3-pro' control/pricing.go Dockerfile.control
```

**Commit:** one commit.

### P7 — Documentation consolidation

See §6. **Commits:** prefer one commit per target merged document (bisectable),
or one atomic commit if the no-loss tool covers the whole set.

**Gate:**

```bash
python3 scripts/docs_noloss_check.py   # introduced in P7; see §6.3
python3 scripts/validate-governance.py
test -f docs/SECURITY.md
test -f docs/SUPPORT_AND_INCIDENT_RUNBOOK.md
test -f docs/RUNBOOKS.md
```

### P8 — `control/` free-file compression

Only files **not** in the 94-set. Never merge test into prod. `main.go` alone.
`api.go` absorbs nothing. Embed hosts stay valid.

**Gate:**

```bash
cd control && go test -count=1 -timeout 45m ./...
test $(git ls-files 'control/*.go' | wc -l) -lt 350
```

**Commits:** one per merge group / domain cluster.

### P9 — Optional named merges

Only with §5.4 guard matrices filled. Money files only merge with money files;
re-derive `moneyAndAdmissionAuthorityFiles` from domain membership in the same
commit.

**Gate:** full local `make ci` subset + mutation suite if money touched.

### P10 — Proof regeneration

See §7. Regenerates census, checkpoint, binding inventories. Does **not**
rewrite old signed image identities.

```bash
cd control && go run . dev checkpoint
python3 scripts/validate-evidence-binding.py
```

### P11 — Completeness

See §9.

---

## 3. Per-move breakage tables

**Mechanical** = literal path rewritable by ordered regex. **By hand** =
computed or dual-meaning paths. By-hand lists aim to be exhaustive at `file:line`.

### 3.1 `sdk/` → `clients/sdk/`

**Mechanical:** `README.md`, `LICENSE`, `NOTICE` path tables; docs mentions;
census keys (prefer regenerate in P10).

**By hand:**

| Site | Why |
|---|---|
| `scripts/sdk-live-python.py:13` | `sys.path.insert(0, "sdk/python")` |
| `scripts/verify-python-sdk-package.sh:11,23,26` | `$ROOT/sdk/python` copies and pytest roots |
| `scripts/sdk-live-typescript.mjs:9` | `import … from "../sdk/typescript/dist/index.js"` |
| `.gitignore` | `sdk/python/build|dist|egg-info`, `sdk/typescript/dist|node_modules` |
| `Makefile` targets referencing `sdk/` | re-`rg -n 'sdk/' Makefile` at execution |

### 3.2 `cli/` → `clients/cli/`

**Mechanical:** `LICENSE`, `NOTICE`, census keys.

**By hand:**

| Site | Why |
|---|---|
| `scripts/build-cli-release.sh:47` | `cp "$ROOT/cli/README.md"` |
| claim-policy `active_surfaces` | `cli/README.md` entry |

### 3.3 `macapp/` → `clients/macapp/`

**By hand:**

| Site | Why |
|---|---|
| `control/two_agent_enrollment_test.go:68` | `filepath.Join("..", "macapp", ...)` |
| `control/containment_identity_test.go:21-22` | dual `macapp/...` candidates |
| `scripts/install.sh:236` | `find` path `*/macapp/ComputeExchangeAgent/merc-agent.sb` |
| `scripts/package-agent-binary.sh:33` | `$ROOT/macapp/...` |
| `scripts/test-agent-package-contains-seatbelt.sh:9,56` | profile path + error string |
| `scripts/local-production-rehearsal.sh:270` | `MERC_SANDBOX_PROFILE=$ROOT/macapp/...` |
| `scripts/local-resilience-rehearsal.sh:217` | same |
| `scripts/prove-local.sh:275` | `cp macapp/ComputeExchangeAgent/merc-agent.sb` |
| `scripts/validate-claim-surfaces.py:77` | default `required_path` fallback |
| claim-policy capability `required_path` | `macapp/ComputeExchangeAgent/Package.swift` |
| `scripts/rename-residue-audit.py:61,143` | `FROZEN_PATHS` / messages |
| `.github/CODEOWNERS` | `/macapp/...` globs |
| `.gitignore` | `macapp/.build/` |

### 3.4 `proto/` → `clients/proto/`

**Mechanical:** README/LICENSE/NOTICE/docs/census.

**By hand:**

| Site | Why |
|---|---|
| `Makefile` `ci` target | `python3 -m json.tool proto/manifest.schema.json` |
| loaders of manifest schema | re-`rg -n 'proto/manifest|proto/enrollment'` at execution |

### 3.5 `proof/` → `evidence/proof/`

| Site | Why |
|---|---|
| `scripts/validate-claim-surfaces.py:17` | `POLICY = ROOT / "proof" / "claims" / "claim-policy.json"` |
| claim-policy internal path strings | updated with other surface moves |

**Not a path move:** `scripts/prove-local.sh:249` SQL value `'proof/input'` is a
job input label — do not rewrite.

### 3.6 `census/` → `evidence/census/`

| Site | Why |
|---|---|
| `control/evidence.go:24` | `isGeneratedReleaseEvidencePath` |
| `control/audit.go:508` | writes path into generated markdown |
| `scripts/validate-repo-boundary.py:37` | `open("census/CODEBASE_CENSUS.json")` |
| `scripts/rename-residue-audit.py` `FROZEN_PATHS` | `("census/", ...)` |
| census self-keys | regenerate in P10 |

### 3.7 `artifacts/` → `evidence/artifacts/`

True filesystem refs are few (mostly census). Never rewrite `.artifacts/` or
`evidence/canary/artifacts/`.

### 3.8 `bench/` → `evidence/immutable-fixtures/`

| Site | Why |
|---|---|
| `scripts/test-bench-accounting.py:158` | `Path("bench/immutable/shared-prefix-v1.json")` |
| census keys | regenerate |

**Do not rewrite:** `merc-bench` UA strings, `cargo bench`, `agent-bench`,
`docs/bench-local-reports/`, `evidence/bench/*`.

### 3.9 `benchmarks/` → `evidence/workload-catalog/`

True root refs: primarily census key `benchmarks/realtime-chat-v1.json`.

**Do not rewrite:** `evidence/benchmarks/**`, `evidence/perf/runtime-benchmarks/**`.

### 3.10 `monitoring/` → `ops/monitoring/`

| Site | Why |
|---|---|
| `docker-compose.observability.yml:33-34,62,96-97` | relative mounts `./monitoring/...` |
| `ops/staging/compose.go-closure.yml:201-202,230,259-260` | `../../monitoring/...` |
| `scripts/validate-observability.mjs:30+` | `read('monitoring/...')` |
| `scripts/test-alert-delivery.sh:36` | `$ROOT/monitoring/alertmanager.yml` |
| `scripts/test-backup-schedule.sh:18` | grep into alerts.yml |
| `scripts/test-backup-age-metric.sh:17` | same |
| `scripts/release-staging.sh:63` | required file list |
| `control/metrics.go:403` | comment (optional) |

**Do not rewrite:** `docs/RUNBOOKS.md#...` links inside alerts.

### 3.11 `logo/` → `web/logo/`

| Site | Why |
|---|---|
| `ops/asset-provenance.json` | `path` + `sha256` — update path only |
| `scripts/rename-residue-audit.py:63` | FROZEN_PATHS `logo/cx-capsule-target.svg` |
| `.gitignore` | `logo/cx-metal*` |
| `Caddyfile:18` | `@immutable path … /logo/*` is a **URL route** — keep |

### 3.12 Settled non-moves

| Path | Reason |
|---|---|
| `control/` | module root; never split `package main` |
| `scripts/` | `ROOT=dirname/..`, `parents[1]`, deployed `ExecStart=/opt/computexchange/scripts/backup.sh` |
| `pricing/` | boot catalogue, Docker COPY, `/version` digest |
| `Dockerfile.control`, compose, `Caddyfile` | runtime relative resolution + cosign/SLSA |
| `docs/speed-lane-reports/` | gitignored, 0 tracked |
| `.github/workflows/publish-candidate.yml` | cosign certificate identity |

---

## 4. The rewrite sweep

### 4.1 Tool

Stdlib Python dry-run rewriter (created in first execution phase that needs it;
not part of this planning commit):

```bash
python3 scripts/refactor_path_rewrite.py --dry-run
python3 scripts/refactor_path_rewrite.py --apply
```

Dry-run prints every `path:line: old → new`. Human scans traps before apply.

### 4.2 Ordered rules (longest prefix first; first match wins)

| # | Match | Replacement | Notes |
|---:|---|---|---|
| 1 | `sdk/typescript/` | `clients/sdk/typescript/` | before bare `sdk/` |
| 2 | `sdk/python/` | `clients/sdk/python/` | |
| 3 | `sdk/` (path context) | `clients/sdk/` | |
| 4 | `macapp/ComputeExchangeAgent/` | `clients/macapp/ComputeExchangeAgent/` | |
| 5 | `macapp/` | `clients/macapp/` | |
| 6 | `cli/README`, `cli/LICENSE` | `clients/cli/...` | high FP if bare `cli/` |
| 7 | `proto/manifest`, `proto/LICENSE`, `proto/enrollment` | `clients/proto/...` | skip English "protocol" |
| 8 | `proof/claims/` | `evidence/proof/claims/` | |
| 9 | `census/CODEBASE_CENSUS` | `evidence/census/CODEBASE_CENSUS` | |
| 10 | `artifacts/runtime/` | `evidence/artifacts/runtime/` | never `.artifacts/` |
| 11 | `benchmarks/realtime-chat` | `evidence/workload-catalog/realtime-chat` | before bare `benchmarks/` |
| 12 | `bench/immutable/` | `evidence/immutable-fixtures/` | before bare `bench/` |
| 13 | `monitoring/` | `ops/monitoring/` | |
| 14 | `logo/` (path context) | `web/logo/` | never Caddy `/logo/*` URL |

### 4.3 False-positive traps

| Trap | Example | Rule |
|---|---|---|
| `docs` is English | "see the docs for" | Never global-replace `docs` |
| `bench` ⊂ `benchmarks` | `runtime-benchmarks` | Longest-prefix; never inside `runtime-benchmarks` or `evidence/benchmarks` |
| `bench` ⊂ tools | `cargo bench`, `merc-bench/1.0` | Allowlist tokens |
| URL ≠ file path | `fetch("/pricing/board.json")` in `web/prices.html:139` | **Never rewrite** `/pricing/` URL routes |
| URL `/logo/*` | `Caddyfile:18` | Stay |
| SQL/job label | `'proof/input'` | Stay |
| Historical docs | `CHANGELOG.md`, decision records | Allowlist (§9) |
| Cosign identity | `publish-candidate.yml` path | **Never move** |
| Dockerfile in SLSA | `"dockerfile":"Dockerfile.control"` | Stay at root |

---

## 5. The `control/` compression plan

### 5.1 Constraints

1. Never merge `*_test.go` into production `.go`.
2. `main.go` stays alone.
3. `api.go` (4797 LOC) absorbs **nothing**.
4. Embed hosts remain valid.
5. Target merged size band **300–1500 LOC**; split above ~2000.
6. Files already ≥2000 stay unmerged hosts: `realtime_store.go` (2405),
   `gateway_parity_harness.go` (1973), `store.go` (1829), `pricing_decision.go` (1825),
   `service_leases.go` (1659), `realtime.go` (1659), `store_jobs.go` (1616).
7. **Money files merge only with money files.** Allowlist re-derived from domain
   membership in the same commit — never copy-paste a stale list.

### 5.2 Wave A — free files only (RECOMMENDED)

**Scope:** 133 free prod + 237 free test = **370 files**. Named 93 untouched.

**Aggressive first-fit packing (hi≈1400 prod / 1800 test):**

```
free prod (excl main): 133 → ~31   (save ~102)
free test:             237 → ~36   (save ~201)
control total:         463 → ~160  (93 named + ~31 + ~36)
```

Prefix-conservative packing saves less (~51 prod + ~116 test → ~296 total).
**Recommend aggressive packing within a documented domain map.**

#### Free-prod merge groups (domain map)

| Target | Sources | ~LOC |
|---|---|---:|
| `verification_plan_bundle.go` | artifact_policy, attempt, class, state, work_plan, plan, resources, artifact | ~1449 |
| `verification_runtime.go` | lifecycle + processor | ~1077 |
| `verification_apply.go` | keep large host (1131) or small-only pairs | 1131 |
| `verification_work.go` | keep (742) or small pair | 742 |
| `project_compiler_bundle.go` | dependency + compiler | ~1095 |
| `project_compile_support.go` | topology, contracts, compile_receipts, calibration, order, materialize, compile_api | ~1485 |
| `service_lease_support.go` | lease_api, data_plane, pricing, market_liquidity | ~902 |
| `service_leases.go` | **absorb nothing** (1659) | 1659 |
| `runtime_profile_bundle.go` | adapter, profile_admission, profile_sync, matrix | ~1374 |
| `runtime_authority.go` | keep as embed host (1097) | 1097 |
| `release_bundle.go` | release_ui, release_artifact, release_launch | ~1476 |
| `fabric_bundle.go` | topology_planner, topology, measurement | ~1306 |
| `lora_bundle.go` | evaluation_api, settlement, dataset_probe, evaluation_receipts | ~1295 |
| `execution_envelope_bundle.go` | envelope_api, overhead, envelope | ~1284 |
| `serving_matrix_bundle.go` | runner + serving_matrix | ~1141 |
| `realtime_support.go` | identity_cache, profiles (**embed**), clearing, supplier_outcome_stats | ~1124 |
| `dev_bundle.go` | dev_authority, dev_project_compile, dev_checkpoint | ~1000 |
| `plan_bundle.go` | plan_actuals, plan_calibration | ~596 |
| `render_bundle.go` | contract, assembly, work_plan, assembly_receipts | ~596 |
| `canary_bundle.go` | decision, policy | ~581 |
| `admin_bundle.go` | admin_authority, admin_mutation_audit | ~485 |
| `media_bundle.go` | merge, contract, segments | ~466 |
| `benchmark_bundle.go` | benchmark, corroboration | ~374 |
| `webhook_bundle.go` | secret, delivery | ~353 |
| `job_support.go` | object_retention, submit_validate | ~342 |
| `operational_controls_bundle.go` | cache + controls | ~337 |
| `api_support.go` | api_error, api_key_cache | ~326 |
| `pricing_support.go` | pricing_extra, pricing_citation_authority | ~263 |
| `prove_bundle.go` | prove_registry, prove | ~211 |
| store_* hosts | **do not** merge `store_payouts`+`store_tasks` (1389+1286>2000); pack disputes/workers/prepaid under 1500 | |

Remaining free singles pack into thematic buckets until free prod ≈30 files.

**Free tests:** merge by domain prefix into ≤1800 LOC files. Do not merge a free
test into a **named** test without P9 guard updates.

| | Files now | Files after | Δ |
|---|---:|---:|---:|
| Free prod | 133 | ~31 | −102 |
| Free test | 237 | ~36 | −201 |
| Named (untouched) | 93 | 93 | 0 |
| **Total control `.go`** | **463** | **~160** | **−~303** |

### 5.3 Wave B — coordinated named merges (OPTIONAL)

Including guard updates:

- Pack ~15 money files under 800 LOC into ~5 money bundles → save ~10 files.
- Realistic total named save: **~20–40** additional files → control **~120–140**.

**Extra cost:** every MUTATIONS sed target, every `os.ReadFile`, every script
path, CODEOWNERS, allowlists update in the **same commit**. Mutation suite needs
DB and is long. A wrong allowlist is a **silent honesty failure**.

**Recommendation: ship Wave A only** unless a later mandate requires Wave B.

### 5.4 The 94 load-bearing basenames

| Basename | Status | Guards |
|---|---|---|
| `accounts.go` | prod | CODEOWNERS,scripts/Makefile/.github |
| `activation_policy_test.go` | test | scripts/Makefile/.github |
| `api.go` | prod | CODEOWNERS,os.ReadFile,scripts/Makefile/.github |
| `batch_policy.go` | prod | MUTATIONS,scripts/Makefile/.github |
| `billing.go` | prod | CODEOWNERS,MUTATIONS,moneyAndAdmissionAuthorityFiles,scripts/Makefile/.github |
| `billing_classes.go` | prod | MUTATIONS |
| `buyer.go` | prod | moneyAndAdmissionAuthorityFiles |
| `buyer_charge_operations.go` | prod | moneyAndAdmissionAuthorityFiles |
| `coalesced_cluster_money_test.go` | test | scripts/Makefile/.github |
| `collect.go` | prod | CODEOWNERS,moneyAndAdmissionAuthorityFiles,scripts/Makefile/.github |
| `compute_plan.go` | prod | MUTATIONS,moneyAndAdmissionAuthorityFiles |
| `connect.go` | prod | CODEOWNERS,scripts/Makefile/.github |
| `crypto.go` | prod | scripts/Makefile/.github |
| `economic_facts.go` | prod | moneyAndAdmissionAuthorityFiles |
| `economic_plan.go` | prod | moneyAndAdmissionAuthorityFiles,scripts/Makefile/.github |
| `embedding_comparator.go` | prod | scripts/Makefile/.github |
| `enrollment.go` | prod | scripts/Makefile/.github |
| `evidence.go` | prod | scripts/Makefile/.github |
| `evidence_binding_payload_guard_test.go` | test | scripts/Makefile/.github |
| `exact_reuse.go` | prod | MUTATIONS,scripts/Makefile/.github |
| `exact_reuse_batch.go` | prod | MUTATIONS,os.ReadFile,scripts/Makefile/.github |
| `exact_reuse_path.go` | prod | scripts/Makefile/.github |
| `execution_mode.go` | prod | scripts/Makefile/.github |
| `execution_mode_test.go` | test | scripts/Makefile/.github |
| `failure_matrix_agent_test.go` | test | scripts/Makefile/.github |
| `failure_matrix_test.go` | test | scripts/Makefile/.github |
| `gateway_parity_cli.go` | prod | scripts/Makefile/.github |
| `gateway_parity_harness.go` | prod | scripts/Makefile/.github |
| `gateway_parity_matrix.go` | prod | scripts/Makefile/.github |
| `gateway_parity_measure_test.go` | test | scripts/Makefile/.github |
| `ledger.go` | STALE | CODEOWNERS,scripts/Makefile/.github |
| `ledger_write.go` | prod | moneyAndAdmissionAuthorityFiles |
| `main.go` | prod | scripts/Makefile/.github |
| `merc_latency_gap_accounting_test.go` | test | scripts/Makefile/.github |
| `metrics.go` | prod | scripts/Makefile/.github |
| `model_onboarding.go` | prod | scripts/Makefile/.github |
| `money_nanos.go` | prod | MUTATIONS |
| `observed_output_settlement.go` | prod | moneyAndAdmissionAuthorityFiles |
| `payment.go` | prod | MUTATIONS,moneyAndAdmissionAuthorityFiles,scripts/Makefile/.github |
| `payment_authority.go` | prod | MUTATIONS,moneyAndAdmissionAuthorityFiles,scripts/Makefile/.github |
| `placement_readiness_test.go` | test | scripts/Makefile/.github |
| `prefix_placement.go` | prod | os.ReadFile,scripts/Makefile/.github |
| `prefix_placement_test.go` | test | scripts/Makefile/.github |
| `prefix_routing.go` | prod | MUTATIONS |
| `prepaid.go` | prod | moneyAndAdmissionAuthorityFiles |
| `pricing.go` | prod | moneyAndAdmissionAuthorityFiles,scripts/Makefile/.github |
| `pricing_decision.go` | prod | MUTATIONS,moneyAndAdmissionAuthorityFiles |
| `pricing_governance.go` | prod | moneyAndAdmissionAuthorityFiles |
| `pricing_governance_test.go` | test | scripts/Makefile/.github |
| `pricing_policy.go` | prod | scripts/Makefile/.github |
| `pricing_test.go` | test | scripts/Makefile/.github |
| `project_declaration.go` | prod | scripts/Makefile/.github |
| `project_declaration_test.go` | test | scripts/Makefile/.github |
| `project_quote.go` | prod | scripts/Makefile/.github |
| `project_submit.go` | prod | scripts/Makefile/.github |
| `project_submit_test.go` | test | scripts/Makefile/.github |
| `quote.go` | prod | MUTATIONS,moneyAndAdmissionAuthorityFiles |
| `realtime.go` | prod | MUTATIONS,os.ReadFile,scripts/Makefile/.github |
| `realtime_integration_test.go` | test | scripts/Makefile/.github |
| `realtime_placement.go` | prod | MUTATIONS,moneyAndAdmissionAuthorityFiles |
| `realtime_pricing_decision.go` | prod | MUTATIONS |
| `realtime_reuse_pricing_decision.go` | prod | MUTATIONS |
| `realtime_store.go` | prod | MUTATIONS,scripts/Makefile/.github |
| `receipt_identity.go` | prod | scripts/Makefile/.github |
| `receipt_identity_test.go` | test | scripts/Makefile/.github |
| `receipt_tamper_test.go` | test | scripts/Makefile/.github |
| `rejection_economics_test.go` | test | scripts/Makefile/.github |
| `runtime_cell_admission_binding.go` | prod | MUTATIONS |
| `runtime_cell_cost.go` | prod | scripts/Makefile/.github |
| `runtime_cell_cost_test.go` | test | scripts/Makefile/.github |
| `runtime_cell_economics.go` | prod | MUTATIONS |
| `runtime_cell_performance.go` | prod | scripts/Makefile/.github |
| `runtime_cell_promotion.go` | prod | scripts/Makefile/.github |
| `runtime_cost_tie_authority.go` | prod | MUTATIONS |
| `runtime_governed_comparison.go` | prod | MUTATIONS,scripts/Makefile/.github |
| `runtime_shadow_selection.go` | prod | MUTATIONS,scripts/Makefile/.github |
| `scheduler.go` | prod | CODEOWNERS,MUTATIONS,moneyAndAdmissionAuthorityFiles,os.ReadFile,scripts/Makefile/.github |
| `second_runtime_chain_test.go` | test | scripts/Makefile/.github |
| `second_runtime_verification_test.go` | test | scripts/Makefile/.github |
| `seed.go` | prod | scripts/Makefile/.github |
| `shape_routing.go` | prod | moneyAndAdmissionAuthorityFiles,scripts/Makefile/.github |
| `storage.go` | prod | scripts/Makefile/.github |
| `store.go` | prod | CODEOWNERS,scripts/Makefile/.github |
| `store_billing.go` | prod | moneyAndAdmissionAuthorityFiles |
| `store_jobs.go` | prod | MUTATIONS,scripts/Makefile/.github |
| `stripe_api_contract.go` | prod | MUTATIONS |
| `stripe_settlement.go` | prod | moneyAndAdmissionAuthorityFiles |
| `stripe_simulator.go` | prod | scripts/Makefile/.github |
| `supplier_accrual.go` | prod | MUTATIONS,moneyAndAdmissionAuthorityFiles |
| `suppliers.go` | prod | CODEOWNERS,scripts/Makefile/.github |
| `two_agent_enrollment_test.go` | test | scripts/Makefile/.github |
| `verification.go` | prod | scripts/Makefile/.github |
| `workers.go` | prod | os.ReadFile,scripts/Makefile/.github |
| `workload_classification.go` | prod | MUTATIONS,scripts/Makefile/.github |

#### Guard update matrix (any Wave B merge consuming `X` into `T`)

| If guard includes… | Same-commit action |
|---|---|
| `MUTATIONS` | Rewrite every entry `"X|…|sed"` → `"T|…|sed"`; ensure sed still matches; run mutation suite |
| `moneyAndAdmissionAuthorityFiles` | Remove `X`, ensure `T` present; **re-derive full list from domain membership**; update `plan_calibration_test.go` mirror; `TestGuardedFileListsAgree` must pass |
| `os.ReadFile` | Change argument to `T` |
| `scripts/Makefile/.github` | `rg -n 'control/X'` and update each hit |
| `CODEOWNERS` | Update literal path; ensure globs still cover `T` |

**Merges that lack this enumeration at execution time are forbidden.**

#### Stale entry action

- Replace `.github/CODEOWNERS` `/control/ledger.go` with `/control/ledger_write.go`.
- Confirm no remaining `control/ledger.go` string outside historical docs.

### 5.5 Embed hosts and `init`

| Host | Directive | Rule |
|---|---|---|
| `store.go` | `schema.sql` (×2) | Named; leave |
| `runtime_authority.go` | `runtime-authority.json`, `evidence-manifest.json` | If merged, move both embeds + vars together |
| `realtime_profiles.go` | `runtime-profiles/*.json` | Keep with profile loader |
| `planner.go` / `pricing.go` / `scheduler.go` `init()` | independent | Multiple `init` OK; no new order deps |

---

## 6. The documentation plan

### 6.1 Target document set

**Root (stay):** `README.md`, `CHANGELOG.md`, `RELEASE_READINESS.md` (pinned),
`LICENSE`, `NOTICE`.

**Under `docs/` — eight legal/policy instruments (paths frozen):**

1. `docs/CANARY_TERMS.md`
2. `docs/PRIVACY_NOTICE_DRAFT.md`
3. `docs/PRIVACY_DATA_GOVERNANCE.md`
4. `docs/DSAR_RUNBOOK.md`
5. `docs/ACCEPTABLE_USE_AND_ABUSE_RESPONSE.md`
6. `docs/SUPPORT_AND_INCIDENT_RUNBOOK.md` ← also external GitHub deep-link
7. `docs/THIRD_PARTY_LICENSES.md` ← machine-parsed table contract
8. `docs/SUPPLY_CHAIN_POLICY.md`

Pinned by `ops/legal-review.json` + validators `validate-governance.py`,
`validate-license-register.py`, `validate-runbook-contacts.py`.

**External path freezes:**

9. `docs/SECURITY.md` — Policy URL (`control/api.go:4017`, `web/.well-known/security.txt`)
10. `docs/RUNBOOKS.md` — **every existing heading/anchor preserved**

**Frozen historical register:**

11. `docs/RENAME_REGISTER.md` — in `FROZEN_PATHS`

**Merged targets:**

12. `docs/PROGRAMME.md`
13. `docs/RUNTIME_AND_PERF.md`
14. `docs/ARCHITECTURE.md`
15. `docs/OPERATIONS_DEPLOY.md` (or fold into RUNBOOKS appendices if tighter budget)

No redirect stubs except keeping the three external paths as real content homes.

### 6.2 Ordered section outlines

#### `docs/RUNBOOKS.md` (absorb; preserve anchors)

Verified alert anchors (11 unique; 27 references in `monitoring/alerts.yml`):

```
#control-plane-or-database-outage
#deploy
#agent-offline-or-task-stall
#queue-stall-and-safe-requeue
#storage-or-database-outage
#backup-and-restore
#webhook-delivery-failure    ← PRE-EXISTING mismatch vs heading Webhook failure
#verification-failure-or-bad-result-dispute
#money-incident-or-payout-hold
#insufficient-capacity
#alert-mapping
```

Also referenced by `ops/systemd/merc-backup.service` / `.timer`
`Documentation=file:docs/RUNBOOKS.md`.

**Pre-existing bug (do not fix):** alerts request `#webhook-delivery-failure` but
heading is `## Webhook failure` (slug `#webhook-failure`).

**Absorb after existing sections:** root `RUNBOOK_ARTIFACTS.md`,
`RUNBOOK_WORKER_FAILURE.md`; other deploy runbook material only if not better in
OPERATIONS_DEPLOY.

#### `docs/PROGRAMME.md`

1. From `docs/MASTER_PROGRAMME_LEDGER.md` (entire)
2. From `docs/SHIPPABILITY_STATUS.md`
3. From `docs/MERC_SHIPPABILITY_DIRECTIVE.md`
4. From `docs/PATH_TO_TEN.md`
5. From `docs/YOUR_10_ACTIONS.md`
6. From `docs/EXTERNAL_ACTION_QUEUE.md`
7. From `docs/FACET_EXTERNAL_ACTION_PACK.md`
8. From `docs/LEVEL_B_OPERATOR_HANDOFF.md`
9. From `docs/EXTERNAL_OPERATOR_HANDOFF.md`
10. From root `ROADMAP.md`, `RELEASE_GATES.md`, `REQUIREMENT_PROOF_MATRIX.md`
11. From `docs/LANE_RESEARCH.md`

#### `docs/RUNTIME_AND_PERF.md`

1. `docs/RUNTIME_MATRIX.md`
2. `docs/RUNTIME_CROSS_TEST_2026-07-30.md`
3. `docs/SPEED_LANE_2026-07-27.md`
4. `docs/BENCH_PROFILES_2026-07-27.md`
5. `docs/FRONTIER_300X.md`
6. `docs/EXECUTION_FRONTIER_LANE.md`
7. `docs/HOT_PATH_DURABLE_ADMISSION.md`
8. `docs/runtime/ADAPTIVE_EXECUTION.md`
9. `docs/runtime/RUNTIME_GGUF_CLOSURE_BASELINE.md`
10. `docs/runtime/SECOND_RUNTIME_CENSUS.md`
11. `docs/runtime/TRANCHE_STATUS.md`
12. `docs/OFFER_MULTIPLICITY.md`

#### `docs/ARCHITECTURE.md`

1. root `EXECUTION_NETWORK_BIBLE.md`
2. `docs/WORKLOAD_IR_V1.md`
3. `docs/CAPACITY_LEASES.md`
4. `docs/FRONTEND_CONTRACT.md`
5. `docs/MEDIA_RENDERING_CONTRACT.md`, `MEDIA_TRANSCODE_CONTRACT.md`
6. `docs/STRIPE_CONNECT.md`, `STRIPE_SANDBOX_SETUP.md`
7. `docs/MONEY_AUDIT_2026-07-27.md`
8. `docs/POSTGRES_TRUST_BOUNDARY.md`
9. `docs/DECISION_ZERO.md`, `DECISION_ZERO_OUTCOME.md`, `DECISION_ZERO_REVERSAL.md`
10. `docs/QUICKSTART.md` (full text; README keeps short pointer)
11. `docs/DEPENDENCY_REVIEW.md`
12. `docs/CANARY_DRIVER_FINDINGS.md`, `CANARY_SCENARIO_DRIVER.md`

#### `docs/OPERATIONS_DEPLOY.md`

1. `docs/DEPLOY_MERC_DROPLET.md`
2. `docs/STAGING_DEPLOYMENT_PLAN.md`
3. `docs/RUNPOD_ESCALATION.md`
4. `docs/CUDA_CHAIN_RUNBOOK.md`
5. Optionally full text from `ops/staging/README.md` with refs updated

### 6.3 No-loss protocol

**Tool:** `scripts/docs_noloss_check.py` (stdlib), created during P7.

**Algorithm:**

1. Build source multiset `S` and merged multiset `M` extracting:
   - AT2/ATX headings (normalized whitespace)
   - GFM table rows (raw line text)
   - Absolute URLs and repo-relative path tokens
   - Fenced code blocks (fence body + per-line atoms)
   - Shell-like command lines (lines starting with prompt `$`)
   - Numeric literals matching
     `r'[-+]?\d[\d,]*(?:\.\d+)?%?|\d+t/s|\d+ LOC'`
2. Apply explicit de-duplication ledger `D` for verbatim cross-source repeats.
3. Exit **1** if any atom in `S` is missing from `M ∪ D` with count deficit.
4. Print deficits (fail) and surpluses (OK).

**Justified de-duplication ledger (initial):**

| Atom class | Why collapsible |
|---|---|
| Identical command blocks copied across QUICKSTART and README | Same bytes in ≥2 sources; ledger records `(hash, sources[])` |
| Excess repeated path tokens | Keep count ≥1; collapse pure duplicates |

Legal files are **not merged**, so their banners are not collapsed away.

**What it catches:** dropped headings, tables, URLs, commands, numbers, fences.

**What it cannot catch:** paraphrased loss; transposition keeping tokens;
semantic contradictions (§6.4); binary assets.

**Reviewer run:**

```bash
python3 scripts/docs_noloss_check.py \
  --sources-list /tmp/merge-sources.txt \
  --merged docs/PROGRAMME.md docs/RUNTIME_AND_PERF.md docs/ARCHITECTURE.md \
           docs/OPERATIONS_DEPLOY.md docs/RUNBOOKS.md \
  --dedupe-ledger docs/_noloss_dedupe_ledger.json
test $? -eq 0
```

### 6.4 Documented contradictions — surface only

Keep **both** sides with source citations. Do not resolve; do not drop one side.

1. Authorization matrix route counts **76 / 77 / 110** across documents
   (`docs/SECURITY.md` claims 110 method/path registrations;
   `docs/DECISION_ZERO_REVERSAL.md` discusses 72→77). Compare
   `ops/authorization-matrix.json` at execution time.
2. MLX peak throughput **6828 t/s** (`SPEED_LANE_2026-07-27.md`) vs
   **310.7 t/s** (`RUNTIME_CROSS_TEST_2026-07-30.md`) — different harnesses.
3–10. Additional cross-doc numeric conflicts encountered while merging must be
   listed in the P7 commit message; do not drop either side.

**Dangling references (do not create files):**

- `docs/REBRAND.md` (`docs/SUPPORT_AND_INCIDENT_RUNBOOK.md:30`)
- `docs/GPU_CAPABILITY.md` (`ops/economics-readiness.json:42`)
- `docs/internal/CREED_AND_PATH_TO_TEN.md` / `docs/CREED_AND_PATH_TO_TEN.md`
  (`control/scheduler.go` comments)

### 6.5 Census staleness

`census/CODEBASE_CENSUS.json` is stale relative to HEAD. Regenerate in P10;
do not hand-edit thousands of keys during merges.

---

## 7. The proof-machinery plan

### 7.1 Five path-into-proof bindings

#### (1) `sourceFingerprint` — `control/evidence.go:106+` (path framing ~148–175)

- **Changes:** every moved path changes fingerprint input (relative path bytes +
  content hash under domain `computexchange-source-fingerprint-v1`).
- **Regenerate:** receipts embedding the live fingerprint after tree is final (P10).
- **Historical mismatch:** old fingerprints **will not match** the new tree.
  **Honest:** they attest to the tree as it was. Do not rewrite history.

#### (2) `worktreeContentDigest` — `control/dev_checkpoint.go:122-156`

- **Changes:** `path\0contentSHA\0` over `git ls-files`.
- **Regenerate:** `cd control && go run . dev checkpoint` (P10). Pre-push hook
  `.githooks/pre-push` demands push-eligible receipt for new commits.
- **Historical:** prior digests are commit-bound; mismatch with HEAD is expected.

#### (3) `//go:embed evidence-manifest.json` — `control/runtime_authority.go:913`

- **Keys:** paths under `evidence/perf/runtime-benchmarks/…` (already under
  `evidence/`; root moves do not change them unless a sweep wrongly rewrites them).
- **Risk:** low for reorg; high if substring rewrite hits these keys.
- **Test:** `runtime_authority_v2_test.go` ~line 483 reads `"../"+path` — loud failure.
- **Phase:** verify in P5/P6/P11.

#### (4) `ops/asset-provenance.json`

- **Changes:** `logo/*` → `web/logo/*` path fields; **sha256 unchanged** (P4).
- **Gate:** `scripts/validate-governance.py` in `make ci`.

#### (5) `.github/workflows/publish-candidate.yml` + `Dockerfile.control`

- **Changes: nothing** — do not move.
- Cosign certificate identity records this workflow path; SLSA predicate embeds
  `"dockerfile":"Dockerfile.control"`. Moving breaks verification of past
  releases; regeneration cannot re-sign history.

### 7.2 Other path-bound machinery

| Binding | Action |
|---|---|
| claim-policy.json | Move with proof; update `active_surfaces` + `required_path` |
| CODEBASE_CENSUS | Move; regenerate keys in P10 |
| evidence-binding-census | Regenerate |
| `*.binding.json` sidecars | Stay adjacent to artifacts |
| `isGeneratedReleaseEvidencePath` | Update census paths; keep `RELEASE_READINESS.md` at root |
| `FROZEN_PATHS` | Update prefixes for moved frozen trees |

### 7.3 Historical receipts that no longer match — honest

| Artifact class | Matches new tree? | Why OK |
|---|---|---|
| Old source fingerprints in dated evidence JSON | No | Attest prior trees |
| Old checkpoint digests | No | Commit-bound; local gate |
| Cosign-signed images | Still verify with **old** identity path | Workflow not moved |
| Binding census for moved fixtures | Need regen | Status honesty |
| Pricing authority under `evidence/benchmarks/` | Yes if not moved | We do not move them |

### 7.4 `.gitignore` rewrite — rule-by-rule map

| Current rule | After |
|---|---|
| `agent/target/` | keep |
| `**/*.rs.bk` | keep |
| `control/merc`, `control/control`, `control/**/*.test`, `*.out`, `coverage.out` | keep |
| `macapp/.build/` | `clients/macapp/.build/` |
| `build/` | keep + add `.local/build/` |
| `**/__pycache__/`, `*.pyc` | keep |
| `sdk/python/build|dist|*.egg-info` | `clients/sdk/python/...` |
| `.artifacts/` | keep (≠ `artifacts/`) |
| `scripts/*.log`, `*.log`, `*.pid` | keep |
| `.env*` secret patterns | keep |
| `agent/agent.toml` | keep |
| `.DS_Store`, `.idea/`, `.vscode/`, `*.swp`, `*~` | keep |
| `*.psd`, `logo/cx-metal*` | `web/logo/cx-metal*` + keep old pattern one transition |
| `models/`, `data/`, `*.gguf`, `*.safetensors`, `*.bin` | keep (sacred) |
| `render/**` binary patterns | keep; optional `.local/render/**` |
| `.claude/`, `web/admin-demo.html` | keep |
| `target/` | keep |
| `diag_images/` | keep |
| `docs/speed-lane-reports/`, `docs/bench-local-reports/` | keep |
| `**/.pytest_cache/` | keep |
| `tmp/` | keep |
| `.gocache/`, `.gomodcache/`, `.gopath/` | keep |
| `*.orig`, `*.rej` | keep |
| `sdk/typescript/dist|node_modules` | `clients/sdk/typescript/...` |
| `.merc-*.env`, `.merc-runpod/`, `.merc-release/`, `.cloudflare-zone-backups/` | keep |
| `evidence/checkpoint/` | keep |
| `.ci-test.json` | keep |
| `.serena/` | keep |
| **New** | `.local/` umbrella |
| **New** | `.local/review/`, `.local/dist/`, `.local/build/`, `.local/render/` |

---

## 8. Untracked-data procedure

`git mv` moves only tracked files. For untracked trees:

```bash
# 1) checksum before
find review -type f -print0 2>/dev/null | sort -z | xargs -0 shasum -a 256 > /tmp/review-before.sha256
# 2) move
mkdir -p .local
mv review .local/review
# 3) checksum after
find .local/review -type f -print0 | sort -z | xargs -0 shasum -a 256 > /tmp/review-after.sha256
# 4) compare by hash multiset (paths differ)
cut -d' ' -f1 /tmp/review-before.sha256 | sort | shasum -a 256
cut -d' ' -f1 /tmp/review-after.sha256 | sort | shasum -a 256
# must match
# 5) only then remove empty source leftovers
```

**Never delete source until destination hash multiset matches.**

| Path | State in this worktree | Action |
|---|---|---|
| `review/` | **absent** | If later copied from main tree, relocate to `.local/review/` with checksums |
| `agent/target/` | gitignored build output | **Leave in place** — regenerable; large; moving breaks incremental builds for no repo benefit |
| `pass3_bundle/` | absent | no-op |
| `build/`, `dist/`, `render/` | absent | ignore rules only |

`FROZEN_PATHS` includes `review/` but the scanner uses `git ls-files` only
(`rename-residue-audit.py` ~line 222) — untracked review is not scanned;
relocating it does not trip the gate.

---

## 9. Completeness proof

### 9.1 Old-prefix residue scan

After all phases, fail on unexpected hits of old live prefixes.

**Allowlisted files (old paths OK):**

- `CHANGELOG.md` — history
- `docs/RENAME_REGISTER.md` — must name what it froze
- Dated decision records that narrate renames (`DECISION_ZERO_REVERSAL.md`, etc.)
- `REFACTOR_PLAN.md` — this plan
- Historical rows inside merged docs that are explicitly historical quotes

**Justification:** historical narrative must remain able to name what was renamed.

```bash
# Example residue check (extend prefixes as phases complete)
rg -n --glob '!CHANGELOG.md' --glob '!docs/RENAME_REGISTER.md' --glob '!REFACTOR_PLAN.md' \
  -e '(?<![\w/])sdk/' -e 'macapp/' -e '(?<!ops/)monitoring/' -e '(?<!web/)logo/' \
  -e 'census/CODEBASE_CENSUS' -e 'proof/claims/' -e 'bench/immutable/' \
  | tee /tmp/residue.txt
test ! -s /tmp/residue.txt
```

### 9.2 Before/after tracked inventory

```bash
git ls-files | sort > /tmp/inventory-before.txt   # freeze at P0 / pre-execution
git ls-files | sort > /tmp/inventory-after.txt
comm -23 /tmp/inventory-before.txt /tmp/inventory-after.txt > /tmp/removed.txt
comm -13 /tmp/inventory-before.txt /tmp/inventory-after.txt > /tmp/added.txt
# Every path in removed.txt must appear in an explicit deletion ledger
# (merged docs, merged go). No silent disappearances.
```

### 9.3 Makefile targets — local vs needs credentials/cloud/hardware

| Target | Local? | Needs |
|---|---|---|
| `make control` / `make build` | yes | Go toolchain |
| `make test-unit` | mostly | may skip DB with env |
| `make test` | partial | `MERC_TEST_DATABASE_URL`, storage |
| `make ci` | partial | DB + isolation scripts; long |
| `make mutation-test` | partial | DB + long runtime |
| `make checkpoint` | yes | Go; gitignored receipt |
| `make license-register` / `release-gates` | yes | may fail on unset contacts (intentional) |
| `make docker-build` | yes | Docker |
| `make prove-local` | partial | Docker/Postgres/MinIO |
| `make stripe-*` | no | Stripe test keys |
| `make private-canary` | no | live env |
| `make droplet-deploy` | no | remote host access |
| `make soak-*` | no | long-running env |
| `make realtime-sdk-conformance` | partial | running control |
| agent `cargo test` | yes | Rust toolchain |
| GPU/Metal benches | no | hardware |

**Unverified after local-only execution:** Stripe live paths, droplet deploy,
cosign publish, RunPod spend, multi-hour soak, real GPU throughput receipts.

---

## 10. Risk register

| Risk | Severity | Likelihood | Mitigation |
|---|---|---|---|
| Weakened `moneyAndAdmissionAuthorityFiles` after merge | **Critical** | Medium if Wave B rushed | Wave A default; money-only merges; re-derive allowlist; mutation suite |
| Silent embed path rot for runtime benchmarks | Critical | Low if sweeps careful | Never rewrite `evidence/perf/runtime-benchmarks` keys; run authority tests |
| Claim-policy path miss | High | Medium | Same commit as moves; `validate-claim-surfaces.py` |
| Compose volume paths wrong → empty observability | High | Medium | P3 gate + `validate-observability.mjs` |
| Asset provenance path/sha mismatch | High | Low | Checksum loop in P4 |
| Doc no-loss tool false green (paraphrase) | Medium | Medium | Human spot-check high-value tables |
| Substring rewrite damages `runtime-benchmarks` or `.artifacts` | High | Medium | Longest-prefix + dry-run |
| Checkpoint/pre-push friction after fingerprint change | Low | High | Expected; regenerate checkpoint |
| `review/` data loss via `git clean` | Critical | Low | Checksum move; never clean `.local/review` |
| Stale `ledger.go` CODEOWNERS | Low | Certain today | Fix in P1/P8 |
| Parallel phases stepping on same files | Medium | Medium | Prefer serial P2→P6 if unsure |
| Worktree missing `review/`/`docker/` vs main assumptions | Medium | Certain | Corrections §; procedures still written |

**Phase most likely to go wrong:** **P7 (documentation consolidation)** —
highest volume of irreversible human-meaning content, no compiler to catch
dropped caveats, contradictions look like merge errors. Second: **P9 money
merges** (if attempted) for silent allowlist weakening. Third: **P2/P3**
multi-file ref updates missing a `filepath.Join` site.

**Watch after landing on main:**

1. First `make ci` on a clean agent with DB.
2. `validate-claim-surfaces` and `validate-governance`.
3. Checkpoint + dry-run push hook.
4. Observability stack still mounts alert rules.
5. Price board still loads; `/pricing/board.json` route unchanged.
6. Cosign verify of **old** images still works (workflow path untouched).

**Could not verify in this planning run:**

- Full `make ci` duration/flakes on this machine
- Whether future commits add new `filepath.Join("..", "macapp", …)` sites
  before execution (listed all current `rg` hits)
- Content of absent `review/` tree
- Live Stripe / droplet / cosign publish paths

---

## Appendix A — Full `control/*.go` LOC listing

Produced by line-count over `git ls-files 'control/*.go'`. Format: `LOC\tpath`.

```
4797	control/api.go
3696	control/job_task_money_integration_test.go
2405	control/realtime_store.go
1973	control/gateway_parity_harness.go
1829	control/store.go
1825	control/pricing_decision.go
1659	control/realtime.go
1659	control/service_leases.go
1616	control/store_jobs.go
1389	control/quote.go
1389	control/store_payouts.go
1386	control/workers.go
1317	control/merc_segment_latency_measure_test.go
1315	control/scheduler.go
1307	control/prefix_kv_hitrate_measure_test.go
1286	control/store_tasks.go
1247	control/gateway_parity_matrix.go
1226	control/enrollment.go
1205	control/release_launch.go
1131	control/verification_apply.go
1097	control/runtime_authority.go
1071	control/serving_matrix_candle_llama_test.go
1031	control/gateway_parity_harness_test.go
1031	control/two_agent_enrollment_test.go
1016	control/runtime_profile_migration_test.go
1013	control/realtime_integration_test.go
1006	control/pricing.go
990	control/collect.go
943	control/store_prepaid.go
930	control/store_workers.go
893	control/execution_envelope.go
866	control/runtime_authority_v2_test.go
863	control/merc_latency_gap_accounting_test.go
847	control/public_path_reuse_proof_test.go
844	control/coalesced_cluster_money_test.go
843	control/store_billing.go
834	control/buyer.go
829	control/serving_matrix.go
813	control/fabric_measurement.go
798	control/activation_policy.go
794	control/authorize_tail_characterize_test.go
782	control/runtime_cell_cost.go
772	control/economic_plan.go
759	control/store_disputes.go
751	control/billing.go
742	control/verification_work.go
737	control/runtime_cell_cost_test.go
727	control/failure_matrix_test.go
727	control/serving_matrix_live_test.go
724	control/realtime_funding_settlement_test.go
719	control/runtime_governed_comparison.go
700	control/fabric_measurement_test.go
700	control/stripe_simulator.go
696	control/metrics.go
694	control/prepaid_funding_authority_test.go
691	control/project_compiler.go
680	control/hot_path_free_admit_probe_test.go
669	control/directive_phase6_economics.go
663	control/compute_plan.go
650	control/service_leases_test.go
644	control/verification_processor.go
637	control/production_reachability_test.go
629	control/prefix_routing.go
629	control/verification.go
627	control/runtime_cell_performance_test.go
624	control/workload_classification.go
610	control/buyer_refund_test.go
606	control/observed_output_settlement.go
603	control/exact_settlement_authority_test.go
589	control/gateway_parity_matrix_test.go
586	control/payment.go
579	control/execution_envelope_test.go
579	control/payment_authority.go
578	control/observed_output_settlement_test.go
576	control/containment_identity_test.go
576	control/money_nanos.go
573	control/activation_policy_test.go
567	control/data_governance.go
564	control/project_declaration_test.go
563	control/runtime_governed_comparison_test.go
562	control/economic_facts_test.go
557	control/runtime_cell_economics.go
557	control/runtime_shadow_selection.go
556	control/plan_actuals_test.go
551	control/true_net_contribution_test.go
548	control/realtime_supplier_outcome_stats.go
545	control/runtime_matrix.go
544	control/prefix_routing_wiring_test.go
537	control/audit.go
537	control/eta_calibration_test.go
532	control/runtime_cell_performance.go
529	control/stripe_cash_events.go
525	control/realtime_clearing_test.go
521	control/runtime_cell_economics_test.go
516	control/inflight_coalescing_test.go
515	control/payout_money_path_test.go
511	control/result_validation.go
509	control/money_nanos_test.go
507	control/accounts.go
506	control/workload_classification_test.go
500	control/project_declaration.go
496	control/runtime_cell_admission_binding.go
495	control/execution_overhead_test.go
494	control/money_completeness_test.go
493	control/dev_checkpoint.go
492	control/quote_test.go
488	control/enrollment_test.go
487	control/main.go
486	control/compute_plan_test.go
486	control/lora_evaluation_receipts.go
480	control/arrival_batch.go
473	control/first_complete_loop_test.go
468	control/realtime_auth_latency_probe_test.go
462	control/canary_decision_test.go
458	control/runtime_matrix_test.go
457	control/admin_mutation_audit_test.go
453	control/ledger_write.go
452	control/workload_simulation_test.go
448	control/prefix_routing_test.go
445	control/currency.go
444	control/project_submit_test.go
443	control/types.go
433	control/verification_lifecycle.go
422	control/prefix_affinity_measure_test.go
419	control/runtime_cell_promotion.go
417	control/gateway_parity_measure_test.go
407	control/input_depth.go
407	control/realtime_supplier_outcome_stats_test.go
405	control/fabric_topology.go
404	control/project_dependency.go
400	control/lora_dataset_probe.go
398	control/gateway_parity_cli.go
395	control/exact_reuse.go
394	control/receipt_identity.go
393	control/traffic_class.go
392	control/arrival_batch_perf_test.go
392	control/verification_plan_test.go
381	control/storage.go
378	control/buyer_supplier_independence.go
378	control/embedding_comparator.go
376	control/failure.go
373	control/pricing_decision_test.go
372	control/dev_project_compile.go
372	control/residency_measurement_test.go
370	control/response_path_latency_test.go
368	control/second_runtime_chain_test.go
365	control/prepaid.go
358	control/prepaid_test.go
356	control/lora_settlement.go
356	control/payment_authority_test.go
355	control/embedding_comparator_test.go
355	control/runtime_profile_sync.go
354	control/suppliers.go
351	control/second_runtime_verification_test.go
347	control/pricing_decision_catalogue_anchor_test.go
342	control/runtime_cell_admission_binding_test.go
342	control/stranger_admission_test.go
339	control/market_liquidity_test.go
339	control/plan_calibration_test.go
339	control/supplier_accrual_test.go
335	control/project_compile_api_test.go
332	control/economic_plan_test.go
325	control/dispute_payout_integration_test.go
321	control/execution_overhead.go
321	control/verification_artifact.go
320	control/directive_phase6_economics_test.go
320	control/project_compile_api.go
319	control/image_generation.go
317	control/paired_cohort_test.go
316	control/verification_class_test.go
315	control/failure_matrix_agent_test.go
314	control/canary_policy.go
313	control/inflight_coalescing.go
312	control/lora_settlement_test.go
312	control/serving_matrix_runner.go
310	control/economic_facts.go
308	control/pricing_test.go
306	control/authority_callgraph_test.go
306	control/realtime_test.go
305	control/cell_authority_binding_test.go
305	control/plan_calibration.go
305	control/service_market_liquidity.go
303	control/market_liquidity.go
302	control/exact_reuse_batch.go
300	control/realtime_cancellation_truth_test.go
298	control/input_depth_test.go
298	control/project_materialize.go
296	control/authorize_settlement_deadlock_test.go
296	control/multi_gpu_admission_test.go
294	control/project_order.go
293	control/media_segments_test.go
292	control/project_quote.go
291	control/plan_actuals.go
290	control/artifact_harness_test.go
290	control/project_submit.go
289	control/currency_test.go
287	control/fuzz_invariants_test.go
286	control/cost_schedule.go
283	control/reuse_money_wiring_test.go
282	control/pricing_governance.go
282	control/webhook_delivery.go
281	control/serving_matrix_test.go
279	control/arrival_batch_test.go
273	control/multi_gpu_admission.go
272	control/billing_test.go
270	control/result_validation_test.go
270	control/runtime_cost_tie_authority.go
269	control/realtime_placement.go
269	control/release_launch_test.go
268	control/object_deletion_integration_test.go
268	control/prepaid_funding_integration_test.go
267	control/canary_decision.go
265	control/risk_reserve_ledger.go
262	control/release_artifact_test.go
262	control/runtime_catalog_integration_test.go
262	control/stripe_api_contract_test.go
261	control/cell_authority_binding.go
259	control/admin_mutation_audit.go
259	control/realtime_clearing.go
258	control/image_generation_test.go
257	control/capability_manifest.go
257	control/execution_mode.go
256	control/planner.go
253	control/exact_reuse_path.go
253	control/runtime_shadow_selection_test.go
248	control/prefix_observation_test.go
241	control/pricing_authority_reconciliation_test.go
237	control/realtime_pricing_decision.go
237	control/runtime_adapter.go
237	control/runtime_profile_admission.go
236	control/service_lease_failover_wiring_test.go
235	control/dispute_operator_resolve_test.go
234	control/prefix_placement_test.go
234	control/service_lease_pricing.go
233	control/operational_controls.go
232	control/verification_resources.go
231	control/admin_authority_test.go
231	control/media_segments.go
228	control/seed.go
227	control/runtime_cell_authority_test.go
226	control/admin_authority.go
221	control/small_job_economics_test.go
220	control/api_error_test.go
220	control/data_governance_integration_test.go
220	control/runtime_adapter_test.go
219	control/embedding_comparator_policy_test.go
219	control/pricing_citation_authority.go
219	control/project_order_test.go
218	control/authorize_late_funding_lock_test.go
218	control/exact_result_retention.go
218	control/receipt_identity_test.go
218	control/verification_drain_test.go
216	control/topology_planner_test.go
214	control/pricing_freshness_test.go
211	control/parity_upstream_capture.go
211	control/service_lease_data_plane.go
210	control/buyer_charge_operations.go
210	control/evidence.go
210	control/project_dependency_test.go
208	control/dev_checkpoint_test.go
208	control/runtime_cost_tie_authority_test.go
206	control/job_object_retention_test.go
205	control/capability_manifest_test.go
205	control/price_board_parity_test.go
202	control/benchmark_corroboration.go
202	control/stripe_cash_events_test.go
201	control/rejection_economics_test.go
199	control/admission_telemetry.go
199	control/exact_reuse_identity_fields_test.go
199	control/project_calibration.go
197	control/payout_fixture_test.go
197	control/provider_cost_authority.go
197	control/verification_plan.go
196	control/payment_test.go
195	control/job_currency_integration_test.go
194	control/lora_evaluation_api_test.go
194	control/worker_leader.go
193	control/verification_work_plan.go
192	control/economic_plan_authority.go
192	control/project_quote_test.go
190	control/money_state_machine_test.go
190	control/receipt_test.go
188	control/render_assembly_receipts.go
187	control/placement_parity_integration_test.go
186	control/job_submit_validate.go
182	control/realtime_profiles.go
180	control/money_authority_audit.go
178	control/canary_policy_test.go
178	control/scheduler_hw_class_deferral_test.go
177	control/project_compiler_test.go
176	control/governance_approval_test.go
176	control/realtime_placement_test.go
174	control/service_lease_data_plane_test.go
173	control/reconcile.go
173	control/shape_routing_test.go
173	control/strict_json_test.go
172	control/benchmark.go
172	control/contract_binding_shape_test.go
172	control/exact_reuse_test.go
171	control/exact_result_retention_test.go
170	control/api_key_cache.go
170	control/governance_approval.go
170	control/topology_planner.go
169	control/directed_routing_test.go
169	control/prefix_routing_path.go
168	control/project_compile_receipts.go
167	control/traffic_class_test.go
166	control/prepaid_envelope_expiry_reservation_test.go
164	control/billing_classes.go
164	control/verification_state.go
163	control/pricing_decision_test_helpers_test.go
163	control/prove.go
162	control/modeled_cost_settlement.go
161	control/stripe_settlement.go
159	control/release_artifact.go
156	control/api_error.go
156	control/job_object_retention.go
156	control/render_work_plan.go
155	control/execution_mode_test.go
153	control/billing_classes_test.go
153	control/quote_currency_integration_test.go
152	control/cad_stranger_share_test.go
152	control/receipt.go
152	control/service_lease_api.go
151	control/batch_policy.go
149	control/supplier_accrual.go
146	control/realtime_usage_bound_test.go
144	control/batch_policy_test.go
144	control/webhook_secret_test.go
142	control/eta_history_test.go
141	control/ratelimit.go
139	control/realtime_pricing_decision_test.go
139	control/scheduler_ask_claim_integration_test.go
139	control/shape_routing.go
137	control/render_assembly.go
137	control/task_artifact_path_test.go
136	control/authorization_matrix_test.go
136	control/verification_class.go
135	control/dev_authority.go
135	control/pricing_governance_test.go
135	control/realtime_identity_cache.go
134	control/directed_routing_admin_test.go
133	control/verification_attempt.go
132	control/service_lease_payout_funding_test.go
132	control/stripe_settlement_test.go
132	control/tenant_isolation_test.go
131	control/project_materialize_test.go
130	control/accounts_free_credit_envelope_test.go
130	control/evidence_binding_payload_guard_test.go
129	control/project_contracts.go
128	control/media_contract_test.go
127	control/payment_authority_cli.go
127	control/realtime_reuse_pricing_decision.go
126	control/verification_resources_test.go
125	control/accounts_login_guard_test.go
125	control/ledger_write_property_test.go
124	control/hw_cost_rank_test.go
124	control/media_contract.go
123	control/webhook_delivery_test.go
121	control/gateway_parity_wave_block_test.go
120	control/execution_envelope_expiry_hold_test.go
119	control/repricing_benchmark_authority_test.go
116	control/connect.go
115	control/render_contract.go
113	control/directed_enrollment_test.go
112	control/release_ui.go
112	control/technical_tabletop_integration_test.go
111	control/charge_retry_test.go
111	control/media_merge.go
110	control/latency_watchdog.go
110	control/placement_readiness_test.go
110	control/strict_json.go
108	control/cuda_admission_test.go
107	control/csp_release_test.go
107	control/earnings_hold_breakdown_test.go
106	control/receipt_tamper_test.go
104	control/operational_control_cache.go
104	control/worker_session_integration_test.go
103	control/realtime_reuse_pricing_decision_test.go
101	control/realtime_identity_cache_test.go
100	control/stripe_simulator_test.go
99	control/prefix_placement.go
98	control/exact_reuse_authority_test.go
98	control/public_surface_test.go
96	control/charge_threshold_test.go
95	control/admission_rank_freeze_test.go
95	control/pricing_restart_test.go
94	control/verification_artifact_test.go
93	control/payment_funding_test.go
92	control/execution_mode_wiring_test.go
91	control/pricing_citation_authority_test.go
91	control/service_lease_pricing_test.go
89	control/public_config.go
88	control/authorize_latency_remeasure_test.go
88	control/crypto_test.go
88	control/fabric_topology_planner.go
85	control/crypto.go
84	control/runtime_selector_rollback_test.go
83	control/model_onboarding_test.go
82	control/input_depth_schema_test.go
82	control/model_onboarding.go
81	control/billing_schema_integration_test.go
81	control/job_submit_validate_test.go
80	control/authority_isolation_test.go
78	control/project_calibration_test.go
77	control/notify.go
77	control/project_topology.go
76	control/fuzz_test.go
76	control/operational_controls_integration_test.go
76	control/pricing_policy.go
76	control/reuse_not_physical_metric_test.go
76	control/stripe_cash_outcome_integration_test.go
75	control/metrics_observability_test.go
75	control/public_config_test.go
74	control/isolated_testdb_test.go
74	control/request_body_limit_test.go
73	control/verification_artifact_policy.go
71	control/webhook_secret.go
70	control/execution_envelope_api.go
69	control/alpha_request.go
67	control/input_depth_stream_integration_test.go
66	control/reuse_underflow_test.go
65	control/reputation.go
64	control/network_market_liquidity.go
64	control/render_work_plan_test.go
63	control/buildinfo.go
62	control/support_tabletop_test.go
60	control/lora_dataset_probe_test.go
60	control/recovery_boundary.go
59	control/admin_workers_integration_test.go
55	control/verification_apply_test.go
55	control/work_elimination_bench_test.go
53	control/lora_evaluation_api.go
53	control/release_ui_test.go
52	control/accounts_free_credit_test.go
50	control/hardening_secret_test.go
50	control/ratelimit_test.go
49	control/batch_exact_meter_test.go
49	control/earnings_carry_test.go
48	control/prove_registry.go
47	control/execution_mode_values_test.go
47	control/stripe_api_contract.go
44	control/pricing_extra.go
42	control/verification_sampling_test.go
41	control/reconcile_test.go
40	control/runtime_model_ingress_test.go
40	control/verification_attempt_cardinality_test.go
39	control/pricing_viability_test.go
39	control/testdb_test.go
34	control/batch_policy_wiring_test.go
34	control/idempotency_test.go
33	control/runtime_profile_testhelper_test.go
32	control/verification_artifact_policy_test.go
31	control/max_usd_test.go
31	control/task_artifact_path.go
30	control/storage_memory_test.go
29	control/render_contract_test.go
28	control/tiebreak_retry_safety_test.go
27	control/failure_test.go
27	control/receipt_label_test.go
27	control/worker_leader_test.go
26	control/remote_response.go
23	control/quote_stats_test.go
```

---

## Appendix B — Execution checklist

- [ ] P1 gitignore
- [ ] P2 clients/
- [ ] P3 ops/monitoring/
- [ ] P4 web/logo/ + provenance
- [ ] P5 evidence/{proof,census,artifacts}
- [ ] P6 immutable-fixtures + workload-catalog
- [ ] P7 docs no-loss merges
- [ ] P8 control free compression
- [ ] P9 optional named (default skip)
- [ ] P10 regenerate census/checkpoint/bindings
- [ ] P11 residue scan + inventory diff
- [ ] Fix CODEOWNERS `ledger.go` → `ledger_write.go`
- [ ] Never move `publish-candidate.yml` or `Dockerfile.control`
- [ ] Never rewrite `fetch("/pricing/board.json")`
- [ ] Never delete untracked `review/` without checksummed relocate

---

*End of plan. The next run that executes this plan must re-read this file and
re-run the measurement commands if HEAD has moved.*

# Lane B — test truth at 7c05e7f01fc29db497bee78220f608d1aa4f7746

Measured 2026-08-24 against this worktree. Go module is `merc/control`
(`control/go.mod` only). Host Go is `go1.26.2 darwin/arm64`; `control/go.mod`
declares `go 1.26.6` with a patched stdlib comment. Sparse checkout roots on
disk: `agent`, `clients`, `control`, `evidence`, `pricing`, `render`, `scripts`.
`ops/`, `docs/`, and `web/` exist in git and are **not** materialized.
`evidence/perf/**` is on disk as Git LFS pointer files (`version https://git-lfs.github.com/spec/v1`), not smudged content.

Services present for this run:

- PostgreSQL 17 on `localhost:5432` (`merc-postgres-1`), DSN
  `postgres://cx:cx@localhost:5432/cx?sslmode=disable`
  (`MERC_TEST_DATABASE_URL`). Schema template
  `merc_schema_ddl_12a13faa22b5e389`.
- Isolated per-run MinIO via `scripts/with-isolated-test-storage.sh`
  (`MERC_TEST_S3_*`). Host MinIO on `:9000` was also healthy and unused by
  the suite wrappers.

`make test` additionally runs `cargo test` and
`scripts/verify-python-sdk-package.sh`. Those were **not** executed; VERIFY
minimum is the Go suite.

## Totals

| Layer | passed | failed | skipped |
| --- | ---: | ---: | ---: |
| Go packages (`go test ./...` in `control/`) | 0 | 1 | 0 |
| Top-level tests (incl. 9 `Fuzz*` seed runs) | 1776 | 31 | 42 |

Package summary line from the JSON record:

```
FAIL	merc/control	1205.914s
```

Process exit: `1`. Wall clock: 2026-08-24T03:00:18Z → 2026-08-24T03:20:27Z.

**No flake was observed.** Every failing test failed the same way on three
`-race -count=1` reruns. **No executed assertion failed on product behavior.**
All 31 failures are ENVIRONMENT-MISSING (sparse-checkout hole, Git LFS
pointer, or Stripe dashboard secrets). Tests that ran their assertions
passed (1776).

That is not the same claim as “0 known P0/P1 correctness defects”. Thirty-one
gates never reached their subject. Widening `ops/`, `docs/`, `web/` and
hydrating LFS (`scripts/hydrate-release-lfs.sh` / `git lfs pull`) is required
before those gates can speak.

## Toolchain smoke

### `go build ./...` (in `control/`)

```
===== go build ./... =====
build_exit=0
```

stdout/stderr empty.

From repo root (no `go.mod`):

```
pattern ./...: directory prefix . does not contain main module or its selected dependencies
```

exit 1. The Makefile target is `cd control && go build ./...`.

### `go vet ./...` (in `control/`)

```
===== go vet ./... =====
vet_exit=0
```

stdout/stderr empty.

### `go test … -run TestPlacementReadiness`

There is **no** test named `TestPlacementReadiness`.
`control/placement_readiness_test.go` defines
`TestCUDAEmbedCellIsMatchedIdentityNotProduct` and
`TestExistingCUDAInferCellIsNotAMatchedEmbedArm`.

Contract filter (in `control/`):

```
===== go test . -count=1 -timeout 5m -run TestPlacementReadiness -v =====
testing: warning: no tests to run
PASS
ok  	merc/control	0.341s [no tests to run]
```

The tests that file actually contains:

```
=== RUN   TestCUDAEmbedCellIsMatchedIdentityNotProduct
--- PASS: TestCUDAEmbedCellIsMatchedIdentityNotProduct (0.02s)
=== RUN   TestExistingCUDAInferCellIsNotAMatchedEmbedArm
--- PASS: TestExistingCUDAInferCellIsNotAMatchedEmbedArm (0.00s)
PASS
ok  	merc/control	0.359s
```

From repo root, `go test ./control/... -run TestPlacementReadiness` fails setup:

```
# ./control/...
pattern ./control/...: directory prefix control does not contain main module or its selected dependencies
FAIL	./control/... [setup failed]
FAIL
```

## Full suite command (real)

Same wrappers as `make test` (isolated DB clone + isolated MinIO), JSON for a
complete failure list, `-count=1`, 45m budget:

```
cd control
export MERC_TEST_DATABASE_URL='postgres://cx:cx@localhost:5432/cx?sslmode=disable'
export MERC_ISOLATED_TEST_DB_TEMPLATE=merc_schema_ddl_12a13faa22b5e389
bash ../scripts/with-isolated-test-storage.sh \
  bash ../scripts/with-isolated-test-db.sh \
  go test -timeout 45m -parallel 16 -count=1 -json ./...
```

Recorded at `/tmp/mercaudit/gotest.json.log` (2 772 170 bytes). Package event:

```
{"Time":"2026-08-23T23:20:26.326353-04:00","Action":"output","Package":"merc/control","Output":"FAIL\n"}
{"Time":"2026-08-23T23:20:26.331475-04:00","Action":"output","Package":"merc/control","Output":"FAIL\tmerc/control\t1205.913s\n"}
{"Time":"2026-08-23T23:20:26.331486-04:00","Action":"fail","Package":"merc/control","Elapsed":1205.914}
```

`go test ./...` from repo root is not a module (see build above). There are no
other `go.mod` files. `scripts/gateway-concurrency-sweep.go` and
`scripts/gateway-parity-v2.go` are standalone `package main` programs, not part
of `merc/control`.

`-tags=integration` (`TestFirstCompleteLoopThroughThePublicAPI`) is **not** in
`go test ./...`; `make test-integration` / `make ci` run it separately. Not
executed here.

## Failing tests

Package for every row: `merc/control`. Classification from three `-race
-count=1` reruns of the same `-run` set (plus a DB-env rerun of
`TestL2StripeWebhookMatrixAgainstRealHandlers`, which the first race set
skipped because `MERC_TEST_DATABASE_URL` was unset and `requireL2TestDatabase`
calls `t.Skip`). All 31 are DETERMINISTIC × ENVIRONMENT-MISSING. Flakes: none.

| Test | Shortest decisive line | Class | Missing |
| --- | --- | --- | --- |
| TestP1IndependentApprovalFollowsGoNoGoLedger | `alpha_candidate_honesty_test.go:184: read go-no-go: open …/ops/go-no-go.json: no such file or directory` | ENVIRONMENT-MISSING | sparse: `ops/go-no-go.json` (in git) |
| TestCitedCanaryComposeOverlayIsTracked | `alpha_candidate_honesty_test.go:244: open …/docs/ALPHA_LAUNCH_READINESS.md: no such file or directory` | ENVIRONMENT-MISSING | sparse: `docs/ALPHA_LAUNCH_READINESS.md` (in git) |
| TestGoNoGoLiveMoneyRemainsProhibited | `alpha_candidate_honesty_test.go:262: open …/ops/go-no-go.json: no such file or directory` | ENVIRONMENT-MISSING | sparse: `ops/go-no-go.json` (in git) |
| TestAlphaSecuritySuite | `alpha_security_suite_test.go:299: open ../ops/authorization-matrix.json: no such file or directory` | ENVIRONMENT-MISSING | sparse: `ops/authorization-matrix.json` (in git) |
| TestAuthorizationMatrixProtectedRoutesRejectAnonymousAndWrongCredentialNamespace | `authorization_matrix_test.go:30: open ../ops/authorization-matrix.json: no such file or directory` | ENVIRONMENT-MISSING | sparse: `ops/authorization-matrix.json` (in git) |
| TestStagingParticipantsAllowlistNamesSealedHashNotSupersededR5 | `canary_policy_test.go:132: read staging allowlist: open ../ops/staging/alpha-participants.json: no such file or directory` | ENVIRONMENT-MISSING | sparse: `ops/staging/alpha-participants.json` (in git) |
| TestOperatorReservedWorkerIDMatchesStagingAllowlistIdentity | `canary_policy_test.go:222: read staging allowlist: open ../ops/staging/alpha-participants.json: no such file or directory` | ENVIRONMENT-MISSING | sparse: `ops/staging/alpha-participants.json` (in git) |
| TestBenchmarkManifestIdentityMatchesTheReceipts | `cell_authority_binding_test.go:329: evidence/perf/runtime-benchmarks/engine-tournament-metal-host-scope-r1.json is not JSON: invalid character 'v' looking for beginning of value` | ENVIRONMENT-MISSING | Git LFS pointer, not smudged (`version https://git-lfs…`) |
| TestBenchmarkManifestExactExecutionIdentityCannotDivergeFromReceipt | `cell_authority_binding_test.go:479: invalid character 'v' looking for beginning of value` | ENVIRONMENT-MISSING | Git LFS pointer under `evidence/perf/runtime-benchmarks/` |
| TestCaddyCSPHashesExactlyBindShippedInlineAssets | `csp_release_test.go:49: open ../web/index.html: no such file or directory` | ENVIRONMENT-MISSING | sparse: `web/index.html` (in git) |
| TestPhase6BoundEnergyAuthorityOnDisk | `directive_phase6_economics_test.go:178: load energy authority: invalid character 'v' looking for beginning of value` | ENVIRONMENT-MISSING | LFS pointer `evidence/perf/ioreport-gpu-energy-authority.json` |
| TestWaveBlockRecomputeLocalMetalReceipt | `gateway_parity_wave_block_test.go:23: invalid character 'v' looking for beginning of value` | ENVIRONMENT-MISSING | LFS pointer `evidence/perf/gateway-parity-v2-local-metal.json` |
| TestQualityContractFilesStayInLockstep | `heterogeneous_admission_test.go:213: open ../ops/acceptable-quality-contracts.json: no such file or directory` | ENVIRONMENT-MISSING | sparse: `ops/acceptable-quality-contracts.json` (in git) |
| TestPublicPriceBoardPageAgreesWithTheServerAuthority | `price_board_parity_test.go:80: run price page script: exit status 1` | ENVIRONMENT-MISSING | sparse: `web/prices.html` (in git). Reproduced: `Error: ENOENT: no such file or directory, open '…/web/prices.html'` from `scripts/price-board-page-prices.mjs` |
| TestPublicPriceBoardPageShowsUnavailableWithoutCurrentAuthority | `price_board_parity_test.go:101: run price page script: exit status 1` | ENVIRONMENT-MISSING | same `web/prices.html` |
| TestPriceBoardWeightingCannotOverrideGovernedSchedule | `price_board_parity_test.go:144: run price page script: exit status 1` | ENVIRONMENT-MISSING | same `web/prices.html` |
| TestBuyerSurfaceCallsEveryRequiredBuyerCapability | `public_surface_test.go:18: open ../web/buyer.html: no such file or directory` | ENVIRONMENT-MISSING | sparse: `web/buyer.html` (in git) |
| TestSupplierSurfaceSeparatesOwnerAndWorkerAuthority | `public_surface_test.go:49: open ../web/supplier.html: no such file or directory` | ENVIRONMENT-MISSING | sparse: `web/supplier.html` (in git) |
| TestDeploymentWiresPublicBrowserAndContactAuthority | `public_surface_test.go:76: open ../ops/staging/compose.go-closure.yml: no such file or directory` | ENVIRONMENT-MISSING | sparse: `ops/staging/compose.go-closure.yml` (in git) |
| TestReleaseImageShipsEveryFileTheRouterServes | `release_artifact_test.go:58: the router serves "GET /.well-known/security.txt" but web/.well-known does not exist` | ENVIRONMENT-MISSING | sparse: `web/.well-known`, `web/assets/site` (in git) |
| TestRemoteProfileDigestBindsEveryDeclaredInput | `release_launch_test.go:137: open ../ops/go-closure-inputs.json: no such file or directory` | ENVIRONMENT-MISSING | sparse: `ops/go-closure-inputs.json` (in git) |
| TestBuildLaunchInputsReportsMissingContractEntries | `release_launch_test.go:212: open ../ops/go-closure-inputs.json: no such file or directory` | ENVIRONMENT-MISSING | sparse: `ops/go-closure-inputs.json` (in git) |
| TestEveryRepricingBenchmarkMatchesItsCitedReceipt | `repricing_benchmark_authority_test.go:40: cited receipt is not JSON: invalid character 'v' looking for beginning of value` (subtest `…/llama-3.2-1b-instruct-q4/batch_infer`) | ENVIRONMENT-MISSING | Git LFS pointer on cited receipt JSON |
| TestBenchmarkManifestMatchesTheReceipts | `runtime_authority_v2_test.go:481: evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r2.json is not JSON: invalid character 'v' looking for beginning of value` | ENVIRONMENT-MISSING | Git LFS pointers under `evidence/perf/runtime-benchmarks/` |
| TestLlamaCppBenchmarkReceiptIsBoundAndHonestAboutItsLimits | `runtime_authority_v2_test.go:538: decode receipt: invalid character 'v' looking for beginning of value` | ENVIRONMENT-MISSING | LFS pointer `evidence/perf/runtime-benchmarks/llama-cpp-metal-llama1-q4-r1.json` |
| TestManifestThroughputIsDerivableFromTheReceipts | `runtime_cell_performance_test.go:893: evidence/perf/runtime-benchmarks/candle-metal-rendering-r1.json is not JSON: invalid character 'v' looking for beginning of value` | ENVIRONMENT-MISSING | Git LFS pointers under `evidence/perf/runtime-benchmarks/` |
| TestWithdrawnPairedCohortCannotOrderActualWinner | `runtime_governed_comparison_test.go:205: invalid character 'v' looking for beginning of value` | ENVIRONMENT-MISSING | LFS pointer `evidence/perf/selector/paired-cohort-embed.json` |
| TestBoundEngineParityReceiptIsLatencyOnlyAndCannotAuthorizeSelection | `runtime_governed_comparison_test.go:815: BOUND engine-parity latency rows = 0, want 2` | ENVIRONMENT-MISSING | `boundEmbedLatencies` reads LFS pointer `evidence/perf/selector/engine-parity-metal-embed-latest.json`; unmarshal fails and the helper returns nil |
| TestBoundLatencyWithoutSupplierActualsProducesNoSelectionOrQualityVerdict | `runtime_governed_comparison_test.go:838: BOUND engine-parity latency rows = 0, want 2` | ENVIRONMENT-MISSING | same LFS pointer |
| TestL2StripeWebhookMatrixRequiresDashboardSecrets | `stripe_l2_webhook_matrix_test.go:75: dashboard webhook secrets required` | ENVIRONMENT-MISSING | env `STRIPE_WEBHOOK_SECRET` and `MERC_CONNECT_WEBHOOK_SECRET` (distinct `whsec_` pair). This test is written to fail-closed when they are absent (`make test-money-contract` is the gated target). |
| TestL2StripeWebhookMatrixAgainstRealHandlers | `stripe_l2_webhook_matrix_test.go:82: dashboard webhook secrets required` | ENVIRONMENT-MISSING | same Stripe dashboard pair; also needs `MERC_TEST_DATABASE_URL` (present in the full run) |

`invalid character 'v'` is the JSON parser hitting the first byte of a Git LFS
pointer (`version https://git-lfs.github.com/spec/v1`). Example on disk:

```
version https://git-lfs.github.com/spec/v1
oid sha256:5259dda030376ee7eea08aff02cbc3d69f1f4e3fc07c16a8958ab4034d734253
size 1403
```

## Race classification (3×)

Command (31-name anchored `-run`, `-race -count=1`, three times). First trio
did not export `MERC_TEST_DATABASE_URL`, so
`TestL2StripeWebhookMatrixAgainstRealHandlers` skipped (`requireL2TestDatabase`).
The other 30 failed identically each time. Then the Stripe handler test was
rerun three times with the DSN set; it failed on secrets every time.

Race run 1 / 2 / 3 package lines:

```
FAIL	merc/control	0.751s
FAIL	merc/control	0.711s
FAIL	merc/control	0.715s
```

Each listed the same 30 `--- FAIL:` names. No `--- PASS:`. No race detector
report in stderr.

AgainstRealHandlers with DSN, three times:

```
--- FAIL: TestL2StripeWebhookMatrixAgainstRealHandlers (0.00s)
    stripe_l2_webhook_matrix_test.go:82: dashboard webhook secrets required
FAIL
FAIL	merc/control	0.384s
FAIL
```

elapsed 0.384s, 0.385s, 0.389s. DETERMINISTIC.

## Money / authority / containment class

`-run` (Go regexp, case variants because `-run` is not substring-unanchored
the way `grep -i` is):

```
[Mm]oney|[Ss]ettle|[Pp]ayout|[Ss]tripe|[Ww]ebhook|[Aa]uthority|[Ee]ntitle|[Ll]edger|[Ss]andbox|[Cc]ontain|[Ee]scrow|[Rr]efund
```

`go test -list` of that pattern: **331** tests. Isolated DB + MinIO, `-count=1`,
`-json`:

```
FAIL	merc/control	325.997s
```

Top-level: **320 pass / 9 fail / 2 skip**. Exit 1.

The 9 failures are exactly the name-matching subset of the table above:

- TestP1IndependentApprovalFollowsGoNoGoLedger
- TestGoNoGoLiveMoneyRemainsProhibited
- TestPhase6BoundEnergyAuthorityOnDisk
- TestPublicPriceBoardPageAgreesWithTheServerAuthority
- TestPublicPriceBoardPageShowsUnavailableWithoutCurrentAuthority
- TestSupplierSurfaceSeparatesOwnerAndWorkerAuthority
- TestDeploymentWiresPublicBrowserAndContactAuthority
- TestL2StripeWebhookMatrixRequiresDashboardSecrets
- TestL2StripeWebhookMatrixAgainstRealHandlers

Skips in this class:

- TestL2HoldWebhookServer — `MERC_L2_HOLD` unset
- TestBothAgentsSettleThroughTheProductionPath — no
  `agent/target/release/merc-agent`

Ledger/settlement/payout/prepaid/stripe-simulator/containment-identity tests
that do not need those missing files **passed**, including
`TestMoneyInvariantsHoldUnderRandomOperationOrderings`,
`TestLoRASettlementConservesMoney`,
`TestExactMoneyRefusesToMixCurrencies`,
`TestAccrualConservesMoneyUnderConcurrentClaims`.

Class verdict: **FAIL**, entirely ENVIRONMENT-MISSING. No money-path assertion
that executed was red.

## Skips (42 top-level)

Not failures. Env-gated measurements, missing `merc-agent` release binary, or
an honest product skip.

| Test | Reason |
| --- | --- |
| TestArrivalBatchingPerfSweep | `MERC_ARRIVAL_BATCH_PERF=1` |
| TestAuthorizeLatencyRemeasureAfterHierarchy | `MERC_AUTH_LATENCY_REMEASURE=1` |
| TestAuthorizeTailCharacterize | `MERC_AUTHORIZE_TAIL_PROBE=1` |
| TestBackupAgeMetricObservation | `MERC_BACKUP_STATUS_FILE` unset |
| TestBlenderRenderBaseline | `MERC_BLENDER_RENDER_BENCH=1` |
| TestBothAgentsExecuteADirectedJobEndToEnd | no `agent/target/release/merc-agent` |
| TestBothAgentsProduceVerifiableReceipts | no `merc-agent` |
| TestBothAgentsSettleThroughTheProductionPath | no `merc-agent` |
| TestControlPlaneHotPathProfile | `MERC_CONTROL_PLANE_PROFILE=1` |
| TestDirectedJobsReachOnlyTheIntendedAgent | no `merc-agent` |
| TestFailureMatrixAgentDeathAfterClaim | no `merc-agent` |
| TestFailureMatrixAgentDeathBeforeClaim | no `merc-agent` |
| TestFailureMatrixRuntimeUnavailable | no `merc-agent` |
| TestGatewayParityAgainstRealEngine | `MERC_GATEWAY_PARITY=1` (+ live upstream) |
| TestHeartbeatIngestBench | `MERC_HEARTBEAT_INGEST_BENCH=1` |
| TestHotPathFreeAdmitProbe | `MERC_HOT_PATH_FREE_PROBE=1` |
| TestL2HoldWebhookServer | `MERC_L2_HOLD` unset |
| TestLiveDeviceIndexBench | `MERC_LIVENESS_INDEX_BENCH=1` |
| TestLiveServingMatrixCandleVsLlamaCppMetal | `MERC_RUN_LIVE_SERVING_MATRIX` unset |
| TestLiveServingMatrixLlamaCppMetal | `MERC_RUN_LIVE_SERVING_MATRIX` unset |
| TestLivenessWriteAmplificationBench | `MERC_WRITE_AMP_BENCH=1` |
| TestMercLatencyGapAccounting | `MERC_LATENCY_GAP_ACCOUNTING=1` |
| TestMercSegmentLatencyMeasure | `MERC_SEGMENT_LATENCY_MEASURE=1` |
| TestMutationTemplateSchema | supervisor-only (`MERC_MUTATION_TEMPLATE_DB`) |
| TestPairedCohortMeasuresCostAndRegret | `MERC_PAIRED_COHORT` is not 1 |
| TestPhase6WriteEvidencePayload | `MERC_PHASE6_ECONOMICS_WRITE=1` |
| TestPrefixKVHitRateAgentRAGProductionClaim | `MERC_PREFIX_KV_HITRATE=1` |
| TestRealtimeAuthLatencyProbe | `MERC_AUTH_LATENCY_PROBE=1` |
| TestRealtimeCandidateProjectionABOnOneDatabase | `MERC_REALTIME_CANDIDATE_AB=1` |
| TestRealtimeSelectionCostDecomposition | `MERC_REALTIME_DECOMP=1` |
| TestRecoveryLaneNetworkInterruption | `scripts/recovery-suite.sh` + `MERC_RECOVERY_SUITE=1` |
| TestRecoveryLaneObjectStoreRestart | same |
| TestRecoveryLanePostgresRestart | same |
| TestRejectedVerificationLeavesNoSupplierCredit | no `merc-agent` |
| TestRenderDevicePlacementBench | `MERC_RENDER_DEVICE_PLACEMENT=1` |
| TestRenderVerifyPipelineBench | `MERC_RENDER_VERIFY_PIPELINE=1` |
| TestSelectorScaleCurve | `MERC_SELECTOR_SCALE_CURVE=1` |
| TestShadowIndexPerOfferGrainNoSiblingRescue | honest skip: one advertised profile (1:1) |
| TestTwoDistinctAgentsEnrolAutonomously | no `merc-agent` |
| TestWriteCellEconomicsCensusReceipt | `MERC_CELL_ECONOMICS_RECEIPT` is not 1 |
| TestWriteEconomicSelectorProofReceipt | `MERC_ECONOMIC_SELECTOR_PROOF_RECEIPT` is not 1 |
| TestWriteGovernedComparisonReceipt | `MERC_GOVERNED_COMPARISON_RECEIPT` is not 1 |

`MERC_LLAMA_EMBED_URL` was unset; several two-agent tests skip on the agent
binary before they would skip on the engine URL.

## Other materialized trees (not executed)

| Tree | What is there | Test truth this run |
| --- | --- | --- |
| `agent/` | Rust `merc-agent`, 137 `#[test]` sites, `cargo test` in `make test` / `make ci` | not run |
| `clients/sdk/python` | installed-wheel tests via `scripts/verify-python-sdk-package.sh` | not run |
| `clients/sdk/typescript` | `test/client.test.js` (node:test) | not run |
| `clients/macapp/.../sandbox-profile-test.sh` | seatbelt profile | not run |
| `pricing/` | `board.json` only (consumed by control tests; `TestPriceBoardObservationsAreAttributed` passed) | n/a |
| `render/` | Python harness, no in-tree pytest target in VERIFY | not run |
| `scripts/` | many gate scripts (`test-placement-readiness.py`, `alpha-security-suite.py`, stripe sandbox, mutation, …) | not run |

## Suite side effect (reverted)

During `go test ./...`, some test rewrote the tracked file
`evidence/canary/l12-p1-canary-rehearsal-quote-refusal-chain.json`
(`observed_at` bumped to 2026-08-24T03:07:57Z; `binding_status` /
`missing_identity_fields` dropped). That path is outside this lane's write
scope and was restored with `git checkout --` after the run. The suite is
not read-only against `evidence/canary/`.

## Blockers if the launcher wants those 31 gates to execute

Do **not** `git sparse-checkout add` from this sandbox (lock is EPERM). Widen
these roots and relaunch, then hydrate LFS:

1. `ops/` — `go-no-go.json`, `authorization-matrix.json`,
   `acceptable-quality-contracts.json`, `go-closure-inputs.json`,
   `staging/alpha-participants.json`, `staging/compose.go-closure.yml`
2. `docs/` — `ALPHA_LAUNCH_READINESS.md`
3. `web/` — `index.html`, `buyer.html`, `supplier.html`, `prices.html`,
   `.well-known/`, `assets/site/`
4. Git LFS smudge of `evidence/perf/**` (CI: `bash scripts/hydrate-release-lfs.sh`)
5. For the L2 Stripe matrix: `STRIPE_WEBHOOK_SECRET` +
   `MERC_CONNECT_WEBHOOK_SECRET` (distinct `whsec_`) and a test
   `STRIPE_SECRET_KEY` (`make test-money-contract`)

## LANE B RESULT

```
LANE B RESULT
build: PASS
vet: PASS
packages: 0 passed / 1 failed / 0 skipped
  (test-level: 1776 passed / 31 failed / 42 skipped; package merc/control FAIL 1205.914s)
money-authority-containment suites: FAIL (9 ENVIRONMENT-MISSING of 331 name-matches; 320 pass, 2 skip)
deterministic failures: none that executed a product assertion
  (31 DETERMINISTIC ENVIRONMENT-MISSING — list in table above)
flakes: none
environment-missing: the 31 tests in the failure table
```

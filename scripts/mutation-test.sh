#!/usr/bin/env bash
# Mutation testing for the money and reuse paths.
#
# A passing suite proves the tests ran, not that they would have caught
# anything. This deliberately injects defects into production code and asserts
# the suite FAILS for each one. A mutation that survives is a hole in the tests,
# not a success.
#
#   bash scripts/mutation-test.sh
#
# Every mutation is reverted whether it is caught or not; the tree is restored
# on any exit path. Filtered contract mutations that do not exercise persistence
# can opt into the fast suite with MERC_MUTATION_UNIT_ONLY=1; the default remains
# the database-backed suite. The parallel checkpoint runner uses the separately
# audited `adaptive` strategy: the fast, database-free package suite runs first,
# then any survivor must fail an explicit finite database contract in
# scripts/mutation-test-contracts.json. A mutation still has to make a real test
# fail; this only avoids re-running unrelated database integration tests.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

CONTROL=control
MERC_MUTATION_FILTER="${MERC_MUTATION_FILTER:-}"
MERC_MUTATION_UNIT_ONLY="${MERC_MUTATION_UNIT_ONLY:-0}"
MERC_MUTATION_LIST="${MERC_MUTATION_LIST:-0}"
MERC_MUTATION_LIST_DETAIL="${MERC_MUTATION_LIST_DETAIL:-0}"
MERC_MUTATION_CASE_IDS="${MERC_MUTATION_CASE_IDS:-}"
MERC_MUTATION_DB_PREFIX="${MERC_MUTATION_DB_PREFIX:-merc_mutation}"
MERC_MUTATION_TEST_STRATEGY="${MERC_MUTATION_TEST_STRATEGY:-full}"
MERC_MUTATION_DB_TEMPLATE="${MERC_MUTATION_DB_TEMPLATE:-}"
MERC_MUTATION_TIMINGS_FILE="${MERC_MUTATION_TIMINGS_FILE:-}"
MERC_MUTATION_GLOBAL_UNIT_PREFLIGHT="${MERC_MUTATION_GLOBAL_UNIT_PREFLIGHT:-0}"
MERC_MUTATION_PREFLIGHT_CACHE="${MERC_MUTATION_PREFLIGHT_CACHE:-}"
MERC_MUTATION_DRY_ORDER="${MERC_MUTATION_DRY_ORDER:-0}"
MUTATION_LOCK=""
BACKUP=""
MUTATION_OBSERVATION=""
MUTATION_PATHWAY=""

case "$MERC_MUTATION_TEST_STRATEGY" in
  full|contracts|adaptive) ;;
  *)
    echo "MERC_MUTATION_TEST_STRATEGY must be full, contracts, or adaptive" >&2
    exit 2
    ;;
esac

if [ "$MERC_MUTATION_LIST" != "1" ]; then
  if [ "$MERC_MUTATION_TEST_STRATEGY" != "full" ] && [ "$MERC_MUTATION_UNIT_ONLY" = "1" ]; then
    echo "MERC_MUTATION_UNIT_ONLY cannot be combined with a contract mutation strategy" >&2
    exit 2
  fi
  if [ "$MERC_MUTATION_UNIT_ONLY" = "1" ]; then
    [ -n "$MERC_MUTATION_FILTER" ] || {
      echo "MERC_MUTATION_UNIT_ONLY=1 requires a narrow MERC_MUTATION_FILTER" >&2
      exit 2
    }
  else
    : "${MERC_TEST_DATABASE_URL:?mutation testing needs a database}"
  fi

  repo_lock_id="$(printf '%s' "$PWD" | shasum -a 256 | cut -c1-16)"
  MUTATION_LOCK="${TMPDIR:-/tmp}/merc-mutation-${repo_lock_id}.lock"
  if ! mkdir "$MUTATION_LOCK" 2>/dev/null; then
    echo "another mutation-test process owns $MUTATION_LOCK; refusing concurrent source mutation" >&2
    exit 2
  fi

  BACKUP="$(mktemp -d "${TMPDIR:-/tmp}/merc-mutation.XXXXXX")"
fi

if [ "$MERC_MUTATION_DRY_ORDER" != "0" ] && [ "$MERC_MUTATION_DRY_ORDER" != "1" ]; then
  echo "MERC_MUTATION_DRY_ORDER must be 0 or 1" >&2
  exit 2
fi

cleanup() {
  # Restore every file touched, always.
  if [ -n "$BACKUP" ] && [ -d "$BACKUP" ]; then
    for f in "$BACKUP"/*.bak; do
      [ -e "$f" ] || continue
      base="$(basename "$f" .bak)"
      cp "$f" "$CONTROL/${base//__//}"
    done
    rm -rf "$BACKUP"
  fi
  if [ -n "$MUTATION_LOCK" ]; then
    rmdir "$MUTATION_LOCK" 2>/dev/null || true
  fi
}

# A signal must restore AND STOP. Sharing one handler across EXIT and the
# signals looked equivalent and was not: bash runs a TERM handler and then
# RESUMES the loop, so cleanup deleted $BACKUP and every mutation after it was
# written to a file whose backup no longer existed. Interrupting a run left the
# working tree holding injected defects, with `cp: .../pricing_decision.go.bak:
# No such file or directory` scrolling past as the only warning — observed on
# 2026-08-04, eight files deep, recoverable only because they were committed.
#
# The trap that exists to protect the tree was corrupting it on the one path
# people actually take, which is Ctrl-C.
on_signal() {
  echo "" >&2
  echo "mutation-test: signal received; restoring the tree and stopping" >&2
  cleanup
  exit 130
}
trap cleanup EXIT
trap on_signal INT TERM

run_unit_tests() {
  local label="${1:-unit}"
  local log
  log="${BACKUP:-${TMPDIR:-/tmp}}/mutation-unit-${label}.log"
  if (
    cd "$CONTROL" &&
      env -u MERC_TEST_DATABASE_URL MERC_ALLOW_SKIPPING_DB_TESTS=1 \
        go test -count=1 -timeout=2m ./... >"$log" 2>&1
  ); then
    return 0
  fi
  cat "$log" >&2 || true
  return 1
}

contract_selector() {
  local source="$1"
  local selector
  selector="$(python3 scripts/mutation-test-contracts.py --root . --source "$source" --selector)" || return 2
  [ -n "$selector" ] || {
    echo "mutation-test: contract selector is empty for control/$source" >&2
    return 2
  }
  printf '%s\n' "$selector"
}

observe_contract_log() {
  local source="$1" log="$2" status="$3" completion_requirement="${4:-}"
  local -a arguments
  local test_name
  arguments=(--log "$log" --exit-code "$status")
  case "$completion_requirement" in
    "") ;;
    all-run) arguments+=(--require-all-run) ;;
    all-pass) arguments+=(--require-all-pass) ;;
    *)
      echo "mutation-test: invalid contract completion requirement: $completion_requirement" >&2
      return 2
      ;;
  esac
  while IFS= read -r test_name; do
    [ -n "$test_name" ] || continue
    arguments+=(--expected "$test_name")
  done < <(python3 scripts/mutation-test-contracts.py --root . --source "$source")
  if ! MUTATION_OBSERVATION="$(python3 scripts/mutation-contract-observer.py "${arguments[@]}")"; then
    echo "mutation-test: contract execution is infrastructure, not a mutation catch: $MUTATION_OBSERVATION" >&2
    cat "$log" >&2 || true
    return 2
  fi
  return 0
}

run_unit_contract_tests() {
  local source="$1" label="$2" selector log status completion_requirement=""
  if [ "$label" = "baseline" ]; then
    completion_requirement="all-run"
  fi
  selector="$(contract_selector "$source")" || return 2
  log="${BACKUP:-${TMPDIR:-/tmp}}/mutation-contract-${label}-${source%.go}-unit.json"
  (
    cd "$CONTROL" &&
      env -u MERC_TEST_DATABASE_URL MERC_ALLOW_SKIPPING_DB_TESTS=1 \
        go test -json -count=1 -timeout=2m -run "$selector" .
  ) >"$log" 2>&1
  status=$?
  observe_contract_log "$source" "$log" "$status" "$completion_requirement"
}

run_db_contract_tests() {
  local source="$1" label="$2" selector log status completion_requirement=""
  if [ "$label" = "baseline" ]; then
    # A source contract may intentionally contain both a unit-only invariant
    # and a database-only invariant. Every named test must run here, while the
    # caller below separately requires at least one named database pass before
    # any mutant may rely on this fallback.
    completion_requirement="all-run"
  fi
  selector="$(contract_selector "$source")" || return 2
  log="${BACKUP:-${TMPDIR:-/tmp}}/mutation-contract-${label}-${source%.go}-db.json"
  # Contract tests are bounded independently of the full-package default. The
  # clean-source preflight and the mutant both execute this exact selector, so a
  # missing or renamed test can never silently count as caught.
  MERC_ISOLATED_TEST_DB_PREFIX="$MERC_MUTATION_DB_PREFIX" \
    MERC_ISOLATED_TEST_DB_TEMPLATE="$MERC_MUTATION_DB_TEMPLATE" \
    bash scripts/with-isolated-test-db.sh \
    bash -c 'cd "$1" && go test -json -count=1 -timeout=2m -run "$2" .' _ "$CONTROL" "$selector" \
    >"$log" 2>&1
  status=$?
  observe_contract_log "$source" "$log" "$status" "$completion_requirement"
}

run_contract_tests() {
  local source="$1"
  run_db_contract_tests "$source" "contract" || return $?
  case "$MUTATION_OBSERVATION" in
    caught:*) return 10 ;;
    pass:*) return 0 ;;
    *)
      echo "mutation-test: database contract did not execute a declared invariant for control/$source: $MUTATION_OBSERVATION" >&2
      return 2
      ;;
  esac
}

run_mutation_tests() {
  local source="$1"
  case "$MERC_MUTATION_TEST_STRATEGY" in
    contracts)
      run_contract_tests "$source"
      return $?
      ;;
    adaptive)
      # Run the exact source contract without a database first. This is not a
      # weaker suite: a clean preflight proves the same named invariants run;
      # a pass or deliberate DB skip must still fail the isolated DB contract.
      # That avoids charging every mutation for unrelated package tests.
      run_unit_contract_tests "$source" "mutant" || return $?
      case "$MUTATION_OBSERVATION" in
        caught:*)
          MUTATION_PATHWAY="PURE"
          return 10
          ;;
        pass:*|skipped:*)
          run_contract_tests "$source"
          case "$?" in
            10) MUTATION_PATHWAY="DB"; return 10 ;;
            0) MUTATION_PATHWAY="DB"; return 0 ;;
            *) return 2 ;;
          esac
          ;;
        *)
          echo "mutation-test: unexpected unit contract observation for control/$source: $MUTATION_OBSERVATION" >&2
          return 2
          ;;
      esac
      ;;
  esac
  if [ "$MERC_MUTATION_UNIT_ONLY" = "1" ]; then
    if run_unit_tests; then
      MUTATION_PATHWAY="UNIT_FULL"
      return 0
    fi
    MUTATION_PATHWAY="UNIT_FULL"
    return 10
  else
    # Every injected defect gets a clean database. Several money-path tests
    # correctly query platform-wide state; reusing one database across mutants
    # lets fixture residue hide a new row behind a LIMIT and makes a mutation
    # result describe the previous mutant's database rather than this source.
    MERC_ISOLATED_TEST_DB_PREFIX="$MERC_MUTATION_DB_PREFIX" \
      MERC_ISOLATED_TEST_DB_TEMPLATE="$MERC_MUTATION_DB_TEMPLATE" \
      bash scripts/with-isolated-test-db.sh \
      bash -c 'cd "$1" && go test -count=1 ./... >/dev/null 2>&1' _ "$CONTROL"
    if [ "$?" -eq 0 ]; then
      MUTATION_PATHWAY="FULL"
      return 0
    fi
    MUTATION_PATHWAY="FULL"
    return 10
  fi
}

selected_mutation_sources() {
  local source entry rest description candidate=0 checked_sources="|"
  for entry in "${MUTATIONS[@]}"; do
    candidate=$((candidate + 1))
    if ! case_is_selected "$candidate"; then
      continue
    fi
    source="${entry%%|*}"
    rest="${entry#*|}"
    description="${rest%%|*}"
    if [ -n "$MERC_MUTATION_FILTER" ] && [[ "$description" != *"$MERC_MUTATION_FILTER"* ]]; then
      continue
    fi
    case "$checked_sources" in
      *"|$source|"*) continue ;;
    esac
    checked_sources="${checked_sources}${source}|"
    printf '%s\n' "$source"
  done
}

preflight_mutation_strategy() {
  local source sources_file
  case "$MERC_MUTATION_TEST_STRATEGY" in
    full)
      return 0
      ;;
    adaptive)
      if [ "$MERC_MUTATION_GLOBAL_UNIT_PREFLIGHT" = "1" ] && ! run_unit_tests "baseline"; then
        echo "mutation-test: clean unit-suite preflight failed" >&2
        return 1
      fi
      ;;
  esac
  sources_file="${BACKUP:-${TMPDIR:-/tmp}}/mutation-preflight-sources"
  selected_mutation_sources >"$sources_file"
  if [ ! -s "$sources_file" ]; then
    echo "mutation-test: selected no sources for contract preflight" >&2
    return 1
  fi
  if [ -n "$MERC_MUTATION_PREFLIGHT_CACHE" ]; then
    if ! python3 scripts/mutation-preflight-cache.py \
      --root . --cache "$MERC_MUTATION_PREFLIGHT_CACHE" --sources "$sources_file" --verify; then
      echo "mutation-test: aggregate preflight cache is not valid for this exact mutation shard" >&2
      return 1
    fi
    return 0
  fi
  while IFS= read -r source; do
    [ -n "$source" ] || continue
    if ! run_unit_contract_tests "$source" "baseline"; then
      echo "mutation-test: clean unit contract preflight failed for control/$source" >&2
      return 1
    fi
    case "$MUTATION_OBSERVATION" in
      pass:*|skipped:*) ;;
      *)
        echo "mutation-test: clean unit contract did not establish a valid baseline for control/$source: $MUTATION_OBSERVATION" >&2
        return 1
        ;;
    esac
    if ! run_db_contract_tests "$source" "baseline" || [[ "$MUTATION_OBSERVATION" != pass:* ]]; then
      echo "mutation-test: clean contract preflight failed for control/$source" >&2
      return 1
    fi
  done <"$sources_file"
}

# file|description|sed-expression
MUTATIONS=(
"money_nanos.go|pricing divides before scaling to nanos|s#int64(units), NanosPerMajorUnit, int64(throughput), true,#int64(units)/int64(throughput), NanosPerMajorUnit, 1, true,#"
"money_nanos.go|pricing compares amounts with mismatched currencies|s#if m.Currency.Code() != other.Currency.Code() {#if false {#"
"pricing_decision.go|pricing ceilings fractional work units to integers|s#units := NanoWorkUnitsFromFloat(unitsPerTask)#units := NanoWorkUnitsFromFloat(math.Ceil(unitsPerTask))#"
"money_nanos.go|pricing quantises buyer gross to micros before supplier share|s#mulDiv(gross.Nanos, shareNanos, NanosPerMajorUnit, true)#mulDiv(gross.Nanos/NanosPerMicro*NanosPerMicro, shareNanos, NanosPerMajorUnit, true)#"
"pricing_decision.go|supplier floor uses reference price while entitlement uses settlement authority|s#return nanosPer1KFromFloat(a.SettlementPricePer1K \* tierMultiplier(tier))#return nanosPer1KFromFloat(a.ReferencePricePer1K * tierMultiplier(tier))#"
"pricing_decision.go|catalogue and compute plan use divergent billable units|s#input = compute.SettlementInputUnits#input = float64(compute.EstimatedInputTokens)#"
"pricing_decision.go|exact supplier floor is silently zeroed|s#supplierRequiredNanos = required.Nanos#supplierRequiredNanos = required.Nanos - required.Nanos#"
"money_nanos.go|buyer gross rounds upward beyond its ceiling|s#int64(price), int64(units), 1_000\*NanosPerMajorUnit, false)#int64(price), int64(units), 1_000*NanosPerMajorUnit, true)#"
"money_nanos.go|supplier entitlement rounds downward below its floor|s#mulDiv(gross.Nanos, shareNanos, NanosPerMajorUnit, true)#mulDiv(gross.Nanos, shareNanos, NanosPerMajorUnit, false)#"
"money_nanos.go|realtime pricing divides the rate before multiplying tokens|s#mulDiv(int64(inputRate), promptTokens, 1_000_000, roundSupplierUp)#mulDiv(int64(inputRate)/1_000_000, promptTokens, 1, roundSupplierUp)#"
"money_nanos.go|realtime input is quantised to micros before token classes combine|s#return inputMoney.Add(outputMoney)#inputMoney.Nanos = inputMoney.Nanos / NanosPerMicro * NanosPerMicro; return inputMoney.Add(outputMoney)#"
"money_nanos.go|realtime buyer charge rounds in the supplier direction|s#realtimeTokenChargeNanos(c, promptTokens, completionTokens, inputRate, outputRate, false)#realtimeTokenChargeNanos(c, promptTokens, completionTokens, inputRate, outputRate, true)#"
"money_nanos.go|realtime supplier entitlement rounds in the buyer direction|s#realtimeTokenChargeNanos(c, promptTokens, completionTokens, inputRate, outputRate, true)#realtimeTokenChargeNanos(c, promptTokens, completionTokens, inputRate, outputRate, false)#"
"money_nanos.go|realtime reuse rounds full price to micros before retained share|s#nanos, err := mul3Div(int64(fullRate), deliveredTokens, realtimeReuseRetainedShareNanos, reusePriceDenominator, false)#fullNanos, err := mulDiv(int64(fullRate), deliveredTokens, 1_000_000, false); if err == nil { fullNanos = fullNanos / NanosPerMicro * NanosPerMicro }; nanos, err := mulDiv(fullNanos, realtimeReuseRetainedShareNanos, NanosPerMajorUnit, false)#"
"money_nanos.go|realtime reuse silently removes retained-share discount|s#realtimeReuseRetainedShareNanos = 400_000_000#realtimeReuseRetainedShareNanos = 1_000_000_000#"
"money_nanos.go|realtime reuse silently removes minimum delivery charge|s#nanos = realtimeReuseMinimumChargeNanos#nanos = 0#"
"realtime.go|realtime supplier offer accepts a zero floor|s#err != nil || supplierInput <= 0#err != nil || supplierInput < 0#"
"realtime.go|realtime supplier offer accepts a sub-nano contribution spread|s#if int64(buyerInput-supplierInput) < minimumRealtimeRateSpreadNanos ||#if false \&\&#"
"realtime_pricing_decision.go|realtime pricing accepts a divergent embedded profile|s#!reflect.DeepEqual(in.Profile, governedProfile)#reflect.DeepEqual(in.Profile, governedProfile)#"
"realtime_pricing_decision.go|realtime pricing accepts a currency mismatch|s#if err := RequireSettlementCurrency(in.Currency.Code()); err != nil {#if err := RequireSettlementCurrency(in.Currency.Code()); err != nil \&\& false {#"
"realtime_pricing_decision.go|realtime pricing ignores the buyer ceiling|s#buyerMaximum.Nanos > authority.BuyerDeclaredCeilingNanos#false#"
"realtime_reuse_pricing_decision.go|realtime reuse accepts a divergent embedded profile|s#!reflect.DeepEqual(governed, in.Profile)#reflect.DeepEqual(governed, in.Profile)#"
"realtime_reuse_pricing_decision.go|realtime reuse accepts a currency mismatch|s#if err := RequireSettlementCurrency(in.Currency.Code()); err != nil {#if err := RequireSettlementCurrency(in.Currency.Code()); err != nil \&\& false {#"
"realtime_reuse_pricing_decision.go|realtime reuse ignores the buyer ceiling|s#charge.Nanos > a.BuyerDeclaredCeilingNanos#false#"
"realtime_store.go|physical contract persistence drops PricingDecision authority|s#pricing.Realtime.BuyerDeclaredCeilingNanos, pricingJSON, pricingSHA256, marketJSON)#pricing.Realtime.BuyerDeclaredCeilingNanos, pricingJSON[:0], pricingSHA256, marketJSON)#"
"realtime_store.go|reuse contract persistence drops PricingDecision authority|s#money.DeliveredTokens, pricingJSON, pricingSHA256)#money.DeliveredTokens, pricingJSON[:0], pricingSHA256)#"
"realtime_store.go|persisted realtime PricingDecision digest mismatch is ignored|s#digest != contract.PricingDecisionSHA256#digest != digest#"
"realtime_store.go|legacy supplier projection may diverge from PricingDecision|s#SupplierInputRate:[[:space:]]*contract.SupplierInputUSDPerMillionTokens,#SupplierInputRate: float64(pricing.Realtime.SupplierInputNanosPerMillion) / float64(NanosPerMajorUnit),#"
"realtime_store.go|persisted reuse delivered tokens may diverge from PricingDecision|s#DeliveredTokens: contract.ReuseDeliveredTokens,#DeliveredTokens: pricing.RealtimeReuse.DeliveredTokens,#"
"realtime_store.go|reuse settlement invents a supplier nano liability|s#\$7,\$8,0,\$8)#\$7,\$8,1,\$8)#"
"realtime_store.go|reuse idempotency accepts a different request digest|s#existing.RequestSHA256 != auth.RequestSHA256#false#"
"realtime_store.go|reuse idempotency replay is bypassed|/func (s \*Store) SettleRealtimeExactReuse/,/Same fund gate/ s#if auth.IdempotencyKey != \"\" {#if false {#"
"realtime_store.go|verified usage may exceed frozen PricingDecision bounds|s#if evidence.PromptTokens > contract.MaximumPromptTokens ||#if false \&\&#"
"realtime_store.go|settlement substitutes buyer authority for supplier floor|s#authority.SupplierInputNanosPerMillion),#authority.BuyerInputNanosPerMillion),#"
"supplier_accrual.go|accrual adds instead of carrying the remainder|s|carryOut = effective % factor|carryOut = 0|"
"supplier_accrual.go|accrual rounds up instead of flooring cents|s|cashCents = effective / factor|cashCents = (effective + factor - 1) / factor|"
"supplier_accrual.go|supplier accrual lock removed|s| FOR UPDATE||"
"billing_classes.go|reused input counted as physical work|s|ClassPrefixReusedInput: false|ClassPrefixReusedInput: true|"
"billing_classes.go|reused tokens billed at the full rate|s|retained := 1.0 - reuseDiscountShare|retained := 1.0|"
"batch_policy.go|token budget ignored|s|if used+cost > budget {|if false {|"
"prefix_routing.go|prefix warmth ignores its TTL|s|AND last_seen_warm > now() - \$3::interval||"
"exact_reuse.go|non-deterministic requests become cacheable|s|return r.Temperature == 0 \&\& (r.TopP == 0 |return true \&\& (r.TopP == 0 |"
"exact_reuse.go|tenant-scoped references accepted|s|^var tenantScopedRefPattern = regexp.MustCompile(\`\^jobs/\`)|var tenantScopedRefPattern = regexp.MustCompile(\`^ZZZNEVERMATCH\`)|"
"workload_classification.go|supplier reputation omitted from workload binding|s|MinReputation: sub.MinReputation|MinReputation: 0|"
"workload_classification.go|input commitment omitted from workload binding|s|InputSHA256:   inputSHA256|InputSHA256:   strings.Repeat(\"0\", 64)|"
"quote.go|quote supply ignores buyer data residency|s|AND (\$5::text\\[\\] IS NULL OR s.data_country = ANY(\$5))|AND true|"
"exact_reuse_batch.go|exact reuse omits frozen workload authority|s|workloadJSON, err := json.Marshal(workloadDecision)|workloadJSON, err := json.Marshal(WorkloadDecision{})|"
"exact_reuse_batch.go|exact reuse hashes request shape but not runtime authority|s|decisionSHA256, err := workloadDecisionDigest(decision)|decisionSHA256, err := workloadBindingDigest(decision.Binding)|"
"scheduler.go|claim ignores frozen runtime candidates|s|j.workload_decision IS NULL|true|"
"compute_plan.go|bound quote recomputes split from the live planner|s|return bound.ComputePlan.SplitSize, nil|return unbound(), nil|"
"compute_plan.go|compute plan accepts tampered task totals|s|if plan.TotalInitialTasks != expectedTotal {|if false {|"
"store_jobs.go|job persistence ignores bound quote compute authority|s#quoteComputeSHA256 == \"\" || quoteComputeSHA256 != computeSHA256#false#"
"exact_reuse_batch.go|exact reuse ignores origin quote compute authority|s#computePlan.OriginComputePlanSHA256 != quoteComputeSHA256#false#"
"realtime_placement.go|frozen multi-GPU placement accepts a different degree|s#selected.Degree != plan.AdmittedTensorParallel#false#"
"realtime_placement.go|realtime placement digest mismatch is ignored|s#actual != digest#actual != actual#"
"realtime_store.go|contract persistence drops selected placement authority|s#auth.InputCommitment, auth.RequestSHA256, placementJSON, placementSHA256,#auth.InputCommitment, auth.RequestSHA256, nil, \"\",#"
"payment_authority.go|production credential silently arms LIVE payments|/if isProductionEnv(cxEnv) {/ { n; s#PaymentModeSealed#PaymentModeLive#; }"
"payment_authority.go|SEALED provider boundary is bypassed|s|return PaymentAuthority{}, errPaymentAuthoritySealed|return authority, nil|"
"payment_authority.go|LIVE activation ignores candidate binding|s#build.Modified || !fullCommitPattern.MatchString(build.Commit) || build.Commit != a.CandidateCommit#false#"
"payment_authority.go|LIVE per-operation cap is ignored|s#amountMinor <= 0 || amountMinor > capMinor#amountMinor <= 0#"
"payment_authority.go|LIVE recovery cannot remain operationally ready|s#return a.Activation != nil && (a.Active || a.RecoveryActive)#return a.Activation != nil \\&\\& a.Active#"
"payment_authority.go|LIVE Stripe key can remain environment-inline|s#if strings.TrimSpace(os.Getenv(stripeSecretKeyFileEnv)) == \"\" ||#if false \\&\\&#"
"stripe_api_contract.go|Stripe API version pin is deleted|s#req.Header.Set(\"Stripe-Version\", stripeAPIVersion)##"
"stripe_api_contract.go|signed Stripe webhook ignores API version|s#if apiVersion != stripeAPIVersion {#if false {#"
"stripe_api_contract.go|signed Stripe webhook ignores livemode|s#if \\*livemode != expectedLive {#if false {#"
"billing.go|Stripe billing bypasses the pinned transport|s#resp, err := doStripeRequest(stripeHTTPClient, req)#resp, err := stripeHTTPClient.Do(req)#"
"payment.go|Stripe payout bypasses the pinned transport|s#resp, err := doStripeRequest(p.http, req)#resp, err := p.http.Do(req)#"

# --- Selector economics and evidence provenance (2026-08-04) ---
# The invariants the overnight selector work created. Without these the mutation
# suite proves only that the older money paths are still guarded, and says
# nothing about the code that now decides which runtime cell wins.
"runtime_cell_economics.go|supplier entitlement stops cancelling unitsPerSec|s#return units / 1000.0 \* pricePer1K \* share#return units / 1000.0 * pricePer1K * share * 1.0001#"
"runtime_cell_economics.go|throughput re-enters the expanded entitlement form|s#seconds := units / unitsPerSec#seconds := units / unitsPerSec * 1.05#"
"runtime_shadow_selection.go|a cost tie is reported as a cost win|s#if !supplierLiabilitiesTieUSD(bestLiability, secondLiability) {#if supplierLiabilitiesTieUSD(bestLiability, secondLiability) {#"
"runtime_shadow_selection.go|a true tie manufactures a winner|s#if math.Abs(a-b) < latencyNoiseAbsMs {#if false {#"
"runtime_shadow_selection.go|latency noise floor stops guarding the ratio band|s#return math.Abs(a-b)/mid < latencyNoiseFraction#return math.Abs(a-b)/mid < 0.0#"
"runtime_governed_comparison.go|an unbound actual is allowed to rule on a prior claim|s#if !strings.EqualFold(binding, BindingBound) {#if false {#"
"runtime_governed_comparison.go|provenance takes the strongest input instead of the weakest|s#return weakestEvidenceBinding(statuses\\.\\.\\.)#return BindingBound#"

# --- Frozen runtime-cell economics at admission (2026-08-04) ---
# The block that binds a cell's accepted economics beside the money. Its whole
# value is that it reproduces exactly and never reports an unknown as zero.
"runtime_cell_admission_binding.go|frozen per-unit cost is taken from the raw accumulator|s#f.ExpectedVOCostUSDPerUnit = f.ExpectedVOCostUSD / f.BillableUnits#f.ExpectedVOCostUSDPerUnit = total / f.BillableUnits#"
"runtime_cell_admission_binding.go|half a combined cost is reported as the whole cost|s#return unknownCost(name + \" is unknown because a component is unknown: \" + c.Basis)#continue#"
"runtime_cell_admission_binding.go|true net is published while a category is unknown|s#if len(out.UnknownCategories) == 0 {#if true {#"
"runtime_cell_admission_binding.go|a partial cost sum calls itself complete|s#f.ExpectedVOCostStatus = frozenVOCostPartial#f.ExpectedVOCostStatus = frozenVOCostComplete#"
"runtime_cell_admission_binding.go|the block stops naming its own unknown terms|s#out.UnknownCategories = append(out.UnknownCategories, out.unknownNamedTerms()...)##"
"pricing_decision.go|a legacy decision is retro-fitted with a frozen cell block|s#if decision.RuntimeCell == nil {#if false {#"

# --- Cost tie authority (2026-08-04) ---
# The block that explains WHY two cells cost the same. Its value is entirely in
# refusing to let a blocked term rule and in not calling a real difference a tie.
"runtime_cost_tie_authority.go|an ASSUMED term is allowed to rule on money|s#MayRule: k == CategoryKnown,#MayRule: true,#"
"runtime_cost_tie_authority.go|a comparison takes the stronger knowledge instead of the weaker|s#if rank(a) <= rank(b) {#if rank(a) >= rank(b) {#"
"runtime_cost_tie_authority.go|a real cost difference is still reported as a forced tie|s#case !out.Tied:#case false:#"
"runtime_cost_tie_authority.go|a governed term that differs is swallowed by the tie verdict|s#case out.LargestGovernedShare > 0:#case false:#"
)

if [ "$MERC_MUTATION_LIST" = "1" ]; then
  index=0
  for entry in "${MUTATIONS[@]}"; do
    index=$((index + 1))
    file="${entry%%|*}"
    rest="${entry#*|}"
    desc="${rest%%|*}"
    if [ "$MERC_MUTATION_LIST_DETAIL" = "1" ]; then
      printf '%s\t%s\t%s\n' "$index" "$file" "$desc"
    else
      printf '%s\t%s\n' "$index" "$desc"
    fi
  done
  exit 0
fi

total_mutations="${#MUTATIONS[@]}"
if [ -n "$MERC_MUTATION_CASE_IDS" ]; then
  if ! [[ "$MERC_MUTATION_CASE_IDS" =~ ^[1-9][0-9]*(,[1-9][0-9]*)*$ ]]; then
    echo "MERC_MUTATION_CASE_IDS must be comma-separated positive case IDs" >&2
    exit 2
  fi
  duplicates="$(printf '%s\n' "$MERC_MUTATION_CASE_IDS" | tr ',' '\n' | sort | uniq -d)"
  if [ -n "$duplicates" ]; then
    echo "MERC_MUTATION_CASE_IDS contains duplicate case IDs: $duplicates" >&2
    exit 2
  fi
  while IFS= read -r requested; do
    if [ "$requested" -gt "$total_mutations" ]; then
      echo "MERC_MUTATION_CASE_IDS names $requested but only $total_mutations cases exist" >&2
      exit 2
    fi
  done < <(printf '%s\n' "$MERC_MUTATION_CASE_IDS" | tr ',' '\n')
fi

# Parallel shards deliberately supply an ordered case list. Membership alone is
# not enough: the first pure case warms a disposable checkout before a costly
# database fallback. Serial runs retain the declaration order.
declare -a MUTATION_CASE_ORDER=()
if [ -n "$MERC_MUTATION_CASE_IDS" ]; then
  while IFS= read -r requested; do
    MUTATION_CASE_ORDER+=("$requested")
  done < <(printf '%s\n' "$MERC_MUTATION_CASE_IDS" | tr ',' '\n')
else
  for ((requested = 1; requested <= total_mutations; requested++)); do
    MUTATION_CASE_ORDER+=("$requested")
  done
fi
if [ "$MERC_MUTATION_DRY_ORDER" = "1" ]; then
  printf '%s\n' "${MUTATION_CASE_ORDER[@]}"
  exit 0
fi

case_is_selected() {
  local candidate="$1"
  [ -z "$MERC_MUTATION_CASE_IDS" ] && return 0
  case ",$MERC_MUTATION_CASE_IDS," in
    *",$candidate,"*) return 0 ;;
    *) return 1 ;;
  esac
}

mutation_clock() {
  python3 - <<'PY'
import time
print(f"{time.monotonic():.6f}")
PY
}

record_mutation_timing() {
  local case_id="$1" source="$2" description="$3" result="$4" pathway="$5" started="$6" finished="$7"
  [ -n "$MERC_MUTATION_TIMINGS_FILE" ] || return 0
  python3 - "$MERC_MUTATION_TIMINGS_FILE" "$case_id" "$source" "$description" "$result" "$pathway" "$started" "$finished" <<'PY'
import json
import os
import sys

path, case_id, source, description, result, pathway, started, finished = sys.argv[1:]
start = float(started)
end = float(finished)
record = {
    "case_id": int(case_id),
    "source": source,
    "description": description,
    "result": result,
    "pathway": pathway or "UNKNOWN",
    "duration_seconds": round(max(0.0, end - start), 6),
    "candidate": os.popen("git rev-parse HEAD").read().strip(),
}
with open(path, "a", encoding="utf-8") as handle:
    handle.write(json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n")
PY
}

if ! preflight_mutation_strategy; then
  exit 2
fi

caught=0
survived=0
declare -a SURVIVORS=()
# A pattern that no longer matches its source tests nothing, and used to say so
# in a lower-case "skip" nobody read. Three had rotted that way — two through a
# currency-generalising rename (microUSDPerCent -> factor) and one through an
# added INSERT column — while the run still reported "0 survived". Silent
# coverage loss and a survivor are the same defect; only one of them announced
# itself. Stale patterns now fail the run.
stale=0
declare -a STALE=()
infrastructure=0
declare -a INFRASTRUCTURE=()
selected_cases=0

printf '%-58s %s\n' "mutation" "result"
printf '%-58s %s\n' "--------" "------"

for case_index in "${MUTATION_CASE_ORDER[@]}"; do
  entry="${MUTATIONS[$((case_index - 1))]}"
  selected_cases=$((selected_cases + 1))
  file="${entry%%|*}"
  rest="${entry#*|}"
  desc="${rest%%|*}"
  expr="${rest#*|}"
  if [ -n "$MERC_MUTATION_FILTER" ] && [[ "$desc" != *"$MERC_MUTATION_FILTER"* ]]; then
    continue
  fi

  src="$CONTROL/$file"
  [ -f "$src" ] || {
    case_started="$(mutation_clock)"
    printf '%-58s %s\n' "$desc" "STALE (missing $file)"
    stale=$((stale+1)); STALE+=("$desc — control/$file no longer exists")
    record_mutation_timing "$case_index" "$file" "$desc" "stale" "NONE" "$case_started" "$(mutation_clock)"
    continue
  }

  case_started="$(mutation_clock)"
  MUTATION_PATHWAY=""
  cp "$src" "$BACKUP/${file//\//__}.bak"
  sed -i '' "$expr" "$src" 2>/dev/null || sed -i "$expr" "$src" 2>/dev/null

  if ! cmp -s "$src" "$BACKUP/${file//\//__}.bak"; then
    # Build first: a mutation that does not compile is not a useful test.
    if ! (cd "$CONTROL" && go build ./... >/dev/null 2>&1); then
      printf '%-58s %s\n' "$desc" "INFRASTRUCTURE (mutation does not compile)"
      infrastructure=$((infrastructure+1))
      INFRASTRUCTURE+=("$desc — injected source does not compile")
      result="infrastructure"
    else
      run_mutation_tests "$file"
      mutation_status=$?
      case "$mutation_status" in
        0)
          printf '%-58s %s\n' "$desc" "SURVIVED"
          survived=$((survived+1))
          SURVIVORS+=("$desc")
          result="survived"
          ;;
        10)
          printf '%-58s %s\n' "$desc" "caught"
          caught=$((caught+1))
          result="caught"
          ;;
        *)
          printf '%-58s %s\n' "$desc" "INFRASTRUCTURE (contract did not prove a legitimate catch)"
          infrastructure=$((infrastructure+1))
          INFRASTRUCTURE+=("$desc — contract execution failed closed")
          result="infrastructure"
          ;;
      esac
    fi
  else
    printf '%-58s %s\n' "$desc" "STALE (pattern did not apply)"
    stale=$((stale+1)); STALE+=("$desc — sed pattern no longer matches control/$file")
    result="stale"
  fi

  cp "$BACKUP/${file//\//__}.bak" "$src"
  record_mutation_timing "$case_index" "$file" "$desc" "$result" "$MUTATION_PATHWAY" "$case_started" "$(mutation_clock)"
done

if [ -n "$MERC_MUTATION_CASE_IDS" ]; then
  requested_cases="$(printf '%s\n' "$MERC_MUTATION_CASE_IDS" | tr ',' '\n' | wc -l | tr -d ' ')"
  if [ "$selected_cases" -ne "$requested_cases" ]; then
    echo "mutation-test: selected $selected_cases of $requested_cases requested cases" >&2
    exit 2
  fi
fi

echo
echo "mutation-test: $caught caught, $survived survived, $stale stale, $infrastructure infrastructure"
status=0
if [ "$survived" -gt 0 ]; then
  echo "surviving mutations are gaps in the suite:"
  for s in "${SURVIVORS[@]}"; do echo "  - $s"; done
  status=1
fi
if [ "$stale" -gt 0 ]; then
  echo "stale mutations tested nothing and must be repointed at the current source:"
  for s in "${STALE[@]}"; do echo "  - $s"; done
  status=1
fi
if [ "$infrastructure" -gt 0 ]; then
  echo "mutation infrastructure failures are neither catches nor survivors:"
  for failure in "${INFRASTRUCTURE[@]}"; do echo "  - $failure"; done
  status=1
fi
exit "$status"

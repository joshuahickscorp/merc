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
# the database-backed suite.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

CONTROL=control
MERC_MUTATION_FILTER="${MERC_MUTATION_FILTER:-}"
MERC_MUTATION_UNIT_ONLY="${MERC_MUTATION_UNIT_ONLY:-0}"
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
cleanup() {
  # Restore every file touched, always.
  if [ -d "$BACKUP" ]; then
    for f in "$BACKUP"/*.bak; do
      [ -e "$f" ] || continue
      base="$(basename "$f" .bak)"
      cp "$f" "$CONTROL/${base//__//}"
    done
    rm -rf "$BACKUP"
  fi
  rmdir "$MUTATION_LOCK" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

run_mutation_tests() {
  if [ "$MERC_MUTATION_UNIT_ONLY" = "1" ]; then
    (
      cd "$CONTROL" &&
        env -u MERC_TEST_DATABASE_URL MERC_ALLOW_SKIPPING_DB_TESTS=1 \
          go test -count=1 ./... >/dev/null 2>&1
    )
  else
    (cd "$CONTROL" && go test -count=1 ./... >/dev/null 2>&1)
  fi
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
"realtime_store.go|physical contract persistence drops PricingDecision authority|s#pricing.Realtime.BuyerDeclaredCeilingNanos, pricingJSON, pricingSHA256)#pricing.Realtime.BuyerDeclaredCeilingNanos, pricingJSON[:0], pricingSHA256)#"
"realtime_store.go|persisted realtime PricingDecision digest mismatch is ignored|s#digest != contract.PricingDecisionSHA256#digest != digest#"
"realtime_store.go|legacy supplier projection may diverge from PricingDecision|s#SupplierInputRate:         contract.SupplierInputUSDPerMillionTokens,#SupplierInputRate:         float64(pricing.Realtime.SupplierInputNanosPerMillion) / float64(NanosPerMajorUnit),#"
"realtime_store.go|verified usage may exceed frozen PricingDecision bounds|s#if evidence.PromptTokens > contract.MaximumPromptTokens ||#if false \&\&#"
"realtime_store.go|settlement substitutes buyer authority for supplier floor|s#authority.SupplierInputNanosPerMillion),#authority.BuyerInputNanosPerMillion),#"
"supplier_accrual.go|accrual adds instead of carrying the remainder|s|carryOut = effective % microUSDPerCent|carryOut = 0|"
"supplier_accrual.go|accrual rounds up instead of flooring cents|s|cashCents = effective / microUSDPerCent|cashCents = (effective + microUSDPerCent - 1) / microUSDPerCent|"
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
)

caught=0
survived=0
declare -a SURVIVORS=()

printf '%-58s %s\n' "mutation" "result"
printf '%-58s %s\n' "--------" "------"

for entry in "${MUTATIONS[@]}"; do
  file="${entry%%|*}"
  rest="${entry#*|}"
  desc="${rest%%|*}"
  expr="${rest#*|}"
  if [ -n "$MERC_MUTATION_FILTER" ] && [[ "$desc" != *"$MERC_MUTATION_FILTER"* ]]; then
    continue
  fi

  src="$CONTROL/$file"
  [ -f "$src" ] || { printf '%-58s %s\n' "$desc" "SKIP (missing $file)"; continue; }

  cp "$src" "$BACKUP/${file//\//__}.bak"
  sed -i '' "$expr" "$src" 2>/dev/null || sed -i "$expr" "$src" 2>/dev/null

  if ! cmp -s "$src" "$BACKUP/${file//\//__}.bak"; then
    # Build first: a mutation that does not compile is not a useful test.
    if ! (cd "$CONTROL" && go build ./... >/dev/null 2>&1); then
      printf '%-58s %s\n' "$desc" "skip (does not compile)"
    elif run_mutation_tests; then
      printf '%-58s %s\n' "$desc" "SURVIVED"
      survived=$((survived+1))
      SURVIVORS+=("$desc")
    else
      printf '%-58s %s\n' "$desc" "caught"
      caught=$((caught+1))
    fi
  else
    printf '%-58s %s\n' "$desc" "skip (pattern did not apply)"
  fi

  cp "$BACKUP/${file//\//__}.bak" "$src"
done

echo
echo "mutation-test: $caught caught, $survived survived"
if [ "$survived" -gt 0 ]; then
  echo "surviving mutations are gaps in the suite:"
  for s in "${SURVIVORS[@]}"; do echo "  - $s"; done
  exit 1
fi
exit 0

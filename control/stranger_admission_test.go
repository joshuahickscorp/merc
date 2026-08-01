package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// strangerDeploymentInputs sets the six deployment inputs the public path needs.
//
// Driving the stranger loop is what found them; none was written down anywhere,
// and a deployment missing any one returns 503 or 409 to a buyer who has done
// nothing wrong. They are in one function because they are one configuration, and
// because two tests asserting the stranger path must assert it under the same
// deployment rather than under two that drifted.
func strangerDeploymentInputs(t *testing.T) {
	t.Helper()
	t.Setenv("MERC_VERIFICATION_SAMPLE_SECRET",
		"first-complete-loop-verification-sampling-secret-0123456789")
	// 1. Signup is only open where the canary decision says so, and the switch and
	//    its decision reference are one decision.
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-first-complete-loop")
	// 2. The sandbox grant. Defaults to zero, and an unfunded stranger cannot buy.
	t.Setenv("MERC_SANDBOX_CREDIT_USD", "5.00")
	// 3. The economic schedule, which has no defaults on purpose: admission returns
	//    503 rather than pricing work against numbers nobody chose.
	t.Setenv("MERC_ECON_SCHEDULE_VERSION", "first-complete-loop-v1")
	t.Setenv("MERC_PROCESSOR_PERCENT_BPS", "350")
	t.Setenv("MERC_PROCESSOR_FIXED_USD", "0.35")
	// Fixed account/invoice overhead is allocated across the $5 economic charge
	// batch. Charging this once per physical task was the source of the historical
	// 11,564-micro buyer / 2-micro supplier receipt.
	t.Setenv("MERC_CONTROL_PLANE_PER_BATCH_USD", "0.005")
	t.Setenv("MERC_MIN_CONTRIBUTION_PER_BATCH_USD", "0.000001")
	t.Setenv("MERC_TARGET_MARGIN_BPS", "300")
	// 4. The USD-reference-to-CAD-settlement rate, operator declared, with an
	//    immutable revision. Neither the application nor a test may invent one.
	t.Setenv("MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE", "1.37")
	t.Setenv("MERC_PRICE_FX_REVISION", "first-complete-loop-operator-declared")
}

// The three-record corpus the programme ledger's figures were measured on.
const strangerCorpus = `{"id":"0","text":"A verifiable compute network settles every task against a receipt."}
{"id":"1","text":"A stranger should not have to understand GPUs."}
{"id":"2","text":"The cheapest verified outcome is not the cheapest attempt."}
`

// TestAStrangerCanBeAdmittedForASubMicroJob is the admission half of the public
// loop, and it needs no agent and no engine — which is the point. The execution
// half is TestFirstCompleteLoopThroughThePublicAPI and needs both, so it can only
// run where the hardware is; this one can run anywhere and is what stops the
// pricing-authority defect from coming back unnoticed.
//
// What it pins: a three-record embed whose entire physical value is ~1 micro-USD
// per task is admitted. Every previous derivation refused it, and each refusal
// looked like a different bug:
//
//	1.676%   a rounded per-task payout divided back into an hourly rate
//	2.1x     a floor from the plan's modeled duration, a ceiling from the catalogue
//	30x      a sub-second duration truncated to a whole integer second
//	~4-30%   the buyer's gross quantised to micro-USD before the supplier's share
//
// All four are one failure: a fixed granularity, or a lost fraction, dominating a
// small quote. Admission is now an identity between two evaluations of one
// expression, so there is no granularity left for a small job to fall through.
//
// Run in BOTH settlement currencies, because the reference/settlement distinction
// is itself one of the four. The floor used to be derived from the USD reference
// price while the entitlement was denominated in the settlement currency, so under
// CAD the two sides sat an entire 1.37x FX rate apart and nothing in the type
// system could object — both were float64 named USD. Under USD settlement the FX
// rate is 1.0 and that defect is invisible, which is exactly why the mandated
// money mode has to be exercised rather than assumed.
func TestAStrangerCanBeAdmittedForASubMicroJob(t *testing.T) {
	for _, settlement := range []string{"usd", "cad"} {
		t.Run(settlement, func(t *testing.T) {
			strangerAdmissionUnderCurrency(t, settlement)
		})
	}
}

func strangerAdmissionUnderCurrency(t *testing.T, settlement string) {
	t.Helper()
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, settlement)

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedTestStore(t)

	schedule, err := BuildCataloguePriceSchedule(supplierShareRate)
	if err != nil {
		t.Fatalf("build catalogue price schedule: %v", err)
	}
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("publish catalogue price schedule: %v", err)
	}
	// Merc refuses work it cannot verify, correctly. seedDemo installs the governed
	// embed honeypot and the input object the verifier fetches.
	if err := seedDemo(ctx, pool, artifacts.storage); err != nil {
		t.Fatalf("seed the verification floor: %v", err)
	}

	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	srv := httptest.NewServer(NewServer(store, artifacts.storage, verifier, nil).Routes())
	t.Cleanup(srv.Close)

	email := "stranger-" + uuid.NewString() + "@example.test"
	signup := postJSON(t, srv.URL+"/v1/signup", "", map[string]any{
		"email": email, "password": "a-stranger-password-1234",
	})
	if signup.status != http.StatusOK && signup.status != http.StatusCreated {
		t.Fatalf("signup: HTTP %d: %s", signup.status, signup.body)
	}
	apiKey, _ := signup.json["sandbox_key"].(string)
	if apiKey == "" {
		t.Fatalf("signup issued no sandbox key: %s", signup.body)
	}

	const ceiling = 1.00
	submit := postJSONWithHeaders(t, srv.URL+"/v1/jobs", apiKey, map[string]string{
		"Idempotency-Key": uuid.NewString(),
	}, map[string]any{
		"job_type": map[string]any{"type": "embed"},
		"model":    map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":     "batch",
		"input":    strangerCorpus,
		"max_usd":  ceiling,
	})
	switch submit.status {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted:
	default:
		// Name the shape of the failure rather than leaving a bare status. A 409 here
		// is the composite pricing authority refusing, and its text carries every
		// failing condition with its figures.
		t.Fatalf("a stranger could not submit a three-record embed: HTTP %d: %s",
			submit.status, submit.body)
	}

	jobIDText, _ := submit.json["job_id"].(string)
	if jobIDText == "" {
		jobIDText, _ = submit.json["id"].(string)
	}
	jobID, err := uuid.Parse(jobIDText)
	if err != nil {
		t.Fatalf("submit returned no usable job id: %s", submit.body)
	}
	estimate, _ := submit.json["estimated_usd"].(float64)
	if estimate <= 0 {
		t.Fatalf("admitted at an estimate of %v; a job that costs nothing was not priced", estimate)
	}
	if estimate > ceiling {
		t.Fatalf("estimate %.9f exceeds the accepted ceiling %.2f", estimate, ceiling)
	}

	// Read the frozen authority back out and assert the identity that makes the
	// admission legitimate rather than merely permitted.
	var pricingJSON, economicJSON []byte
	if err := pool.QueryRow(ctx, `
		SELECT j.pricing_decision, p.plan_json
		  FROM jobs j JOIN job_economic_plans p ON p.job_id=j.id
		 WHERE j.id=$1`, jobID).
		Scan(&pricingJSON, &economicJSON); err != nil {
		t.Fatalf("read frozen job authority: %v", err)
	}
	var pricing PricingDecision
	if err := json.Unmarshal(pricingJSON, &pricing); err != nil {
		t.Fatalf("decode frozen pricing decision: %v", err)
	}
	var economic EconomicPlan
	if err := json.Unmarshal(economicJSON, &economic); err != nil {
		t.Fatalf("decode frozen economic plan: %v", err)
	}

	if pricing.SupplierEntitlementPolicy != economicRoundingPolicy {
		t.Fatalf("job was admitted under %q, not the exact policy %q; the legacy hourly "+
			"comparison decided this job",
			pricing.SupplierEntitlementPolicy, economicRoundingPolicy)
	}
	if pricing.SupplierRequiredNanos <= 0 {
		t.Fatalf("supplier floor is %d nanos; a floor of zero admits anything",
			pricing.SupplierRequiredNanos)
	}
	if pricing.SupplierGrossNanos < pricing.SupplierRequiredNanos {
		t.Fatalf("admitted with the supplier entitled to %d nanos against a %d nano floor",
			pricing.SupplierGrossNanos, pricing.SupplierRequiredNanos)
	}
	settlementProjectionNanos := usdToMicros(economic.SupplierPayoutPerTaskUSD) * NanosPerMicro
	if settlementProjectionNanos < pricing.SupplierRequiredNanos {
		t.Fatalf("ledger settlement projection is %d nanos against the admitted supplier floor %d",
			settlementProjectionNanos, pricing.SupplierRequiredNanos)
	}
	// Conservation, exactly: the supplier can never be owed more per task than the
	// buyer was charged for it.
	if economic.BuyerChargePerTaskNanos > 0 &&
		economic.SupplierPayoutPerTaskNanos > economic.BuyerChargePerTaskNanos {
		t.Fatalf("supplier is owed %d nanos per task against a %d nano buyer charge",
			economic.SupplierPayoutPerTaskNanos, economic.BuyerChargePerTaskNanos)
	}
	// The exact base carries a fraction of a micro-USD that the micro-USD
	// projection cannot hold. That is what makes this fixture a test of the failure
	// class rather than of a job large enough for the class not to bite: if the
	// exact figure were a whole number of micros, rounding it would cost nothing
	// and every derivation this file records would have agreed all along.
	if economic.BaseComputePerTaskNanos%NanosPerMicro == 0 {
		t.Fatalf("exact base compute %d nanos per task is a whole number of micro-USD, "+
			"so this fixture no longer exercises the sub-micro rounding class",
			economic.BaseComputePerTaskNanos)
	}
	full, err := fullSuccessEconomicScenario(economic)
	if err != nil {
		t.Fatalf("full-success economics: %v", err)
	}
	if full.ContributionMarginUSD <= 0 {
		t.Fatalf("tiny job is admitted at non-positive modeled contribution: %+v", full)
	}
	if full.NetBilledUSD <= 0 || full.SupplierLiabilityUSD/full.NetBilledUSD < 0.25 {
		t.Fatalf("tiny-job supplier share is still commercially meaningless: supplier %.9f / buyer %.9f",
			full.SupplierLiabilityUSD, full.NetBilledUSD)
	}
	if full.ControlPlaneCostUSD >= 0.005*float64(full.AcceptedTasks) {
		t.Fatalf("control overhead was still charged once per physical task: %+v", full)
	}
	if got, want := usdToMicros(full.NetBilledUSD),
		usdToMicros(full.SupplierLiabilityUSD)+usdToMicros(full.ProcessorFeeUSD)+
			usdToMicros(full.ControlPlaneCostUSD)+usdToMicros(full.ContributionMarginUSD); got != want {
		t.Fatalf("commercial split does not conserve: buyer=%d parts=%d micro-units", got, want)
	}
	// And the quantisation the old path applied would still REFUSE this job. That
	// is the exact property worth asserting: not that some fraction is lost, but
	// that losing it is the difference between admitted and refused. If a later
	// change makes the fixture large enough for the loss not to matter, this fails
	// and says so rather than passing on a job that never exercised the defect.
	quantised := economic.BaseComputePerTaskNanos / NanosPerMicro * NanosPerMicro
	quantisedEntitlement := int64(math.Ceil(float64(quantised) * supplierShareRate))
	if quantisedEntitlement >= pricing.SupplierRequiredNanos {
		t.Fatalf("a micro-quantised base of %d nanos still entitles the supplier to %d "+
			"against a %d nano floor, so this fixture no longer distinguishes the exact "+
			"path from the rounded one", quantised, quantisedEntitlement,
			pricing.SupplierRequiredNanos)
	}

	if pricing.Currency != settlement || economic.Schedule.Currency != settlement {
		t.Fatalf("job settled in pricing=%q economic=%q, not the configured %q",
			pricing.Currency, economic.Schedule.Currency, settlement)
	}
	fixed := pricing.FixedPoint
	if fixed == nil || fixed.Currency != settlement {
		t.Fatalf("admitted decision lacks currency-bound fixed-point economics: %+v", fixed)
	}
	if fixed.SupplierEntitlementsNanos <
		pricing.SupplierRequiredNanos*int64(economic.Input.InitialTaskCount) {
		t.Fatalf("fixed supplier entitlement %d is below total floor %d",
			fixed.SupplierEntitlementsNanos,
			pricing.SupplierRequiredNanos*int64(economic.Input.InitialTaskCount))
	}
	if fixed.BuyerChargeNanos > fixed.AcceptedCeilingNanos {
		t.Fatalf("fixed buyer charge %d exceeds ceiling %d",
			fixed.BuyerChargeNanos, fixed.AcceptedCeilingNanos)
	}
	if fixed.BuyerChargeNanos != fixed.SupplierEntitlementsNanos+
		fixed.KnownVariableCostsNanos+fixed.KnownCostContributionNanos {
		t.Fatal("fixed pricing does not conserve buyer = supplier + variable + contribution")
	}
	if fixed.TrueNetContributionNanos != nil || len(fixed.UnknownCostCategories) == 0 {
		t.Fatalf("decision claimed true net despite unknown named costs: %+v", fixed)
	}
	t.Logf("ADMITTED in %s: %.2f billable units at %.8f/1k (reference %.8f, FX %.4g); "+
		"buyer gross %d nanos/task, supplier entitled %d nanos/task against a %d nano "+
		"floor (headroom %d), buyer estimate %.9f",
		strings.ToUpper(settlement), pricing.BillableUnits,
		pricing.Catalogue.SettlementPricePer1K, pricing.Catalogue.ReferencePricePer1K,
		pricing.Catalogue.ReferenceToSettlementRate,
		economic.BaseComputePerTaskNanos, pricing.SupplierGrossNanos,
		pricing.SupplierRequiredNanos,
		pricing.SupplierGrossNanos-pricing.SupplierRequiredNanos, estimate)
}

// TestAdmissionRefusesAnEntitlementBelowItsFloor is the negative control.
//
// The identity above is only worth asserting if the check it satisfies can still
// fail. Shave one nano off the frozen entitlement and admission must refuse and
// say by how much — otherwise the previous test proves that admission passes
// everything, not that this job clears its floor.
func TestAdmissionRefusesAnEntitlementBelowItsFloor(t *testing.T) {
	decision := PricingDecision{
		ExecutionMode:             computeExecutionDistributed,
		SupplierEntitlementPolicy: economicRoundingPolicy,
		SupplierGrossNanos:        1392,
		SupplierRequiredNanos:     1393,
	}
	err := admissionEntitlementRefusal(decision)
	if err == nil {
		t.Fatal("a one-nano shortfall was admitted")
	}
	for _, want := range []string{"1392", "1393", "shortfall of 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not report %q", err, want)
		}
	}
	decision.SupplierGrossNanos = 1393
	if err := admissionEntitlementRefusal(decision); err != nil {
		t.Fatalf("an entitlement exactly at its floor was refused: %v", err)
	}

	// A floor of zero is an ABSENT floor, not a permissive one, and a decision that
	// claims the exact policy while carrying one has turned the check off while
	// still reporting that it ran. That is the shape a units-derivation failure
	// would take, so it is refused rather than treated as an answer.
	decision.SupplierRequiredNanos = 0
	err = admissionEntitlementRefusal(decision)
	if err == nil {
		t.Fatal("a zero supplier floor was accepted as an exact entitlement policy")
	}
	if !strings.Contains(err.Error(), "admits any entitlement") {
		t.Fatalf("refusal %q does not name the absent floor", err)
	}
}

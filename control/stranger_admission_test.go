package main

import (
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

// A stranger-facing API must refuse before writing when production has no
// scope-compatible, currently advertised embed lane. Checked-in evidence is
// correctly unbindable (zero routable production authority). Do not install
// TEST_ONLY publication authority here — that seam can mint a token-like embed
// lane and would turn this into a false-green admission. Exercise both
// settlement currencies so FX cannot hide the refusal.
func TestAStrangerSubmissionRefusesWithoutScopeCompatiblePerformanceAuthority(t *testing.T) {
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
	// Merc refuses work it cannot verify, correctly. seedDemo installs the governed
	// embed honeypot and the input object the verifier fetches.
	mustf(t, seedDemo(ctx, pool, artifacts.storage), "seed the verification floor: %v")

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

	const singleRecordCorpus = `{"id":"0","text":"A stranger should not have to understand GPUs."}` + "\n"
	const ceiling = 1.00
	var jobsBefore int
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&jobsBefore),
		"count jobs before refused submission: %v")
	submit := postJSONWithHeaders(t, srv.URL+"/v1/jobs", apiKey, map[string]string{
		"Idempotency-Key": uuid.NewString(),
	}, map[string]any{
		"job_type": map[string]any{"type": "embed"},
		"model":    map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":     "batch",
		"input":    singleRecordCorpus,
		"max_usd":  ceiling,
		"verification": map[string]any{
			"redundancy_frac": 1.0,
			"honeypot_frac":   0.0,
		},
	})
	// Production has no advertised embed cell: honest buyer ingress is 400 with
	// zero durable side effects. A 2xx would mean zero-routable-authority broke.
	if submit.status == http.StatusOK || submit.status == http.StatusAccepted ||
		submit.status == http.StatusCreated {
		t.Fatalf("production-zero-authority embed was admitted HTTP %d: %s",
			submit.status, submit.body)
	}
	if !strings.Contains(submit.body, "not advertised") &&
		!strings.Contains(submit.body, "unavailable") {
		t.Fatalf("refusal does not name missing advertised authority: HTTP %d body=%s",
			submit.status, submit.body)
	}
	var jobsAfter int
	mustf(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&jobsAfter),
		"count jobs after refused submission: %v")
	if jobsAfter != jobsBefore {
		t.Fatalf("scope-incompatible submission wrote %d jobs", jobsAfter-jobsBefore)
	}
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
	mustf(t, admissionEntitlementRefusal(decision), "an entitlement exactly at its floor was refused: %v")

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

package main

import (
	"testing"

	"github.com/google/uuid"
)

// TestOpenExposureInOneCurrencyDoesNotOffsetAnother closes the gap that mutation
// 108 found by surviving.
//
// Mutant 108 drops the `j.currency` predicate from the prepaid-residual arm of
// buyer_open_exposure.go. It survived the campaign not because the mutation is
// unreachable — the predicate is live production SQL — but because every
// open-exposure test was USD-only, so no test ever created a foreign-currency
// open residual that the missing predicate could wrongly count. A surviving
// mutant names an untested invariant, and this is the invariant it named:
//
//	exposure held in one currency must not offset funds in another.
//
// Without the predicate, a CAD job's reserved residual is subtracted from a USD
// admission, and the buyer is refused money they actually have. That is the same
// class of defect as the cross-currency faults already closed in this programme
// (USD free credit against a CAD ceiling, prepaid migration relabelling non-USD
// cash), which is why it is worth a direct invariant rather than an incidental
// assertion.
//
// Both directions are asserted on purpose. Checking only that the USD view is
// zero would pass even if the CAD job had never been inserted — a test that
// cannot fail manufactures confidence.
func TestOpenExposureInOneCurrencyDoesNotOffsetAnother(t *testing.T) {
	installSettlementCurrencyForTest(t, "usd")
	ctx, _, pool := openIsolatedTestStore(t)

	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1,$2,0)`,
		buyerID, buyerID.String()+"@open-exp-currency.invalid"); err != nil {
		t.Fatal(err)
	}

	// One open prepaid job, denominated in CAD, holding a large residual.
	const cadReservedUSD = 5.00
	jobID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,task_count,prepaid_required,estimated_usd,currency)
		VALUES ($1,$2,'queued','embed','open-exp-currency',1,true,$3,'cad')`,
		jobID, buyerID, cadReservedUSD/2); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO job_economic_plans
		  (job_id,plan_version,schedule_version,plan_json,initial_task_count,
		   buyer_charge_per_task_usd,supplier_payout_per_task_usd,
		   initial_buyer_charge_usd,reserved_buyer_charge_usd,currency)
		VALUES ($1,1,'test','{"schedule":{"currency":"cad"}}',1,$2,$3,$2,$4,'cad')`,
		jobID, cadReservedUSD/2, cadReservedUSD*0.35, cadReservedUSD); err != nil {
		t.Fatal(err)
	}

	// The CAD residual must be visible under CAD. If this is zero the fixture
	// never landed and the USD assertion below would be vacuous.
	cadHeld, err := prepaidOpenReservationMicrosInCurrency(ctx, pool, buyerID, "cad")
	if err != nil {
		t.Fatalf("cad open reservation: %v", err)
	}
	if cadHeld != usdToMicros(cadReservedUSD) {
		t.Fatalf("cad open reservation = %d micros, want %d — fixture did not land, so the "+
			"usd assertion below would prove nothing", cadHeld, usdToMicros(cadReservedUSD))
	}

	// And it must be invisible under USD. Dropping the currency predicate makes
	// this return the CAD residual and wrongly starve a USD admission.
	usdHeld, err := prepaidOpenReservationMicrosInCurrency(ctx, pool, buyerID, "usd")
	if err != nil {
		t.Fatalf("usd open reservation: %v", err)
	}
	if usdHeld != 0 {
		t.Fatalf("usd open exposure = %d micros, want 0: a CAD job's reserved residual is "+
			"offsetting USD funds, so the buyer is refused money they hold", usdHeld)
	}
}

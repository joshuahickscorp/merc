package main

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// One execution, 128 deliveries: the money read out of the ledger rather than out
// of the pricing function.
//
// inflight_coalescing_test.go already proves the arithmetic (a follower's supplier
// liability is zero, Merc's contribution is positive, the buyer pays less than a
// fresh execution) and that 128 concurrent claims elect exactly one leader. What
// it does not do is put 128 deliveries through the store and then count what the
// ledger holds — and the arithmetic being right is not the same claim as the
// persistence being right. A settlement path that double-credited a supplier, or
// that reused one delivery authority for every follower, would pass every
// assertion in that file.
//
// What this proves, at store level:
//
//   - 128 followers produce 128 DISTINCT delivery authorities and 128 receipts;
//   - not one of them writes a supplier credit;
//   - every one of them charges the buyer less than a fresh execution;
//   - Merc's contribution is positive on every one, because storage, lookup and
//     delivery are real costs and a follower must not be free;
//   - the ledger conserves per follower — buyer debit equals platform take when
//     no supplier is owed;
//   - two tenants asking for the identical thing never share an authority.
//
// What it does NOT prove, and the reason is stated rather than papered over: the
// LEADER's single supplier payable. That payable is written by the realtime
// finalise path, which no test drives yet, so "one payable for 128 deliveries" is
// asserted here only as its follower half — zero payables among the 128. The
// batch chain proves a single payable per execution separately, in
// TestBothAgentsSettleThroughTheProductionPath.
const coalescedFollowers = 128

func TestOneExecutionWith128FollowersWritesNoSupplierCreditAndOneAuthorityEach(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)

	buyerID := uuid.New()
	otherBuyerID := uuid.New()
	for _, id := range []uuid.UUID{buyerID, otherBuyerID} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO buyers (id,email,password_hash,free_credit_usd)
			VALUES ($1,$2,'x',100.0)`, id, id.String()+"@coalesced.invalid"); err != nil {
			t.Fatalf("seed buyer: %v", err)
		}
	}

	profile := sortedVLLMProfiles()[0]
	fullPer1K := fullPricePer1KFromRealtime(
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)

	// The one physical execution: one result artifact, one token count. Every
	// follower is delivered THIS, which is what makes them followers.
	const deliveredTokens int64 = 64
	leaderResultRef := "cas/sha256/" + uuid.NewString()
	leaderResultSHA := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	// What a fresh execution would have cost the buyer, for the comparison the
	// whole mechanism exists to make.
	fresh := PriceAccounting(TokenAccounting{
		ClassUncachedInput: 100, ClassGeneratedOutput: deliveredTokens,
	}, fullPer1K)

	currency, err := SettlementCurrency()
	if err != nil {
		t.Fatal(err)
	}
	money, err := SettleRealtimeReuseHitMoney(currency, deliveredTokens,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens)
	if err != nil || !money.Conserved() || !money.ConservedExact() || money.SupplierLiabilityMicros != 0 {
		t.Fatalf("follower money invariant broken before any write: %+v", money)
	}

	authorities := map[uuid.UUID]bool{}
	requestIDs := map[string]bool{}
	for i := 0; i < coalescedFollowers; i++ {
		// A distinct request identity per follower, because each is a different
		// buyer request that happened to collapse onto one execution. Reusing one
		// identity would be testing idempotency, not coalescing.
		requestID := fmt.Sprintf("req_follower_%03d_%s", i, uuid.NewString())
		hit := ExactCacheHit{ResultRef: leaderResultRef, OutputTokens: deliveredTokens}
		contract, settlement, err := store.SettleRealtimeExactReuse(ctx,
			RealtimeContractAuthorization{
				RequestID: requestID, BuyerID: buyerID, Profile: profile,
				InputCommitment:   "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				RequestSHA256:     "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
				MaximumPriceUSD:   microsToUSD(money.BuyerDebitMicros),
				EstimatedPriceUSD: microsToUSD(money.BuyerDebitMicros),
				ReuseClass:        ClassCoalescedDelivery,
			}, hit, money, leaderResultSHA)
		if err != nil {
			t.Fatalf("follower %d: settle: %v", i, err)
		}
		if contract.Pricing == nil || contract.Pricing.RealtimeReuse == nil ||
			contract.Pricing.RealtimeReuse.ReuseClass != ClassCoalescedDelivery ||
			settlement.SupplierPayableNanos != 0 ||
			settlement.BuyerChargeNanos != settlement.KnownCostContributionNanos {
			t.Fatalf("follower %d lost coalesced exact authority: contract=%+v settlement=%+v", i, contract, settlement)
		}

		if authorities[contract.ID] {
			t.Fatalf("follower %d reused delivery authority %s; 128 buyers would share "+
				"one receipt", i, contract.ID)
		}
		authorities[contract.ID] = true
		if requestIDs[requestID] {
			t.Fatalf("follower %d reused request id %s", i, requestID)
		}
		requestIDs[requestID] = true

		if contract.WorkerID != uuid.Nil || contract.SupplierID != uuid.Nil {
			t.Fatalf("follower %d scheduled a worker: worker=%s supplier=%s",
				i, contract.WorkerID, contract.SupplierID)
		}
		if settlement.SupplierPayableUSD != 0 {
			t.Fatalf("follower %d minted a supplier payable of %v", i, settlement.SupplierPayableUSD)
		}
		if settlement.BuyerChargeUSD <= 0 {
			t.Fatalf("follower %d was delivered free; storage, lookup and delivery are "+
				"real costs", i)
		}
		if settlement.BuyerChargeUSD >= fresh {
			t.Fatalf("follower %d paid %.9f, not less than a fresh execution's %.9f",
				i, settlement.BuyerChargeUSD, fresh)
		}
	}

	if len(authorities) != coalescedFollowers {
		t.Fatalf("%d distinct delivery authorities for %d followers",
			len(authorities), coalescedFollowers)
	}

	// The ledger, read once across the whole cluster.
	var supplierRows, buyerRows, platformRows int
	var buyerMicros, platformMicros, supplierMicros int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE kind='supplier_credit'),
		       count(*) FILTER (WHERE kind='buyer_charge'),
		       count(*) FILTER (WHERE kind='platform_take'),
		       COALESCE((-sum(amount_usd) FILTER (WHERE kind='buyer_charge')*1000000)::bigint,0),
		       COALESCE((sum(amount_usd) FILTER (WHERE kind='platform_take')*1000000)::bigint,0),
		       COALESCE((sum(amount_usd) FILTER (WHERE kind='supplier_credit')*1000000)::bigint,0)
		  FROM ledger_entries
		 WHERE execution_contract_id = ANY($1)`, authorityIDs(authorities)).
		Scan(&supplierRows, &buyerRows, &platformRows,
			&buyerMicros, &platformMicros, &supplierMicros); err != nil {
		t.Fatalf("read cluster ledger: %v", err)
	}
	if supplierRows != 0 || supplierMicros != 0 {
		t.Fatalf("%d supplier credits worth %d micros across %d followers; the supplier "+
			"is paid once by the leader for the one execution",
			supplierRows, supplierMicros, coalescedFollowers)
	}
	if buyerRows != coalescedFollowers {
		t.Fatalf("%d buyer charges for %d followers", buyerRows, coalescedFollowers)
	}
	if platformRows != coalescedFollowers {
		t.Fatalf("%d platform takes for %d followers", platformRows, coalescedFollowers)
	}
	// With no supplier owed, every micro the buyer paid is Merc's contribution.
	if buyerMicros != platformMicros {
		t.Fatalf("cluster ledger not conserved: buyer %d micros, platform %d micros",
			buyerMicros, platformMicros)
	}
	if platformMicros <= 0 {
		t.Fatalf("Merc contribution across the cluster is %d micros", platformMicros)
	}

	// Every follower can fetch its own receipt, and only its own.
	fetched := 0
	for id := range authorities {
		receipt, err := store.RealtimeReceipt(ctx, buyerID, id)
		if err != nil {
			t.Fatalf("receipt for %s: %v", id, err)
		}
		if receipt.ContractID != id.String() {
			t.Fatalf("receipt for %s reported contract %s", id, receipt.ContractID)
		}
		fetched++
		// The other tenant must not be able to read it. This is the cross-tenant
		// disclosure the reuse key's tenant scope exists to prevent, checked at the
		// read rather than only at the lookup.
		if _, err := store.RealtimeReceipt(ctx, otherBuyerID, id); err == nil {
			t.Fatalf("a second tenant read follower receipt %s", id)
		}
	}
	if fetched != coalescedFollowers {
		t.Fatalf("fetched %d receipts for %d followers", fetched, coalescedFollowers)
	}

	t.Logf("128 followers: %d authorities, %d receipts, %d supplier credits, "+
		"buyer %d micros == platform %d micros, fresh execution would have cost %.9f",
		len(authorities), fetched, supplierRows, buyerMicros, platformMicros, fresh)
}

func authorityIDs(set map[uuid.UUID]bool) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

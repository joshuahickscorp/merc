package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBatchFeeAllocationConservesAndRejectsInvalidWeights(t *testing.T) {
	for _, tc := range []struct {
		fee     int64
		weights []int64
		valid   bool
	}{{101, []int64{1, 1, 1}, true}, {0, []int64{3}, true}, {-1, []int64{1}, false}, {1, nil, false}, {1, []int64{0}, false}} {
		weights := make([]batchFeeWeight, len(tc.weights))
		for i, weight := range tc.weights {
			weights[i] = batchFeeWeight{JobID: uuid.New(), WeightMicros: weight}
		}
		allocations, err := allocateBatchFeeMicros(tc.fee, weights)
		if (err == nil) != tc.valid {
			t.Fatalf("fee=%d weights=%v err=%v", tc.fee, tc.weights, err)
		}
		var sum int64
		for _, allocation := range allocations {
			sum += allocation.AllocatedMicros
		}
		if err == nil && sum != tc.fee {
			t.Fatalf("fee=%d allocation sum=%d", tc.fee, sum)
		}
	}
}

func allocationByJob(items []batchFeeAllocation) map[uuid.UUID]int64 {
	result := make(map[uuid.UUID]int64, len(items))
	for _, item := range items {
		result[item.JobID] = item.AllocatedMicros
	}
	return result
}

func TestBatchFeeAllocationUsesDeterministicLargestRemainder(t *testing.T) {
	jobA := uuid.MustParse("10000000-0000-4000-8000-000000000001")
	jobB := uuid.MustParse("10000000-0000-4000-8000-000000000002")
	jobC := uuid.MustParse("10000000-0000-4000-8000-000000000003")
	weights := []batchFeeWeight{
		{JobID: jobA, WeightMicros: 100},
		{JobID: jobB, WeightMicros: 1},
		{JobID: jobC, WeightMicros: 1},
	}
	allocation, err := allocateBatchFeeMicros(2, weights)
	if err != nil {
		t.Fatal(err)
	}
	got := allocationByJob(allocation)
	if got[jobA] != 2 || got[jobB] != 0 || got[jobC] != 0 {
		t.Fatalf("largest-remainder allocation=%v, want heavy job to receive both micros", got)
	}

	// Equal remainders are resolved by immutable job ID, not caller order.
	tied := []batchFeeWeight{
		{JobID: jobC, WeightMicros: 1},
		{JobID: jobA, WeightMicros: 1},
		{JobID: jobB, WeightMicros: 1},
	}
	first, err := allocateBatchFeeMicros(2, tied)
	if err != nil {
		t.Fatal(err)
	}
	reversed := []batchFeeWeight{tied[2], tied[1], tied[0]}
	second, err := allocateBatchFeeMicros(2, reversed)
	if err != nil {
		t.Fatal(err)
	}
	firstByJob := allocationByJob(first)
	secondByJob := allocationByJob(second)
	if firstByJob[jobA] != 1 || firstByJob[jobB] != 1 || firstByJob[jobC] != 0 {
		t.Fatalf("tie allocation=%v, want lowest two immutable job IDs", firstByJob)
	}
	for jobID, amount := range firstByJob {
		if secondByJob[jobID] != amount {
			t.Fatalf("allocation changed under permutation: first=%v second=%v", firstByJob, secondByJob)
		}
	}
}

func TestBatchFeeAllocationRandomizedConservationQuotaAndPermutation(t *testing.T) {
	random := rand.New(rand.NewSource(20260728))
	for iteration := 0; iteration < 10_000; iteration++ {
		count := 1 + random.Intn(20)
		fee := int64(random.Intn(1_000_001))
		weights := make([]batchFeeWeight, count)
		var total int64
		for index := range weights {
			weight := int64(1 + random.Intn(1_000_000))
			weights[index] = batchFeeWeight{
				JobID:        uuid.MustParse("20000000-0000-4000-8000-" + strings.Repeat("0", 8) + fmt.Sprintf("%04x", index+1)),
				WeightMicros: weight,
			}
			total += weight
		}
		allocation, err := allocateBatchFeeMicros(fee, weights)
		if err != nil {
			t.Fatalf("iteration=%d: %v", iteration, err)
		}
		var sum int64
		for index, item := range allocation {
			sum += item.AllocatedMicros
			var numerator, floor, remainder big.Int
			numerator.Mul(big.NewInt(fee), big.NewInt(weights[index].WeightMicros))
			floor.QuoRem(&numerator, big.NewInt(total), &remainder)
			minimum := floor.Int64()
			maximum := minimum
			if remainder.Sign() > 0 {
				maximum++
			}
			if item.AllocatedMicros < minimum || item.AllocatedMicros > maximum {
				t.Fatalf(
					"iteration=%d job=%s allocation=%d outside quota [%d,%d]",
					iteration, item.JobID, item.AllocatedMicros, minimum, maximum,
				)
			}
		}
		if sum != fee {
			t.Fatalf("iteration=%d allocated=%d fee=%d", iteration, sum, fee)
		}

		permuted := append([]batchFeeWeight(nil), weights...)
		random.Shuffle(len(permuted), func(i, j int) {
			permuted[i], permuted[j] = permuted[j], permuted[i]
		})
		again, err := allocateBatchFeeMicros(fee, permuted)
		if err != nil {
			t.Fatalf("iteration=%d permuted: %v", iteration, err)
		}
		got, want := allocationByJob(again), allocationByJob(allocation)
		for jobID, amount := range want {
			if got[jobID] != amount {
				t.Fatalf(
					"iteration=%d allocation changed under permutation for %s: got=%d want=%d",
					iteration, jobID, got[jobID], amount,
				)
			}
		}
	}

	duplicate := uuid.MustParse("30000000-0000-4000-8000-000000000001")
	for name, weights := range map[string][]batchFeeWeight{
		"nil job":   {{JobID: uuid.Nil, WeightMicros: 1}},
		"duplicate": {{JobID: duplicate, WeightMicros: 1}, {JobID: duplicate, WeightMicros: 2}},
		"sum overflow": {{JobID: duplicate, WeightMicros: math.MaxInt64},
			{JobID: uuid.MustParse("30000000-0000-4000-8000-000000000002"), WeightMicros: 1}},
	} {
		if _, err := allocateBatchFeeMicros(1, weights); err == nil {
			t.Fatalf("%s allocation was accepted", name)
		}
	}
}

func seedBatchFeeFixture(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	pi string,
	weights []int64,
	feeMicros int64,
) (uuid.UUID, uuid.UUID, []uuid.UUID) {
	t.Helper()
	buyerID := uuid.New()
	batchID := uuid.New()
	var batchMicros int64
	for _, weight := range weights {
		batchMicros += weight
	}
	if _, err := pool.Exec(ctx, `INSERT INTO charge_batches
		(id,buyer_id,amount_usd,status,stripe_pi,charged_at)
		VALUES ($1,$2,$3::numeric/1000000,'charged',$4,now())`,
		batchID, buyerID, batchMicros, pi,
	); err != nil {
		t.Fatalf("seed charge batch: %v", err)
	}
	jobIDs := make([]uuid.UUID, len(weights))
	for index, weight := range weights {
		jobIDs[index] = uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO jobs
			(id,buyer_id,created_at,status,job_type,input_ref,billed_usd,
			 charge_batch_id,charge_status)
			VALUES ($1,$2,$3,'complete','embed',$4,$5::numeric/1000000,$6,'charged')`,
			jobIDs[index], buyerID, time.Unix(int64(index+1), 0).UTC(),
			"fixture/"+jobIDs[index].String(), weight, batchID,
		); err != nil {
			t.Fatalf("seed batch job %d: %v", index, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ledger_entries
		(kind,buyer_id,amount_usd,payout_status,payout_ref)
		VALUES ('stripe_fee',$1,-($2::numeric/1000000),'released',$3)`,
		buyerID, feeMicros, pi,
	); err != nil {
		t.Fatalf("seed Stripe fee: %v", err)
	}
	return buyerID, batchID, jobIDs
}

func TestBatchFeeAllocationIsDurableConcurrentAndFailClosed(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	pi := "pi_batch_fee_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	buyerID, batchID, jobIDs := seedBatchFeeFixture(t, ctx, pool, pi, []int64{100, 1, 1}, 2)

	missing, err := store.BatchStripeFeesMissingAllocations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != pi {
		t.Fatalf("missing allocations=%v, want [%s]", missing, pi)
	}

	const callers = 8
	var wait sync.WaitGroup
	errorsSeen := make(chan error, callers)
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			allocated, err := store.AllocateBatchStripeFee(ctx, pi)
			if err == nil && !allocated {
				err = errors.New("matching batch was not allocated")
			}
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent allocation: %v", err)
		}
	}

	type persisted struct {
		JobID    uuid.UUID
		Weight   int64
		Amount   int64
		Ordinal  int
		Created  time.Time
		StripePI string
		Method   string
	}
	read := func() []persisted {
		rows, err := pool.Query(ctx, `SELECT job_id,
				(billed_weight_usd*1000000)::bigint,
				(allocated_fee_usd*1000000)::bigint,
				allocation_ordinal,allocated_at,stripe_pi,allocation_method
			FROM charge_batch_fee_allocations
			WHERE charge_batch_id=$1 ORDER BY allocation_ordinal`, batchID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []persisted
		for rows.Next() {
			var item persisted
			if err := rows.Scan(
				&item.JobID, &item.Weight, &item.Amount,
				&item.Ordinal, &item.Created, &item.StripePI, &item.Method,
			); err != nil {
				t.Fatal(err)
			}
			result = append(result, item)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := read()
	if len(first) != 3 {
		t.Fatalf("persisted allocations=%d, want 3", len(first))
	}
	for index, item := range first {
		wantAmount := int64(0)
		if index == 0 {
			wantAmount = 2
		}
		if item.JobID != jobIDs[index] || item.Ordinal != index ||
			item.StripePI != pi || item.Method != batchFeeAllocationHamiltonV1 ||
			item.Amount != wantAmount {
			t.Fatalf("allocation[%d]=%+v", index, item)
		}
	}
	for index, wantMicros := range []int64{2, 0, 0} {
		invoice, err := store.JobInvoice(ctx, jobIDs[index], buyerID)
		if err != nil {
			t.Fatalf("job %d invoice: %v", index, err)
		}
		if invoice.ProcessorFeeAllocatedUSD == nil ||
			math.Abs(*invoice.ProcessorFeeAllocatedUSD-float64(wantMicros)/1_000_000) > 1e-12 {
			t.Fatalf(
				"job %d processor fee=%v, want %d microUSD",
				index, invoice.ProcessorFeeAllocatedUSD, wantMicros,
			)
		}
		if invoice.ProcessorFeeAllocationMethod == nil ||
			*invoice.ProcessorFeeAllocationMethod != batchFeeAllocationHamiltonV1 {
			t.Fatalf("job %d allocation method=%v, want %s",
				index, invoice.ProcessorFeeAllocationMethod, batchFeeAllocationHamiltonV1)
		}
		if invoice.PlatformNetAfterProcessorUSD == nil ||
			math.Abs(*invoice.PlatformNetAfterProcessorUSD+float64(wantMicros)/1_000_000) > 1e-12 {
			t.Fatalf(
				"job %d platform net=%v, want -%d microUSD",
				index, invoice.PlatformNetAfterProcessorUSD, wantMicros,
			)
		}
		encoded, err := json.Marshal(invoice)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"processor_fee_allocated_usd"`) ||
			strings.Contains(string(encoded), pi) ||
			strings.Contains(string(encoded), `"stripe_pi"`) {
			t.Fatalf("invoice fee attribution is absent or leaks provider identity: %s", encoded)
		}
	}

	allocated, err := store.AllocateBatchStripeFee(ctx, pi)
	if err != nil || !allocated {
		t.Fatalf("idempotent allocation=(%t,%v)", allocated, err)
	}
	second := read()
	for index := range first {
		if first[index] != second[index] {
			t.Fatalf("idempotent call rewrote allocation[%d]: before=%+v after=%+v",
				index, first[index], second[index])
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE charge_batch_fee_allocations
		SET allocated_fee_usd=allocated_fee_usd+0.000001
		WHERE charge_batch_id=$1 AND job_id=$2`, batchID, jobIDs[0]); err == nil {
		t.Fatal("database allowed mutation of a settled fee allocation")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM charge_batch_fee_allocations
		WHERE charge_batch_id=$1 AND job_id=$2`, batchID, jobIDs[0]); err == nil {
		t.Fatal("database allowed deletion of a settled fee allocation")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO charge_batches
		(buyer_id,amount_usd,status,stripe_pi,charged_at)
		VALUES ($1,0.000001,'charged',$2,now())`, uuid.New(), pi); err == nil {
		t.Fatal("database allowed one Stripe PaymentIntent to bind two charge batches")
	}
	missing, err = store.BatchStripeFeesMissingAllocations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("fully allocated batch remains missing: %v", missing)
	}

	legacyPI := "pi_legacy_fee_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	legacyBuyer, legacyBatch, legacyJobs := seedBatchFeeFixture(
		t, ctx, pool, legacyPI, []int64{100, 1, 1}, 2,
	)
	if _, err := pool.Exec(ctx, `INSERT INTO charge_batch_fee_allocations
		(charge_batch_id,job_id,stripe_pi,allocation_ordinal,
		 billed_weight_usd,allocated_fee_usd,allocation_method)
		VALUES
		($1,$2,$3,0,0.000100,0.000001,$6),
		($1,$4,$3,1,0.000001,0.000000,$6),
		($1,$5,$3,2,0.000001,0.000001,$6)`,
		legacyBatch, legacyJobs[0], legacyPI, legacyJobs[1], legacyJobs[2],
		batchFeeAllocationLegacyV0,
	); err != nil {
		t.Fatal(err)
	}
	allocated, err = store.AllocateBatchStripeFee(ctx, legacyPI)
	if err != nil || !allocated {
		t.Fatalf("legacy allocation verification=(%t,%v)", allocated, err)
	}
	legacyInvoice, err := store.JobInvoice(ctx, legacyJobs[2], legacyBuyer)
	if err != nil {
		t.Fatal(err)
	}
	if legacyInvoice.ProcessorFeeAllocationMethod == nil ||
		*legacyInvoice.ProcessorFeeAllocationMethod != batchFeeAllocationLegacyV0 ||
		legacyInvoice.ProcessorFeeAllocatedUSD == nil ||
		math.Abs(*legacyInvoice.ProcessorFeeAllocatedUSD-0.000001) > 1e-12 {
		t.Fatalf("legacy invoice did not preserve its economic method: %+v", legacyInvoice)
	}

	partialPI := "pi_partial_fee_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	partialBuyer, partialBatch, partialJobs := seedBatchFeeFixture(
		t, ctx, pool, partialPI, []int64{1, 1}, 1,
	)
	outsiderJob := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO jobs
		(id,buyer_id,status,job_type,input_ref,billed_usd,charge_status)
		VALUES ($1,$2,'complete','embed',$3,0.000001,'charged')`,
		outsiderJob, partialBuyer, "fixture/"+outsiderJob.String(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO charge_batch_fee_allocations
		(charge_batch_id,job_id,stripe_pi,allocation_ordinal,billed_weight_usd,allocated_fee_usd)
		VALUES ($1,$2,$3,0,0.000001,0.000001)`,
		partialBatch, outsiderJob, partialPI,
	); err == nil {
		t.Fatal("database allowed an allocation to bind a job outside its charge batch")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO charge_batch_fee_allocations
		(charge_batch_id,job_id,stripe_pi,allocation_ordinal,billed_weight_usd,allocated_fee_usd)
		VALUES ($1,$2,$3,0,0.000001,0.000001)`,
		partialBatch, partialJobs[0], "pi_wrong_provider_reference",
	); err == nil {
		t.Fatal("database allowed an allocation to bind the wrong provider reference")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO charge_batch_fee_allocations
		(charge_batch_id,job_id,stripe_pi,allocation_ordinal,
		 billed_weight_usd,allocated_fee_usd,allocation_method)
		VALUES ($1,$2,$3,0,0.000001,0.000001,'unknown_method')`,
		partialBatch, partialJobs[0], partialPI,
	); err == nil {
		t.Fatal("database allowed an unknown fee allocation method")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO charge_batch_fee_allocations
		(charge_batch_id,job_id,stripe_pi,allocation_ordinal,billed_weight_usd,allocated_fee_usd)
		VALUES ($1,$2,$3,0,0.000001,0.000001)`,
		partialBatch, partialJobs[0], partialPI,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AllocateBatchStripeFee(ctx, partialPI); err == nil ||
		!strings.Contains(err.Error(), "partial immutable fee allocation") {
		t.Fatalf("partial immutable allocation was not rejected: %v", err)
	}
	if _, err := store.JobInvoice(ctx, partialJobs[0], partialBuyer); err == nil ||
		!strings.Contains(err.Error(), "processor-fee allocation is incomplete") {
		t.Fatalf("invoice exposed a partial batch fee allocation: %v", err)
	}

	singleBuyer := uuid.New()
	singleJob := uuid.New()
	singlePI := "pi_single_fee_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := pool.Exec(ctx, `INSERT INTO jobs
		(id,buyer_id,status,job_type,input_ref,actual_usd,billed_usd,
		 charge_status,stripe_pi)
		VALUES ($1,$2,'complete','embed',$3,0.01,0.01,'charged',$4)`,
		singleJob, singleBuyer, "fixture/"+singleJob.String(), singlePI,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ledger_entries
		(kind,buyer_id,amount_usd,payout_status,payout_ref)
		VALUES ('stripe_fee',$1,-0.000007,'released',$2)`,
		singleBuyer, singlePI,
	); err != nil {
		t.Fatal(err)
	}
	singleInvoice, err := store.JobInvoice(ctx, singleJob, singleBuyer)
	if err != nil {
		t.Fatal(err)
	}
	if singleInvoice.ProcessorFeeAllocatedUSD == nil ||
		math.Abs(*singleInvoice.ProcessorFeeAllocatedUSD-0.000007) > 1e-12 {
		t.Fatalf("single-job processor fee=%v, want 0.000007",
			singleInvoice.ProcessorFeeAllocatedUSD)
	}
	if singleInvoice.ProcessorFeeAllocationMethod == nil ||
		*singleInvoice.ProcessorFeeAllocationMethod != processorFeeAllocationDirectV1 {
		t.Fatalf("single-job allocation method=%v, want %s",
			singleInvoice.ProcessorFeeAllocationMethod, processorFeeAllocationDirectV1)
	}
	if singleInvoice.PlatformNetAfterProcessorUSD == nil ||
		math.Abs(*singleInvoice.PlatformNetAfterProcessorUSD+0.000007) > 1e-12 {
		t.Fatalf("single-job platform net=%v, want -0.000007",
			singleInvoice.PlatformNetAfterProcessorUSD)
	}
}

func TestBatchFeeAllocationSchemaUpgradeLabelsAndPreservesLegacyFacts(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	pi := "pi_upgrade_fee_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, batchID, jobIDs := seedBatchFeeFixture(
		t, ctx, pool, pi, []int64{100, 1, 1}, 2,
	)
	if _, err := pool.Exec(ctx, `INSERT INTO charge_batch_fee_allocations
		(charge_batch_id,job_id,stripe_pi,allocation_ordinal,
		 billed_weight_usd,allocated_fee_usd)
		VALUES
		($1,$2,$3,0,0.000100,0.000001),
		($1,$4,$3,1,0.000001,0.000000),
		($1,$5,$3,2,0.000001,0.000001)`,
		batchID, jobIDs[0], pi, jobIDs[1], jobIDs[2],
	); err != nil {
		t.Fatal(err)
	}

	// Reconstruct the prior table shape, then reapply the canonical schema as
	// an upgrade. The migration must retain every amount and label the method;
	// it must not rewrite history using the new allocator.
	if _, err := pool.Exec(ctx, `
		DROP TRIGGER charge_batch_fee_allocations_append_only
			ON charge_batch_fee_allocations;
		ALTER TABLE charge_batch_fee_allocations DROP COLUMN allocation_method;
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, canonicalSchema); err != nil {
		t.Fatalf("reapply canonical schema over legacy allocations: %v", err)
	}
	if _, err := pool.Exec(ctx, canonicalSchema); err != nil {
		t.Fatalf("reapply current canonical schema idempotently: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT job_id,
			(allocated_fee_usd*1000000)::bigint,allocation_method
		FROM charge_batch_fee_allocations
		WHERE charge_batch_id=$1 ORDER BY allocation_ordinal`, batchID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantAmounts := []int64{1, 0, 1}
	index := 0
	for rows.Next() {
		var jobID uuid.UUID
		var amount int64
		var method string
		if err := rows.Scan(&jobID, &amount, &method); err != nil {
			t.Fatal(err)
		}
		if index >= len(jobIDs) || jobID != jobIDs[index] ||
			amount != wantAmounts[index] || method != batchFeeAllocationLegacyV0 {
			t.Fatalf("upgraded legacy allocation[%d]=(%s,%d,%s)",
				index, jobID, amount, method)
		}
		index++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if index != len(jobIDs) {
		t.Fatalf("upgraded legacy rows=%d, want %d", index, len(jobIDs))
	}
	allocated, err := store.AllocateBatchStripeFee(ctx, pi)
	if err != nil || !allocated {
		t.Fatalf("upgraded legacy verification=(%t,%v)", allocated, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM charge_batch_fee_allocations
		WHERE charge_batch_id=$1`, batchID); err == nil {
		t.Fatal("schema reapply did not restore append-only protection")
	}
}

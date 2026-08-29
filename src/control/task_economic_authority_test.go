package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestQuoteUniformTaskEconomicAuthorityRefusesUnpricedTaskGeometry(t *testing.T) {
	sub := jobSubmit{JobType: JobType{Type: "batch_infer"}}
	valid := []byte("{\"prompt\":\"one exact task\"}\n")
	if err := validateQuoteUniformTaskEconomicAuthority(sub, valid, 1, 1, 0); err != nil {
		t.Fatalf("one primary plus exact redundancy was refused: %v", err)
	}
	for _, tc := range []struct {
		name                    string
		input                   []byte
		primary, redundancy, hp int
	}{
		{"uneven tail", valid, 2, 1, 0},
		{"no effective redundancy", valid, 1, 0, 0},
		{"heterogeneous honeypot", valid, 1, 1, 1},
		{"blank line dropped", []byte("{\"prompt\":\"a\"}\n\n"), 1, 1, 0},
		{"CRLF normalized", []byte("{\"prompt\":\"a\"}\r\n"), 1, 1, 0},
		{"final newline added", []byte("{\"prompt\":\"a\"}"), 1, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateQuoteUniformTaskEconomicAuthority(
				sub, tc.input, tc.primary, tc.redundancy, tc.hp)
			if !errors.Is(err, errHeterogeneousTaskEconomicsUnavailable) {
				t.Fatalf("error = %v, want heterogeneous task-economics refusal", err)
			}
		})
	}
}

func TestJobRequestObjectPrefixSeparatesConflictingIdempotentBodies(t *testing.T) {
	jobID := uuid.New()
	first := strings.Repeat("a", 64)
	second := strings.Repeat("b", 64)
	firstPrefix := jobRequestObjectPrefix(jobID, first)
	secondPrefix := jobRequestObjectPrefix(jobID, second)
	if firstPrefix == secondPrefix {
		t.Fatalf("conflicting request bodies share object prefix %q", firstPrefix)
	}
	if !strings.HasSuffix(firstPrefix, "/requests/"+first) ||
		!strings.HasSuffix(secondPrefix, "/requests/"+second) {
		t.Fatalf("request digest is absent from object prefixes: %q %q",
			firstPrefix, secondPrefix)
	}
	if got := jobRequestObjectPrefix(jobID, ""); got != "jobs/"+jobID.String() {
		t.Fatalf("historical empty-digest object prefix=%q", got)
	}
}

func TestCurrentUniformTaskEconomicAuthorityRequiresExactClones(t *testing.T) {
	compute := ComputePlan{
		Version: computePlanVersion, SettlementInputUnits: 1, InputRecords: 32,
		InputDepthProfile: &InputDepthProfile{P90DepthBand: inputDepthBandShort},
		PrimaryTasks:      1, RedundancyTasks: 1, HoneypotTasks: 0, TotalInitialTasks: 2,
	}
	economic := EconomicPlan{
		EconomicRoundingPolicy:  economicRoundingPolicy,
		Input:                   EconomicPlanInput{InitialTaskCount: 2, ExtraTaskReserve: 1},
		BuyerChargePerTaskNanos: 200, SupplierPayoutPerTaskNanos: 100,
	}
	pricing := PricingDecision{
		ExecutionMode:             computeExecutionDistributed,
		TaskEconomicPolicy:        uniformSinglePrimaryTaskEconomicsV1,
		SupplierEntitlementPolicy: economicRoundingPolicy,
		SupplierGrossNanos:        100, SupplierRequiredNanos: 100,
	}
	inputSHA := strings.Repeat("a", 64)
	workload := WorkloadDecision{Binding: WorkloadBinding{InputSHA256: inputSHA}}
	primary := taskRow{
		ID: uuid.New(), InputRef: "jobs/j/tasks/p/input.jsonl", InputDepthBand: "short",
		InputSHA256: inputSHA, ChunkIndex: 0, ExpectedOutputRecords: 32,
	}
	clone := taskRow{
		ID: uuid.New(), IsRedundancy: true, InputRef: primary.InputRef,
		InputSHA256:    primary.InputSHA256,
		InputDepthBand: primary.InputDepthBand, ChunkIndex: primary.ChunkIndex,
		ExpectedOutputRecords: primary.ExpectedOutputRecords,
	}
	if err := validateCurrentUniformTaskEconomicAuthority(
		workload, compute, economic, pricing, []taskRow{primary, clone}); err != nil {
		t.Fatalf("exact uniform task authority was refused: %v", err)
	}

	bad := clone
	bad.ID = uuid.New()
	bad.ExpectedOutputRecords++
	if err := validateCurrentUniformTaskEconomicAuthority(
		workload, compute, economic, pricing, []taskRow{primary, bad}); !errors.Is(err, errHeterogeneousTaskEconomicsUnavailable) {
		t.Fatalf("mismatched clone error = %v, want refusal", err)
	}

	multi := compute
	multi.PrimaryTasks = 2
	multi.TotalInitialTasks = 3
	if err := validateCurrentUniformTaskEconomicAuthority(workload, multi, economic, pricing, nil); !errors.Is(err, errHeterogeneousTaskEconomicsUnavailable) {
		t.Fatalf("multi-primary quote authority error = %v, want refusal", err)
	}

	legacyGeometry := compute
	legacyGeometry.Version = computePlanVersionV2
	legacyGeometry.SettlementInputUnits = 0
	if err := validateCurrentUniformTaskEconomicAuthority(
		workload, legacyGeometry, economic, pricing, nil,
	); !errors.Is(err, errHeterogeneousTaskEconomicsUnavailable) {
		t.Fatalf("legacy estimated-token task authority error = %v, want refusal", err)
	}
}

func TestSubmitJobTxRefusesTaskOwnedByAnotherJobBeforeWrites(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
	task := makeTasks(f, 1)[0]
	task.JobID = uuid.New()
	err := store.SubmitJobTx(ctx, &jobRow{ID: f.JobID}, []taskRow{task})
	if err == nil || !strings.Contains(err.Error(), "want submitted job") {
		t.Fatalf("foreign task job error = %v, want ownership refusal", err)
	}
	if countJobRows(t, ctx, pool, f.JobID) != 0 {
		t.Fatal("foreign task job id left a job row")
	}
	var taskRows, planRows, reserveRows int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE id=$1`, task.ID).Scan(&taskRows))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM job_economic_plans WHERE job_id=$1`, f.JobID).Scan(&planRows))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM job_economic_reserves WHERE job_id=$1`, f.JobID).Scan(&reserveRows))
	if taskRows != 0 || planRows != 0 || reserveRows != 0 {
		t.Fatalf("foreign task job id left task=%d plan=%d reserve=%d",
			taskRows, planRows, reserveRows)
	}
}

func TestSubmitJobTxRefusesMismatchedUniformCloneWithZeroWrites(t *testing.T) {
	// Install the explicit current mechanics authority before seeding workers;
	// worker/profile binding is itself fail-closed against the active runtime
	// projection and must never borrow the superseded production receipt.
	installTestOnlyCombinedTokenAuthority(t)
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 2})
	f.Plan = BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 0.40, InitialTaskCount: 2, ExtraTaskReserve: 1,
		SupplierShare: f.Plan.Input.SupplierShare,
	}, f.Plan.Schedule)
	tasks := makeTasks(f, 2)
	tasks[0].InputRef = "money/exact-primary.jsonl"
	tasks[0].InputDepthBand = inputDepthBandShort
	tasks[0].ExpectedOutputRecords = 1
	tasks[1].IsRedundancy = true
	tasks[1].InputRef = "money/different-redundancy.jsonl"
	tasks[1].InputDepthBand = tasks[0].InputDepthBand
	tasks[1].ExpectedOutputRecords = tasks[0].ExpectedOutputRecords
	job := validJobRowClasses(t, f, tasks, "", 1, 1, 0)
	job.EconomicInputSource = economicInputSourceSubmitStream
	for i := range tasks {
		tasks[i].InputSHA256 = job.WorkloadDecision.Binding.InputSHA256
	}

	err := store.SubmitJobTx(ctx, job, tasks)
	if !errors.Is(err, errQuotePhysicalAuthorityUnavailable) ||
		!errors.Is(err, errHeterogeneousTaskEconomicsUnavailable) {
		t.Fatalf("mismatched exact clone error = %v, want physical task-economics refusal", err)
	}
	if countJobRows(t, ctx, pool, f.JobID) != 0 {
		t.Fatal("mismatched exact clone left a job row")
	}
	var taskRows, reserveRows int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE job_id=$1`, f.JobID).Scan(&taskRows))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM job_economic_reserves WHERE job_id=$1`, f.JobID).Scan(&reserveRows))
	if taskRows != 0 || reserveRows != 0 {
		t.Fatalf("mismatched exact clone left tasks=%d reserve=%d", taskRows, reserveRows)
	}
}

func TestDynamicTaskWritersRefuseHeterogeneousCurrentCloneBeforeReserveSpend(t *testing.T) {
	// Keep one synthetic authority identity for the whole table while giving
	// every writer its own durable database. A current dynamic-task regression
	// must enter through one fully published v3 schedule; it may not borrow a
	// stale schedule or superseded production receipt.
	installBoundCataloguePublicationAuthorityForTest(t)
	installTestOnlyCombinedTokenAuthority(t)
	pinBoardClockForPublication(t)
	for _, writer := range []string{"hedge", "tiebreak", "planned_tiebreak"} {
		t.Run(writer, func(t *testing.T) {
			ctx, store, pool := openIsolatedMoneyPathStore(t)
			schedule, err := BuildCataloguePriceSchedule()
			mustf(t, err, "build current catalogue schedule: %v")
			_, err = store.ApplyRepricing(ctx, schedule)
			mustf(t, err, "publish current catalogue schedule: %v")
			f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 2})
			f.Plan = BuildEconomicPlan(EconomicPlanInput{
				BaseComputeUSD: 0.40, InitialTaskCount: 2, ExtraTaskReserve: 1,
				SupplierShare: f.Plan.Input.SupplierShare,
			}, f.Plan.Schedule)
			tasks := makeTasks(f, 2)
			tasks[0].InputRef = "money/exact-primary.jsonl"
			tasks[0].InputDepthBand = inputDepthBandShort
			tasks[0].ExpectedOutputRecords = 1
			tasks[1].IsRedundancy = true
			tasks[1].InputRef = tasks[0].InputRef
			tasks[1].InputDepthBand = tasks[0].InputDepthBand
			tasks[1].ChunkIndex = tasks[0].ChunkIndex
			tasks[1].ExpectedOutputRecords = tasks[0].ExpectedOutputRecords
			job := validJobRowClasses(t, f, tasks, "", 1, 1, 0)
			job.EconomicInputSource = economicInputSourceSubmitStream
			for i := range tasks {
				tasks[i].InputSHA256 = job.WorkloadDecision.Binding.InputSHA256
			}
			// validJobRowClasses deliberately fabricates a self-contained catalogue
			// for historical money-path tests. This is a current-ingress test: bind
			// the job back to the genuinely published v3 schedule before submitting.
			currentCatalogue, err := store.LoadCataloguePriceAuthority(
				ctx, job.WorkloadDecision.Binding.Model.Ref)
			mustf(t, err, "load current catalogue authority: %v")
			currentEconomicInput := job.EconomicPlan.Input
			currentEconomicInput.SupplierShare = currentCatalogue.SupplierShare
			currentEconomic := BuildEconomicPlan(
				currentEconomicInput, job.EconomicPlan.Schedule)
			if !currentEconomic.Executable {
				t.Fatalf("current task fixture economics blocked: %s", currentEconomic.BlockReason)
			}
			currentPlacement := placementForPricingFixture(
				t, job.WorkloadDecision, currentCatalogue)
			currentPricing, err := newDistributedPricingDecision(
				job.WorkloadDecision, job.ComputePlan, currentPlacement,
				currentEconomic, currentCatalogue, job.Tier, "")
			mustf(t, err, "bind current task fixture pricing: %v")
			job.EconomicPlan = currentEconomic
			job.PlacementRequirement = currentPlacement
			job.PricingDecision = currentPricing
			job.HWClasses = append([]string(nil), currentPlacement.HWClasses...)
			job.OfferedRateUsdHr = currentPlacement.OfferedRateUsdHr
			job.EstimatedUSD = currentEconomic.InitialBuyerChargeUSD
			mustf(t, store.SubmitJobTx(ctx, job, tasks), "submit uniform task fixture: %v")
			var primarySHA, cloneSHA string
			mustf(t, pool.QueryRow(ctx,
				`SELECT input_sha256 FROM tasks WHERE id=$1`, tasks[0].ID).Scan(&primarySHA),
				"read primary task digest: %v")
			mustf(t, pool.QueryRow(ctx,
				`SELECT input_sha256 FROM tasks WHERE id=$1`, tasks[1].ID).Scan(&cloneSHA),
				"read clone task digest: %v")
			if primarySHA != job.WorkloadDecision.Binding.InputSHA256 || cloneSHA != primarySHA {
				t.Fatalf("durable task digests primary=%q clone=%q workload=%q",
					primarySHA, cloneSHA, job.WorkloadDecision.Binding.InputSHA256)
			}
			if writer == "hedge" {
				if _, updateErr := pool.Exec(ctx,
					`UPDATE tasks SET input_ref='jobs/attacker/repointed.jsonl' WHERE id=$1`, tasks[0].ID,
				); updateErr == nil || !strings.Contains(updateErr.Error(), "input reference") {
					t.Fatalf("live input-ref update error=%v, want immutable authority", updateErr)
				}
				forgedID := uuid.New()
				_, insertErr := pool.Exec(ctx, `
					INSERT INTO tasks
					  (id,job_id,status,is_redundancy,input_ref,input_sha256,input_depth_band,
					   result_key,chunk_index,expected_output_records,
					   economic_buyer_charge_usd,economic_supplier_payout_usd,
					   economic_buyer_charge_nanos,economic_supplier_payout_nanos)
					SELECT $1,job_id,'queued',true,input_ref,input_sha256,input_depth_band,
					       $2,chunk_index,expected_output_records,
					       economic_buyer_charge_usd,economic_supplier_payout_usd,
					       economic_buyer_charge_nanos,economic_supplier_payout_nanos
					  FROM tasks WHERE id=$3`,
					forgedID, taskAttemptResultKey(f.JobID, forgedID, 0), tasks[0].ID)
				if insertErr == nil || !strings.Contains(insertErr.Error(), "exceeds initial plus consumed reserve") {
					t.Fatalf("unreserved current clone error=%v, want schema refusal", insertErr)
				}
			}

			err = nil
			switch writer {
			case "hedge":
				_, err = pool.Exec(ctx, `UPDATE jobs SET status='running' WHERE id=$1`, f.JobID)
				mustf(t, err, "mark hedge job running: %v")
				_, err = pool.Exec(ctx, `UPDATE tasks SET status='running' WHERE id=$1`, tasks[0].ID)
				mustf(t, err, "mark hedge anchor running: %v")
				_, err = store.InsertHedgeTask(
					ctx, f.JobID, tasks[0].ID, f.WorkerID, "money/different.jsonl", 0)
			case "tiebreak":
				_, err = store.InsertTiebreakTask(
					ctx, f.JobID, tasks[0].ID, f.WorkerID, "money/different.jsonl", 0)
			case "planned_tiebreak":
				tx, beginErr := pool.Begin(ctx)
				mustf(t, beginErr, "begin planned tiebreak: %v")
				_, err = insertPlannedTiebreakTx(ctx, tx, nil, VerificationEffect{
					ID: uuid.New(), Kind: VerificationEffectInsertTiebreak,
					JobID: f.JobID, TaskID: uuid.New(), PrimaryTaskID: tasks[0].ID,
					PeerWorkerID: f.WorkerID, InputRef: "money/different.jsonl", ChunkIndex: 0,
				})
				mustf(t, tx.Commit(ctx), "commit refused planned tiebreak tx: %v")
			}
			if !errors.Is(err, errHeterogeneousTaskEconomicsUnavailable) {
				t.Fatalf("%s mismatch error = %v, want heterogeneous refusal", writer, err)
			}
			var taskRows, consumed int
			must(t, pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE job_id=$1`, f.JobID).Scan(&taskRows))
			must(t, pool.QueryRow(ctx, `SELECT consumed_tasks FROM job_economic_reserves WHERE job_id=$1`, f.JobID).Scan(&consumed))
			if taskRows != 2 || consumed != 0 {
				t.Fatalf("%s mismatch left tasks=%d consumed_reserve=%d", writer, taskRows, consumed)
			}
		})
	}
}

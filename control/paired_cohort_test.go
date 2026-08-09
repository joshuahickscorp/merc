package main

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

// The former paired cohort drove this workload through both embed cells. Its
// retained receipt is WITHDRAWN: the benchmark measures embeddings/s while the
// actual text settlement contract uses max(records, raw bytes/4), and no frozen
// conversion binds those dimensions. The ordinary test preserves the exact
// corpus arithmetic as a negative regression. The opt-in entry point refuses
// before agent enrolment or durable writes until a new, separately identified
// authority path measures the settlement unit itself.
//
// MERC_PAIRED_COHORT remains registered as an opt-in skip so attempts to revive
// the physical harness are visible and fail loudly instead of minting evidence
// under the withdrawn receipt identity.
//
// cohortRecordsPerTask is the batch size each cohort task embeds. Batch 32 is on
// the measured curve in evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r1.json,
// where the two engines are 2.7x apart — far enough that a real difference cannot
// be mistaken for timer noise.
const cohortRecordsPerTask = 32

const (
	// This rate is a synthetic reconstruction of the withdrawn historical cohort,
	// not the current market-board catalogue price. It is useful for proving that
	// the runnable harness and the withdrawn receipt used the same arithmetic; it
	// cannot mint replacement pricing authority.
	cohortHistoricalFixtureReferencePricePer1K = 0.00625
	cohortHistoricalFixtureSupplierShare       = 0.97

	// These two figures are the tempting records-only calculation. They are
	// retained solely as a negative regression below: text settlement authority
	// is max(records, exact raw input bytes/4), so neither may price the real
	// 2,380-byte cohort corpus.
	cohortRecordOnlyBaseComputeUSD    = 0.0002
	cohortRecordOnlySupplierPayoutUSD = 0.000194

	cohortCorpusBytes                          = 2380
	cohortSettlementInputUnits                 = 595.0
	cohortHistoricalBaseComputePerTaskUSD      = 0.003719
	cohortHistoricalSupplierPayoutPerTaskUSD   = 0.003607
	cohortHistoricalSupplierPayoutPerOutputUSD = 0.00011271875
	cohortHistoricalBaseComputePerTaskNanos    = 3_718_750
	cohortHistoricalSupplierPayoutPerTaskNanos = 3_607_188

	// The retained source receipt was withdrawn. The fixture rate above is not
	// authority to supersede it, so the generator remains terminal until a new
	// path/id and a current catalogue authority are introduced deliberately.
	cohortReceiptBindingStatus = BindingWithdrawn
)

func pairedCohortCorpus() []byte {
	var corpus []byte
	for i := 0; i < cohortRecordsPerTask; i++ {
		corpus = append(corpus, []byte(fmt.Sprintf(
			`{"id":"%d","text":"Merc settles every task against a receipt, record %d."}`+"\n",
			i, i))...)
	}
	return corpus
}

// pairedCohortGeometry reconstructs only the exact input and money geometry of
// the withdrawn historical cohort. It is deliberately not an executable job:
// the retained embed benchmarks measure embeddings/s while the ComputePlan
// settles token-like input units. No authority currently binds that conversion.
type pairedCohortGeometry struct {
	Workload  WorkloadDecision
	Catalogue CataloguePriceAuthority
	Economic  EconomicPlan
	Compute   ComputePlan
}

func pairedCohortGeometryFixture(
	t *testing.T, directedCellID string, corpus []byte,
) pairedCohortGeometry {
	t.Helper()

	workload, err := buildWorkloadDecisionDirected(jobSubmit{
		JobType: JobType{Type: "embed"},
		Model:   ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Tier:    "batch",
		Constraints: JobConstraints{
			MaxDurationSecs: 3600,
		},
	}, strings.Repeat("a", 64), directedCellID)
	mustf(t, err, "build paired-cohort workload decision: %v")

	depth, err := buildInputDepthProfileFromJSONL(corpus)
	mustf(t, err, "measure paired-cohort input depth: %v")
	if depth.ShortRecords+depth.MediumRecords+depth.LongRecords != cohortRecordsPerTask {
		t.Fatalf("paired-cohort corpus contains %d records, want %d",
			depth.ShortRecords+depth.MediumRecords+depth.LongRecords,
			cohortRecordsPerTask)
	}

	schedule := testEconomicSchedule()
	authority := catalogueAuthorityFixtureAtReferencePrice(
		t, workload, schedule.Currency, cohortHistoricalFixtureSupplierShare,
		cohortHistoricalFixtureReferencePricePer1K,
	)
	baseComputeNanos := exactBaseComputeNanosForJobType(
		authority, workload.Binding.JobType, workload.Binding.Tier,
		len(corpus), cohortRecordsPerTask, 1, 1,
	)
	baseComputeUSD, err := estimateJobSettlementForJobType(
		authority, workload.Binding.JobType, len(corpus), cohortRecordsPerTask,
		workload.Binding.Tier,
	)
	mustf(t, err, "derive paired-cohort catalogue settlement: %v")
	economicPlan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   baseComputeUSD,
		BaseComputeNanos: baseComputeNanos,
		InitialTaskCount: 1,
		ExtraTaskReserve: economicExtraTaskReserve(1),
		SupplierShare:    cohortHistoricalFixtureSupplierShare,
	}, schedule)
	if !economicPlan.Executable {
		t.Fatalf("paired-cohort economic plan blocked: %s", economicPlan.BlockReason)
	}
	mustf(t, ValidateEconomicPlanSnapshot(economicPlan),
		"paired-cohort economic plan invalid: %v")

	computePlan, err := newDistributedComputePlan(
		workload,
		cohortRecordsPerTask,
		int64(len(corpus)),
		depth,
		cohortRecordsPerTask,
		1,
		0,
		0,
		quoteTimeFromETABands(60, 0, false),
		"static",
		economicPlan.Input.BaseComputeUSD,
		0,
		QuoteConfidence{Score: 0.9, Reasons: []string{"paired cohort freezes exact MiniLM geometry"}},
		[]string{"paired cohort has no live fleet estimate"},
	)
	mustf(t, err, "build paired-cohort compute plan: %v")
	return pairedCohortGeometry{
		Workload: workload, Catalogue: authority,
		Economic: economicPlan, Compute: computePlan,
	}
}

// This assertion is deliberately not guarded by MERC_PAIRED_COHORT. The real
// engines remain opt-in, but their frozen economic geometry must be checked on
// every ordinary test run so the harness cannot silently return to the old
// one-record/$0.20 fixture.
func TestPairedCohortFreezesCanonicalMiniLMUnitsEconomicsAndGeometry(t *testing.T) {
	corpus := pairedCohortCorpus()
	if len(corpus) != cohortCorpusBytes {
		t.Fatalf("paired-cohort corpus bytes=%d, want frozen regression geometry %d",
			len(corpus), cohortCorpusBytes)
	}
	for _, cellID := range []string{candleEmbedCell, llamaEmbedCell} {
		t.Run(cellID, func(t *testing.T) {
			fixture := pairedCohortGeometryFixture(t, cellID, corpus)

			if fixture.Compute.InputRecords != cohortRecordsPerTask ||
				fixture.Compute.SplitSize != cohortRecordsPerTask ||
				fixture.Compute.PrimaryTasks != 1 {
				t.Fatalf("paired-cohort geometry drifted: compute=%+v", fixture.Compute)
			}
			if fixture.Compute.InputBytes != int64(len(corpus)) {
				t.Fatalf("paired-cohort byte authority=%d, want %d",
					fixture.Compute.InputBytes, len(corpus))
			}
			if got := fixture.Compute.SettlementInputUnits; got != cohortSettlementInputUnits {
				t.Fatalf("paired-cohort settlement units=%v, want %v",
					got, cohortSettlementInputUnits)
			}
			if got := fixture.Catalogue.ReferencePricePer1K; got != cohortHistoricalFixtureReferencePricePer1K {
				t.Fatalf("MiniLM reference price=%v, want %v",
					got, cohortHistoricalFixtureReferencePricePer1K)
			}
			if got := fixture.Economic.Input.BaseComputeUSD; got != cohortHistoricalBaseComputePerTaskUSD {
				t.Fatalf("MiniLM 32-record base compute=%v, want %v",
					got, cohortHistoricalBaseComputePerTaskUSD)
			}
			if got := fixture.Economic.SupplierPayoutPerTaskUSD; got != cohortHistoricalSupplierPayoutPerTaskUSD {
				t.Fatalf("MiniLM supplier payout/task=%v, want %v",
					got, cohortHistoricalSupplierPayoutPerTaskUSD)
			}
			perOutput := fixture.Economic.SupplierPayoutPerTaskUSD / cohortRecordsPerTask
			if math.Abs(perOutput-cohortHistoricalSupplierPayoutPerOutputUSD) > 1e-15 {
				t.Fatalf("MiniLM supplier payout/output=%.15f, want %.15f",
					perOutput, cohortHistoricalSupplierPayoutPerOutputUSD)
			}
			if fixture.Economic.Input.BaseComputeNanos != cohortHistoricalBaseComputePerTaskNanos ||
				fixture.Economic.SupplierPayoutPerTaskNanos != cohortHistoricalSupplierPayoutPerTaskNanos {
				t.Fatalf("MiniLM exact nanos base/payout=%d/%d, want %d/%d",
					fixture.Economic.Input.BaseComputeNanos,
					fixture.Economic.SupplierPayoutPerTaskNanos,
					cohortHistoricalBaseComputePerTaskNanos,
					cohortHistoricalSupplierPayoutPerTaskNanos)
			}
			mustf(t, ValidateComputePlanEconomicSnapshot(
				fixture.Compute, fixture.Workload, fixture.Economic),
				"paired-cohort compute/economic authority invalid: %v")

			_, err := supplierAdmissionCeilingUSDHr(
				fixture.Catalogue, fixture.Workload.RuntimeJobType,
				fixture.Workload.Binding.Tier,
				admissionCellsForWorkload(fixture.Workload),
				fixture.Workload.Binding.Constraints.HWClasses,
			)
			if err == nil {
				t.Fatal("historical embed cohort unexpectedly became current-admissible")
			}
			if !strings.Contains(err.Error(), "no frozen unit conversion authority") &&
				!strings.Contains(err.Error(), "no usable benchmark authority") {
				t.Fatalf("current embed cohort refusal hid its physical/unit authority gap: %v", err)
			}

			// $0.000194/task is 32/1000 × $0.00625 × 97%: correct
			// arithmetic for a records-only contract, but not for this text contract
			// and corpus. Prove it cannot be substituted into the valid composite
			// plan; otherwise the original correction would merely replace one
			// unbound denominator with another.
			recordOnlyPlan := BuildEconomicPlan(EconomicPlanInput{
				BaseComputeUSD:   cohortRecordOnlyBaseComputeUSD,
				BaseComputeNanos: 200_000,
				InitialTaskCount: 1,
				ExtraTaskReserve: economicExtraTaskReserve(1),
				SupplierShare:    cohortHistoricalFixtureSupplierShare,
			}, testEconomicSchedule())
			if got := recordOnlyPlan.SupplierPayoutPerTaskUSD; got != cohortRecordOnlySupplierPayoutUSD {
				t.Fatalf("negative fixture no longer demonstrates records-only payout: %v", got)
			}
			if err := ValidateComputePlanEconomicSnapshot(
				fixture.Compute, fixture.Workload, recordOnlyPlan); err == nil {
				t.Fatal("accepted records-only $0.000194 payout for a 2,380-byte text corpus")
			}
		})
	}
}

func pairedCohortEvidenceWriteAuthority() error {
	if cohortReceiptBindingStatus != BindingBound {
		return fmt.Errorf(
			"paired-cohort receipt remains %s: the synthetic historical catalogue fixture cannot mint BOUND replacement economics",
			cohortReceiptBindingStatus)
	}
	return nil
}

func TestPairedCohortWriterCannotMintBoundEconomicsFromHistoricalFixture(t *testing.T) {
	err := pairedCohortEvidenceWriteAuthority()
	if err == nil || !strings.Contains(err.Error(), BindingWithdrawn) ||
		!strings.Contains(err.Error(), "cannot mint BOUND") {
		t.Fatalf("paired-cohort evidence write authority=%v, want terminal WITHDRAWN refusal", err)
	}
}

func TestPairedCohortMeasuresCostAndRegret(t *testing.T) {
	if os.Getenv("MERC_PAIRED_COHORT") != "1" {
		t.Skip("MERC_PAIRED_COHORT is not 1; the paired cohort is an opt-in " +
			"multi-minute run against two real agents")
	}
	fixture := pairedCohortGeometryFixture(t, candleEmbedCell, pairedCohortCorpus())
	_, err := supplierAdmissionCeilingUSDHr(
		fixture.Catalogue, fixture.Workload.RuntimeJobType,
		fixture.Workload.Binding.Tier,
		admissionCellsForWorkload(fixture.Workload),
		fixture.Workload.Binding.Constraints.HWClasses,
	)
	if err == nil {
		t.Fatal("paired cohort unexpectedly acquired current embed unit-conversion authority")
	}
	t.Fatalf("paired cohort is terminally refused before agent enrolment or durable writes: %v; introduce a new evidence path only after binding embeddings/s to settlement units/s", err)
}

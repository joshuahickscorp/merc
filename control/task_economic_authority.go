package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// uniformSinglePrimaryTaskEconomicsV1 is the deliberately narrow current
// settlement posture while Merc has no immutable per-task unit allocation.
// One primary owns the whole priced input; every other initially priced task is
// an exact redundancy clone of it. Historical decisions remain replayable under
// their own frozen policy, but cannot mint new work through this gate.
const uniformSinglePrimaryTaskEconomicsV1 = "uniform_single_primary_exact_clone_v1"

var errHeterogeneousTaskEconomicsUnavailable = errors.New(
	"exact heterogeneous per-task economics are unavailable")

// Current private-canary policy injects a known-answer honeypot into every
// physical job. That is deliberately heterogeneous work and cannot borrow the
// sole primary's money under the bounded v1 posture. Refuse quote and submit at
// the same pre-side-effect boundary until per-task allocation is frozen.
func validateCurrentUniformCanaryAuthority(canaryEnabled bool) error {
	if canaryEnabled {
		return fmt.Errorf("%w: private canary requires a heterogeneous honeypot that has no exact per-task allocation",
			errHeterogeneousTaskEconomicsUnavailable)
	}
	return nil
}

// validateQuoteUniformTaskEconomicAuthority proves that the bytes a quote
// prices can become exactly one task input, plus exact redundancy clones. The
// JSONL uploader drops blank lines, normalizes CRLF to LF, and appends a final
// newline; any such transformation means the task would not own the exact byte
// geometry used to derive its floor, so current admission refuses it.
func validateQuoteUniformTaskEconomicAuthority(
	sub jobSubmit,
	input []byte,
	primaryTasks, redundancyTasks, honeypotTasks int,
) error {
	if err := validateCurrentUniformTaskCounts(
		primaryTasks, redundancyTasks, honeypotTasks); err != nil {
		return err
	}
	if isBinaryMediaJob(sub) {
		return nil // one media/rendering task owns the exact full object
	}
	canonical, err := singleTaskJSONLBytes(input)
	if err != nil {
		return fmt.Errorf("%w: %v", errHeterogeneousTaskEconomicsUnavailable, err)
	}
	if !bytes.Equal(canonical, input) {
		return fmt.Errorf("%w: JSONL splitting would change the priced byte geometry (blank lines, CRLF, or a missing final newline)",
			errHeterogeneousTaskEconomicsUnavailable)
	}
	return nil
}

// validateCurrentUniformTaskCounts is the earliest side-effect boundary. It is
// intentionally independent of pricing objects so submit can refuse before it
// copies or registers a honeypot alias. The full immutable-plan/task proof runs
// again at durable ingress.
func validateCurrentUniformTaskCounts(
	primaryTasks, redundancyTasks, honeypotTasks int,
) error {
	if primaryTasks != 1 {
		return fmt.Errorf("%w: current pricing requires exactly one primary task, got %d",
			errHeterogeneousTaskEconomicsUnavailable, primaryTasks)
	}
	if redundancyTasks < 1 {
		return fmt.Errorf("%w: current pricing requires at least one exact redundancy clone",
			errHeterogeneousTaskEconomicsUnavailable)
	}
	if honeypotTasks != 0 {
		return fmt.Errorf("%w: current pricing cannot assign the primary task's money to %d heterogeneous honeypot tasks",
			errHeterogeneousTaskEconomicsUnavailable, honeypotTasks)
	}
	return nil
}

func singleTaskJSONLBytes(input []byte) ([]byte, error) {
	reader := bufio.NewReader(bytes.NewReader(input))
	var out bytes.Buffer
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			if len(bytes.TrimSpace(trimmed)) != 0 {
				out.Write(trimmed)
				out.WriteByte('\n')
			}
		}
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			break
		}
	}
	return out.Bytes(), nil
}

// validateCurrentUniformTaskEconomicAuthority is the durable current-ingress
// backstop. With one primary, BillableUnits is also the exact per-task unit
// count used by exactTaskEconomics. Redundancy and every later reserve task must
// be clones of that same input and therefore carry the same frozen nanos.
// Passing nil tasks validates a quote's frozen structural authority; job ingress
// supplies the concrete rows and proves every initial obligation is a clone.
func validateCurrentUniformTaskEconomicAuthority(
	workload WorkloadDecision,
	compute ComputePlan,
	economic EconomicPlan,
	pricing PricingDecision,
	tasks []taskRow,
) error {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", errHeterogeneousTaskEconomicsUnavailable,
			fmt.Sprintf(format, args...))
	}
	if pricing.ExecutionMode != computeExecutionDistributed ||
		pricing.TaskEconomicPolicy != uniformSinglePrimaryTaskEconomicsV1 {
		return refuse("distributed pricing lacks current uniform task-economic policy %q",
			uniformSinglePrimaryTaskEconomicsV1)
	}
	if compute.Version != computePlanVersion ||
		!finiteNonNegative(compute.SettlementInputUnits) ||
		compute.SettlementInputUnits <= 0 {
		return refuse("current task economics require ComputePlan v%d with positive exact settlement_input_units",
			computePlanVersion)
	}
	if compute.PrimaryTasks != 1 || compute.RedundancyTasks < 1 ||
		compute.HoneypotTasks != 0 ||
		compute.TotalInitialTasks != compute.PrimaryTasks+compute.RedundancyTasks {
		return refuse("frozen compute geometry is %d primary, %d redundancy, %d honeypot, %d total; want one primary, exact redundancy clones, and no honeypots",
			compute.PrimaryTasks, compute.RedundancyTasks, compute.HoneypotTasks,
			compute.TotalInitialTasks)
	}
	if economic.EconomicRoundingPolicy != economicRoundingPolicy ||
		economic.Input.InitialTaskCount != compute.TotalInitialTasks ||
		economic.Input.ExtraTaskReserve != 1 ||
		economic.BuyerChargePerTaskNanos <= 0 ||
		economic.SupplierPayoutPerTaskNanos <= 0 {
		return refuse("economic plan does not freeze one exact uniform amount for every initial task and the sole reserve clone")
	}
	if pricing.SupplierEntitlementPolicy != economicRoundingPolicy ||
		pricing.SupplierGrossNanos != economic.SupplierPayoutPerTaskNanos ||
		pricing.SupplierRequiredNanos <= 0 ||
		pricing.SupplierGrossNanos < pricing.SupplierRequiredNanos {
		return refuse("pricing entitlement does not equal the exact uniform task payout/floor")
	}
	if len(tasks) == 0 {
		return nil
	}
	if len(tasks) != compute.TotalInitialTasks {
		return refuse("received %d initial task rows, want %d", len(tasks), compute.TotalInitialTasks)
	}
	var primary *taskRow
	for i := range tasks {
		task := &tasks[i]
		if task.IsHoneypot {
			return refuse("task %s is a heterogeneous honeypot", task.ID)
		}
		if !task.IsRedundancy {
			if primary != nil {
				return refuse("more than one primary task is present")
			}
			primary = task
		}
	}
	if primary == nil || primary.InputRef == "" || primary.ExpectedOutputRecords <= 0 ||
		!validSHA256(primary.InputSHA256) ||
		primary.InputSHA256 != workload.Binding.InputSHA256 {
		return refuse("the sole primary task lacks exact input/output geometry")
	}
	if primary.ChunkIndex != 0 ||
		primary.ExpectedOutputRecords != int64(compute.InputRecords) {
		return refuse("the sole primary task geometry chunk=%d records=%d does not equal priced whole-input chunk=0 records=%d",
			primary.ChunkIndex, primary.ExpectedOutputRecords, compute.InputRecords)
	}
	if compute.InputDepthProfile != nil &&
		primary.InputDepthBand != compute.InputDepthProfile.P90DepthBand {
		return refuse("the sole primary task depth %q does not equal priced p90 depth %q",
			primary.InputDepthBand, compute.InputDepthProfile.P90DepthBand)
	}
	cloneCount := 0
	for i := range tasks {
		task := &tasks[i]
		if task == primary {
			continue
		}
		cloneCount++
		if !task.IsRedundancy || task.InputRef != primary.InputRef ||
			task.InputSHA256 != primary.InputSHA256 ||
			task.InputDepthBand != primary.InputDepthBand ||
			task.ChunkIndex != primary.ChunkIndex ||
			task.ExpectedOutputRecords != primary.ExpectedOutputRecords {
			return refuse("task %s is not an exact input/output clone of primary %s",
				task.ID, primary.ID)
		}
	}
	if cloneCount != compute.RedundancyTasks {
		return refuse("received %d exact clones, want %d", cloneCount, compute.RedundancyTasks)
	}
	return nil
}

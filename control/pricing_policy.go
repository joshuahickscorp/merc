package main

import (
	"fmt"
	"strings"
)

// supplierSharePolicyRevision is part of every newly published catalogue
// schedule.  The supplier share is a market term for a physical workload, not
// a process setting: it has to travel with the published price, the frozen
// pricing decision, the admission ceiling and the eventual supplier payable.
const supplierSharePolicyRevision = "physical-workload-share-v1"

type supplierSharePolicy struct {
	JobType       string
	ModelID       string
	SupplierShare float64
	Rationale     string
}

// physicalSupplierSharePolicies is deliberately keyed by the exact physical
// lane.  There is no default: publishing an unreviewed model under an existing
// percentage would recreate the old process-wide take rate under another name.
//
// Embed is high-throughput, short-tail work. Batch inference carries the longer
// completion/verification reserve and lower repeatability that the market needs
// Merc to finance. Fixed processor and control-plane costs are accounted for
// separately by EconomicSchedule; these shares describe the physical market
// term left after that honest batch allocation, rather than hiding those costs
// in a supplier haircut.
var physicalSupplierSharePolicies = []supplierSharePolicy{
	{
		JobType: "embed", ModelID: "all-minilm-l6-v2", SupplierShare: 0.94,
		Rationale: "high-throughput embedding lane with short execution tail",
	},
	{
		JobType: "batch_infer", ModelID: "llama-3.2-1b-instruct-q4", SupplierShare: 0.86,
		Rationale: "longer-tailed inference lane with verification and capacity reserve",
	},
}

func supplierShareForWorkload(jobType, modelID string) (float64, error) {
	jobType = strings.TrimSpace(jobType)
	modelID = strings.TrimSpace(modelID)
	for _, policy := range physicalSupplierSharePolicies {
		if policy.JobType == jobType && policy.ModelID == modelID {
			return policy.SupplierShare, nil
		}
	}
	return 0, fmt.Errorf(
		"no supplier-share policy for physical workload job_type=%q model=%q",
		jobType, modelID,
	)
}

func validateSupplierSharePolicy(jobType, modelID string, share float64) error {
	want, err := supplierShareForWorkload(jobType, modelID)
	if err != nil {
		return err
	}
	if share != want {
		return fmt.Errorf(
			"supplier share %.8f does not match %s policy %.8f for %s/%s",
			share, supplierSharePolicyRevision, want, jobType, modelID,
		)
	}
	return nil
}

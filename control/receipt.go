package main

import "github.com/google/uuid"

type ClearingReceipt struct {
	JobID           uuid.UUID              `json:"job_id"`
	Status          string                 `json:"status"`
	AuthorityStatus string                 `json:"authority_status"`
	Workload        *WorkloadDecision      `json:"workload_decision,omitempty"`
	ComputePlan     *ComputePlan           `json:"compute_plan,omitempty"`
	Placement       *PlacementRequirement  `json:"placement_requirement,omitempty"`
	Pricing         *PricingDecision       `json:"pricing_decision,omitempty"`
	Authority       ReceiptAuthority       `json:"authority"`
	Reconciliation  *PricingReconciliation `json:"pricing_reconciliation,omitempty"`
	Invoice         *InvoiceView           `json:"invoice"`
	Verification    Verification           `json:"verification"`
	Classes         []string               `json:"verification_classes"`
	Tasks           []TaskReceipt          `json:"tasks"`
}

type ReceiptAuthority struct {
	WorkloadDecisionSHA256     string `json:"workload_decision_sha256,omitempty"`
	ComputePlanSHA256          string `json:"compute_plan_sha256,omitempty"`
	PlacementRequirementSHA256 string `json:"placement_requirement_sha256,omitempty"`
	PricingDecisionSHA256      string `json:"pricing_decision_sha256,omitempty"`
	// VerificationContractSHA256 cites the acceptance-time verification identity
	// frozen on the compute plan. Absent on historical receipts. Task-level
	// verification_selected still answers whether a check ran; this digest
	// answers under which class/evaluator/thresholds the job was accepted.
	VerificationContractSHA256 string `json:"verification_contract_sha256,omitempty"`
}

type PricingReconciliation struct {
	Currency                    string  `json:"currency"`
	AcceptedBuyerPrice          float64 `json:"accepted_buyer_price"`
	MaximumBuyerPrice           float64 `json:"maximum_buyer_price"`
	SettledBuyerPrice           float64 `json:"settled_buyer_price"`
	ModeledSupplierCost         float64 `json:"modeled_supplier_cost"`
	SettledSupplierCost         float64 `json:"settled_supplier_cost"`
	ModeledPlatformContribution float64 `json:"modeled_platform_contribution"`
	SettledPlatformTake         float64 `json:"settled_platform_take"`
	// SettledPlatformTake is retained for compatibility; it is a gross ledger
	// spread. The contribution object refuses to call it net while any cost is
	// unknown.
	SettledPlatformGrossSpread float64                   `json:"settled_platform_gross_spread"`
	SettledContribution        *EconomicContributionView `json:"settled_contribution,omitempty"`
	ContributionSettlement     *ContributionSettlement   `json:"contribution_settlement,omitempty"`
	ModeledPaymentCost         float64                   `json:"modeled_payment_cost"`
	SettledPaymentCost         *float64                  `json:"settled_payment_cost,omitempty"`
	CatalogueScheduleSHA256    string                    `json:"catalogue_schedule_sha256"`
	FXRevision                 string                    `json:"fx_revision"`
}

type TaskReceipt struct {
	ChunkIndex int    `json:"chunk_index"`
	Status     string `json:"status"`
	IsHoneypot bool   `json:"is_honeypot"`
	// VerificationClass is the governed class this task was verified under, and
	// VerificationSelected whether the check actually ran. Together they let a
	// buyer distinguish "not checked because sampling declined" from "not checked
	// because nothing has been decided yet" — which the receipt could not say
	// before, so an unsampled task and an unverified one read identically.
	VerificationClass    string `json:"verification_class,omitempty"`
	VerificationSelected *bool  `json:"verification_selected,omitempty"`
	RuntimeCellID        string `json:"runtime_cell_id,omitempty"`
	RuntimeID            string `json:"runtime_id,omitempty"`
	RuntimeMatrixSHA     string `json:"runtime_matrix_sha256,omitempty"`
	ModelKind            string `json:"model_kind,omitempty"`
	WorkerClass          string `json:"worker_class"` // engine|build_hash|build_identity_policy (classKey); "" if unknown
	BuildIdentityPolicy  string `json:"build_identity_policy,omitempty"`
	VerificationKind     string `json:"verification_kind"` // latest comparison event kind; "" if none
	Verdict              string `json:"verdict"`           // durable current task verdict; "" until verified
	// Generative batch settlement evidence. Present when the task had a
	// generative output ceiling so the buyer can audit ceiling vs observed
	// vs the rebate applied at ledger-write time. Omitted for embed/legacy.
	OutputTokenCeiling   *int64   `json:"output_token_ceiling,omitempty"`
	ObservedOutputTokens *int64   `json:"observed_output_tokens,omitempty"`
	FrozenBuyerChargeUSD *float64 `json:"frozen_buyer_charge_usd,omitempty"`
	BilledBuyerChargeUSD *float64 `json:"billed_buyer_charge_usd,omitempty"`
	OutputTokenRebateUSD *float64 `json:"output_token_rebate_usd,omitempty"`
	// ContributionFloorClamped is true when the token-proportional rebate was
	// reduced so the settled charge still covers the accepted contribution floor.
	// Surfaced so the buyer can see why the full unused-output rebate was not applied.
	ContributionFloorClamped *bool    `json:"contribution_floor_clamped,omitempty"`
	UnclampedRebateUSD       *float64 `json:"unclamped_output_token_rebate_usd,omitempty"`
}

func taskReceiptRow(chunkIndex int, status string, isHoneypot bool, engine, build, kind, verdict string) TaskReceipt {
	return taskReceiptRowWithRuntime(chunkIndex, status, isHoneypot, engine, build, kind, verdict, "", "", "", "")
}

func taskReceiptRowWithRuntime(chunkIndex int, status string, isHoneypot bool, engine, build, kind, verdict, cellID, runtimeID, matrixSHA, modelKind string) TaskReceipt {
	return taskReceiptRowWithRuntimePolicy(chunkIndex, status, isHoneypot, engine, build, "",
		kind, verdict, cellID, runtimeID, matrixSHA, modelKind)
}

func taskReceiptRowWithRuntimePolicy(chunkIndex int, status string, isHoneypot bool, engine, build, buildPolicy, kind, verdict, cellID, runtimeID, matrixSHA, modelKind string) TaskReceipt {
	workerClass := classKey(engine, build)
	if buildPolicy != "" {
		workerClass = classKey(engine, build, buildPolicy)
	}
	return TaskReceipt{
		ChunkIndex:          chunkIndex,
		Status:              status,
		IsHoneypot:          isHoneypot,
		RuntimeCellID:       cellID,
		RuntimeID:           runtimeID,
		RuntimeMatrixSHA:    matrixSHA,
		ModelKind:           modelKind,
		WorkerClass:         workerClass,
		BuildIdentityPolicy: buildPolicy,
		VerificationKind:    kind,
		Verdict:             verdict,
	}
}

func assembleClearingReceipt(
	jobID uuid.UUID,
	status string,
	workload *WorkloadDecision,
	computePlan *ComputePlan,
	placement *PlacementRequirement,
	pricing *PricingDecision,
	inv *InvoiceView,
	verif Verification,
	classes []string,
	tasks []TaskReceipt,
) ClearingReceipt {
	out := ClearingReceipt{
		JobID: jobID, Status: status, AuthorityStatus: "legacy_unverifiable",
		Workload: workload, ComputePlan: computePlan, Placement: placement, Pricing: pricing,
		Invoice: inv, Verification: verif, Classes: classes, Tasks: tasks,
	}
	if workload != nil {
		out.Authority.WorkloadDecisionSHA256, _ = workloadDecisionDigest(*workload)
	}
	if computePlan != nil {
		out.Authority.ComputePlanSHA256, _ = computePlanDigest(*computePlan)
		out.Authority.VerificationContractSHA256 = computePlan.VerificationContractSHA256
	}
	if placement != nil {
		out.Authority.PlacementRequirementSHA256, _ = placementRequirementDigest(*placement)
	}
	if pricing != nil {
		out.Authority.PricingDecisionSHA256, _ = pricingDecisionDigest(*pricing)
		out.AuthorityStatus = "verified"
		reconciliation := &PricingReconciliation{
			Currency:           pricing.Currency,
			AcceptedBuyerPrice: pricing.BuyerPrice,
			MaximumBuyerPrice:  pricing.MaximumBuyerPrice,
			ModeledSupplierCost: roundEconomicUSD(
				pricing.PrimarySupplierCost.Amount + pricing.VerificationCost.Amount,
			),
			ModeledPlatformContribution: pricing.PlatformContribution.Amount,
			ModeledPaymentCost:          pricing.PaymentCost.Amount,
			CatalogueScheduleSHA256:     pricing.Catalogue.ScheduleSHA256,
			FXRevision:                  pricing.Catalogue.FXRevision,
		}
		if inv != nil {
			reconciliation.SettledBuyerPrice = inv.ActualUSD
			reconciliation.SettledSupplierCost = inv.SupplierPaidUSD
			reconciliation.SettledPlatformTake = inv.PlatformTakeUSD
			reconciliation.SettledPlatformGrossSpread = inv.PlatformGrossSpreadUSD
			reconciliation.SettledContribution = inv.Contribution
			reconciliation.ContributionSettlement = inv.ContributionSettlement
			reconciliation.SettledPaymentCost = inv.ProcessorFeeAllocatedUSD
			if settlement := inv.ContributionSettlement; settlement != nil {
				componentUSD := func(component ContributionSettlementComponent) (float64, bool) {
					if component.AmountNanos == nil {
						return 0, false
					}
					return float64(*component.AmountNanos) / float64(NanosPerMajorUnit), true
				}
				if amount, ok := componentUSD(settlement.BuyerNetAmount); ok {
					reconciliation.SettledBuyerPrice = amount
				}
				if amount, ok := componentUSD(settlement.SupplierEntitlements); ok {
					reconciliation.SettledSupplierCost = amount
				}
				if buyer, buyerOK := componentUSD(settlement.BuyerNetAmount); buyerOK {
					if supplier, supplierOK := componentUSD(settlement.SupplierEntitlements); supplierOK {
						gross := buyer - supplier
						reconciliation.SettledPlatformTake = gross
						reconciliation.SettledPlatformGrossSpread = gross
					}
				}
				if amount, ok := componentUSD(settlement.ProcessorFee); ok &&
					settlement.ProcessorFee.Status == contributionComponentSettled {
					reconciliation.SettledPaymentCost = &amount
				}
			}
		}
		out.Reconciliation = reconciliation
	}
	return out
}

package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Structural money/admission authority guard.
//
// Two independent views, then reconcile them. That is the whole design.
//
// View 1 — Declared money authority (declaredMoneyAuthoritySinks below):
// an explicit, reviewable catalogue of the sink operations the codebase
// claims own money or economic admission. A human can read it.
//
// View 2 — Observed reachability into money sinks (observeMoneyAuthority):
// derived from the AST every test run, never from filenames. A function is
// money/admission authority if its identity is a known sink or its body can
// reach one through the package call graph.
//
// Reconciliation:
//
//	observed sink not in declaration  → FAIL (money path nobody declared)
//	declared sink not observed        → FAIL (stale or overbroad declaration)
//
// The calibration/overhead consumption invariant roots at the union of
// (a) every function in the legacy 22-filename list and (b) every function
// in the structural observed-authority set. Dual-run until the filename
// list is proven removable; see TestNoCallPathFromMoneyOrAdmissionIntoCalibrationReads.
//
// Coverage note: measurement at land time found no live path from the
// previously unguarded money modules into calibration/overhead reads.
// The defect this closes is coverage, not a live breach.
//
// No golang.org/x/tools. Hand-written go/parser + go/ast only, matching
// authority_callgraph_test.go.

// moneyAuthoritySinkBareName reports whether a function's bare name (no
// receiver) is a money or economic-admission sink under the structural rules.
// Comments, tests, and read-only vocabulary are not consulted.
func moneyAuthoritySinkBareName(name string) bool {
	// 1. Ledger / authority writes
	if strings.HasPrefix(name, "insertLedger") ||
		name == "InsertLedgerEntries" ||
		name == "insertMoneyAuthorityAction" ||
		strings.HasPrefix(name, "insertJobDispute") ||
		name == "insertServiceLeaseLedgerEntryTx" ||
		name == "insertNewLedgerEntryTx" {
		return true
	}
	// 2. Frozen price
	if strings.HasPrefix(name, "new") && strings.HasSuffix(name, "PricingDecision") {
		return true
	}
	if name == "BuildEconomicPlan" || name == "BuildCataloguePriceSchedule" {
		return true
	}
	if strings.HasPrefix(name, "Validate") && strings.Contains(name, "PricingDecision") {
		return true
	}
	// 3. Balance mutate — prepaid
	if strings.Contains(name, "reservePrepaid") || strings.Contains(name, "ReservePrepaid") ||
		strings.Contains(name, "debitPrepaid") || strings.Contains(name, "DebitPrepaid") ||
		strings.Contains(name, "creditPrepaid") || strings.Contains(name, "CreditPrepaid") ||
		strings.Contains(name, "maybeDebitPrepaid") ||
		name == "BeginPrepaidRefund" || name == "CompletePrepaidRefund" ||
		name == "SeedPrepaidBalance" || name == "BeginPrepaidTopup" {
		return true
	}
	// 3b. risk-reserve
	if strings.Contains(name, "AccrueRiskReserve") || strings.Contains(name, "ReleaseRiskReserve") ||
		strings.Contains(name, "ConsumeRiskReserve") {
		return true
	}
	// 4. Cash
	if name == "chargePaymentIntent" || name == "chargeBuyer" || name == "RefundCharge" {
		return true
	}
	// 5. Settlement amount
	if strings.HasPrefix(name, "settleLoRA") || strings.HasPrefix(name, "settleObserved") ||
		strings.HasPrefix(name, "settleStorageEgress") || strings.HasPrefix(name, "SettleRealtime") ||
		strings.HasPrefix(name, "settleFinalServiceLease") || name == "SettleJobSLA" ||
		strings.HasPrefix(name, "clampSettlementToContribution") {
		return true
	}
	// 6. Payout state machine + subsidy
	switch name {
	case "ClaimPayout", "FinalizePayout", "DeferPayout", "MarkPayout",
		"MarkPayoutOutcomeUnknown", "ReleasePayoutTx",
		"AuthorizePayoutSubsidy", "CreateSubsidyFund",
		"reservePayoutFunding", "reserveBuyerTopupPayoutFunding",
		"reserveServiceLeasePayoutFunding":
		return true
	}
	// 7. Economic admission
	if name == "AuthorizeRealtimeContract" || name == "CreateServiceLease" ||
		name == "createJob" || name == "SubmitJobTx" ||
		strings.HasPrefix(name, "authorizePaymentOperation") {
		return true
	}
	// 8. Supplier liability / Merc contribution
	if name == "accrueSupplierLiability" ||
		name == "splitSupplierLiabilityMicros" ||
		name == "splitSupplierLiabilityMicrosForCurrency" {
		return true
	}
	// 9. Execution-envelope reserve / capture / release (prepaid-cap admission)
	if name == "CreateExecutionEnvelope" || name == "SpendExecutionEnvelope" ||
		name == "reserveEnvelopeSpendTx" || name == "captureEnvelopeSpendTx" ||
		name == "releaseEnvelopeSpendForContractTx" ||
		name == "ReleaseExpiredExecutionEnvelopes" ||
		name == "RecoverOrphanEnvelopeSpends" {
		return true
	}
	// 10. Stripe cash-event application (provider cash truth → ledger state)
	if name == "ApplyPaymentEventTx" ||
		name == "applyStripeChargeRefundState" ||
		name == "applyStripeDisputeState" {
		return true
	}
	return false
}

func moneyAuthoritySinkKey(key string) bool {
	name := key
	if i := strings.LastIndex(key, "."); i >= 0 {
		name = key[i+1:]
	}
	return moneyAuthoritySinkBareName(name)
}

// declaredMoneyAuthoritySinks is View 1.
// declaredMoneyAuthoritySinks is the human-readable View-1 core: the sink
// operations the system claims own money or economic admission. The full
// declaredMoneyAuthority list below is the reverse-reachable closure of these
// sinks under the package call graph (callers inherit authority).
var declaredMoneyAuthoritySinks = []string{
	// 1. Ledger / authority writes
	"Store.InsertLedgerEntries",                  // store_billing.go
	"insertJobDisputeBuyerRefundsTx",             // ledger_write.go
	"insertJobDisputeClawbacksTx",                // ledger_write.go
	"insertLedgerEntryIfAbsentByRefTx",           // ledger_write.go
	"insertLedgerEntryIfAbsentExactTx",           // ledger_write.go
	"insertLedgerEntryOnTaskConflictDoNothingTx", // ledger_write.go
	"insertLedgerEntryTx",                        // ledger_write.go
	"insertMoneyAuthorityAction",                 // money_authority_audit.go
	"insertNewLedgerEntryTx",                     // ledger_write.go
	"insertServiceLeaseLedgerEntryTx",            // service_leases.go
	// 2. Frozen price constructors and validators
	"BuildCataloguePriceSchedule",                         // pricing.go
	"BuildEconomicPlan",                                   // economic_plan.go
	"ValidateDistributedPricingDecisionSnapshot",          // pricing_decision.go
	"ValidateDistributedPricingDecisionSnapshotWithStore", // pricing_decision.go
	"ValidateExactReusePricingDecisionSnapshot",           // pricing_decision.go
	"ValidateRealtimePricingDecisionSnapshot",             // realtime_pricing_decision.go
	"ValidateRealtimeReusePricingDecisionSnapshot",        // realtime_reuse_pricing_decision.go
	"ValidateServiceLeasePricingDecisionSnapshot",         // service_lease_pricing.go
	"newDistributedPricingDecision",                       // pricing_decision.go
	"newExactReusePricingDecision",                        // pricing_decision.go
	"newRealtimePricingDecision",                          // realtime_pricing_decision.go
	"newRealtimeReusePricingDecision",                     // realtime_reuse_pricing_decision.go
	"newServiceLeasePricingDecision",                      // service_lease_pricing.go
	// 3. Balance mutate (prepaid + risk-reserve)
	"AccrueRiskReserveAtSettlementTx",            // risk_reserve_ledger.go
	"Store.AccrueRiskReserveAtSettlement",        // risk_reserve_ledger.go
	"Store.BeginPrepaidRefund",                   // store_prepaid.go
	"Store.BeginPrepaidTopup",                    // store_prepaid.go
	"Store.CompletePrepaidRefund",                // store_prepaid.go
	"Store.ConsumeRiskReserveOnRefund",           // risk_reserve_ledger.go
	"Store.CreditPrepaidTopup",                   // store_prepaid.go
	"Store.DebitPrepaidForTask",                  // store_prepaid.go
	"Store.ReleaseRiskReserveAfterDisputeWindow", // risk_reserve_ledger.go
	"Store.SeedPrepaidBalance",                   // store_prepaid.go
	"creditPrepaidBalanceTx",                     // store_prepaid.go
	"debitPrepaidForExecutionContractTx",         // store_prepaid.go
	"debitPrepaidForSLAPremiumTx",                // store_prepaid.go
	"debitPrepaidByRefTx",                        // store_prepaid.go: shared path for the two callers below
	"debitPrepaidForServiceLeaseTx",              // store_prepaid.go
	"debitPrepaidForTaskTx",                      // store_prepaid.go
	"maybeDebitPrepaidForRealtimeTx",             // store_prepaid.go
	"reservePrepaidForJobTx",                     // store_prepaid.go
	"reservePrepaidForServiceLeaseTx",            // store_prepaid.go
	// 4. Cash (charge / refund)
	"ManualExportPayout.RefundCharge", // payment.go
	"StripePayout.RefundCharge",       // payment.go
	"chargeBuyer",                     // billing.go
	"chargePaymentIntent",             // billing.go
	"stubPayout.RefundCharge",         // payment.go
	// 5. Settlement amount
	"SettleRealtimeReuseHitMoney",             // exact_reuse.go
	"SettleRealtimeReuseHitMoneyWithFX",       // exact_reuse.go
	"Store.SettleJobSLA",                      // collect.go
	"Store.SettleRealtimeExactReuse",          // realtime_store.go
	"clampSettlementToContributionFloorNanos", // observed_output_settlement.go
	"clampSettlementToContributionFloorUSD",   // observed_output_settlement.go
	"settleFinalServiceLeaseTx",               // service_leases.go
	"settleLoRAExact",                         // lora_settlement.go
	"settleLoRARun",                           // lora_settlement.go
	"settleObservedOutputTokens",              // observed_output_settlement.go
	"settleObservedOutputTokensWithSchedule",  // observed_output_settlement.go
	"settleStorageEgressFromBytes",            // modeled_cost_settlement.go
	// 6. Payout state machine + subsidy
	"Store.AuthorizePayoutSubsidy",     // store_payouts.go
	"Store.ClaimPayout",                // store_payouts.go
	"Store.CreateSubsidyFund",          // store_payouts.go
	"Store.DeferPayout",                // store_payouts.go
	"Store.FinalizePayout",             // store_payouts.go
	"Store.MarkPayout",                 // store_payouts.go
	"Store.MarkPayoutOutcomeUnknown",   // store_payouts.go
	"Store.ReleasePayoutTx",            // store_payouts.go
	"reserveBuyerTopupPayoutFunding",   // store_payouts.go
	"reservePayoutFunding",             // store_payouts.go
	"reserveServiceLeasePayoutFunding", // store_payouts.go
	// 7. Economic admission
	"Server.createJob",                // api.go
	"Store.AuthorizeRealtimeContract", // realtime_store.go
	"Store.CreateServiceLease",        // service_leases.go
	"Store.SubmitJobTx",               // store_jobs.go
	"authorizePaymentOperation",       // payment_authority.go
	"authorizePaymentOperationAt",     // payment_authority.go
	// 8. Supplier liability / Merc contribution
	"accrueSupplierLiability",                 // supplier_accrual.go
	"splitSupplierLiabilityMicros",            // payment.go
	"splitSupplierLiabilityMicrosForCurrency", // payment.go
	// 9. Execution-envelope reserve / capture / release
	"Store.CreateExecutionEnvelope",          // execution_envelope.go
	"Store.RecoverOrphanEnvelopeSpends",      // execution_envelope.go
	"Store.ReleaseExpiredExecutionEnvelopes", // execution_envelope.go
	"Store.SpendExecutionEnvelope",           // execution_envelope.go
	"captureEnvelopeSpendTx",                 // execution_envelope.go
	"releaseEnvelopeSpendForContractTx",      // execution_envelope.go
	"reserveEnvelopeSpendTx",                 // execution_envelope.go
	// 10. Stripe cash-event application
	"Store.ApplyPaymentEventTx",    // stripe_cash_events.go
	"applyStripeChargeRefundState", // stripe_cash_events.go
	"applyStripeDisputeState",      // stripe_cash_events.go
}

// Full declared authority set: reverse-reachable closure of the sink
// catalogue under the package call graph at land time (392 symbols).
// This is the machine-checked half of View 1; the sink catalogue above is
// the human-readable core. Edit deliberately when observation changes.
var declaredMoneyAuthority = []string{
	// Spot quoting is money authority: PriceSpotQuotePlan sets the MAXIMUM
	// AUTHORIZED CHARGE a buyer accepts, and ProduceSpotQuote is its entry
	// point. Declared deliberately rather than allowlisted away — a producer
	// that decides the ceiling belongs in this census by definition.
	"PriceSpotQuotePlan",
	"ProduceSpotQuote",
	"AccrueRiskReserveAtSettlementTx",                     // risk_reserve_ledger.go
	"BuildCataloguePriceSchedule",                         // pricing.go
	"BuildEconomicPlan",                                   // economic_plan.go
	"DefaultBoundIdentity",                                // receipt_identity.go
	"EstimateAcrossRegisteredRuntimes",                    // runtime_adapter.go
	"ManualExportPayout.RefundCharge",                     // payment.go
	"PublishedCatalogueResults",                           // pricing.go
	"ResolveRepoSourceCommit",                             // receipt_identity.go
	"ResolveWorkerRuntimeProfile",                         // runtime_profile_admission.go
	"Server.Routes",                                       // api.go
	"Server.buildQuoteWithSchedule",                       // quote.go
	"Server.chargeForJob",                                 // billing.go
	"Server.createJob",                                    // api.go
	"Server.estimateJobSettlement",                        // api.go
	"Server.finalizeJobIfDone",                            // api.go
	"Server.handleAdminCreateSubsidyFund",                 // api.go
	"Server.handleAdminDirectedJob",                       // runtime_cell_cost.go
	"Server.handleAdminRefundPrepaid",                     // prepaid.go
	"Server.handleAdminRefundRealtimeContract",            // api.go
	"Server.handleAdminReleasePayout",                     // api.go
	"Server.handleAdminResolveDispute",                    // api.go
	"Server.handleAdminSelectorActivation",                // runtime_cell_cost.go
	"Server.handleAdminSelectorLiabilityRegret",           // runtime_cell_cost.go
	"Server.handleAdminSelectorPromotion",                 // runtime_cell_cost.go
	"Server.handleAdminSelectorRollback",                  // runtime_cell_cost.go
	"Server.handleAdminSubsidizePayout",                   // api.go
	"Server.handleBillingSetup",                           // billing.go
	"Server.handleBillingTopup",                           // prepaid.go
	"Server.handleCancelServiceLease",                     // service_lease_api.go
	"Server.handleChatCompletions",                        // realtime.go
	"Server.handleConnectWebhook",                         // suppliers.go
	"Server.handleCreateExecutionEnvelope",                // execution_envelope_api.go
	"Server.handleCreateJob",                              // api.go
	"Server.handleCreateServiceLease",                     // service_lease_api.go
	"Server.handleJobInvoice",                             // api.go
	"Server.handleJobReceipt",                             // api.go
	"Server.handleModels",                                 // api.go
	"Server.handlePriceBoardData",                         // api.go
	"Server.handlePriceEstimate",                          // api.go
	"Server.handleProjectCompile",                         // project_compile_api.go
	"Server.handleQuote",                                  // quote.go
	"Server.handleStripeWebhook",                          // billing.go
	"Server.handleSupplierOnboard",                        // suppliers.go
	"Server.handleSupplierStatus",                         // suppliers.go
	"Server.handleWorkerCommit",                           // api.go
	"Server.handleWorkerConnectStatus",                    // connect.go
	"Server.handleWorkerHeartbeat",                        // api.go
	"Server.handleWorkerPoll",                             // api.go
	"Server.handleWorkerRegister",                         // api.go
	"Server.handleUIEarn",                                 // ui_facade.go
	"Server.handleWorkerViability",                        // api.go
	"Server.loadCurrentPublicCatalogue",                   // api.go
	"Server.normalizeWorkloadRequest",                     // job_submit_validate.go
	"Server.refundPrepaidRemainder",                       // prepaid.go
	"Server.tryRealtimeCoalescedDelivery",                 // realtime.go
	"Server.tryRealtimeExactReuse",                        // realtime.go
	"SettleRealtimeReuseHitMoney",                         // exact_reuse.go
	"SettleRealtimeReuseHitMoneyWithFX",                   // exact_reuse.go
	"Store.AccrueRiskReserveAtSettlement",                 // risk_reserve_ledger.go
	"Store.ApplyActivationPolicy",                         // activation_policy.go
	"Store.ApplyRepricing",                                // pricing.go
	"Store.activationForNewAdmission",                     // activation_policy.go
	"Store.ApplyPaymentEventTx",                           // stripe_cash_events.go
	"Store.AuthorizePayoutSubsidy",                        // store_payouts.go
	"Store.AuthorizeRealtimeContract",                     // realtime_store.go
	"Store.BeginPrepaidRefund",                            // store_prepaid.go
	"Store.BeginPrepaidTopup",                             // store_prepaid.go
	"Store.CancelServiceLease",                            // service_leases.go
	"Store.CaptureExecutionEnvelopeSpendForTest",          // execution_envelope.go
	"Store.ClaimPayout",                                   // store_payouts.go
	"Store.ClawbackTaskCredit",                            // store_tasks.go
	"Store.CompletePrepaidRefund",                         // store_prepaid.go
	"Store.ContributionSettlementForJob",                  // contribution_settlement.go
	"Store.ConsumeRiskReserveOnRefund",                    // risk_reserve_ledger.go
	"Store.CreateExecutionEnvelope",                       // execution_envelope.go
	"Store.CreateServiceLease",                            // service_leases.go
	"Store.CreateSubsidyFund",                             // store_payouts.go
	"Store.CreditPrepaidTopup",                            // store_prepaid.go
	"Store.CurrentActivationPolicy",                       // activation_policy.go
	"Store.DebitPrepaidForTask",                           // store_prepaid.go
	"Store.DeferPayout",                                   // store_payouts.go
	"Store.EvaluateCellPromotion",                         // runtime_cell_promotion.go
	"Store.FailoverPendingServiceLeases",                  // service_leases.go
	"Store.FinalizeExpiredServiceLeases",                  // service_leases.go
	"Store.FinalizeJobTx",                                 // store_jobs.go
	"Store.FinalizePayout",                                // store_payouts.go
	"Store.FinalizeRealtimeFailure",                       // realtime_store.go
	"Store.FinalizeRealtimeSuccess",                       // realtime_store.go
	"Store.FinalizeTaskVerification",                      // verification_state.go
	"Store.GetBindableQuote",                              // quote.go
	"Store.InsertLedgerEntries",                           // store_billing.go
	"Store.InsertQuote",                                   // quote.go
	"Store.InsertStripeFee",                               // collect.go
	"Store.JobComputePlan",                                // store_jobs.go
	"Store.JobInvoice",                                    // store_billing.go
	"Store.JobPricingDecision",                            // store_jobs.go
	"Store.JobPrimaryChunkCount",                          // store_jobs.go
	"Store.JobTaskReceipts",                               // store_tasks.go
	"Store.LoadCataloguePriceAuthority",                   // pricing_decision.go
	"Store.MarkPayout",                                    // store_payouts.go
	"Store.MarkPayoutOutcomeUnknown",                      // store_payouts.go
	"Store.MeasuredSupplierLiabilityProxies",              // runtime_cell_cost.go
	"Store.MeasuredSupplierLiabilityProxiesByHardware",    // runtime_cell_cost.go
	"Store.Migrate",                                       // store.go
	"Store.adoptMigratedSchema",                           // store.go
	"Store.migrate",                                       // store.go
	"Store.ReconcileBuyerChargeOperation",                 // buyer_charge_operations.go
	"Store.RecoverOrphanEnvelopeSpends",                   // execution_envelope.go
	"Store.RecoverStaleRealtimeContracts",                 // realtime_store.go
	"Store.RefreshActivationPolicy",                       // activation_policy.go
	"Store.RefundRealtimeContract",                        // realtime_store.go
	"Store.ReleaseEligibleRiskReserves",                   // risk_reserve_ledger.go
	"Store.ReleaseExpiredExecutionEnvelopes",              // execution_envelope.go
	"Store.ReleasePayoutTx",                               // store_payouts.go
	"Store.ReleaseRiskReserveAfterDisputeWindow",          // risk_reserve_ledger.go
	"Store.ResolveDisputeTx",                              // store_disputes.go
	"Store.RollbackActivationPolicy",                      // activation_policy.go
	"Store.SeedPrepaidBalance",                            // store_prepaid.go
	"Store.SelectorLiabilityRegretForScope",               // runtime_cell_cost.go
	"Store.SetDisputeStatus",                              // store_disputes.go
	"Store.SettleJobSLA",                                  // collect.go
	"Store.SettlePendingRealtimeIntents",                  // realtime_store.go
	"Store.SettleRealtimeExactReuse",                      // realtime_store.go
	"Store.SpendExecutionEnvelope",                        // execution_envelope.go
	"Store.SubmitExactReuseBatchJob",                      // exact_reuse_batch.go
	"Store.SubmitJobTx",                                   // store_jobs.go
	"Store.TerminateServiceLeaseNoReplacement",            // service_leases.go
	"Store.TrueNetContributionForAccount",                 // store_billing.go
	"Store.TrueNetContributionForWorkloadClass",           // store_billing.go
	"Store.UpsertWorker",                                  // store_workers.go
	"Store.ValidateAdvertisedRuntimeCatalog",              // runtime_matrix.go
	"Store.VerifyJobTx",                                   // verification_apply.go
	"Store.applyVerificationDecision",                     // verification_apply.go
	"Store.attachObservedOutputInvoiceEvidence",           // store_billing.go
	"Store.completeJobEconomics",                          // store_jobs.go
	"Store.finalizeExpiredServiceLease",                   // service_leases.go
	"Store.observedOutputSettlementForTask",               // verification_processor.go
	"Store.reconcilePrepaidTopup",                         // prepaid.go
	"Store.requirePromotionGateVerdict",                   // activation_policy.go
	"Store.resolveDispute",                                // store_disputes.go
	"Store.settledSupplierLiabilitiesForTasks",            // runtime_cell_cost.go
	"Store.writeActivationPolicy",                         // activation_policy.go
	"StripePayout.RefundCharge",                           // payment.go
	"StripePayout.ReverseTransfer",                        // payment.go
	"StripePayout.Send",                                   // payment.go
	"StripePayout.accountCountry",                         // stripe_settlement.go
	"StripePayout.settlementCurrencies",                   // stripe_settlement.go
	"StripePayout.verifySettlementCurrency",               // stripe_settlement.go
	"SupplierAdmissionViability",                          // runtime_cell_performance.go
	"ValidateComputePlanEconomicSnapshot",                 // compute_plan.go
	"ValidateDistributedPricingDecisionSnapshot",          // pricing_decision.go
	"ValidateDistributedPricingDecisionSnapshotWithStore", // pricing_decision.go
	"ValidateEconomicPlanSnapshot",                        // economic_plan.go
	"ValidateExactReusePricingDecisionSnapshot",           // pricing_decision.go
	"ValidateRealtimePricingDecisionSnapshot",             // realtime_pricing_decision.go
	"ValidateRealtimeReusePricingDecisionSnapshot",        // realtime_reuse_pricing_decision.go
	"ValidateReceiptIdentity",                             // receipt_identity.go
	"ValidateServiceLeasePricingDecisionSnapshot",         // service_lease_pricing.go
	"ValidateWorkloadDecisionSnapshot",                    // workload_classification.go
	"VerificationProcessor.Drain",                         // verification_processor.go
	"VerificationProcessor.ProcessAttempt",                // verification_processor.go
	"VerificationProcessor.createPlan",                    // verification_processor.go
	"VerificationProcessor.processDrainWork",              // verification_processor.go
	"VerificationProcessor.processLeased",                 // verification_processor.go
	"VerificationProcessor.processLeasedOnce",             // verification_processor.go
	"VerificationProcessor.taskPayoutEntriesAt",           // verification_processor.go
	"Verifier.PlanTaskResult",                             // verification_plan.go
	"Verifier.resolveTiebreak",                            // verification.go
	"Verifier.verifyTaskResult",                           // verification.go
	"Workers.Run",                                         // workers.go
	"Workers.chargeBatch",                                 // collect.go
	"Workers.collectCharges",                              // collect.go
	"Workers.executeReversal",                             // workers.go
	"Workers.finalizeJobs",                                // workers.go
	"Workers.processReversals",                            // workers.go
	"Workers.reconcileLedger",                             // reconcile.go
	"Workers.recoverOrphanEnvelopeSpends",                 // workers.go
	"Workers.recoverRealtimeContracts",                    // workers.go
	"Workers.recoverServiceLeases",                        // workers.go
	"Workers.recoverVerification",                         // workers.go
	"Workers.releaseExpiredExecutionEnvelopes",            // workers.go
	"Workers.releaseMaturedRiskReserves",                  // workers.go
	"Workers.releasePayouts",                              // workers.go
	"Workers.resolveDisputes",                             // workers.go
	"Workers.retryFailedSingle",                           // collect.go
	"Workers.settleRealtimeSettlementIntents",             // workers.go
	"Workers.settleSLAOutcomes",                           // collect.go
	"WriteBoundEvidenceJSON",                              // receipt_identity.go
	"accrueSupplierLiability",                             // supplier_accrual.go
	"activationAdmissionRevision",                         // activation_policy.go
	"activationRoutableProjection",                        // node_capability.go — dual-write of activation policy into wac.routable (not a node Capability fact)
	"activationSnapshotFrom",                              // activation_policy.go
	"admissionUnitsPerSec",                                // runtime_cell_performance.go
	"advertisedProjectRuntimeContracts",                   // project_contracts.go
	"advertisedRuntimeCapabilities",                       // activation_policy.go
	"advertisedRuntimeCell",                               // runtime_matrix.go
	"advertisedRuntimeJobModel",                           // runtime_matrix.go
	"advertisedRuntimeModel",                              // runtime_matrix.go
	"advertisedRuntimeModelKinds",                         // runtime_matrix.go
	"applyDisputeBuyerRefundFundingTx",                    // store_disputes.go
	"applyStripeChargeRefundState",                        // stripe_cash_events.go
	"applyStripeDisputeState",                             // stripe_cash_events.go
	"authorityCell.Routable",                              // runtime_authority.go
	"authorizePaymentOperation",                           // payment_authority.go
	"authorizePaymentOperationAt",                         // payment_authority.go
	"buildProjectIR",                                      // project_compiler.go
	"buildCatalogueResultPhysicalAuthority",               // pricing_citation_authority.go
	"buildWorkloadDecision",                               // workload_classification.go
	"buildWorkloadDecisionDirected",                       // workload_classification.go
	"buildWorkloadDecisionForSubmit",                      // workload_classification.go
	"buildWorkloadDecisionFromBinding",                    // workload_classification.go
	"buildWorkloadDecisionFromBindingDirected",            // workload_classification.go
	"captureEnvelopeSpendTx",                              // execution_envelope.go
	"canonicalFrozenMercSourceCommit",                     // runtime_cell_performance.go
	"cellAuthorityBindable",                               // cell_authority_binding.go
	"citedReceiptPayload",                                 // pricing_citation_authority.go
	"chargeBuyer",                                         // billing.go
	"chargeOrDeferJob",                                    // billing.go
	"chargePaymentIntent",                                 // billing.go
	"chargePaymentIntentWithKeys",                         // billing.go
	"clampSettlementToContributionFloorNanos",             // observed_output_settlement.go
	"clampSettlementToContributionFloorUSD",               // observed_output_settlement.go
	"clawbackTaskCreditTx",                                // verification_apply.go
	"cmdLaunchInputs",                                     // release_launch.go
	"cmdProve",                                            // prove.go
	"cmdReleaseEvidence",                                  // release_launch.go
	"cmdSourceID",                                         // prove.go
	"cmdVerify",                                           // prove.go
	"compileLaunchPlan",                                   // release_launch.go
	"compileProject",                                      // project_compiler.go
	"consumeLegacyRiskReserveForCauseTx",                  // risk_reserve_ledger.go
	"consumeRiskReserveForCauseTx",                        // risk_reserve_ledger.go
	"consumeRiskReserveForDisputeRefundTx",                // risk_reserve_ledger.go
	"consumeRiskReserveForSLARefundTx",                    // risk_reserve_ledger.go
	"creditPrepaidBalanceTx",                              // store_prepaid.go
	"currentRuntimeCellBenchmarkIdentity",                 // cell_authority_binding.go
	"currentActivation",                                   // activation_policy.go
	"currentActivationEntries",                            // activation_policy.go
	"debitPrepaidForExecutionContractTx",                  // store_prepaid.go
	"debitPrepaidForSLAPremiumTx",                         // store_prepaid.go
	"debitPrepaidByRefTx",                                 // store_prepaid.go: shared path for the two callers below
	"debitPrepaidForServiceLeaseTx",                       // store_prepaid.go
	"debitPrepaidForTaskTx",                               // store_prepaid.go
	"directedRuntimeCapabilities",                         // activation_policy.go
	"directedRuntimeModel",                                // runtime_matrix.go
	"dispatchBuyer",                                       // buyer.go
	"dispatchDev",                                         // dev_checkpoint.go
	"dispatchLaunchRelease",                               // release_launch.go
	"dispatchProject",                                     // dev_project_compile.go
	"dispatchRelease",                                     // stripe_simulator.go
	"distributedPricingDecisionAtRate",                    // pricing_decision.go
	"documentActivation",                                  // activation_policy.go
	"documentActivationEntries",                           // activation_policy.go
	"documentAdvertisedCells",                             // runtime_authority.go
	"documentRoutableCells",                               // runtime_authority.go
	"economicPlanDigest",                                  // pricing_decision.go
	"enrollableProfileForEngine",                          // runtime_profile_admission.go
	"ensureConnectAccount",                                // connect.go
	"ensurePricingCitationBindable",                       // pricing_citation_authority.go
	"ensureStripeCustomer",                                // billing.go
	"finalizeRealtimeFailure",                             // realtime.go
	"freezeRuntimeCellPerformance",                        // runtime_cell_performance.go
	"generatedRuntimeModelRef",                            // runtime_matrix.go
	"genericProfileAdapter.Estimate",                      // runtime_adapter.go
	"genericProfileAdapter.Supports",                      // runtime_adapter.go
	"gitBytes",                                            // evidence.go
	"gitObjectExists",                                     // cell_authority_binding.go
	"governedAdmissionUnitRates",                          // runtime_cell_performance.go
	"governPublishedPrice",                                // pricing_governance.go
	"governedProfileIdentity",                             // runtime_profile_admission.go
	"insertActivationPolicy",                              // activation_policy.go
	"insertActivationPolicyLocked",                        // activation_policy.go
	"insertJobDisputeBuyerRefundsTx",                      // ledger_write.go
	"insertJobDisputeClawbacksTx",                         // ledger_write.go
	"insertLedgerEntryIfAbsentByRefTx",                    // ledger_write.go
	"insertLedgerEntryIfAbsentExactTx",                    // ledger_write.go
	"insertLedgerEntryOnTaskConflictDoNothingTx",          // ledger_write.go
	"insertLedgerEntryTx",                                 // ledger_write.go
	"insertMoneyAuthorityAction",                          // money_authority_audit.go
	"insertNewLedgerEntryTx",                              // ledger_write.go
	"insertServiceLeaseLedgerEntryTx",                     // service_leases.go
	"launchConfigValuesForRoot",                           // release_launch.go
	"launchSource",                                        // release_launch.go
	"lfsObjectPath",                                       // evidence.go
	"loadActivationAtStartup",                             // activation_policy.go
	"loadContributionJobFacts",                            // contribution_settlement.go
	"loadContributionObservedOutputRebate",                // contribution_settlement.go
	"loadLFSIndex",                                        // evidence.go
	"loadObservedOutputSettlement",                        // observed_output_settlement.go
	"main",                                                // main.go
	"maybeDebitPrepaidForRealtimeTx",                      // store_prepaid.go
	"measurableResidencyModel",                            // runtime_matrix.go
	"newDistributedPricingDecision",                       // pricing_decision.go
	"newExactReusePricingDecision",                        // pricing_decision.go
	"newRealtimePricingDecision",                          // realtime_pricing_decision.go
	"newRealtimeReusePricingDecision",                     // realtime_reuse_pricing_decision.go
	"newRuntimeActivation",                                // activation_policy.go
	"newServiceLeasePricingDecision",                      // service_lease_pricing.go
	"normalizeAdvertisedRuntimeModelRef",                  // runtime_matrix.go
	"normalizeAndValidateJobSubmit",                       // job_submit_validate.go
	"onboardingLink",                                      // connect.go
	"performDevCheckpoint",                                // dev_checkpoint.go
	"persistMinorUnitSettlement",                          // store.go
	"placementRequirementFor",                             // quote.go
	"planShadowSelection",                                 // runtime_shadow_selection.go
	"pricingRepoRootCandidates",                           // pricing_citation_authority.go
	"pricingPowerAuthoritySnapshot",                       // pricing_citation_authority.go
	"measuredPowerAuthoritySnapshot",                      // pricing_citation_authority.go
	"pricingThroughputAuthoritySnapshot",                  // pricing_citation_authority.go
	"projectActivationPolicyIntoRegistry",                 // activation_policy.go
	"projectWorkerRuntimeCapabilities",                    // runtime_matrix.go
	"quoteCompiledProject",                                // project_quote.go
	"quoteDependentProjectStep",                           // project_dependency.go
	"quoteInitialProjectRoots",                            // project_dependency.go
	"rankAndFreezeAdmissionCell",                          // workload_classification.go
	"readActivationSnapshot",                              // activation_policy.go
	"recordStripeFee",                                     // billing.go
	"releaseEnvelopeSpendForContractTx",                   // execution_envelope.go
	"releaseLegacyRiskReserveTx",                          // risk_reserve_ledger.go
	"releaseRiskReserveTx",                                // risk_reserve_ledger.go
	"repoRootOrCwd",                                       // prove.go
	"reserveBuyerTopupPayoutFunding",                      // store_payouts.go
	"reserveEnvelopeSpendTx",                              // execution_envelope.go
	"reservePayoutFunding",                                // store_payouts.go
	"reservePrepaidForJobTx",                              // store_prepaid.go
	"reservePrepaidForServiceLeaseTx",                     // store_prepaid.go
	"reserveServiceLeasePayoutFunding",                    // store_payouts.go
	"resolveCellPerformance",                              // runtime_cell_performance.go
	"resolveCitedEvidencePath",                            // pricing_citation_authority.go
	"resolveDeclaredProjectContracts",                     // project_contracts.go
	"resolveDisputeInTx",                                  // store_disputes.go
	"resolveLFSPayload",                                   // evidence.go
	"buildBatchRuntimeDecision",                           // runtime_decision.go
	"restorePrepaidByDebitRefTx",                          // store_prepaid.go
	"restorePrepaidForDisputeJobTasksTx",                  // store_prepaid.go
	"restorePrepaidForExecutionContractRefundTx",          // store_prepaid.go
	"restorePrepaidForSLAPremiumRefundTx",                 // store_prepaid.go
	"restorePrepaidForTaskDebitTx",                        // store_prepaid.go
	"revalidateCataloguePriceScheduleCurrent",             // pricing_citation_authority.go
	"revalidateCataloguePriceSchedulePhysicalCurrent",     // pricing_citation_authority.go
	"revalidateCatalogueResultPhysicalCurrent",            // pricing_citation_authority.go
	"routableCellPerformance",                             // runtime_cell_performance.go
	"routableProfileForEngine",                            // runtime_profile_admission.go
	"runDevAuthority",                                     // dev_authority.go
	"runDevCheckpoint",                                    // dev_checkpoint.go
	"runProjectCompile",                                   // dev_project_compile.go
	"runProjectQuote",                                     // dev_project_compile.go
	"runProjectQuoteRoots",                                // dev_project_compile.go
	"runProjectQuoteStep",                                 // dev_project_compile.go
	"runProjectSubmit",                                    // dev_project_compile.go
	"runProjectSubmitRoots",                               // dev_project_compile.go
	"runProjectSubmitStep",                                // dev_project_compile.go
	"runWorkerLeader",                                     // worker_leader.go
	"runtimeActivation.cellRoutable",                      // activation_policy.go
	"runtimeCapabilitiesForBindingDirected",               // workload_classification.go
	"runtimeCapabilityForBindingDirected",                 // workload_classification.go
	"runtimeProfileByID",                                  // runtime_profile_admission.go
	"selectAdmissionCandidates",                           // workload_classification.go
	"settleFinalServiceLeaseTx",                           // service_leases.go
	"settleLoRAExact",                                     // lora_settlement.go
	"settleLoRARun",                                       // lora_settlement.go
	"settleObservedOutputTokens",                          // observed_output_settlement.go
	"settleObservedOutputTokensWithSchedule",              // observed_output_settlement.go
	"settleSLAOutcome",                                    // collect.go
	"settleStorageEgressFromBytes",                        // modeled_cost_settlement.go
	"setupIntent",                                         // billing.go
	"sourceFingerprint",                                   // evidence.go
	"sustainedWattsForPublication",                        // pricing.go
	"splitSupplierLiabilityMicros",                        // payment.go
	"splitSupplierLiabilityMicrosForCurrency",             // payment.go
	"stripeCreateRefund",                                  // prepaid.go
	"stripeForm",                                          // billing.go
	"stripeGet",                                           // billing.go
	"fetchStripeTransferSnapshot",                         // reconcile.go
	"stripeTransferredUSD",                                // reconcile.go
	"storedRoutableEntryHasCurrentGlobalAuthority",        // activation_policy.go
	"stubPayout.RefundCharge",                             // payment.go
	"submitCompiledProject",                               // project_submit.go
	"submitDependentProjectStep",                          // project_dependency.go
	"submitInitialProjectRoots",                           // project_dependency.go
	"supplierAdmissionCeilingUSDHr",                       // pricing_decision.go
	"syncActivationPolicy",                                // activation_policy.go
	"syncRuntimeProfiles",                                 // runtime_profile_sync.go
	"validateAdvertisedRuntimeCatalogRows",                // runtime_matrix.go
	"validateAdvertisedRuntimeJobModel",                   // runtime_matrix.go
	"validateAllRepricingBenchmarkCitations",              // pricing_citation_authority.go
	"validateCurrentCataloguePriceAuthorityFrom",          // pricing_decision.go
	"validateBoundCataloguePriceAuthorityFrom",            // pricing_decision.go
	"validateCurrentPlacementPerformanceAuthority",        // runtime_cell_performance.go
	"validateCurrentPlacementPerformanceAuthorityAt",      // runtime_cell_performance.go
	"validateCurrentPlacementRequirement",                 // quote.go
	"validateCurrentRuntimeCellPerformanceAuthority",      // runtime_cell_performance.go
	"validateCurrentRuntimeCellPerformanceAuthorityAt",    // runtime_cell_performance.go
	"validateDependentQuoteForSubmit",                     // project_dependency.go
	"validateGitObject",                                   // receipt_identity.go
	"validateHeartbeatModelIDs",                           // runtime_matrix.go
	"validateHeartbeatResidentModels",                     // runtime_matrix.go
	"validateHeartbeatRuntimeModels",                      // runtime_matrix.go
	"validateInitialProjectQuoteForSubmit",                // project_dependency.go
	"validateMercSourceCommit",                            // cell_authority_binding.go
	"validateMeasuredProxyCurrentExecutionIdentity",       // runtime_cell_promotion.go
	"validatePricingPowerCitation",                        // pricing_citation_authority.go
	"validatePricingThroughputRuntimeCellIdentity",        // pricing_citation_authority.go
	"validateProjectQuoteForSubmit",                       // project_submit.go
	"validateRepricingBenchmarkCitation",                  // pricing_citation_authority.go
	"validateVerificationSettlementTx",                    // verification_apply.go
	"validateWorkerRuntimeProjection",                     // runtime_matrix.go
	"verifyLFSPayload",                                    // evidence.go
	"verifyPromotionScopeAgainstAuthority",                // runtime_cell_promotion.go
	"verifyProofLedger",                                   // prove.go
}

// moneyAuthorityObservation is View 2: structural observation for one package parse.
type moneyAuthorityObservation struct {
	// Sinks are functions whose identity matches moneyAuthoritySinkKey.
	Sinks map[string]bool
	// Authority is every function that is a sink or can reach a sink (reverse BFS).
	Authority map[string]bool
	// File of each authority key (from the call graph).
	File map[string]string
}

// observeMoneyAuthority derives View 2 from the call graph. No filename list
// is consulted. Test files are already excluded by buildCallGraph.
func observeMoneyAuthority(g *callGraph) moneyAuthorityObservation {
	obs := moneyAuthorityObservation{
		Sinks:     map[string]bool{},
		Authority: map[string]bool{},
		File:      map[string]string{},
	}
	// Reverse edges: callee → callers.
	rev := map[string]map[string]bool{}
	for from, tos := range g.edges {
		for to := range tos {
			if rev[to] == nil {
				rev[to] = map[string]bool{}
			}
			rev[to][from] = true
		}
	}
	queue := make([]string, 0, 64)
	for key, file := range g.file {
		if !moneyAuthoritySinkKey(key) {
			continue
		}
		obs.Sinks[key] = true
		obs.Authority[key] = true
		obs.File[key] = file
		queue = append(queue, key)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for caller := range rev[cur] {
			if obs.Authority[caller] {
				continue
			}
			obs.Authority[caller] = true
			obs.File[caller] = g.file[caller]
			queue = append(queue, caller)
		}
	}
	return obs
}

func sortedMoneyAuthorityKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func setFromList(list []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range list {
		m[s] = true
	}
	return m
}

// TestMoneyAuthorityDeclarationMatchesObservation reconciles View 1 and View 2.
// Both directions fail with the symbol named.
//
// Two layers:
//  1. Sink catalogue (small, human-readable) must match observed sinks.
//  2. Full authority set (reverse-reachable closure) must match observation.
//
// A new helper that reaches a ledger write fails (2) even when it is not itself
// a sink; a stale declared symbol fails (1) or (2) when it no longer reaches money.
func TestMoneyAuthorityDeclarationMatchesObservation(t *testing.T) {
	graph := buildCallGraph(t)
	obs := observeMoneyAuthority(graph)

	declaredSinks := setFromList(declaredMoneyAuthoritySinks)
	declaredAuth := setFromList(declaredMoneyAuthority)

	if len(declaredSinks) != len(declaredMoneyAuthoritySinks) {
		t.Fatalf("declaredMoneyAuthoritySinks has duplicate entries (%d unique of %d)",
			len(declaredSinks), len(declaredMoneyAuthoritySinks))
	}
	if len(declaredAuth) != len(declaredMoneyAuthority) {
		t.Fatalf("declaredMoneyAuthority has duplicate entries (%d unique of %d)",
			len(declaredAuth), len(declaredMoneyAuthority))
	}

	// Every declared sink must appear in the full declaration.
	for _, key := range declaredMoneyAuthoritySinks {
		if !declaredAuth[key] {
			t.Errorf("declared sink %s missing from declaredMoneyAuthority", key)
		}
	}

	// --- sinks ---
	var undeclaredSinks []string
	for _, key := range sortedMoneyAuthorityKeys(obs.Sinks) {
		if !declaredSinks[key] {
			undeclaredSinks = append(undeclaredSinks, fmt.Sprintf("%s (%s)", key, graph.file[key]))
		}
	}
	if len(undeclaredSinks) > 0 {
		t.Errorf("observed but undeclared money/admission sinks (%d) — a money path nobody declared:\n  %s",
			len(undeclaredSinks), strings.Join(undeclaredSinks, "\n  "))
	}

	var unobservedSinks []string
	for _, key := range sortedMoneyAuthorityKeys(declaredSinks) {
		if !obs.Sinks[key] {
			if _, exists := graph.file[key]; !exists {
				unobservedSinks = append(unobservedSinks, key+" (not a declared function in package)")
			} else {
				unobservedSinks = append(unobservedSinks, fmt.Sprintf("%s (%s) (declared but not a money sink under structural rules)", key, graph.file[key]))
			}
		}
	}
	if len(unobservedSinks) > 0 {
		t.Errorf("declared but unobserved money/admission sinks (%d) — stale or overbroad authority:\n  %s",
			len(unobservedSinks), strings.Join(unobservedSinks, "\n  "))
	}

	// --- full authority (reachability closure) ---
	var undeclaredAuth []string
	for _, key := range sortedMoneyAuthorityKeys(obs.Authority) {
		if !declaredAuth[key] {
			undeclaredAuth = append(undeclaredAuth, fmt.Sprintf("%s (%s)", key, graph.file[key]))
		}
	}
	if len(undeclaredAuth) > 0 {
		t.Errorf("observed but undeclared money/admission authority (%d) — a money path nobody declared:\n  %s",
			len(undeclaredAuth), strings.Join(undeclaredAuth, "\n  "))
	}

	var unobservedAuth []string
	for _, key := range sortedMoneyAuthorityKeys(declaredAuth) {
		if !obs.Authority[key] {
			if _, exists := graph.file[key]; !exists {
				unobservedAuth = append(unobservedAuth, key+" (not a declared function in package)")
			} else {
				unobservedAuth = append(unobservedAuth, fmt.Sprintf("%s (%s) (declared but does not reach a money sink)", key, graph.file[key]))
			}
		}
	}
	if len(unobservedAuth) > 0 {
		t.Errorf("declared but unobserved money/admission authority (%d) — stale or overbroad authority:\n  %s",
			len(unobservedAuth), strings.Join(unobservedAuth, "\n  "))
	}

	files := map[string]bool{}
	for key := range obs.Authority {
		files[graph.file[key]] = true
	}
	t.Logf("money authority census: %d sinks, %d authority functions across %d files (structural; no filename list)",
		len(obs.Sinks), len(obs.Authority), len(files))
}

// TestMoneyAuthorityIgnoresReadOnlyAndComments pins two non-firing cases so a
// future tightening cannot silently start treating reports as authority.
func TestMoneyAuthorityIgnoresReadOnlyAndComments(t *testing.T) {
	graph := buildCallGraph(t)
	obs := observeMoneyAuthority(graph)

	// Read-only balance getters must not be sinks. They may still appear in the
	// authority set if a money writer happens to call them — that is reachability
	// noise, not sink misclassification. Sink membership is the claim under test.
	readOnly := []string{
		"Store.BuyerPrepaidBalanceMicros",
		"Store.BuyerPrepaidAvailableMicros",
		"Store.RiskReserveBalanceMicros",
		"Store.SupplierAccrual",
	}
	for _, key := range readOnly {
		if obs.Sinks[key] {
			t.Errorf("read-only getter %s classified as a money sink", key)
		}
		if _, exists := graph.file[key]; !exists {
			t.Logf("note: %s not present in package; skip existence check", key)
		}
	}

	// A function whose only money vocabulary is in a comment is not in the graph
	// as a sink: comments are not parsed into edges (parser mode 0). Spot-check
	// that we never invent keys from comments.
	if obs.Sinks["thisIsNotARealFunctionInsertLedger"] {
		t.Error("comment-only or invented sink key appeared in observation")
	}
}

// structuralMoneyAuthorityRoots returns the sorted function keys that the
// structural view treats as money/admission roots (the full authority set).
func structuralMoneyAuthorityRoots(t *testing.T, g *callGraph) []string {
	t.Helper()
	obs := observeMoneyAuthority(g)
	return sortedMoneyAuthorityKeys(obs.Authority)
}

// filenameMoneyAuthorityRoots returns every function declared in the legacy
// 22-filename list. Dual-run union partner for structural roots.
func filenameMoneyAuthorityRoots(t *testing.T, g *callGraph) []string {
	t.Helper()
	guarded := map[string]bool{}
	for _, name := range moneyAndAdmissionAuthorityFiles {
		guarded[name] = true
	}
	roots := make([]string, 0, 64)
	for key, file := range g.file {
		if guarded[file] {
			roots = append(roots, key)
		}
	}
	sort.Strings(roots)
	return roots
}

// unionMoneyAuthorityRoots is the dual-run root set: filename list ∪ structural.
func unionMoneyAuthorityRoots(t *testing.T, g *callGraph) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, key := range filenameMoneyAuthorityRoots(t, g) {
		seen[key] = true
	}
	for _, key := range structuralMoneyAuthorityRoots(t, g) {
		seen[key] = true
	}
	return sortedMoneyAuthorityKeys(seen)
}

package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Structural money/admission authority guard.
//
// Three independent views, then reconcile them. That is the whole design.
//
// View 1 — Declared structural money authority
// (declaredStructuralMoneyAuthoritySinks below):
// an explicit, reviewable catalogue of the sink operations the codebase
// claims own money or economic admission. A human can read it.
//
// View 2 — Observed reachability into money sinks (observeMoneyAuthority):
// derived from the AST every test run, never from filenames. A sink is an
// AST-observed SQL/provider cash mutation; it counts only when an actual
// HTTP/webhook/job/CLI entrypoint reaches it.
// A function is money/admission authority if that live sink can be reached
// through the package call graph.
//
// Reconciliation:
//
//	observed sink not in declaration  → FAIL (money path nobody declared)
//	declared sink not observed        → FAIL (stale or overbroad declaration)
//
// View 3 — Exact amount determination (declaredMoneyAmountAuthority below):
// walks forward from a real sink and admits only functions with exact-money
// type signatures (plus the reviewed scalar arithmetic primitives beneath
// them). This catches the functions that determine a ledger amount but are
// called BY a sink, which a reverse-only closure cannot see.
//
// The calibration/overhead consumption invariant roots at the union of
// (a) every function in the legacy 22-filename list and (b) every function
// in both structural observed-authority sets. Dual-run until the filename
// list is proven removable; see TestNoCallPathFromMoneyOrAdmissionIntoCalibrationReads.
//
// Coverage note: measurement at land time found no live path from the
// previously unguarded money modules into calibration/overhead reads.
// The defect this closes is coverage, not a live breach.
//
// No golang.org/x/tools. Hand-written go/parser + go/ast only, matching
// authority_callgraph_test.go.

// moneyAuthorityDomainTaxonomy is review vocabulary only. It never participates
// in sink classification or reachability: names, comments, tests, and read-only
// words cannot grant money authority or hide a structural cash effect.
var moneyAuthorityDomainTaxonomy = []string{
	"quote",
	"admission",
	"reserve",
	"charge",
	"refund",
	"settlement",
	"liability",
	"contribution",
	"payout",
	"spend authority",
}

// declaredStructuralMoneyAuthoritySinks is View 1's exact, reviewable set of
// live direct effects. Each item is an entrypoint-reachable AST-observed SQL
// mutation of a reviewed money table or POST to a provider money endpoint.
// It is intentionally not a name classifier. The CLI transport and Stripe
// setup rails below are deliberately present: the CLI accepts a
// caller-configured base/method/path and the Stripe adapter performs reviewed
// provider operations beyond cash-only URL fragments. They remain authority
// until production constrains those transports, rather than being hidden by a
// test-only trust assertion.
var declaredStructuralMoneyAuthoritySinks = []string{
	"GatewayParityClient.CompleteOneStream",
	"Store.AllocateBatchStripeFee",
	"Store.ApplyPaymentEventTx",
	"Store.ApplyRepricing",
	"Store.AuthorizePayoutSubsidy",
	"Store.AuthorizeRealtimeContract",
	"Store.BeginBuyerChargeOperation",
	"Store.BeginPrepaidRefund",
	"Store.BeginPrepaidTopup",
	"Store.BumpChargeBatchRetry",
	"Store.CancelServiceLease",
	"Store.ClaimOutcomeUnknownPayouts",
	"Store.ClaimPayout",
	"Store.ClaimReversals",
	"Store.ClawbackTaskCredit",
	"Store.CompletePrepaidRefund",
	"Store.CreateBuyerAccount",
	"Store.CreateExecutionEnvelope",
	"Store.CreateProjectOrder",
	"Store.CreateServiceLease",
	"Store.CreateSubsidyFund",
	"Store.CreditPrepaidTopup",
	"Store.DeferPayout",
	"Store.FailoverServiceLease",
	"Store.FinalizePayout",
	"Store.FinalizeRealtimeFailure",
	"Store.FinalizeRealtimeSuccess",
	"Store.FinalizeReversal",
	"Store.FormChargeBatch",
	"Store.FreezeChargeAmount",
	"Store.HeartbeatRealtimeOffer",
	"Store.HeartbeatServiceLease",
	"Store.IncrementChargeAttempts",
	"Store.InsertHedgeTask",
	"Store.InsertQuote",
	"Store.InsertRealtimeSettlementIntent",
	"Store.InsertTiebreakTask",
	"Store.MarkBuyerDeferredNoCard",
	"Store.MarkChargeBatchCharged",
	"Store.MarkChargeManualReview",
	"Store.MarkJobDeferred",
	"Store.MarkPayoutOutcomeUnknown",
	"Store.MarkReversalFailed",
	"Store.Migrate",
	"Store.NoteBuyerChargeOutcomeUnknown",
	"Store.NoteDisputeNoPeer",
	"Store.PersistVerificationWorkPlan",
	"Store.RecordDispute",
	"Store.RecordRealtimeSettlementIntentFailure",
	"Store.RecoverOrphanEnvelopeSpends",
	"Store.RecoverStalePayoutOperations",
	"Store.RecoverStaleRealtimeContracts",
	"Store.ReflipNoCardJobs",
	"Store.RefundRealtimeContract",
	"Store.ReleaseExpiredExecutionEnvelopes",
	"Store.ReleasePayoutTx",
	"Store.SetBillingPMByCustomer",
	"Store.SetChargeNextAt",
	"Store.SetChargeStatus",
	"Store.SetJobCharged",
	"Store.SetJobActualUSD",
	"Store.SetSupplierPayoutsEnabledByAcct",
	"Store.SettlePendingRealtimeIntents",
	"Store.SettleRealtimeExactReuse",
	"Store.SubmitExactReuseBatchJob",
	"Store.SubmitJobTx",
	"Store.TerminateServiceLeaseNoReplacement",
	"Store.UpsertRealtimeOffer",
	"Store.UpsertServiceLeaseOffer",
	"Store.UpsertWorker",
	"Store.finalizeExpiredServiceLease",
	"Store.markRealtimeSettlementIntentAttempt",
	"Store.markServiceLeaseWorkerLost",
	"Store.setActiveDisputeStatus",
	"Server.handleChatCompletions",
	"Server.handleServiceLeaseChatCompletions",
	"StripePayout.RefundCharge",
	"StripePayout.ReverseTransfer",
	"StripePayout.Send",
	"accrueSupplierLiability",
	"appendDisputeEventTx",
	"applyDisputeBuyerRefundFundingTx",
	"applyStripeChargeRefundState",
	"applyStripeDisputeState",
	"captureEnvelopeSpendTx",
	"budgetWarnOnDispatch",
	"chargePaymentIntent",
	"client.doHeaders",
	"clawbackTaskCreditTx",
	"Store.completeJobEconomics",
	"consumeEconomicReserveTx",
	"creditPrepaidBalanceTx",
	"debitPrepaidByRefTx",
	"debitPrepaidForSLAPremiumTx",
	"debitPrepaidForTaskTx",
	"ensureConnectAccount",
	"ensureStripeCustomer",
	"failJobAndSettleOnce",
	"finalizeBuyerChargeOperation",
	"insertMoneyAuthorityAction",
	"insertJobDisputeBuyerRefundsTx",
	"insertJobDisputeClawbacksTx",
	"insertJobSLAPremiumChargeTx",
	"insertPlannedTiebreakTx",
	"insertLedgerEntryIfAbsentByRefTx",
	"insertLedgerEntryOnTaskConflictDoNothingTx",
	"insertLedgerEntryTx",
	"lockSupplierAccrual",
	"meterServiceLeaseTx",
	"markBudgetStoppedJobs",
	"onboardingLink",
	"recomputeStripeCollectionFunding",
	"recordBuyerCashCollection",
	"releaseEnvelopeSpendForContractTx",
	"releaseRealtimeCapacity",
	"reserveBuyerTopupPayoutFunding",
	"reserveEnvelopeSpendTx",
	"reservePayoutFunding",
	"reservePrepaidForJobTx",
	"resolveDisputeInTx",
	"seedDemo",
	"stripeCreateRefund",
	"setupIntent",
	"syncRuntimeCatalog",
	"Workers.deliverWebhook",
}

// declaredStructuralMoneyAuthority is the reverse-reachable closure of the
// declared structural sinks at land time. It is static on purpose: a new live
// caller of a money sink or a stale disconnected declaration fails the guard.
var declaredStructuralMoneyAuthority = []string{
	"AccrueRiskReserveAtSettlementTx",
	"GatewayParityClient.CompleteOneStream",
	"RunGatewayParityInterleavedLevel",
	"RunGatewayParityMatrix",
	"RunGatewayParityMatrixCell",
	"Server.Routes",
	"Server.chargeForJob",
	"Server.createJob",
	"Server.finalizeJobIfDone",
	"Server.handleAdminCreateSubsidyFund",
	"Server.handleAdminDirectedJob",
	"Server.handleAdminRefundPrepaid",
	"Server.handleAdminRefundRealtimeContract",
	"Server.handleAdminReleasePayout",
	"Server.handleAdminResolveDispute",
	"Server.handleAdminSubsidizePayout",
	"Server.handleBillingSetup",
	"Server.handleBillingTopup",
	"Server.handleCancelServiceLease",
	"Server.handleChatCompletions",
	"Server.handleCreateExecutionEnvelope",
	"Server.handleCreateJob",
	"Server.handleCreateServiceLease",
	"Server.handleFileDispute",
	"Server.handleQuote",
	"Server.handleServiceLeaseHeartbeat",
	"Server.handleServiceLeaseChatCompletions",
	"Server.handleStripeWebhook",
	"Server.handleSupplierOnboard",
	"Server.handleWorkerCommit",
	"Server.handleWorkerRegister",
	"Server.refundPrepaidRemainder",
	"Server.tryRealtimeCoalescedDelivery",
	"Server.tryRealtimeExactReuse",
	"Store.AllocateBatchStripeFee",
	"Store.ApplyPaymentEventTx",
	"Store.ApplyRepricing",
	"Store.AuthorizePayoutSubsidy",
	"Store.AuthorizeRealtimeContract",
	"Store.BeginBuyerChargeOperation",
	"Store.BeginPrepaidRefund",
	"Store.BeginPrepaidTopup",
	"Store.BumpChargeBatchRetry",
	"Store.CancelServiceLease",
	"Store.ClaimOutcomeUnknownPayouts",
	"Store.ClaimPayout",
	"Store.ClaimReversals",
	"Store.ClawbackTaskCredit",
	"Store.CompletePrepaidRefund",
	"Store.CreateExecutionEnvelope",
	"Store.CreateServiceLease",
	"Store.CreateSubsidyFund",
	"Store.CreditPrepaidTopup",
	"Store.DeferPayout",
	"Store.FailoverPendingServiceLeases",
	"Store.FailoverServiceLease",
	"Store.FinalizeExpiredServiceLeases",
	"Store.FinalizeJobTx",
	"Store.FinalizePayout",
	"Store.FinalizeRealtimeFailure",
	"Store.FinalizeRealtimeSuccess",
	"Store.FinalizeReversal",
	"Store.FormChargeBatch",
	"Store.HeartbeatServiceLease",
	"Store.InsertHedgeTask",
	"Store.InsertQuote",
	"Store.InsertRealtimeSettlementIntent",
	"Store.InsertStripeFee",
	"Store.InsertTiebreakTask",
	"Store.MarkChargeBatchCharged",
	"Store.MarkPayoutOutcomeUnknown",
	"Store.MarkReversalFailed",
	"Store.Migrate",
	"Store.NoteBuyerChargeOutcomeUnknown",
	"Store.NoteDisputeNoPeer",
	"Store.ReconcileBuyerChargeOperation",
	"Store.RecordDispute",
	"Store.RecordRealtimeSettlementIntentFailure",
	"Store.RecoverOrphanEnvelopeSpends",
	"Store.RecoverServiceLeases",
	"Store.RecoverStalePayoutOperations",
	"Store.RecoverStaleRealtimeContracts",
	"Store.RefundRealtimeContract",
	"Store.ReleaseExpiredExecutionEnvelopes",
	"Store.ReleasePayoutTx",
	"Store.ResolveDisputeTx",
	"Store.SetDisputeReverifying",
	"Store.SetDisputeStatus",
	"Store.SetJobCharged",
	"Store.SettleJobSLA",
	"Store.SettlePendingRealtimeIntents",
	"Store.SettleRealtimeExactReuse",
	"Store.SubmitExactReuseBatchJob",
	"Store.SubmitJobTx",
	"Store.TerminateServiceLeaseNoReplacement",
	"Store.UpsertWorker",
	"Store.VerifyJobTx",
	"Store.applyVerificationDecision",
	"Store.completeJobEconomics",
	"Store.finalizeExpiredServiceLease",
	"Store.markRealtimeSettlementIntentAttempt",
	"Store.markServiceLeaseWorkerLost",
	"Store.reconcilePrepaidTopup",
	"Store.resolveDispute",
	"Store.setActiveDisputeStatus",
	"StripePayout.RefundCharge",
	"StripePayout.ReverseTransfer",
	"StripePayout.Send",
	"VerificationProcessor.Drain",
	"VerificationProcessor.ProcessAttempt",
	"VerificationProcessor.createPlan",
	"VerificationProcessor.processDrainWork",
	"VerificationProcessor.processLeased",
	"VerificationProcessor.processLeasedOnce",
	"Verifier.PlanTaskResult",
	"Verifier.dispatchTiebreak",
	"Verifier.resolveTiebreak",
	"Verifier.verifyTaskResult",
	"Workers.Run",
	"Workers.chargeBatch",
	"Workers.collectCharges",
	"Workers.deliverPendingWebhooks",
	"Workers.deliverWebhook",
	"Workers.executeReversal",
	"Workers.finalizeJobs",
	"Workers.hedgeStragglers",
	"Workers.processReversals",
	"Workers.raceEndgameTails",
	"Workers.recoverOrphanEnvelopeSpends",
	"Workers.recoverRealtimeContracts",
	"Workers.recoverServiceLeases",
	"Workers.recoverVerification",
	"Workers.releaseExpiredExecutionEnvelopes",
	"Workers.releasePayouts",
	"Workers.resolveDisputes",
	"Workers.retryFailedSingle",
	"Workers.settleRealtimeSettlementIntents",
	"Workers.settleSLAOutcomes",
	"accrueSupplierLiability",
	"appendDisputeEventTx",
	"applyDisputeBuyerRefundFundingTx",
	"applyStripeChargeRefundState",
	"applyStripeDisputeState",
	"captureEnvelopeSpendTx",
	"chargeBuyer",
	"chargeOrDeferJob",
	"chargePaymentIntent",
	"client.do",
	"client.doHeaders",
	"clawbackTaskCreditTx",
	"consumeEconomicReserveTx",
	"creditPrepaidBalanceTx",
	"cmdCancel",
	"cmdEstimate",
	"cmdEvents",
	"cmdExplainScheduler",
	"cmdFailures",
	"cmdInvoice",
	"cmdKeys",
	"cmdLogin",
	"cmdMe",
	"cmdModels",
	"cmdQuote",
	"cmdReceipt",
	"cmdResults",
	"cmdSignup",
	"cmdStatus",
	"cmdSubmit",
	"debitPrepaidByRefTx",
	"debitPrepaidForExecutionContractTx",
	"debitPrepaidForSLAPremiumTx",
	"debitPrepaidForServiceLeaseTx",
	"debitPrepaidForTaskTx",
	"dispatchBuyer",
	"ensureConnectAccount",
	"ensureStripeCustomer",
	"finalizeBuyerChargeOperation",
	"finalizeRealtimeFailure",
	"fetchResults",
	"insertJobDisputeBuyerRefundsTx",
	"insertJobDisputeClawbacksTx",
	"insertJobSLAPremiumChargeTx",
	"insertLedgerEntryIfAbsentByRefTx",
	"insertLedgerEntryIfAbsentExactTx",
	"insertLedgerEntryOnTaskConflictDoNothingTx",
	"insertLedgerEntryTx",
	"insertNewLedgerEntryTx",
	"insertPlannedTiebreakTx",
	"insertServiceLeaseLedgerEntryTx",
	"lockSupplierAccrual",
	"main",
	"maybeDebitPrepaidForRealtimeTx",
	"meterServiceLeaseTx",
	"onboardingLink",
	"recomputeStripeCollectionFunding",
	"recordBuyerCashCollection",
	"recordStripeFee",
	"releaseEnvelopeSpendForContractTx",
	"reserveBuyerTopupPayoutFunding",
	"reserveEnvelopeSpendTx",
	"reservePayoutFunding",
	"reservePrepaidForJobTx",
	"reservePrepaidForServiceLeaseTx",
	"reserveServiceLeasePayoutFunding",
	"resolveDisputeInTx",
	"runGatewayParityCLI",
	"runGatewayParityMatrixLive",
	"runGatewayParityMatrixStandinSelfTest",
	"runGatewayParityPrefixHitLevel",
	"runGatewayParityStandinSelfTest",
	"runWorkerLeader",
	"settleFinalServiceLeaseTx",
	"settleSLAOutcome",
	"seedDemo",
	"setupIntent",
	"stripeCreateRefund",
	"Server.claimWithWait",
	"Server.handleConnectWebhook",
	"Server.handleCreateProjectOrder",
	"Server.handleRealtimeWorkerHeartbeat",
	"Server.handleRealtimeWorkerRegister",
	"Server.handleServiceLeaseOffer",
	"Server.handleSignup",
	"Server.handleSupplierStatus",
	"Server.handleWorkerFail",
	"Server.handleWorkerPoll",
	"Store.ClaimTasksTx",
	"Store.CreateBuyerAccount",
	"Store.CreateProjectOrder",
	"Store.FailTaskAndSettleJob",
	"Store.FailTaskTx",
	"Store.FreezeChargeAmount",
	"Store.HeartbeatRealtimeOffer",
	"Store.IncrementChargeAttempts",
	"Store.MarkBuyerDeferredNoCard",
	"Store.MarkChargeManualReview",
	"Store.MarkJobDeferred",
	"Store.PersistVerificationWorkPlan",
	"Store.ReflipNoCardJobs",
	"Store.SetBillingPMByCustomer",
	"Store.SetChargeNextAt",
	"Store.SetChargeStatus",
	"Store.SetJobActualUSD",
	"Store.SetSupplierPayoutsEnabledByAcct",
	"Store.SweepBudgetStops",
	"Store.UpsertRealtimeOffer",
	"Store.UpsertServiceLeaseOffer",
	"VerificationProcessor.createArtifactFailurePlan",
	"VerificationProcessor.createOversizedArtifactPlan",
	"VerificationProcessor.createUnavailableArtifactPlan",
	"Workers.reapStuckJobs",
	"Workers.requeueStaleTasks",
	"Workers.sweepBudgetStops",
	"budgetWarnOnDispatch",
	"failJobAndSettleOnce",
	"insertMoneyAuthorityAction",
	"markBudgetStoppedJobs",
	"releaseRealtimeCapacity",
	"syncRuntimeCatalog",
	"waitForJob",
}

// declaredMoneyAmountAuthority is the third, deliberately narrow view of
// money authority: exact amount determination below a declared money sink.
//
// The main declaration above is reverse-reachable (who can CAUSE a money
// write).  That direction cannot see helpers that a sink calls to determine the
// amount.  This separate catalogue closes that blind spot without turning every
// numeric helper in the package into money authority.  Observation selects only
// functions reached forward from a real sink whose declared parameter/result
// types carry exact currency/rate/unit semantics, plus the small reviewed list
// of scalar arithmetic primitives used beneath those types.  It never reads a
// comment, test, filename, or read-only word such as "balance".
//
// Keep this in exact observed order only for review; the test sorts before
// comparison and rejects both an undeclared new determiner and a stale entry.
var declaredMoneyAmountAuthority = []string{
	"BuyerRealtimeTokenChargeNanos",         // money_nanos.go
	"CatalogueGrossNanos",                   // money_nanos.go
	"LedgerMicrosFromNanos",                 // money_nanos.go
	"MoneyNanosFromUSDFloat",                // money_nanos.go
	"NanoWorkUnitsFromFloat",                // money_nanos.go
	"NewMoneyNanos",                         // money_nanos.go
	"RealtimeReuseBuyerChargeNanos",         // money_nanos.go
	"SupplierEntitlementNanos",              // money_nanos.go
	"SupplierRealtimeTokenEntitlementNanos", // money_nanos.go
	"catalogueSettlementPriceNanosPer1K",    // pricing_decision.go
	"ceilUSDToNanos",                        // economic_plan.go
	"egressNanosForBytes",                   // cost_schedule.go
	"exactTaskEconomics",                    // pricing_decision.go
	"mul3Div",                               // money_nanos.go
	"mulDiv",                                // money_nanos.go
	"nanoRatePerMillionFromFloat",           // money_nanos.go
	"nanosPer1KFromFloat",                   // money_nanos.go
	"providerCostNanos",                     // provider_cost_authority.go
	"realtimeAuthNeedNanos",                 // realtime_store.go
	"realtimeTokenChargeNanos",              // money_nanos.go
	"riskReserveNanos",                      // cost_schedule.go
	"serviceLeaseComponent",                 // service_lease_pricing.go
	"storageNanosForBytes",                  // cost_schedule.go
	"tokenChargeExact",                      // realtime_store.go
}

// moneyAuthorityObservation is View 2: structural observation for one package parse.
type moneyAuthorityObservation struct {
	// Sinks are live functions whose AST contains a money-table mutation or
	// provider money POST. The human name taxonomy is deliberately not used to
	// classify a sink: vocabulary can neither grant authority nor hide a generic
	// db.Exec/provider call.
	Sinks map[string]bool
	// Authority is every function that is a sink or can reach a sink (reverse BFS).
	Authority map[string]bool
	// File of each authority key (from the call graph).
	File map[string]string
	// Entrypoints is the real process/HTTP/webhook/CLI root set used for this
	// census. It makes disconnected declarations stale rather than authority.
	Entrypoints map[string]bool
}

// observeMoneyAuthority derives View 2 from the call graph. No filename list
// is consulted. Test files are already excluded by buildCallGraph.
func observeMoneyAuthority(g *callGraph) moneyAuthorityObservation {
	obs := moneyAuthorityObservation{
		Sinks:       map[string]bool{},
		Authority:   map[string]bool{},
		File:        map[string]string{},
		Entrypoints: map[string]bool{},
	}
	for key := range g.entrypoints {
		obs.Entrypoints[key] = true
	}
	live := g.authorityReachableFrom(g.entrypoints)
	authorityEdges := g.authorityEdges
	if authorityEdges == nil {
		authorityEdges = g.edges
	}
	// Reverse edges: callee → callers.
	rev := map[string]map[string]bool{}
	for from, tos := range authorityEdges {
		for to := range tos {
			if rev[to] == nil {
				rev[to] = map[string]bool{}
			}
			rev[to][from] = true
		}
	}
	queue := make([]string, 0, 64)
	for key, file := range g.file {
		if !live[key] {
			continue
		}
		if !g.structuralMoneySinks[key] {
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
			if !live[caller] {
				continue
			}
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

// moneyAmountScalarHelperBareName covers the intentionally scalar pieces of
// exact monetary arithmetic.  They carry int64 at their Go boundary, so a
// signature-only classifier cannot see them; each is listed because it is
// directly part of an exact amount derivation, not because its name sounds like
// money.  The forward-reachability condition below still prevents an unrelated
// helper from entering the census.
func moneyAmountScalarHelperBareName(name string) bool {
	switch name {
	case "mulDiv", "mul3Div",
		"ceilUSDToNanos", "storageNanosForBytes", "egressNanosForBytes", "riskReserveNanos",
		"providerCostNanos", "realtimeAuthNeedNanos",
		"MinorToMicros", "MajorToMinor", "ParseMajorToMinorExact":
		return true
	}
	return false
}

func moneyAmountScalarHelperKey(key string) bool {
	name := key
	if i := strings.LastIndex(key, "."); i >= 0 {
		name = key[i+1:]
	}
	return moneyAmountScalarHelperBareName(name)
}

// forwardReachable returns roots plus every package declaration they can call
// or pass as a value.  `edges` intentionally includes function values: handing
// exact arithmetic to a callback still lets the money sink determine an amount
// through it, so omitting it would be a coverage hole.
func forwardReachable(g *callGraph, roots map[string]bool) map[string]bool {
	reached := map[string]bool{}
	queue := make([]string, 0, len(roots))
	for key := range roots {
		reached[key] = true
		queue = append(queue, key)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		authorityEdges := g.authorityEdges
		if authorityEdges == nil {
			authorityEdges = g.edges
		}
		for callee := range authorityEdges[cur] {
			if reached[callee] {
				continue
			}
			reached[callee] = true
			queue = append(queue, callee)
		}
	}
	return reached
}

type moneyAmountAuthorityObservation struct {
	Authority map[string]bool
	File      map[string]string
}

// observeMoneyAmountAuthority is the forward counterpart to the reverse sink
// closure.  It catches changing CatalogueGrossNanos (and its other exact-money
// peers) even though those functions do not themselves write a ledger row.
func observeMoneyAmountAuthority(g *callGraph, money moneyAuthorityObservation) moneyAmountAuthorityObservation {
	obs := moneyAmountAuthorityObservation{
		Authority: map[string]bool{},
		File:      map[string]string{},
	}
	for key := range forwardReachable(g, money.Sinks) {
		if !g.exactMoneySignature[key] && !moneyAmountScalarHelperKey(key) {
			continue
		}
		obs.Authority[key] = true
		obs.File[key] = g.file[key]
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

func moneyAuthorityDeclarationDiff(observed, declared map[string]bool) (undeclared, stale []string) {
	for _, key := range sortedMoneyAuthorityKeys(observed) {
		if !declared[key] {
			undeclared = append(undeclared, key)
		}
	}
	for _, key := range sortedMoneyAuthorityKeys(declared) {
		if !observed[key] {
			stale = append(stale, key)
		}
	}
	return undeclared, stale
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

	declaredSinks := setFromList(declaredStructuralMoneyAuthoritySinks)
	declaredAuth := setFromList(declaredStructuralMoneyAuthority)

	if len(declaredSinks) != len(declaredStructuralMoneyAuthoritySinks) {
		t.Fatalf("declaredStructuralMoneyAuthoritySinks has duplicate entries (%d unique of %d)",
			len(declaredSinks), len(declaredStructuralMoneyAuthoritySinks))
	}
	if len(declaredAuth) != len(declaredStructuralMoneyAuthority) {
		t.Fatalf("declaredStructuralMoneyAuthority has duplicate entries (%d unique of %d)",
			len(declaredAuth), len(declaredStructuralMoneyAuthority))
	}
	// Every declared sink must appear in the full declaration.
	for _, key := range declaredStructuralMoneyAuthoritySinks {
		if !declaredAuth[key] {
			t.Errorf("declared sink %s missing from declaredStructuralMoneyAuthority", key)
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

// TestMoneyAmountAuthorityDeclarationMatchesObservation closes the direction
// the reverse sink closure cannot cover: a money writer calls an exact amount
// determiner rather than being called by it.  Both mismatches are errors.  A
// new amount calculation has to be reviewed and declared; a deleted or
// disconnected declaration must not remain as comforting dead text.
func TestMoneyAmountAuthorityDeclarationMatchesObservation(t *testing.T) {
	graph := buildCallGraph(t)
	money := observeMoneyAuthority(graph)
	observed := observeMoneyAmountAuthority(graph, money)
	declared := setFromList(declaredMoneyAmountAuthority)

	if len(declared) != len(declaredMoneyAmountAuthority) {
		t.Fatalf("declaredMoneyAmountAuthority has duplicate entries (%d unique of %d)",
			len(declared), len(declaredMoneyAmountAuthority))
	}

	undeclared, stale := moneyAuthorityDeclarationDiff(observed.Authority, declared)
	if len(undeclared) > 0 {
		rows := make([]string, 0, len(undeclared))
		for _, key := range undeclared {
			rows = append(rows, fmt.Sprintf("%s (%s)", key, observed.File[key]))
		}
		t.Errorf("observed but undeclared money amount authority (%d) — a sink can determine money through an unreviewed function:\n  %s",
			len(rows), strings.Join(rows, "\n  "))
	}
	if len(stale) > 0 {
		rows := make([]string, 0, len(stale))
		for _, key := range stale {
			if file, exists := graph.file[key]; exists {
				rows = append(rows, fmt.Sprintf("%s (%s) (declared but no longer reached from a money sink)", key, file))
			} else {
				rows = append(rows, key+" (not a declared function in package)")
			}
		}
		t.Errorf("declared but stale money amount authority (%d) — remove or reconnect the declaration:\n  %s",
			len(rows), strings.Join(rows, "\n  "))
	}

	files := map[string]bool{}
	for key := range observed.Authority {
		files[observed.File[key]] = true
	}
	t.Logf("money amount census: %d exact determinations across %d files (forward from %d structural sinks)",
		len(observed.Authority), len(files), len(money.Sinks))
}

func TestMoneyAmountAuthorityReconciliationRefusesBothDirections(t *testing.T) {
	undeclared, stale := moneyAuthorityDeclarationDiff(
		map[string]bool{"unlistedExactAmount": true},
		map[string]bool{"staleExactAmount": true},
	)
	if got, want := strings.Join(undeclared, ","), "unlistedExactAmount"; got != want {
		t.Fatalf("undeclared=%q want %q", got, want)
	}
	if got, want := strings.Join(stale, ","), "staleExactAmount"; got != want {
		t.Fatalf("stale=%q want %q", got, want)
	}
}

// The observation must genuinely walk downward from a money sink.  This is an
// in-memory mutation proof: an otherwise unlisted function with an exact-money
// signature is caught when a sink can reach it, while read-only vocabulary is
// irrelevant because no comments or names are inspected for this decision.
func TestMoneyAmountAuthorityFindsForwardExactDeterminer(t *testing.T) {
	graph := &callGraph{
		edges: map[string]map[string]bool{
			"syntheticDBWriter": {"syntheticExactDeterminer": true},
		},
		file: map[string]string{
			"syntheticDBWriter":        "synthetic_writer.go",
			"syntheticExactDeterminer": "synthetic_amount.go",
		},
		exactMoneySignature: map[string]bool{
			"syntheticExactDeterminer": true,
		},
		structuralMoneySinks: map[string]bool{"syntheticDBWriter": true},
		entrypoints:          map[string]bool{"syntheticDBWriter": true},
	}
	money := observeMoneyAuthority(graph)
	observed := observeMoneyAmountAuthority(graph, money)
	if !observed.Authority["syntheticExactDeterminer"] {
		t.Fatal("forward exact determiner was not observed from synthetic money sink")
	}
}

// TestMoneyAuthorityIgnoresReadOnlyAndComments pins two non-firing cases so a
// future tightening cannot silently start treating reports as authority.
func TestMoneyAuthorityIgnoresReadOnlyAndComments(t *testing.T) {
	graph := buildCallGraph(t)
	obs := observeMoneyAuthority(graph)
	amounts := observeMoneyAmountAuthority(graph, obs)

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
	// A historical vocabulary-only classification once marked these hard-refusal
	// rails as money sinks. They have no structural SQL/provider cash effect, so
	// their names must never make the declaration appear current.
	for _, key := range []string{
		"ManualExportPayout.RefundCharge",
		"stubPayout.RefundCharge",
	} {
		if obs.Sinks[key] {
			t.Errorf("name-only refusal rail %s classified as a structural money sink", key)
		}
	}

	// A money-looking read or display projection is not an amount determiner.
	// The forward census uses typed inputs/results and a sink-reachability edge,
	// not comments or words such as "balance", "USD", or "String".
	for _, key := range []string{
		"Store.BuyerPrepaidBalanceMicros",
		"Store.BuyerPrepaidAvailableMicros",
		"Store.RiskReserveBalanceMicros",
		"MoneyNanos.USDFloat",
		"MoneyNanos.String",
		"MoneyNanos.IsZero",
	} {
		if amounts.Authority[key] {
			t.Errorf("read-only/display function %s classified as exact money authority", key)
		}
	}
}

// structuralMoneyAuthorityRoots returns the sorted function keys that the
// structural views treat as money/admission roots: both reverse-reachable
// writer authority and forward exact amount determination.
func structuralMoneyAuthorityRoots(t *testing.T, g *callGraph) []string {
	t.Helper()
	money := observeMoneyAuthority(g)
	amounts := observeMoneyAmountAuthority(g, money)
	roots := map[string]bool{}
	for key := range money.Authority {
		roots[key] = true
	}
	for key := range amounts.Authority {
		roots[key] = true
	}
	return sortedMoneyAuthorityKeys(roots)
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

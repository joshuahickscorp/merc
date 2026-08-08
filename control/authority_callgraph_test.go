package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A call graph, because an identifier scan is not one.
//
// TestCalibrationIsUnreachableFromMoneyAndAdmissionPaths asks "does this file's
// code name this symbol", which is the right question for a file that must never
// touch calibration and the wrong one for a file that is merely ALLOWED to. Two
// of the six allowlisted files are api.go and workers.go, which between them hold
// most of the control plane. A money handler in api.go calling a calibration
// reader in api.go passes that test completely, because the check is per file and
// api.go is on the list.
//
// This closes that hole by following calls instead of names: it builds the
// package's call graph from the AST and fails if any function declared in a
// money, pricing, settlement or admission file can reach a calibration or
// overhead READ, at any depth.
//
// Reads, not writes. Recording an observation from the finalize hook is the
// point of the table; the invariant is that no decision about money or admission
// may CONSUME one.
//
// No SSA and no golang.org/x/tools. This module has five direct dependencies and
// ships in the production container; pulling a program-analysis toolchain into
// go.mod and the SBOM for a test is a worse trade than writing the traversal.
// Everything here is one package, so a call resolves by name, which is the case
// SSA would mostly be buying.
//
// What this does NOT prove, stated here rather than left to be discovered:
//
//   - It is a call graph, not a data-flow analysis. A handler outside the guarded
//     files could read a calibration number and pass it in as an argument, and no
//     reachability check can see that — the receiving function cannot distinguish
//     it from any other input. Closing that needs taint tracking, which is a
//     different and much larger tool.
//   - Reflection and dynamically-built dispatch are invisible. Neither appears on
//     these paths today.
//   - Method calls resolve by bare name across every receiver type, so the graph
//     over-approximates. That direction is safe: it can report a path that no
//     concrete type actually takes, never miss one that does.
//
// Verified by mutation, not by inspection: planting
// `mutantPricingEntryPoint → mutantHelperReadsCalibration → Store.PlanAccuracy`
// in pricing.go fails this test with the full chain printed, and the identifier
// gate alone does not catch the two-hop form when the helper sits in an
// allowlisted file.

// callGraph maps a function key to the keys it can reach in one step.
type callGraph struct {
	// edges is keyed by "Recv.Name" for methods and "Name" for functions.
	// Built from every identifier that names a declared function (calls and
	// func values). Used for money-authority observation: a sink handed off as
	// a value is still reachable.
	edges map[string]map[string]bool
	// authorityEdges contains only exact source-level internal call targets (or
	// an exact reviewed dynamic-dispatch target). Unlike edges, it never fans a
	// selector out by bare method name across unrelated receiver types. Money
	// declaration staleness is proved from this graph, not an over-approximation
	// that could keep dead authority text alive.
	authorityEdges map[string]map[string]bool
	// callEdges follows only CallExpr sites. Used for the calibration
	// consumption check so HTTP route registration (passing a handler method
	// value into mux.Handle) is not treated as the router consuming what the
	// handler reads. A real multi-hop call chain still appears here.
	callEdges map[string]map[string]bool
	// byName indexes every declaration sharing a bare name, so a call through a
	// receiver whose concrete type is not resolved here reaches all candidates.
	// Over-approximating edges can only make this gate stricter.
	byName map[string][]string
	// functionsByName contains only package functions (not methods); a bare Go
	// call can target only this set. methodsByReceiver is the corresponding
	// exact receiver-qualified lookup used by authorityEdges.
	functionsByName     map[string][]string
	methodsByReceiver   map[string][]string
	functionResultTypes map[string]string
	structFields        map[string]map[string]string
	interfaceTypes      map[string]bool
	interfaceMethods    map[string]map[string]bool
	// file records where each declaration lives.
	file map[string]string
	// exactMoneySignature records functions whose parameter or result type is a
	// currency-bound exact-money/rate/unit type.  It is structural type data
	// collected from the declaration, not a filename or comment classifier.
	// The money-authority guard uses it for the narrow forward census of amount
	// determination below a money sink.
	exactMoneySignature map[string]bool
	// structuralMoneySinks are AST-observed mutations of reviewed money tables
	// or POSTs to provider money endpoints.  They deliberately do not depend on
	// a function name such as "charge" or "refund": a generic db.Exec or an
	// innocuously named provider POST is still a money sink.
	structuralMoneySinks map[string]bool
	// moneyMutationTableHits is the literal SQL census supporting the reviewed
	// schema-column scope. Tests require every scoped table to exist in schema.sql
	// and to have an observed non-test mutation site.
	moneyMutationTableHits map[string]bool
	// moneyMutationColumnHits records the exact static fields observed for mixed
	// scopes. An unknown column list is intentionally recorded only at table
	// level, because it is fail-closed authority but cannot prove a specific
	// column's provenance.
	moneyMutationColumnHits map[string]map[string]bool
	// moneyMutationEffectsByFunction preserves the direct effect provenance so
	// scope coverage can require a real entrypoint-reachable writer for every
	// reviewed mixed-table field, not merely any disconnected helper.
	moneyMutationEffectsByFunction map[string]map[string]moneyMutationEffect
	// entrypoints are actual process, HTTP, webhook, job, and CLI roots.  A
	// declaration is authority only if it is reachable from one of these roots;
	// disconnected helpers and refusal-only code cannot keep a stale declaration
	// green merely because its name sounds monetary.
	entrypoints map[string]bool
	// unresolvedMoneyExpressions are dynamic database/bulk/provider operation
	// sites. They are not silently ignored: a separate test requires an exact
	// file/function/line/kind exception with a reviewed reason for every one.
	unresolvedMoneyExpressions map[string]moneyExpressionIssue
	// unresolvedAuthorityDispatches are package methods invoked through an
	// interface/unknown receiver. They must have an exact reviewed target list;
	// silently fanning out by selector name invalidates stale-authority proofs.
	unresolvedAuthorityDispatches map[string]authorityDispatchIssue
	// unresolvedAuthorityDispatchCandidates records every syntactically
	// unresolved selector collision first. After the full structural scan, only
	// collisions with an internal method that can reach a money sink become a
	// dispatch boundary. This keeps unrelated external Error/Close/String calls
	// out of a money-proof ledger without allowing an unknown money method to
	// disappear behind a bare receiver.
	unresolvedAuthorityDispatchCandidates map[string]authorityDispatchIssue
}

type authorityDispatchIssue struct {
	File     string
	Function string
	Line     int
	Method   string
}

// packageInitializationRoot represents Go's package-level variable
// initializers. Function init declarations are roots in their own right below;
// this synthetic root captures eager calls in var initializers as well.
const packageInitializationRoot = "package.init"

func (issue authorityDispatchIssue) key() string {
	return issue.File + ":" + strconv.Itoa(issue.Line) + ":" + issue.Function + ":" + issue.Method
}

type moneyExpressionIssue struct {
	File     string
	Function string
	Line     int
	Kind     string
}

func (issue moneyExpressionIssue) key() string {
	return issue.File + ":" + strconv.Itoa(issue.Line) + ":" + issue.Function + ":" + issue.Kind
}

type moneyExpressionEffect struct {
	Table    string
	Columns  []string
	WholeRow bool
}

type dynamicDBEvidence struct {
	// Mode identifies a concrete source-derived proof below. It is not prose:
	// every dynamic database exception must bind its exact source artifact and
	// produce the same reviewed money effects (including an intentionally empty
	// set for a proved read-only/non-money statement).
	Mode     string
	Artifact string
}

type moneyExpressionException struct {
	Reason               string
	Effects              []moneyExpressionEffect
	DeriveLiteralEffects bool
	DBEvidence           dynamicDBEvidence
}

// reviewedMoneyExpressionExceptions pins the small set of dynamic operation
// sites that cannot be made one static expression without obscuring their
// bounded purpose. The key is exact file:line:function:kind, not a wildcard.
// An exception that can mutate money state must also name its exact reviewed
// table/column effects; exceptions are not a way to hide a real sink.
var reviewedMoneyExpressionExceptions = map[string]moneyExpressionException{
	"buyer.go:94:client.doHeaders:dynamic-provider-money-method": {
		Reason: "CLI transport accepts a caller-configured base URL, method, and path; it remains a potential provider-money sink until production transport authority is constrained",
	},
	"data_governance.go:38:queryJSON:dynamic-db-statement": {
		Reason:     "private DSAR QueryRow wrapper is called only from the read-only export transaction's finite SELECT collection",
		DBEvidence: dynamicDBEvidence{Mode: "readonly-dsar", Artifact: "data_governance.go"},
	},
	"data_governance.go:407:enforceBuyerTombstone:dynamic-db-statement": {
		Reason:               "finite local list of static privacy-purge statements; reviewed money records are retained/redacted rather than silently ignored",
		DeriveLiteralEffects: true,
		DBEvidence:           dynamicDBEvidence{Mode: "literal-collection", Artifact: "data_governance.go"},
		Effects: []moneyExpressionEffect{
			{Table: "billing_customers", WholeRow: true},
			{Table: "buyers", Columns: []string{"free_credit_usd"}},
			{Table: "disputes", WholeRow: true},
			{Table: "quotes", WholeRow: true},
		},
	},
	"gateway_parity_harness.go:848:GatewayParityClient.CompleteOneStream:dynamic-provider-money-endpoint": {
		Reason: "benchmark data-plane endpoint remains structural provider authority until its base URL is finitely constrained",
	},
	"payment.go:390:StripePayout.ReverseTransfer:dynamic-provider-money-endpoint": {
		Reason: "Stripe transfer reversal path includes the escaped transfer identifier and is protected by provider idempotency",
	},
	"realtime.go:1168:Server.handleChatCompletions:dynamic-provider-money-endpoint": {
		Reason: "selected runtime-cell endpoint remains structural provider authority until its origin is finitely constrained",
	},
	"realtime_store.go:861:Store.AuthorizeRealtimeContract:dynamic-db-statement": {
		Reason:     "local selector receives only the two reviewed realtime offer SQL constants; its capacity decrement is coupled to the separately observed reservation batch",
		DBEvidence: dynamicDBEvidence{Mode: "bounded-realtime-offer", Artifact: "realtime_supplier_outcome_stats.go"},
		Effects: []moneyExpressionEffect{
			{Table: "realtime_worker_offers", Columns: []string{"available_sequences"}},
		},
	},
	"runtime_profile_sync.go:114:syncRuntimeProfiles:dynamic-db-statement": {
		Reason:     "version interpolation and fixed child-table loop are bounded to runtime profile projections, not money tables",
		DBEvidence: dynamicDBEvidence{Mode: "runtime-profile-version-template", Artifact: "capability_manifest.go"},
	},
	"runtime_profile_sync.go:144:syncRuntimeProfiles:dynamic-db-statement": {
		Reason:     "fixed local child-table loop deletes only runtime profile projections, not money tables",
		DBEvidence: dynamicDBEvidence{Mode: "runtime-profile-child-list", Artifact: "runtime_profile_sync.go"},
	},
	"seed.go:119:seedDemo:dynamic-db-statement": {
		Reason:               "finite local list of static development seed statements; no caller-provided SQL",
		DeriveLiteralEffects: true,
		DBEvidence:           dynamicDBEvidence{Mode: "literal-collection", Artifact: "seed.go"},
		Effects: []moneyExpressionEffect{
			{Table: "buyers", Columns: []string{"free_credit_usd"}},
			{Table: "workers", Columns: []string{"min_payout_usd_hr"}},
		},
	},
	"scheduler.go:1020:Store.ClaimTasksTx:dynamic-db-statement": {
		Reason:     "local claim helper invokes the repository-reviewed ClaimTaskSQL builder with one of two fixed predicates; it only claims lifecycle state before later observed settlement",
		DBEvidence: dynamicDBEvidence{Mode: "bounded-claim-builder", Artifact: "scheduler.go"},
	},
	"service_lease_data_plane.go:169:Server.handleServiceLeaseChatCompletions:dynamic-provider-money-endpoint": {
		Reason: "reserved runtime-cell endpoint remains structural provider authority until its origin is finitely constrained",
	},
	"serving_matrix_runner.go:78:OpenAICompatClient.CompleteOne:dynamic-provider-money-endpoint": {
		Reason: "serving-matrix endpoint remains structural provider authority until its base URL is finitely constrained",
	},
	"store.go:110:Store.Migrate:dynamic-db-statement": {
		Reason:     "canonicalSchema is the embedded, repository-reviewed schema payload executed only during migration",
		DBEvidence: dynamicDBEvidence{Mode: "embedded-schema", Artifact: "schema.sql"},
		Effects: []moneyExpressionEffect{
			{Table: "buyer_cash_collections", WholeRow: true},
			{Table: "charge_batch_fee_allocations", WholeRow: true},
			{Table: "disputes", WholeRow: true},
			{Table: "job_economic_plans", WholeRow: true},
			{Table: "prepaid_refund_operations", WholeRow: true},
			{Table: "realtime_authorization_events", WholeRow: true},
			{Table: "realtime_settlements", WholeRow: true},
			{Table: "realtime_worker_offers", Columns: []string{"status"}},
		},
	},
	"store_tasks.go:1266:Store.TaskDurationHistogram:dynamic-db-statement": {
		Reason:     "locally formatted histogram remains a SELECT-only task-duration report; no caller-controlled SQL is accepted",
		DBEvidence: dynamicDBEvidence{Mode: "readonly-duration-template", Artifact: "store_tasks.go"},
	},
	"workers.go:1111:Workers.deliverWebhook:dynamic-provider-money-endpoint": {
		Reason: "buyer-configured webhook remains structural provider authority until its destination is finitely constrained",
	},
}

// reviewedAuthorityDispatches binds an interface/unknown-receiver method call
// to the concrete in-package targets that production construction permits. The
// key is exact file:line:containing-function:method; no name-wide fallback is
// accepted. Populate only after reviewing the dispatch boundary itself.
var reviewedAuthorityDispatches = map[string][]string{}

// reviewedAuthorityInterfaceImplementations is the finite production
// construction ledger for package interfaces that can cross an authority
// boundary. It is keyed by declared interface type, never a bare method name:
// only a receiver admitted at that interface boundary receives a call edge.
// Test-only fakes are absent because buildCallGraph excludes test files; the
// startup-refused ManualExportPayout is intentionally absent as well.
var reviewedAuthorityInterfaceImplementations = map[string][]string{
	"Payout":                 {"StripePayout", "stubPayout"},
	"PayoutReverser":         {"StripePayout", "stubPayout"},
	"ServingEngineClient":    {"OpenAICompatClient"},
	"taskResultPutPresigner": {"Storage"},
	"verificationStore":      {"Store", "recordingVerificationStore"},
}

type nonLiveAuthorityInterfaceImplementation struct {
	Reason string
}

// reviewedNonLiveAuthorityInterfaceImplementations makes a concrete adapter
// exclusion an executable claim, not prose. The test below discovers every
// in-package implementation of each authority interface and requires it to be
// either live/constructed in the implementation ledger above or explicitly
// excluded here with no entrypoint-reachable constructor or method.
var reviewedNonLiveAuthorityInterfaceImplementations = map[string]nonLiveAuthorityInterfaceImplementation{
	"Payout.ManualExportPayout": {
		Reason: "manual export is a test/development-only file instruction rail; production construction is startup-refused and no non-test entrypoint reaches its constructor or methods",
	},
	"PayoutReverser.ManualExportPayout": {
		Reason: "manual export reversal/refund methods are hard refusals; production construction is startup-refused and no non-test entrypoint reaches its constructor or methods",
	},
}

// exactMoneySignatureTypes are the type-level money boundary.  They are kept
// explicit rather than matching words such as "price" or "balance": vocabulary
// is not authority, and this list names the types that preserve the exact
// currency/rate/quantity semantics the money layer actually consumes.
var exactMoneySignatureTypes = map[string]bool{
	"MoneyNanos":                true,
	"MinorAmount":               true,
	"NanoUSDPerHour":            true,
	"NanoUSDPerThousandUnits":   true,
	"NanoMajorPerMillionTokens": true,
	"NanoWorkUnits":             true,
	"NanoUnitsPerSecond":        true,
	"DurationNanos":             true,
	"RemainderCarry":            true,
}

// moneyMutationScope is the review ledger for database effects that can set
// quote, admission, reserve, charge, refund, settlement, liability,
// contribution, payout, or spend authority.  A one-purpose financial ledger
// can be whole-row scoped.  A mixed operational table is deliberately scoped
// to the exact authority columns below, so an unrelated UPDATE jobs SET status
// does not become a money sink while UPDATE jobs SET max_usd cannot hide.
//
// Categories are audit metadata only.  Runtime sink observation is based only
// on parsed SQL/CopyFrom effects and live reachability; it never uses names or
// comments in production code.
type moneyMutationScope struct {
	Categories []string
	Columns    map[string]bool
	WholeRow   bool
}

func moneyWholeRow(categories ...string) moneyMutationScope {
	return moneyMutationScope{Categories: categories, WholeRow: true}
}

func moneyColumns(categories []string, columns ...string) moneyMutationScope {
	set := make(map[string]bool, len(columns))
	for _, column := range columns {
		set[column] = true
	}
	return moneyMutationScope{Categories: categories, Columns: set}
}

// moneyMutationScopes is the canonical inclusion ledger.  Do not replace this
// with a word classifier: generic writers are intentionally caught by their
// SQL effect, while mixed operational tables must name their authority fields.
var moneyMutationScopes = map[string]moneyMutationScope{
	"buyer_cash_collections":           moneyWholeRow("charge"),
	"buyer_charge_operations":          moneyWholeRow("charge"),
	"buyer_prepaid_balances":           moneyWholeRow("charge", "refund", "spend authority"),
	"catalogue_price_schedules":        moneyWholeRow("quote"),
	"charge_batch_fee_allocations":     moneyWholeRow("charge", "contribution"),
	"charge_batches":                   moneyWholeRow("charge"),
	"dispute_events":                   moneyWholeRow("refund", "liability"),
	"dispute_payout_holds":             moneyWholeRow("liability", "payout"),
	"disputes":                         moneyWholeRow("refund", "liability"),
	"execution_contracts":              moneyWholeRow("quote", "admission", "reserve", "charge"),
	"execution_envelope_events":        moneyWholeRow("reserve"),
	"execution_envelope_spends":        moneyWholeRow("spend authority"),
	"execution_envelopes":              moneyWholeRow("reserve", "spend authority"),
	"job_cost_settlements":             moneyWholeRow("settlement", "contribution"),
	"job_dispute_refunds":              moneyWholeRow("refund"),
	"job_economic_plans":               moneyWholeRow("quote", "reserve"),
	"job_economic_reserves":            moneyWholeRow("reserve"),
	"job_token_accounting":             moneyWholeRow("settlement", "contribution"),
	"ledger_entries":                   moneyWholeRow("settlement", "liability"),
	"model_price_history":              moneyWholeRow("quote"),
	"platform_subsidy_funds":           moneyWholeRow("spend authority", "payout"),
	"prepaid_refund_operations":        moneyWholeRow("refund"),
	"prepaid_topup_operations":         moneyWholeRow("charge"),
	"quotes":                           moneyWholeRow("quote"),
	"realtime_authorization_events":    moneyWholeRow("reserve"),
	"realtime_refunds":                 moneyWholeRow("refund"),
	"realtime_settlement_intents":      moneyWholeRow("settlement"),
	"realtime_settlements":             moneyWholeRow("settlement", "contribution", "liability", "payout"),
	"service_lease_events":             moneyWholeRow("admission", "settlement"),
	"service_lease_meterings":          moneyWholeRow("settlement", "contribution", "liability", "payout"),
	"service_lease_supplier_meterings": moneyWholeRow("settlement", "liability", "payout"),
	"service_leases":                   moneyWholeRow("admission", "reserve", "settlement", "contribution", "liability", "payout"),
	"stripe_charge_cash_state":         moneyWholeRow("charge"),
	"stripe_dispute_cash_state":        moneyWholeRow("refund", "liability"),
	"stripe_webhook_events":            moneyWholeRow("charge", "refund", "liability"),
	"supplier_minor_unit_settlements":  moneyWholeRow("settlement", "liability", "payout"),
	"supplier_payout_accruals":         moneyWholeRow("liability", "payout"),
	"supplier_payout_funding":          moneyWholeRow("liability", "payout", "spend authority"),
	"supplier_payout_funding_state":    moneyWholeRow("liability", "payout"),
	"supplier_payout_operations":       moneyWholeRow("payout"),

	// Mixed lifecycle tables: only these frozen/financial/admission columns
	// qualify.  Their surrounding status and scheduler fields remain out of the
	// observed sink set unless they live in a one-purpose authority ledger above.
	"admin_actions": moneyColumns([]string{"spend authority"},
		"amount_cents", "currency", "fund_id", "fund_ref", "authorization_ref"),
	"billing_customers": moneyColumns([]string{"charge"}, "default_payment_method"),
	"buyers":            moneyColumns([]string{"spend authority"}, "free_credit_usd"),
	"jobs": moneyColumns([]string{"quote", "admission", "reserve", "charge", "spend authority"},
		"estimated_usd", "actual_usd", "offered_rate_usd_hr", "max_usd", "budget_state",
		"charge_status", "charge_attempt_usd", "charge_batch_id", "charge_attempts", "charge_next_at",
		"firm_quote", "firm_quote_max_usd", "sla_premium_usd", "billed_usd",
		"charge_requested_cents", "charge_received_cents", "charge_currency", "prepaid_required",
		"currency", "quote_id", "pricing_decision", "pricing_decision_sha256", "project_order_id", "project_step_id"),
	"models": moneyColumns([]string{"quote"},
		"price_per_1k", "price_source", "price_formula", "price_reference_currency",
		"price_reference_per_1k", "price_currency", "price_schedule_sha256", "price_schedule_version"),
	"project_orders": moneyColumns([]string{"quote", "admission", "reserve", "spend authority"},
		"currency", "buyer_ceiling_nanos"),
	"project_order_steps": moneyColumns([]string{"quote", "reserve", "spend authority"},
		"quote_id", "pricing_decision_sha256", "accepted_ceiling_nanos"),
	"realtime_coalesced_deliveries": moneyColumns([]string{"settlement", "contribution", "liability", "payout"},
		"currency", "counterfactual_supplier_entitlement_nanos"),
	"realtime_worker_offers": moneyColumns([]string{"quote", "admission"},
		"runtime_profile_id", "placement_plan", "placement_plan_sha256", "warmth",
		"max_active_sequences", "available_sequences", "supplier_input_usd_per_million_tokens",
		"supplier_output_usd_per_million_tokens", "status"),
	"service_lease_worker_offers": moneyColumns([]string{"quote", "admission"},
		"runtime_profile_id", "region", "maximum_warm_replicas", "available_warm_replicas",
		"supplier_nanos_per_replica_hour", "residency_nanos_per_replica_hour", "status"),
	"tasks": moneyColumns([]string{"charge", "settlement", "liability", "payout"},
		"economic_buyer_charge_usd", "economic_supplier_payout_usd",
		"economic_buyer_charge_nanos", "economic_supplier_payout_nanos"),
	"suppliers":               moneyColumns([]string{"payout"}, "payouts_enabled"),
	"verification_work_plans": moneyColumns([]string{"settlement", "liability", "payout"}, "settlement_json"),
	"workers":                 moneyColumns([]string{"admission", "payout"}, "min_payout_usd_hr"),
}

// dormantNonLiveMoneyMutationScopes records reviewed schema/code paths that
// are deliberately not current money authority. They stay in the semantic
// inclusion ledger because their tables/fields have financial meaning, but
// must not be counted as observed until a separately reviewed lifecycle makes
// the writer reachable from a process, HTTP, webhook, job, or CLI root.
//
// This is not a permissive allowlist: TestMoneyMutationTableScopeIsLive
// verifies both directions (the exact writer remains present and unreachable;
// any new writer or entrypoint reachability fails) and reports it as a
// shippability gap rather than silently treating it as non-financial data.
type dormantNonLiveMoneyMutationScope struct {
	Writers []string
	Reason  string
}

var dormantNonLiveMoneyMutationScopes = map[string]dormantNonLiveMoneyMutationScope{
	"job_cost_settlements": {
		Writers: []string{"Store.PersistCostSettlementActuals"},
		Reason:  "exact storage/egress settlement evidence has a sole dormant writer and no non-test lifecycle entrypoint; wiring it into settlement requires a separately reviewed lifecycle change",
	},
}

// moneyAuthoritySchemaExclusions is the other half of the schema-column
// census.  These fields have money-adjacent vocabulary but are explicitly
// observations, measurements, or labels and are never read to decide quote,
// admission, reserve, charge, refund, settlement, liability, contribution,
// payout, or spend authority.  A new candidate column must enter either this
// reviewed exclusion ledger or moneyMutationScopes; the test below refuses an
// unclassified third state.
var moneyAuthoritySchemaExclusions = map[string]string{
	"execution_overhead_actuals.avoided_estimate_usd":      "observed overhead outcome only; pricing and admission may not consume it",
	"execution_overhead_actuals.measured_supplier_usd":     "observed overhead outcome only; pricing and admission may not consume it",
	"fabric_collective_measurements.p50_round_trip_micros": "latency measurement, not currency",
	"fabric_collective_measurements.p95_round_trip_micros": "latency measurement, not currency",
	"fabric_link_measurements.p50_round_trip_micros":       "latency measurement, not currency",
	"fabric_link_measurements.p95_round_trip_micros":       "latency measurement, not currency",
	"plan_actuals.*":                                       "observed-only learner inputs; schema forbids money/reserve/pricing/admission reads",
	"realtime_offer_samples.*":                             "bounded operational evidence, never a price/capacity/settlement authority",
	"realtime_supplier_outcome_stats.refund_count":         "aggregate outcome statistic, not a refund decision",
	"realtime_supplier_outcome_stats.verified_settlements": "derived reputation aggregate; it does not set quote, reserve, charge, payable, or payout amounts",
	"runtime_shadow_selections.cost_hw_class":              "shadow-routing label, not a settled cost",
	"service_lease_offer_samples.*":                        "bounded operational evidence, never a price/capacity/settlement authority",
	"benchmark_results.claimed_rate":                       "uncorroborated performance measurement, never an exact money/rate authority",
}

// This name is intentionally absent rather than silently forgotten. Tasks
// inherit the frozen currency semantics from their job economic plan and carry
// exact USD/nano amounts. Jobs have a generic currency field and are scoped
// above. If tasks grows one, it must move into the scope or exclusion ledger.
var moneyAuthorityAbsentFieldAdjudications = map[string]string{
	"tasks.currency": "tasks has no currency column; frozen economic USD/nano pairs inherit their plan currency",
}

// sqlMutationTargetRE finds the operation prefix. The qualified target itself
// is parsed separately so a non-public schema (or quoted identifier) cannot
// make us mistake the schema name for the table name.
var sqlMutationTargetRE = regexp.MustCompile(
	`(?is)\b(insert\s+into|update|delete\s+from|truncate(?:\s+table)?|merge\s+into)\s+(?:only\s+)?`,
)
var sqlMutationVerbRE = regexp.MustCompile(`(?is)\b(insert|update|delete|truncate|merge|call)\b`)
var sqlProcedureCallRE = regexp.MustCompile(`(?is)\bcall\s+(?:"?[a-z_][a-z0-9_]*"?\.)?"?[a-z_][a-z0-9_]*"?\s*\(`)
var sqlRoutineCallRE = regexp.MustCompile(`(?is)\b(?:"?[a-z_][a-z0-9_]*"?\.)*"?([a-z_][a-z0-9_]*)"?\s*\(`)
var sqlLineCommentRE = regexp.MustCompile(`(?m)--[^\n]*`)
var sqlBlockCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)
var schemaExecuteFormatRE = regexp.MustCompile(`(?is)\bexecute\s+format\s*\(\s*'((?:''|[^'])*)'`)
var schemaExecuteFunctionRE = regexp.MustCompile(`(?is)\bexecute\s+function\b`)
var schemaExecuteRE = regexp.MustCompile(`(?is)\bexecute\b`)

var sqlUpdateSetRE = regexp.MustCompile(`(?is)\bset\s+`)
var sqlUpdateAssignmentColumnRE = regexp.MustCompile(`(?is)^\s*(?:"?[a-z_][a-z0-9_]*"?\.)?"?([a-z_][a-z0-9_]*)"?\s*=`)

var providerMoneyEndpointFragments = []string{
	"payment_intents", "refunds", "transfers", "payouts", "charges", "disputes",
}

// PostgreSQL built-ins used by the repository's static query corpus. Unknown
// routines in a SELECT/CTE are refused: a SELECT can invoke a user-defined
// function with a ledger write, so verb-only SQL classification is not enough.
// Additions require an explicit review of the routine's side effects rather
// than assuming every parenthesized SELECT expression is read-only.
var reviewedReadOnlySQLRoutines = map[string]bool{
	"array_agg":             true,
	"array_length":          true,
	"abs":                   true,
	"avg":                   true,
	"bool_or":               true,
	"btrim":                 true,
	"clock_timestamp":       true,
	"coalesce":              true,
	"count":                 true,
	"current_setting":       true,
	"date_trunc":            true,
	"digest":                true,
	"encode":                true,
	"exists":                true,
	"extract":               true,
	"gen_random_uuid":       true,
	"greatest":              true,
	"hashtextextended":      true,
	"jsonb_agg":             true,
	"jsonb_array_elements":  true,
	"jsonb_build_object":    true,
	"jsonb_each":            true,
	"jsonb_object_agg":      true,
	"jsonb_to_recordset":    true,
	"least":                 true,
	"lower":                 true,
	"make_interval":         true,
	"max":                   true,
	"min":                   true,
	"nextval":               true,
	"now":                   true,
	"nullif":                true,
	"pg_advisory_lock":      true,
	"pg_advisory_unlock":    true,
	"pg_advisory_xact_lock": true,
	"pg_try_advisory_lock":  true,
	"percentile_cont":       true,
	"percentile_disc":       true,
	"row_number":            true,
	"set_config":            true,
	"sha256":                true,
	"string_agg":            true,
	"split_part":            true,
	"sum":                   true,
	"to_jsonb":              true,
	"trim":                  true,
	"uuid_generate_v4":      true,
}

// reviewedStripePOSTOperations is the finite operation ledger for the one
// production Stripe POST adapter.  The runtime classifier in
// stripePOSTOperation refuses any path outside this set; the tests below
// reconcile this list with both that classifier and every direct stripeForm
// caller.  It is intentionally broader than the cash-only URL fragments:
// creating a customer, account, or account link changes provider authority and
// must not become an unobserved POST merely because it does not transfer money
// in the same request.
var reviewedStripePOSTOperations = map[string]string{
	"account_links":   "setup",
	"accounts":        "setup",
	"customers":       "setup",
	"payment_intents": "charge",
	"refunds":         "refund",
	"setup_intents":   "setup",
}

func staticStringLiterals(expr ast.Expr) []string {
	var out []string
	ast.Inspect(expr, func(node ast.Node) bool {
		lit, ok := node.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err == nil {
			out = append(out, value)
		}
		return true
	})
	return out
}

// directStaticStringLiterals follows only direct string construction
// (including concatenation), not nested maps or format arguments.  Provider
// endpoint classification must not mistake a form key such as
// capabilities[transfers][requested] for an actual /transfers request.
func directStaticStringLiterals(expr ast.Expr) []string {
	switch node := expr.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return nil
		}
		value, err := strconv.Unquote(node.Value)
		if err != nil {
			return nil
		}
		return []string{value}
	case *ast.BinaryExpr:
		if node.Op != token.ADD {
			return nil
		}
		return append(directStaticStringLiterals(node.X), directStaticStringLiterals(node.Y)...)
	case *ast.ParenExpr:
		return directStaticStringLiterals(node.X)
	default:
		return nil
	}
}

// staticStringExpression returns one exact literal value only when the AST
// proves the whole expression is static (including literal concatenation).
// Unlike staticStringLiterals it will not look through fmt.Sprintf, maps, or a
// variable: those are precisely the dynamic forms that need an exception.
func staticStringExpression(expr ast.Expr) (string, bool) {
	return staticStringExpressionWithResolver(expr, nil, map[string]bool{})
}

func staticStringExpressionWithResolver(expr ast.Expr, expressions map[string]ast.Expr, seen map[string]bool) (string, bool) {
	switch node := expr.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(node.Value)
		return value, err == nil
	case *ast.BinaryExpr:
		if node.Op != token.ADD {
			return "", false
		}
		left, leftOK := staticStringExpressionWithResolver(node.X, expressions, seen)
		right, rightOK := staticStringExpressionWithResolver(node.Y, expressions, seen)
		return left + right, leftOK && rightOK
	case *ast.ParenExpr:
		return staticStringExpressionWithResolver(node.X, expressions, seen)
	case *ast.Ident:
		if expressions == nil || seen[node.Name] || expressions[node.Name] == nil {
			return "", false
		}
		seen[node.Name] = true
		value, ok := staticStringExpressionWithResolver(expressions[node.Name], expressions, seen)
		delete(seen, node.Name)
		return value, ok
	default:
		return "", false
	}
}

func isSQLExecutionCall(call *ast.CallExpr) bool {
	switch callExprFuncName(call.Fun) {
	case "Exec", "ExecContext", "Query", "QueryContext", "QueryRow", "QueryRowContext":
		return true
	default:
		return false
	}
}

// isBatchQueueCall recognizes pgx.Batch.Queue's SQL carrier shape. We do not
// rely on a receiver type name here: a same-shaped Queue with a mutating SQL
// literal must be reviewed rather than becoming an invisible write merely
// because the parser lacks type information.
func isBatchQueueCall(call *ast.CallExpr) bool {
	return callExprFuncName(call.Fun) == "Queue"
}

func expressionLooksLikeContext(expr ast.Expr) bool {
	switch node := expr.(type) {
	case *ast.Ident:
		name := strings.ToLower(node.Name)
		return name == "ctx" || name == "context" || strings.HasSuffix(name, "ctx") || strings.HasSuffix(name, "context")
	case *ast.CallExpr:
		switch callExprFuncName(node.Fun) {
		case "Context", "Background", "TODO":
			return true
		}
	case *ast.ParenExpr:
		return expressionLooksLikeContext(node.X)
	}
	return false
}

func sqlStatementArgument(call *ast.CallExpr) (ast.Expr, bool) {
	if isBatchQueueCall(call) {
		if len(call.Args) == 0 {
			return nil, false
		}
		// pgx.Batch.Queue accepts (statement, arguments...), unlike Exec's
		// conventional (ctx, statement, ...).
		return call.Args[0], true
	}
	if !isSQLExecutionCall(call) || len(call.Args) == 0 {
		return nil, false
	}
	switch callExprFuncName(call.Fun) {
	case "ExecContext", "QueryContext", "QueryRowContext":
		if len(call.Args) < 2 {
			return nil, false
		}
		return call.Args[1], true
	case "Exec", "Query", "QueryRow":
		if len(call.Args) == 1 {
			return call.Args[0], true
		}
		// pgx uses (ctx, statement, ...); database/sql uses
		// (statement, arguments...). Prefer an obviously literal statement,
		// then the context-shaped first argument, and otherwise default to the
		// stdlib form so an opaque database/sql query cannot hide behind bind 1.
		if _, static := staticStringExpression(call.Args[0]); static {
			return call.Args[0], true
		}
		if _, static := staticStringExpression(call.Args[1]); static {
			return call.Args[1], true
		}
		if expressionLooksLikeContext(call.Args[0]) {
			return call.Args[1], true
		}
		return call.Args[0], true
	default:
		return nil, false
	}
}

func hasMoneyEndpointFragment(values []string) bool {
	for _, value := range values {
		value = strings.ToLower(value)
		for _, fragment := range providerMoneyEndpointFragments {
			if strings.Contains(value, fragment) {
				return true
			}
		}
	}
	return false
}

func isStripeProviderEndpoint(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSuffix(parsed.Hostname(), "."), "api.stripe.com")
}

func opaqueSQLRoutineCalls(statement string) []string {
	lower := strings.ToLower(strings.TrimSpace(statement))
	if !strings.HasPrefix(lower, "select") && !strings.HasPrefix(lower, "with") {
		return nil
	}
	// DML/CALL statements are already handled by the mutation/procedure
	// parser. Routine scanning is for queries that otherwise look read-only.
	if sqlMutationVerbRE.MatchString(lower) {
		return nil
	}
	lower = sqlLineCommentRE.ReplaceAllString(lower, "")
	lower = sqlBlockCommentRE.ReplaceAllString(lower, "")
	// These are SQL grammar forms rather than invocable routines. The regex is
	// deliberately simple; filtering the syntax tokens here keeps an unknown
	// user function such as SELECT settle_money($1) fail-closed.
	syntax := map[string]bool{
		"and": true, "any": true, "as": true, "by": true, "case": true,
		"cast": true, "filter": true, "from": true, "group": true, "in": true,
		"join": true, "lateral": true, "not": true, "on": true, "or": true,
		"over": true, "select": true, "using": true, "values": true, "when": true,
		"where": true,
	}
	seen := map[string]bool{}
	for _, match := range sqlRoutineCallRE.FindAllStringSubmatch(lower, -1) {
		if len(match) != 2 {
			continue
		}
		name := strings.ToLower(match[1])
		if syntax[name] || reviewedReadOnlySQLRoutines[name] {
			continue
		}
		seen[name] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// providerEndpointArguments identifies URL-bearing arguments without deciding
// whether the call is a POST. Keeping those two questions separate is what
// lets a variable-built HTTP method fail closed instead of being mistaken for
// a harmless non-POST merely because it is not the literal token "POST".
// The bool reports a known request-construction shape; generic adapters are
// considered only when their arguments prove a POST below.
func providerEndpointArguments(call *ast.CallExpr) ([]ast.Expr, bool) {
	switch callExprFuncName(call.Fun) {
	case "stripeForm":
		if len(call.Args) < 2 {
			return nil, false
		}
		return call.Args[1:2], true
	case "NewRequest":
		if len(call.Args) < 2 {
			return nil, false
		}
		return call.Args[1:2], true
	case "NewRequestWithContext":
		if len(call.Args) < 3 {
			return nil, false
		}
		return call.Args[2:3], true
	case "Post", "PostForm":
		if len(call.Args) < 1 {
			return nil, false
		}
		return call.Args[:1], true
	default:
		return call.Args, false
	}
}

func providerHTTPMethodState(expr ast.Expr, expressions map[string]ast.Expr) (post, known bool) {
	if value, static := staticStringExpressionWithResolver(expr, expressions, map[string]bool{}); static {
		return strings.EqualFold(value, "POST"), true
	}
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel == nil || !strings.HasPrefix(selector.Sel.Name, "Method") {
		return false, false
	}
	return selector.Sel.Name == "MethodPost", true
}

func providerPostMethodState(call *ast.CallExpr, expressions map[string]ast.Expr) (post, known bool) {
	switch callExprFuncName(call.Fun) {
	case "stripeForm", "Post", "PostForm":
		return true, true
	case "NewRequest":
		if len(call.Args) < 1 {
			return false, false
		}
		return providerHTTPMethodState(call.Args[0], expressions)
	case "NewRequestWithContext":
		if len(call.Args) < 2 {
			return false, false
		}
		return providerHTTPMethodState(call.Args[1], expressions)
	default:
		// Generic adapters are structural only when a method argument itself
		// proves POST. An arbitrary call that happens to carry a URL is not a
		// provider request. A known non-POST stays known so GET cannot become a
		// false money sink merely by mentioning a refund resource.
		sawKnown := false
		for _, arg := range call.Args {
			isPost, isKnown := providerHTTPMethodState(arg, expressions)
			if !isKnown {
				continue
			}
			sawKnown = true
			if isPost {
				return true, true
			}
		}
		return false, sawKnown
	}
}

func genericPOSTEndpointArguments(call *ast.CallExpr, expressions map[string]ast.Expr) []ast.Expr {
	for i, arg := range call.Args {
		post, known := providerHTTPMethodState(arg, expressions)
		if !known || !post {
			continue
		}
		if i+1 < len(call.Args) {
			return call.Args[i+1 : i+2]
		}
		return nil
	}
	return nil
}

// providerMoneyEndpointState returns whether a request might address a money
// provider endpoint, whether that endpoint expression is dynamic, and the
// independently-derived method state. An opaque endpoint is money-relevant by
// default: a helper-returned URL may be /v1/refunds.
func providerMoneyEndpointState(call *ast.CallExpr, expressions map[string]ast.Expr) (moneyEndpoint, dynamicEndpoint, post, methodKnown bool) {
	endpoints, knownShape := providerEndpointArguments(call)
	post, methodKnown = providerPostMethodState(call, expressions)
	if !knownShape && !post {
		return false, false, post, methodKnown
	}
	if !knownShape {
		// A generic adapter with method/path/body arguments must not treat its
		// opaque request body as an opaque endpoint. The argument immediately
		// following the proven POST method is the only URL carrier we accept.
		endpoints = genericPOSTEndpointArguments(call, expressions)
		if len(endpoints) == 0 {
			return false, false, post, methodKnown
		}
	}
	// stripeForm is the canonical provider adapter. Its runtime operation
	// classifier is an exhaustive refusal boundary, but every use remains a
	// provider authority operation (including setup rails) for this structural
	// census. A newly added adapter operation cannot hide behind an abbreviated
	// URL-fragment vocabulary.
	if callExprFuncName(call.Fun) == "stripeForm" {
		moneyEndpoint = true
	}
	for _, endpoint := range endpoints {
		if value, static := staticStringExpressionWithResolver(endpoint, expressions, map[string]bool{}); static {
			if hasMoneyEndpointFragment([]string{value}) || isStripeProviderEndpoint(value) {
				moneyEndpoint = true
			}
			continue
		}
		fragments := staticStringLiterals(endpoint)
		if len(fragments) == 0 || hasMoneyEndpointFragment(fragments) {
			moneyEndpoint = true
			dynamicEndpoint = true
		}
	}
	return moneyEndpoint, dynamicEndpoint, post, methodKnown
}

func unresolvedMoneyExpressionKinds(call *ast.CallExpr) []string {
	return unresolvedMoneyExpressionKindsWithResolver(call, nil)
}

func unresolvedMoneyExpressionKindsWithResolver(call *ast.CallExpr, expressions map[string]ast.Expr) []string {
	issues := map[string]bool{}
	if statement, ok := sqlStatementArgument(call); ok {
		if sql, static := staticStringExpressionWithResolver(statement, expressions, map[string]bool{}); static {
			// A stored procedure hides its body from this AST scanner. Even a
			// literal CALL must be independently reviewed rather than treated as
			// read-only simply because there is no SQL table target at the site.
			if sqlProcedureCallRE.MatchString(sql) {
				issues["opaque-db-procedure"] = true
			}
			for _, routine := range opaqueSQLRoutineCalls(sql) {
				issues["opaque-db-routine:"+routine] = true
			}
		} else {
			fragments := staticStringLiterals(statement)
			// A dynamic statement whose available source fragments contain a
			// mutation verb can hide its target or columns. A completely opaque
			// SQL execution call is also refused, including Query/QueryRow: the
			// scanner cannot prove a runtime string remains read-only.
			mutatingFragment := false
			for _, fragment := range fragments {
				if sqlMutationVerbRE.MatchString(strings.ToLower(fragment)) {
					mutatingFragment = true
					break
				}
			}
			// A Batch.Queue carries arbitrary SQL and its result is consumed later
			// through BatchResults.Exec. Any non-static Queue is therefore refused
			// outright: even a visible SELECT fragment could conceal a second
			// mutation or be rebuilt before enqueueing.
			if isBatchQueueCall(call) || mutatingFragment || (len(fragments) == 0 && isSQLExecutionCall(call)) {
				issues["dynamic-db-statement"] = true
			}
		}
	}
	if callExprFuncName(call.Fun) == "CopyFrom" {
		if len(call.Args) < 3 {
			issues["dynamic-copyfrom"] = true
		} else if _, targetOK := pgxCopyFromTarget(call.Args[1]); !targetOK {
			issues["dynamic-copyfrom"] = true
		} else if _, columnsOK := staticStringSlice(call.Args[2]); !columnsOK {
			issues["dynamic-copyfrom"] = true
		}
	}
	if moneyEndpoint, dynamicEndpoint, post, methodKnown := providerMoneyEndpointState(call, expressions); moneyEndpoint {
		// A money URL paired with a non-literal method is a mutating-provider
		// risk until a review pins the method source. Do not let a variable such
		// as method := os.Getenv("METHOD") turn a refund request into a silent
		// non-sink.
		if !methodKnown {
			issues["dynamic-provider-money-method"] = true
		}
		if post && dynamicEndpoint {
			issues["dynamic-provider-money-endpoint"] = true
		}
	}
	out := make([]string, 0, len(issues))
	for kind := range issues {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

// moneyMutationEffect is one parsed direct persistence effect.  UnknownColumns
// means the target table was known but static SQL did not expose a column list;
// that is deliberately fail-closed for a mixed table rather than a bypass.
type moneyMutationEffect struct {
	Table   string
	Columns map[string]bool
	// WholeRow distinguishes a row-level mutation (DELETE, or a mutation of a
	// one-purpose financial ledger) from a precise mixed-table field update.
	// It lets a reviewed dynamic SQL collection prove that it deletes a billing
	// record rather than claiming it only updated one selected column.
	WholeRow       bool
	UnknownColumns bool
}

func mergeMoneyMutationEffect(dst map[string]moneyMutationEffect, effect moneyMutationEffect) {
	prior, exists := dst[effect.Table]
	if !exists {
		prior = moneyMutationEffect{Table: effect.Table, Columns: map[string]bool{}}
	}
	if prior.Columns == nil {
		prior.Columns = map[string]bool{}
	}
	for column := range effect.Columns {
		prior.Columns[column] = true
	}
	prior.WholeRow = prior.WholeRow || effect.WholeRow
	prior.UnknownColumns = prior.UnknownColumns || effect.UnknownColumns
	dst[effect.Table] = prior
}

// addMoneyMutationEffect records one already-classified persistence effect in
// the structural census. Both literal SQL/CopyFrom effects and the small,
// exact reviewed dynamic-operation ledger use this path, so an exception
// cannot make a real financial write disappear from sink reachability or
// column-provenance coverage.
func addMoneyMutationEffect(graph *callGraph, from string, effect moneyMutationEffect) {
	if effect.Table == "" {
		return
	}
	graph.structuralMoneySinks[from] = true
	graph.moneyMutationTableHits[effect.Table] = true
	if graph.moneyMutationColumnHits[effect.Table] == nil {
		graph.moneyMutationColumnHits[effect.Table] = map[string]bool{}
	}
	for column := range effect.Columns {
		graph.moneyMutationColumnHits[effect.Table][column] = true
	}
	if graph.moneyMutationEffectsByFunction == nil {
		graph.moneyMutationEffectsByFunction = map[string]map[string]moneyMutationEffect{}
	}
	if graph.moneyMutationEffectsByFunction[from] == nil {
		graph.moneyMutationEffectsByFunction[from] = map[string]moneyMutationEffect{}
	}
	mergeMoneyMutationEffect(graph.moneyMutationEffectsByFunction[from], effect)
}

func liveMoneyMutationEffectHits(graph *callGraph) (tables map[string]bool, columns map[string]map[string]bool) {
	tables = map[string]bool{}
	columns = map[string]map[string]bool{}
	for from := range graph.authorityReachableFrom(graph.entrypoints) {
		for table, effect := range graph.moneyMutationEffectsByFunction[from] {
			tables[table] = true
			if columns[table] == nil {
				columns[table] = map[string]bool{}
			}
			for column := range effect.Columns {
				columns[table][column] = true
			}
		}
	}
	return tables, columns
}

func sqlIdentifierList(raw string) (map[string]bool, bool) {
	columns := map[string]bool{}
	for _, rawColumn := range strings.Split(raw, ",") {
		column := strings.TrimSpace(strings.Trim(rawColumn, `"`))
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(column) {
			return nil, false
		}
		columns[strings.ToLower(column)] = true
	}
	return columns, len(columns) > 0
}

func sqlQualifiedIdentifierAt(sql string, start int) (table string, end int, ok bool) {
	for start < len(sql) && (sql[start] == ' ' || sql[start] == '\n' || sql[start] == '\t' || sql[start] == '\r') {
		start++
	}
	parts := make([]string, 0, 2)
	for {
		if start >= len(sql) {
			return "", start, false
		}
		quoted := sql[start] == '"'
		if quoted {
			start++
		}
		segmentStart := start
		for start < len(sql) {
			b := sql[start]
			if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' {
				start++
				continue
			}
			break
		}
		if start == segmentStart {
			return "", start, false
		}
		segment := sql[segmentStart:start]
		if quoted {
			if start >= len(sql) || sql[start] != '"' {
				return "", start, false
			}
			start++
		}
		parts = append(parts, strings.ToLower(segment))
		if start >= len(sql) || sql[start] != '.' {
			break
		}
		start++
	}
	return parts[len(parts)-1], start, true
}

func sqlWordBoundary(text string, start, end int) bool {
	if start > 0 {
		b := text[start-1]
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' {
			return false
		}
	}
	if end < len(text) {
		b := text[end]
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' {
			return false
		}
	}
	return true
}

// topLevelSQLClauseEnd finds the first UPDATE-clause boundary while ignoring
// quoted text and subqueries. It is intentionally narrow, but prevents a
// WHERE charge_status=... reference from being mistaken for SET charge_status.
func topLevelSQLClauseEnd(text string) int {
	depth := 0
	var quote byte
	for i := 0; i < len(text); i++ {
		b := text[i]
		if quote != 0 {
			if b == quote {
				if i+1 < len(text) && text[i+1] == quote {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		switch b {
		case '\'', '"':
			quote = b
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if depth != 0 {
			continue
		}
		for _, keyword := range []string{"where", "returning", "from"} {
			if strings.HasPrefix(text[i:], keyword) && sqlWordBoundary(text, i, i+len(keyword)) {
				return i
			}
		}
	}
	return len(text)
}

func splitTopLevelSQLAssignments(text string) []string {
	var out []string
	start, depth := 0, 0
	var quote byte
	for i := 0; i < len(text); i++ {
		b := text[i]
		if quote != 0 {
			if b == quote {
				if i+1 < len(text) && text[i+1] == quote {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		switch b {
		case '\'', '"':
			quote = b
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, text[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, text[start:])
	return out
}

// sqlMutationColumns returns the directly mutated columns for a parsed target.
// It intentionally returns known=false for valid but non-literal forms (for
// example INSERT ... SELECT): the caller treats that as authority on a mixed
// table, because accepting an unparsed write would reopen the generic-writer
// bypass this guard exists to close.
func sqlMutationColumns(sql, operation string, targetEnd int) (columns map[string]bool, known bool) {
	tail := sql[targetEnd:]
	switch {
	case strings.HasPrefix(operation, "delete"), strings.HasPrefix(operation, "truncate"):
		return nil, true
	case strings.HasPrefix(operation, "insert"):
		tail = strings.TrimSpace(tail)
		if !strings.HasPrefix(tail, "(") {
			return nil, false
		}
		closeAt := strings.Index(tail, ")")
		if closeAt <= 1 {
			return nil, false
		}
		return sqlIdentifierList(tail[1:closeAt])
	case strings.HasPrefix(operation, "update"):
		setAt := sqlUpdateSetRE.FindStringIndex(tail)
		if setAt == nil {
			return nil, false
		}
		setClause := tail[setAt[1]:]
		setClause = setClause[:topLevelSQLClauseEnd(setClause)]
		columns := map[string]bool{}
		for _, assignment := range splitTopLevelSQLAssignments(setClause) {
			match := sqlUpdateAssignmentColumnRE.FindStringSubmatch(assignment)
			if len(match) != 2 {
				return nil, false
			}
			columns[strings.ToLower(match[1])] = true
		}
		return columns, len(columns) > 0
	default:
		return nil, false
	}
}

func moneyMutationEffectForTarget(table, operation string, columns map[string]bool, knownColumns bool) (moneyMutationEffect, bool) {
	scope, scoped := moneyMutationScopes[table]
	if !scoped {
		return moneyMutationEffect{}, false
	}
	effect := moneyMutationEffect{Table: table, Columns: map[string]bool{}}
	if scope.WholeRow || strings.HasPrefix(operation, "delete") || strings.HasPrefix(operation, "truncate") {
		effect.WholeRow = true
		return effect, true
	}
	if !knownColumns {
		effect.UnknownColumns = true
		return effect, true
	}
	for column := range columns {
		if scope.Columns[column] {
			effect.Columns[column] = true
		}
	}
	return effect, len(effect.Columns) > 0
}

func moneyMutationEffectsForSQL(sql string) []moneyMutationEffect {
	lower := strings.ToLower(sql)
	byTable := map[string]moneyMutationEffect{}
	for _, match := range sqlMutationTargetRE.FindAllStringSubmatchIndex(lower, -1) {
		if len(match) != 4 || match[2] < 0 {
			continue
		}
		operation := lower[match[2]:match[3]]
		table, targetEnd, parsed := sqlQualifiedIdentifierAt(lower, match[1])
		if !parsed {
			continue
		}
		columns, known := sqlMutationColumns(lower, operation, targetEnd)
		if effect, ok := moneyMutationEffectForTarget(table, operation, columns, known); ok {
			mergeMoneyMutationEffect(byTable, effect)
		}
	}
	return sortedMoneyMutationEffects(byTable)
}

func sortedMoneyMutationEffects(byTable map[string]moneyMutationEffect) []moneyMutationEffect {
	tables := make([]string, 0, len(byTable))
	for table := range byTable {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	out := make([]moneyMutationEffect, 0, len(tables))
	for _, table := range tables {
		out = append(out, byTable[table])
	}
	return out
}

// reviewedMoneyMutationEffects converts the exact reviewed dynamic-operation
// ledger into normal structural effects. Invalid entries deliberately produce
// no effect here; TestNoUnreviewedDynamicMoneyExpressions rejects them before
// they can become a silent source of authority coverage.
func reviewedMoneyMutationEffects(exception moneyExpressionException) []moneyMutationEffect {
	byTable := map[string]moneyMutationEffect{}
	for _, reviewed := range exception.Effects {
		table := strings.ToLower(strings.TrimSpace(reviewed.Table))
		scope, scoped := moneyMutationScopes[table]
		if !scoped {
			continue
		}
		effect := moneyMutationEffect{
			Table:    table,
			Columns:  map[string]bool{},
			WholeRow: scope.WholeRow || reviewed.WholeRow,
		}
		if effect.WholeRow {
			mergeMoneyMutationEffect(byTable, effect)
			continue
		}
		for _, reviewedColumn := range reviewed.Columns {
			column := strings.ToLower(strings.TrimSpace(reviewedColumn))
			if scope.Columns[column] {
				effect.Columns[column] = true
			}
		}
		if len(effect.Columns) > 0 {
			mergeMoneyMutationEffect(byTable, effect)
		}
	}
	return sortedMoneyMutationEffects(byTable)
}

// literalMoneyMutationEffectsForIssue derives persistence effects from the
// finite literal SQL collection in one exception's containing function. It is
// intentionally used only for explicitly marked finite collections: opaque
// runtime SQL remains fail-closed and cannot manufacture its own coverage.
func literalMoneyMutationEffectsForIssue(t *testing.T, issue moneyExpressionIssue) []moneyMutationEffect {
	t.Helper()
	source, err := os.ReadFile(issue.File)
	if err != nil {
		t.Fatalf("read dynamic money exception source %s: %v", issue.File, err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), issue.File, source, 0)
	if err != nil {
		t.Fatalf("parse dynamic money exception source %s: %v", issue.File, err)
	}
	var function *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Body != nil && declKey(fn) == issue.Function {
			function = fn
			break
		}
	}
	if function == nil {
		t.Fatalf("dynamic money exception %s does not name a function in %s", issue.key(), issue.File)
	}
	byTable := map[string]moneyMutationEffect{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		statement, err := strconv.Unquote(literal.Value)
		if err != nil {
			return true
		}
		for _, effect := range moneyMutationEffectsForSQL(statement) {
			mergeMoneyMutationEffect(byTable, effect)
		}
		return true
	})
	return sortedMoneyMutationEffects(byTable)
}

func sourceFunctionForIssue(t *testing.T, issue moneyExpressionIssue) (*token.FileSet, *ast.File, *ast.FuncDecl) {
	t.Helper()
	source, err := os.ReadFile(issue.File)
	if err != nil {
		t.Fatalf("read dynamic money exception source %s: %v", issue.File, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, issue.File, source, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse dynamic money exception source %s: %v", issue.File, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Body != nil && declKey(fn) == issue.Function {
			return fset, file, fn
		}
	}
	t.Fatalf("dynamic money exception %s does not name a function in %s", issue.key(), issue.File)
	return nil, nil, nil
}

func mergeMoneyMutationEffectsFromSQL(byTable map[string]moneyMutationEffect, statements ...string) {
	for _, statement := range statements {
		for _, effect := range moneyMutationEffectsForSQL(statement) {
			mergeMoneyMutationEffect(byTable, effect)
		}
	}
}

// embeddedSchemaMoneyMutationEffects derives the financial DML embedded in a
// schema migration and rejects any dynamic SQL carrier that could hide a DML
// target. The current schema has only format-built ALTER TABLE statements for
// idempotent constraint upgrades; any new EXECUTE expression must be reviewed
// here before it can enter the migration authority path.
func embeddedSchemaMoneyMutationEffects(t *testing.T, artifact string) []moneyMutationEffect {
	t.Helper()
	withoutComments := sqlBlockCommentRE.ReplaceAllString(sqlLineCommentRE.ReplaceAllString(artifact, ""), "")
	withoutTriggerCalls := schemaExecuteFunctionRE.ReplaceAllString(withoutComments, "")
	formats := schemaExecuteFormatRE.FindAllStringSubmatch(withoutTriggerCalls, -1)
	withoutFormats := schemaExecuteFormatRE.ReplaceAllString(withoutTriggerCalls, "")
	if schemaExecuteRE.MatchString(withoutFormats) {
		t.Fatal("embedded schema has an unreviewed dynamic EXECUTE carrier; derive its financial DML or reject it")
	}
	for _, match := range formats {
		if len(match) != 2 {
			t.Fatal("embedded schema dynamic EXECUTE format could not be parsed")
		}
		format := strings.TrimSpace(strings.ToLower(strings.ReplaceAll(match[1], "''", "'")))
		if !strings.HasPrefix(format, "alter table ") || sqlMutationVerbRE.MatchString(format) || sqlProcedureCallRE.MatchString(format) {
			t.Fatalf("embedded schema dynamic EXECUTE is not reviewed DDL: %q", format)
		}
	}
	byTable := map[string]moneyMutationEffect{}
	mergeMoneyMutationEffectsFromSQL(byTable, artifact)
	return sortedMoneyMutationEffects(byTable)
}

func staticSQLLiteralsInFunction(fn *ast.FuncDecl) []string {
	var statements []string
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		statement, err := strconv.Unquote(literal.Value)
		if err != nil {
			return true
		}
		lower := strings.ToLower(strings.TrimSpace(statement))
		if strings.HasPrefix(lower, "select") || strings.HasPrefix(lower, "with") {
			statements = append(statements, statement)
		}
		return true
	})
	return statements
}

func stringLiteralsInFunction(fn *ast.FuncDecl) []string {
	var values []string
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil {
			values = append(values, value)
		}
		return true
	})
	return values
}

func isReadOnlySQLStatement(statement string) bool {
	lower := strings.ToLower(strings.TrimSpace(statement))
	if !strings.HasPrefix(lower, "select") && !strings.HasPrefix(lower, "with") {
		return false
	}
	return !sqlMutationVerbRE.MatchString(lower) && !sqlProcedureCallRE.MatchString(lower) && len(opaqueSQLRoutineCalls(statement)) == 0
}

func functionHasExplicitReadOnlyTransaction(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || callExprFuncName(call.Fun) != "BeginTx" {
			return true
		}
		for _, arg := range call.Args {
			literal, ok := arg.(*ast.CompositeLit)
			if !ok || typeNameFromExpr(literal.Type) != "TxOptions" {
				continue
			}
			for _, element := range literal.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				name, ok := field.Key.(*ast.Ident)
				if !ok || name.Name != "AccessMode" {
					continue
				}
				if selector, ok := field.Value.(*ast.SelectorExpr); ok && selector.Sel.Name == "ReadOnly" {
					found = true
				}
			}
		}
		return true
	})
	return found
}

func authorityCallers(graph *callGraph, target string) []string {
	callers := map[string]bool{}
	for from, targets := range graph.authorityEdges {
		if targets[target] {
			callers[from] = true
		}
	}
	out := make([]string, 0, len(callers))
	for caller := range callers {
		out = append(out, caller)
	}
	sort.Strings(out)
	return out
}

func functionByKey(t *testing.T, fileName, key string) (*token.FileSet, *ast.FuncDecl) {
	t.Helper()
	source, err := os.ReadFile(fileName)
	if err != nil {
		t.Fatalf("read %s: %v", fileName, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, fileName, source, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", fileName, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Body != nil && declKey(fn) == key {
			return fset, fn
		}
	}
	t.Fatalf("function %s not found in %s", key, fileName)
	return nil, nil
}

func directCallsNamed(fn *ast.FuncDecl, name string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && callExprFuncName(call.Fun) == name {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

func staticPackageStringExpressions(t *testing.T) map[string]ast.Expr {
	t.Helper()
	entries, err := filepath.Glob("*.go")
	must(t, err)
	files := map[string]*ast.File{}
	fset := token.NewFileSet()
	for _, name := range entries {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		mustf(t, err, "parse %s: %v", name, err)
		files[name] = file
	}
	return packageStaticStringExpressions(files)
}

func exactStaticStringSet(t *testing.T, values []string) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	for _, value := range values {
		if value == "" || set[value] {
			t.Fatalf("non-exact static string artifact set: %q", value)
		}
		set[value] = true
	}
	return set
}

func compareStaticStringSets(t *testing.T, label string, got, want map[string]bool) {
	t.Helper()
	var missing, extra []string
	for value := range want {
		if !got[value] {
			missing = append(missing, value)
		}
	}
	for value := range got {
		if !want[value] {
			extra = append(extra, value)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("%s static artifact set drift: missing=%v extra=%v", label, missing, extra)
	}
}

// dynamicDBEvidenceEffects proves each allowed dynamic database carrier from
// its actual source artifact. It returns the derived structural money effects;
// the caller compares them to the exception ledger so a proof cannot merely
// describe its own effects in prose.
func dynamicDBEvidenceEffects(t *testing.T, graph *callGraph, issue moneyExpressionIssue, evidence dynamicDBEvidence) []moneyMutationEffect {
	t.Helper()
	if strings.TrimSpace(evidence.Mode) == "" || strings.TrimSpace(evidence.Artifact) == "" {
		t.Fatalf("dynamic database exception %s lacks artifact-bound evidence", issue.key())
	}
	byTable := map[string]moneyMutationEffect{}
	switch evidence.Mode {
	case "literal-collection":
		return literalMoneyMutationEffectsForIssue(t, issue)

	case "readonly-dsar":
		callers := authorityCallers(graph, issue.Function)
		if got, want := strings.Join(callers, ","), "Store.ExportBuyerData"; got != want {
			t.Fatalf("DSAR dynamic query wrapper callers=%q want %q", got, want)
		}
		_, caller := functionByKey(t, "data_governance.go", "Store.ExportBuyerData")
		if !functionHasExplicitReadOnlyTransaction(caller) {
			t.Fatal("DSAR query wrapper caller lacks explicit pgx read-only transaction")
		}
		calls := directCallsNamed(caller, "queryJSON")
		if len(calls) != 1 || len(calls[0].Args) < 3 {
			t.Fatalf("DSAR must have exactly one finite queryJSON call, got %d", len(calls))
		}
		queryField, ok := calls[0].Args[2].(*ast.SelectorExpr)
		if !ok || queryField.Sel.Name != "query" {
			t.Fatal("DSAR queryJSON must consume the finite item.query collection, not an arbitrary expression")
		}
		statements := staticSQLLiteralsInFunction(caller)
		if len(statements) == 0 {
			t.Fatal("DSAR has no finite static SELECT collection")
		}
		for _, statement := range statements {
			if !isReadOnlySQLStatement(statement) {
				t.Fatalf("DSAR static query is not read-only: %q", statement)
			}
			mergeMoneyMutationEffectsFromSQL(byTable, statement)
		}

	case "bounded-realtime-offer":
		_, _, fn := sourceFunctionForIssue(t, issue)
		resolver := staticPackageStringExpressions(t)
		calls := directCallsNamed(fn, "scanOffer")
		if len(calls) == 0 {
			t.Fatal("realtime offer selector has no finite scanOffer callers")
		}
		wantNames := exactStaticStringSet(t, []string{"realtimeAuthorizeSelectOfferSQLBlocking", "realtimeAuthorizeSelectOfferSQLSkip"})
		gotNames := map[string]bool{}
		for _, call := range calls {
			if len(call.Args) != 1 {
				t.Fatal("realtime offer selector call has non-finite arguments")
			}
			name, ok := call.Args[0].(*ast.Ident)
			if !ok {
				t.Fatal("realtime offer selector accepts a non-static SQL artifact")
			}
			statement, ok := staticStringExpressionWithResolver(name, resolver, map[string]bool{})
			if !ok {
				t.Fatalf("realtime offer SQL artifact %s is not static", name.Name)
			}
			gotNames[name.Name] = true
			mergeMoneyMutationEffectsFromSQL(byTable, statement)
		}
		compareStaticStringSets(t, "realtime offer SQL", gotNames, wantNames)

	case "runtime-profile-version-template":
		_, _, fn := sourceFunctionForIssue(t, issue)
		foundTemplate, foundVersionConst := false, false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || callExprFuncName(call.Fun) != "Sprint" || len(call.Args) != 1 {
				return true
			}
			if ident, ok := call.Args[0].(*ast.Ident); ok && ident.Name == "capabilityManifestVersion" {
				foundVersionConst = true
			}
			return true
		})
		for _, literal := range stringLiteralsInFunction(fn) {
			if strings.Contains(strings.ToLower(literal), "insert into runtime_profiles") {
				foundTemplate = true
				mergeMoneyMutationEffectsFromSQL(byTable, literal)
			}
		}
		if !foundTemplate || !foundVersionConst {
			t.Fatal("runtime profile dynamic SQL must remain the static runtime_profiles template plus capabilityManifestVersion")
		}
		source, err := os.ReadFile(evidence.Artifact)
		if err != nil || !regexp.MustCompile(`(?m)^const\s+capabilityManifestVersion\s*=\s*[0-9]+\s*$`).Match(source) {
			t.Fatalf("runtime profile SQL version source %s is not a literal constant", evidence.Artifact)
		}

	case "runtime-profile-child-list":
		_, _, fn := sourceFunctionForIssue(t, issue)
		want := exactStaticStringSet(t, []string{"runtime_profile_models", "runtime_profile_hardware", "runtime_profile_capabilities"})
		got := map[string]bool{}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			expr, ok := node.(ast.Expr)
			if !ok {
				return true
			}
			values, ok := staticStringSlice(expr)
			if !ok {
				return true
			}
			candidate := map[string]bool{}
			for _, value := range values {
				candidate[value] = true
			}
			if len(candidate) == len(want) {
				matches := true
				for value := range want {
					if !candidate[value] {
						matches = false
					}
				}
				if matches {
					got = candidate
				}
			}
			return true
		})
		compareStaticStringSets(t, "runtime profile child-table", got, want)
		for table := range got {
			mergeMoneyMutationEffectsFromSQL(byTable, "DELETE FROM "+table+" WHERE runtime_profile_id = $1 AND revision = $2")
		}

	case "bounded-claim-builder":
		_, _, fn := sourceFunctionForIssue(t, issue)
		want := exactStaticStringSet(t, []string{"t.claimed_by = $1 AND t.started_at IS NULL", "t.claimed_by IS NULL"})
		got := map[string]bool{}
		for _, call := range directCallsNamed(fn, "scanClaim") {
			if len(call.Args) != 1 {
				t.Fatal("claim SQL builder accepts a non-finite argument")
			}
			predicate, ok := staticStringExpression(call.Args[0])
			if !ok {
				t.Fatal("claim SQL builder predicate is dynamic")
			}
			got[predicate] = true
			mergeMoneyMutationEffectsFromSQL(byTable, ClaimTaskSQL(predicate))
		}
		compareStaticStringSets(t, "claim SQL predicates", got, want)

	case "embedded-schema":
		fset, _, fn := sourceFunctionForIssue(t, issue)
		foundCarrier := false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || fset.Position(call.Pos()).Line != issue.Line {
				return true
			}
			statement, ok := sqlStatementArgument(call)
			if !ok {
				return true
			}
			ident, ok := statement.(*ast.Ident)
			foundCarrier = ok && ident.Name == "canonicalSchema"
			return true
		})
		if !foundCarrier {
			t.Fatal("migration dynamic SQL carrier is no longer the exact canonicalSchema embed")
		}
		source, err := os.ReadFile(issue.File)
		if err != nil {
			t.Fatalf("read migration source: %v", err)
		}
		if !strings.Contains(string(source), "//go:embed "+evidence.Artifact) {
			t.Fatalf("migration source no longer embeds %s", evidence.Artifact)
		}
		artifact, err := os.ReadFile(evidence.Artifact)
		if err != nil || len(strings.TrimSpace(string(artifact))) == 0 {
			t.Fatalf("embedded schema artifact %s is unavailable or empty: %v", evidence.Artifact, err)
		}
		// canonicalSchema is executed as one dynamic carrier. Its tracked body is
		// nevertheless finite, so derive every scoped DML effect from that exact
		// embedded artifact and compare it with the exception ledger below. This
		// turns a new financial migration statement into an observed Store.Migrate
		// sink instead of letting an opaque schema payload silently bypass the
		// authority census.
		for _, effect := range embeddedSchemaMoneyMutationEffects(t, string(artifact)) {
			mergeMoneyMutationEffect(byTable, effect)
		}

	case "readonly-duration-template":
		fset, _, fn := sourceFunctionForIssue(t, issue)
		foundCarrier, foundTemplate := false, false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if fset.Position(call.Pos()).Line == issue.Line && callExprFuncName(call.Fun) == "Query" {
				statement, statementOK := sqlStatementArgument(call)
				if ident, ok := statement.(*ast.Ident); statementOK && ok && ident.Name == "query" {
					foundCarrier = true
				}
			}
			if callExprFuncName(call.Fun) == "Sprintf" && len(call.Args) > 0 {
				if format, ok := staticStringExpression(call.Args[0]); ok && isReadOnlySQLStatement(format) {
					foundTemplate = true
					mergeMoneyMutationEffectsFromSQL(byTable, format)
				}
			}
			return true
		})
		if !foundCarrier || !foundTemplate {
			t.Fatal("duration histogram dynamic SQL must remain a local SELECT template passed only to Query")
		}

	default:
		t.Fatalf("dynamic database exception %s has unknown evidence mode %q", issue.key(), evidence.Mode)
	}
	return sortedMoneyMutationEffects(byTable)
}

func moneyMutationEffectFingerprints(effects []moneyMutationEffect) []string {
	fingerprints := make([]string, 0, len(effects))
	for _, effect := range effects {
		columns := make([]string, 0, len(effect.Columns))
		for column := range effect.Columns {
			columns = append(columns, column)
		}
		sort.Strings(columns)
		fingerprints = append(fingerprints, fmt.Sprintf("%s|row=%t|unknown=%t|columns=%s",
			effect.Table, effect.WholeRow, effect.UnknownColumns, strings.Join(columns, ",")))
	}
	sort.Strings(fingerprints)
	return fingerprints
}

func compositeLiteralTypeName(expr ast.Expr) string {
	switch node := expr.(type) {
	case *ast.Ident:
		return node.Name
	case *ast.SelectorExpr:
		return node.Sel.Name
	case *ast.ParenExpr:
		return compositeLiteralTypeName(node.X)
	default:
		return ""
	}
}

func staticCompositeStringValues(expr ast.Expr) ([]string, bool) {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	values := make([]string, 0, len(lit.Elts))
	for _, element := range lit.Elts {
		if keyed, ok := element.(*ast.KeyValueExpr); ok {
			element = keyed.Value
		}
		literal, ok := element.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return nil, false
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			return nil, false
		}
		values = append(values, value)
	}
	return values, true
}

func pgxCopyFromTarget(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok || compositeLiteralTypeName(lit.Type) != "Identifier" {
		return "", false
	}
	parts, ok := staticCompositeStringValues(expr)
	if !ok || len(parts) == 0 {
		return "", false
	}
	return strings.ToLower(parts[len(parts)-1]), true
}

func staticStringSlice(expr ast.Expr) ([]string, bool) {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	array, ok := lit.Type.(*ast.ArrayType)
	if !ok || compositeLiteralTypeName(array.Elt) != "string" {
		return nil, false
	}
	return staticCompositeStringValues(expr)
}

// moneyMutationEffectsForCopyFrom detects pgx.CopyFrom by its literal target
// identifier and supplied column slice.  This is separate from SQL parsing so
// a frozen task amount bulk-write cannot evade the structural census merely
// because pgx encodes it in the wire protocol rather than an INSERT string.
func moneyMutationEffectsForCopyFrom(call *ast.CallExpr) []moneyMutationEffect {
	if callExprFuncName(call.Fun) != "CopyFrom" || len(call.Args) < 3 {
		return nil
	}
	table, ok := pgxCopyFromTarget(call.Args[1])
	if !ok {
		return nil
	}
	columns, known := staticStringSlice(call.Args[2])
	columnSet := map[string]bool{}
	if known {
		for _, column := range columns {
			columnSet[strings.ToLower(column)] = true
		}
	}
	effect, observed := moneyMutationEffectForTarget(table, "insert", columnSet, known)
	if !observed {
		return nil
	}
	return []moneyMutationEffect{effect}
}

// moneyMutationEffectsForCallWithResolver extracts effects from the one SQL
// carrier argument, resolving package-level literal constants when the caller
// has already indexed them. This keeps a static named query from being treated
// as harmless merely because it is not spelled at the call site.
func moneyMutationEffectsForCallWithResolver(call *ast.CallExpr, expressions map[string]ast.Expr) []moneyMutationEffect {
	byTable := map[string]moneyMutationEffect{}
	if statement, ok := sqlStatementArgument(call); ok {
		if sql, static := staticStringExpressionWithResolver(statement, expressions, map[string]bool{}); static {
			for _, effect := range moneyMutationEffectsForSQL(sql) {
				mergeMoneyMutationEffect(byTable, effect)
			}
		} else {
			// Preserve literal-fragment parsing for an otherwise unresolved
			// expression. The dynamic-expression gate below will still refuse it;
			// any unambiguous literal effect is conservatively observed too.
			for _, sql := range staticStringLiterals(statement) {
				for _, effect := range moneyMutationEffectsForSQL(sql) {
					mergeMoneyMutationEffect(byTable, effect)
				}
			}
		}
	}
	for _, effect := range moneyMutationEffectsForCopyFrom(call) {
		mergeMoneyMutationEffect(byTable, effect)
	}
	return sortedMoneyMutationEffects(byTable)
}

func moneyMutationEffectsForCall(call *ast.CallExpr) []moneyMutationEffect {
	return moneyMutationEffectsForCallWithResolver(call, nil)
}

func moneyMutationTablesForCall(call *ast.CallExpr) []string {
	effects := moneyMutationEffectsForCall(call)
	tables := make([]string, 0, len(effects))
	for _, effect := range effects {
		tables = append(tables, effect.Table)
	}
	return tables
}

func callMutatesMoneyTable(call *ast.CallExpr) bool {
	return len(moneyMutationTablesForCall(call)) > 0
}

func callHasPOSTMethod(call *ast.CallExpr) bool {
	post, known := providerPostMethodState(call, nil)
	return known && post
}

func callPostsToProviderMoneyEndpoint(call *ast.CallExpr) bool {
	return callPostsToProviderMoneyEndpointWithResolver(call, nil)
}

// callPostsToProviderMoneyEndpointWithResolver treats an unknown method at a
// money endpoint as a potential sink. The companion dynamic-expression guard
// then requires an exact review; this preserves fail-closed reachability while
// keeping known GET requests out of the money census.
func callPostsToProviderMoneyEndpointWithResolver(call *ast.CallExpr, expressions map[string]ast.Expr) bool {
	moneyEndpoint, _, post, methodKnown := providerMoneyEndpointState(call, expressions)
	return moneyEndpoint && (post || !methodKnown)
}

func addFunctionTargets(g *callGraph, expr ast.Expr, out map[string]bool) {
	ast.Inspect(expr, func(node ast.Node) bool {
		var name string
		switch value := node.(type) {
		case *ast.Ident:
			name = value.Name
		case *ast.SelectorExpr:
			name = value.Sel.Name
		default:
			return true
		}
		for _, key := range g.byName[name] {
			out[key] = true
		}
		return true
	})
}

func isHTTPRouteRegistration(call *ast.CallExpr) bool {
	switch callExprFuncName(call.Fun) {
	case "Handle", "HandleFunc":
		return true
	default:
		return false
	}
}

func typeCarriesExactMoney(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return exactMoneySignatureTypes[t.Name]
	case *ast.StarExpr:
		return typeCarriesExactMoney(t.X)
	case *ast.ArrayType:
		return typeCarriesExactMoney(t.Elt)
	case *ast.Ellipsis:
		return typeCarriesExactMoney(t.Elt)
	case *ast.MapType:
		return typeCarriesExactMoney(t.Key) || typeCarriesExactMoney(t.Value)
	case *ast.IndexExpr:
		return typeCarriesExactMoney(t.X) || typeCarriesExactMoney(t.Index)
	case *ast.IndexListExpr:
		if typeCarriesExactMoney(t.X) {
			return true
		}
		for _, index := range t.Indices {
			if typeCarriesExactMoney(index) {
				return true
			}
		}
	}
	return false
}

func fieldListCarriesExactMoney(fields *ast.FieldList) bool {
	if fields == nil {
		return false
	}
	for _, field := range fields.List {
		if typeCarriesExactMoney(field.Type) {
			return true
		}
	}
	return false
}

func funcDeclCarriesExactMoney(fn *ast.FuncDecl) bool {
	return fieldListCarriesExactMoney(fn.Type.Params) ||
		fieldListCarriesExactMoney(fn.Type.Results)
}

func funcDeclPrimaryConcreteResultType(fn *ast.FuncDecl) string {
	if fn.Type.Results == nil || len(fn.Type.Results.List) == 0 {
		return ""
	}
	return typeNameFromExpr(fn.Type.Results.List[0].Type)
}

// callExprFuncName returns the bare function/method name of a call's Fun, or "".
func callExprFuncName(fun ast.Expr) string {
	switch node := fun.(type) {
	case *ast.Ident:
		return node.Name
	case *ast.SelectorExpr:
		return node.Sel.Name
	case *ast.IndexExpr: // generic instantiation
		return callExprFuncName(node.X)
	case *ast.ParenExpr:
		return callExprFuncName(node.X)
	}
	return ""
}

func declKey(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
	}
	return receiverTypeName(decl.Recv.List[0].Type) + "." + decl.Name.Name
}

func receiverTypeName(expr ast.Expr) string {
	switch node := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(node.X)
	case *ast.Ident:
		return node.Name
	case *ast.IndexExpr: // generic receiver
		return receiverTypeName(node.X)
	}
	return "?"
}

type authorityResolutionContext struct {
	bindings map[string]string
}

func typeNameFromExpr(expr ast.Expr) string {
	switch node := expr.(type) {
	case *ast.StarExpr:
		return typeNameFromExpr(node.X)
	case *ast.Ident:
		return node.Name
	case *ast.SelectorExpr:
		return node.Sel.Name
	case *ast.IndexExpr:
		return typeNameFromExpr(node.X)
	case *ast.IndexListExpr:
		return typeNameFromExpr(node.X)
	case *ast.ParenExpr:
		return typeNameFromExpr(node.X)
	}
	return ""
}

func expressionConcreteType(graph *callGraph, expr ast.Expr) string {
	switch node := expr.(type) {
	case *ast.CompositeLit:
		return typeNameFromExpr(node.Type)
	case *ast.UnaryExpr:
		if node.Op == token.AND {
			return expressionConcreteType(graph, node.X)
		}
	case *ast.StarExpr:
		return expressionConcreteType(graph, node.X)
	case *ast.CallExpr:
		if targets := graph.functionsByName[callExprFuncName(node.Fun)]; len(targets) == 1 {
			return graph.functionResultTypes[targets[0]]
		}
		// A selector call whose target is not an in-package function has an
		// external result type. Preserve that fact so a later cmd.Run() does not
		// look like an untyped call to Workers.Run merely because the names
		// collide. It deliberately cannot resolve to a local method key.
		if _, external := node.Fun.(*ast.SelectorExpr); external {
			return "<external>"
		}
	case *ast.ParenExpr:
		return expressionConcreteType(graph, node.X)
	}
	return ""
}

func indexStructFields(graph *callGraph, file *ast.File) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if interfaceType, isInterface := typeSpec.Type.(*ast.InterfaceType); isInterface {
				graph.interfaceTypes[typeSpec.Name.Name] = true
				if graph.interfaceMethods[typeSpec.Name.Name] == nil {
					graph.interfaceMethods[typeSpec.Name.Name] = map[string]bool{}
				}
				for _, field := range interfaceType.Methods.List {
					for _, method := range field.Names {
						graph.interfaceMethods[typeSpec.Name.Name][method.Name] = true
					}
				}
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			if graph.structFields[typeSpec.Name.Name] == nil {
				graph.structFields[typeSpec.Name.Name] = map[string]string{}
			}
			for _, field := range structType.Fields.List {
				typeName := typeNameFromExpr(field.Type)
				if typeName == "" {
					continue
				}
				for _, name := range field.Names {
					graph.structFields[typeSpec.Name.Name][name.Name] = typeName
				}
			}
		}
	}
}

func authorityResolutionContextFor(graph *callGraph, fn *ast.FuncDecl) authorityResolutionContext {
	ctx := authorityResolutionContext{bindings: map[string]string{}}
	bindFields := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, field := range fields.List {
			typeName := typeNameFromExpr(field.Type)
			if typeName == "" {
				continue
			}
			for _, name := range field.Names {
				ctx.bindings[name.Name] = typeName
			}
		}
	}
	bindFields(fn.Recv)
	bindFields(fn.Type.Params)
	if fn.Body == nil {
		return ctx
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch decl := node.(type) {
		case *ast.DeclStmt:
			gen, ok := decl.Decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				return true
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				typeName := typeNameFromExpr(value.Type)
				if typeName == "" && len(value.Names) == len(value.Values) {
					for i, name := range value.Names {
						if concrete := expressionConcreteType(graph, value.Values[i]); concrete != "" {
							ctx.bindings[name.Name] = concrete
						}
					}
					continue
				}
				for _, name := range value.Names {
					if typeName != "" {
						ctx.bindings[name.Name] = typeName
					}
				}
			}
		case *ast.AssignStmt:
			if len(decl.Lhs) != len(decl.Rhs) {
				return true
			}
			for i, lhs := range decl.Lhs {
				name, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				if deref, ok := decl.Rhs[i].(*ast.StarExpr); ok {
					if source, ok := deref.X.(*ast.Ident); ok && ctx.bindings[source.Name] != "" {
						ctx.bindings[name.Name] = ctx.bindings[source.Name]
						continue
					}
				}
				if concrete := expressionConcreteType(graph, decl.Rhs[i]); concrete != "" {
					ctx.bindings[name.Name] = concrete
				}
			}
		}
		return true
	})
	return ctx
}

func (g *callGraph) resolveReceiverType(expr ast.Expr, ctx authorityResolutionContext) string {
	switch node := expr.(type) {
	case *ast.Ident:
		if typeName := ctx.bindings[node.Name]; typeName != "" {
			return typeName
		}
		if _, isStruct := g.structFields[node.Name]; isStruct {
			return node.Name // method expression such as Store.Method
		}
	case *ast.SelectorExpr:
		parent := g.resolveReceiverType(node.X, ctx)
		if parent != "" {
			return g.structFields[parent][node.Sel.Name]
		}
	case *ast.UnaryExpr:
		if node.Op == token.AND {
			return g.resolveReceiverType(node.X, ctx)
		}
	case *ast.CompositeLit:
		return typeNameFromExpr(node.Type)
	case *ast.CallExpr:
		return expressionConcreteType(g, node)
	case *ast.ParenExpr:
		return g.resolveReceiverType(node.X, ctx)
	}
	return ""
}

func (g *callGraph) hasInternalMethodNamed(name string) bool {
	for key := range g.methodsByReceiver {
		if strings.HasSuffix(key, "."+name) {
			return true
		}
	}
	return false
}

func (g *callGraph) reviewedInterfaceMethodTargets(interfaceName, method string) []string {
	var targets []string
	for _, receiver := range reviewedAuthorityInterfaceImplementations[interfaceName] {
		targets = append(targets, g.methodsByReceiver[receiver+"."+method]...)
	}
	return targets
}

// authorityTargetsForExpr resolves only Go shapes whose internal target is
// exact. An unknown selector that shares an in-package method name is returned
// as unresolved, so stale declarations cannot be justified by a bare-name fan
// out. External calls that do not share a package method remain irrelevant.
func (g *callGraph) authorityTargetsForExpr(expr ast.Expr, ctx authorityResolutionContext) (targets []string, unresolvedMethod string) {
	switch node := expr.(type) {
	case *ast.Ident:
		return append([]string(nil), g.functionsByName[node.Name]...), ""
	case *ast.SelectorExpr:
		if receiver := g.resolveReceiverType(node.X, ctx); receiver != "" {
			if targets := g.methodsByReceiver[receiver+"."+node.Sel.Name]; len(targets) > 0 {
				return append([]string(nil), targets...), ""
			}
			// An interface has no concrete method declaration in this package. If
			// its invoked method is implemented internally, make the exact call
			// site a reviewed dispatch boundary. A concrete/external receiver with
			// no local method is simply outside this package graph, not evidence
			// that every same-named local method could run.
			if g.interfaceTypes[receiver] {
				if targets := g.reviewedInterfaceMethodTargets(receiver, node.Sel.Name); len(targets) > 0 {
					return targets, ""
				}
				if g.hasInternalMethodNamed(node.Sel.Name) {
					return nil, node.Sel.Name
				}
			}
			return nil, ""
		}
		// An untyped receiver is not evidence that an external method ran. If its
		// selector collides with any package method, however, silently dropping
		// the edge would let a live money caller disappear from the exact stale
		// proof. Require an exact call-site review instead of fanning out by bare
		// name. Typed external concrete receivers above remain outside this
		// package graph; this branch is only the genuinely unresolved case.
		if g.hasInternalMethodNamed(node.Sel.Name) {
			return nil, node.Sel.Name
		}
	case *ast.IndexExpr:
		return g.authorityTargetsForExpr(node.X, ctx)
	case *ast.IndexListExpr:
		return g.authorityTargetsForExpr(node.X, ctx)
	case *ast.ParenExpr:
		return g.authorityTargetsForExpr(node.X, ctx)
	}
	return nil, ""
}

func addAuthorityTargets(graph *callGraph, from string, expr ast.Expr, ctx authorityResolutionContext, file string, line int, out map[string]bool) {
	targets, unresolvedMethod := graph.authorityTargetsForExpr(expr, ctx)
	for _, target := range targets {
		if target == from {
			continue
		}
		out[target] = true
	}
	if unresolvedMethod == "" {
		return
	}
	issue := authorityDispatchIssue{File: file, Function: from, Line: line, Method: unresolvedMethod}
	graph.unresolvedAuthorityDispatchCandidates[issue.key()] = issue
}

func authorityFunctionsReachingStructuralMoneySink(graph *callGraph) map[string]bool {
	reverse := map[string]map[string]bool{}
	for from, targets := range graph.authorityEdges {
		for target := range targets {
			if reverse[target] == nil {
				reverse[target] = map[string]bool{}
			}
			reverse[target][from] = true
		}
	}
	reaches := map[string]bool{}
	queue := make([]string, 0, len(graph.structuralMoneySinks))
	for sink := range graph.structuralMoneySinks {
		reaches[sink] = true
		queue = append(queue, sink)
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for caller := range reverse[current] {
			if reaches[caller] {
				continue
			}
			reaches[caller] = true
			queue = append(queue, caller)
		}
	}
	return reaches
}

// finalizeAuthorityDispatches converts unknown selector collisions into
// fail-closed review points only when a same-named in-package method can reach
// a structural money sink. This is the relevant ambiguity for the money stale
// proof; an external err.Error or bytes.Buffer.Write must not manufacture a
// money edge merely because an unrelated local type happens to use that name.
func finalizeAuthorityDispatches(graph *callGraph) {
	moneyReachable := authorityFunctionsReachingStructuralMoneySink(graph)
	for key, issue := range graph.unresolvedAuthorityDispatchCandidates {
		relevant := false
		for methodKey := range graph.methodsByReceiver {
			if strings.HasSuffix(methodKey, "."+issue.Method) && moneyReachable[methodKey] {
				relevant = true
				break
			}
		}
		if !relevant {
			continue
		}
		graph.unresolvedAuthorityDispatches[key] = issue
		for _, target := range reviewedAuthorityDispatches[key] {
			if target != issue.Function {
				graph.authorityEdges[issue.Function][target] = true
			}
		}
	}
}

// addAuthorityFunctionValueTargets unwraps route/middleware adapters such as
// http.HandlerFunc(s.handle). It resolves each embedded selector by receiver
// type, never by bare method name, so handler registration remains an actual
// entrypoint without reintroducing the stale-proof ambiguity.
func addAuthorityFunctionValueTargets(graph *callGraph, from string, expr ast.Expr, ctx authorityResolutionContext, file string, line int, out map[string]bool) {
	addAuthorityTargets(graph, from, expr, ctx, file, line, out)
	ast.Inspect(expr, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		addAuthorityTargets(graph, from, selector, ctx, file, line, out)
		return true
	})
}

// packageStaticStringExpressions indexes package-level const/static var values
// so query fragments such as executionEnvelopeColumns are proved static rather
// than needlessly entered into the dynamic-operation exception ledger. Local
// variables remain dynamic unless their surrounding expression exposes a
// mutation verb, which is the conservative boundary for this lightweight AST
// guard.
func packageStaticStringExpressions(files map[string]*ast.File) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST && gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				valueSpec, ok := spec.(*ast.ValueSpec)
				if !ok || len(valueSpec.Names) != len(valueSpec.Values) {
					continue
				}
				for i, name := range valueSpec.Names {
					out[name.Name] = valueSpec.Values[i]
				}
			}
		}
	}
	return out
}

// buildCallGraph parses every non-test Go file in the package.
func buildCallGraph(t *testing.T) *callGraph {
	t.Helper()
	graph := &callGraph{
		edges:                                 map[string]map[string]bool{},
		authorityEdges:                        map[string]map[string]bool{},
		callEdges:                             map[string]map[string]bool{},
		byName:                                map[string][]string{},
		functionsByName:                       map[string][]string{},
		methodsByReceiver:                     map[string][]string{},
		functionResultTypes:                   map[string]string{},
		structFields:                          map[string]map[string]string{},
		interfaceTypes:                        map[string]bool{},
		interfaceMethods:                      map[string]map[string]bool{},
		file:                                  map[string]string{},
		exactMoneySignature:                   map[string]bool{},
		structuralMoneySinks:                  map[string]bool{},
		moneyMutationTableHits:                map[string]bool{},
		moneyMutationColumnHits:               map[string]map[string]bool{},
		moneyMutationEffectsByFunction:        map[string]map[string]moneyMutationEffect{},
		entrypoints:                           map[string]bool{},
		unresolvedMoneyExpressions:            map[string]moneyExpressionIssue{},
		unresolvedAuthorityDispatches:         map[string]authorityDispatchIssue{},
		unresolvedAuthorityDispatchCandidates: map[string]authorityDispatchIssue{},
	}
	graph.file[packageInitializationRoot] = "<package initialization>"
	graph.edges[packageInitializationRoot] = map[string]bool{}
	graph.authorityEdges[packageInitializationRoot] = map[string]bool{}
	graph.callEdges[packageInitializationRoot] = map[string]bool{}
	graph.entrypoints[packageInitializationRoot] = true
	entries, err := filepath.Glob("*.go")
	must(t, err)
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	for _, name := range entries {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		mustf(t, err, "parse %s: %v", name)
		parsed[name] = file
		indexStructFields(graph, file)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := declKey(fn)
			graph.file[key] = name
			graph.byName[fn.Name.Name] = append(graph.byName[fn.Name.Name], key)
			if fn.Recv == nil || len(fn.Recv.List) == 0 {
				graph.functionsByName[fn.Name.Name] = append(graph.functionsByName[fn.Name.Name], key)
			} else {
				graph.methodsByReceiver[key] = append(graph.methodsByReceiver[key], key)
			}
			graph.functionResultTypes[key] = funcDeclPrimaryConcreteResultType(fn)
			graph.exactMoneySignature[key] = funcDeclCarriesExactMoney(fn)
			if key == "main" || fn.Name.Name == "init" {
				// main is the concrete process entrypoint. Its call closure includes
				// CLI dispatch and the worker leader started by the server process.
				// init and package initializers execute before main, so their direct
				// persistence/provider calls must be live roots too.
				graph.entrypoints[key] = true
			}
		}
	}
	staticStrings := packageStaticStringExpressions(parsed)

	// Go evaluates package-level variable initializers before init/main. They
	// are outside FuncDecl bodies, so scan their direct calls under a synthetic
	// root rather than letting eager persistence or provider work look
	// disconnected. A function value alone is not invoked during initialization;
	// only CallExpr sites receive an edge here.
	initializerContext := authorityResolutionContext{bindings: map[string]string{}}
	for name, file := range parsed {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				valueSpec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, value := range valueSpec.Values {
					ast.Inspect(value, func(node ast.Node) bool {
						call, ok := node.(*ast.CallExpr)
						if !ok {
							return true
						}
						line := fset.Position(call.Pos()).Line
						for _, effect := range moneyMutationEffectsForCallWithResolver(call, staticStrings) {
							addMoneyMutationEffect(graph, packageInitializationRoot, effect)
						}
						for _, kind := range unresolvedMoneyExpressionKindsWithResolver(call, staticStrings) {
							issue := moneyExpressionIssue{File: name, Function: packageInitializationRoot, Line: line, Kind: kind}
							graph.unresolvedMoneyExpressions[issue.key()] = issue
							if exception, reviewed := reviewedMoneyExpressionExceptions[issue.key()]; reviewed {
								for _, effect := range reviewedMoneyMutationEffects(exception) {
									addMoneyMutationEffect(graph, packageInitializationRoot, effect)
								}
							}
						}
						if callPostsToProviderMoneyEndpointWithResolver(call, staticStrings) {
							graph.structuralMoneySinks[packageInitializationRoot] = true
						}
						addAuthorityTargets(graph, packageInitializationRoot, call.Fun, initializerContext, name, line, graph.authorityEdges[packageInitializationRoot])
						addAuthorityFunctionValueTargets(graph, packageInitializationRoot, call.Fun, initializerContext, name, line, graph.authorityEdges[packageInitializationRoot])
						addFunctionTargets(graph, call.Fun, graph.edges[packageInitializationRoot])
						if cname := callExprFuncName(call.Fun); cname != "" {
							for _, target := range graph.byName[cname] {
								if target != packageInitializationRoot {
									graph.callEdges[packageInitializationRoot][target] = true
								}
							}
						}
						return true
					})
				}
			}
		}
	}

	for name, file := range parsed {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			from := declKey(fn)
			resolution := authorityResolutionContextFor(graph, fn)
			if graph.edges[from] == nil {
				graph.edges[from] = map[string]bool{}
			}
			if graph.authorityEdges[from] == nil {
				graph.authorityEdges[from] = map[string]bool{}
			}
			if graph.callEdges[from] == nil {
				graph.callEdges[from] = map[string]bool{}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				// A method value can be retained in a loop/callback table rather than
				// passed directly as a CallExpr argument (Workers.Run is the important
				// production example).  It remains a possible runtime edge. Resolve
				// the receiver exactly here; unlike the historical identifier graph,
				// this does not fan a selector out across every same-named method.
				if selector, ok := n.(*ast.SelectorExpr); ok {
					line := fset.Position(selector.Pos()).Line
					addAuthorityTargets(graph, from, selector, resolution, name, line, graph.authorityEdges[from])
				}
				// Ident/selector edges: every identifier that names a declared
				// function, whether called or passed as a value.
				id, ok := n.(*ast.Ident)
				if !ok {
					if sel, isSel := n.(*ast.SelectorExpr); isSel {
						id = sel.Sel
					} else if call, isCall := n.(*ast.CallExpr); isCall {
						line := fset.Position(call.Pos()).Line
						mutationEffects := moneyMutationEffectsForCallWithResolver(call, staticStrings)
						for _, effect := range mutationEffects {
							addMoneyMutationEffect(graph, from, effect)
						}
						for _, kind := range unresolvedMoneyExpressionKindsWithResolver(call, staticStrings) {
							issue := moneyExpressionIssue{
								File:     name,
								Function: from,
								Line:     line,
								Kind:     kind,
							}
							graph.unresolvedMoneyExpressions[issue.key()] = issue
							if exception, reviewed := reviewedMoneyExpressionExceptions[issue.key()]; reviewed {
								for _, effect := range reviewedMoneyMutationEffects(exception) {
									addMoneyMutationEffect(graph, from, effect)
								}
							}
						}
						if callPostsToProviderMoneyEndpointWithResolver(call, staticStrings) {
							graph.structuralMoneySinks[from] = true
						}
						if isHTTPRouteRegistration(call) {
							// Registered handlers are real HTTP/webhook entrypoints even
							// when middleware wraps them before mux.Handle receives them.
							for _, arg := range call.Args {
								addAuthorityFunctionValueTargets(graph, from, arg, resolution, name, line, graph.entrypoints)
							}
						}
						addAuthorityTargets(graph, from, call.Fun, resolution, name, line, graph.authorityEdges[from])
						// A callback or goroutine target handed to another function is
						// still a possible runtime edge. Resolve it by exact receiver
						// type, never the old bare-name fan-out.
						for _, arg := range call.Args {
							addAuthorityFunctionValueTargets(graph, from, arg, resolution, name, line, graph.authorityEdges[from])
						}
						// CallExpr edges: only actual invocations.
						if cname := callExprFuncName(call.Fun); cname != "" {
							for _, target := range graph.byName[cname] {
								if target != from {
									graph.callEdges[from][target] = true
								}
							}
						}
						return true
					} else {
						return true
					}
				}
				for _, target := range graph.byName[id.Name] {
					if target != from {
						graph.edges[from][target] = true
					}
				}
				return true
			})
			_ = name
		}
	}
	finalizeAuthorityDispatches(graph)
	return graph
}

// reachableFrom returns roots plus every declaration reached through calls or
// function values.  Entrypoint reachability is deliberately separate from the
// reverse money closure: a disconnected declaration cannot become current
// authority merely because it can theoretically call a money sink.
func (g *callGraph) reachableFrom(roots map[string]bool) map[string]bool {
	return g.reachableFromEdges(roots, g.edges)
}

func (g *callGraph) authorityReachableFrom(roots map[string]bool) map[string]bool {
	edges := g.authorityEdges
	// Synthetic tests that construct only the historical edges field retain the
	// same semantics, while production graphs always provide authorityEdges.
	if edges == nil {
		edges = g.edges
	}
	return g.reachableFromEdges(roots, edges)
}

func (g *callGraph) reachableFromEdges(roots map[string]bool, edges map[string]map[string]bool) map[string]bool {
	reached := map[string]bool{}
	queue := make([]string, 0, len(roots))
	for key := range roots {
		if _, exists := g.file[key]; !exists {
			continue
		}
		reached[key] = true
		queue = append(queue, key)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for callee := range edges[cur] {
			if reached[callee] {
				continue
			}
			reached[callee] = true
			queue = append(queue, callee)
		}
	}
	return reached
}

// reaches returns the shortest path from `from` to any key in `targets`, or nil,
// following identifier edges (calls and func values).
func (g *callGraph) reaches(from string, targets map[string]bool) []string {
	return g.reachesOn(from, targets, g.edges)
}

// reachesByCall is reaches restricted to CallExpr edges. Prefer this when
// asking whether a money path consumes a calibration read: route tables that
// merely register an admin handler must not count as consumption.
func (g *callGraph) reachesByCall(from string, targets map[string]bool) []string {
	return g.reachesOn(from, targets, g.callEdges)
}

func (g *callGraph) reachesOn(from string, targets map[string]bool, edges map[string]map[string]bool) []string {
	type step struct {
		key  string
		path []string
	}
	seen := map[string]bool{from: true}
	queue := []step{{key: from, path: []string{from}}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if targets[current.key] && current.key != from {
			return current.path
		}
		next := make([]string, 0, len(edges[current.key]))
		for key := range edges[current.key] {
			next = append(next, key)
		}
		sort.Strings(next) // deterministic path in the failure message
		for _, key := range next {
			if seen[key] {
				continue
			}
			seen[key] = true
			queue = append(queue, step{key: key, path: append(append([]string{}, current.path...), key)})
		}
	}
	return nil
}

// calibrationReadFunctions are the functions that CONSUME an observation.
//
// The recorders are deliberately absent: RecordPlanActuals, recordPlanActuals
// and RecordExecutionOverhead write, and writing from the finalize hook is what
// the tables are for. JobsMissingOverheadActuals reads the jobs table to find
// work for the recorder, not the observations, so it is not a consumer either.
var calibrationReadFunctions = []string{
	"Store.ResolvePlanCalibration",
	"Store.PlanAccuracy",
	"CalibrationPromotable",
	"Store.ExecutionOverhead",
	"Store.ExecutionOverheadHealth",
}

// moneyAndAdmissionAuthorityFiles own a decision about money or about whether
// work may be admitted or placed. Kept in step with the guarded list in
// plan_calibration_test.go by TestGuardedFileListsAgree.
//
// LEGACY filename list — retained for dual-run union with the structural
// money-authority observation (see money_authority_guard_test.go). Roots for
// TestNoCallPathFromMoneyOrAdmissionIntoCalibrationReads are
// filename-roots ∪ structural-authority-roots. Do not remove this list until
// dual-run has been green long enough to prove the structural view alone is
// sufficient; removal is remaining work, not done in this change.
var moneyAndAdmissionAuthorityFiles = []string{
	"billing.go", "buyer.go", "buyer_charge_operations.go", "collect.go",
	"economic_plan.go", "economic_facts.go", "ledger_write.go", "payment.go",
	"payment_authority.go", "prepaid.go", "pricing.go", "pricing_decision.go",
	"pricing_governance.go", "quote.go", "compute_plan.go", "scheduler.go",
	"realtime_placement.go", "shape_routing.go", "supplier_accrual.go",
	"stripe_settlement.go", "observed_output_settlement.go", "store_billing.go",
}

func TestNoCallPathFromMoneyOrAdmissionIntoCalibrationReads(t *testing.T) {
	graph := buildCallGraph(t)

	targets := map[string]bool{}
	for _, name := range calibrationReadFunctions {
		if _, declared := graph.edges[name]; !declared {
			t.Fatalf("%s is not a declared function; this gate is guarding a name "+
				"that no longer exists, which is the same as guarding nothing", name)
		}
		targets[name] = true
	}

	// Dual-run: prove every legacy filename still exists, then root at the
	// union of filename roots and structural money-authority reachability.
	for _, name := range moneyAndAdmissionAuthorityFiles {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("guarded file %s does not exist: %v", name, err)
		}
	}

	fileRoots := filenameMoneyAuthorityRoots(t, graph)
	structuralRoots := structuralMoneyAuthorityRoots(t, graph)
	roots := unionMoneyAuthorityRoots(t, graph)
	if len(roots) == 0 {
		t.Fatal("no money/admission authority roots; the traversal is vacuous")
	}

	for _, root := range roots {
		// CallExpr edges only: registration of an admin handler is not consumption.
		if path := graph.reachesByCall(root, targets); path != nil {
			t.Errorf("%s (%s) reaches calibration read %s:\n  %s",
				root, graph.file[root], path[len(path)-1], strings.Join(path, "\n  → "))
		}
	}
	t.Logf("checked %d roots (filename=%d structural=%d union=%d; dual-run) against %d calibration reads",
		len(roots), len(fileRoots), len(structuralRoots), len(roots), len(targets))
}

// The two gates must guard the same set of files, or tightening one silently
// leaves a hole in the other.
func TestGuardedFileListsAgree(t *testing.T) {
	// The identifier gate's list, copied here so a divergence is a test failure
	// rather than something a reader has to notice.
	identifierGate := []string{
		"billing.go", "buyer.go", "buyer_charge_operations.go", "collect.go",
		"economic_plan.go", "economic_facts.go", "ledger_write.go", "payment.go",
		"payment_authority.go", "prepaid.go", "pricing.go", "pricing_decision.go",
		"pricing_governance.go", "quote.go", "compute_plan.go", "scheduler.go",
		"realtime_placement.go", "shape_routing.go", "supplier_accrual.go",
		"stripe_settlement.go", "observed_output_settlement.go", "store_billing.go",
	}
	got := append([]string(nil), moneyAndAdmissionAuthorityFiles...)
	want := append([]string(nil), identifierGate...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the call-graph gate guards\n  %v\nand the identifier gate guards\n  %v",
			got, want)
	}
}

// The traversal must actually find a path when one exists, or a green result
// means nothing. This plants the exact shape the identifier gate cannot see: a
// guarded file calling a helper in an ALLOWED file that reads calibration.
func TestCallGraphFindsAnIndirectPath(t *testing.T) {
	graph := buildCallGraph(t)

	// A real edge that exists in the tree: whatever calls the resolver.
	callers := 0
	for from, to := range graph.edges {
		if to["Store.ResolvePlanCalibration"] {
			callers++
			t.Logf("%s (%s) calls the calibration resolver", from, graph.file[from])
		}
	}

	// Synthesize the two-hop shape and confirm the traversal reports it.
	graph.edges["fake.MoneyHandler"] = map[string]bool{"fake.Helper": true}
	graph.edges["fake.Helper"] = map[string]bool{"Store.PlanAccuracy": true}
	path := graph.reaches("fake.MoneyHandler", map[string]bool{"Store.PlanAccuracy": true})
	if len(path) != 3 {
		t.Fatalf("two-hop path was reported as %v; the traversal does not follow calls", path)
	}

	// And a function that reaches nothing must report nothing.
	graph.edges["fake.Isolated"] = map[string]bool{}
	if path := graph.reaches("fake.Isolated", map[string]bool{"Store.PlanAccuracy": true}); path != nil {
		t.Fatalf("an isolated function reported a path: %v", path)
	}
}

var schemaCreateTableRE = regexp.MustCompile(`(?im)^CREATE TABLE IF NOT EXISTS\s+([a-z_][a-z0-9_]*)`)
var schemaCreateTableBlockRE = regexp.MustCompile(`(?is)CREATE TABLE IF NOT EXISTS\s+([a-z_][a-z0-9_]*)\s*\((.*?)\);`)
var schemaAlterTableBlockRE = regexp.MustCompile(`(?is)ALTER TABLE\s+([a-z_][a-z0-9_]*)\s+(.*?);`)
var schemaAlterAddColumnRE = regexp.MustCompile(`(?is)\bADD\s+COLUMN(?:\s+IF\s+NOT\s+EXISTS)?\s+"?([a-z_][a-z0-9_]*)"?`)
var schemaIdentifierRE = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
var moneyAuthorityCandidateTokens = map[string]bool{
	"amount": true, "balance": true, "price": true, "rate": true,
	"currency": true, "charge": true, "refund": true, "payout": true,
	"reserve": true, "fund": true, "liability": true, "contribution": true,
	"cost": true, "fee": true, "credit": true, "ceiling": true,
	"spend": true, "billed": true, "payment": true, "settlement": true,
	"entitlement": true, "accrual": true, "quote": true, "budget": true,
}

func moneyAuthorityCandidateColumn(column string) bool {
	for _, token := range strings.Split(strings.ToLower(column), "_") {
		singular := strings.TrimSuffix(token, "s")
		if moneyAuthorityCandidateTokens[token] || moneyAuthorityCandidateTokens[singular] {
			return true
		}
		// Exact money units often appear fused into one database-safe token,
		// e.g. accrued_microusd. Do not substring-match `currency`: that would
		// incorrectly classify max_concurrency as an economic field.
		if strings.HasSuffix(token, "usd") || strings.HasSuffix(token, "nanos") ||
			strings.HasSuffix(token, "micros") || strings.HasSuffix(token, "cents") {
			return true
		}
	}
	return false
}

func schemaColumnName(line string) string {
	line = strings.TrimSpace(strings.SplitN(line, "--", 2)[0])
	if line == "" {
		return ""
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	name := strings.ToLower(strings.Trim(fields[0], `"`))
	switch name {
	case "constraint", "primary", "foreign", "unique", "check", "exclude":
		return ""
	}
	if !schemaIdentifierRE.MatchString(name) {
		return ""
	}
	return name
}

// schemaTableColumns reads the schema's CREATE and ADD COLUMN forms. It is a
// deliberately small schema census rather than a SQL migration executor: this
// guard needs stable table/column names, not type checking or DDL side effects.
// The ALTER matcher intentionally also sees literal ALTER TABLE statements
// embedded in a DO block, such as the jobs.currency compatibility migration.
func schemaTableColumns(t *testing.T) map[string]map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	text := string(raw)
	out := map[string]map[string]bool{}
	for _, match := range schemaCreateTableBlockRE.FindAllStringSubmatch(text, -1) {
		if len(match) != 3 {
			continue
		}
		table := strings.ToLower(match[1])
		if out[table] == nil {
			out[table] = map[string]bool{}
		}
		for _, line := range strings.Split(match[2], "\n") {
			if column := schemaColumnName(line); column != "" {
				out[table][column] = true
			}
		}
	}
	for _, match := range schemaAlterTableBlockRE.FindAllStringSubmatch(text, -1) {
		if len(match) != 3 {
			continue
		}
		table := strings.ToLower(match[1])
		if out[table] == nil {
			// A migration against an absent base table is still a schema mistake,
			// but keep parsing it so the ledger test can report the exact target.
			out[table] = map[string]bool{}
		}
		for _, columnMatch := range schemaAlterAddColumnRE.FindAllStringSubmatch(match[2], -1) {
			if len(columnMatch) == 2 {
				out[table][strings.ToLower(columnMatch[1])] = true
			}
		}
	}
	return out
}

func schemaTableNames(t *testing.T) map[string]bool {
	t.Helper()
	columns := schemaTableColumns(t)
	out := make(map[string]bool, len(columns))
	for table := range columns {
		out[table] = true
	}
	return out
}

func moneyAuthorityExclusion(table, column string) (string, bool) {
	if reason, ok := moneyAuthoritySchemaExclusions[table+"."+column]; ok {
		return reason, true
	}
	reason, ok := moneyAuthoritySchemaExclusions[table+".*"]
	return reason, ok
}

func firstCallExpr(t *testing.T, source string) *ast.CallExpr {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", source, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	var first *ast.CallExpr
	ast.Inspect(file, func(node ast.Node) bool {
		if first == nil {
			if call, ok := node.(*ast.CallExpr); ok {
				first = call
			}
		}
		return first == nil
	})
	if first == nil {
		t.Fatal("synthetic source had no call")
	}
	return first
}

func callExprNamed(t *testing.T, source, name string) *ast.CallExpr {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", source, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	var found *ast.CallExpr
	ast.Inspect(file, func(node ast.Node) bool {
		if found != nil {
			return false
		}
		if call, ok := node.(*ast.CallExpr); ok && callExprFuncName(call.Fun) == name {
			found = call
			return false
		}
		return true
	})
	if found == nil {
		t.Fatalf("synthetic source had no %s call", name)
	}
	return found
}

// TestMoneyAuthoritySchemaColumnLedger is the mechanical semantic census.
// Every authority-looking schema field is either included in an exact live
// scope or explicitly excluded with a reason. This prevents the older
// table-only list from quietly missing new jobs/tasks/models/offer authority.
func TestMoneyAuthoritySchemaColumnLedger(t *testing.T) {
	schema := schemaTableColumns(t)
	if !schema["jobs"]["currency"] || !moneyMutationScopes["jobs"].Columns["currency"] {
		t.Error("jobs.currency compatibility ALTER was not parsed and included as quote/charge authority")
	}
	if schema["tasks"]["currency"] {
		t.Error("tasks.currency is present; classify it explicitly instead of relying on inherited plan currency")
	}
	coveredCategories := map[string]bool{}
	tables := make([]string, 0, len(moneyMutationScopes))
	for table := range moneyMutationScopes {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		scope := moneyMutationScopes[table]
		columns, exists := schema[table]
		if !exists {
			t.Errorf("reviewed money table %q is absent from schema.sql", table)
			continue
		}
		if !scope.WholeRow && len(scope.Columns) == 0 {
			t.Errorf("mixed money table %q has no authority columns", table)
		}
		for _, category := range scope.Categories {
			coveredCategories[category] = true
		}
		for column := range scope.Columns {
			if !columns[column] {
				t.Errorf("reviewed authority column %s.%s is absent from schema.sql", table, column)
			}
		}
	}

	for key, reason := range moneyAuthoritySchemaExclusions {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("schema exclusion %q has no review reason", key)
			continue
		}
		parts := strings.Split(key, ".")
		tableColumns, tableExists := schema[parts[0]]
		if len(parts) != 2 || !tableExists || parts[1] != "*" && !tableColumns[parts[1]] {
			t.Errorf("schema exclusion %q does not name a current schema column", key)
			continue
		}
		if parts[1] == "*" && !tableExists {
			t.Errorf("schema exclusion %q does not name a current schema table", key)
		}
	}
	for key, reason := range moneyAuthorityAbsentFieldAdjudications {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("absent-field adjudication %q has no review reason", key)
			continue
		}
		parts := strings.Split(key, ".")
		tableColumns, tableExists := schema[parts[0]]
		if len(parts) != 2 || !tableExists {
			t.Errorf("absent-field adjudication %q does not name a current schema table", key)
			continue
		}
		if tableColumns[parts[1]] {
			t.Errorf("absent-field adjudication %q is now present; classify it in the inclusion or exclusion ledger", key)
		}
	}

	for table, columns := range schema {
		for column := range columns {
			if !moneyAuthorityCandidateColumn(column) {
				continue
			}
			scope, included := moneyMutationScopes[table]
			if included && (scope.WholeRow || scope.Columns[column]) {
				continue
			}
			if _, excluded := moneyAuthorityExclusion(table, column); excluded {
				continue
			}
			t.Errorf("unclassified money/admission schema field %s.%s: add it to a precise scope or an explicit exclusion", table, column)
		}
	}

	for _, category := range []string{
		"quote", "admission", "reserve", "charge", "refund", "settlement",
		"liability", "contribution", "payout", "spend authority",
	} {
		if !coveredCategories[category] {
			t.Errorf("schema authority ledger has no %q classification", category)
		}
	}
	t.Logf("reconciled %d authority tables and %d explicit non-authority exclusions against schema columns", len(tables), len(moneyAuthoritySchemaExclusions))
}

// TestMoneyMutationTableScopeIsLive reconciles the authority ledger with the
// current package and proves each generic field mutation is structurally
// caught. It does not use the function name or source-file vocabulary.
func TestMoneyMutationTableScopeIsLive(t *testing.T) {
	graph := buildCallGraph(t)
	liveTableHits, liveColumnHits := liveMoneyMutationEffectHits(graph)
	liveFunctions := graph.authorityReachableFrom(graph.entrypoints)
	tables := make([]string, 0, len(moneyMutationScopes))
	for table := range moneyMutationScopes {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		scope := moneyMutationScopes[table]
		if dormant, dormantNonLive := dormantNonLiveMoneyMutationScopes[table]; dormantNonLive {
			if strings.TrimSpace(dormant.Reason) == "" || len(dormant.Writers) == 0 {
				t.Errorf("dormant_non_live money table %q lacks a writer or review reason", table)
				continue
			}
			if liveTableHits[table] {
				t.Errorf("dormant_non_live money table %q became entrypoint-reachable; remove its dormant classification and review the lifecycle", table)
			}
			actualWriters := map[string]bool{}
			for writer, effects := range graph.moneyMutationEffectsByFunction {
				if _, writesTable := effects[table]; writesTable {
					actualWriters[writer] = true
				}
			}
			expectedWriters := setFromList(dormant.Writers)
			if got, want := strings.Join(sortedMoneyAuthorityKeys(actualWriters), ","), strings.Join(sortedMoneyAuthorityKeys(expectedWriters), ","); got != want {
				t.Errorf("dormant_non_live money table %q writers=%q want exact reviewed set %q", table, got, want)
			}
			for _, writer := range dormant.Writers {
				if _, declared := graph.file[writer]; !declared {
					t.Errorf("dormant_non_live money table %q names missing writer %s", table, writer)
				}
				if liveFunctions[writer] {
					t.Errorf("dormant_non_live money writer %s is entrypoint-reachable; remove the dormant classification", writer)
				}
			}
			continue
		}
		if !liveTableHits[table] {
			t.Errorf("reviewed money table %q has no entrypoint-reachable literal non-test SQL/CopyFrom mutation", table)
		}
		if scope.WholeRow {
			call := firstCallExpr(t, "package main\nfunc generic(tx any) { tx.Exec(nil, `DELETE FROM "+table+"`) }")
			if !callMutatesMoneyTable(call) {
				t.Errorf("generic Exec deletion of %s was not classified as a money sink", table)
			}
			continue
		}
		for column := range scope.Columns {
			if !liveColumnHits[table][column] {
				t.Errorf("reviewed authority column %s.%s has no entrypoint-reachable literal non-test SQL/CopyFrom mutation", table, column)
			}
			// The function and value are deliberately generic. Exact target field,
			// not money-looking vocabulary, must be sufficient for classification.
			call := firstCallExpr(t, "package main\nfunc generic(tx any) { tx.Exec(nil, `UPDATE "+table+" SET "+column+"=1`) }")
			if !callMutatesMoneyTable(call) {
				t.Errorf("generic Exec mutation of %s.%s was not classified as money/admission authority", table, column)
			}
		}
	}
	for table, dormant := range dormantNonLiveMoneyMutationScopes {
		if _, scoped := moneyMutationScopes[table]; !scoped {
			t.Errorf("dormant_non_live table %q is not in the semantic money scope", table)
		}
		if strings.TrimSpace(dormant.Reason) == "" {
			t.Errorf("dormant_non_live table %q has no review reason", table)
		}
	}
	t.Logf("reconciled %d reviewed money/admission tables against entrypoint-reachable literal mutation + generic field tests", len(tables))
}

// TestMoneyMutationColumnsIgnorePredicates proves the UPDATE parser stops at
// the top-level WHERE/RETURNING boundary. A status transition that merely tests
// charge_status is lifecycle control, not a write to charge authority.
func TestMoneyMutationColumnsIgnorePredicates(t *testing.T) {
	call := firstCallExpr(t, "package main\nfunc generic(tx any) { tx.Exec(nil, `UPDATE jobs SET status='running' WHERE charge_status='charged' RETURNING id`) }")
	if callMutatesMoneyTable(call) {
		t.Fatal("UPDATE jobs SET status ... WHERE charge_status was classified from its predicate instead of its SET target")
	}

	call = firstCallExpr(t, "package main\nfunc generic(tx any) { tx.Exec(nil, `UPDATE jobs SET max_usd=1 WHERE status='queued'`) }")
	if !callMutatesMoneyTable(call) {
		t.Fatal("generic UPDATE jobs SET max_usd was not structurally classified")
	}
	call = firstCallExpr(t, "package main\nfunc generic(tx any) { tx.Exec(nil, `UPDATE tasks SET economic_buyer_charge_nanos=1 WHERE status='queued'`) }")
	if !callMutatesMoneyTable(call) {
		t.Fatal("generic UPDATE tasks SET frozen economic amount was not structurally classified")
	}
}

// TestMoneyMutationSQLFormsFailClosed covers mutation syntax that does not
// look like the historical INSERT/UPDATE/DELETE trio. Schema qualification
// must not hide a money table, MERGE lacks a reliable SET parser and is thus
// table-level authority, and CALL is opaque until an exact review exception
// explains the stored procedure body.
func TestMoneyMutationSQLFormsFailClosed(t *testing.T) {
	qualifiedUpdate := firstCallExpr(t, "package main\nfunc generic(db any) { db.Exec(`UPDATE accounting.jobs SET max_usd=$1 WHERE id=$2`, 1, 2) }")
	if !callMutatesMoneyTable(qualifiedUpdate) {
		t.Fatal("schema-qualified UPDATE jobs.max_usd was not structurally classified")
	}

	truncate := firstCallExpr(t, "package main\nfunc generic(db any) { db.Exec(`TRUNCATE TABLE accounting.jobs`) }")
	if !callMutatesMoneyTable(truncate) {
		t.Fatalf("schema-qualified TRUNCATE jobs was not structurally classified; effects=%#v", moneyMutationEffectsForCall(truncate))
	}
	effects := moneyMutationEffectsForCall(truncate)
	if len(effects) != 1 || effects[0].Table != "jobs" || !effects[0].WholeRow {
		t.Fatalf("TRUNCATE effects=%#v; want a whole-row jobs effect", effects)
	}

	merge := firstCallExpr(t, "package main\nfunc generic(db any) { db.Exec(`MERGE INTO accounting.jobs j USING incoming i ON j.id=i.id WHEN MATCHED THEN UPDATE SET max_usd=i.max_usd`) }")
	if !callMutatesMoneyTable(merge) {
		t.Fatal("schema-qualified MERGE jobs was not fail-closed as authority")
	}
	effects = moneyMutationEffectsForCall(merge)
	if len(effects) != 1 || effects[0].Table != "jobs" || !effects[0].UnknownColumns {
		t.Fatalf("MERGE effects=%#v; want an unknown-column jobs effect", effects)
	}

	procedure := firstCallExpr(t, "package main\nfunc generic(db any) { db.Exec(`CALL accounting.set_job_budget($1)`, 1) }")
	if !hasUnresolvedMoneyExpressionKind(procedure, "opaque-db-procedure") {
		t.Fatal("stored procedure CALL was not refused for exact review")
	}
}

// TestReadOnlySQLProofRejectsCallableRoutine prevents a dynamic SELECT
// exception from treating a user-defined function as harmless. PostgreSQL
// functions can mutate financial state even when their outer statement begins
// with SELECT, so only the reviewed built-in routine ledger may pass.
func TestReadOnlySQLProofRejectsCallableRoutine(t *testing.T) {
	if isReadOnlySQLStatement(`SELECT settle_money($1)`) {
		t.Fatal("SELECT settle_money was accepted as read-only by a dynamic SQL proof")
	}
	if !isReadOnlySQLStatement(`SELECT count(*) FROM jobs`) {
		t.Fatal("reviewed PostgreSQL aggregate was rejected as read-only")
	}
}

// TestSQLStatementArgumentHandlesStdlibBinds ensures the database/sql shape
// (statement first) cannot be interpreted as pgx's context-first shape. A
// literal statement paired with a bind must still drive structural effects.
func TestSQLStatementArgumentHandlesStdlibBinds(t *testing.T) {
	exec := firstCallExpr(t, "package main\nfunc generic(db any) { db.Exec(`UPDATE jobs SET max_usd=$1 WHERE id=$2`, 1, 2) }")
	if !callMutatesMoneyTable(exec) {
		t.Fatal("database/sql Exec statement was displaced by its first bind")
	}
	queryRow := firstCallExpr(t, "package main\nfunc generic(db any) { db.QueryRow(`UPDATE jobs SET max_usd=$1 WHERE id=$2 RETURNING id`, 1, 2) }")
	if !callMutatesMoneyTable(queryRow) {
		t.Fatal("database/sql QueryRow statement was displaced by its first bind")
	}
}

// TestMoneyMutationCopyFromDetectsFrozenTaskEconomics pins the bulk path. A
// pgx CopyFrom has no INSERT SQL for the normal classifier to inspect, so both
// target and supplied columns must be read from the AST. A status-only copy
// remains non-authority; a dynamic column list fail-closes once the known
// financial target table has been identified.
func TestMoneyMutationCopyFromDetectsFrozenTaskEconomics(t *testing.T) {
	call := firstCallExpr(t, `package main
func genericCopy(ctx any, tx any, rows any) {
  tx.CopyFrom(ctx, pgx.Identifier{"tasks"}, []string{
    "id", "economic_buyer_charge_usd", "economic_supplier_payout_usd",
    "economic_buyer_charge_nanos", "economic_supplier_payout_nanos",
  }, rows)
}`)
	if !callMutatesMoneyTable(call) {
		t.Fatal("pgx CopyFrom tasks frozen economic columns was not structurally classified")
	}
	effects := moneyMutationEffectsForCall(call)
	if len(effects) != 1 || effects[0].Table != "tasks" {
		t.Fatalf("CopyFrom effects=%#v; want one tasks effect", effects)
	}
	for _, column := range []string{
		"economic_buyer_charge_usd", "economic_supplier_payout_usd",
		"economic_buyer_charge_nanos", "economic_supplier_payout_nanos",
	} {
		if !effects[0].Columns[column] {
			t.Errorf("CopyFrom did not retain frozen task authority column %q", column)
		}
	}

	statusOnly := firstCallExpr(t, `package main
func genericCopy(ctx any, tx any, rows any) {
  tx.CopyFrom(ctx, pgx.Identifier{"tasks"}, []string{"id", "status"}, rows)
}`)
	if callMutatesMoneyTable(statusOnly) {
		t.Fatal("status-only CopyFrom(tasks) was classified as money authority")
	}

	dynamicColumns := firstCallExpr(t, `package main
func genericCopy(ctx any, tx any, columns []string, rows any) {
  tx.CopyFrom(ctx, pgx.Identifier{"tasks"}, columns, rows)
}`)
	if !callMutatesMoneyTable(dynamicColumns) {
		t.Fatal("CopyFrom(tasks) with a dynamic column list did not fail closed")
	}
}

// TestMoneyMutationBatchQueueDetectsMoneyEffectsAndDeclarationMismatch keeps
// the pgx batching path attached to the function that enqueues the statement,
// rather than the later BatchResults.Exec consumer. A generic Queue INSERT or
// UPDATE must therefore become a sink and make an absent declaration fail.
func TestMoneyMutationBatchQueueDetectsMoneyEffectsAndDeclarationMismatch(t *testing.T) {
	insert := firstCallExpr(t, `package main
func genericQueue(batch any) {
  batch.Queue(`+"`INSERT INTO execution_contracts (id, maximum_price_usd) VALUES ($1, $2)`"+`)
}`)
	update := firstCallExpr(t, `package main
func genericQueue(batch any) {
  batch.Queue(`+"`UPDATE jobs SET max_usd=$1 WHERE id=$2`"+`)
}`)
	for _, call := range []*ast.CallExpr{insert, update} {
		if !callMutatesMoneyTable(call) {
			t.Fatalf("generic Batch.Queue %q was not structurally classified", callExprFuncName(call.Fun))
		}
	}
	if effects := moneyMutationEffectsForCall(update); len(effects) != 1 || effects[0].Table != "jobs" || !effects[0].Columns["max_usd"] {
		t.Fatalf("Queue UPDATE effects=%#v; want jobs.max_usd", effects)
	}

	graph := &callGraph{
		edges:                   map[string]map[string]bool{"Server.handleLive": {"batchWriter": true}, "batchWriter": {}},
		file:                    map[string]string{"Server.handleLive": "api.go", "batchWriter": "generic.go"},
		structuralMoneySinks:    map[string]bool{},
		moneyMutationTableHits:  map[string]bool{},
		moneyMutationColumnHits: map[string]map[string]bool{},
		entrypoints:             map[string]bool{"Server.handleLive": true},
	}
	for _, call := range []*ast.CallExpr{insert, update} {
		for _, effect := range moneyMutationEffectsForCall(call) {
			addMoneyMutationEffect(graph, "batchWriter", effect)
		}
	}
	observed := observeMoneyAuthority(graph)
	undeclared, stale := moneyAuthorityDeclarationDiff(observed.Sinks, map[string]bool{})
	if got, want := strings.Join(undeclared, ","), "batchWriter"; got != want {
		t.Fatalf("Queue sink undeclared=%q want %q", got, want)
	}
	if len(stale) != 0 {
		t.Fatalf("Queue sink declaration stale=%v, want none", stale)
	}
}

func hasUnresolvedMoneyExpressionKind(call *ast.CallExpr, want string) bool {
	for _, kind := range unresolvedMoneyExpressionKinds(call) {
		if kind == want {
			return true
		}
	}
	return false
}

// TestDynamicMoneyExpressionsFailClosed attacks three bypass forms. The
// production reconciliation below permits an exception only when a reviewer
// has pinned that exact source site, not merely a function or file prefix.
func TestDynamicMoneyExpressionsFailClosed(t *testing.T) {
	dynamicSQL := callExprNamed(t, `package main
func generic(tx any, table string) {
  tx.Exec(nil, fmt.Sprintf("UPDATE %s SET max_usd=1", table))
}`, "Exec")
	if !hasUnresolvedMoneyExpressionKind(dynamicSQL, "dynamic-db-statement") {
		t.Fatal("fmt.Sprintf mutation SQL was not refused as a dynamic DB statement")
	}

	opaqueExecContext := firstCallExpr(t, `package main
func generic(ctx any, tx any, statement string) {
  tx.ExecContext(ctx, statement)
}`)
	if !hasUnresolvedMoneyExpressionKind(opaqueExecContext, "dynamic-db-statement") {
		t.Fatal("opaque ExecContext statement was not refused")
	}

	opaqueQueryRowContext := firstCallExpr(t, `package main
func generic(ctx any, tx any, statement string) {
  tx.QueryRowContext(ctx, statement)
}`)
	if !hasUnresolvedMoneyExpressionKind(opaqueQueryRowContext, "dynamic-db-statement") {
		t.Fatal("opaque QueryRowContext statement was not refused")
	}

	mutatingRoutine := firstCallExpr(t, `package main
func generic(ctx any, tx any) {
  tx.QueryRow(ctx, "SELECT settle_money($1)", "buyer")
}`)
	if !hasUnresolvedMoneyExpressionKind(mutatingRoutine, "opaque-db-routine:settle_money") {
		t.Fatal("SELECT-callable mutating routine was not refused")
	}

	dynamicCopy := firstCallExpr(t, `package main
func generic(ctx any, tx any, table string, columns []string, rows any) {
  tx.CopyFrom(ctx, pgx.Identifier{table}, columns, rows)
}`)
	if !hasUnresolvedMoneyExpressionKind(dynamicCopy, "dynamic-copyfrom") {
		t.Fatal("variable-built pgx CopyFrom target/columns was not refused")
	}

	dynamicEndpoint := callExprNamed(t, `package main
func generic(client any, receipt string) {
  endpoint := fmt.Sprintf("https://api.stripe.com/v1/refunds/%s", receipt)
  client.NewRequestWithContext(nil, http.MethodPost, endpoint, nil)
}`, "NewRequestWithContext")
	if !hasUnresolvedMoneyExpressionKind(dynamicEndpoint, "dynamic-provider-money-endpoint") {
		t.Fatal("fmt.Sprintf-built POST provider endpoint was not refused")
	}

	dynamicMethod := firstCallExpr(t, `package main
func generic(ctx any, client any, method string) {
  client.NewRequestWithContext(ctx, method, "https://api.stripe.com/v1/refunds", nil)
}`)
	if !hasUnresolvedMoneyExpressionKind(dynamicMethod, "dynamic-provider-money-method") {
		t.Fatal("variable-built provider money method was not refused")
	}
	if !callPostsToProviderMoneyEndpoint(dynamicMethod) {
		t.Fatal("variable-built provider money method was not retained as a potential structural sink")
	}

	staticNamedMethod := firstCallExpr(t, `package main
func generic(ctx any, client any) {
  client.NewRequestWithContext(ctx, postMethod, "https://api.stripe.com/v1/refunds", nil)
}`)
	resolver := map[string]ast.Expr{
		"postMethod": &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote("POST")},
	}
	if !callPostsToProviderMoneyEndpointWithResolver(staticNamedMethod, resolver) {
		t.Fatal("package-level static POST method was not recognized")
	}
	if got := unresolvedMoneyExpressionKindsWithResolver(staticNamedMethod, resolver); len(got) != 0 {
		t.Fatalf("static named POST method was needlessly refused: %v", got)
	}

	dynamicQueue := firstCallExpr(t, `package main
func generic(batch any, statement string) {
  batch.Queue(statement)
}`)
	if !hasUnresolvedMoneyExpressionKind(dynamicQueue, "dynamic-db-statement") {
		t.Fatal("variable-built Batch.Queue statement was not refused")
	}
}

// TestNoUnreviewedDynamicMoneyExpressions reconciles all production dynamic
// operation sites against the exact exception ledger. Exceptions are bi-
// directional: a dead line number is an error rather than permanent debt.
func TestNoUnreviewedDynamicMoneyExpressions(t *testing.T) {
	graph := buildCallGraph(t)
	keys := make([]string, 0, len(graph.unresolvedMoneyExpressions))
	for key := range graph.unresolvedMoneyExpressions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		exception, reviewed := reviewedMoneyExpressionExceptions[key]
		if !reviewed || strings.TrimSpace(exception.Reason) == "" {
			t.Errorf("unreviewed dynamic money operation %s; use an exact exception only with a reason", key)
		}
	}
	for key, exception := range reviewedMoneyExpressionExceptions {
		if strings.TrimSpace(exception.Reason) == "" {
			t.Errorf("dynamic money exception %s has no review reason", key)
			continue
		}
		issue, current := graph.unresolvedMoneyExpressions[key]
		if !current {
			t.Errorf("stale dynamic money exception %s; remove it or pin the current exact site", key)
		}
		if len(exception.Effects) > 0 && !exception.DeriveLiteralEffects && issue.Kind != "dynamic-db-statement" {
			t.Errorf("dynamic money exception %s declares effects without source derivation; do not self-assert dynamic SQL coverage", key)
		}
		if issue.Kind == "dynamic-db-statement" {
			if strings.TrimSpace(exception.DBEvidence.Mode) == "" || strings.TrimSpace(exception.DBEvidence.Artifact) == "" {
				t.Errorf("dynamic database exception %s lacks artifact-bound source evidence", key)
			}
		} else if exception.DBEvidence.Mode != "" || exception.DBEvidence.Artifact != "" {
			t.Errorf("non-database exception %s carries database evidence", key)
		}
		seenEffects := map[string]bool{}
		for _, reviewedEffect := range exception.Effects {
			table := strings.ToLower(strings.TrimSpace(reviewedEffect.Table))
			if table == "" || table != reviewedEffect.Table {
				t.Errorf("dynamic money exception %s has non-canonical table effect %q", key, reviewedEffect.Table)
				continue
			}
			scope, scoped := moneyMutationScopes[table]
			if !scoped {
				t.Errorf("dynamic money exception %s names unreviewed money table %s", key, table)
				continue
			}
			if reviewedEffect.WholeRow {
				if len(reviewedEffect.Columns) != 0 {
					t.Errorf("dynamic money exception %s supplies columns for row-level effect %s", key, table)
				}
				effectKey := table + ".*"
				if seenEffects[effectKey] {
					t.Errorf("dynamic money exception %s repeats effect %s", key, effectKey)
					continue
				}
				seenEffects[effectKey] = true
				if !graph.moneyMutationTableHits[table] {
					t.Errorf("dynamic money exception %s did not produce a structural hit for %s", key, table)
				}
				continue
			}
			if scope.WholeRow {
				t.Errorf("dynamic money exception %s must mark one-purpose ledger %s as a row-level effect", key, table)
				continue
			}
			if len(reviewedEffect.Columns) == 0 {
				t.Errorf("dynamic money exception %s lacks exact columns for mixed table %s", key, table)
				continue
			}
			for _, reviewedColumn := range reviewedEffect.Columns {
				column := strings.ToLower(strings.TrimSpace(reviewedColumn))
				effectKey := table + "." + column
				if column == "" || column != reviewedColumn {
					t.Errorf("dynamic money exception %s has non-canonical column effect %s.%s", key, table, reviewedColumn)
					continue
				}
				if !scope.Columns[column] {
					t.Errorf("dynamic money exception %s names unreviewed authority column %s", key, effectKey)
					continue
				}
				if seenEffects[effectKey] {
					t.Errorf("dynamic money exception %s repeats effect %s", key, effectKey)
					continue
				}
				seenEffects[effectKey] = true
				if !graph.moneyMutationColumnHits[table][column] {
					t.Errorf("dynamic money exception %s did not produce a structural hit for %s", key, effectKey)
				}
			}
		}
		if current && issue.Kind == "dynamic-db-statement" {
			derived := dynamicDBEvidenceEffects(t, graph, issue, exception.DBEvidence)
			declared := reviewedMoneyMutationEffects(exception)
			if got, want := strings.Join(moneyMutationEffectFingerprints(derived), ";"), strings.Join(moneyMutationEffectFingerprints(declared), ";"); got != want {
				t.Errorf("dynamic money exception %s effects do not match its artifact-bound source proof:\n  derived: %s\n  ledger: %s", key, got, want)
			}
		}
	}
	if len(keys) > 0 {
		t.Logf("reconciled %d reviewed dynamic DB/CopyFrom/provider operation sites", len(keys))
	}
}

// TestNoUnreviewedAuthorityDispatches keeps the reachability proof honest at
// interface boundaries. A selector whose receiver cannot be resolved must not
// inherit every same-named package method; it needs an exact, reviewed target
// set at that call site instead. This is deliberately bidirectional so a
// removed dispatch cannot leave a permanently permissive exception behind.
func TestNoUnreviewedAuthorityDispatches(t *testing.T) {
	graph := buildCallGraph(t)
	keys := make([]string, 0, len(graph.unresolvedAuthorityDispatches))
	for key := range graph.unresolvedAuthorityDispatches {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		issue := graph.unresolvedAuthorityDispatches[key]
		targets, reviewed := reviewedAuthorityDispatches[key]
		if !reviewed || len(targets) == 0 {
			t.Errorf("unreviewed authority dispatch %s; pin exact in-package targets or remove the reachable call", key)
			continue
		}
		seen := map[string]bool{}
		for _, target := range targets {
			if target == "" || seen[target] {
				t.Errorf("authority dispatch %s has an empty or duplicate target %q", key, target)
				continue
			}
			seen[target] = true
			if _, exists := graph.file[target]; !exists {
				t.Errorf("authority dispatch %s names missing target %s", key, target)
			}
			if !strings.HasSuffix(target, "."+issue.Method) {
				t.Errorf("authority dispatch %s target %s does not implement method %s", key, target, issue.Method)
			}
		}
	}
	for key, targets := range reviewedAuthorityDispatches {
		if _, current := graph.unresolvedAuthorityDispatches[key]; !current {
			t.Errorf("stale authority dispatch review %s; remove it", key)
		}
		if len(targets) == 0 {
			t.Errorf("authority dispatch review %s has no targets", key)
		}
	}
	if len(keys) > 0 {
		t.Logf("reconciled %d interface/unknown-receiver authority dispatches", len(keys))
	}
}

// TestReviewedAuthorityInterfaceImplementations proves that the compact
// interface ledger above cannot silently become a name-based escape hatch. A
// listed concrete receiver must implement every declared method of the exact
// interface, and every target must be a production declaration in this graph.
func TestReviewedAuthorityInterfaceImplementations(t *testing.T) {
	graph := buildCallGraph(t)
	interfaces := make([]string, 0, len(reviewedAuthorityInterfaceImplementations))
	for interfaceName := range reviewedAuthorityInterfaceImplementations {
		interfaces = append(interfaces, interfaceName)
	}
	sort.Strings(interfaces)
	for _, interfaceName := range interfaces {
		if !graph.interfaceTypes[interfaceName] {
			t.Errorf("reviewed authority interface %s is not a current interface declaration", interfaceName)
			continue
		}
		receivers := reviewedAuthorityInterfaceImplementations[interfaceName]
		if len(receivers) == 0 {
			t.Errorf("reviewed authority interface %s has no production receiver", interfaceName)
			continue
		}
		methods := graph.interfaceMethods[interfaceName]
		for _, receiver := range receivers {
			for method := range methods {
				key := receiver + "." + method
				if len(graph.methodsByReceiver[key]) == 0 {
					t.Errorf("reviewed authority receiver %s does not implement %s.%s", receiver, interfaceName, method)
				}
			}
		}
	}
}

func authorityInterfaceImplementers(graph *callGraph, interfaceName string) []string {
	methods := graph.interfaceMethods[interfaceName]
	byReceiver := map[string]bool{}
	for methodKey := range graph.methodsByReceiver {
		receiver, _, ok := strings.Cut(methodKey, ".")
		if !ok || receiver == "" {
			continue
		}
		implements := true
		for method := range methods {
			if len(graph.methodsByReceiver[receiver+"."+method]) == 0 {
				implements = false
				break
			}
		}
		if implements {
			byReceiver[receiver] = true
		}
	}
	implementers := make([]string, 0, len(byReceiver))
	for receiver := range byReceiver {
		implementers = append(implementers, receiver)
	}
	sort.Strings(implementers)
	return implementers
}

func TestAuthorityInterfaceImplementationCensus(t *testing.T) {
	graph := buildCallGraph(t)
	live := graph.authorityReachableFrom(graph.entrypoints)
	interfaces := make([]string, 0, len(reviewedAuthorityInterfaceImplementations))
	for interfaceName := range reviewedAuthorityInterfaceImplementations {
		interfaces = append(interfaces, interfaceName)
	}
	sort.Strings(interfaces)

	seenExclusions := map[string]bool{}
	for _, interfaceName := range interfaces {
		listed := setFromList(reviewedAuthorityInterfaceImplementations[interfaceName])
		for _, receiver := range authorityInterfaceImplementers(graph, interfaceName) {
			exclusionKey := interfaceName + "." + receiver
			exclusion, excluded := reviewedNonLiveAuthorityInterfaceImplementations[exclusionKey]
			if listed[receiver] && excluded {
				t.Errorf("authority interface implementation %s is both live and excluded", exclusionKey)
				continue
			}
			if listed[receiver] {
				continue
			}
			if !excluded || strings.TrimSpace(exclusion.Reason) == "" {
				t.Errorf("unreviewed production implementation %s; list it as live or add a non-live exclusion with a reason", exclusionKey)
				continue
			}
			seenExclusions[exclusionKey] = true
			for method := range graph.interfaceMethods[interfaceName] {
				if live[receiver+"."+method] {
					t.Errorf("non-live implementation %s has entrypoint-reachable method %s.%s", exclusionKey, receiver, method)
				}
			}
			for function, result := range graph.functionResultTypes {
				if result == receiver && live[function] {
					t.Errorf("non-live implementation %s has entrypoint-reachable constructor %s", exclusionKey, function)
				}
			}
		}
	}
	for key, exclusion := range reviewedNonLiveAuthorityInterfaceImplementations {
		if strings.TrimSpace(exclusion.Reason) == "" {
			t.Errorf("non-live authority interface exclusion %s has no reason", key)
		}
		if !seenExclusions[key] {
			t.Errorf("stale/non-implementing authority interface exclusion %s", key)
		}
	}
}

// TestNoUnscannedProductionMoneyOperationsInSubpackages prevents the root
// package's intentionally explicit Glob("*.go") boundary from becoming a
// blind spot. This guard is package-local because its reachability model is
// package-local: a production subpackage that grows a SQL/CopyFrom/provider
// money effect must get its own structural authority scanner rather than
// silently inheriting no coverage at all.
func TestNoUnscannedProductionMoneyOperationsInSubpackages(t *testing.T) {
	err := filepath.Walk(".", func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if path == ".git" || path == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || !strings.Contains(filepath.Clean(path), string(filepath.Separator)) {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			effects := moneyMutationEffectsForCall(call)
			issues := unresolvedMoneyExpressionKinds(call)
			if len(effects) == 0 && len(issues) == 0 && !callPostsToProviderMoneyEndpoint(call) {
				return true
			}
			t.Errorf("unscanned production subpackage money/provider operation in %s at %d; add package-aware structural authority coverage before introducing it", path, fset.Position(call.Pos()).Line)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk production subpackages: %v", err)
	}
}

func TestStructuralMoneySinkRequiresEntrypointReachability(t *testing.T) {
	live := &callGraph{
		edges: map[string]map[string]bool{
			"Server.handleLive": {"plainDBWriter": true},
			"plainDBWriter":     {},
		},
		file: map[string]string{
			"Server.handleLive": "api.go",
			"plainDBWriter":     "generic.go",
		},
		structuralMoneySinks: map[string]bool{"plainDBWriter": true},
		entrypoints:          map[string]bool{"Server.handleLive": true},
	}
	if !observeMoneyAuthority(live).Sinks["plainDBWriter"] {
		t.Fatal("generic structural SQL writer reachable from HTTP entrypoint was not observed")
	}

	disconnected := &callGraph{
		edges: map[string]map[string]bool{
			"plainDBWriter": {},
			"main":          {},
		},
		file: map[string]string{
			"plainDBWriter": "generic.go",
			"main":          "main.go",
		},
		structuralMoneySinks: map[string]bool{"plainDBWriter": true},
		entrypoints:          map[string]bool{"main": true},
	}
	obs := observeMoneyAuthority(disconnected)
	if obs.Sinks["plainDBWriter"] {
		t.Fatal("disconnected structural writer became authority without an entrypoint path")
	}
	_, stale := moneyAuthorityDeclarationDiff(obs.Sinks, map[string]bool{"plainDBWriter": true})
	if got, want := strings.Join(stale, ","), "plainDBWriter"; got != want {
		t.Fatalf("disconnected declared authority stale=%q want %q", got, want)
	}
}

func TestPackageInitializationIsAnEntrypoint(t *testing.T) {
	production := buildCallGraph(t)
	if !production.entrypoints[packageInitializationRoot] || !production.entrypoints["init"] {
		t.Fatalf("buildCallGraph omitted package initialization roots: %#v", production.entrypoints)
	}

	graph := &callGraph{
		edges: map[string]map[string]bool{
			packageInitializationRoot: {"initializerWriter": true},
			"initializerWriter":       {},
		},
		authorityEdges: map[string]map[string]bool{
			packageInitializationRoot: {"initializerWriter": true},
			"initializerWriter":       {},
		},
		file: map[string]string{
			packageInitializationRoot: "<package initialization>",
			"initializerWriter":       "initializer.go",
		},
		structuralMoneySinks: map[string]bool{"initializerWriter": true},
		entrypoints:          map[string]bool{packageInitializationRoot: true},
	}
	if !observeMoneyAuthority(graph).Sinks["initializerWriter"] {
		t.Fatal("top-level initializer path was treated as disconnected")
	}
}

func TestUnresolvedMoneyMethodSelectorFailsClosed(t *testing.T) {
	graph := &callGraph{
		methodsByReceiver: map[string][]string{"Store.Write": {"Store.Write"}},
		authorityEdges:    map[string]map[string]bool{"caller": {}, "Store.Write": {}},
		structuralMoneySinks: map[string]bool{
			"Store.Write": true,
		},
		unresolvedAuthorityDispatches:         map[string]authorityDispatchIssue{},
		unresolvedAuthorityDispatchCandidates: map[string]authorityDispatchIssue{},
	}
	selector := callExprNamed(t, `package main
func caller(opaque any) { opaque.Write() }
`, "Write").Fun
	_, unresolved := graph.authorityTargetsForExpr(selector, authorityResolutionContext{bindings: map[string]string{}})
	if unresolved != "Write" {
		t.Fatalf("unresolved selector collision=%q want Write", unresolved)
	}
	addAuthorityTargets(graph, "caller", selector, authorityResolutionContext{bindings: map[string]string{}}, "synthetic.go", 2, graph.authorityEdges["caller"])
	finalizeAuthorityDispatches(graph)
	if len(graph.unresolvedAuthorityDispatches) != 1 {
		t.Fatalf("unresolved money selector was not promoted to a reviewed dispatch boundary: %#v", graph.unresolvedAuthorityDispatches)
	}
}

func TestStructuralMoneySinkFindsGenericProviderPOST(t *testing.T) {
	call := firstCallExpr(t, `package main
func innocuous(client any) {
  client.NewRequestWithContext(nil, http.MethodPost, "https://api.stripe.com/v1/refunds", nil)
}`)
	if !callPostsToProviderMoneyEndpoint(call) {
		t.Fatal("generic provider POST to a money endpoint was not structurally classified")
	}
}

func TestStripePOSTOperationLedger(t *testing.T) {
	entries, err := filepath.Glob("*.go")
	must(t, err)
	seen := map[string]bool{}
	for _, name := range entries {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		mustf(t, err, "parse %s: %v", name, err)
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || callExprFuncName(call.Fun) != "stripeForm" {
				return true
			}
			if len(call.Args) < 2 {
				t.Errorf("stripeForm call in %s has no operation path", name)
				return true
			}
			path, ok := staticStringExpression(call.Args[1])
			if !ok {
				t.Errorf("stripeForm call in %s has a dynamic operation path", name)
				return true
			}
			seen[path] = true
			return true
		})
	}
	if len(seen) == 0 {
		t.Fatal("no production stripeForm call was found")
	}
	if got, want := len(seen), len(reviewedStripePOSTOperations); got != want {
		t.Errorf("Stripe operation ledger count=%d want %d", got, want)
	}
	for path := range seen {
		if _, reviewed := reviewedStripePOSTOperations[path]; !reviewed {
			t.Errorf("production stripeForm path %q is absent from the operation ledger", path)
		}
	}
	for path, kind := range reviewedStripePOSTOperations {
		if !seen[path] {
			t.Errorf("stale Stripe operation ledger entry %q", path)
		}
		form := url.Values{}
		wantOperation := paymentOperationSetup
		switch kind {
		case "charge":
			form.Set("amount", "1")
			form.Set("currency", "cad")
			wantOperation = paymentOperationCharge
		case "refund":
			form.Set("amount", "1")
			wantOperation = paymentOperationRefund
		case "setup":
		default:
			t.Errorf("Stripe operation ledger %q has unknown kind %q", path, kind)
			continue
		}
		operation, _, _, err := stripePOSTOperation(path, form)
		if err != nil || operation != wantOperation {
			t.Errorf("stripePOSTOperation(%q)=(%q,%v), want %q,nil", path, operation, err, wantOperation)
		}
	}
	if _, _, _, err := stripePOSTOperation("unreviewed_operation", url.Values{}); err == nil {
		t.Fatal("unreviewed Stripe operation path was accepted")
	}
}

func TestStripePOSTOutsideLegacyFragmentSetIsFailClosed(t *testing.T) {
	call := firstCallExpr(t, `package main
func innocuous(client any) {
  client.NewRequestWithContext(nil, http.MethodPost, "https://api.stripe.com/v1/subscriptions", nil)
}`)
	if !callPostsToProviderMoneyEndpoint(call) {
		t.Fatal("Stripe POST outside the legacy fragment list was not structurally classified")
	}

	setup := firstCallExpr(t, `package main
func setup(ctx any, form any) {
  stripeForm(ctx, "accounts", form, "")
}`)
	if !callPostsToProviderMoneyEndpoint(setup) {
		t.Fatal("reviewed Stripe setup operation was not retained as provider authority")
	}
}

func TestUnconstrainedCLITransportRemainsStructuralAuthority(t *testing.T) {
	graph := buildCallGraph(t)
	if !graph.structuralMoneySinks["client.doHeaders"] {
		t.Fatal("caller-configured CLI transport was not retained as a structural money sink")
	}
	if !observeMoneyAuthority(graph).Sinks["client.doHeaders"] {
		t.Fatal("entrypoint-reachable CLI transport was not observed as authority")
	}
}

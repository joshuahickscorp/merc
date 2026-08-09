#!/usr/bin/env python3
"""Emit one machine-readable state receipt for the current HEAD.

Written because a progress report is prose and prose drifts. Every field here is
read from the tree, from git, from the product's own authority code, or from a
live database — never from a previous report. Where a fact cannot be determined
without a database, the field says so rather than guessing.

Two fields in the first version were hard-coded rather than derived —
`full_merc_chain_proof: False` and `lifecycle_ceiling_until_chain: "VALIDATED"` —
and they went stale the moment the chain completed, which is precisely the drift
this file exists to prevent. Anything that can be computed is now computed, and
the runtime digests come from `merc dev authority`, which calls the same functions
admission and dispatch call.

    python3 scripts/branch-state-receipt.py \
        --database-url postgres://cx:cx@localhost:5432/merc_state \
        --out evidence/state/branch-state.json

The database is optional. Without it the receipt still reports everything the
tree knows and marks the projection fields `not_probed`.
"""

import argparse
import json
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def git(*args, default=""):
    try:
        return subprocess.run(
            ["git", "-C", ROOT, *args],
            capture_output=True, text=True, check=True,
        ).stdout.strip()
    except (subprocess.CalledProcessError, FileNotFoundError):
        return default


def read(path):
    with open(os.path.join(ROOT, path)) as handle:
        return handle.read()


def grep_count(pattern, *paths, exclude_tests=True):
    """Files whose CODE matches, excluding _test.go when asked.

    Returns FILES, not occurrences. A caller that wants a count of definitions
    must use count_matches — reporting file count as case count reported the
    fourteen-case failure matrix as two, because it lives in two files.
    """
    hits = []
    for path in paths:
        full = os.path.join(ROOT, path)
        if not os.path.isdir(full):
            continue
        for entry in sorted(os.listdir(full)):
            if not entry.endswith(".go") or (exclude_tests and entry.endswith("_test.go")):
                continue
            if re.search(pattern, read(os.path.join(path, entry))):
                hits.append(f"{path}/{entry}")
    return hits


def count_matches(pattern, *paths, exclude_tests=False):
    """Total occurrences, across files. See grep_count for why both exist."""
    total = 0
    for path in paths:
        full = os.path.join(ROOT, path)
        if not os.path.isdir(full):
            continue
        for entry in sorted(os.listdir(full)):
            if not entry.endswith(".go") or (exclude_tests and entry.endswith("_test.go")):
                continue
            total += len(re.findall(pattern, read(os.path.join(path, entry))))
    return total


def authority_state():
    doc = json.loads(read("control/runtime-authority.json"))
    profiles = []
    for profile in doc["runtimes"]:
        cells = []
        for cell in profile["cells"]:
            own = cell.get("lifecycle", "")
            effective = own or profile["lifecycle"]
            cells.append({
                "cell_id": cell["id"],
                "workload": cell["job"],
                "model": cell["model"],
                "wire_kind": cell.get("wire_kind") or "(inherits model)",
                "verification": cell["verification"],
                "declared_lifecycle": own or "(inherits profile)",
                "effective_lifecycle": effective,
                "routable": effective in ("CANARY", "ACTIVE"),
                "quality_tier": cell.get("quality_tier", ""),
                "benchmark_authority": cell.get("benchmark_authority", ""),
                "rejection_reason": cell.get("rejection_reason", ""),
                "max_batch": cell.get("max_batch", 0),
                "max_concurrency": cell.get("max_concurrency", 0),
            })
        profiles.append({
            "runtime_profile_id": profile["runtime_id"],
            "revision": profile["revision"],
            "engine": profile["engine"],
            "lifecycle": profile["lifecycle"],
            "cells": cells,
        })
    return doc["matrix_version"], profiles


def probe_database(url):
    """Read what the database actually projects, not what the document says."""
    def q(sql):
        out = subprocess.run(
            ["psql", url, "-tAF", "\x1f", "-c", sql],
            capture_output=True, text=True,
        )
        if out.returncode != 0:
            raise RuntimeError(out.stderr.strip())
        # Strip NEWLINES only. str.strip() also removes the field separator, so a
        # row whose last column is empty came back one field short and the caller
        # indexed off the end — a receipt that fails on exactly the rows with the
        # least to say.
        return [line.split("\x1f") for line in out.stdout.strip("\n").splitlines() if line]

    profiles = [
        {"runtime_profile_id": r[0], "revision": r[1], "lifecycle": r[2],
         "routable": r[3] == "t", "is_current": r[4] == "t", "digest": r[5]}
        for r in q("SELECT runtime_profile_id, revision, lifecycle, routable, is_current,"
                   " profile_digest FROM runtime_profiles ORDER BY 1,2")
    ]
    cells = [
        {"runtime_profile_id": r[0], "revision": r[1], "cell_id": r[2],
         "lifecycle": r[3], "routable": r[4] == "t", "quality_tier": r[5],
         "benchmark_authority": r[6]}
        for r in q("SELECT runtime_profile_id, revision, cell_id, lifecycle, routable,"
                   " quality_tier, benchmark_authority FROM runtime_profile_models"
                   " ORDER BY 1,2,3")
    ]
    workers = q(
        "SELECT COUNT(*),"
        " COUNT(*) FILTER (WHERE runtime_profile_id IS NOT NULL),"
        " COUNT(*) FILTER (WHERE runtime_profile_id IS NULL),"
        " COUNT(*) FILTER (WHERE runtime_profile_digest IS NOT NULL)"
        " FROM workers")[0]
    constraints = [r[0] for r in q(
        "SELECT conname FROM pg_constraint WHERE conname IN"
        " ('workers_engine_valid','workers_runtime_profile_identity',"
        "  'workers_runtime_profile_fk')")]
    columns = [r[0] for r in q(
        "SELECT column_name || ':' || is_nullable FROM information_schema.columns"
        " WHERE table_name='workers' AND column_name IN"
        " ('engine','runtime_profile_id','runtime_profile_revision','runtime_profile_digest')"
        " ORDER BY 1")]
    tables = [r[0] for r in q(
        "SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename IN"
        " ('inflight_executions','inflight_requests','execution_overhead_actuals',"
        "  'plan_actuals','runtime_profile_models') ORDER BY 1")]
    return {
        "profiles": profiles,
        "cells": cells,
        "workers": {
            "total": int(workers[0]),
            "bound_to_profile": int(workers[1]),
            "unbound": int(workers[2]),
            "with_digest": int(workers[3]),
        },
        "present_constraints": constraints,
        "worker_columns": columns,
        "present_tables": tables,
    }


def dev_authority():
    """Runtime authority as the PRODUCT computes it, not as the document reads.

    Shelling to the binary rather than reimplementing the digest in Python is the
    whole point: a second implementation would drift, and a receipt whose numbers
    are its own opinion is not evidence.
    """
    out = subprocess.run(
        ["go", "run", ".", "dev", "authority"],
        cwd=os.path.join(ROOT, "control"),
        capture_output=True, text=True,
    )
    if out.returncode != 0:
        return {"error": out.stderr.strip()[-2000:]}
    try:
        return json.loads(out.stdout)
    except ValueError as error:
        return {"error": f"unparseable: {error}"}


def callers(pattern, *, exclude=()):
    """Production callers only: no tests, no definition sites the caller names."""
    hits = grep_count(pattern, "control")
    return [h for h in hits if not any(h.endswith(e) for e in exclude)]


def latest_checkpoint_receipt():
    directory = os.path.join(ROOT, "evidence", "checkpoint")
    if not os.path.isdir(directory):
        return {"present": False, "reason": "no evidence/checkpoint directory"}
    receipts = []
    for entry in sorted(os.listdir(directory)):
        if not entry.endswith(".json"):
            continue
        try:
            with open(os.path.join(directory, entry)) as handle:
                receipts.append(json.load(handle))
        except (OSError, ValueError):
            continue
    if not receipts:
        return {"present": False, "reason": "no receipts on disk"}
    head = git("rev-parse", "HEAD")
    for receipt in receipts:
        if receipt.get("head") == head:
            steps = receipt.get("steps", [])
            return {
                "present": True,
                "binds_current_head": True,
                "head": receipt["head"],
                "worktree_digest": receipt.get("worktree_digest", ""),
                "mutation_restored": receipt.get("mutation_restored", False),
                "steps": {s["name"]: s.get("exit_code", 1) for s in steps},
                "push_eligible": (
                    receipt.get("mutation_restored", False)
                    and bool(steps)
                    and all(s.get("exit_code", 1) == 0 and not s.get("skipped")
                            for s in steps)
                ),
            }
    return {
        "present": True,
        "binds_current_head": False,
        "receipts_for": [r.get("head", "")[:12] for r in receipts],
        "current_head": head[:12],
    }


def production_callers(symbol):
    """Non-test references to a symbol, excluding its own declaration and comments.

    Crude but honest: a line that declares `func X(` or `X = ` or that begins with
    a comment is not a call. Everything else in a non-test .go file is. This is
    what separates "the function exists" from "something runs it", which is the
    single distinction every stale progress report in this repository got wrong.
    """
    out = []
    for path in ("control", "agent/src"):
        full = os.path.join(ROOT, path)
        if not os.path.isdir(full):
            continue
        for entry in sorted(os.listdir(full)):
            if not (entry.endswith(".go") or entry.endswith(".rs")):
                continue
            if entry.endswith("_test.go"):
                continue
            for number, line in enumerate(read(os.path.join(path, entry)).splitlines(), 1):
                if symbol not in line:
                    continue
                stripped = line.strip()
                if stripped.startswith("//") or stripped.startswith("--"):
                    continue
                if re.search(rf"^\s*(func|pub fn|fn)\s+(\([^)]*\)\s*)?{re.escape(symbol)}\b", line):
                    continue
                if re.search(rf"^\s*{re.escape(symbol)}\s*(=|:)", line):
                    continue
                out.append(f"{path}/{entry}:{number}")
    return out


def classify(symbol_callers, *, schema_present, note):
    if symbol_callers:
        return {"classification": "PRODUCTION_WIRED", "callers": symbol_callers, "note": note}
    if schema_present:
        return {"classification": "IMPLEMENTED_UNWIRED", "callers": [], "note": note}
    return {"classification": "ABSENT", "callers": [], "note": note}


def schema_has(schema, *fragments):
    return {fragment: (fragment in schema) for fragment in fragments}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--database-url", default=os.environ.get("MERC_STATE_DATABASE_URL", ""))
    parser.add_argument("--out", default="")
    args = parser.parse_args()

    matrix_version, profiles = authority_state()
    schema = read("control/schema.sql")
    authority = dev_authority()
    api_source = read("control/api.go")
    store_jobs_source = read("control/store_jobs.go")
    project_submit_tests = read("control/project_submit_test.go")
    project_declaration_source = read("control/project_declaration.go")
    project_declaration_tests = read("control/project_declaration_test.go")
    pricing_source = read("control/pricing.go")
    pricing_tests = read("control/pricing_test.go")
    pricing_governance_tests = read("control/pricing_governance_test.go")

    # The definition site is not a caller. Excluding it is the difference between
    # "wired" and "declared", which is the exact thing prose gets wrong.
    coalescing_callers = callers(
        r"ClaimInflightExecution\(", exclude=("inflight_coalescing.go",))
    coalescing_resolvers = callers(
        r"ResolveInflightSuccess\(|ResolveInflightFailure\(|AwaitInflightResult\(",
        exclude=("inflight_coalescing.go",))
    activation_writers = callers(
        r"ApplyActivationPolicy\(|syncActivationPolicy\(|loadActivationAtStartup\(",
        exclude=("activation_policy.go",))
    verification_class_writers = callers(
        r"deriveTaskVerificationClass\(|governComputePlanVerificationClass\(",
        exclude=("verification_class.go",))

    receipt = {
        "schema_version": 1,
        "generated_from": "HEAD, the working tree, and (optionally) a live database",
        "source": {
            "commit": git("rev-parse", "HEAD"),
            "branch": git("rev-parse", "--abbrev-ref", "HEAD"),
            "tree_clean": git("status", "--porcelain") == "",
            "pushed": git("rev-parse", "HEAD") == git("rev-parse", "@{upstream}", default="?"),
        },
        "authority_document": {
            "matrix_version": matrix_version,
            "profiles": profiles,
            "routable_cells": [
                f"{p['runtime_profile_id']}/{c['cell_id']}"
                for p in profiles for c in p["cells"] if c["routable"]
            ],
            "directed_only_cells": [
                f"{p['runtime_profile_id']}/{c['cell_id']}"
                for p in profiles for c in p["cells"]
                if not c["routable"] and c["effective_lifecycle"] in
                ("VALIDATED", "REAL_RUNTIME_PROVEN")
            ],
            "rejected_cells": [
                f"{p['runtime_profile_id']}/{c['cell_id']}"
                for p in profiles for c in p["cells"]
                if c["effective_lifecycle"] == "REJECTED_FOR_CONTRACT"
            ],
        },
        "runtime_authority_computed": authority,
        "activation_policy": {
            "table_declared": "runtime_activation_policies" in schema,
            "append_only_trigger":
                "runtime_activation_policies_append_only" in schema,
            "promotion_receipt_required":
                "runtime_activation_policies_promotion_evidenced" in schema,
            "capability_manifest_version_column":
                "capability_manifest_version" in schema,
            "digest_history_table":
                "runtime_profile_digest_history" in schema,
            "production_writers": activation_writers,
        },
        "capability_wiring": {
            "coalescing_production_callers": coalescing_callers,
            "coalescing_resolver_callers": coalescing_resolvers,
            "coalescing_lease_table": "inflight_executions" in schema,
            # Successful followers are not inferred from the short-lived lease
            # counter. Their physical-leader relationship and avoided supplier
            # entitlement are written atomically with the logical settlement.
            # This says only that the production writer/table exists in source;
            # it is not a claim of a real vLLM execution or true net cost.
            "coalescing_receipt_provenance": {
                "delivery_table_declared": "realtime_coalesced_deliveries" in schema,
                "production_references": production_callers(
                    "realtime_coalesced_deliveries"),
                "leader_contract_required": bool(re.search(
                    r"leader_contract_id\s+UUID\s+NOT\s+NULL\s+REFERENCES\s+execution_contracts",
                    schema)),
                "counterfactual_entitlement_recorded":
                    "counterfactual_supplier_entitlement_nanos" in schema,
                "handler_money_receipt_test":
                    "TestProductionRealtimeCoalescing128DeliveriesOnePhysicalSettlement"
                    in read("control/coalesced_cluster_money_test.go"),
            },
            "coalescing_status": (
                "handler_wired" if coalescing_callers else "primitive_only"),
            # Quote-bound batch work uses one immutable PricingDecision. This
            # intentionally reports source and integration-test presence, not a
            # checkpoint or a Stripe/CAD external receipt.
            "quote_bound_pricing_identity": {
                "server_reuses_reviewed_decision": "pricingDecision = qBind.Pricing" in api_source,
                "store_requires_exact_digest": "pricingSHA256 != quotePricingSHA256" in store_jobs_source,
                "derived_origin_link_not_accepted_for_bound_jobs":
                    "job pricing decision does not exactly match its bound quote" in store_jobs_source,
                "cad_project_public_authority_refusal_tests": {
                    "admission_refuses_without_scope_compatible_authority":
                        "TestProjectCompilerCADEmbedAdmissionRefusesWithoutScopeCompatibleAuthority"
                        in project_submit_tests,
                    "execution_refuses_before_durable_mutation":
                        "TestProjectCompilerCADEmbedExecutionRefusesBeforeDurableMutation"
                        in project_submit_tests,
                },
            },
            # The catalogue has exactly one buyer-price derivation: the governed
            # market board schedule. Supplier economics remains an explicitly
            # non-authoritative cost-floor comparison for the market-gap report;
            # this structural evidence is deliberately not a true-net or market
            # clearing claim.
            "catalogue_pricing_authority": {
                "schedule_builder_present": "func BuildCataloguePriceSchedule" in pricing_source,
                "published_results_are_schedule_only": bool(re.search(
                    r"func PublishedCatalogueResults\(\).*?BuildCataloguePriceSchedule\(\)",
                    pricing_source, re.DOTALL)),
                "market_board_price_deriver_present": "func repriceFromMarketBoard" in pricing_source,
                "alternate_cost_plus_catalogue_deriver_absent":
                    "repriceFromSupplierEconomics" not in pricing_source,
                "diagnostic_cost_floor_callers": production_callers(
                    "diagnosticCostFloorFromSupplierEconomics"),
                "regression_tests": [
                    name for name in (
                        "TestPublishedCatalogueResultsOmitsUnmeasuredModels",
                        "TestDiagnosticCostFloorMathIsCorrect",
                        "TestShippedBoardPublishesAViablePrice",
                    ) if name in pricing_tests or name in pricing_governance_tests
                ],
                "scope": "source-and-test authority boundary only; not a live market, true-net, or Stripe receipt",
            },
            # The compiler is a product command and its CAD refusal tests reach
            # real authenticated handlers. Current embed performance and
            # settlement units have no governed conversion, so the source
            # truth is a buyer-visible 503 with zero durable writes—not an
            # admission, execution, or settlement claim.
            "project_compiler": {
                "production_command": bool(grep_count(r"func dispatchProject", "control")),
                "buyer_approved_probe_required":
                    "project quote requires an exact buyer-approved bounded probe"
                    in read("control/project_quote.go"),
                "public_cad_authority_refusal_tests": {
                    "admission_refuses_without_scope_compatible_authority":
                        "TestProjectCompilerCADEmbedAdmissionRefusesWithoutScopeCompatibleAuthority"
                        in project_submit_tests,
                    "execution_refuses_before_durable_mutation":
                        "TestProjectCompilerCADEmbedExecutionRefusesBeforeDurableMutation"
                        in project_submit_tests,
                },
                "dependent_step_submit_refused":
                    "currently supports only independent finite steps"
                    in read("control/project_submit.go"),
                "dependency_artifact_dataflow_required":
                    "validateProjectArtifactDataflow(declaration.Steps)"
                    in project_declaration_source,
                "dependency_artifact_dataflow_test":
                    "TestProjectDeclarationRequiresArtifactBoundDependencies"
                    in project_declaration_tests,
            },
            "second_runtime_driver": bool(
                os.path.exists(os.path.join(ROOT, "agent/src/runtime_driver.rs"))),
            "directed_routing": bool(
                grep_count(r"func buildWorkloadDecisionDirected", "control")),
            "selector_type_present": bool(grep_count(r"RuntimeSelector", "control")),
            # Probed against what the selector was actually built as, not against
            # three table names from a design that was never implemented. The
            # decisions live in runtime_shadow_selections; the OUTCOME side is a
            # read over `tasks` rather than a table, because every fact it needs is
            # already persisted by the money path (control/runtime_cell_cost.go);
            # and a promotion is an entry in runtime_activation_policies carrying
            # the receipt the gate in control/runtime_cell_promotion.go derives.
            # Reporting the old names as absent described a gap that does not exist
            # while hiding the one that does — see selector_cost_basis below.
            "selector_tables": schema_has(
                schema, "runtime_shadow_selections", "runtime_activation_policies"),
            "selector_cost_basis": {
                "measured_cost_read": bool(
                    grep_count(r"func \(s \*Store\) MeasuredCellCostsByHardware", "control")),
                "regret_read": bool(
                    grep_count(r"func \(s \*Store\) SelectorRegretForScope", "control")),
                "promotion_gate": bool(
                    grep_count(r"func \(s \*Store\) EvaluateCellPromotion", "control")),
                "shadow_basis_recorded": "selection_basis" in schema,
                # The gap that remains: no cohort has been driven through live
                # agents, so the regret figure has no production cohort behind it.
                "paired_cohort_receipt": bool(
                    os.path.exists(os.path.join(ROOT, "evidence/perf/selector"))),
            },
            "tokenization_cache_callers": grep_count(
                r"TokenizationCache|tokenizationCache", "control"),
            "tool_schema_cache_callers": grep_count(
                r"ToolSchemaCache|toolSchemaCache", "control"),
            "exact_reuse_batch_enabled":
                "const batchExactReuseEnabled = false" not in
                read("control/exact_reuse_batch.go"),
            "batching_authority": "control/batch_policy.go:SelectBatch"
            if grep_count(r"func SelectBatch", "control") else "absent",
            "batching_traffic_classes": [
                name for name in
                ("INTERACTIVE", "BATCH_PRIORITY", "BATCH_STANDARD", "BACKGROUND")
                if grep_count(name, "control")
            ],
            "overhead_actuals_table": "execution_overhead_actuals" in schema,
            "overhead_actuals_writers": callers(
                r"execution_overhead_actuals", exclude=()),
            "verification_class_writers": verification_class_writers,
            "verification_classes_declared": schema_has(
                schema, "tasks_verification_class_known",
                "tasks_derive_verification_class",
                "verification_work_governed_class_selected"),
            # A TEST harness, named as one. It is what makes the chain and
            # failure-matrix proofs possible, and it is not a production caller;
            # reporting it under either label alone would be misleading.
            "artifact_test_harness": grep_count(
                r"func newArtifactHarness", "control", exclude_tests=False),
            "checkpoint_cli": bool(grep_count(r"func runDevCheckpoint", "control")),
            "pre_push_hook_present": os.path.exists(
                os.path.join(ROOT, ".githooks", "pre-push")),
        },
        "checkpoint_authority": latest_checkpoint_receipt(),
        # Mechanical caller counts, not a reading of the code's intentions. A
        # symbol whose only non-test appearance is its own declaration is dead,
        # whatever the comment above it says it is for.
        "dead_or_unwired_symbols": {
            symbol: production_callers(symbol)
            for symbol in (
                "RenewInflightLease",
                "sweepExpiredInflight",
                "InflightFollowers",
                "ClassCoalescedDelivery",
                "buildWorkloadDecisionDirected",
                "EvictPrefixCacheToBudget",
                "DeepestWarmPrefix",
                "PrefixCacheValue",
                "preferenceForTier",
                "SelectBatch",
                "TokenBudgetFor",
            )
        },
        "unresolved_migrations": {
            # Read from the schema text, because the intent lives there and the
            # database only shows the end state of whatever ran.
            "workers_engine_valid_still_declared":
                "workers_engine_valid CHECK (engine = 'candle')" in schema,
            "workers_engine_valid_conditionally_dropped":
                "workers_engine_valid retained" in schema,
            "runtime_profile_id_nullable_by_design":
                "worker_capability_requires_profile" in schema,
        },
        # DERIVED. The first version of this block asserted
        # full_merc_chain_proof: False and a VALIDATED ceiling, and both went
        # stale the moment the chain completed. Nothing here is a constant that
        # a previous report chose.
        "second_runtime_evidence": {
            "benchmark_receipt":
                "evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r1.json",
            "autonomous_agent_chain_tests": [
                name for name in (
                    "control/two_agent_enrollment_test.go",
                    "control/second_runtime_chain_test.go",
                    "control/second_runtime_verification_test.go",
                    "control/rejection_economics_test.go",
                    "control/receipt_tamper_test.go",
                    "control/failure_matrix_test.go",
                    "control/failure_matrix_agent_test.go",
                ) if os.path.exists(os.path.join(ROOT, name))
            ],
            "chain_receipt_present": os.path.exists(
                os.path.join(ROOT, "evidence/chain/two-agent-product-chain.json")),
            "llama_cpp_cell_lifecycles": {
                cell["cell_id"]: cell["effective_lifecycle"]
                for p in profiles if p["runtime_profile_id"] == "llama_cpp_metal"
                for cell in p["cells"]
            },
            "failure_matrix_cases": count_matches(
                r"func TestFailureMatrix\w+\(", "control"),
            "failure_matrix_agent_process_cases": count_matches(
                r"func TestFailureMatrix(AgentDeath|RuntimeUnavailable)\w*\(", "control"),
        },
        "grok_status": "DEFERRED_USAGE_LIMIT",
        "adversarial_review_label": "NOT_GROK_INDEPENDENT",
    }

    if args.database_url:
        try:
            receipt["database_projection"] = probe_database(args.database_url)
        except Exception as error:  # noqa: BLE001 - reported, never swallowed
            receipt["database_projection"] = {"error": str(error)}
    else:
        receipt["database_projection"] = "not_probed"

    if args.out:
        path = os.path.join(ROOT, args.out)
        _scripts = os.path.join(ROOT, "scripts")
        if _scripts not in sys.path:
            sys.path.insert(0, _scripts)
        from lib.evidence_binding import EvidenceBindingError, emit_bound_json
        try:
            emit_bound_json(
                path,
                receipt,
                harness="scripts/branch-state-receipt.py",
                repo_root=ROOT,
                build_binary_path=os.path.join(ROOT, "scripts", "branch-state-receipt.py"),
                exact_config="branch-state probe config embedded",
                raw_samples="database_projection + file probes embedded",
            )
        except EvidenceBindingError as exc:
            print(f"REFUSED evidence write: {exc}", file=sys.stderr)
            raise SystemExit(2)
        print(f"state receipt written to {args.out}", file=sys.stderr)
    else:
        print(json.dumps(receipt, indent=2))


if __name__ == "__main__":
    main()

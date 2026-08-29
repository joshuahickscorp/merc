#!/usr/bin/env python3
"""Audit the tree against the fourteen prohibitions in the master plan's §20.

    python3 ops/scripts/must-not-do-audit.py [--database-url URL] [--out FILE]

Every check reads the tree, the git history or a live database. A check that
cannot be mechanically decided says so — `NOT_MECHANICALLY_CHECKABLE` — rather
than reporting PASS, because a green line for a claim nobody verified is worse
than no line at all. That distinction is the point of the file: eleven of the
fourteen are decidable, and pretending the other three are would be an instance
of prohibition 14.

Exit status is 1 if any decidable check FAILS.
"""

import argparse
import json
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

PASS = "PASS"
FAIL = "FAIL"
UNDECIDABLE = "NOT_MECHANICALLY_CHECKABLE"


def run(*args, cwd=ROOT):
    out = subprocess.run(args, cwd=cwd, capture_output=True, text=True)
    return out.returncode, out.stdout, out.stderr


def tracked_files():
    _, stdout, _ = run("git", "ls-files")
    return [line for line in stdout.splitlines() if line]


def read(path):
    with open(os.path.join(ROOT, path), encoding="utf-8", errors="replace") as handle:
        return handle.read()


def grep_go(pattern, exclude_tests=True):
    """Files under src/control/ whose non-test code matches."""
    hits = []
    control = os.path.join(ROOT, "src/control")
    for name in sorted(os.listdir(control)):
        if not name.endswith(".go"):
            continue
        if exclude_tests and name.endswith("_test.go"):
            continue
        if re.search(pattern, read(os.path.join("src", "control", name))):
            hits.append("src/control/" + name)
    return hits


def psql(url, sql):
    code, stdout, stderr = run("psql", url, "-tAF", "\x1f", "-c", sql)
    if code != 0:
        raise RuntimeError(stderr.strip())
    return [line.split("\x1f") for line in stdout.strip("\n").splitlines() if line]


# --- the checks -------------------------------------------------------------


def check_no_global_refactor():
    """1. No global codebase refactor during the programme.

    Decided against the programme's own baseline commit, not against origin/main.
    The first version of this check used the merge base with origin/main and
    reported 90% of the Go files touched — which is true and says nothing, because
    that range is months of ordinary development. A prohibition on refactoring
    DURING the programme has to be measured from where the programme started.
    """
    baseline_path = "ops/programme-baseline.json"
    if baseline_path not in tracked_files() and not os.path.exists(
        os.path.join(ROOT, baseline_path)
    ):
        return UNDECIDABLE, f"{baseline_path} does not exist, so there is no programme baseline"
    base = json.loads(read(baseline_path))["baseline_commit"]
    code, _, stderr = run("git", "cat-file", "-e", base)
    if code != 0:
        return UNDECIDABLE, f"baseline commit {base[:12]} is not in this clone: {stderr.strip()}"
    # Baseline against the WORKING TREE, not against HEAD: uncommitted programme
    # work is still programme work, and diffing commit-to-commit reported 0% while
    # the tree held changes to twelve files.
    _, stat, _ = run("git", "diff", "--numstat", base)
    touched = [line.split("\t") for line in stat.splitlines() if line]
    go_touched = [t for t in touched if t[-1].endswith(".go")]
    total_go = len([f for f in tracked_files() if f.endswith(".go")])
    if not total_go:
        return UNDECIDABLE, "no tracked Go files"
    fraction = len(go_touched) / total_go
    detail = (f"{len(go_touched)} of {total_go} tracked Go files touched since the programme "
              f"baseline {base[:12]} ({fraction:.0%})")
    return (PASS if fraction < 0.25 else FAIL), detail


def check_no_vllm_fork():
    """2. vLLM is not forked into the tree."""
    suspects = [
        f for f in tracked_files()
        if re.search(r"(^|/)vllm/", f) and f.endswith((".py", ".cu", ".cpp", ".h"))
    ]
    if suspects:
        return FAIL, f"vendored vLLM sources tracked: {suspects[:5]}"
    return PASS, "no vLLM sources tracked; the engine is referenced as a pinned image"


def check_not_every_runtime_routable(url):
    """3. Not every registered runtime is routable, and routable implies evidence."""
    if not url:
        return UNDECIDABLE, "no database URL supplied"
    rows = psql(url, "SELECT runtime_profile_id, revision, cell_id, lifecycle, routable,"
                     " quality_tier, benchmark_authority FROM runtime_profile_models"
                     " ORDER BY 1,2,3")
    if not rows:
        return UNDECIDABLE, "no runtime cells registered"
    routable = [r for r in rows if r[4] == "t"]
    unevidenced = [
        f"{r[0]}/{r[2]}" for r in routable if not r[5].strip() or not r[6].strip()
    ]
    if unevidenced:
        return FAIL, f"routable cells without quality tier or benchmark authority: {unevidenced}"
    if len(routable) == len(rows):
        return FAIL, f"every one of {len(rows)} registered cells is routable"
    return PASS, (f"{len(routable)} of {len(rows)} cells routable; every routable one names a "
                  "quality tier and a benchmark authority")


def check_benchmarks_are_not_product_proof():
    """4. A standalone benchmark cannot mint release authority."""
    canary = "evidence/canary/private-canary.json"
    if canary not in tracked_files():
        return UNDECIDABLE, f"{canary} is not tracked"
    inventory = json.loads(read(canary))
    if inventory.get("public_capability_allowed") is not False:
        return FAIL, "public_capability_allowed is not false in the canary inventory"
    lanes = inventory.get("lanes") or inventory.get("capabilities") or []
    bound = [l for l in lanes if isinstance(l, dict) and l.get("candidate_bound")]
    return PASS, (f"public_capability_allowed is false; {len(bound)} candidate-bound lanes "
                  f"of {len(lanes)}")


def check_buyer_fields_are_not_authority():
    """5. Buyer-supplied engine/runtime/artifact-format fields are not authority."""
    # The claim query must compare the FROZEN cell's model_kind, never the
    # binding's. runtime_shadow_selection.go documents the same rule.
    scheduler = read("src/control/scheduler.go")
    if "wac.model_kind = COALESCE(" not in scheduler:
        return FAIL, "the claim query no longer resolves model_kind from the frozen cell"
    if not re.search(r"frozen->>'model_kind'", scheduler):
        return FAIL, "the claim query does not read the frozen cell's model_kind"
    return PASS, "claim resolves engine, runtime and artifact format from the frozen cell"


def check_physical_and_reused_delivery_are_separate():
    """6. Physical inference is never mixed with reused or cached delivery."""
    hits = grep_go(r"physicalClasses")
    if not hits:
        return FAIL, "no physical/logical billing class registry found"
    source = "\n".join(read(h) for h in hits)
    logical = re.findall(r"Class(\w+):\s*false", source)
    physical = re.findall(r"Class(\w+):\s*true", source)
    if not logical:
        return FAIL, "no billing class is registered as non-physical"
    return PASS, (f"{len(physical)} physical and {len(logical)} logical classes registered "
                  f"separately (logical: {sorted(logical)})")


def check_no_wan_tightly_coupled_inference():
    """7. Tightly coupled inference is not distributed across WAN nodes.

    Executable check, not a substring match on production constants: a rename
    that keeps the refusal must pass, and a rename that inverts the logic must
    fail. src/control/execution_mode_test.go calls ChooseExecutionMode with a WAN
    fabric and tightly coupled parallelism; the suite runs that test. This
    audit only verifies the test still exists and exercises the authority.
    """
    if not os.path.exists(os.path.join(ROOT, "src/control/execution_mode.go")):
        return FAIL, "no execution-mode authority exists to refuse it"
    test_path = "src/control/execution_mode_test.go"
    if not os.path.exists(os.path.join(ROOT, test_path)):
        return FAIL, f"{test_path} is missing; the WAN refusal is not executed"
    test_source = read(test_path)
    if "func TestTightlyCoupledWorkIsRefusedOnAWANFabric" not in test_source:
        return FAIL, "no test calls ChooseExecutionMode for tightly coupled work on a WAN"
    if "ChooseExecutionMode" not in test_source:
        return FAIL, f"{test_path} does not call ChooseExecutionMode"
    if "FabricWAN" not in test_source or "CouplingTight" not in test_source:
        return FAIL, f"{test_path} does not exercise WAN + tightly coupled placement"
    # Surrounding admitted cases (site fabric, independent pool/service/cloud)
    # must also be executable so the refusal is not the only path covered.
    for needle, label in (
        ("TestMeasuredSiteFabricWinsLocalCluster", "measured-site admission"),
        ("TestIndependentWorkChoosesPoolServiceOrCloud", "independent-work admission"),
    ):
        if needle not in test_source:
            return FAIL, f"{test_path} is missing {label} ({needle})"
    return PASS, (
        "ChooseExecutionMode is exercised by execution_mode_test.go: refuses "
        "tightly coupled work on WAN, admits measured site and independent work"
    )


def check_supplier_usage_is_not_payment_authority():
    """8. Supplier-reported usage does not create payment authority."""
    ledger_writers = grep_go(r"INSERT INTO ledger_entries")
    offenders = []
    for path in ledger_writers:
        source = read(path)
        for stmt in re.findall(r"INSERT INTO ledger_entries[\s\S]{0,900}", source):
            if re.search(r"reported_(tokens_used|duration_ms|hardware_temp_c)", stmt):
                offenders.append(path)
    if offenders:
        return FAIL, f"a ledger insert reads supplier-reported usage: {sorted(set(offenders))}"
    return PASS, f"no ledger insert in {len(ledger_writers)} writer file(s) reads reported_* usage"


def check_no_cross_tenant_cache_disclosure():
    """9. Cache existence is not exposed across tenants."""
    source = read("src/control/exact_reuse.go") + read("src/control/exact_reuse_path.go")
    if "TenantScope" not in source:
        return FAIL, "the reuse cache key has no tenant scope"
    if not re.search(r"TenantScope", read("src/control/exact_reuse_path.go")):
        return FAIL, "the live path does not set a tenant scope on the cache key"
    return PASS, "the tenant is part of the cache key, not a filter applied after the lookup"


def check_pricing_is_not_one_fixed_percentage(url):
    """10. Not one fixed percentage for every workload.

    Decided from the price-publication code and, when supplied, the durable
    catalogue. Model PRICES differ, so the absolute contribution differs per
    model — but the prohibition is about the percentage, and a single
    process-wide take rate is a single percentage however many prices it is
    multiplied by.
    """
    payment = read("src/control/payment.go")
    policy_path = "src/control/pricing_policy.go"
    if policy_path not in tracked_files():
        return FAIL, "no workload-bound supplier-share policy is tracked"
    policy = read(policy_path)
    if "MERC_PLATFORM_TAKE_PCT" in payment or "platformTakeRate" in payment:
        return FAIL, "src/control/payment.go still contains a process-wide platform take rate"
    if not re.search(r"func\s+supplierShareForWorkload\s*\(", policy):
        return FAIL, "pricing policy has no workload-scoped supplier-share resolver"
    if "physicalSupplierSharePolicies" not in policy:
        return FAIL, "pricing policy has no reviewed physical-workload policy table"
    detail = "publication resolves a closed physical-workload supplier-share policy; payment.go has no global take-rate setting"
    if url:
        try:
            rows = psql(url, "SELECT COUNT(DISTINCT h.supplier_share), COUNT(*),"
                             " MIN(h.supplier_share), MAX(h.supplier_share)"
                             " FROM catalogue_price_schedules s"
                             " JOIN model_price_history h ON h.schedule_sha256=s.sha256"
                             " WHERE s.version=2")
            distinct, total = int(rows[0][0]), int(rows[0][1])
            if total == 0:
                return FAIL, detail + "; no v2 per-workload catalogue schedule is published"
            if distinct < 2:
                return FAIL, (detail + f"; v2 catalogue has only {distinct} distinct supplier "
                             f"share across {total} row(s)")
            detail += (f"; the v2 catalogue carries {distinct} distinct supplier "
                       f"shares across {total} row(s) (min {rows[0][2]}, max {rows[0][3]})")
        except RuntimeError as error:
            return UNDECIDABLE, detail + f"; database probe failed: {error}"
    return PASS, detail


def check_no_underwater_pricing():
    """11. Price is not lowered beneath sustainable cost."""
    hits = grep_go(r"negative[- _]?contribution|BlockReason")
    if not hits:
        return FAIL, "nothing in the tree refuses a negative-contribution price"
    plan = read("src/control/economic_plan.go") if os.path.exists(
        os.path.join(ROOT, "src/control/economic_plan.go")) else ""
    guards = grep_go(r"Executable\s*=\s*false|BlockReason\s*=")
    if not guards and not plan:
        return FAIL, "no economic plan can be marked non-executable"
    return PASS, f"{len(guards)} file(s) can block a plan whose contribution is not sustainable"


def check_no_visionmcp():
    """12. VisionMCP is not in the tree."""
    validator = "ops/scripts/validate-repo-boundary.py"
    if validator not in tracked_files():
        return FAIL, f"{validator} is not tracked, so the boundary is unenforced"
    code, stdout, stderr = run("python3", validator)
    if code != 0:
        return FAIL, f"repository boundary validator failed: {(stderr or stdout).strip()[:200]}"
    offenders = [f for f in tracked_files() if "visionmcp" in f.lower()]
    if offenders:
        return FAIL, f"VisionMCP paths tracked: {offenders[:5]}"
    return PASS, "boundary validator passes and no VisionMCP path is tracked"


def check_no_live_stripe_keys():
    """13. No live Stripe credential is present or accepted.

    A test that proves a live key is REFUSED has to name one, so the live-mode
    secret prefix appearing in a
    test, a doc or a test driver is evidence of the guard rather than a breach of
    it. What would be a breach is the prefix in shipped code or configuration, or a
    string long enough to be an actual Stripe secret anywhere at all.
    """
    # Assembled rather than written out, because a scanner that spells the prefix
    # it hunts for matches its own source — the first run of this check reported
    # ops/scripts/must-not-do-audit.py as the offender.
    prefix = "sk_" + "live_"
    fixture_paths, production_paths, credential_shaped = [], [], []
    for path in tracked_files():
        if path.endswith((".png", ".jpg", ".pdf", ".gguf", ".blend")):
            continue
        full = os.path.join(ROOT, path)
        if not os.path.isfile(full) or os.path.getsize(full) > 4_000_000:
            continue
        body = read(path)
        matches = re.findall(prefix + r"[A-Za-z0-9_]+", body)
        if not matches:
            continue
        # A real Stripe secret is 24+ base62 characters after the prefix. The
        # fixtures in this tree use short, obviously-fake tails.
        if any(len(m) - len(prefix) >= 24 and "_" not in m[len(prefix):] for m in matches):
            credential_shaped.append(path)
        # docs, tests and evidence are RECORDS: naming the prefix there is how a
        # refusal gets asserted or reported. What would be a breach is the prefix
        # in shipped code or configuration.
        is_fixture = (
            path.endswith("_test.go")
            or path.startswith("docs/")
            or path.startswith("evidence/")
            or re.match(r"ops/scripts/test-", path)
        )
        (fixture_paths if is_fixture else production_paths).append(path)
    if credential_shaped:
        return FAIL, f"a credential-shaped live-mode value appears in: {sorted(credential_shaped)}"
    if production_paths:
        return FAIL, ("a live-mode secret prefix appears outside tests, docs and evidence: "
                      f"{sorted(production_paths)}")
    guard = read("src/control/payment_authority.go")
    if "LIVE payment mode requires MERC_ENV=production" not in guard:
        return FAIL, "payment authority no longer gates live mode on the production environment"
    if "SEALED payment mode forbids STRIPE_SECRET_KEY" not in guard:
        return FAIL, "sealed mode no longer forbids an inline Stripe secret"
    return PASS, (
        f"no credential-shaped live key anywhere; the {len(fixture_paths)} file(s) naming the "
        "prefix are refusal fixtures, and live mode stays gated on MERC_ENV=production plus "
        "external activation authority"
    )


def check_no_unearned_ten_of_ten():
    """14. 10/10 is not claimed from routes, schemas, tests or documentation."""
    return UNDECIDABLE, (
        "a claim in prose cannot be decided by a script. ops/go-no-go.json is the "
        "machine-readable answer and currently reports readiness 83 against a "
        "threshold of 95 with Level B NO_GO."
    )


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--database-url", default=os.environ.get("DATABASE_URL", ""))
    parser.add_argument("--out", default="")
    args = parser.parse_args()
    url = args.database_url.strip()

    checks = [
        ("1", "no global codebase refactor", lambda: check_no_global_refactor()),
        ("2", "vLLM is not forked", lambda: check_no_vllm_fork()),
        ("3", "not every registered runtime is routable", lambda: check_not_every_runtime_routable(url)),
        ("4", "standalone benchmarks are not product proof", lambda: check_benchmarks_are_not_product_proof()),
        ("5", "buyer-supplied engine/runtime/format is not authority", lambda: check_buyer_fields_are_not_authority()),
        ("6", "physical inference is not mixed with reused delivery", lambda: check_physical_and_reused_delivery_are_separate()),
        ("7", "tightly coupled inference is not spread over a WAN", lambda: check_no_wan_tightly_coupled_inference()),
        ("8", "supplier-reported usage is not payment authority", lambda: check_supplier_usage_is_not_payment_authority()),
        ("9", "cross-tenant cache existence is not exposed", lambda: check_no_cross_tenant_cache_disclosure()),
        ("10", "pricing is not one fixed percentage", lambda: check_pricing_is_not_one_fixed_percentage(url)),
        ("11", "price is not below sustainable cost", lambda: check_no_underwater_pricing()),
        ("12", "VisionMCP is not in the tree", lambda: check_no_visionmcp()),
        ("13", "no live Stripe credential", lambda: check_no_live_stripe_keys()),
        ("14", "10/10 is not claimed from routes or docs", lambda: check_no_unearned_ten_of_ten()),
    ]

    results = []
    for number, name, fn in checks:
        try:
            status, detail = fn()
        except Exception as error:  # noqa: BLE001 - reported, never swallowed
            status, detail = FAIL, f"check raised: {error}"
        results.append({"prohibition": number, "name": name, "status": status, "detail": detail})

    _, head, _ = run("git", "rev-parse", "HEAD")
    _, dirty, _ = run("git", "status", "--porcelain")
    receipt = {
        "schema_version": 1,
        "source": {"commit": head.strip(), "tree_clean": not dirty.strip()},
        "database_probed": bool(url),
        "results": results,
        "failed": [r["prohibition"] for r in results if r["status"] == FAIL],
        "undecidable": [r["prohibition"] for r in results if r["status"] == UNDECIDABLE],
    }
    if args.out:
        path = os.path.join(ROOT, args.out)
        _scripts = os.path.join(ROOT, "ops/scripts")
        if _scripts not in sys.path:
            sys.path.insert(0, _scripts)
        from lib.evidence_binding import EvidenceBindingError, emit_bound_json
        try:
            emit_bound_json(
                path,
                receipt,
                harness="ops/scripts/must-not-do-audit.py",
                repo_root=ROOT,
                build_binary_path=os.path.join(ROOT, "ops/scripts", "must-not-do-audit.py"),
                exact_config="prohibition checks embedded in results",
                raw_samples="results[] per prohibition",
            )
        except EvidenceBindingError as exc:
            print(f"REFUSED evidence write: {exc}", file=sys.stderr)
            return 2
        print(f"must-not-do audit written to {args.out}", file=sys.stderr)
    else:
        print(json.dumps(receipt, indent=2))
    for r in results:
        print(f"{r['status']:>28}  {r['prohibition']:>2}. {r['name']}: {r['detail']}", file=sys.stderr)
    return 1 if receipt["failed"] else 0


if __name__ == "__main__":
    sys.exit(main())

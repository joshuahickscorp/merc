#!/usr/bin/env python3
"""Generate the alpha license inventory and dependency-license report.

Derived artifacts only. This script reads the real lockfiles / manifests:

  control/go.mod, control/go.sum          (git show if not on disk)
  agent/Cargo.lock + crate Cargo.toml     (declared license fields)
  clients/sdk/python/pyproject.toml
  clients/sdk/typescript/package.json
  clients/sdk/typescript/package-lock.json

Go module metadata has no license field. Licenses for go.mod modules are
taken from the LICENSE file inside the exact module zip on proxy.golang.org,
then classified from that text. Absence is recorded as UNDECLARED, never
guessed.

Model weights, fonts, and visual assets are NOT this graph. They stay in
docs/THIRD_PARTY_LICENSES.md, where two catalogue models remain BLOCKED.

    python3 scripts/generate-license-inventory.py
"""

from __future__ import annotations

import hashlib
import io
import json
import re
import subprocess
import sys
import urllib.error
import urllib.request
import zipfile
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]

DRAFT_HEADER = (
    "DRAFT · INTERNAL · NOT LEGAL ADVICE\n"
    "Not reviewed by counsel. Does not constitute legal approval or compliance."
)

# First-party packages in the Cargo lock (the agent itself).
FIRST_PARTY_CARGO = {"merc-agent"}

# License expressions that are compatible with: proprietary control plane
# (not distributed as source) + Apache-2.0 agent/clients (distributed to
# named alpha suppliers). Disjunctive OR that includes a permissive option
# is treated as choosable-permissive.
PERMISSIVE_TOKENS = {
    "MIT",
    "APACHE-2.0",
    "APACHE-2.0 WITH LLVM-EXCEPTION",
    "BSD-2-CLAUSE",
    "BSD-3-CLAUSE",
    "ISC",
    "UNLICENSE",
    "ZLIB",
    "0BSD",
    "UNICODE-3.0",
    "CDLA-PERMISSIVE-2.0",
    "BSL-1.0",
    "MPL-2.0",
}

COPYLEFT_TOKENS = {
    "GPL-2.0",
    "GPL-2.0-ONLY",
    "GPL-2.0-OR-LATER",
    "GPL-3.0",
    "GPL-3.0-ONLY",
    "GPL-3.0-OR-LATER",
    "AGPL-3.0",
    "AGPL-3.0-ONLY",
    "AGPL-3.0-OR-LATER",
    "LGPL-2.1",
    "LGPL-2.1-ONLY",
    "LGPL-2.1-OR-LATER",
    "LGPL-3.0",
    "SSPL-1.0",
    "BUSL-1.1",
    "COMMONS-CLAUSE",
}


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def git_show(rel: str) -> str | None:
    path = ROOT / rel
    if path.is_file():
        return path.read_text(encoding="utf-8")
    completed = subprocess.run(
        ["git", "show", f"HEAD:{rel}"],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    if completed.returncode != 0:
        return None
    return completed.stdout


def git_blob_sha256(rel: str) -> str | None:
    path = ROOT / rel
    if path.is_file():
        return sha256_bytes(path.read_bytes())
    completed = subprocess.run(
        ["git", "show", f"HEAD:{rel}"],
        cwd=ROOT,
        capture_output=True,
        check=False,
    )
    if completed.returncode != 0:
        return None
    return sha256_bytes(completed.stdout)


def head_commit() -> str:
    completed = subprocess.run(
        ["git", "rev-parse", "HEAD"],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    return (completed.stdout or "").strip()


def parse_go_mod(text: str) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    in_require = False
    indirect = False
    # Track whether we are in the second require() block by seeing // indirect
    # on lines; also handle single-line require.
    for raw in text.splitlines():
        line = raw.strip()
        if line.startswith("require ("):
            in_require = True
            continue
        if in_require and line == ")":
            in_require = False
            continue
        if line.startswith("require ") and not line.startswith("require ("):
            body = line[len("require ") :].strip()
            rows.append(_go_require_line(body))
            continue
        if not in_require or not line or line.startswith("//"):
            continue
        rows.append(_go_require_line(line))
    return [r for r in rows if r]


def _go_require_line(line: str) -> dict[str, Any] | None:
    indirect = "// indirect" in line
    body = line.split("//", 1)[0].strip()
    parts = body.split()
    if len(parts) < 2:
        return None
    return {
        "path": parts[0],
        "version": parts[1],
        "indirect": indirect,
    }


def parse_go_sum(text: str) -> list[dict[str, str]]:
    seen: dict[tuple[str, str], str] = {}
    for line in text.splitlines():
        parts = line.split()
        if len(parts) < 3:
            continue
        path, ver, digest = parts[0], parts[1], parts[2]
        if ver.endswith("/go.mod"):
            continue
        seen[(path, ver)] = digest
    return [
        {"path": path, "version": ver, "h1": digest}
        for (path, ver), digest in sorted(seen.items())
    ]


def classify_license_text(text: str) -> str:
    upper = text.upper()
    head = upper[:2500]
    if "APACHE LICENSE" in head and "VERSION 2.0" in head:
        if "MIT LICENSE" in head or "THIS PROJECT IS COVERED BY TWO DIFFERENT LICENSES" in head:
            return "MIT AND Apache-2.0"
        return "Apache-2.0"
    if "MIT LICENSE" in head or (
        "PERMISSION IS HEREBY GRANTED, FREE OF CHARGE" in head
        and "THE SOFTWARE IS PROVIDED \"AS IS\"" in head
    ):
        return "MIT"
    if "REDISTRIBUTION AND USE IN SOURCE AND BINARY FORMS" in head:
        if "ENDORSE OR PROMOTE" in head or "NOR THE NAMES OF" in head:
            return "BSD-3-Clause"
        return "BSD-2-Clause"
    if "MOZILLA PUBLIC LICENSE" in head and "2.0" in head:
        return "MPL-2.0"
    if "GNU AFFERO GENERAL PUBLIC LICENSE" in head:
        return "AGPL-3.0"
    if "GNU GENERAL PUBLIC LICENSE" in head:
        if "VERSION 3" in head:
            return "GPL-3.0"
        return "GPL-2.0"
    if "GNU LESSER GENERAL PUBLIC LICENSE" in head:
        return "LGPL-2.1-or-later"
    return "UNCLASSIFIED_LICENSE_TEXT"


def fetch_go_license(path: str, version: str) -> dict[str, Any]:
    url = f"https://proxy.golang.org/{path}/@v/{version}.zip"
    try:
        with urllib.request.urlopen(url, timeout=30) as response:
            blob = response.read()
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {
            "declared_license": None,
            "concluded_spdx": "UNDECLARED",
            "license_source": "proxy.golang.org fetch failed",
            "license_file": None,
            "fetch_error": str(exc),
        }
    try:
        archive = zipfile.ZipFile(io.BytesIO(blob))
    except zipfile.BadZipFile as exc:
        return {
            "declared_license": None,
            "concluded_spdx": "UNDECLARED",
            "license_source": "module zip unreadable",
            "license_file": None,
            "fetch_error": str(exc),
        }
    candidates = []
    for name in archive.namelist():
        base = name.rsplit("/", 1)[-1].upper()
        if base in {"LICENSE", "LICENSE.MD", "LICENSE.TXT", "LICENCE", "COPYING", "COPYING.MD"}:
            candidates.append(name)
    if not candidates:
        return {
            "declared_license": None,
            "concluded_spdx": "UNDECLARED",
            "license_source": "no LICENSE file in module zip",
            "license_file": None,
            "zip_sha256": sha256_bytes(blob),
        }
    # Prefer a root-ish LICENSE (shortest path).
    candidates.sort(key=lambda n: (n.count("/"), n))
    chosen = candidates[0]
    text = archive.read(chosen).decode("utf-8", "replace")
    return {
        "declared_license": None,
        "concluded_spdx": classify_license_text(text),
        "license_source": "LICENSE file inside proxy.golang.org module zip",
        "license_file": chosen,
        "zip_sha256": sha256_bytes(blob),
        "license_sha256": sha256_bytes(text.encode("utf-8")),
    }


def parse_cargo_lock(text: str) -> list[dict[str, str]]:
    packages: list[dict[str, str]] = []
    current: dict[str, str] = {}
    in_package = False
    for line in text.splitlines():
        if line.strip() == "[[package]]":
            if current.get("name") and current.get("version"):
                packages.append(current)
            current = {}
            in_package = True
            continue
        if not in_package:
            continue
        if line.startswith("[") and not line.startswith("[["):
            if current.get("name") and current.get("version"):
                packages.append(current)
            current = {}
            in_package = False
            continue
        if line.startswith("name = "):
            current["name"] = line.split("=", 1)[1].strip().strip('"')
        elif line.startswith("version = "):
            current["version"] = line.split("=", 1)[1].strip().strip('"')
        elif line.startswith("source = "):
            current["source"] = line.split("=", 1)[1].strip().strip('"')
        elif line.startswith("checksum = "):
            current["checksum"] = line.split("=", 1)[1].strip().strip('"')
    if current.get("name") and current.get("version"):
        packages.append(current)
    return packages


def crate_declared_license(name: str, version: str) -> tuple[str | None, str]:
    registry = Path.home() / ".cargo/registry/src"
    if not registry.is_dir():
        return None, "cargo registry src not present"
    for index in registry.iterdir():
        manifest = index / f"{name}-{version}" / "Cargo.toml"
        if not manifest.is_file():
            continue
        license_expr = None
        license_file = None
        for raw in manifest.read_text(encoding="utf-8", errors="replace").splitlines():
            stripped = raw.strip()
            if stripped.startswith("license ="):
                license_expr = stripped.split("=", 1)[1].strip().strip('"')
            elif stripped.startswith("license-file ="):
                license_file = stripped.split("=", 1)[1].strip().strip('"')
        if license_expr:
            return license_expr, str(manifest)
        if license_file:
            return f"LICENSE_FILE:{license_file}", str(manifest)
        return None, str(manifest)
    return None, "crate sources not in local cargo registry"


def normalize_spdx_token(token: str) -> str:
    return re.sub(r"\s+", " ", token.strip()).upper().replace(" ", "-")


def _strip_parens(expr: str) -> str:
    text = expr.strip()
    while text.startswith("(") and text.endswith(")"):
        depth = 0
        wrapped = True
        for index, char in enumerate(text):
            if char == "(":
                depth += 1
            elif char == ")":
                depth -= 1
                if depth == 0 and index != len(text) - 1:
                    wrapped = False
                    break
        if not wrapped:
            break
        text = text[1:-1].strip()
    return text


def split_top_level(expr: str, operator: str) -> list[str] | None:
    """Split on a top-level SPDX operator; ignore the operator inside parentheses."""
    text = _strip_parens(expr)
    pattern = re.compile(rf"\s+{re.escape(operator)}\s+", re.I)
    parts: list[str] = []
    depth = 0
    last = 0
    for match in pattern.finditer(text):
        depth = text[: match.start()].count("(") - text[: match.start()].count(")")
        if depth != 0:
            continue
        parts.append(text[last : match.start()].strip())
        last = match.end()
    if not parts:
        return None
    parts.append(text[last:].strip())
    return parts


def split_or_and(expr: str) -> tuple[str, list[str]]:
    """Return ('OR'|'AND'|'ATOM', tokens). Slash used as OR in some crates."""
    cleaned = expr.replace("/", " OR ")
    and_parts = split_top_level(cleaned, "AND")
    if and_parts and len(and_parts) > 1:
        return "AND", and_parts
    or_parts = split_top_level(cleaned, "OR")
    if or_parts and len(or_parts) > 1:
        return "OR", or_parts
    return "ATOM", [_strip_parens(cleaned)]


def _expr_class(expr: str) -> str:
    """Classify a possibly compound SPDX expression as PERMISSIVE/WEAK_COPYLEFT/COPYLEFT/UNKNOWN."""
    kind, parts = split_or_and(expr)
    if kind == "ATOM":
        tok = normalize_spdx_token(parts[0])
        if tok in COPYLEFT_TOKENS or tok.startswith("GPL") or tok.startswith("AGPL"):
            return "COPYLEFT"
        if tok in PERMISSIVE_TOKENS or tok.startswith("APACHE-2.0"):
            return "PERMISSIVE"
        if "LGPL" in tok:
            return "WEAK_COPYLEFT"
        return "UNKNOWN"
    classes = [_expr_class(part) for part in parts]
    if kind == "OR":
        if "PERMISSIVE" in classes:
            return "PERMISSIVE"
        if "WEAK_COPYLEFT" in classes and "COPYLEFT" not in classes:
            return "WEAK_COPYLEFT"
        if all(c == "COPYLEFT" for c in classes):
            return "COPYLEFT"
        return "UNKNOWN"
    # AND: the strictest conjunct wins.
    if "COPYLEFT" in classes or "UNKNOWN" in classes:
        return "COPYLEFT" if "COPYLEFT" in classes else "UNKNOWN"
    if "WEAK_COPYLEFT" in classes:
        return "WEAK_COPYLEFT"
    return "PERMISSIVE"


def compatibility(spdx: str | None, surface: str) -> dict[str, str]:
    if not spdx or spdx in {"UNDECLARED", "UNCLASSIFIED_LICENSE_TEXT"}:
        return {
            "verdict": "CANNOT_CONCLUDE",
            "reason": "no concluded license; absence is not a permissive grant",
        }
    kind, parts = split_or_and(spdx)
    cls = _expr_class(spdx)
    if kind == "OR" and cls == "PERMISSIVE":
        return {
            "verdict": "COMPATIBLE_PERMISSIVE_OPTION",
            "reason": (
                f"disjunctive license includes a permissive option; "
                f"{surface} can take MIT/Apache/BSD and ignore weaker/stronger alternatives"
            ),
        }
    if cls == "PERMISSIVE":
        return {
            "verdict": "COMPATIBLE_PERMISSIVE",
            "reason": f"permissive license is compatible with {surface}",
        }
    if cls == "WEAK_COPYLEFT":
        return {
            "verdict": "COMPATIBLE_NOTICE",
            "reason": f"weak copyleft; file-level notices required if {surface} distributes modified files",
        }
    if cls == "COPYLEFT":
        return {
            "verdict": "INCOMPATIBLE_COPYLEFT",
            "reason": f"strong copyleft is incompatible with how {surface} is licensed",
        }
    return {
        "verdict": "CANNOT_CONCLUDE",
        "reason": f"unrecognized SPDX {spdx!r}",
    }


def parse_pyproject(text: str) -> dict[str, Any]:
    name = None
    version = None
    license_id = None
    requires: list[str] = []
    in_project = False
    in_deps = False
    for raw in text.splitlines():
        line = raw.rstrip()
        if line.startswith("[project]"):
            in_project = True
            in_deps = False
            continue
        if line.startswith("["):
            in_project = line.startswith("[project]")
            in_deps = False
            continue
        if in_project and line.startswith("name"):
            name = line.split("=", 1)[1].strip().strip('"')
        if in_project and line.startswith("license"):
            license_id = line.split("=", 1)[1].strip().strip('"')
        if in_project and line.startswith("dependencies"):
            in_deps = True
            if line.strip().endswith("[]"):
                requires = []
                in_deps = False
            continue
        if in_deps:
            if line.strip().startswith("]"):
                in_deps = False
            else:
                item = line.strip().strip(",").strip('"')
                if item:
                    requires.append(item)
    return {
        "name": name or "merc",
        "version": version or "dynamic",
        "license": license_id,
        "dependencies": requires,
    }


def parse_package_json(text: str) -> dict[str, Any]:
    doc = json.loads(text)
    return {
        "name": doc.get("name"),
        "version": doc.get("version"),
        "license": doc.get("license"),
        "dependencies": doc.get("dependencies") or {},
        "devDependencies": doc.get("devDependencies") or {},
    }


def parse_package_lock(text: str) -> list[dict[str, Any]]:
    doc = json.loads(text)
    rows = []
    for key, meta in (doc.get("packages") or {}).items():
        rows.append(
            {
                "path": key or "(root)",
                "version": meta.get("version"),
                "license": meta.get("license"),
                "dev": bool(meta.get("dev")),
            }
        )
    return rows


def md_escape(value: Any) -> str:
    return str(value).replace("|", "\\|")


def write_text(path: Path, body: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(body, encoding="utf-8")


def main() -> int:
    generated_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    commit = head_commit()

    inputs = {
        "control/go.mod": git_show("control/go.mod"),
        "control/go.sum": git_show("control/go.sum"),
        "agent/Cargo.lock": git_show("agent/Cargo.lock"),
        "agent/Cargo.toml": git_show("agent/Cargo.toml"),
        "clients/sdk/python/pyproject.toml": git_show("clients/sdk/python/pyproject.toml"),
        "clients/sdk/typescript/package.json": git_show("clients/sdk/typescript/package.json"),
        "clients/sdk/typescript/package-lock.json": git_show(
            "clients/sdk/typescript/package-lock.json"
        ),
    }
    missing = [name for name, text in inputs.items() if text is None]
    if missing:
        print(f"generate-license-inventory: missing {missing}", file=sys.stderr)
        return 1

    input_hashes = {name: git_blob_sha256(name) for name in inputs}

    go_mod_rows = parse_go_mod(inputs["control/go.mod"])
    go_sum_rows = parse_go_sum(inputs["control/go.sum"])
    go_mod_keys = {(r["path"], r["version"]) for r in go_mod_rows}

    go_components: list[dict[str, Any]] = []
    for row in go_mod_rows:
        license_info = fetch_go_license(row["path"], row["version"])
        surface = "proprietary control plane (not publicly distributed at backend alpha)"
        compat = compatibility(license_info.get("concluded_spdx"), surface)
        go_components.append(
            {
                "ecosystem": "go",
                "name": row["path"],
                "version": row["version"],
                "relation": "indirect" if row["indirect"] else "direct",
                "in_go_mod": True,
                **license_info,
                "compatibility": compat,
                "distribution_surface": surface,
            }
        )

    go_sum_only = [
        row
        for row in go_sum_rows
        if (row["path"], row["version"]) not in go_mod_keys
    ]

    cargo_packages = parse_cargo_lock(inputs["agent/Cargo.lock"])
    rust_components: list[dict[str, Any]] = []
    for pkg in cargo_packages:
        name, version = pkg["name"], pkg["version"]
        if name in FIRST_PARTY_CARGO:
            rust_components.append(
                {
                    "ecosystem": "cargo",
                    "name": name,
                    "version": version,
                    "relation": "first-party",
                    "declared_license": "Apache-2.0",
                    "concluded_spdx": "Apache-2.0",
                    "license_source": "agent/Cargo.toml license = Apache-2.0",
                    "compatibility": {
                        "verdict": "FIRST_PARTY",
                        "reason": "merc agent; Apache-2.0, intended for supplier distribution",
                    },
                    "distribution_surface": "Apache-2.0 agent binary (operator-known suppliers)",
                    "checksum": pkg.get("checksum"),
                }
            )
            continue
        declared, source = crate_declared_license(name, version)
        # Normalise slash-OR used by a few crates into SPDX OR.
        concluded = None
        if declared and not declared.startswith("LICENSE_FILE:"):
            concluded = declared.replace("/", " OR ")
        surface = "Apache-2.0 agent binary (operator-known suppliers at backend alpha)"
        rust_components.append(
            {
                "ecosystem": "cargo",
                "name": name,
                "version": version,
                "relation": "dependency",
                "declared_license": declared,
                "concluded_spdx": concluded or "UNDECLARED",
                "license_source": source,
                "compatibility": compatibility(concluded, surface),
                "distribution_surface": surface,
                "source": pkg.get("source"),
                "checksum": pkg.get("checksum"),
            }
        )

    py = parse_pyproject(inputs["clients/sdk/python/pyproject.toml"])
    ts = parse_package_json(inputs["clients/sdk/typescript/package.json"])
    ts_lock = parse_package_lock(inputs["clients/sdk/typescript/package-lock.json"])

    python_components = [
        {
            "ecosystem": "python",
            "name": py["name"],
            "version": py["version"],
            "relation": "first-party",
            "declared_license": py["license"],
            "concluded_spdx": py["license"] or "UNDECLARED",
            "license_source": "clients/sdk/python/pyproject.toml",
            "runtime_dependencies": py["dependencies"],
            "compatibility": {
                "verdict": "FIRST_PARTY",
                "reason": "buyer SDK; Apache-2.0; no runtime third-party dependencies",
            },
            "distribution_surface": "Apache-2.0 Python SDK (not required for backend alpha)",
        }
    ]

    js_components = []
    for row in ts_lock:
        is_root = row["path"] in {"", "(root)"}
        surface = "Apache-2.0 TypeScript SDK (dev-time only at backend alpha)"
        if is_root:
            js_components.append(
                {
                    "ecosystem": "npm",
                    "name": ts["name"],
                    "version": row["version"] or ts["version"],
                    "relation": "first-party",
                    "declared_license": row["license"] or ts["license"],
                    "concluded_spdx": row["license"] or ts["license"] or "UNDECLARED",
                    "license_source": "clients/sdk/typescript/package.json",
                    "compatibility": {
                        "verdict": "FIRST_PARTY",
                        "reason": "buyer SDK; Apache-2.0; no runtime third-party dependencies",
                    },
                    "distribution_surface": surface,
                }
            )
            continue
        name = row["path"].removeprefix("node_modules/")
        js_components.append(
            {
                "ecosystem": "npm",
                "name": name,
                "version": row["version"],
                "relation": "devDependency" if row["dev"] else "dependency",
                "declared_license": row["license"],
                "concluded_spdx": row["license"] or "UNDECLARED",
                "license_source": "clients/sdk/typescript/package-lock.json",
                "compatibility": compatibility(row["license"], surface),
                "distribution_surface": surface,
            }
        )

    all_components = go_components + rust_components + python_components + js_components
    verdicts = Counter(
        c.get("compatibility", {}).get("verdict", "CANNOT_CONCLUDE") for c in all_components
    )
    incompatible = [
        c
        for c in all_components
        if c.get("compatibility", {}).get("verdict") == "INCOMPATIBLE_COPYLEFT"
    ]
    undeclared = [
        c
        for c in all_components
        if c.get("concluded_spdx") in {None, "UNDECLARED", "UNCLASSIFIED_LICENSE_TEXT"}
        and c.get("relation") != "first-party"
    ]

    doc = {
        "schema_version": 1,
        "kind": "merc_generated_license_inventory",
        "status": "GENERATED_DRAFT_NOT_APPROVAL",
        "generated_at": generated_at,
        "source_commit": commit,
        "generator": "scripts/generate-license-inventory.py",
        "honesty": {
            "not_legal_advice": True,
            "not_counsel_review": True,
            "does_not_constitute_approval_or_compliance": True,
            "go_licenses": (
                "Taken from LICENSE files inside the exact module zip on "
                "proxy.golang.org for every module listed in control/go.mod. "
                "Go module metadata has no license field; nothing is guessed."
            ),
            "cargo_licenses": (
                "Declared license fields from the crate Cargo.toml matching "
                "agent/Cargo.lock name+version in the local cargo registry."
            ),
            "excluded": (
                "Catalogue model weights, Geist fonts, and visual assets are "
                "not software-graph components. They remain in "
                "docs/THIRD_PARTY_LICENSES.md (Llama and MiniLM rows BLOCKED)."
            ),
            "alpha_distribution": (
                "Backend alpha does not ship a public website or a public "
                "binary. The agent may be given to operator-known suppliers; "
                "that is limited Apache-2.0 distribution. The control plane "
                "is proprietary and is not distributed as source."
            ),
        },
        "inputs": {
            name: {"sha256": digest, "present": digest is not None}
            for name, digest in input_hashes.items()
        },
        "counts": {
            "go_mod_modules": len(go_components),
            "go_sum_module_versions": len(go_sum_rows),
            "go_sum_not_in_go_mod": len(go_sum_only),
            "cargo_packages": len(rust_components),
            "python_packages": len(python_components),
            "npm_packages": len(js_components),
            "total_components": len(all_components),
            "incompatible_copyleft": len(incompatible),
            "undeclared": len(undeclared),
            "verdicts": dict(verdicts),
        },
        "go_sum_not_in_go_mod": go_sum_only,
        "components": all_components,
    }

    json_path = ROOT / "docs" / "generated" / "license-inventory.json"
    write_text(json_path, json.dumps(doc, indent=2, sort_keys=False) + "\n")

    inventory_md = render_inventory_md(doc)
    write_text(ROOT / "docs" / "LICENSE_INVENTORY.md", inventory_md)

    print(
        f"license-inventory: {len(all_components)} components "
        f"(go={len(go_components)} cargo={len(rust_components)} "
        f"py={len(python_components)} npm={len(js_components)})"
    )
    print(f"license-inventory: incompatible_copyleft={len(incompatible)} undeclared={len(undeclared)}")
    print(f"license-inventory: wrote {json_path.relative_to(ROOT)}")
    print("license-inventory: wrote docs/LICENSE_INVENTORY.md")
    return 0


def render_inventory_md(doc: dict[str, Any]) -> str:
    lines = [
        "# Open-source license inventory",
        "",
        f"> **{DRAFT_HEADER.splitlines()[0]}**",
        ">",
        f"> {DRAFT_HEADER.splitlines()[1]}",
        ">",
        "> **GENERATED** from the real dependency graph by "
        "`scripts/generate-license-inventory.py`. Do not hand-edit. "
        "Regenerate with that script. This is not a license clearance.",
        "",
        f"- Generated at: `{doc['generated_at']}`",
        f"- Source commit: `{doc['source_commit'] or 'UNKNOWN'}`",
        f"- Status: `{doc['status']}`",
        "",
        "## Graph binding",
        "",
        "| Manifest | SHA-256 |",
        "|---|---|",
    ]
    for name, meta in doc["inputs"].items():
        lines.append(f"| `{name}` | `{meta['sha256']}` |")
    counts = doc["counts"]
    lines += [
        "",
        "## Counts",
        "",
        f"- Go modules in `control/go.mod`: {counts['go_mod_modules']}",
        f"- Go `go.sum` module versions (including test-only checksums): {counts['go_sum_module_versions']}",
        f"- `go.sum` versions not listed in `go.mod`: {counts['go_sum_not_in_go_mod']}",
        f"- Cargo packages in `agent/Cargo.lock`: {counts['cargo_packages']}",
        f"- Python packages: {counts['python_packages']} (first-party SDK, zero runtime deps)",
        f"- npm packages in the TypeScript lock: {counts['npm_packages']}",
        f"- Incompatible copyleft in this software graph: {counts['incompatible_copyleft']}",
        f"- Undeclared / unclassified: {counts['undeclared']}",
        "",
        "Model weights (Llama 3.2, MiniLM), Geist fonts, and visual assets are",
        "**not** in this inventory. See `docs/THIRD_PARTY_LICENSES.md` — status",
        "**INCOMPLETE / RELEASE BLOCKING**. Those rows stay BLOCKED.",
        "",
        "## How Merc is distributed at backend alpha",
        "",
        "- Control plane: proprietary. Not published as source. Not a public download.",
        "- Agent (`agent/`): Apache-2.0. May be given to operator-known suppliers.",
        "- Python / TypeScript SDKs: Apache-2.0. Not required to run the backend alpha.",
        "- No public website, no public package registry publish, no live-money binary.",
        "",
        "## Components",
        "",
        "| Ecosystem | Name | Version | Relation | Concluded SPDX | Compatibility |",
        "|---|---|---|---|---|---|",
    ]
    for row in doc["components"]:
        lines.append(
            "| {eco} | `{name}` | `{ver}` | {rel} | {spdx} | {verdict} |".format(
                eco=md_escape(row.get("ecosystem")),
                name=md_escape(row.get("name")),
                ver=md_escape(row.get("version")),
                rel=md_escape(row.get("relation")),
                spdx=md_escape(row.get("concluded_spdx")),
                verdict=md_escape(row.get("compatibility", {}).get("verdict")),
            )
        )
    lines += [
        "",
        "## Compatibility summary",
        "",
        "The table above is the complete generated graph. The machine-readable "
        "JSON beside this file retains the source and reason for every verdict.",
        "",
    ]
    for key, value in sorted(counts["verdicts"].items()):
        lines.append(f"- `{key}`: {value}")
    lines += [
        f"- Incompatible copyleft: **{counts['incompatible_copyleft']}**",
        f"- Undeclared or unclassified: **{counts['undeclared']}**",
        "",
        "No software-graph row is a legal clearance. Catalogue models, fonts, "
        "and visual assets remain governed by `docs/THIRD_PARTY_LICENSES.md`.",
    ]
    lines += [
        "",
        "## `go.sum` versions not in `go.mod`",
        "",
        "These are checksum-only (typically test or historical). They are not",
        "the build graph. Licenses were not fetched for them.",
        "",
    ]
    extras = doc.get("go_sum_not_in_go_mod") or []
    if not extras:
        lines.append("None.")
    else:
        lines += ["| Module | Version |", "|---|---|"]
        for row in extras:
            lines.append(f"| `{row['path']}` | `{row['version']}` |")
    lines.append("")
    return "\n".join(lines)


if __name__ == "__main__":
    raise SystemExit(main())

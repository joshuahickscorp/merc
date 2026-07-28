from __future__ import annotations

import hashlib
import json
import math
import re
from pathlib import Path
from typing import Any

from blender_vision.core.errors import SecurityError
from blender_vision.core.util import canonical_json, sha256_file

_PROMPT_INJECTION_PATTERNS = (
    re.compile(r"\bignore (?:all |any )?(?:previous|prior|system) instructions?\b", re.I),
    re.compile(r"\breveal (?:the )?(?:system prompt|hidden instructions?|secrets?)\b", re.I),
    re.compile(r"\b(?:execute|run) (?:this )?(?:command|shell|code)\b", re.I),
    re.compile(r"\bdeveloper message\b", re.I),
    re.compile(r"\btool call\b", re.I),
)
_SECRET_KEY = re.compile(
    r"(?:^|[_-])(?:authorization|cookie|password|passwd|secret|token|api[_-]?key|"
    r"private[_-]?key)(?:$|[_-])",
    re.I,
)
_SECRET_VALUE = (
    re.compile(r"\b(?:sk|ghp|github_pat|xox[baprs])[-_A-Za-z0-9]{12,}\b"),
    re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----"),
    re.compile(r"\bBearer\s+[A-Za-z0-9._~+/=-]{12,}\b", re.I),
)
_EXECUTABLE_KEYS = {
    "code",
    "command",
    "commands",
    "script",
    "source_code",
    "entrypoint",
    "entry_point",
    "python",
    "shell",
    "eval",
    "exec",
}
_PROTOTYPE_KEYS = {"__proto__", "prototype", "constructor"}
_SAFE_RIGHTS = {
    "SYNTHETIC_OWNED",
    "SYNTHETIC_OWNED_CC0",
    "OWNED",
    "CC0",
    "INTERNAL",
    "LICENSED",
    "PUBLIC_AUTHORIZED",
}
ADVERSARIAL_ATTACK_IDS = (
    "malicious_web_prompt_injection",
    "poisoned_design_exports",
    "hostile_glb_blend",
    "path_traversal",
    "symlink_escape",
    "stale_source_maps",
    "artifact_substitution",
    "benchmark_leakage",
    "evaluator_tampering",
    "secret_exposure",
    "unauthorized_asset_reuse",
    "unsafe_generated_backend_code",
    "auth_bypass",
    "sql_injection",
    "cross_tenant_leakage",
    "performance_score_gaming",
    "visual_local_repair_global_regression",
)


class ContentTrustBoundary:
    """Keep source text as untrusted evidence and never promote it to instructions."""

    @staticmethod
    def inspect(value: str) -> dict[str, Any]:
        findings = [
            {
                "kind": "PROMPT_INJECTION",
                "pattern": pattern.pattern,
                "span": [match.start(), match.end()],
            }
            for pattern in _PROMPT_INJECTION_PATTERNS
            if (match := pattern.search(value))
        ]
        return {
            "authority": "UNTRUSTED_OBSERVED_CONTENT",
            "instruction_authority": False,
            "findings": findings,
            "promotion_allowed": not findings,
        }

    @classmethod
    def require_non_instructional(cls, value: str) -> dict[str, Any]:
        result = cls.inspect(value)
        if result["findings"]:
            raise SecurityError(
                "untrusted source contains prompt-injection text and cannot become instructions"
            )
        return result


class DesignExportPolicy:
    """Bound exported design JSON before it reaches graph compilation."""

    maximum_bytes = 16 * 1024 * 1024
    maximum_nodes = 20_000
    maximum_depth = 96
    maximum_string_bytes = 1024 * 1024

    @classmethod
    def load(cls, path: Path) -> tuple[dict[str, Any], dict[str, Any]]:
        supplied = path.expanduser().absolute()
        if supplied.is_symlink():
            raise SecurityError("design export cannot be a symlink")
        resolved = supplied.resolve()
        if not resolved.is_file():
            raise SecurityError("design export must be a regular file")
        digest, size = sha256_file(resolved)
        if size > cls.maximum_bytes:
            raise SecurityError("design export exceeds the 16 MiB bound")
        try:
            payload = json.loads(
                resolved.read_text(encoding="utf-8"),
                parse_constant=lambda value: (_ for _ in ()).throw(
                    ValueError(f"non-finite JSON number: {value}")
                ),
            )
        except (OSError, UnicodeError, json.JSONDecodeError, ValueError) as error:
            raise SecurityError(f"design export is not strict JSON: {error}") from error
        summary = cls.validate(payload)
        return payload, {"sha256": digest, "size": size, **summary}

    @classmethod
    def validate(cls, payload: Any) -> dict[str, Any]:
        if not isinstance(payload, dict):
            raise SecurityError("design export root must be an object")
        node_count = 0
        string_bytes = 0
        injection_findings = 0

        def walk(value: Any, depth: int, path: str) -> None:
            nonlocal node_count, string_bytes, injection_findings
            if depth > cls.maximum_depth:
                raise SecurityError("design export exceeds the maximum nesting depth")
            node_count += 1
            if node_count > cls.maximum_nodes:
                raise SecurityError("design export exceeds the maximum node count")
            if isinstance(value, dict):
                for key, item in value.items():
                    if not isinstance(key, str):
                        raise SecurityError("design export object keys must be strings")
                    if key.casefold() in _PROTOTYPE_KEYS:
                        raise SecurityError(
                            f"design export contains a prototype-poisoning key at {path}/{key}"
                        )
                    if key in {"importPath", "import_path"} and isinstance(item, str):
                        candidate = Path(item)
                        if (
                            item.startswith(("javascript:", "data:", "file:"))
                            or "\x00" in item
                            or candidate.is_absolute()
                            or ".." in candidate.parts
                        ):
                            raise SecurityError(
                                f"design export contains an unsafe import path at {path}/{key}"
                            )
                    walk(item, depth + 1, f"{path}/{key}")
                return
            if isinstance(value, list):
                for index, item in enumerate(value):
                    walk(item, depth + 1, f"{path}/{index}")
                return
            if isinstance(value, str):
                string_bytes += len(value.encode("utf-8"))
                if string_bytes > cls.maximum_string_bytes:
                    raise SecurityError("design export string content exceeds the 1 MiB bound")
                injection_findings += len(
                    ContentTrustBoundary.inspect(value)["findings"]
                )
                return
            if isinstance(value, (int, float)) and not isinstance(value, bool):
                if not math.isfinite(float(value)):
                    raise SecurityError(f"design export contains a non-finite number at {path}")
                return
            if value is not None and not isinstance(value, bool):
                raise SecurityError(f"design export contains a non-JSON value at {path}")

        walk(payload, 0, "")
        return {
            "node_count": node_count,
            "string_bytes": string_bytes,
            "prompt_injection_findings": injection_findings,
            "instruction_authority": False,
        }


class BlendInputPolicy:
    """Validate the bounded preflight possible before Blender opens a file."""

    @staticmethod
    def validate(path: Path, *, maximum_bytes: int = 2 * 1024 * 1024 * 1024) -> dict[str, Any]:
        supplied = path.expanduser().absolute()
        if supplied.is_symlink() or supplied.suffix.casefold() != ".blend":
            raise SecurityError("BLEND input must be a non-symlink .blend file")
        resolved = supplied.resolve()
        if not resolved.is_file():
            raise SecurityError("BLEND input is missing")
        digest, size = sha256_file(resolved)
        if not 12 <= size <= maximum_bytes:
            raise SecurityError("BLEND input size is outside the bounded range")
        header = resolved.read_bytes()[:12]
        if not re.fullmatch(rb"BLENDER[_-][vV]\d{3}", header):
            raise SecurityError("BLEND input has an invalid file header")
        return {
            "sha256": digest,
            "size": size,
            "autoexec_required_disabled": True,
            "factory_startup_required": True,
        }


class SourceMapAuthority:
    @staticmethod
    def verify(
        source: Path,
        generated: Path,
        source_map: dict[str, Any],
        *,
        source_revision: str,
    ) -> dict[str, Any]:
        if source.is_symlink() or generated.is_symlink():
            raise SecurityError("source-map inputs cannot be symlinks")
        if not source.is_file() or not generated.is_file():
            raise SecurityError("source-map inputs must be regular files")
        required = {
            "schema": "vision.source-map/v1",
            "source_revision": source_revision,
            "source_sha256": sha256_file(source)[0],
            "generated_sha256": sha256_file(generated)[0],
        }
        mismatches = {
            key: {"expected": expected, "observed": source_map.get(key)}
            for key, expected in required.items()
            if source_map.get(key) != expected
        }
        if mismatches:
            raise SecurityError(f"stale or substituted source map: {sorted(mismatches)}")
        return required


class SealedBenchmarkBoundary:
    @staticmethod
    def verify(
        builder_root: Path,
        oracle_root: Path,
        *,
        canaries: list[str],
        maximum_scan_bytes: int = 32 * 1024 * 1024,
    ) -> dict[str, Any]:
        if not canaries or any(not value for value in canaries):
            raise ValueError("sealed benchmark requires non-empty canaries")
        builder_supplied = builder_root.expanduser().absolute()
        oracle_supplied = oracle_root.expanduser().absolute()
        if builder_supplied.is_symlink() or oracle_supplied.is_symlink():
            raise SecurityError("builder and oracle roots cannot be symlinks")
        builder = builder_supplied.resolve()
        oracle = oracle_supplied.resolve()
        if not builder.is_dir() or not oracle.is_dir():
            raise SecurityError("builder and oracle roots must exist")
        if builder == oracle or builder.is_relative_to(oracle) or oracle.is_relative_to(builder):
            raise SecurityError("builder and oracle roots must be disjoint")
        scanned = 0
        files = 0
        encoded = [value.encode("utf-8") for value in canaries]
        for path in sorted(builder.rglob("*")):
            if path.is_symlink():
                raise SecurityError("builder workspace cannot contain symlinks")
            if not path.is_file():
                continue
            files += 1
            data = path.read_bytes()
            scanned += len(data)
            if scanned > maximum_scan_bytes:
                raise SecurityError("builder leakage scan exceeds the 32 MiB bound")
            if any(value in data for value in encoded):
                raise SecurityError(
                    f"sealed evaluator canary leaked into builder workspace: {path.name}"
                )
        for path in sorted(oracle.rglob("*")):
            if path.is_symlink():
                raise SecurityError("oracle workspace cannot contain symlinks")
        return {
            "builder_root_sha256": _tree_digest(builder),
            "oracle_root_sha256": _tree_digest(oracle),
            "builder_file_count": files,
            "builder_scanned_bytes": scanned,
            "canary_count": len(canaries),
            "leakage_detected": False,
        }


class DigestBoundEvaluator:
    @staticmethod
    def verify(payload: Any, expected_sha256: str) -> str:
        observed = hashlib.sha256(canonical_json(payload)).hexdigest()
        if observed != expected_sha256:
            raise SecurityError("evaluator payload digest mismatch")
        return observed


class SecretExposurePolicy:
    @classmethod
    def findings(cls, value: Any, path: str = "") -> list[str]:
        found: list[str] = []
        if isinstance(value, dict):
            for key, item in value.items():
                key_path = f"{path}/{key}"
                if _SECRET_KEY.search(str(key)):
                    found.append(key_path)
                found.extend(cls.findings(item, key_path))
        elif isinstance(value, list):
            for index, item in enumerate(value):
                found.extend(cls.findings(item, f"{path}/{index}"))
        elif isinstance(value, str) and any(
            pattern.search(value) for pattern in _SECRET_VALUE
        ):
            found.append(path or "/")
        return sorted(set(found))

    @classmethod
    def assert_clean(cls, value: Any) -> None:
        findings = cls.findings(value)
        if findings:
            raise SecurityError(f"secret-bearing fields or values are forbidden: {findings}")


class AssetRightsPolicy:
    @staticmethod
    def authorize(
        *,
        artifact_digest: str,
        registered_digests: set[str],
        rights_state: str,
    ) -> dict[str, Any]:
        if artifact_digest not in registered_digests:
            raise SecurityError("asset reuse requires a registered source artifact")
        if rights_state not in _SAFE_RIGHTS:
            raise SecurityError("asset reuse requires an explicit usable rights decision")
        return {
            "artifact_digest": artifact_digest,
            "rights_state": rights_state,
            "authorized": True,
        }


class GeneratedBackendPolicy:
    @classmethod
    def validate(cls, value: Any, path: str = "") -> None:
        if isinstance(value, dict):
            for key, item in value.items():
                key_name = str(key).casefold().replace("-", "_")
                if key_name in _EXECUTABLE_KEYS:
                    raise SecurityError(
                        f"generated backend executable code is forbidden at {path}/{key}"
                    )
                cls.validate(item, f"{path}/{key}")
        elif isinstance(value, list):
            for index, item in enumerate(value):
                cls.validate(item, f"{path}/{index}")
        elif callable(value):
            raise SecurityError(f"generated backend callable is forbidden at {path}")
        SecretExposurePolicy.assert_clean(value)


class AuthTenantPolicy:
    @staticmethod
    def authorize(
        *,
        authorization: str,
        authenticated: bool,
        required_permission: str | None,
        permissions: set[str],
        expected_tenant: str | None,
        actor_tenant: str | None,
    ) -> dict[str, Any]:
        if authorization not in {"public", "authenticated", "permission"}:
            raise SecurityError("endpoint authorization mode is invalid")
        if authorization != "public" and not authenticated:
            raise SecurityError("authentication is required")
        if authorization == "permission" and (
            not required_permission or required_permission not in permissions
        ):
            raise SecurityError("required permission is missing")
        if expected_tenant is not None and (
            not actor_tenant or actor_tenant != expected_tenant
        ):
            raise SecurityError("cross-tenant access is forbidden")
        return {
            "authorized": True,
            "authorization": authorization,
            "tenant": expected_tenant,
        }


class PerformanceEvidenceGuard:
    @staticmethod
    def verify(
        *,
        source_git_head: str,
        current_git_head: str,
        raw_metric_digests: list[str],
        preservation_gates: dict[str, bool],
        thresholds_changed_after_run: bool,
    ) -> dict[str, Any]:
        if source_git_head != current_git_head:
            raise SecurityError("performance evidence is stale")
        if not raw_metric_digests or any(
            not re.fullmatch(r"[0-9a-f]{64}", value) for value in raw_metric_digests
        ):
            raise SecurityError("performance score requires raw metric artifact digests")
        if thresholds_changed_after_run:
            raise SecurityError("performance thresholds changed after execution")
        if not preservation_gates or not all(preservation_gates.values()):
            raise SecurityError("performance score cannot bypass preservation gates")
        return {"accepted": True, "raw_metric_count": len(raw_metric_digests)}


class GlobalRegressionGuard:
    @staticmethod
    def verify(
        *,
        local_gate_passed: bool,
        route_state_gates: dict[str, bool],
    ) -> dict[str, Any]:
        if not local_gate_passed:
            raise SecurityError("local repair gate failed")
        if not route_state_gates or not all(route_state_gates.values()):
            failed = sorted(
                key for key, passed in route_state_gates.items() if not passed
            )
            raise SecurityError(
                f"local visual repair broke global route/state gates: {failed}"
            )
        return {"accepted": True, "global_gate_count": len(route_state_gates)}


class SecurityAcceptanceAuthority:
    """Require one preserved, digest-bound result for every fixed attack class."""

    @staticmethod
    def verify(
        *,
        case_results: list[dict[str, Any]],
        required_attack_ids: list[str],
        input_artifact_digests: dict[str, str],
        negative_controls: dict[str, bool],
    ) -> dict[str, Any]:
        expected = set(ADVERSARIAL_ATTACK_IDS)
        required = set(required_attack_ids)
        observed = {str(item.get("id", "")) for item in case_results}
        if required != expected or len(required_attack_ids) != len(expected):
            raise SecurityError("adversarial manifest does not declare the exact attack suite")
        if observed != expected or len(case_results) != len(expected):
            raise SecurityError("adversarial results do not cover the exact attack suite")
        if set(input_artifact_digests) != expected:
            raise SecurityError("adversarial input artifact set is incomplete")
        for item in case_results:
            if item.get("disposition") not in {"REJECTED", "NEUTRALIZED"}:
                raise SecurityError(f"attack was not contained: {item.get('id')}")
            if item.get("passed") is not True:
                raise SecurityError(f"attack did not pass its guard: {item.get('id')}")
            input_digest = str(item.get("input_sha256", ""))
            if not re.fullmatch(r"[0-9a-f]{64}", input_digest):
                raise SecurityError(f"attack input is not digest-bound: {item.get('id')}")
            if input_artifact_digests.get(str(item.get("id"))) != input_digest:
                raise SecurityError(f"attack input artifact was substituted: {item.get('id')}")
        required_controls = {
            "omitted_attack",
            "substituted_attack_input",
            "manifest_attack_set_tamper",
            "false_pass_disposition",
        }
        if set(negative_controls) != required_controls or not all(
            negative_controls.values()
        ):
            raise SecurityError("adversarial negative controls did not all reject")
        return {
            "accepted": True,
            "attack_count": len(case_results),
            "negative_control_count": len(negative_controls),
        }


def _tree_digest(root: Path) -> str:
    entries = [
        {
            "path": path.relative_to(root).as_posix(),
            "sha256": sha256_file(path)[0],
        }
        for path in sorted(candidate for candidate in root.rglob("*") if candidate.is_file())
    ]
    return hashlib.sha256(canonical_json(entries)).hexdigest()

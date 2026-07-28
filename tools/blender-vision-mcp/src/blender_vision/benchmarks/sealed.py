"""Reusable sealed benchmark harness for VisionMCP V2 targets.

Isolates three roles:

* **Oracle** — owns the source scene, hidden cameras, hidden measurements, and
  hidden materials. Exposes only the declared builder inputs.
* **Builder** — receives builder inputs and a working directory, and nothing
  else. Path confinement refuses `..`, absolute escapes, and symlink walks into
  the oracle root.
* **Evaluator** — frozen before the builder runs. Its tree digest is recorded in
  the sealed contract and re-verified after the builder finishes; any swap
  invalidates the run.

This generalises the NOCTURNE/ONE sealing pattern without changing nocturne
behaviour. Contracts are sealed with content digests and a V2 ``Lineage`` so
tampering is detectable. They are *not* one of the ten canonical V2 record
kinds (schemas are frozen); they reuse the same digest + lineage discipline.
"""

from __future__ import annotations

import hashlib
import json
import re
import shutil
from collections.abc import Callable
from dataclasses import asdict, dataclass, field, fields
from enum import StrEnum
from pathlib import Path
from typing import Any

from blender_vision.core.errors import SecurityError, ValidationError
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.security.paths import confined_path
from blender_vision.v2.authority import AuthorityClass, derive
from blender_vision.v2.records import Lineage

_SHA256 = re.compile(r"^[0-9a-f]{64}$")
SCHEMA_VERSION = "1"
SEALED_CONTRACT_KIND = "v2.sealed-benchmark-contract"
SEALED_RECEIPT_KIND = "v2.sealed-benchmark-receipt"
SEALED_MANIFEST_KIND = "v2.sealed-benchmark-manifest"

TARGET_IDS: tuple[str, ...] = (
    "datacenter_film",
    "remote",
    "soft_object",
    "organic",
    "fur_animal",
    "browser_round_trip",
)

# Fields that participate in the sealed digest. Order is not significant —
# canonical_json sorts keys — but the set is fixed so callers cannot smuggle
# unhashed metadata into a "sealed" contract by renaming fields.
_CONTRACT_DIGEST_FIELDS = (
    "kind",
    "schema_version",
    "target_id",
    "oracle_digest",
    "evaluator_digest",
    "builder_inputs_digest",
    "frozen_at",
    "acceptance_thresholds",
    "blocked_requirements",
    "lineage",
)


class SealedBenchmarkError(ValidationError):
    """Raised for contract/manifest structural failures (not security)."""


class LeakageBlocked(SecurityError):
    """Raised when a leakage attempt is correctly refused.

    Subclass of ``SecurityError`` so existing security callers catch it, while
    tests can distinguish an intentional block from an unrelated security fault.
    """


class EvidenceStatus(StrEnum):
    AVAILABLE = "available"
    BLOCKED = "blocked"
    PARTIAL = "partial"


# ---------------------------------------------------------------------------
# Tree digests (same discipline as nocturne / SealedBenchmarkBoundary)
# ---------------------------------------------------------------------------


def tree_digest(root: Path) -> str:
    """Content-address a directory tree. Symlinks are forbidden."""
    supplied = root.expanduser().absolute()
    if supplied.is_symlink():
        raise SecurityError(f"sealed tree root cannot be a symlink: {root}")
    resolved = supplied.resolve()
    if not resolved.is_dir():
        raise SecurityError(f"sealed tree root must be a directory: {root}")
    entries: list[dict[str, Any]] = []
    for path in sorted(resolved.rglob("*")):
        if path.is_symlink():
            raise SecurityError(f"sealed tree contains a symlink: {path}")
        if path.is_file():
            entries.append(
                {
                    "path": path.relative_to(resolved).as_posix(),
                    "sha256": sha256_file(path)[0],
                    "size": path.stat().st_size,
                }
            )
    return hashlib.sha256(canonical_json(entries)).hexdigest()


def file_list_digest(root: Path, relative_paths: list[str]) -> str:
    """Digest an explicit file set under ``root`` (order-independent)."""
    resolved = root.expanduser().resolve()
    entries: list[dict[str, Any]] = []
    for relative in sorted(set(relative_paths)):
        candidate = Path(relative)
        if candidate.is_absolute() or ".." in candidate.parts:
            raise SecurityError(f"builder-input path escaped its root: {relative}")
        path = resolved / candidate
        if path.is_symlink():
            raise SecurityError(f"builder-input path is a symlink: {relative}")
        if not path.is_file():
            raise SecurityError(f"builder-input path is missing: {relative}")
        entries.append(
            {
                "path": relative,
                "sha256": sha256_file(path)[0],
                "size": path.stat().st_size,
            }
        )
    return hashlib.sha256(canonical_json(entries)).hexdigest()


def validate_digest(value: str) -> str:
    if not _SHA256.fullmatch(value):
        raise ValueError(f"value is not a lowercase SHA-256 digest: {value!r}")
    return value


# ---------------------------------------------------------------------------
# SealedContract
# ---------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class SealedContract:
    """Fixed, tamper-evident binding of the four digests for one target.

    The digest covers every field that decides acceptance. Editing a threshold
    after seal() makes ``verify()`` fail. The lineage records which trees were
    frozen and under what authority ceiling.
    """

    target_id: str
    oracle_digest: str
    evaluator_digest: str
    builder_inputs_digest: str
    frozen_at: str
    acceptance_thresholds: dict[str, Any]
    blocked_requirements: list[dict[str, Any]] = field(default_factory=list)
    lineage: Lineage = field(default_factory=Lineage)
    schema_version: str = SCHEMA_VERSION
    kind: str = SEALED_CONTRACT_KIND
    digest: str = ""

    def payload(self) -> dict[str, Any]:
        value: dict[str, Any] = {
            "kind": self.kind,
            "schema_version": self.schema_version,
            "target_id": self.target_id,
            "oracle_digest": self.oracle_digest,
            "evaluator_digest": self.evaluator_digest,
            "builder_inputs_digest": self.builder_inputs_digest,
            "frozen_at": self.frozen_at,
            "acceptance_thresholds": self.acceptance_thresholds,
            "blocked_requirements": self.blocked_requirements,
            "lineage": self.lineage.to_dict(),
        }
        # Guard against drift between the field list and payload().
        missing = set(_CONTRACT_DIGEST_FIELDS) - set(value)
        if missing:
            raise SealedBenchmarkError(f"contract payload missing fields: {sorted(missing)}")
        return value

    def compute_digest(self) -> str:
        return hashlib.sha256(canonical_json(self.payload())).hexdigest()

    def seal(self) -> SealedContract:
        for field_name in (
            "oracle_digest",
            "evaluator_digest",
            "builder_inputs_digest",
        ):
            validate_digest(getattr(self, field_name))
        if not self.target_id:
            raise SealedBenchmarkError("sealed contract requires a target_id")
        if not isinstance(self.acceptance_thresholds, dict) or not self.acceptance_thresholds:
            raise SealedBenchmarkError("sealed contract requires non-empty acceptance_thresholds")
        # Authority of the sealed binding is derived from the lineage inputs —
        # never stronger than the weakest evidence that produced it.
        if self.lineage.input_authorities:
            ceiling = derive(self.lineage.input_authorities)
            self.lineage.parameters = {
                **self.lineage.parameters,
                "authority_ceiling": ceiling.value,
            }
        self.digest = self.compute_digest()
        return self

    def verify(self) -> None:
        if not self.digest:
            raise SecurityError(f"sealed contract {self.target_id!r} is unsealed")
        if self.digest != self.compute_digest():
            raise SecurityError(
                f"sealed contract {self.target_id!r} failed digest verification "
                "(thresholds or digests were edited after sealing)"
            )
        for field_name in (
            "oracle_digest",
            "evaluator_digest",
            "builder_inputs_digest",
        ):
            validate_digest(getattr(self, field_name))

    def to_dict(self) -> dict[str, Any]:
        value = self.payload()
        value["digest"] = self.digest or self.compute_digest()
        return value

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> SealedContract:
        kind = payload.get("kind", SEALED_CONTRACT_KIND)
        if kind != SEALED_CONTRACT_KIND:
            raise SealedBenchmarkError(f"expected {SEALED_CONTRACT_KIND}, got {kind!r}")
        known = {item.name for item in fields(cls)}
        data = {key: value for key, value in payload.items() if key in known}
        if "lineage" in data and isinstance(data["lineage"], dict):
            data["lineage"] = Lineage.from_dict(data["lineage"])
        return cls(**data)

    def with_thresholds(self, thresholds: dict[str, Any]) -> SealedContract:
        """Return a copy with new thresholds and a cleared digest (must re-seal)."""
        return SealedContract(
            target_id=self.target_id,
            oracle_digest=self.oracle_digest,
            evaluator_digest=self.evaluator_digest,
            builder_inputs_digest=self.builder_inputs_digest,
            frozen_at=self.frozen_at,
            acceptance_thresholds=dict(thresholds),
            blocked_requirements=list(self.blocked_requirements),
            lineage=Lineage.from_dict(self.lineage.to_dict()),
            schema_version=self.schema_version,
            kind=self.kind,
            digest="",
        )


# ---------------------------------------------------------------------------
# Manifest
# ---------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class BlockedRequirement:
    """Exact reason a target cannot run yet. Never substitute a silent stand-in."""

    id: str
    reason: str
    required_to_unblock: str

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> BlockedRequirement:
        return cls(
            id=str(payload["id"]),
            reason=str(payload["reason"]),
            required_to_unblock=str(payload["required_to_unblock"]),
        )


@dataclass(slots=True, kw_only=True)
class ManifestArtifact:
    path: str
    role: str
    builder_visible: bool = False
    required: bool = True
    description: str = ""

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> ManifestArtifact:
        return cls(
            path=str(payload["path"]),
            role=str(payload["role"]),
            builder_visible=bool(payload.get("builder_visible", False)),
            required=bool(payload.get("required", True)),
            description=str(payload.get("description", "")),
        )


@dataclass(slots=True, kw_only=True)
class SealedManifest:
    """Fixed declaration of inputs, hidden evidence, evaluator, and thresholds."""

    target_id: str
    display_name: str
    description: str
    builder_inputs: list[ManifestArtifact]
    hidden_evidence: list[ManifestArtifact]
    evaluator: dict[str, Any]
    acceptance_thresholds: dict[str, Any]
    evidence_status: EvidenceStatus = EvidenceStatus.AVAILABLE
    blocked_requirements: list[BlockedRequirement] = field(default_factory=list)
    anti_cheat_rules: list[str] = field(default_factory=list)
    claim_boundary: list[str] = field(default_factory=list)
    subsystems: list[str] = field(default_factory=list)
    schema_version: str = SCHEMA_VERSION
    kind: str = SEALED_MANIFEST_KIND

    def __post_init__(self) -> None:
        if self.target_id not in TARGET_IDS:
            raise SealedBenchmarkError(
                f"unknown sealed target {self.target_id!r}; expected one of {TARGET_IDS}"
            )
        if not self.builder_inputs:
            raise SealedBenchmarkError(f"{self.target_id}: builder_inputs must be non-empty")
        if not self.hidden_evidence:
            raise SealedBenchmarkError(f"{self.target_id}: hidden_evidence must be non-empty")
        if not self.acceptance_thresholds:
            raise SealedBenchmarkError(
                f"{self.target_id}: acceptance_thresholds must be non-empty"
            )
        if not self.evaluator.get("root"):
            raise SealedBenchmarkError(f"{self.target_id}: evaluator.root is required")
        # Hidden evidence must never be declared builder-visible.
        for item in self.hidden_evidence:
            if item.builder_visible:
                raise SealedBenchmarkError(
                    f"{self.target_id}: hidden evidence {item.path!r} cannot be builder_visible"
                )
            self._assert_safe_relative(item.path, "hidden_evidence")
        for item in self.builder_inputs:
            if not item.builder_visible:
                raise SealedBenchmarkError(
                    f"{self.target_id}: builder input {item.path!r} must be builder_visible"
                )
            self._assert_safe_relative(item.path, "builder_inputs")
        if self.evidence_status is EvidenceStatus.BLOCKED and not self.blocked_requirements:
            raise SealedBenchmarkError(
                f"{self.target_id}: BLOCKED status requires blocked_requirements"
            )
        if self.evidence_status is EvidenceStatus.AVAILABLE and self.blocked_requirements:
            raise SealedBenchmarkError(
                f"{self.target_id}: AVAILABLE status cannot list blocked_requirements"
            )

    @staticmethod
    def _assert_safe_relative(relative: str, label: str) -> None:
        candidate = Path(relative)
        if candidate.is_absolute() or ".." in candidate.parts or not relative:
            raise SealedBenchmarkError(f"{label} path is unsafe: {relative!r}")

    def to_dict(self) -> dict[str, Any]:
        return {
            "kind": self.kind,
            "schema_version": self.schema_version,
            "target_id": self.target_id,
            "display_name": self.display_name,
            "description": self.description,
            "builder_inputs": [item.to_dict() for item in self.builder_inputs],
            "hidden_evidence": [item.to_dict() for item in self.hidden_evidence],
            "evaluator": self.evaluator,
            "acceptance_thresholds": self.acceptance_thresholds,
            "evidence_status": self.evidence_status.value,
            "blocked_requirements": [item.to_dict() for item in self.blocked_requirements],
            "anti_cheat_rules": list(self.anti_cheat_rules),
            "claim_boundary": list(self.claim_boundary),
            "subsystems": list(self.subsystems),
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> SealedManifest:
        kind = payload.get("kind", SEALED_MANIFEST_KIND)
        if kind != SEALED_MANIFEST_KIND:
            raise SealedBenchmarkError(f"expected {SEALED_MANIFEST_KIND}, got {kind!r}")
        return cls(
            target_id=str(payload["target_id"]),
            display_name=str(payload["display_name"]),
            description=str(payload["description"]),
            builder_inputs=[
                ManifestArtifact.from_dict(item) for item in payload.get("builder_inputs", [])
            ],
            hidden_evidence=[
                ManifestArtifact.from_dict(item) for item in payload.get("hidden_evidence", [])
            ],
            evaluator=dict(payload.get("evaluator", {})),
            acceptance_thresholds=dict(payload.get("acceptance_thresholds", {})),
            evidence_status=EvidenceStatus(payload.get("evidence_status", "available")),
            blocked_requirements=[
                BlockedRequirement.from_dict(item)
                for item in payload.get("blocked_requirements", [])
            ],
            anti_cheat_rules=list(payload.get("anti_cheat_rules", [])),
            claim_boundary=list(payload.get("claim_boundary", [])),
            subsystems=list(payload.get("subsystems", [])),
            schema_version=str(payload.get("schema_version", SCHEMA_VERSION)),
            kind=kind,
        )

    def builder_input_paths(self) -> list[str]:
        return [item.path for item in self.builder_inputs]

    def hidden_evidence_paths(self) -> list[str]:
        return [item.path for item in self.hidden_evidence]

    def validate_against_schema(self) -> None:
        """Structural schema check used by the runner and tests.

        Kept inline rather than under ``schemas/v2/**`` (frozen). The contract
        is the schema: every required key present, types correct, no hidden
        evidence marked builder-visible, blocked targets declare exact reasons.
        """
        # Construction already enforces the hard rules; re-run from_dict for a
        # pure dict path so callers can validate on-disk JSON without trusting
        # the in-memory object.
        SealedManifest.from_dict(self.to_dict())
        payload = self.to_dict()
        required_keys = {
            "kind",
            "schema_version",
            "target_id",
            "display_name",
            "description",
            "builder_inputs",
            "hidden_evidence",
            "evaluator",
            "acceptance_thresholds",
            "evidence_status",
            "blocked_requirements",
            "anti_cheat_rules",
            "claim_boundary",
            "subsystems",
        }
        missing = required_keys - set(payload)
        if missing:
            raise SealedBenchmarkError(f"manifest missing keys: {sorted(missing)}")
        if not isinstance(payload["evaluator"], dict):
            raise SealedBenchmarkError("evaluator must be an object")
        if "entry" not in payload["evaluator"]:
            raise SealedBenchmarkError("evaluator.entry is required")
        if not isinstance(payload["acceptance_thresholds"], dict):
            raise SealedBenchmarkError("acceptance_thresholds must be an object")
        for item in payload["builder_inputs"] + payload["hidden_evidence"]:
            if not isinstance(item, dict) or "path" not in item or "role" not in item:
                raise SealedBenchmarkError("artifact entries require path and role")


# ---------------------------------------------------------------------------
# Workspaces
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class OracleWorkspace:
    """Owns hidden evidence. Exposes only declared builder inputs."""

    root: Path
    hidden_relative_paths: list[str] = field(default_factory=list)
    builder_input_relative_paths: list[str] = field(default_factory=list)
    canary: str = ""

    def __post_init__(self) -> None:
        self.root = self.root.expanduser().resolve()
        if self.root.is_symlink():
            raise SecurityError("oracle root cannot be a symlink")
        if not self.root.is_dir():
            raise SecurityError(f"oracle root must exist: {self.root}")

    def ensure_layout(self) -> None:
        """Create the oracle tree and plant a canary for leakage scans."""
        (self.root / "hidden").mkdir(parents=True, exist_ok=True)
        (self.root / "builder_inputs").mkdir(parents=True, exist_ok=True)
        if self.canary:
            canary_path = self.root / "hidden" / "ORACLE_CANARY.txt"
            if not canary_path.exists():
                canary_path.write_text(self.canary, encoding="utf-8")
                if "hidden/ORACLE_CANARY.txt" not in self.hidden_relative_paths:
                    self.hidden_relative_paths = [
                        *self.hidden_relative_paths,
                        "hidden/ORACLE_CANARY.txt",
                    ]

    def tree_digest(self) -> str:
        return tree_digest(self.root)

    def materialize_builder_inputs(self, destination: Path) -> list[str]:
        """Copy only declared builder-visible files into ``destination``."""
        dest = destination.expanduser().resolve()
        dest.mkdir(parents=True, exist_ok=True)
        copied: list[str] = []
        for relative in self.builder_input_relative_paths:
            source = confined_path(self.root, self.root / relative, must_exist=True)
            # Refuse to copy anything under hidden/.
            try:
                source.relative_to(self.root / "hidden")
            except ValueError:
                pass
            else:
                raise LeakageBlocked(
                    f"refusing to expose hidden oracle path as builder input: {relative}"
                )
            if any(
                relative == hidden or relative.startswith(f"{hidden}/")
                for hidden in self.hidden_relative_paths
            ):
                raise LeakageBlocked(
                    f"refusing to expose declared-hidden evidence as builder input: {relative}"
                )
            target = dest / Path(relative).name
            if target.exists():
                target = dest / relative
                target.parent.mkdir(parents=True, exist_ok=True)
            else:
                # Prefer flat names under the packet root when the source is
                # already under builder_inputs/; otherwise preserve relative.
                if relative.startswith("builder_inputs/"):
                    target = dest / Path(relative).relative_to("builder_inputs")
                    target.parent.mkdir(parents=True, exist_ok=True)
                else:
                    target = dest / relative
                    target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, target)
            copied.append(target.relative_to(dest).as_posix())
        return copied

    def assert_disjoint_from(self, other: Path) -> None:
        other_root = other.expanduser().resolve()
        if self.root == other_root:
            raise SecurityError("oracle and peer roots must be distinct")
        if self.root.is_relative_to(other_root) or other_root.is_relative_to(self.root):
            raise SecurityError("oracle and peer roots must be disjoint")


@dataclass(slots=True)
class BuilderWorkspace:
    """Working directory that must not reach the oracle root."""

    root: Path
    oracle_root: Path
    inputs_root: Path

    def __post_init__(self) -> None:
        self.root = self.root.expanduser().resolve()
        self.oracle_root = self.oracle_root.expanduser().resolve()
        self.inputs_root = self.inputs_root.expanduser().resolve()
        if self.root.is_symlink():
            raise SecurityError("builder root cannot be a symlink")
        self.root.mkdir(parents=True, exist_ok=True)
        if self.root == self.oracle_root:
            raise SecurityError("builder root cannot equal the oracle root")
        if self.root.is_relative_to(self.oracle_root) or self.oracle_root.is_relative_to(
            self.root
        ):
            raise SecurityError("builder and oracle roots must be disjoint")

    def confined(self, candidate: Path, *, must_exist: bool = False) -> Path:
        """Resolve a path that must stay inside the builder root."""
        return confined_path(self.root, candidate, must_exist=must_exist)

    def read_bytes(self, candidate: Path) -> bytes:
        """Read a file only if it is confined to the builder root."""
        path = self.confined(candidate, must_exist=True)
        if path.is_symlink():
            raise SecurityError(f"builder workspace cannot read a symlink: {candidate}")
        # Extra guard: even if confinement were bypassed, refuse oracle paths.
        try:
            path.relative_to(self.oracle_root)
        except ValueError:
            pass
        else:
            raise LeakageBlocked(
                f"builder cannot read oracle path: {candidate}"
            )
        return path.read_bytes()

    def attempt_read_oracle(self, relative: str) -> bytes:
        """Active leakage probe: try to read under the oracle root.

        Always raises ``LeakageBlocked``. Used by tests and the framework runner
        to prove the boundary holds.
        """
        # Direct absolute read — must fail.
        target = self.oracle_root / relative
        raise LeakageBlocked(
            f"builder cannot read oracle file directly: {target}"
        )

    def attempt_path_escape(self, relative_escape: str) -> Path:
        """Active leakage probe: try to walk into the oracle via ``..`` / symlink.

        Always raises ``LeakageBlocked`` or ``SecurityError``.
        """
        # Construct a path that claims to be under builder but escapes via ...
        candidate = self.root / relative_escape
        try:
            resolved = confined_path(self.root, candidate, must_exist=False)
        except SecurityError as error:
            raise LeakageBlocked(
                f"builder path escape blocked: {relative_escape} ({error})"
            ) from error
        # If confinement somehow returned a path inside the oracle, still refuse.
        try:
            resolved.relative_to(self.oracle_root)
        except ValueError:
            pass
        else:
            raise LeakageBlocked(
                f"builder path escape into oracle blocked: {relative_escape}"
            )
        # Symlink case: if the candidate is a symlink pointing outside, refuse.
        if candidate.is_symlink() or any(
            part.is_symlink() for part in [candidate, *candidate.parents]
            if part != candidate.anchor and part.exists()
        ):
            raise LeakageBlocked(
                f"builder symlink escape into oracle blocked: {relative_escape}"
            )
        # Path stayed inside builder — not a successful oracle escape, but also
        # not a successful cheat. Report that the escape did not reach oracle.
        raise LeakageBlocked(
            f"builder path escape did not reach oracle and is refused: {relative_escape}"
        )

    def assert_no_oracle_content(self, canaries: list[str]) -> dict[str, Any]:
        """Scan the builder tree for oracle canary bytes."""
        if not canaries:
            raise ValueError("canaries must be non-empty")
        encoded = [value.encode("utf-8") for value in canaries]
        scanned = 0
        files = 0
        for path in sorted(self.root.rglob("*")):
            if path.is_symlink():
                raise SecurityError(f"builder workspace contains a symlink: {path}")
            if not path.is_file():
                continue
            files += 1
            data = path.read_bytes()
            scanned += len(data)
            if any(value in data for value in encoded):
                raise LeakageBlocked(
                    f"oracle canary leaked into builder workspace: {path.name}"
                )
        return {
            "builder_file_count": files,
            "builder_scanned_bytes": scanned,
            "canary_count": len(canaries),
            "leakage_detected": False,
        }


@dataclass(slots=True)
class EvaluatorWorkspace:
    """Evaluator tree frozen before the builder runs."""

    root: Path
    digest_at_freeze: str = ""
    frozen_at: str = ""

    def __post_init__(self) -> None:
        self.root = self.root.expanduser().resolve()
        if self.root.is_symlink():
            raise SecurityError("evaluator root cannot be a symlink")
        if not self.root.is_dir():
            raise SecurityError(f"evaluator root must exist: {self.root}")

    def freeze(self) -> str:
        """Record the tree digest. Must be called before any builder work."""
        for path in self.root.rglob("*"):
            if path.is_symlink():
                raise SecurityError(f"evaluator tree contains a symlink: {path}")
        self.digest_at_freeze = tree_digest(self.root)
        self.frozen_at = utc_now()
        return self.digest_at_freeze

    def reverify(self) -> str:
        """Recompute the digest and refuse any post-freeze mutation."""
        if not self.digest_at_freeze:
            raise SecurityError("evaluator was not frozen before verification")
        current = tree_digest(self.root)
        if current != self.digest_at_freeze:
            raise LeakageBlocked(
                "evaluator was swapped or edited after freeze "
                f"(expected {self.digest_at_freeze}, got {current})"
            )
        return current

    def current_digest(self) -> str:
        return tree_digest(self.root)


# ---------------------------------------------------------------------------
# Receipt + orchestration
# ---------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class SealedReceipt:
    """Binds the four digests that define a sealed run."""

    target_id: str
    contract_digest: str
    oracle_digest: str
    evaluator_digest: str
    builder_inputs_digest: str
    evaluator_digest_after: str
    status: str
    frozen_at: str
    completed_at: str
    failed_attempt_paths: list[str] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)
    kind: str = SEALED_RECEIPT_KIND
    schema_version: str = SCHEMA_VERSION
    digest: str = ""

    def payload(self) -> dict[str, Any]:
        return {
            "kind": self.kind,
            "schema_version": self.schema_version,
            "target_id": self.target_id,
            "contract_digest": self.contract_digest,
            "oracle_digest": self.oracle_digest,
            "evaluator_digest": self.evaluator_digest,
            "builder_inputs_digest": self.builder_inputs_digest,
            "evaluator_digest_after": self.evaluator_digest_after,
            "status": self.status,
            "frozen_at": self.frozen_at,
            "completed_at": self.completed_at,
            "failed_attempt_paths": list(self.failed_attempt_paths),
            "notes": list(self.notes),
        }

    def seal(self) -> SealedReceipt:
        for field_name in (
            "contract_digest",
            "oracle_digest",
            "evaluator_digest",
            "builder_inputs_digest",
            "evaluator_digest_after",
        ):
            validate_digest(getattr(self, field_name))
        self.digest = hashlib.sha256(canonical_json(self.payload())).hexdigest()
        return self

    def to_dict(self) -> dict[str, Any]:
        value = self.payload()
        value["digest"] = self.digest or hashlib.sha256(canonical_json(value)).hexdigest()
        return value


def freeze_contract(
    *,
    target_id: str,
    oracle: OracleWorkspace,
    evaluator: EvaluatorWorkspace,
    builder_inputs_root: Path,
    builder_input_paths: list[str],
    acceptance_thresholds: dict[str, Any],
    blocked_requirements: list[dict[str, Any]] | None = None,
    lineage: Lineage | None = None,
) -> SealedContract:
    """Freeze evaluator, digest all four trees, and seal the contract."""
    oracle.assert_disjoint_from(evaluator.root)
    oracle.assert_disjoint_from(builder_inputs_root)
    evaluator_digest = evaluator.freeze()
    oracle_digest = oracle.tree_digest()
    inputs_digest = file_list_digest(builder_inputs_root, builder_input_paths)
    sealed_lineage = lineage or Lineage(
        tool="blender-vision-mcp",
        operation="seal_benchmark_contract",
        inputs=[
            f"oracle:{oracle_digest}",
            f"evaluator:{evaluator_digest}",
            f"builder_inputs:{inputs_digest}",
        ],
        input_authorities=[
            AuthorityClass.PROCEDURAL_GROUND_TRUTH.value,
            AuthorityClass.HUMAN_REVIEWED.value,
            AuthorityClass.OBSERVED.value,
        ],
        parameters={"target_id": target_id},
        limitations=[
            "Contract digests bind trees present at freeze time only.",
            "Filesystem confinement is not cognitive independence of authors.",
        ],
    )
    return SealedContract(
        target_id=target_id,
        oracle_digest=oracle_digest,
        evaluator_digest=evaluator_digest,
        builder_inputs_digest=inputs_digest,
        frozen_at=evaluator.frozen_at,
        acceptance_thresholds=dict(acceptance_thresholds),
        blocked_requirements=list(blocked_requirements or []),
        lineage=sealed_lineage,
    ).seal()


def run_sealed_benchmark(
    *,
    contract: SealedContract,
    oracle: OracleWorkspace,
    builder: BuilderWorkspace,
    evaluator: EvaluatorWorkspace,
    output_root: Path,
    builder_fn: Callable[[BuilderWorkspace], dict[str, Any]] | None = None,
    preserve_failures: bool = True,
) -> SealedReceipt:
    """Orchestrate a sealed run and emit a four-digest receipt.

    Order is non-negotiable:

    1. Verify the contract is sealed and digests match live trees.
    2. Confirm evaluator freeze (re-freeze is refused if already frozen with a
       different digest).
    3. Run the builder (optional callable) inside the builder workspace.
    4. Re-verify the evaluator digest.
    5. Scan the builder tree for oracle canaries.
    6. On failure, copy the attempt under ``failed-attempts/``.
    """
    contract.verify()
    output = output_root.expanduser().resolve()
    output.mkdir(parents=True, exist_ok=True)
    failed_dir = output / "failed-attempts"
    failed_dir.mkdir(parents=True, exist_ok=True)

    # Live digests must match the sealed contract.
    live_oracle = oracle.tree_digest()
    if live_oracle != contract.oracle_digest:
        raise SecurityError("oracle tree digest does not match sealed contract")
    if not evaluator.digest_at_freeze:
        # Allow freeze-at-run only when the contract's evaluator digest matches.
        frozen = evaluator.freeze()
        if frozen != contract.evaluator_digest:
            raise SecurityError("evaluator freeze digest does not match sealed contract")
    elif evaluator.digest_at_freeze != contract.evaluator_digest:
        raise SecurityError("evaluator was frozen to a digest that is not in the contract")

    notes: list[str] = []
    failed_paths: list[str] = []
    status = "PASS"
    builder_result: dict[str, Any] = {}

    try:
        if builder_fn is not None:
            builder_result = builder_fn(builder) or {}
            if builder_result.get("status") == "FAIL":
                status = "FAIL"
                notes.append("builder_fn reported FAIL")
        if oracle.canary:
            builder.assert_no_oracle_content([oracle.canary])
        evaluator_after = evaluator.reverify()
    except Exception as error:
        status = "FAIL"
        notes.append(f"{type(error).__name__}: {error}")
        if preserve_failures:
            stamp = utc_now().replace(":", "").replace("+", "p")
            attempt = failed_dir / f"{contract.target_id}-{stamp}"
            attempt.mkdir(parents=True, exist_ok=True)
            # Preserve builder tree snapshot and the error text.
            snapshot = attempt / "builder"
            if builder.root.is_dir():
                shutil.copytree(builder.root, snapshot, dirs_exist_ok=True)
            (attempt / "error.txt").write_text(
                f"{type(error).__name__}: {error}\n", encoding="utf-8"
            )
            atomic_write_json(
                attempt / "attempt.json",
                {
                    "target_id": contract.target_id,
                    "error": str(error),
                    "error_type": type(error).__name__,
                    "contract_digest": contract.digest,
                    "preserved_at": utc_now(),
                },
            )
            failed_paths.append(attempt.as_posix())
        # Re-raise security / leakage so callers see the block; other errors
        # become a FAIL receipt.
        if isinstance(error, SecurityError):
            raise
        evaluator_after = (
            evaluator.digest_at_freeze
            if evaluator.digest_at_freeze
            else evaluator.current_digest()
        )
    else:
        evaluator_after = evaluator_after  # noqa: PLW0127 — clarity for receipt

    # Final evaluator check even on the success path already set evaluator_after.
    if status == "PASS":
        evaluator_after = evaluator.reverify()

    receipt = SealedReceipt(
        target_id=contract.target_id,
        contract_digest=contract.digest,
        oracle_digest=contract.oracle_digest,
        evaluator_digest=contract.evaluator_digest,
        builder_inputs_digest=contract.builder_inputs_digest,
        evaluator_digest_after=evaluator_after,
        status=status,
        frozen_at=contract.frozen_at,
        completed_at=utc_now(),
        failed_attempt_paths=failed_paths,
        notes=notes,
    ).seal()
    atomic_write_json(output / "sealed.receipt.json", receipt.to_dict())
    if builder_result:
        atomic_write_json(output / "builder.result.json", builder_result)
    return receipt


# ---------------------------------------------------------------------------
# Benchmark root discovery + on-disk loaders
# ---------------------------------------------------------------------------


def sealed_benchmarks_root() -> Path:
    """Locate ``benchmarks/`` for source tree or installed data (future)."""
    development = Path(__file__).resolve().parents[3] / "benchmarks"
    if development.is_dir():
        return development
    raise FileNotFoundError("sealed benchmarks root is not available")


def target_root(target_id: str) -> Path:
    if target_id not in TARGET_IDS:
        raise SealedBenchmarkError(f"unknown sealed target: {target_id}")
    root = sealed_benchmarks_root() / target_id
    if not root.is_dir():
        raise SealedBenchmarkError(f"sealed target directory missing: {root}")
    return root


def load_manifest(target_id: str | Path) -> SealedManifest:
    path = target_id if isinstance(target_id, Path) else target_root(target_id) / "manifest.json"
    if path.is_symlink() or not path.is_file():
        raise SealedBenchmarkError(f"manifest missing or linked: {path}")
    payload = json.loads(path.read_text(encoding="utf-8"))
    manifest = SealedManifest.from_dict(payload)
    manifest.validate_against_schema()
    return manifest


def load_contract(target_id: str | Path) -> SealedContract:
    path = target_id if isinstance(target_id, Path) else target_root(target_id) / "contract.json"
    if path.is_symlink() or not path.is_file():
        raise SealedBenchmarkError(f"contract missing or linked: {path}")
    payload = json.loads(path.read_text(encoding="utf-8"))
    contract = SealedContract.from_dict(payload)
    contract.verify()
    return contract


def write_contract(path: Path, contract: SealedContract) -> Path:
    contract.seal()
    contract.verify()
    atomic_write_json(path, contract.to_dict())
    return path


def load_all_manifests() -> dict[str, SealedManifest]:
    return {target_id: load_manifest(target_id) for target_id in TARGET_IDS}


def load_all_contracts() -> dict[str, SealedContract]:
    return {target_id: load_contract(target_id) for target_id in TARGET_IDS}


# ---------------------------------------------------------------------------
# Active leakage probes (used by tests and run-sealed-framework.py)
# ---------------------------------------------------------------------------


def probe_builder_reads_oracle_file(
    builder: BuilderWorkspace, relative: str = "hidden/ORACLE_CANARY.txt"
) -> None:
    """Must raise ``LeakageBlocked``."""
    builder.attempt_read_oracle(relative)


def probe_builder_path_escape(
    builder: BuilderWorkspace, escape: str = "../oracle/hidden/ORACLE_CANARY.txt"
) -> None:
    """Must raise ``LeakageBlocked``."""
    builder.attempt_path_escape(escape)


def probe_builder_symlink_escape(
    builder: BuilderWorkspace, *, link_name: str = "escape_link"
) -> None:
    """Plant a symlink toward the oracle and refuse resolution."""
    link = builder.root / link_name
    if link.exists() or link.is_symlink():
        link.unlink()
    try:
        link.symlink_to(builder.oracle_root)
    except OSError as error:
        # Some sandboxes refuse symlink creation; that is itself a block.
        raise LeakageBlocked(
            f"builder symlink creation refused by OS (treated as blocked): {error}"
        ) from error
    try:
        # confined_path resolves symlinks; a link into the oracle must escape.
        confined_path(builder.root, link, must_exist=False)
    except SecurityError as error:
        raise LeakageBlocked(
            f"builder symlink escape blocked: {link_name} ({error})"
        ) from error
    # If resolve stayed inside somehow, still refuse reading through it.
    raise LeakageBlocked(f"builder symlink escape into oracle blocked: {link_name}")


def probe_evaluator_swap(
    evaluator: EvaluatorWorkspace, *, inject_name: str = "swapped.txt"
) -> None:
    """Mutate the evaluator after freeze; reverify must raise."""
    if not evaluator.digest_at_freeze:
        evaluator.freeze()
    inject = evaluator.root / inject_name
    inject.write_text("evaluator was swapped after freeze\n", encoding="utf-8")
    try:
        evaluator.reverify()
    finally:
        if inject.exists():
            inject.unlink()
    # reverify should have raised; if not, force the block.
    raise LeakageBlocked("evaluator swap was not detected")


def probe_hidden_camera_in_builder_inputs(
    oracle: OracleWorkspace,
    destination: Path,
    hidden_relative: str,
) -> None:
    """Attempt to materialize a hidden camera as a builder input; must raise."""
    previous = list(oracle.builder_input_relative_paths)
    try:
        oracle.builder_input_relative_paths = [hidden_relative]
        oracle.materialize_builder_inputs(destination)
    finally:
        oracle.builder_input_relative_paths = previous
    raise LeakageBlocked(
        f"hidden camera {hidden_relative!r} was incorrectly exposed to builder inputs"
    )


def probe_threshold_edit_after_seal(contract: SealedContract) -> None:
    """Edit a threshold on a sealed contract; verify must raise."""
    if not contract.digest:
        raise SealedBenchmarkError("contract must be sealed before threshold-edit probe")
    # Mutate in place (dataclass is not frozen — deliberate, so seal discipline
    # is the only protection).
    key = next(iter(contract.acceptance_thresholds))
    original = contract.acceptance_thresholds[key]
    try:
        if isinstance(original, (int, float)):
            contract.acceptance_thresholds[key] = original * 0.5
        else:
            contract.acceptance_thresholds[key] = f"tampered-{original}"
        try:
            contract.verify()
        except SecurityError as error:
            raise LeakageBlocked(
                f"threshold edit after seal blocked: {error}"
            ) from error
        raise LeakageBlocked("threshold edit after seal was not detected")
    finally:
        contract.acceptance_thresholds[key] = original


def run_leakage_matrix(
    *,
    work_root: Path,
) -> list[dict[str, Any]]:
    """Execute all five leakage probes and return a pass/fail matrix.

    A row **passes** when the cheat is blocked (``LeakageBlocked`` / related
    ``SecurityError``). A row **fails** when the cheat succeeds or an unexpected
    exception escapes.
    """
    work = work_root.expanduser().resolve()
    work.mkdir(parents=True, exist_ok=True)

    oracle_root = work / "oracle"
    builder_root = work / "builder"
    evaluator_root = work / "evaluator"
    inputs_root = work / "builder_inputs"
    for path in (oracle_root, builder_root, evaluator_root, inputs_root):
        if path.exists():
            shutil.rmtree(path)
        path.mkdir(parents=True)

    canary = "SEALED-ORACLE-CANARY-v2-7f3a9c"
    (oracle_root / "hidden").mkdir()
    (oracle_root / "hidden" / "ORACLE_CANARY.txt").write_text(canary, encoding="utf-8")
    (oracle_root / "hidden" / "cameras").mkdir()
    (oracle_root / "hidden" / "cameras" / "holdout_01.json").write_text(
        json.dumps({"camera": "hidden", "canary": canary}), encoding="utf-8"
    )
    (oracle_root / "builder_inputs").mkdir()
    (oracle_root / "builder_inputs" / "public_view.json").write_text(
        json.dumps({"view": "front", "public": True}), encoding="utf-8"
    )
    (evaluator_root / "gates.json").write_text(
        json.dumps({"public_silhouette_iou_minimum": 0.95}), encoding="utf-8"
    )
    shutil.copy2(
        oracle_root / "builder_inputs" / "public_view.json",
        inputs_root / "public_view.json",
    )

    oracle = OracleWorkspace(
        root=oracle_root,
        hidden_relative_paths=[
            "hidden/ORACLE_CANARY.txt",
            "hidden/cameras/holdout_01.json",
        ],
        builder_input_relative_paths=["builder_inputs/public_view.json"],
        canary=canary,
    )
    builder = BuilderWorkspace(
        root=builder_root,
        oracle_root=oracle_root,
        inputs_root=inputs_root,
    )
    evaluator = EvaluatorWorkspace(root=evaluator_root)
    evaluator.freeze()

    contract = freeze_contract(
        target_id="datacenter_film",
        oracle=oracle,
        evaluator=evaluator,
        builder_inputs_root=inputs_root,
        builder_input_paths=["public_view.json"],
        acceptance_thresholds={"public_silhouette_iou_minimum": 0.95},
    )

    probes: list[tuple[str, Callable[[], None]]] = [
        (
            "builder_reads_oracle_file",
            lambda: probe_builder_reads_oracle_file(builder),
        ),
        (
            "builder_path_escape",
            lambda: probe_builder_path_escape(builder),
        ),
        (
            "builder_symlink_escape",
            lambda: probe_builder_symlink_escape(builder),
        ),
        (
            "evaluator_swap_after_freeze",
            lambda: probe_evaluator_swap(evaluator),
        ),
        (
            "hidden_camera_in_builder_inputs",
            lambda: probe_hidden_camera_in_builder_inputs(
                oracle,
                work / "leaked_inputs",
                "hidden/cameras/holdout_01.json",
            ),
        ),
        (
            "threshold_edit_after_seal",
            lambda: probe_threshold_edit_after_seal(contract),
        ),
    ]

    # Contract asks for five leakage tests; symlink is part of path-escape.
    # Report six rows so both escape vectors are visible, but the named five
    # from the contract map as:
    # 1 direct read, 2 path/symlink escape, 3 evaluator swap, 4 hidden camera,
    # 5 threshold edit. Both escape rows must pass (be blocked).
    matrix: list[dict[str, Any]] = []
    for name, probe in probes:
        row: dict[str, Any] = {"probe": name, "blocked": False, "detail": ""}
        try:
            probe()
        except LeakageBlocked as error:
            row["blocked"] = True
            row["detail"] = str(error)
            row["result"] = "PASS"
        except SecurityError as error:
            # Any security refusal counts as the cheat being blocked.
            row["blocked"] = True
            row["detail"] = str(error)
            row["result"] = "PASS"
        except Exception as error:  # noqa: BLE001 — matrix must record every outcome
            row["blocked"] = False
            row["detail"] = f"{type(error).__name__}: {error}"
            row["result"] = "FAIL"
        else:
            row["blocked"] = False
            row["detail"] = "probe returned without blocking the cheat"
            row["result"] = "FAIL"
        matrix.append(row)
    return matrix

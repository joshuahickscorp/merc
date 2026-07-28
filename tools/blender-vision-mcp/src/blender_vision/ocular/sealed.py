"""Phase S — sealed ocular benchmarks with single split authority.

Eight targets: live tabletop, remote, reflective/transparent object, browser
page, data-centre, dynamic room memory, soft object, organic/fur.

Oracle / builder / evaluator isolation. Train/hidden split is owned by exactly
one canonical source (``SplitAuthority``); indices are never recomputed after
seal. Leakage canaries inject secret markers and verify absence from builder
views.
"""

from __future__ import annotations

import hashlib
import json
import secrets
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any

from blender_vision.core.errors import SecurityError, ValidationError
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.ocular.attestation import ExecutionClass
from blender_vision.security.paths import confined_path
from blender_vision.v2.authority import AuthorityClass
from blender_vision.v2.records import Lineage

SCHEMA_VERSION = "1"
SEALED_OCULAR_KIND = "ocular.sealed-benchmark-contract"
TARGET_IDS: tuple[str, ...] = (
    "live_tabletop",
    "remote",
    "reflective_transparent",
    "browser_page",
    "data_centre",
    "dynamic_room_memory",
    "soft_object",
    "organic_fur",
)


class EvidenceStatus(StrEnum):
    AVAILABLE = "available"
    BLOCKED = "blocked"
    PARTIAL = "partial"
    SYNTHETIC = "synthetic"


class Role(StrEnum):
    ORACLE = "oracle"
    BUILDER = "builder"
    EVALUATOR = "evaluator"


@dataclass(slots=True)
class SplitAuthority:
    """Single canonical train/hidden split. Never recompute after seal.

    The leak that previously survived a physical run happened when train/hidden
    membership was recomputed from a different formula than the one that wrote
    the files. This object is the only allowed source of truth.
    """

    target_id: str
    seed: int
    total_views: int
    train_indices: tuple[int, ...]
    hidden_indices: tuple[int, ...]
    digest: str = ""
    sealed: bool = False

    def __post_init__(self) -> None:
        train = set(self.train_indices)
        hidden = set(self.hidden_indices)
        if train & hidden:
            raise ValidationError(
                f"split overlap for {self.target_id}: {sorted(train & hidden)}"
            )
        if train | hidden != set(range(self.total_views)):
            raise ValidationError(
                f"split does not cover [0, {self.total_views}) for {self.target_id}"
            )
        if not self.train_indices or not self.hidden_indices:
            raise ValidationError(f"empty train or hidden for {self.target_id}")

    def seal(self) -> SplitAuthority:
        payload = {
            "target_id": self.target_id,
            "seed": self.seed,
            "total_views": self.total_views,
            "train_indices": list(self.train_indices),
            "hidden_indices": list(self.hidden_indices),
        }
        self.digest = hashlib.sha256(canonical_json(payload)).hexdigest()
        self.sealed = True
        return self

    def is_train(self, index: int) -> bool:
        if not self.sealed:
            raise ValidationError("SplitAuthority must be sealed before use")
        return index in self.train_indices

    def is_hidden(self, index: int) -> bool:
        if not self.sealed:
            raise ValidationError("SplitAuthority must be sealed before use")
        return index in self.hidden_indices

    def assert_builder_may_read(self, index: int) -> None:
        if self.is_hidden(index):
            raise SecurityError(
                f"builder denied hidden view index {index} for {self.target_id}"
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "target_id": self.target_id,
            "seed": self.seed,
            "total_views": self.total_views,
            "train_indices": list(self.train_indices),
            "hidden_indices": list(self.hidden_indices),
            "digest": self.digest,
            "sealed": self.sealed,
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> SplitAuthority:
        split = cls(
            target_id=str(payload["target_id"]),
            seed=int(payload["seed"]),
            total_views=int(payload["total_views"]),
            train_indices=tuple(int(i) for i in payload["train_indices"]),
            hidden_indices=tuple(int(i) for i in payload["hidden_indices"]),
            digest=str(payload.get("digest", "")),
            sealed=bool(payload.get("sealed", False)),
        )
        if split.sealed and split.digest:
            live = hashlib.sha256(
                canonical_json(
                    {
                        "target_id": split.target_id,
                        "seed": split.seed,
                        "total_views": split.total_views,
                        "train_indices": list(split.train_indices),
                        "hidden_indices": list(split.hidden_indices),
                    }
                )
            ).hexdigest()
            if live != split.digest:
                raise SecurityError(
                    f"split digest mismatch for {split.target_id}: sealed split was mutated"
                )
        return split


def make_split(
    target_id: str,
    *,
    total_views: int = 32,
    hidden_count: int = 8,
    seed: int = 20260727,
) -> SplitAuthority:
    """Canonical construction: evenly spaced hidden indices, remainder train.

    After ``seal()``, callers must load this object rather than re-deriving
    indices from total/hidden counts.
    """
    if hidden_count < 1 or hidden_count >= total_views:
        raise ValidationError("hidden_count must be in [1, total_views)")
    # Even spacing — deterministic from seed only as a salt on the phase.
    phase = seed % total_views
    hidden = sorted(
        {
            (phase + int(round(i * total_views / hidden_count))) % total_views
            for i in range(hidden_count)
        }
    )
    while len(hidden) < hidden_count:
        candidate = (hidden[-1] + 1) % total_views
        if candidate not in hidden:
            hidden.append(candidate)
        else:
            break
    hidden_t = tuple(sorted(hidden)[:hidden_count])
    train_t = tuple(i for i in range(total_views) if i not in set(hidden_t))
    return SplitAuthority(
        target_id=target_id,
        seed=seed,
        total_views=total_views,
        train_indices=train_t,
        hidden_indices=hidden_t,
    ).seal()


@dataclass(slots=True)
class SealedOcularManifest:
    target_id: str
    display_name: str
    evidence_status: EvidenceStatus
    acceptance_thresholds: dict[str, Any]
    blocked_requirements: list[dict[str, Any]] = field(default_factory=list)
    builder_inputs: list[str] = field(default_factory=list)
    hidden_evidence: list[str] = field(default_factory=list)
    claim_boundary: list[str] = field(default_factory=list)
    split: SplitAuthority | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "kind": "ocular.sealed-benchmark-manifest",
            "schema_version": SCHEMA_VERSION,
            "target_id": self.target_id,
            "display_name": self.display_name,
            "evidence_status": self.evidence_status.value,
            "acceptance_thresholds": dict(self.acceptance_thresholds),
            "blocked_requirements": list(self.blocked_requirements),
            "builder_inputs": list(self.builder_inputs),
            "hidden_evidence": list(self.hidden_evidence),
            "claim_boundary": list(self.claim_boundary),
            "split": None if self.split is None else self.split.to_dict(),
        }


@dataclass(slots=True)
class SealedOcularContract:
    target_id: str
    oracle_digest: str
    evaluator_digest: str
    builder_inputs_digest: str
    split_digest: str
    frozen_at: str
    acceptance_thresholds: dict[str, Any]
    blocked_requirements: list[dict[str, Any]] = field(default_factory=list)
    lineage: Lineage = field(default_factory=Lineage)
    digest: str = ""
    canary_marker: str = ""

    def payload(self) -> dict[str, Any]:
        return {
            "kind": SEALED_OCULAR_KIND,
            "schema_version": SCHEMA_VERSION,
            "target_id": self.target_id,
            "oracle_digest": self.oracle_digest,
            "evaluator_digest": self.evaluator_digest,
            "builder_inputs_digest": self.builder_inputs_digest,
            "split_digest": self.split_digest,
            "frozen_at": self.frozen_at,
            "acceptance_thresholds": self.acceptance_thresholds,
            "blocked_requirements": self.blocked_requirements,
            "lineage": self.lineage.to_dict(),
            # canary_marker is oracle-only secret; NOT part of builder-visible digest fields
            # but is hashed into the contract so evaluator can verify it was sealed.
            "canary_marker_digest": hashlib.sha256(
                self.canary_marker.encode("utf-8")
            ).hexdigest()
            if self.canary_marker
            else "",
        }

    def seal(self) -> SealedOcularContract:
        self.digest = hashlib.sha256(canonical_json(self.payload())).hexdigest()
        return self

    def verify(self) -> None:
        live = hashlib.sha256(canonical_json(self.payload())).hexdigest()
        if not self.digest or live != self.digest:
            raise SecurityError(f"contract digest mismatch for {self.target_id}")

    def to_dict(self) -> dict[str, Any]:
        value = self.payload()
        value["digest"] = self.digest
        # Never serialise the raw canary into builder-visible artifacts from this method
        # when used for builder handoff — callers that need it use oracle path.
        return value


def tree_digest(root: Path) -> str:
    supplied = root.expanduser().resolve()
    if not supplied.is_dir():
        raise ValidationError(f"not a directory: {root}")
    entries: list[dict[str, Any]] = []
    for path in sorted(supplied.rglob("*")):
        if path.is_symlink():
            raise SecurityError(f"symlink forbidden in sealed tree: {path}")
        if path.is_file():
            entries.append(
                {
                    "path": path.relative_to(supplied).as_posix(),
                    "sha256": sha256_file(path)[0],
                    "size": path.stat().st_size,
                }
            )
    return hashlib.sha256(canonical_json(entries)).hexdigest()


def file_list_digest(root: Path, relative_paths: list[str]) -> str:
    resolved = root.expanduser().resolve()
    entries: list[dict[str, Any]] = []
    for relative in sorted(set(relative_paths)):
        candidate = Path(relative)
        if candidate.is_absolute() or ".." in candidate.parts:
            raise SecurityError(f"builder-input path escaped its root: {relative}")
        path = confined_path(resolved, resolved / candidate)
        if not path.is_file():
            raise ValidationError(f"missing builder input: {relative}")
        entries.append(
            {
                "path": relative,
                "sha256": sha256_file(path)[0],
                "size": path.stat().st_size,
            }
        )
    return hashlib.sha256(canonical_json(entries)).hexdigest()


def default_manifests(seed: int = 20260727) -> dict[str, SealedOcularManifest]:
    """Eight sealed ocular targets with honest evidence status."""
    Spec = tuple[str, str, EvidenceStatus, dict[str, Any], list[dict[str, Any]], list[str]]
    specs: list[Spec] = [
        (
            "live_tabletop",
            "Live tabletop multi-object memory",
            EvidenceStatus.SYNTHETIC,
            {"min_track_precision": 0.7, "max_identity_swap_rate": 0.05},
            [],
            ["No live webcam on this host; synthetic tabletop sequence is diagnostic."],
        ),
        (
            "remote",
            "Consumer remote ocular loop",
            EvidenceStatus.SYNTHETIC,
            {"min_views": 24, "require_perception_identity": True},
            [
                {
                    "id": "user_remote_photos",
                    "reason": (
                        "User-supplied remote photos not present; "
                        "fixture is procedural only."
                    ),
                }
            ],
            ["Fixture is not the user's remote."],
        ),
        (
            "reflective_transparent",
            "Reflective / transparent object",
            EvidenceStatus.BLOCKED,
            {"specular_separation_required": True},
            [
                {
                    "id": "polarization_capture",
                    "reason": "No polarised multi-view capture on this host.",
                }
            ],
            ["Specular/transparent geometry remains unresolved without dedicated capture."],
        ),
        (
            "browser_page",
            "Browser page eyeball",
            EvidenceStatus.PARTIAL,
            {"min_contradiction_detectors": 7},
            [],
            ["Physical browser runs via with-one-browser.sh; demo detectors always run."],
        ),
        (
            "data_centre",
            "Data-centre film / rack scene",
            EvidenceStatus.PARTIAL,
            {"min_beats": 1},
            [],
            ["Cinematic scene; geometry claims capped by observed footage."],
        ),
        (
            "dynamic_room_memory",
            "Dynamic room memory",
            EvidenceStatus.SYNTHETIC,
            {"change_classes_required": 4},
            [],
            ["Cross-session identity must survive restart."],
        ),
        (
            "soft_object",
            "Soft deformable object",
            EvidenceStatus.SYNTHETIC,
            {"topology_stable": True},
            [],
            ["Soft deformation; metric volume is inferred."],
        ),
        (
            "organic_fur",
            "Organic / fur surface",
            EvidenceStatus.SYNTHETIC,
            {"groom_not_mesh_only": True},
            [
                {
                    "id": "real_animal_capture",
                    "reason": "Synthetic fur is not evidence about a real animal.",
                }
            ],
            ["Synthetic fur only."],
        ),
    ]
    manifests: dict[str, SealedOcularManifest] = {}
    for target_id, name, status, thresholds, blocked, claims in specs:
        split = make_split(target_id, total_views=32, hidden_count=8, seed=seed)
        manifests[target_id] = SealedOcularManifest(
            target_id=target_id,
            display_name=name,
            evidence_status=status,
            acceptance_thresholds=thresholds,
            blocked_requirements=blocked,
            builder_inputs=[
                "builder_inputs/requirements.json",
                "builder_inputs/split_train_only.json",
            ],
            hidden_evidence=[
                "oracle/hidden/split.json",
                "oracle/hidden/canary.txt",
                "oracle/hidden/holdout_views.json",
            ],
            claim_boundary=claims,
            split=split,
        )
    return manifests


def materialise_target(root: Path, manifest: SealedOcularManifest) -> SealedOcularContract:
    """Write oracle / builder_inputs / evaluator trees for one target and seal."""
    if manifest.split is None:
        raise ValidationError(f"manifest {manifest.target_id} lacks split")
    target_root = root / manifest.target_id
    oracle = target_root / "oracle"
    builder = target_root / "builder_inputs"
    evaluator = target_root / "evaluator"
    hidden = oracle / "hidden"
    for path in (hidden, builder, evaluator):
        path.mkdir(parents=True, exist_ok=True)

    canary = f"OCULAR-CANARY-{manifest.target_id}-{secrets.token_hex(16)}"
    canary_digest = hashlib.sha256(canary.encode("utf-8")).hexdigest()
    (hidden / "canary.txt").write_text(canary + "\n", encoding="utf-8")
    atomic_write_json(hidden / "split.json", manifest.split.to_dict())
    atomic_write_json(
        hidden / "holdout_views.json",
        {
            "target_id": manifest.target_id,
            "hidden_indices": list(manifest.split.hidden_indices),
            "note": "Evaluator only. Builder must not read this file.",
        },
    )
    # Canary seal metadata is part of the oracle tree *before* digest so the
    # sealed contract does not drift when this file is present at verify time.
    atomic_write_json(
        hidden / "canary_seal.json",
        {
            "canary_marker": canary,
            "canary_marker_digest": canary_digest,
        },
    )
    # Oracle source description (not builder-visible beyond declared inputs).
    atomic_write_json(
        oracle / "source.json",
        {
            "target_id": manifest.target_id,
            "display_name": manifest.display_name,
            "evidence_status": manifest.evidence_status.value,
            "claim_boundary": list(manifest.claim_boundary),
        },
    )

    # Builder sees requirements + train indices only (not hidden list).
    atomic_write_json(
        builder / "requirements.json",
        {
            "target_id": manifest.target_id,
            "acceptance_thresholds": manifest.acceptance_thresholds,
            "blocked_requirements": manifest.blocked_requirements,
            "claim_boundary": list(manifest.claim_boundary),
        },
    )
    atomic_write_json(
        builder / "split_train_only.json",
        {
            "target_id": manifest.target_id,
            "split_digest": manifest.split.digest,
            "train_indices": list(manifest.split.train_indices),
            "total_views": manifest.split.total_views,
            "note": (
                "Hidden indices are intentionally absent. Recomputing them from "
                "total_views is a contract violation — use this train list only."
            ),
        },
    )

    # Evaluator gates (frozen).
    atomic_write_json(
        evaluator / "gates.json",
        {
            "target_id": manifest.target_id,
            "acceptance_thresholds": manifest.acceptance_thresholds,
            "require_canary_absent_from_builder": True,
            "require_split_digest_match": True,
            "split_digest": manifest.split.digest,
        },
    )
    atomic_write_json(
        evaluator / "README.json",
        {
            "role": Role.EVALUATOR.value,
            "may_read": ["oracle/hidden", "builder outputs", "gates"],
            "must_not": ["mutate oracle after freeze"],
        },
    )

    contract = SealedOcularContract(
        target_id=manifest.target_id,
        oracle_digest=tree_digest(oracle),
        evaluator_digest=tree_digest(evaluator),
        builder_inputs_digest=file_list_digest(
            builder, ["requirements.json", "split_train_only.json"]
        ),
        split_digest=manifest.split.digest,
        frozen_at=utc_now(),
        acceptance_thresholds=dict(manifest.acceptance_thresholds),
        blocked_requirements=list(manifest.blocked_requirements),
        lineage=Lineage(
            tool="blender-vision-mcp",
            tool_version="0.1.0",
            operation="ocular.sealed.materialise",
            inputs=[manifest.target_id],
            parameters={"schema_version": SCHEMA_VERSION},
        ),
        canary_marker=canary,
    ).seal()
    atomic_write_json(target_root / "contract.json", contract.to_dict())
    atomic_write_json(target_root / "manifest.json", manifest.to_dict())
    return contract


def builder_read(target_root: Path, relative: str) -> Path:
    """Builder path confinement: only builder_inputs/ is readable."""
    root = target_root.expanduser().resolve()
    if relative.startswith("oracle") or relative.startswith("evaluator"):
        raise SecurityError(f"builder cannot read {relative}")
    if ".." in Path(relative).parts or Path(relative).is_absolute():
        raise SecurityError(f"path escape: {relative}")
    # Only builder_inputs tree.
    rel = relative.removeprefix("builder_inputs/").removeprefix("builder_inputs")
    if not rel or rel == relative and relative.startswith("oracle"):
        raise SecurityError(f"builder cannot read {relative}")
    return confined_path(root / "builder_inputs", (root / "builder_inputs" / rel))


def run_leakage_canaries(root: Path) -> list[dict[str, Any]]:
    """Inject/read probes that must be blocked; verify canary absent from builder."""
    matrix: list[dict[str, Any]] = []
    for target_id in TARGET_IDS:
        target_root = root / target_id
        if not target_root.is_dir():
            matrix.append(
                {
                    "probe": f"{target_id}:present",
                    "blocked": False,
                    "result": "FAIL",
                    "detail": "target tree missing",
                }
            )
            continue

        # Probe 1: builder reads oracle canary → must raise.
        blocked = False
        detail = ""
        try:
            builder_read(target_root, "oracle/hidden/canary.txt")
            detail = "builder was allowed to resolve oracle canary path"
        except SecurityError as exc:
            blocked = True
            detail = str(exc)
        matrix.append(
            {
                "probe": f"{target_id}:builder_oracle_canary",
                "blocked": blocked,
                "result": "PASS" if blocked else "FAIL",
                "detail": detail,
            }
        )

        # Probe 2: canary string must not appear in builder_inputs tree.
        canary_path = target_root / "oracle" / "hidden" / "canary.txt"
        canary = canary_path.read_text(encoding="utf-8").strip() if canary_path.is_file() else ""
        builder_blob = ""
        for path in (target_root / "builder_inputs").rglob("*"):
            if path.is_file():
                builder_blob += path.read_text(encoding="utf-8", errors="replace")
        present = bool(canary) and canary in builder_blob
        matrix.append(
            {
                "probe": f"{target_id}:canary_absent_from_builder",
                "blocked": not present,
                "result": "PASS" if (canary and not present) else "FAIL",
                "detail": (
                    "canary leaked into builder_inputs"
                    if present
                    else "canary absent as required"
                ),
            }
        )

        # Probe 3: recomputing hidden indices must not be used — train list is authority.
        split_train = json.loads(
            (target_root / "builder_inputs" / "split_train_only.json").read_text(encoding="utf-8")
        )
        split_path = target_root / "oracle" / "hidden" / "split.json"
        sealed_split = SplitAuthority.from_dict(
            json.loads(split_path.read_text(encoding="utf-8"))
        )
        train_match = list(split_train["train_indices"]) == list(sealed_split.train_indices)
        # Hidden must not be in builder payload.
        hidden_leaked = "hidden_indices" in split_train
        matrix.append(
            {
                "probe": f"{target_id}:single_split_authority",
                "blocked": train_match and not hidden_leaked,
                "result": "PASS" if (train_match and not hidden_leaked) else "FAIL",
                "detail": (
                    f"train_match={train_match} hidden_leaked={hidden_leaked} "
                    f"split_digest={sealed_split.digest[:12]}"
                ),
            }
        )

        # Probe 4: builder denied hidden index via SplitAuthority.
        blocked = False
        try:
            sealed_split.assert_builder_may_read(sealed_split.hidden_indices[0])
            detail = "hidden index was readable"
        except SecurityError as exc:
            blocked = True
            detail = str(exc)
        matrix.append(
            {
                "probe": f"{target_id}:hidden_index_denial",
                "blocked": blocked,
                "result": "PASS" if blocked else "FAIL",
                "detail": detail,
            }
        )

    return matrix


def materialise_all(root: Path, *, seed: int = 20260727) -> dict[str, SealedOcularContract]:
    root = root.expanduser().resolve()
    root.mkdir(parents=True, exist_ok=True)
    contracts: dict[str, SealedOcularContract] = {}
    for target_id, manifest in default_manifests(seed=seed).items():
        contracts[target_id] = materialise_target(root, manifest)
    return contracts


def run_sealed_ocular(
    output: Path,
    *,
    seed: int = 20260727,
) -> dict[str, Any]:
    """Materialise eight sealed benchmarks, run canaries, write receipt."""
    output = output.expanduser().resolve()
    output.mkdir(parents=True, exist_ok=True)
    bench_root = output / "benchmarks"
    contracts = materialise_all(bench_root, seed=seed)
    matrix = run_leakage_canaries(bench_root)

    # Verify contracts still match trees.
    verify_failures = 0
    for target_id, contract in contracts.items():
        try:
            contract.verify()
            oracle = bench_root / target_id / "oracle"
            evaluator = bench_root / target_id / "evaluator"
            if tree_digest(oracle) != contract.oracle_digest:
                raise SecurityError("oracle tree changed after seal")
            if tree_digest(evaluator) != contract.evaluator_digest:
                raise SecurityError("evaluator tree changed after seal")
        except (SecurityError, ValidationError) as exc:
            verify_failures += 1
            matrix.append(
                {
                    "probe": f"{target_id}:contract_verify",
                    "blocked": False,
                    "result": "FAIL",
                    "detail": str(exc),
                }
            )

    failures = sum(1 for row in matrix if row["result"] != "PASS") + verify_failures
    receipt = {
        "schema": "ocular.sealed-receipt/1",
        "completed_at": utc_now(),
        "targets": list(TARGET_IDS),
        "target_count": len(TARGET_IDS),
        "contracts": {
            tid: {
                "digest": c.digest,
                "split_digest": c.split_digest,
                "oracle_digest": c.oracle_digest,
                "evaluator_digest": c.evaluator_digest,
                "builder_inputs_digest": c.builder_inputs_digest,
            }
            for tid, c in contracts.items()
        },
        "leakage_matrix": matrix,
        "failures": failures,
        "status": "PASS" if failures == 0 else "FAIL",
        "laws": [
            "single_split_authority",
            "oracle_builder_evaluator_isolation",
            "canary_markers",
            "no_hidden_recompute",
        ],
        "execution_class": ExecutionClass.PHYSICAL.value,
        "authority": AuthorityClass.RUNTIME_OBSERVED.value,
    }
    atomic_write_json(output / "sealed.receipt.json", receipt)
    return receipt

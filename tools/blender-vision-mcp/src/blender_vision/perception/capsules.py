from __future__ import annotations

import hashlib
import json
import re
import tempfile
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import canonical_json, sha256_file, utc_now
from blender_vision.projects.store import ProjectStore

_AUTHORIZED_ASSET_RIGHTS = {
    "OWNED",
    "LICENSED",
    "SYNTHETIC_OWNED",
    "INTERNAL_AUTHORIZED",
    "CC0",
}
_FORBIDDEN_CAPSULE_ROLES = {
    "dom.html",
    "design.source",
    "screenshot.viewport",
    "screenshot.full",
}
_FRAMEWORKS = {"react", "vanilla", "threejs", "react-three-fiber", "storybook", "blender-previs"}


class ExperienceIRCompiler:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def compile(self, capture_ids: list[str]) -> dict[str, Any]:
        capture_ids = sorted(set(capture_ids))
        if not capture_ids:
            raise ValueError("ExperienceIR requires at least one capture")
        captures = [self._capture(capture_id) for capture_id in capture_ids]
        graph_records = [
            record
            for capture_id in capture_ids
            for record in self._graphs(capture_id)
        ]
        if not graph_records:
            raise ValueError("ExperienceIR requires at least one perceptual graph")
        created_at = utc_now()
        payload = {
            "schema": "vision.experience-ir/v1",
            "authority": "DERIVED",
            "capture_ids": capture_ids,
            "targets": [capture["request"]["target"] for capture in captures],
            "source_governance": [
                {
                    "capture_id": capture["capture_id"],
                    "rights_decision": capture["request"]["rights_decision"],
                    "source_id": capture["request"].get("source_id"),
                }
                for capture in captures
            ],
            "graph_citations": [
                {
                    "capture_id": record["capture_id"],
                    "graph_type": record["graph_type"],
                    "artifact_digest": record["artifact_digest"],
                    "authority": record["authority"],
                }
                for record in graph_records
            ],
            "layout_constraints": self._layout(graph_records),
            "states": self._states(graph_records),
            "responsive_rules": self._responsive(graph_records),
            "interactions": self._interactions(graph_records),
            "motion": self._motion(graph_records),
            "graphics": self._graphics(graph_records),
            "design_system": self._design(graph_records),
            "accessibility": self._accessibility(captures),
            "uncertainty": self._uncertainty(graph_records),
            "limitations": sorted(
                {
                    limitation
                    for capture in captures
                    for limitation in capture["limitations"]
                }
            ),
            "created_at": created_at,
        }
        identity = {
            key: value for key, value in payload.items() if key != "created_at"
        }
        payload["id"] = hashlib.sha256(canonical_json(identity)).hexdigest()
        record = self._ingest_json(payload, "experience-ir")
        with self.project.connection() as connection:
            connection.execute(
                "INSERT OR IGNORE INTO experience_ir_records("
                "id,capture_ids_json,artifact_digest,authority,created_at) VALUES(?,?,?,?,?)",
                (
                    payload["id"],
                    canonical_json(capture_ids).decode(),
                    record["digest"],
                    "DERIVED",
                    created_at,
                ),
            )
        return {**payload, "artifact_digest": record["digest"]}

    def get(self, experience_ir_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT artifact_digest FROM experience_ir_records WHERE id=?",
                (experience_ir_id,),
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown ExperienceIR: {experience_ir_id}")
        return json.loads(
            self.artifacts.path_for(row["artifact_digest"]).read_text(encoding="utf-8")
        )

    def _capture(self, capture_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT normalized_request_json,limitations_json,status,manifest_digest "
                "FROM observation_captures WHERE id=?",
                (capture_id,),
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown capture: {capture_id}")
        if row["status"] != "COMPLETE" or not row["manifest_digest"]:
            raise ValueError(f"capture is not complete: {capture_id}")
        path = self.artifacts.path_for(row["manifest_digest"])
        actual, _ = sha256_file(path)
        if actual != row["manifest_digest"]:
            raise ValueError(f"capture manifest failed integrity: {capture_id}")
        return {
            "capture_id": capture_id,
            "request": json.loads(row["normalized_request_json"]),
            "limitations": json.loads(row["limitations_json"]),
            "manifest_digest": row["manifest_digest"],
        }

    def _graphs(self, capture_id: str) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT graph_type,artifact_digest,authority FROM perceptual_graphs "
                "WHERE capture_id=? ORDER BY graph_type",
                (capture_id,),
            ).fetchall()
        records = []
        for row in rows:
            path = self.artifacts.path_for(row["artifact_digest"])
            actual, _ = sha256_file(path)
            if actual != row["artifact_digest"]:
                raise ValueError(
                    f"perceptual graph failed integrity: {capture_id}/{row['graph_type']}"
                )
            records.append(
                {
                    "capture_id": capture_id,
                    "graph_type": row["graph_type"],
                    "artifact_digest": row["artifact_digest"],
                    "authority": row["authority"],
                    "graph": json.loads(path.read_text(encoding="utf-8")),
                }
            )
        return records

    @staticmethod
    def _layout(records: list[dict[str, Any]]) -> list[dict[str, Any]]:
        return [
            {
                "id": node["id"],
                "selector": node.get("selector"),
                "role": node.get("role"),
                "bounds": node.get("bounds"),
                "styles": node.get("styles"),
                "source_binding": node.get("sourceBinding"),
                "evidence": record["artifact_digest"],
            }
            for record in records
            if record["graph_type"] == "LayoutGraph"
            for node in record["graph"].get("nodes", [])
        ]

    @staticmethod
    def _states(records: list[dict[str, Any]]) -> dict[str, Any]:
        graphs = [
            record for record in records if record["graph_type"] == "StateGraph"
        ]
        return {
            "nodes": [
                {
                    "id": node["id"],
                    "visible_elements": [
                        {
                            "selector": element["selector"],
                            "role": element.get("role"),
                            "attributes": element.get("attributes", {}),
                        }
                        for element in node.get("visible_elements", [])
                    ],
                    "evidence_references": node["evidence_references"],
                }
                for record in graphs
                for node in record["graph"].get("nodes", [])
            ],
            "transitions": [
                edge
                for record in graphs
                for edge in record["graph"].get("edges", [])
            ],
        }

    @staticmethod
    def _responsive(records: list[dict[str, Any]]) -> dict[str, Any]:
        graphs = [
            record for record in records if record["graph_type"] == "ResponsiveGraph"
        ]
        return {
            "viewports": [
                {
                    "id": node["id"],
                    "viewport": node["viewport"],
                    "elements": [
                        element["selector"] for element in node.get("elements", [])
                    ],
                    "evidence_references": node["evidence_references"],
                }
                for record in graphs
                for node in record["graph"].get("nodes", [])
            ],
            "transitions": [
                edge
                for record in graphs
                for edge in record["graph"].get("edges", [])
            ],
            "input_modes": [
                record["graph"].get("input_mode_variants", {})
                for record in graphs
            ],
        }

    @staticmethod
    def _interactions(records: list[dict[str, Any]]) -> list[dict[str, Any]]:
        return [
            edge
            for record in records
            if record["graph_type"] == "InteractionGraph"
            for edge in record["graph"].get("edges", [])
            if edge.get("status") == "OBSERVED"
        ]

    @staticmethod
    def _motion(records: list[dict[str, Any]]) -> dict[str, Any]:
        graphs = [
            record for record in records if record["graph_type"] == "MotionGraph"
        ]
        return {
            "tracks": [
                {
                    "id": node["id"],
                    "selector": node["selector"],
                    "animation": node.get("animation"),
                    "animation_samples": node.get("animation_samples", []),
                    "scroll_samples": node.get("scroll_samples", []),
                    "evidence_references": node["evidence_references"],
                }
                for record in graphs
                for node in record["graph"].get("nodes", [])
            ],
            "inference": [
                record["graph"].get("inference", {}) for record in graphs
            ],
            "reduced_motion_variants": [
                record["graph"].get("reduced_motion_variant") for record in graphs
            ],
            "replay_contracts": [
                record["graph"].get("replay_contract", {}) for record in graphs
            ],
        }

    @staticmethod
    def _graphics(records: list[dict[str, Any]]) -> dict[str, Any]:
        graphs = [
            record
            for record in records
            if record["graph_type"] == "GraphicsFrameGraph"
        ]
        return {
            "surfaces": [
                surface
                for record in graphs
                for surface in record["graph"].get("surface_classification", [])
            ],
            "runtime_nodes": [
                node
                for record in graphs
                for node in record["graph"].get("nodes", [])
                if node.get("domain_type") in {"RuntimeCamera", "RuntimeSceneObject"}
            ],
            "frames": [
                frame
                for record in graphs
                for frame in record["graph"].get("frames", [])
            ],
            "materialized_gltf": [
                record["graph"].get("materialized_gltf")
                for record in graphs
                if record["graph"].get("materialized_gltf")
            ],
        }

    @staticmethod
    def _design(records: list[dict[str, Any]]) -> dict[str, Any]:
        graphs = [
            record
            for record in records
            if record["graph_type"] == "DesignSystemGraph"
        ]
        return {
            "components": [
                {
                    "id": node["id"],
                    "name": node.get("name"),
                    "semantic_name": node.get("semantic_name"),
                    "domain_type": node["domain_type"],
                    "source_binding": node.get("source_binding"),
                }
                for record in graphs
                for node in record["graph"].get("nodes", [])
                if "Component" in node["domain_type"]
                or node["domain_type"] in {"COMPONENT", "COMPONENT_SET", "INSTANCE"}
            ],
            "tokens": [
                token
                for record in graphs
                for token in record["graph"].get("tokens", [])
            ],
        }

    def _accessibility(self, captures: list[dict[str, Any]]) -> list[dict[str, Any]]:
        result = []
        with self.project.connection() as connection:
            for capture in captures:
                row = connection.execute(
                    "SELECT artifact_digest FROM observation_capture_artifacts "
                    "WHERE capture_id=? AND role='accessibility.tree'",
                    (capture["capture_id"],),
                ).fetchone()
                if row:
                    tree = json.loads(
                        self.artifacts.path_for(row["artifact_digest"]).read_text(
                            encoding="utf-8"
                        )
                    )
                    result.append(
                        {
                            "capture_id": capture["capture_id"],
                            "artifact_digest": row["artifact_digest"],
                            "node_count": len(tree.get("nodes", [])),
                            "authority": "OBSERVED",
                        }
                    )
        return result

    @staticmethod
    def _uncertainty(records: list[dict[str, Any]]) -> list[dict[str, Any]]:
        return [
            {
                "graph_type": record["graph_type"],
                "node_id": node["id"],
                "uncertainty": node["uncertainty"],
            }
            for record in records
            for node in record["graph"].get("nodes", [])
            if node.get("uncertainty")
        ]

    def _ingest_json(self, value: dict[str, Any], prefix: str) -> dict[str, Any]:
        staging = self.project.root / "observations" / ".staging"
        staging.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(
            prefix=f"{prefix}-", suffix=".json", dir=staging
        ) as file:
            file.write(canonical_json(value))
            file.flush()
            return self.artifacts.ingest_file(
                Path(file.name), media_type="application/json"
            ).to_dict()


class FeatureCapsuleCompiler:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.ir = ExperienceIRCompiler(project)

    def compile(
        self,
        experience_ir_id: str,
        *,
        semantic_purpose: str,
        kind: str,
        framework: str,
        owned_asset_mappings: list[dict[str, Any]] | None = None,
        implementation_interface: dict[str, Any] | None = None,
        performance_budget: dict[str, Any] | None = None,
        verification_thresholds: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        semantic_purpose = semantic_purpose.strip()
        if not semantic_purpose:
            raise ValueError("Feature Capsule requires a semantic purpose")
        if framework not in _FRAMEWORKS:
            raise ValueError(f"unsupported capsule framework: {framework}")
        experience = self.ir.get(experience_ir_id)
        assets = self._validate_asset_mappings(owned_asset_mappings or [])
        tests = self._tests(experience, performance_budget or {}, verification_thresholds or {})
        emitted = self._emit(framework, kind, semantic_purpose)
        created_at = utc_now()
        capsule = {
            "schema": "vision.feature-capsule/v1",
            "authority": "DERIVED",
            "semantic_purpose": semantic_purpose,
            "kind": kind,
            "framework": framework,
            "experience_ir_id": experience_ir_id,
            "target_evidence_digests": [
                citation["artifact_digest"]
                for citation in experience["graph_citations"]
            ],
            "clean_room_behavior": {
                "states": experience["states"],
                "motion": experience["motion"],
                "layout_constraints": experience["layout_constraints"],
                "responsive_rules": experience["responsive_rules"],
                "interaction_triggers": experience["interactions"],
                "graphics_behavior": experience["graphics"],
            },
            "design_token_mapping": experience["design_system"]["tokens"],
            "component_mapping": experience["design_system"]["components"],
            "accessibility_behavior": experience["accessibility"],
            "performance_budget": performance_budget or {},
            "owned_asset_mappings": assets,
            "implementation_interface": implementation_interface or {},
            "framework_output": emitted,
            "test_fixture": tests,
            "verification_thresholds": verification_thresholds or {},
            "known_limitations": experience["limitations"],
            "provenance": {
                "experience_ir_artifact": self._ir_digest(experience_ir_id),
                "source_governance": experience["source_governance"],
                "clean_room_policy": "no-reference-source-or-protected-asset-payload",
            },
            "rights_restrictions": [
                governance["rights_decision"]
                for governance in experience["source_governance"]
            ],
            "created_at": created_at,
        }
        identity = {
            key: value for key, value in capsule.items() if key != "created_at"
        }
        capsule["id"] = hashlib.sha256(canonical_json(identity)).hexdigest()
        self._assert_clean(capsule)
        manifest = self._ingest_json(capsule, "feature-capsule")
        test_record = self._ingest_json(tests, "capsule-tests")
        with self.project.connection() as connection:
            connection.execute(
                "INSERT OR IGNORE INTO feature_capsules("
                "id,experience_ir_id,kind,framework,status,manifest_digest,test_digest,"
                "created_at) VALUES(?,?,?,?,?,?,?,?)",
                (
                    capsule["id"],
                    experience_ir_id,
                    kind,
                    framework,
                    "COMPILED",
                    manifest["digest"],
                    test_record["digest"],
                    created_at,
                ),
            )
        return {
            **capsule,
            "manifest_digest": manifest["digest"],
            "test_digest": test_record["digest"],
        }

    def get(self, capsule_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT manifest_digest FROM feature_capsules WHERE id=?",
                (capsule_id,),
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown Feature Capsule: {capsule_id}")
        return json.loads(
            self.artifacts.path_for(row["manifest_digest"]).read_text(encoding="utf-8")
        )

    @staticmethod
    def _validate_asset_mappings(
        mappings: list[dict[str, Any]],
    ) -> list[dict[str, Any]]:
        validated = []
        for mapping in mappings:
            rights = str(mapping.get("rights_state", ""))
            if rights not in _AUTHORIZED_ASSET_RIGHTS:
                raise PermissionError(
                    f"capsule asset is not owned or licensed: {mapping.get('semantic_role')}"
                )
            replacement_digest = str(mapping.get("replacement_digest", ""))
            if not re.fullmatch(r"[a-f0-9]{64}", replacement_digest):
                raise ValueError("asset mapping requires an exact replacement_digest")
            validated.append(
                {
                    "semantic_role": str(mapping["semantic_role"]),
                    "replacement_digest": replacement_digest,
                    "rights_state": rights,
                    "license_reference": mapping.get("license_reference"),
                }
            )
        return validated

    @staticmethod
    def _tests(
        experience: dict[str, Any],
        performance_budget: dict[str, Any],
        thresholds: dict[str, Any],
    ) -> dict[str, Any]:
        return {
            "schema": "vision.feature-capsule-tests/v1",
            "state_ids": [node["id"] for node in experience["states"]["nodes"]],
            "transition_ids": [
                edge["id"] for edge in experience["states"]["transitions"]
            ],
            "responsive_viewports": [
                node["viewport"] for node in experience["responsive_rules"]["viewports"]
            ],
            "motion_track_ids": [
                track["id"] for track in experience["motion"]["tracks"]
            ],
            "reduced_motion_required": any(
                variant is not None
                for variant in experience["motion"]["reduced_motion_variants"]
            ),
            "interaction_ids": [
                edge["id"] for edge in experience["interactions"]
            ],
            "accessibility_artifacts": [
                item["artifact_digest"] for item in experience["accessibility"]
            ],
            "performance_budget": performance_budget,
            "verification_thresholds": thresholds,
            "global_non_regression_required": True,
        }

    @staticmethod
    def _emit(framework: str, kind: str, purpose: str) -> dict[str, Any]:
        if framework == "react":
            content = (
                "export interface FeatureCapsuleProps { stateId: string; "
                "renderState: (stateId: string) => React.ReactNode; }\n"
                "export function FeatureCapsule(props: FeatureCapsuleProps) {\n"
                "  return <>{props.renderState(props.stateId)}</>;\n"
                "}\n"
            )
            path = "FeatureCapsule.tsx"
        elif framework == "vanilla":
            content = (
                "export function mountFeatureCapsule(root, model, renderState) {\n"
                "  if (!root || !model) throw new Error('capsule contract required');\n"
                "  return renderState(root, model.initialState);\n"
                "}\n"
            )
            path = "feature-capsule.js"
        else:
            content = canonical_json(
                {
                    "adapter": framework,
                    "kind": kind,
                    "semantic_purpose": purpose,
                    "contract": "consume-feature-capsule-manifest",
                }
            ).decode()
            path = f"{framework}-adapter.json"
        return {
            "files": [
                {
                    "path": path,
                    "content": content,
                    "sha256": hashlib.sha256(content.encode()).hexdigest(),
                }
            ],
            "contains_reference_source": False,
            "contains_reference_assets": False,
        }

    @staticmethod
    def _assert_clean(capsule: dict[str, Any]) -> None:
        serialized = canonical_json(capsule)
        for role in _FORBIDDEN_CAPSULE_ROLES:
            if f'"role":"{role}"'.encode() in serialized:
                raise PermissionError(f"capsule includes forbidden raw evidence role: {role}")
        for file in capsule["framework_output"]["files"]:
            if "<!doctype" in file["content"].lower() or "__next_data__" in file["content"].lower():
                raise PermissionError("capsule output appears to contain copied page source")

    def _ir_digest(self, experience_ir_id: str) -> str:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT artifact_digest FROM experience_ir_records WHERE id=?",
                (experience_ir_id,),
            ).fetchone()
        if row is None:
            raise KeyError(experience_ir_id)
        return str(row["artifact_digest"])

    def _ingest_json(self, value: dict[str, Any], prefix: str) -> dict[str, Any]:
        staging = self.project.root / "observations" / ".staging"
        staging.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(
            prefix=f"{prefix}-", suffix=".json", dir=staging
        ) as file:
            file.write(canonical_json(value))
            file.flush()
            return self.artifacts.ingest_file(
                Path(file.name), media_type="application/json"
            ).to_dict()


class FeatureCapsuleVerifier:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.compiler = FeatureCapsuleCompiler(project)

    def verify(self, capsule_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT manifest_digest,test_digest FROM feature_capsules WHERE id=?",
                (capsule_id,),
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown Feature Capsule: {capsule_id}")
        failures = []
        for role, digest in (
            ("manifest", row["manifest_digest"]),
            ("tests", row["test_digest"]),
        ):
            path = self.artifacts.path_for(digest)
            if not path.is_file():
                failures.append({"role": role, "reason": "missing"})
                continue
            actual, _ = sha256_file(path)
            if actual != digest:
                failures.append(
                    {"role": role, "reason": "digest_mismatch", "actual": actual}
                )
        capsule: dict[str, Any] | None = None
        tests: dict[str, Any] | None = None
        if not any(item["role"] == "manifest" for item in failures):
            try:
                capsule = self.compiler.get(capsule_id)
            except (OSError, json.JSONDecodeError) as error:
                failures.append(
                    {"role": "manifest", "reason": f"invalid_json:{type(error).__name__}"}
                )
        if not any(item["role"] == "tests" for item in failures):
            try:
                tests = json.loads(
                    self.artifacts.path_for(row["test_digest"]).read_text(encoding="utf-8")
                )
            except (OSError, json.JSONDecodeError) as error:
                failures.append(
                    {"role": "tests", "reason": f"invalid_json:{type(error).__name__}"}
                )
        if capsule is not None:
            try:
                self.compiler._assert_clean(capsule)
            except PermissionError as error:
                failures.append({"role": "clean_room", "reason": str(error)})
        empty_behavior = {
            "states": {"nodes": []},
            "interaction_triggers": [],
            "responsive_rules": {"viewports": []},
            "motion": {"tracks": []},
        }
        behavior = (
            capsule.get("clean_room_behavior", empty_behavior)
            if capsule is not None
            else empty_behavior
        )
        tests = tests or {
            "state_ids": [],
            "interaction_ids": [],
            "responsive_viewports": [],
            "motion_track_ids": [],
            "reduced_motion_required": False,
            "global_non_regression_required": False,
        }
        coverage = {
            "states": bool(tests["state_ids"])
            or not behavior["states"]["nodes"],
            "interactions": bool(tests["interaction_ids"])
            or not behavior["interaction_triggers"],
            "responsive": bool(tests["responsive_viewports"])
            or not behavior["responsive_rules"]["viewports"],
            "motion": bool(tests["motion_track_ids"])
            or not behavior["motion"]["tracks"],
            "reduced_motion": (
                tests["reduced_motion_required"]
                if behavior["motion"]["tracks"]
                else True
            ),
            "global_non_regression": tests["global_non_regression_required"] is True,
        }
        for gate, passed in coverage.items():
            if not passed:
                failures.append({"role": "coverage", "reason": f"{gate}_missing"})
        created_at = utc_now()
        report = {
            "schema": "vision.feature-capsule-evaluation/v1",
            "capsule_id": capsule_id,
            "authority": "VERIFIED",
            "status": "PASS" if not failures else "FAIL",
            "integrity": not any(item["role"] in {"manifest", "tests"} for item in failures),
            "clean_room": not any(item["role"] == "clean_room" for item in failures),
            "coverage": coverage,
            "failures": failures,
            "manifest_digest": row["manifest_digest"],
            "test_digest": row["test_digest"],
            "created_at": created_at,
        }
        report["id"] = hashlib.sha256(
            canonical_json({key: value for key, value in report.items() if key != "created_at"})
        ).hexdigest()
        report_record = self.compiler._ingest_json(report, "capsule-evaluation")
        with self.project.connection() as connection:
            connection.execute(
                "INSERT OR REPLACE INTO capsule_evaluations("
                "id,capsule_id,status,report_digest,created_at) VALUES(?,?,?,?,?)",
                (
                    report["id"],
                    capsule_id,
                    report["status"],
                    report_record["digest"],
                    created_at,
                ),
            )
            connection.execute(
                "UPDATE feature_capsules SET status=? WHERE id=?",
                (
                    "VERIFIED" if report["status"] == "PASS" else "REJECTED",
                    capsule_id,
                ),
            )
        return {**report, "report_digest": report_record["digest"]}

from __future__ import annotations

import json
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.models import SceneLifecycleState
from blender_vision.core.util import atomic_write_json, sha256_file, utc_now
from blender_vision.projects.store import ProjectStore
from blender_vision.security.paths import confined_path, safe_filename


class SceneStore:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def import_blend(self, source: Path) -> dict[str, Any]:
        source = source.expanduser().resolve()
        if source.suffix.lower() != ".blend":
            raise ValueError("authoritative Blender scene must use the .blend format")
        artifact = self.artifacts.ingest_file(source, media_type="application/x-blender")
        scene_id = str(uuid.uuid4())
        relative = Path("scene") / f"{scene_id}_{safe_filename(source.name)}"
        self.artifacts.materialize(artifact.digest, self.project.root / relative)
        with self.project.connection() as connection:
            authoritative = (
                connection.execute(
                    "SELECT 1 FROM scene_assets WHERE is_authoritative=1 LIMIT 1"
                ).fetchone()
                is None
            )
            connection.execute(
                "INSERT INTO scene_assets"
                "(id,artifact_digest,original_name,relative_path,state,is_authoritative,"
                "created_at) "
                "VALUES(?,?,?,?,?,?,?)",
                (
                    scene_id,
                    artifact.digest,
                    source.name,
                    str(relative),
                    SceneLifecycleState.DRAFT.value,
                    int(authoritative),
                    utc_now(),
                ),
            )
        return {
            "id": scene_id,
            "artifact": artifact.to_dict(),
            "original_name": source.name,
            "relative_path": str(relative),
            "state": SceneLifecycleState.DRAFT.value,
            "is_authoritative": authoritative,
        }

    def get(self, scene_id: str | None = None) -> dict[str, Any]:
        with self.project.connection() as connection:
            if scene_id:
                row = connection.execute(
                    "SELECT * FROM scene_assets WHERE id=?", (scene_id,)
                ).fetchone()
            else:
                row = connection.execute(
                    "SELECT * FROM scene_assets WHERE is_authoritative=1 LIMIT 1"
                ).fetchone()
        if row is None:
            raise FileNotFoundError("project has no imported Blender scene")
        value = dict(row)
        value["inventory"] = (
            json.loads(value.pop("inventory_json")) if value["inventory_json"] else None
        )
        value["absolute_path"] = str(self.project.root / value["relative_path"])
        return value

    def set_inventory(self, scene_id: str, inventory: dict[str, Any]) -> None:
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE scene_assets SET inventory_json=? WHERE id=?",
                (json.dumps(inventory), scene_id),
            )

    def register_generated(self, source: Path, *, original_name: str) -> dict[str, Any]:
        source = confined_path(self.project.root, source, must_exist=True)
        artifact = self.artifacts.ingest_file(source, media_type="application/x-blender")
        scene_id = str(uuid.uuid4())
        with self.project.connection() as connection:
            initial_scene = connection.execute(
                "SELECT 1 FROM scene_assets LIMIT 1"
            ).fetchone() is None
            state = (
                SceneLifecycleState.DRAFT
                if initial_scene
                else SceneLifecycleState.CANDIDATE
            )
            connection.execute(
                "INSERT INTO scene_assets"
                "(id,artifact_digest,original_name,relative_path,state,is_authoritative,"
                "created_at) "
                "VALUES(?,?,?,?,?,?,?)",
                (
                    scene_id,
                    artifact.digest,
                    original_name,
                    str(source.relative_to(self.project.root)),
                    state.value,
                    int(initial_scene),
                    utc_now(),
                ),
            )
        return {
            "id": scene_id,
            "artifact": artifact.to_dict(),
            "original_name": original_name,
            "relative_path": str(source.relative_to(self.project.root)),
            "absolute_path": str(source),
            "state": state.value,
            "is_authoritative": initial_scene,
        }

    def transition(
        self,
        scene_id: str,
        target: SceneLifecycleState | str,
        *,
        reviewer: str,
        reason: str,
        evaluation_id: str | None = None,
    ) -> dict[str, Any]:
        if not reviewer.strip() or not reason.strip():
            raise ValueError("scene transitions require a named reviewer or policy and reason")
        destination = SceneLifecycleState(target)
        scene = self.get(scene_id)
        source = SceneLifecycleState(scene["state"])
        allowed = {
            SceneLifecycleState.DRAFT: {SceneLifecycleState.CANDIDATE},
            SceneLifecycleState.CANDIDATE: {
                SceneLifecycleState.REJECTED,
                SceneLifecycleState.ACCEPTED,
            },
            SceneLifecycleState.ACCEPTED: {SceneLifecycleState.PROMOTED},
            SceneLifecycleState.PROMOTED: set(),
            SceneLifecycleState.REJECTED: set(),
            SceneLifecycleState.SUPERSEDED: set(),
        }
        if destination not in allowed[source]:
            raise ValueError(f"invalid scene transition: {source.value} -> {destination.value}")
        if destination in {
            SceneLifecycleState.ACCEPTED,
            SceneLifecycleState.PROMOTED,
        }:
            if not evaluation_id:
                raise ValueError(
                    f"candidate {destination.value.lower()} requires a passed "
                    "transactional evaluation"
                )
            with self.project.connection() as connection:
                evaluation = connection.execute(
                    "SELECT * FROM candidate_evaluations WHERE id=? AND scene_id=?",
                    (evaluation_id, scene_id),
                ).fetchone()
            if not self._verified_passed_evaluation(evaluation, scene):
                raise ValueError(
                    f"candidate {destination.value.lower()} requires its own verified "
                    "passed evaluation receipt"
                )

        previous: list[dict[str, Any]] = []
        if destination == SceneLifecycleState.PROMOTED:
            with self.project.connection() as connection:
                previous = [
                    dict(row)
                    for row in connection.execute(
                        "SELECT id,state,artifact_digest FROM scene_assets "
                        "WHERE is_authoritative=1 AND id<>? ORDER BY created_at,id",
                        (scene_id,),
                    )
                ]
            invalid_previous = [
                item["id"]
                for item in previous
                if item["state"] not in {"DRAFT", "ACCEPTED", "PROMOTED"}
            ]
            if invalid_previous:
                raise ValueError(
                    "cannot promote while an authoritative scene has an invalid "
                    f"supersession state: {', '.join(invalid_previous)}"
                )

        result = self._create_transition_receipt(
            scene=scene,
            source=source,
            destination=destination,
            reviewer=reviewer,
            reason=reason,
            evaluation_id=evaluation_id,
        )
        superseded_results = [
            self._create_transition_receipt(
                scene=item,
                source=SceneLifecycleState(item["state"]),
                destination=SceneLifecycleState.SUPERSEDED,
                reviewer=reviewer,
                reason=f"Superseded by promoted scene {scene_id}: {reason.strip()}",
                evaluation_id=evaluation_id,
                superseded_by_scene_id=scene_id,
            )
            for item in previous
        ]

        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current = connection.execute(
                "SELECT state FROM scene_assets WHERE id=?", (scene_id,)
            ).fetchone()
            if current is None or current["state"] != source.value:
                raise RuntimeError("scene lifecycle state changed during transition preparation")
            if destination in {
                SceneLifecycleState.ACCEPTED,
                SceneLifecycleState.PROMOTED,
            }:
                evaluation = connection.execute(
                    "SELECT * FROM candidate_evaluations WHERE id=? AND scene_id=?",
                    (evaluation_id, scene_id),
                ).fetchone()
                if not self._verified_passed_evaluation(evaluation, scene):
                    raise RuntimeError(
                        "candidate evaluation changed during transition preparation"
                    )
            if destination == SceneLifecycleState.PROMOTED:
                current_previous = [
                    dict(row)
                    for row in connection.execute(
                        "SELECT id,state,artifact_digest FROM scene_assets "
                        "WHERE is_authoritative=1 AND id<>? ORDER BY created_at,id",
                        (scene_id,),
                    )
                ]
                if current_previous != previous:
                    raise RuntimeError(
                        "authoritative scene set changed during promotion preparation"
                    )
                for item, superseded in zip(previous, superseded_results, strict=True):
                    connection.execute(
                        "UPDATE scene_assets SET state='SUPERSEDED',is_authoritative=0 "
                        "WHERE id=?",
                        (item["id"],),
                    )
                    self._insert_transition(connection, superseded)
                connection.execute("UPDATE scene_assets SET is_authoritative=0")
                connection.execute(
                    "UPDATE scene_assets SET state=?,is_authoritative=1 WHERE id=?",
                    (destination.value, scene_id),
                )
            else:
                connection.execute(
                    "UPDATE scene_assets SET state=? WHERE id=?", (destination.value, scene_id)
                )
            self._insert_transition(connection, result)
        return {
            **result["receipt_payload"],
            "receipt": result["artifact"].to_dict(),
            "path": result["path"],
            "superseded_transitions": [
                {
                    **item["receipt_payload"],
                    "receipt": item["artifact"].to_dict(),
                    "path": item["path"],
                }
                for item in superseded_results
            ],
        }

    def _verified_passed_evaluation(self, row: Any, scene: dict[str, Any]) -> bool:
        if row is None or row["status"] != "PASSED":
            return False
        digest = row["receipt_digest"]
        try:
            path = self.artifacts.path_for(digest)
            actual_digest, _size = sha256_file(path)
            payload = json.loads(path.read_text(encoding="utf-8"))
        except (FileNotFoundError, json.JSONDecodeError, OSError, TypeError, ValueError):
            return False
        return bool(
            actual_digest == digest
            and payload.get("receipt_type") == "candidate_evaluation_transaction"
            and payload.get("id") == row["id"]
            and payload.get("scene_id") == scene["id"]
            and payload.get("scene_artifact_digest") == scene["artifact_digest"]
            and payload.get("status") == "PASSED"
        )

    def _create_transition_receipt(
        self,
        *,
        scene: dict[str, Any],
        source: SceneLifecycleState,
        destination: SceneLifecycleState,
        reviewer: str,
        reason: str,
        evaluation_id: str | None,
        superseded_by_scene_id: str | None = None,
    ) -> dict[str, Any]:
        transition_id = str(uuid.uuid4())
        created_at = utc_now()
        receipt = {
            "schema_version": 1,
            "receipt_type": "scene_lifecycle_transition",
            "id": transition_id,
            "scene_id": scene["id"],
            "scene_artifact_digest": scene["artifact_digest"],
            "from_state": source.value,
            "to_state": destination.value,
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "evaluation_id": evaluation_id,
            "created_at": created_at,
        }
        if superseded_by_scene_id:
            receipt["superseded_by_scene_id"] = superseded_by_scene_id
        receipt_path = self.project.root / "receipts" / f"scene-transition-{transition_id}.json"
        atomic_write_json(receipt_path, receipt)
        artifact = self.artifacts.ingest_file(
            receipt_path, media_type="application/vnd.bvmcp.scene-transition+json"
        )
        return {
            "receipt_payload": receipt,
            "artifact": artifact,
            "path": str(receipt_path.relative_to(self.project.root)),
        }

    @staticmethod
    def _insert_transition(connection: Any, result: dict[str, Any]) -> None:
        receipt = result["receipt_payload"]
        connection.execute(
            "INSERT INTO scene_transitions(id,scene_id,from_state,to_state,reviewer,reason,"
            "evaluation_id,receipt_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
            (
                receipt["id"],
                receipt["scene_id"],
                receipt["from_state"],
                receipt["to_state"],
                receipt["reviewer"],
                receipt["reason"],
                receipt["evaluation_id"],
                result["artifact"].digest,
                receipt["created_at"],
            ),
        )

    def transitions(self, scene_id: str | None = None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            if scene_id:
                rows = connection.execute(
                    "SELECT * FROM scene_transitions WHERE scene_id=? ORDER BY created_at,id",
                    (scene_id,),
                ).fetchall()
            else:
                rows = connection.execute(
                    "SELECT * FROM scene_transitions ORDER BY created_at,id"
                ).fetchall()
        return [dict(row) for row in rows]

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute("SELECT * FROM scene_assets ORDER BY created_at").fetchall()
        return [dict(row) for row in rows]

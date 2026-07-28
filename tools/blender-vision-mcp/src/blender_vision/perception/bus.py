from __future__ import annotations

import hashlib
import json
import re
import tempfile
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import canonical_json, sha256_file, utc_now
from blender_vision.perception.contracts import CaptureOutcome, SensorAdapter
from blender_vision.projects.store import ProjectStore

_ROLE_PATTERN = re.compile(r"^[a-z][a-z0-9_.-]{0,95}$")


class AdapterRegistry:
    def __init__(self) -> None:
        self._adapters: dict[str, SensorAdapter] = {}

    def register(self, adapter: SensorAdapter) -> None:
        if not adapter.name or adapter.name in self._adapters:
            raise ValueError(f"duplicate or empty adapter name: {adapter.name!r}")
        self._adapters[adapter.name] = adapter

    def get(self, name: str) -> SensorAdapter:
        try:
            return self._adapters[name]
        except KeyError as error:
            raise KeyError(f"unknown sensor adapter: {name}") from error

    def list(self) -> list[dict[str, str]]:
        return [
            {"name": adapter.name, "version": adapter.version}
            for adapter in sorted(self._adapters.values(), key=lambda item: item.name)
        ]


class CaptureBus:
    """Content-addressed, interruption-safe capture orchestration."""

    def __init__(self, project: ProjectStore, registry: AdapterRegistry):
        self.project = project
        self.registry = registry
        self.artifacts = ArtifactStore(project)

    def observe(
        self,
        adapter_name: str,
        target: dict[str, Any],
        config: dict[str, Any],
        *,
        rights_decision: str,
        source_id: str | None = None,
    ) -> dict[str, Any]:
        if not rights_decision.strip():
            raise ValueError("rights_decision is required")
        adapter = self.registry.get(adapter_name)
        normalized_target = adapter.normalize_target(target)
        if not normalized_target.get("id"):
            raise ValueError("sensor adapter must provide a stable target id")
        normalized_config = adapter.normalize_config(normalized_target, config)
        environment = adapter.environment(normalized_config)
        request = {
            "adapter": adapter.name,
            "adapter_version": adapter.version,
            "target": normalized_target,
            "configuration": normalized_config,
            "environment": environment,
            "rights_decision": rights_decision.strip(),
            "source_id": source_id,
        }
        capture_id = hashlib.sha256(canonical_json(request)).hexdigest()
        existing = self.get(capture_id)
        if existing and existing["status"] == "COMPLETE" and self.verify(capture_id)["valid"]:
            existing["reused"] = True
            return existing

        now = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO observation_captures("
                "id,target_id,source_id,adapter,adapter_version,normalized_request_json,"
                "environment_json,rights_decision,status,authority,manifest_digest,"
                "summary_json,limitations_json,attempt_count,created_at,updated_at"
                ") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) "
                "ON CONFLICT(id) DO UPDATE SET status='CAPTURING',"
                "attempt_count=observation_captures.attempt_count+1,updated_at=excluded.updated_at",
                (
                    capture_id,
                    normalized_target.get("id"),
                    source_id,
                    adapter.name,
                    adapter.version,
                    canonical_json(request).decode(),
                    canonical_json(environment).decode(),
                    rights_decision.strip(),
                    "CAPTURING",
                    "OBSERVED",
                    None,
                    "{}",
                    "[]",
                    1,
                    now,
                    now,
                ),
            )
        self._event(capture_id, "capture.started", {"request": request})

        def sink(
            role: str,
            data: bytes,
            media_type: str,
            metadata: dict[str, Any] | None = None,
        ) -> dict[str, Any]:
            return self._ingest_capture_artifact(
                capture_id,
                role,
                data,
                media_type,
                metadata=metadata or {},
            )

        try:
            outcome = adapter.capture(normalized_target, normalized_config, sink)
            if not isinstance(outcome, CaptureOutcome):
                raise TypeError("sensor adapter returned an invalid CaptureOutcome")
            manifest = self._finalize(capture_id, request, outcome)
        except Exception as error:
            with self.project.connection() as connection:
                connection.execute(
                    "UPDATE observation_captures SET status='INTERRUPTED',updated_at=? WHERE id=?",
                    (utc_now(), capture_id),
                )
            self._event(
                capture_id,
                "capture.interrupted",
                {"error_type": type(error).__name__, "message": str(error)},
            )
            raise
        result = self.get(capture_id)
        if result is None:
            raise RuntimeError("completed capture could not be reloaded")
        result["manifest"] = manifest
        result["reused"] = False
        return result

    def _ingest_capture_artifact(
        self,
        capture_id: str,
        role: str,
        data: bytes,
        media_type: str,
        *,
        metadata: dict[str, Any],
    ) -> dict[str, Any]:
        if not _ROLE_PATTERN.fullmatch(role):
            raise ValueError(f"invalid artifact role: {role!r}")
        record = self._ingest_bytes(data, media_type, f"{role.replace('.', '-')}.bin")
        now = utc_now()
        with self.project.connection() as connection:
            existing = connection.execute(
                "SELECT artifact_digest FROM observation_capture_artifacts "
                "WHERE capture_id=? AND role=?",
                (capture_id, role),
            ).fetchone()
            if existing and existing["artifact_digest"] != record["digest"]:
                raise RuntimeError(
                    f"capture artifact conflict for {role}: an interrupted capture cannot "
                    "silently mix sensor states"
                )
            connection.execute(
                "INSERT OR IGNORE INTO observation_capture_artifacts("
                "capture_id,role,artifact_digest,media_type,metadata_json,created_at"
                ") VALUES(?,?,?,?,?,?)",
                (
                    capture_id,
                    role,
                    record["digest"],
                    media_type,
                    canonical_json(metadata).decode(),
                    now,
                ),
            )
        return {"role": role, **record, "media_type": media_type, "metadata": metadata}

    def _ingest_bytes(self, data: bytes, media_type: str, name: str) -> dict[str, Any]:
        staging = self.project.root / "observations" / ".staging"
        staging.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(prefix="capture-", suffix=f"-{name}", dir=staging) as file:
            file.write(data)
            file.flush()
            record = self.artifacts.ingest_file(Path(file.name), media_type=media_type)
        return record.to_dict()

    def _event(self, capture_id: str, event_type: str, payload: dict[str, Any]) -> None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT COALESCE(MAX(sequence),0)+1 FROM observation_events WHERE capture_id=?",
                (capture_id,),
            ).fetchone()
            sequence = int(row[0])
        receipt = {
            "schema": "vision.observation-event/v1",
            "capture_id": capture_id,
            "sequence": sequence,
            "event_type": event_type,
            "payload": payload,
            "created_at": utc_now(),
        }
        record = self._ingest_bytes(
            canonical_json(receipt), "application/json", f"event-{sequence}.json"
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO observation_events("
                "capture_id,sequence,event_type,payload_json,receipt_digest,created_at"
                ") VALUES(?,?,?,?,?,?)",
                (
                    capture_id,
                    sequence,
                    event_type,
                    canonical_json(payload).decode(),
                    record["digest"],
                    receipt["created_at"],
                ),
            )

    def _finalize(
        self,
        capture_id: str,
        request: dict[str, Any],
        outcome: CaptureOutcome,
    ) -> dict[str, Any]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT role,artifact_digest,media_type,metadata_json,created_at "
                "FROM observation_capture_artifacts WHERE capture_id=? ORDER BY role",
                (capture_id,),
            ).fetchall()
        if not rows:
            raise RuntimeError("sensor adapter produced no evidence artifacts")
        created_at = utc_now()
        manifest = {
            "schema": "vision.observation-envelope/v1",
            "capture_id": capture_id,
            "authority": "OBSERVED",
            "request": request,
            "artifacts": [
                {
                    "role": row["role"],
                    "digest": row["artifact_digest"],
                    "media_type": row["media_type"],
                    "metadata": json.loads(row["metadata_json"]),
                    "created_at": row["created_at"],
                }
                for row in rows
            ],
            "summary": outcome.summary,
            "limitations": outcome.limitations,
            "created_at": created_at,
        }
        manifest_record = self._ingest_bytes(
            canonical_json(manifest), "application/json", "observation-envelope.json"
        )
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE observation_captures SET status='COMPLETE',manifest_digest=?,"
                "summary_json=?,limitations_json=?,updated_at=? WHERE id=?",
                (
                    manifest_record["digest"],
                    canonical_json(outcome.summary).decode(),
                    canonical_json(outcome.limitations).decode(),
                    created_at,
                    capture_id,
                ),
            )
            for graph in outcome.graphs:
                role = str(graph["role"])
                row = connection.execute(
                    "SELECT artifact_digest FROM observation_capture_artifacts "
                    "WHERE capture_id=? AND role=?",
                    (capture_id, role),
                ).fetchone()
                if row is None:
                    raise RuntimeError(f"graph role was not emitted: {role}")
                graph_id = hashlib.sha256(
                    canonical_json(
                        {
                            "capture_id": capture_id,
                            "graph_type": graph["graph_type"],
                            "digest": row["artifact_digest"],
                        }
                    )
                ).hexdigest()
                connection.execute(
                    "INSERT OR REPLACE INTO perceptual_graphs("
                    "id,capture_id,graph_type,artifact_digest,node_count,edge_count,"
                    "authority,created_at) VALUES(?,?,?,?,?,?,?,?)",
                    (
                        graph_id,
                        capture_id,
                        graph["graph_type"],
                        row["artifact_digest"],
                        int(graph.get("node_count", 0)),
                        int(graph.get("edge_count", 0)),
                        str(graph.get("authority", "OBSERVED")),
                        created_at,
                    ),
                )
        self._event(
            capture_id,
            "capture.completed",
            {"manifest_digest": manifest_record["digest"], "artifact_count": len(rows)},
        )
        return manifest

    def get(self, capture_id: str) -> dict[str, Any] | None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM observation_captures WHERE id=?", (capture_id,)
            ).fetchone()
            if row is None:
                return None
            artifacts = connection.execute(
                "SELECT role,artifact_digest,media_type,metadata_json,created_at "
                "FROM observation_capture_artifacts WHERE capture_id=? ORDER BY role",
                (capture_id,),
            ).fetchall()
        return {
            "capture_id": row["id"],
            "adapter": row["adapter"],
            "adapter_version": row["adapter_version"],
            "status": row["status"],
            "authority": row["authority"],
            "manifest_digest": row["manifest_digest"],
            "request": json.loads(row["normalized_request_json"]),
            "environment": json.loads(row["environment_json"]),
            "summary": json.loads(row["summary_json"]),
            "limitations": json.loads(row["limitations_json"]),
            "attempt_count": row["attempt_count"],
            "created_at": row["created_at"],
            "updated_at": row["updated_at"],
            "artifacts": [
                {
                    "role": item["role"],
                    "digest": item["artifact_digest"],
                    "media_type": item["media_type"],
                    "metadata": json.loads(item["metadata_json"]),
                    "created_at": item["created_at"],
                }
                for item in artifacts
            ],
        }

    def verify(self, capture_id: str) -> dict[str, Any]:
        capture = self.get(capture_id)
        if capture is None:
            raise KeyError(f"unknown capture: {capture_id}")
        failures: list[dict[str, Any]] = []
        for artifact in capture["artifacts"]:
            path = self.artifacts.path_for(artifact["digest"])
            if not path.is_file():
                failures.append({"role": artifact["role"], "reason": "missing"})
                continue
            actual, _ = sha256_file(path)
            if actual != artifact["digest"]:
                failures.append(
                    {"role": artifact["role"], "reason": "digest_mismatch", "actual": actual}
                )
        manifest_digest = capture["manifest_digest"]
        if capture["status"] == "COMPLETE" and not manifest_digest:
            failures.append({"role": "manifest", "reason": "missing_database_reference"})
        elif manifest_digest:
            path = self.artifacts.path_for(manifest_digest)
            if not path.is_file():
                failures.append({"role": "manifest", "reason": "missing"})
            else:
                actual, _ = sha256_file(path)
                if actual != manifest_digest:
                    failures.append(
                        {"role": "manifest", "reason": "digest_mismatch", "actual": actual}
                    )
                else:
                    try:
                        manifest = json.loads(path.read_text(encoding="utf-8"))
                    except (OSError, json.JSONDecodeError):
                        failures.append({"role": "manifest", "reason": "invalid_json"})
                    else:
                        expected_artifacts = [
                            {
                                "role": item["role"],
                                "digest": item["digest"],
                                "media_type": item["media_type"],
                                "metadata": item["metadata"],
                                "created_at": item["created_at"],
                            }
                            for item in capture["artifacts"]
                        ]
                        if manifest.get("capture_id") != capture_id:
                            failures.append(
                                {"role": "manifest", "reason": "capture_id_mismatch"}
                            )
                        if manifest.get("request") != capture["request"]:
                            failures.append({"role": "manifest", "reason": "request_mismatch"})
                        if manifest.get("artifacts") != expected_artifacts:
                            failures.append(
                                {"role": "manifest", "reason": "artifact_index_mismatch"}
                            )
        with self.project.connection() as connection:
            events = connection.execute(
                "SELECT sequence,event_type,payload_json,receipt_digest,created_at "
                "FROM observation_events WHERE capture_id=? ORDER BY sequence",
                (capture_id,),
            ).fetchall()
        for event in events:
            path = self.artifacts.path_for(event["receipt_digest"])
            if not path.is_file():
                failures.append(
                    {"role": f"event.{event['sequence']}", "reason": "missing"}
                )
                continue
            actual, _ = sha256_file(path)
            if actual != event["receipt_digest"]:
                failures.append(
                    {
                        "role": f"event.{event['sequence']}",
                        "reason": "digest_mismatch",
                        "actual": actual,
                    }
                )
                continue
            try:
                receipt = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, json.JSONDecodeError):
                failures.append(
                    {"role": f"event.{event['sequence']}", "reason": "invalid_json"}
                )
                continue
            if (
                receipt.get("capture_id") != capture_id
                or receipt.get("sequence") != event["sequence"]
                or receipt.get("event_type") != event["event_type"]
                or receipt.get("payload") != json.loads(event["payload_json"])
                or receipt.get("created_at") != event["created_at"]
            ):
                failures.append(
                    {"role": f"event.{event['sequence']}", "reason": "receipt_mismatch"}
                )
        return {
            "capture_id": capture_id,
            "valid": not failures and capture["status"] == "COMPLETE",
            "status": capture["status"],
            "manifest_digest": manifest_digest,
            "failures": failures,
        }

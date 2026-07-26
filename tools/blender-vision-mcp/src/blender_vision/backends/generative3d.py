from __future__ import annotations

import hashlib
import json
import math
import uuid
from pathlib import Path
from typing import Any, Protocol

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.errors import ProjectError
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.projects.store import ProjectStore
from blender_vision.security.adversarial import GeneratedBackendPolicy

OPERATIONS = {
    "generate_shape",
    "generate_shape_and_material",
    "generate_multiview_images",
    "generate_texture",
    "retopologize",
    "export_candidate",
}
PBR_CHANNELS = {
    "base_color",
    "normal",
    "roughness",
    "metallic",
    "ambient_occlusion",
    "emissive",
    "opacity",
}
TERMINAL_REQUEST_STATUSES = {"COMPLETED", "FAILED"}


class Generative3DBackend(Protocol):
    identity: str

    def generate_shape(self, request: dict[str, Any]) -> dict[str, Any]: ...

    def generate_shape_and_material(self, request: dict[str, Any]) -> dict[str, Any]: ...

    def generate_multiview_images(self, request: dict[str, Any]) -> dict[str, Any]: ...

    def generate_texture(self, request: dict[str, Any]) -> dict[str, Any]: ...

    def retopologize(self, request: dict[str, Any]) -> dict[str, Any]: ...

    def export_candidate(self, request: dict[str, Any]) -> dict[str, Any]: ...


class GenerativeProposalStore:
    """Govern distributed generative proposals without granting evidence authority."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def request(
        self,
        operation: str,
        *,
        backend: str,
        inputs: dict[str, Any],
        checkpoint: str,
        license_record: dict[str, Any],
        backend_configuration: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        if operation not in OPERATIONS:
            raise ValueError("unsupported generative 3D operation")
        if not backend.strip() or not checkpoint.strip():
            raise ValueError("generative request requires backend and checkpoint identities")
        if not license_record.get("license"):
            raise ValueError("generative backend requires an explicit license identifier")
        configuration = dict(backend_configuration or {})
        if self._contains_secret(configuration):
            raise ValueError("generative backend configuration cannot persist credentials")
        GeneratedBackendPolicy.validate(configuration)
        reference_ids = self._string_list(inputs.get("reference_ids", []), "reference ids")
        artifact_digests = self._string_list(
            inputs.get("artifact_digests", []), "artifact digests"
        )
        with self.project.connection() as connection:
            known_references = {
                row[0] for row in connection.execute("SELECT id FROM reference_items")
            }
            known_artifacts = {row[0] for row in connection.execute("SELECT digest FROM artifacts")}
        if not set(reference_ids).issubset(known_references):
            raise ValueError("generative request references unknown evidence")
        if not set(artifact_digests).issubset(known_artifacts):
            raise ValueError("generative request references unknown input artifacts")
        if operation in {"retopologize", "export_candidate"} and not artifact_digests:
            raise ValueError(f"{operation} requires a registered input artifact")
        if not reference_ids and not artifact_digests and not str(inputs.get("prompt", "")).strip():
            raise ValueError("generative request requires evidence, an artifact, or a prompt")
        normalized_inputs = {
            **inputs,
            "reference_ids": reference_ids,
            "artifact_digests": artifact_digests,
        }
        stable = {
            "operation": operation,
            "backend": backend.strip(),
            "checkpoint": checkpoint.strip(),
            "license": license_record,
            "backend_configuration": configuration,
            "inputs": normalized_inputs,
        }
        cache_key = hashlib.sha256(canonical_json(stable)).hexdigest()
        with self.project.connection() as connection:
            existing = connection.execute(
                "SELECT id FROM generative_requests WHERE cache_key=? "
                "ORDER BY created_at,id LIMIT 1",
                (cache_key,),
            ).fetchone()
        if existing:
            return self.get_request(existing["id"])
        request_id = str(uuid.uuid4())
        now = utc_now()
        record = {
            "schema_version": 2,
            "record_type": "generative_3d_request",
            "id": request_id,
            "operation": operation,
            "backend": backend.strip(),
            "checkpoint": checkpoint.strip(),
            "license": license_record,
            "backend_configuration": configuration,
            "inputs": normalized_inputs,
            "input_artifact_digests": sorted(set(artifact_digests)),
            "status": "REQUESTED",
            "authority": "proposal_only",
            "acceptance_eligible": False,
            "cache_key": cache_key,
            "created_at": now,
        }
        relative = Path("receipts") / f"generative-request-{request_id}.json"
        atomic_write_json(self.project.root / relative, record)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.generative-request+json",
        )
        existing_id = None
        with self.project.connection() as connection:
            inserted = connection.execute(
                "INSERT OR IGNORE INTO generative_requests("
                "id,operation,backend,request_json,request_digest,license_json,cache_key,status,"
                "created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
                (
                    request_id,
                    operation,
                    backend.strip(),
                    json.dumps(record),
                    artifact.digest,
                    json.dumps(license_record),
                    cache_key,
                    "REQUESTED",
                    now,
                    now,
                ),
            )
            if inserted.rowcount == 0:
                existing = connection.execute(
                    "SELECT id FROM generative_requests WHERE cache_key=?", (cache_key,)
                ).fetchone()
                if existing is None:
                    raise RuntimeError("generative request cache insertion raced without a winner")
                existing_id = existing["id"]
        if existing_id:
            return self.get_request(existing_id)
        return {
            **record,
            "request_digest": artifact.digest,
            "artifact": artifact.to_dict(),
            "path": str(relative),
        }

    def bind_job(self, request_id: str, job_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            job = connection.execute(
                "SELECT operation,config_json FROM jobs WHERE id=?", (job_id,)
            ).fetchone()
            request = connection.execute(
                "SELECT status,job_id FROM generative_requests WHERE id=?", (request_id,)
            ).fetchone()
            if request is None:
                raise KeyError(f"unknown generative request: {request_id}")
            if job is None or job["operation"] != "generative3d.execute":
                raise ValueError("generative request requires a generative3d.execute job")
            if json.loads(job["config_json"]).get("request_id") != request_id:
                raise ValueError("generative job is bound to a different request")
            if request["status"] == "QUEUED" and request["job_id"] == job_id:
                return self.get_request(request_id)
            if request["status"] != "REQUESTED":
                raise ValueError("only a requested generative proposal can be queued")
            connection.execute(
                "UPDATE generative_requests SET status='QUEUED',job_id=?,updated_at=? WHERE id=?",
                (job_id, utc_now(), request_id),
            )
        return self.get_request(request_id)

    def mark_failed(self, request_id: str, *, error: dict[str, Any]) -> dict[str, Any]:
        del error
        now = utc_now()
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT status FROM generative_requests WHERE id=?", (request_id,)
            ).fetchone()
            if row is None:
                raise KeyError(f"unknown generative request: {request_id}")
            if row["status"] == "FAILED":
                return self.get_request(request_id)
            if row["status"] == "COMPLETED":
                raise ValueError("completed generative request cannot be failed")
            connection.execute(
                "UPDATE generative_requests SET status='FAILED',updated_at=? WHERE id=?",
                (now, request_id),
            )
        return self.get_request(request_id)

    def import_result(
        self,
        request_id: str,
        *,
        mesh_digests: list[str],
        texture_digests: list[str],
        image_digests: list[str] | None = None,
        pbr_channels: dict[str, str],
        backend_identity: str,
        checkpoint: str,
        input_reference_ids: list[str],
        generation_seed: int,
        confidence: float,
        known_limitations: list[str],
    ) -> dict[str, Any]:
        if not isinstance(confidence, (int, float)) or not math.isfinite(float(confidence)):
            raise ValueError("generative confidence must be finite")
        if not 0.0 <= float(confidence) <= 1.0:
            raise ValueError("generative confidence must be between zero and one")
        if isinstance(generation_seed, bool) or not isinstance(generation_seed, int):
            raise ValueError("generative result requires an integer generation seed")
        if not known_limitations or any(not str(item).strip() for item in known_limitations):
            raise ValueError("generative result must declare known limitations")
        request = self.get_request(request_id)
        if request["status"] not in {"REQUESTED", "QUEUED"}:
            raise ValueError("generative request already has a terminal result")
        if backend_identity != request["backend"] or checkpoint != request["checkpoint"]:
            raise ValueError("generative result backend or checkpoint differs from its request")
        expected_references = set(request["inputs"].get("reference_ids", []))
        if set(input_reference_ids) != expected_references:
            raise ValueError("generative result must retain the exact requested evidence inputs")
        meshes = sorted(set(self._string_list(mesh_digests, "mesh digests")))
        textures = sorted(set(self._string_list(texture_digests, "texture digests")))
        images = sorted(set(self._string_list(image_digests or [], "image digests")))
        if not set(pbr_channels).issubset(PBR_CHANNELS):
            raise ValueError("generative result contains an unsupported PBR channel")
        if any(not isinstance(value, str) or not value for value in pbr_channels.values()):
            raise ValueError("generative PBR channels require artifact digests")
        digests = set(meshes) | set(textures) | set(images) | set(pbr_channels.values())
        with self.project.connection() as connection:
            artifact_rows = {
                row["digest"]: row["media_type"]
                for row in connection.execute("SELECT digest,media_type FROM artifacts")
            }
        if not digests or not digests.issubset(artifact_rows):
            raise ValueError("generative result requires registered output artifacts")
        if any(not artifact_rows[digest].startswith("model/") for digest in meshes):
            raise ValueError("generative mesh outputs must use model media types")
        if any(not artifact_rows[digest].startswith("image/") for digest in textures + images):
            raise ValueError("generative image outputs must use image media types")
        if any(
            not artifact_rows[digest].startswith("image/") for digest in pbr_channels.values()
        ):
            raise ValueError("generative PBR outputs must use image media types")
        self._validate_output_contract(
            request["operation"], meshes, textures, images, pbr_channels
        )
        result_id = str(uuid.uuid4())
        now = utc_now()
        result = {
            "schema_version": 2,
            "record_type": "generative_3d_result",
            "id": result_id,
            "request_id": request_id,
            "request_digest": request["request_digest"],
            "operation": request["operation"],
            "mesh_digests": meshes,
            "texture_digests": textures,
            "image_digests": images,
            "pbr_channels": dict(sorted(pbr_channels.items())),
            "backend_identity": backend_identity,
            "checkpoint": checkpoint,
            "license": request["license"],
            "commercial_eligible": bool(
                request["license"].get("commercial_use") is True
                and not request["license"].get("research_only", False)
            ),
            "input_reference_ids": sorted(expected_references),
            "input_artifact_digests": request.get("input_artifact_digests", []),
            "generation_seed": generation_seed,
            "confidence": float(confidence),
            "known_limitations": [str(item).strip() for item in known_limitations],
            "evidence_class": "SYNTHETIC_HYPOTHESIS",
            "status": "HYPOTHESIS",
            "acceptance_eligible": False,
            "authority": {
                "geometry": "inferred_proposal_only",
                "hidden_surfaces": "unverified",
                "materials": "appearance_initialization_only",
            },
            "created_at": now,
        }
        relative = Path("receipts") / f"generative-result-{result_id}.json"
        atomic_write_json(self.project.root / relative, result)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.generative-result+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current = connection.execute(
                "SELECT status FROM generative_requests WHERE id=?", (request_id,)
            ).fetchone()
            if current is None or current["status"] not in {"REQUESTED", "QUEUED"}:
                raise RuntimeError("generative request changed during result finalization")
            connection.execute(
                "INSERT INTO generative_results("
                "id,request_id,result_json,record_digest,status,created_at) VALUES(?,?,?,?,?,?)",
                (result_id, request_id, json.dumps(result), artifact.digest, "HYPOTHESIS", now),
            )
            connection.execute(
                "UPDATE generative_requests SET status='COMPLETED',updated_at=? WHERE id=?",
                (now, request_id),
            )
        return {**result, "artifact": artifact.to_dict(), "path": str(relative)}

    def get_request(self, request_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM generative_requests WHERE id=?", (request_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown generative request: {request_id}")
        value = json.loads(row["request_json"])
        value["status"] = row["status"]
        value["request_digest"] = row["request_digest"]
        value["job_id"] = row["job_id"]
        return value

    def get_result(self, request_id: str, *, verify: bool = True) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT result_json,record_digest FROM generative_results WHERE request_id=?",
                (request_id,),
            ).fetchone()
        if row is None:
            raise KeyError(f"generative request has no imported result: {request_id}")
        if verify:
            audit = self.audit(request_id)
            if audit["invalid_request_ids"] or audit["invalid_result_ids"]:
                raise ProjectError("generative proposal receipt or artifact lineage is invalid")
        return {**json.loads(row["result_json"]), "record_digest": row["record_digest"]}

    def audit(self, request_id: str | None = None) -> dict[str, Any]:
        with self.project.connection() as connection:
            if request_id:
                request_rows = connection.execute(
                    "SELECT * FROM generative_requests WHERE id=?", (request_id,)
                ).fetchall()
            else:
                request_rows = connection.execute(
                    "SELECT * FROM generative_requests ORDER BY created_at,id"
                ).fetchall()
            result_rows = connection.execute(
                "SELECT * FROM generative_results ORDER BY created_at,id"
            ).fetchall()
            artifacts = {
                row["digest"]: {
                    "relative_path": row["relative_path"],
                    "media_type": row["media_type"],
                }
                for row in connection.execute(
                    "SELECT digest,relative_path,media_type FROM artifacts"
                )
            }
            known_references = {
                row[0] for row in connection.execute("SELECT id FROM reference_items")
            }
        selected_ids = {row["id"] for row in request_rows}
        results_by_request = {
            row["request_id"]: row for row in result_rows if row["request_id"] in selected_ids
        }
        invalid_requests = []
        invalid_results = []
        requests = []
        results = []
        for row in request_rows:
            record = json.loads(row["request_json"])
            result_row = results_by_request.get(row["id"])
            stable = {
                "operation": record.get("operation"),
                "backend": record.get("backend"),
                "checkpoint": record.get("checkpoint"),
                "license": record.get("license"),
                "backend_configuration": record.get("backend_configuration"),
                "inputs": record.get("inputs"),
            }
            expected_cache_key = hashlib.sha256(canonical_json(stable)).hexdigest()
            valid = bool(
                row["request_digest"]
                and self._artifact_json(artifacts, row["request_digest"]) == record
                and record.get("id") == row["id"]
                and record.get("operation") == row["operation"]
                and record.get("backend") == row["backend"]
                and record.get("license") == json.loads(row["license_json"])
                and bool(record.get("license", {}).get("license"))
                and record.get("checkpoint")
                and record.get("cache_key") == row["cache_key"] == expected_cache_key
                and not self._contains_secret(record.get("backend_configuration", {}))
                and record.get("acceptance_eligible") is False
                and set(record.get("inputs", {}).get("reference_ids", []))
                .issubset(known_references)
                and set(record.get("input_artifact_digests", [])).issubset(artifacts)
                and (
                    row["operation"] not in {"retopologize", "export_candidate"}
                    or bool(record.get("input_artifact_digests"))
                )
                and (
                    (row["status"] == "COMPLETED" and result_row is not None)
                    or (row["status"] != "COMPLETED" and result_row is None)
                )
            )
            if not valid:
                invalid_requests.append(row["id"])
            requests.append(
                {
                    **record,
                    "status": row["status"],
                    "request_digest": row["request_digest"],
                    "valid": valid,
                }
            )
            if result_row is None:
                continue
            result = json.loads(result_row["result_json"])
            output_digests = {
                *result.get("mesh_digests", []),
                *result.get("texture_digests", []),
                *result.get("image_digests", []),
                *result.get("pbr_channels", {}).values(),
            }
            result_valid = bool(
                result_row["record_digest"]
                and self._artifact_json(artifacts, result_row["record_digest"]) == result
                and result.get("request_id") == row["id"]
                and result.get("request_digest") == row["request_digest"]
                and result.get("operation") == row["operation"]
                and result.get("backend_identity") == row["backend"]
                and result.get("checkpoint") == record.get("checkpoint")
                and result.get("license") == record.get("license")
                and result.get("commercial_eligible")
                is (
                    record.get("license", {}).get("commercial_use") is True
                    and not record.get("license", {}).get("research_only", False)
                )
                and set(result.get("input_reference_ids", []))
                == set(record.get("inputs", {}).get("reference_ids", []))
                and result.get("evidence_class") == "SYNTHETIC_HYPOTHESIS"
                and result.get("status") == "HYPOTHESIS"
                and result.get("acceptance_eligible") is False
                and result.get("authority")
                == {
                    "geometry": "inferred_proposal_only",
                    "hidden_surfaces": "unverified",
                    "materials": "appearance_initialization_only",
                }
                and isinstance(result.get("generation_seed"), int)
                and not isinstance(result.get("generation_seed"), bool)
                and isinstance(result.get("confidence"), (int, float))
                and not isinstance(result.get("confidence"), bool)
                and math.isfinite(float(result.get("confidence")))
                and 0.0 <= float(result.get("confidence")) <= 1.0
                and bool(result.get("known_limitations"))
                and all(str(item).strip() for item in result.get("known_limitations", []))
                and set(result.get("pbr_channels", {})).issubset(PBR_CHANNELS)
                and output_digests
                and all(self._artifact_valid(artifacts, digest) for digest in output_digests)
                and all(
                    artifacts[digest]["media_type"].startswith("model/")
                    for digest in result.get("mesh_digests", [])
                )
                and all(
                    artifacts[digest]["media_type"].startswith("image/")
                    for digest in [
                        *result.get("texture_digests", []),
                        *result.get("image_digests", []),
                        *result.get("pbr_channels", {}).values(),
                    ]
                )
            )
            try:
                self._validate_output_contract(
                    row["operation"],
                    result.get("mesh_digests", []),
                    result.get("texture_digests", []),
                    result.get("image_digests", []),
                    result.get("pbr_channels", {}),
                )
            except ValueError:
                result_valid = False
            if not result_valid:
                invalid_results.append(result["id"])
            results.append(
                {
                    **result,
                    "record_digest": result_row["record_digest"],
                    "valid": result_valid,
                }
            )
        return {
            "requests": requests,
            "results": results,
            "invalid_request_ids": invalid_requests,
            "invalid_result_ids": invalid_results,
        }

    @staticmethod
    def _validate_output_contract(
        operation: str,
        meshes: list[str],
        textures: list[str],
        images: list[str],
        pbr_channels: dict[str, str],
    ) -> None:
        if operation in {"generate_shape", "retopologize", "export_candidate"} and not meshes:
            raise ValueError(f"{operation} requires at least one mesh artifact")
        if operation == "generate_shape_and_material" and (
            not meshes or not (textures or pbr_channels)
        ):
            raise ValueError("generate_shape_and_material requires mesh and material artifacts")
        if operation == "generate_multiview_images" and not images:
            raise ValueError("generate_multiview_images requires image artifacts")
        if operation == "generate_texture" and not (textures or pbr_channels):
            raise ValueError("generate_texture requires texture or PBR artifacts")

    @staticmethod
    def _string_list(value: Any, label: str) -> list[str]:
        if not isinstance(value, list) or any(
            not isinstance(item, str) or not item for item in value
        ):
            raise ValueError(f"generative {label} must be a list of non-empty strings")
        return sorted(set(value))

    @staticmethod
    def _contains_secret(value: Any) -> bool:
        if isinstance(value, dict):
            return any(
                any(term in str(key).lower() for term in ("token", "secret", "password", "api_key"))
                or GenerativeProposalStore._contains_secret(item)
                for key, item in value.items()
            )
        if isinstance(value, list):
            return any(GenerativeProposalStore._contains_secret(item) for item in value)
        return False

    def _artifact_json(
        self, artifacts: dict[str, dict[str, str]], digest: str
    ) -> dict[str, Any] | None:
        if not self._artifact_valid(artifacts, digest):
            return None
        try:
            value = json.loads(
                (self.project.root / artifacts[digest]["relative_path"]).read_text(
                    encoding="utf-8"
                )
            )
        except (OSError, json.JSONDecodeError):
            return None
        return value if isinstance(value, dict) else None

    def _artifact_valid(self, artifacts: dict[str, dict[str, str]], digest: str) -> bool:
        item = artifacts.get(digest)
        if not item:
            return False
        path = self.project.root / item["relative_path"]
        return path.is_file() and sha256_file(path)[0] == digest

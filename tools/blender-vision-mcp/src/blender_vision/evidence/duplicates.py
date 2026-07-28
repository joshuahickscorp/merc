from __future__ import annotations

import json
import uuid
from pathlib import Path
from typing import Any

from PIL import Image, ImageOps

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.evidence.targets import TargetResolver
from blender_vision.projects.store import ProjectStore

AUTHORITY_ORDER = {
    "user_owned": 0,
    "manufacturer_authoritative": 1,
    "licensed_or_reusable": 2,
    "public_factual_technical": 3,
    "diagnostic_third_party": 4,
}


class EvidenceDuplicateStore:
    """Suppress repeated image evidence without treating copies as independent observations."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def audit(
        self,
        target_id: str | None = None,
        *,
        record: bool = True,
        maximum_hamming_distance: int = 6,
    ) -> dict[str, Any]:
        if not 0 <= maximum_hamming_distance <= 16:
            raise ValueError("duplicate Hamming-distance threshold must be between 0 and 16")
        resolution = TargetResolver(self.project).get(target_id)
        sources = self._records(resolution["id"])
        fingerprints: dict[str, dict[str, Any]] = {}
        failures: list[dict[str, str]] = []
        for source in sources:
            try:
                fingerprints[source["id"]] = self._fingerprint(source)
            except (OSError, ValueError) as error:
                failures.append(
                    {
                        "source_id": source["id"],
                        "reference_id": source["reference_id"],
                        "error": f"{type(error).__name__}: {error}",
                    }
                )

        edges: list[dict[str, Any]] = []
        parent = {source_id: source_id for source_id in fingerprints}

        def find(source_id: str) -> str:
            while parent[source_id] != source_id:
                parent[source_id] = parent[parent[source_id]]
                source_id = parent[source_id]
            return source_id

        def union(left: str, right: str) -> None:
            left_root, right_root = find(left), find(right)
            if left_root != right_root:
                parent[right_root] = left_root

        source_ids = sorted(fingerprints)
        for index, left_id in enumerate(source_ids):
            left = fingerprints[left_id]
            for right_id in source_ids[index + 1 :]:
                right = fingerprints[right_id]
                normal = self._distance(left["dhash"], right["dhash"])
                mirrored = self._distance(left["dhash"], right["mirrored_dhash"])
                if left["artifact_digest"] == right["artifact_digest"]:
                    relationship = "EXACT_DUPLICATE"
                    distance = 0
                elif mirrored <= maximum_hamming_distance and mirrored < normal:
                    relationship = "MIRRORED_DUPLICATE"
                    distance = mirrored
                elif normal <= maximum_hamming_distance:
                    relationship = "PERCEPTUAL_DUPLICATE"
                    distance = normal
                else:
                    continue
                union(left_id, right_id)
                edges.append(
                    {
                        "left_source_id": left_id,
                        "right_source_id": right_id,
                        "relationship": relationship,
                        "hamming_distance": distance,
                        "normal_hamming_distance": normal,
                        "mirrored_hamming_distance": mirrored,
                    }
                )

        members: dict[str, list[dict[str, Any]]] = {}
        by_id = {source["id"]: source for source in sources}
        for source_id in source_ids:
            members.setdefault(find(source_id), []).append(by_id[source_id])
        groups = []
        eligibility = {
            source["id"]: {
                "independent_evidence_eligible": True,
                "coverage_eligible": source["status"] == "ACQUIRED",
                "canonical_source_id": source["id"],
                "reasons": [],
            }
            for source in sources
        }
        for group_members in members.values():
            if len(group_members) < 2:
                continue
            canonical = min(group_members, key=self._canonical_rank)
            group_ids = sorted(item["id"] for item in group_members)
            group_edges = [
                edge
                for edge in edges
                if edge["left_source_id"] in group_ids and edge["right_source_id"] in group_ids
            ]
            groups.append(
                {
                    "canonical_source_id": canonical["id"],
                    "source_ids": group_ids,
                    "reference_ids": sorted(item["reference_id"] for item in group_members),
                    "relationships": sorted(
                        {edge["relationship"] for edge in group_edges}
                    ),
                    "edges": group_edges,
                }
            )
            for member in group_members:
                state = eligibility[member["id"]]
                state["canonical_source_id"] = canonical["id"]
                if member["id"] != canonical["id"]:
                    state["independent_evidence_eligible"] = False
                    state["coverage_eligible"] = False
                    state["reasons"].append("duplicate_of_canonical_source")

        groups.sort(key=lambda item: item["canonical_source_id"])
        report = {
            "schema_version": 1,
            "receipt_type": "evidence_duplicate_audit",
            "target_id": resolution["id"],
            "source_count": len(sources),
            "fingerprinted_source_count": len(fingerprints),
            "unique_media_count": len(sources)
            - sum(len(group["source_ids"]) - 1 for group in groups),
            "duplicate_group_count": len(groups),
            "duplicate_groups": groups,
            "duplicate_edges": edges,
            "fingerprint_failures": failures,
            "source_eligibility": eligibility,
            "policy": {
                "maximum_dhash_hamming_distance": maximum_hamming_distance,
                "duplicates_count_as_independent_observations": False,
                "canonical_selection": (
                    "authority_then_quality_then_resolution_then_creation_order"
                ),
                "suppression_changes_source_rights": False,
            },
        }
        if not record:
            return report
        run_id = str(uuid.uuid4())
        created_at = utc_now()
        report.update({"id": run_id, "status": "COMPLETE", "created_at": created_at})
        relative = Path("receipts") / f"evidence-duplicates-{run_id}.json"
        atomic_write_json(self.project.root / relative, report)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.evidence-duplicates+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO evidence_duplicate_runs(id,target_id,status,report_json,"
                "artifact_digest,created_at) VALUES(?,?,?,?,?,?)",
                (
                    run_id,
                    resolution["id"],
                    report["status"],
                    json.dumps(report),
                    artifact.digest,
                    created_at,
                ),
            )
            for source_id, state in eligibility.items():
                if not state["independent_evidence_eligible"]:
                    connection.execute(
                        "UPDATE reference_items SET acceptance_eligible=0 "
                        "WHERE id=(SELECT reference_id FROM evidence_sources WHERE id=?)",
                        (source_id,),
                    )
        return {**report, "artifact": artifact.to_dict(), "path": str(relative)}

    def _records(self, target_id: str) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            return [
                {
                    **dict(row),
                    "source": json.loads(row["source_json"]),
                    "metadata": json.loads(row["metadata_json"]),
                }
                for row in connection.execute(
                    "SELECT s.*,r.artifact_digest,r.relative_path,r.media_type,r.metadata_json "
                    "FROM evidence_sources s JOIN reference_items r ON r.id=s.reference_id "
                    "WHERE s.target_id=? AND s.status='ACQUIRED' AND r.media_type LIKE 'image/%' "
                    "ORDER BY s.created_at,s.id",
                    (target_id,),
                )
            ]

    def _fingerprint(self, source: dict[str, Any]) -> dict[str, Any]:
        path = (self.project.root / source["relative_path"]).resolve()
        path.relative_to(self.project.root.resolve())
        with Image.open(path) as image:
            image.load()
            normalized = ImageOps.exif_transpose(image).convert("L")
            dhash = self._dhash(normalized)
            mirrored_dhash = self._dhash(ImageOps.mirror(normalized))
        return {
            "artifact_digest": source["artifact_digest"],
            "dhash": dhash,
            "mirrored_dhash": mirrored_dhash,
        }

    @staticmethod
    def _dhash(image: Image.Image) -> int:
        resized = image.resize((9, 8), Image.Resampling.LANCZOS)
        pixels = list(resized.get_flattened_data())
        value = 0
        for row in range(8):
            for column in range(8):
                offset = row * 9 + column
                value = (value << 1) | int(pixels[offset] > pixels[offset + 1])
        return value

    @staticmethod
    def _distance(left: int, right: int) -> int:
        return (left ^ right).bit_count()

    @staticmethod
    def _canonical_rank(source: dict[str, Any]) -> tuple[Any, ...]:
        record = source["source"]
        return (
            AUTHORITY_ORDER.get(str(record.get("authority_class")), 99),
            -float(record.get("quality_score", 0.0)),
            -int(source["metadata"].get("width", 0)) * int(source["metadata"].get("height", 0)),
            source["created_at"],
            source["id"],
        )

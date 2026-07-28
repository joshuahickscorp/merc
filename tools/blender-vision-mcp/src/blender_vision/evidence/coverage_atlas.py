from __future__ import annotations

import json
import math
import uuid
from typing import Any

from blender_vision.core.util import utc_now
from blender_vision.projects.store import ProjectStore


class SurfaceCoverageAtlas:
    """Canonical component/region cells that make missing evidence explicit."""

    def __init__(self, project: ProjectStore):
        self.project = project

    def bootstrap(
        self,
        *,
        target_id: str,
        regions: list[str],
        synchronize: bool = False,
    ) -> dict[str, Any]:
        normalized_regions = list(
            dict.fromkeys(str(item).strip() for item in regions if str(item).strip())
        )
        if synchronize:
            now = utc_now()
            with self.project.connection() as connection:
                rows = connection.execute(
                    "SELECT id,region,record_json FROM surface_coverage_cells "
                    "WHERE target_id=?",
                    (target_id,),
                ).fetchall()
                for row in rows:
                    record = json.loads(row["record_json"])
                    active = row["region"] in normalized_regions
                    if record.get("active", True) == active:
                        continue
                    record["active"] = active
                    record["retired_reason"] = (
                        None if active else "not present in synchronized category ontology"
                    )
                    record["updated_at"] = now
                    connection.execute(
                        "UPDATE surface_coverage_cells SET record_json=?,updated_at=? "
                        "WHERE id=?",
                        (json.dumps(record), now, row["id"]),
                    )
        created = []
        for region in normalized_regions:
            with self.project.connection() as connection:
                existing = connection.execute(
                    "SELECT record_json FROM surface_coverage_cells "
                    "WHERE target_id=? AND region=?",
                    (target_id, region),
                ).fetchone()
            if existing:
                created.append(json.loads(existing["record_json"]))
                continue
            cell_id = str(uuid.uuid4())
            now = utc_now()
            record = {
                "id": cell_id,
                "target_id": target_id,
                "component_id": None,
                "region": region,
                "active": True,
                "retired_reason": None,
                "observation_count": 0,
                "best_incidence_angle_degrees": None,
                "best_resolution_pixels": None,
                "occlusion_fraction": 1.0,
                "reflection_risk": "unknown",
                "evidence_class": "UNSEEN",
                "uncertainty": {"classification": "unobserved"},
                "observation_ids": [],
                "updated_at": now,
            }
            with self.project.connection() as connection:
                connection.execute(
                    "INSERT INTO surface_coverage_cells"
                    "(id,target_id,component_id,region,record_json,created_at,updated_at) "
                    "VALUES(?,?,?,?,?,?,?)",
                    (cell_id, target_id, None, region, json.dumps(record), now, now),
                )
            created.append(record)
        return {"target_id": target_id, "cells": created, "cell_count": len(created)}

    def observe(
        self,
        cell_id: str,
        *,
        observation_id: str,
        incidence_angle_degrees: float,
        resolution_pixels: int,
        occlusion_fraction: float,
        reflection_risk: str,
        evidence_class: str,
        uncertainty: dict[str, Any],
    ) -> dict[str, Any]:
        cell = self.get(cell_id)
        angle = float(incidence_angle_degrees)
        occlusion = float(occlusion_fraction)
        if not math.isfinite(angle) or not 0.0 <= angle <= 90.0:
            raise ValueError("incidence angle must be within 0 to 90 degrees")
        if resolution_pixels <= 0 or not math.isfinite(occlusion) or not 0.0 <= occlusion <= 1.0:
            raise ValueError("coverage resolution and occlusion are invalid")
        if evidence_class in {"SYNTHETIC_HYPOTHESIS", "UNSEEN"}:
            raise ValueError("hypothetical or unseen records cannot count as observations")
        observation_ids = set(cell["observation_ids"])
        observation_ids.add(observation_id)
        cell["observation_count"] = len(observation_ids)
        cell["best_incidence_angle_degrees"] = min(
            angle,
            cell["best_incidence_angle_degrees"]
            if cell["best_incidence_angle_degrees"] is not None
            else angle,
        )
        cell["best_resolution_pixels"] = max(
            resolution_pixels, cell["best_resolution_pixels"] or 0
        )
        cell["occlusion_fraction"] = min(occlusion, cell["occlusion_fraction"])
        cell["reflection_risk"] = reflection_risk
        cell["evidence_class"] = evidence_class
        cell["uncertainty"] = uncertainty
        cell["observation_ids"] = sorted(observation_ids)
        cell["updated_at"] = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE surface_coverage_cells SET record_json=?,updated_at=? WHERE id=?",
                (json.dumps(cell), cell["updated_at"], cell_id),
            )
        return cell

    def observe_governed_source(
        self,
        source_id: str,
        *,
        observations: list[dict[str, Any]],
    ) -> dict[str, Any]:
        """Credit inspected regions only to an acquired, named-reviewed source."""
        from blender_vision.evidence.acquisition import EvidenceAcquisitionStore

        acquisition = EvidenceAcquisitionStore(self.project)
        source = acquisition.get(source_id)
        if source["status"] != "ACQUIRED" or not source.get("reference_id"):
            raise ValueError("surface observations require an acquired source reference")
        if not acquisition.authority_status(source_id)["acquisition_valid"]:
            raise ValueError(
                "surface observations require receipt-verified source acquisition"
            )
        if not source.get("reviewed_by") or not source.get("reviewed_at"):
            raise ValueError("surface observations require a named governance review")
        if not source["rights"].get("internal_use"):
            raise PermissionError("source rights do not permit reconstruction use")
        if not observations:
            raise ValueError("governed surface observation list cannot be empty")
        report = self.analyze(source["target_id"])
        cells = {
            cell["region"]: cell
            for cell in report["cells"]
            if cell.get("active", True)
        }
        unknown = sorted(
            {
                str(item.get("region", "")).strip()
                for item in observations
                if str(item.get("region", "")).strip() not in cells
            }
        )
        if unknown:
            raise KeyError(f"unknown active surface regions: {unknown}")
        credited = []
        for item in observations:
            region = str(item.get("region", "")).strip()
            if not region:
                raise ValueError("surface observation region cannot be blank")
            credited.append(
                self.observe(
                    cells[region]["id"],
                    observation_id=f"{source['reference_id']}:{region}",
                    incidence_angle_degrees=item["incidence_angle_degrees"],
                    resolution_pixels=item["resolution_pixels"],
                    occlusion_fraction=item["occlusion_fraction"],
                    reflection_risk=item["reflection_risk"],
                    evidence_class=item["evidence_class"],
                    uncertainty={
                        **dict(item["uncertainty"]),
                        "source_id": source_id,
                        "reference_id": source["reference_id"],
                    },
                )
            )
        return {
            "source_id": source_id,
            "reference_id": source["reference_id"],
            "credited_regions": [item["region"] for item in credited],
            "cells": credited,
            "coverage": self.analyze(source["target_id"]),
        }

    def get(self, cell_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT record_json FROM surface_coverage_cells WHERE id=?", (cell_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown surface coverage cell: {cell_id}")
        return json.loads(row["record_json"])

    def analyze(self, target_id: str | None = None) -> dict[str, Any]:
        with self.project.connection() as connection:
            if target_id:
                rows = connection.execute(
                    "SELECT record_json FROM surface_coverage_cells WHERE target_id=? "
                    "ORDER BY region,id",
                    (target_id,),
                ).fetchall()
            else:
                rows = connection.execute(
                    "SELECT record_json FROM surface_coverage_cells ORDER BY region,id"
                ).fetchall()
        all_cells = [json.loads(row["record_json"]) for row in rows]
        cells = [cell for cell in all_cells if cell.get("active", True)]
        unresolved = [
            cell
            for cell in cells
            if cell["observation_count"] == 0
            or cell["occlusion_fraction"] > 0.5
            or (cell["best_resolution_pixels"] or 0) < 512
        ]
        return {
            "cell_count": len(cells),
            "retired_cell_count": len(all_cells) - len(cells),
            "observed_cell_count": len(cells) - len(unresolved),
            "coverage_fraction": (
                round((len(cells) - len(unresolved)) / len(cells), 6) if cells else 0.0
            ),
            "cells": cells,
            "unresolved_regions": [cell["region"] for cell in unresolved],
            "next_best_evidence": [
                {
                    "region": cell["region"],
                    "request": (
                        f"Capture {cell['region']} at 20–35 degrees incidence, at least 1600 "
                        "pixels across the target region, sharp and unobscured"
                    ),
                }
                for cell in unresolved
            ],
        }

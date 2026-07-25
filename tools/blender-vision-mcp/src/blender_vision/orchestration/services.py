from __future__ import annotations

import json
from typing import Any

from blender_vision.core.util import utc_now
from blender_vision.projects.store import ProjectStore

SERVICES = {
    "vggt",
    "feature_detector",
    "segmentation",
    "generative_3d",
    "blender_trusted_worker",
}


class WarmServiceRegistry:
    """Track optional warm workers and evict by pressure and expected reuse."""

    def __init__(self, project: ProjectStore):
        self.project = project

    def update(
        self,
        name: str,
        *,
        status: str,
        memory_gb: float,
        expected_reuse: float,
        backend: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        if name not in SERVICES:
            raise ValueError("unsupported warm service")
        if status not in {"COLD", "STARTING", "WARM", "EVICTED", "FAILED"}:
            raise ValueError("invalid warm service status")
        if memory_gb < 0 or not 0.0 <= expected_reuse <= 1.0:
            raise ValueError("warm service memory and reuse values are invalid")
        record = {
            "name": name,
            "status": status,
            "memory_gb": float(memory_gb),
            "expected_reuse": float(expected_reuse),
            "backend": backend or {},
            "updated_at": utc_now(),
        }
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO warm_services(name,status,record_json,updated_at) VALUES(?,?,?,?) "
                "ON CONFLICT(name) DO UPDATE SET status=excluded.status,"
                "record_json=excluded.record_json,updated_at=excluded.updated_at",
                (name, status, json.dumps(record), record["updated_at"]),
            )
        return record

    def evict_for_pressure(self, *, required_free_gb: float) -> dict[str, Any]:
        if required_free_gb < 0:
            raise ValueError("required free memory cannot be negative")
        services = [item for item in self.list() if item["status"] == "WARM"]
        services.sort(key=lambda item: (item["expected_reuse"], -item["memory_gb"], item["name"]))
        evicted = []
        freed = 0.0
        for item in services:
            if freed >= required_free_gb:
                break
            updated = self.update(
                item["name"],
                status="EVICTED",
                memory_gb=item["memory_gb"],
                expected_reuse=item["expected_reuse"],
                backend=item["backend"],
            )
            evicted.append(updated)
            freed += item["memory_gb"]
        return {"required_free_gb": required_free_gb, "freed_gb": freed, "evicted": evicted}

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT record_json FROM warm_services ORDER BY name"
            ).fetchall()
        return [json.loads(row["record_json"]) for row in rows]

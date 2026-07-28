from __future__ import annotations

import json
from typing import Any

from blender_vision.core.util import utc_now
from blender_vision.parametric.components import ComponentSpec
from blender_vision.projects.store import ProjectStore


class ComponentStore:
    def __init__(self, project: ProjectStore):
        self.project = project

    def create(self, component: ComponentSpec) -> dict[str, Any]:
        record = component.to_dict()
        now = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO components(id,type,record_json,revision,created_at,updated_at) "
                "VALUES(?,?,?,?,?,?)",
                (component.id, component.type.value, json.dumps(record), 1, now, now),
            )
        return {**record, "revision": 1, "created_at": now, "updated_at": now}

    def update_parameters(self, component_id: str, parameters: dict[str, Any]) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT record_json,revision,created_at FROM components WHERE id=?", (component_id,)
            ).fetchone()
            if row is None:
                raise FileNotFoundError(f"unknown component: {component_id}")
            record = json.loads(row["record_json"])
            record["parameters"].update(parameters)
            revision = int(row["revision"]) + 1
            updated_at = utc_now()
            connection.execute(
                "UPDATE components SET record_json=?,revision=?,updated_at=? WHERE id=?",
                (json.dumps(record), revision, updated_at, component_id),
            )
        return {
            **record,
            "revision": revision,
            "created_at": row["created_at"],
            "updated_at": updated_at,
        }

    def get(self, component_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM components WHERE id=?", (component_id,)
            ).fetchone()
        if row is None:
            raise FileNotFoundError(f"unknown component: {component_id}")
        return {
            **json.loads(row["record_json"]),
            "revision": row["revision"],
            "created_at": row["created_at"],
            "updated_at": row["updated_at"],
        }

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute("SELECT * FROM components ORDER BY created_at").fetchall()
        return [
            {
                **json.loads(row["record_json"]),
                "revision": row["revision"],
                "created_at": row["created_at"],
                "updated_at": row["updated_at"],
            }
            for row in rows
        ]

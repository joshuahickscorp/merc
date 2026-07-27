"""Archetype registry and licensing/provenance manifest."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from blender_vision.core.errors import ValidationError
from blender_vision.core.util import utc_now
from blender_vision.procedural.archetype import Archetype
from blender_vision.procedural.datacenter import DATACENTER_ARCHETYPES, make_archetype


@dataclass(slots=True)
class ArchetypeManifest:
    """Library-level manifest with per-archetype licensing and provenance."""

    library_id: str
    created_at: str
    archetypes: list[dict[str, Any]] = field(default_factory=list)
    licensing_summary: str = "internal-synthetic-procedural"
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "library_id": self.library_id,
            "created_at": self.created_at,
            "licensing_summary": self.licensing_summary,
            "archetypes": list(self.archetypes),
            "notes": list(self.notes),
        }


class ArchetypeLibrary:
    """In-process registry of importable archetypes."""

    def __init__(self) -> None:
        self._registry: dict[str, type[Archetype]] = dict(DATACENTER_ARCHETYPES)

    def register(self, cls: type[Archetype]) -> None:
        if not cls.name:
            raise ValidationError("archetype class must declare a non-empty name")
        self._registry[cls.name] = cls

    def get(self, name: str) -> type[Archetype]:
        if name not in self._registry:
            raise ValidationError(
                f"archetype {name!r} is not registered; known: {sorted(self._registry)}"
            )
        return self._registry[name]

    def create(self, name: str, params: dict[str, Any] | None = None) -> Archetype:
        return self.get(name)(params)

    def names(self) -> list[str]:
        return sorted(self._registry)

    def __contains__(self, name: str) -> bool:
        return name in self._registry

    def __len__(self) -> int:
        return len(self._registry)

    def manifest(self, *, library_id: str = "datacenter-v1") -> ArchetypeManifest:
        entries: list[dict[str, Any]] = []
        for name in self.names():
            instance = self.create(name)
            entries.append(instance.to_manifest_entry())
        return ArchetypeManifest(
            library_id=library_id,
            created_at=utc_now(),
            archetypes=entries,
            notes=[
                "All archetypes are synthetic PROCEDURAL_GROUND_TRUTH geometry.",
                "Dimensional parameters follow EIA-310 / IEC 60297 where applicable.",
            ],
        )


_DEFAULT = ArchetypeLibrary()


def default_library() -> ArchetypeLibrary:
    return _DEFAULT


def list_archetypes() -> list[str]:
    return default_library().names()


def get_archetype(name: str, params: dict[str, Any] | None = None) -> Archetype:
    return make_archetype(name, params)

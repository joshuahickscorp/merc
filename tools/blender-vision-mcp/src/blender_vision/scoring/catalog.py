from __future__ import annotations

import hashlib
import json
import os
import subprocess
from importlib import resources
from pathlib import Path
from typing import Any

from blender_vision.scoring.models import CapabilityFacet


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        separators=(",", ":"),
        sort_keys=True,
    ).encode("utf-8")


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def sha256_path(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def default_authority_root() -> Path:
    configured = os.environ.get("BVMCP_100_PLUS_ROOT")
    if configured:
        return Path(configured).expanduser().resolve()
    source_root = Path(__file__).resolve().parents[3] / "benchmarks" / "100_plus"
    if source_root.is_dir():
        return source_root
    installed = resources.files("blender_vision").joinpath("benchmarks", "data", "100_plus")
    return Path(str(installed))


class CapabilityCatalog:
    def __init__(self, root: Path | None = None):
        self.root = (root or default_authority_root()).resolve()
        self.facets_path = self.root / "original_facets.json"
        self.schema_path = self.root / "score_schema.json"
        self.registry_path = self.root / "acceptance_registry.json"
        self._raw_facets = self._load_json(self.facets_path)
        self.registry = self._load_json(self.registry_path)
        self.facets = [CapabilityFacet.model_validate(item) for item in self._raw_facets["facets"]]
        ids = [facet.id for facet in self.facets]
        if len(ids) != len(set(ids)):
            raise ValueError("capability facet IDs must be unique")

    @staticmethod
    def _load_json(path: Path) -> Any:
        if not path.is_file():
            raise FileNotFoundError(path)
        return json.loads(path.read_text(encoding="utf-8"))

    @property
    def catalog_sha256(self) -> str:
        return sha256_path(self.facets_path)

    @property
    def registry_sha256(self) -> str:
        return sha256_path(self.registry_path)

    def list(self, domain: str | None = None) -> list[CapabilityFacet]:
        facets = self.facets
        if domain:
            if domain not in {"app", "3d", "system"}:
                raise ValueError(f"unknown capability domain: {domain}")
            facets = [facet for facet in facets if facet.domain == domain]
        return sorted(facets, key=lambda item: (item.domain, item.id))

    def get(self, facet_id: str) -> CapabilityFacet:
        for facet in self.facets:
            if facet.id == facet_id:
                return facet
        raise KeyError(f"unknown capability facet: {facet_id}")

    def select(self, selector: str) -> list[CapabilityFacet]:
        if selector == "all":
            return self.list()
        if selector in {"app", "3d", "system"}:
            return self.list(selector)
        return [self.get(selector)]

    def git_head(self) -> str:
        configured = os.environ.get("BVMCP_GIT_HEAD")
        if configured:
            if len(configured) != 40:
                raise ValueError("BVMCP_GIT_HEAD must be a full 40-character Git SHA")
            return configured
        project_root = Path(__file__).resolve().parents[3]
        process = subprocess.run(
            ["git", "-C", str(project_root), "rev-parse", "HEAD"],
            check=False,
            capture_output=True,
            text=True,
        )
        head = process.stdout.strip()
        if process.returncode != 0 or len(head) != 40:
            raise RuntimeError(
                "capability evaluation requires a Git checkout or explicit BVMCP_GIT_HEAD"
            )
        return head

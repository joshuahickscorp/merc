from __future__ import annotations

import ast
import hashlib
import platform
import re
from pathlib import Path
from typing import Any

from blender_vision.core.util import canonical_json, sha256_file
from blender_vision.perception.contracts import ArtifactSink, CaptureOutcome
from blender_vision.perception.query import ObservationQueryService
from blender_vision.projects.store import ProjectStore

_LANGUAGES = {
    ".py": "python",
    ".js": "javascript",
    ".jsx": "javascript",
    ".ts": "typescript",
    ".tsx": "typescript",
    ".css": "css",
    ".scss": "scss",
    ".sass": "sass",
    ".less": "less",
    ".html": "html",
    ".vue": "vue",
    ".svelte": "svelte",
    ".glsl": "glsl",
    ".vert": "glsl",
    ".frag": "glsl",
    ".wgsl": "wgsl",
    ".json": "json",
    ".toml": "toml",
    ".yaml": "yaml",
    ".yml": "yaml",
}
_ASSET_SUFFIXES = {
    ".png",
    ".jpg",
    ".jpeg",
    ".webp",
    ".svg",
    ".gif",
    ".glb",
    ".gltf",
    ".obj",
    ".fbx",
    ".blend",
    ".hdr",
    ".exr",
}
_SKIP_DIRECTORIES = {
    ".git",
    ".hg",
    ".svn",
    ".venv",
    "node_modules",
    "dist",
    "build",
    "__pycache__",
    ".pytest_cache",
    ".mypy_cache",
    ".ruff_cache",
}
_BUILD_FILES = {
    "package.json",
    "pyproject.toml",
    "vite.config.js",
    "vite.config.ts",
    "webpack.config.js",
    "tsconfig.json",
    "Dockerfile",
}
_IMPORT_RE = re.compile(
    r"(?:from\s+['\"]([^'\"]+)['\"]|import\s+(?:[^'\";]+?\s+from\s+)?['\"]([^'\"]+)['\"]"
    r"|require\(\s*['\"]([^'\"]+)['\"]\s*\))"
)
_SYMBOL_RE = re.compile(
    r"(?:export\s+(?:default\s+)?(?:async\s+)?(?:function|class|const|let|var)\s+"
    r"|(?:async\s+)?(?:function|class)\s+|(?:const|let|var)\s+)"
    r"([A-Za-z_$][\w$]*)",
)
_SELECTOR_RE = re.compile(r"(?m)([^{}]+)\{")
_TOKEN_RE = re.compile(r"(--[\w-]+)\s*:\s*([^;}{]+)")
_HTML_ID_RE = re.compile(r"\bid\s*=\s*['\"]([^'\"]+)['\"]")
_HTML_CLASS_RE = re.compile(r"\bclass(?:Name)?\s*=\s*['\"]([^'\"]+)['\"]")
_ASSET_RE = re.compile(
    r"['\"]([^'\"]+\.(?:png|jpe?g|webp|svg|gif|glb|gltf|obj|fbx|blend|hdr|exr))"
    r"(?:[?#][^'\"]*)?['\"]",
    re.IGNORECASE,
)


def _repository_manifest(root: Path) -> list[dict[str, Any]]:
    entries: list[dict[str, Any]] = []
    total_size = 0
    for path in sorted(root.rglob("*")):
        relative = path.relative_to(root)
        if any(part in _SKIP_DIRECTORIES for part in relative.parts):
            continue
        if path.is_symlink() or not path.is_file():
            continue
        if (
            path.suffix.lower() not in _LANGUAGES
            and path.name not in _BUILD_FILES
            and path.suffix.lower() not in _ASSET_SUFFIXES
        ):
            continue
        digest, size = sha256_file(path)
        total_size += size
        if len(entries) >= 5000 or total_size > 25 * 1024 * 1024:
            raise ValueError("repository source snapshot exceeds the 5,000 file/25 MiB bound")
        entries.append(
            {
                "path": relative.as_posix(),
                "digest": digest,
                "size": size,
                "language": _LANGUAGES.get(path.suffix.lower(), "asset"),
            }
        )
    if not entries:
        raise ValueError("repository has no supported source, configuration, or asset files")
    return entries


class CodeRepositoryAdapter:
    name = "code.repository"
    version = "1"

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        root = Path(str(target.get("path", ""))).expanduser().resolve()
        if not root.is_dir():
            raise ValueError("code.repository target.path must be an existing directory")
        manifest = _repository_manifest(root)
        digest = hashlib.sha256(canonical_json(manifest)).hexdigest()
        return {
            "id": str(target.get("id") or digest),
            "kind": "repository",
            "path": str(root),
            "digest": digest,
            "file_count": len(manifest),
            "manifest": manifest,
        }

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        maximum_files = max(
            1, min(int(config.get("maximum_files", target["file_count"])), 5000)
        )
        bindings = []
        for item in config.get("runtime_bindings", []):
            source_path = Path(str(item.get("source_path", "")))
            if (
                source_path.is_absolute()
                or ".." in source_path.parts
                or not str(item.get("runtime_node_id", "")).strip()
            ):
                raise ValueError("runtime bindings require a confined source_path and node id")
            bindings.append(
                {
                    "runtime_node_id": str(item["runtime_node_id"]),
                    "source_path": source_path.as_posix(),
                    "symbol": str(item.get("symbol", "")),
                    "binding_kind": str(item.get("binding_kind", "instrumented")),
                    "capture_id": item.get("capture_id"),
                }
            )
        return {
            "maximum_files": maximum_files,
            "runtime_bindings": bindings,
            "linked_capture_ids": sorted(
                {str(value) for value in config.get("linked_capture_ids", [])}
            ),
        }

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {
            "platform": platform.platform(),
            "python": platform.python_version(),
            "parser": "python-ast-plus-static-language-patterns",
            "adapter": self.name,
            "adapter_version": self.version,
            "maximum_files": config["maximum_files"],
        }

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        root = Path(target["path"])
        current = _repository_manifest(root)
        current_digest = hashlib.sha256(canonical_json(current)).hexdigest()
        if current_digest != target["digest"]:
            raise RuntimeError("repository changed between normalization and capture")
        selected = current[: config["maximum_files"]]
        manifest = {
            "schema": "vision.source-manifest/v1",
            "repository_digest": target["digest"],
            "files": selected,
            "truncated": len(selected) != len(current),
        }
        manifest_artifact = sink(
            "source.manifest", canonical_json(manifest), "application/json", None
        )
        graph = _code_graph(
            root,
            selected,
            manifest_artifact["digest"],
            config["runtime_bindings"],
            config["linked_capture_ids"],
        )
        sink("source.graph", canonical_json(graph), "application/json", None)
        return CaptureOutcome(
            summary={
                "file_count": len(selected),
                "node_count": len(graph["nodes"]),
                "edge_count": len(graph["edges"]),
                "symbol_count": sum(
                    node["domain_type"]
                    in {"Symbol", "Component", "Hook", "Route", "Store"}
                    for node in graph["nodes"]
                ),
                "selector_count": sum(
                    node["domain_type"] == "CSSSelector" for node in graph["nodes"]
                ),
                "runtime_binding_count": len(graph["runtime_bindings"]),
            },
            limitations=[
                "Python symbols use the standard AST; other languages use bounded static patterns.",
                "No repository code is executed and no dependency installation is performed.",
                "Runtime bindings are OBSERVED only when supplied by governed instrumentation.",
            ],
            graphs=[
                {
                    "graph_type": "CodeGraph",
                    "role": "source.graph",
                    "node_count": len(graph["nodes"]),
                    "edge_count": len(graph["edges"]),
                    "authority": "MIXED",
                }
            ],
        )


def _code_graph(
    root: Path,
    files: list[dict[str, Any]],
    manifest_digest: str,
    runtime_bindings: list[dict[str, Any]],
    linked_capture_ids: list[str],
) -> dict[str, Any]:
    evidence = [{"role": "source.manifest", "artifact_digest": manifest_digest}]
    nodes: list[dict[str, Any]] = []
    edges: list[dict[str, Any]] = []
    file_nodes: dict[str, str] = {}
    symbol_nodes: dict[tuple[str, str], str] = {}
    for entry in files:
        path = entry["path"]
        file_id = f"file:{path}"
        file_nodes[path] = file_id
        domain_type = _file_domain(path, entry["language"])
        nodes.append(
            {
                "id": file_id,
                "domain_type": domain_type,
                "path": path,
                "language": entry["language"],
                "digest": entry["digest"],
                "size": entry["size"],
                "evidence_references": evidence,
                "authority": "OBSERVED",
                "confidence": 1.0,
                "source_restrictions": ["governed-repository-snapshot"],
                "uncertainty": [],
                "revision_lineage": [],
            }
        )
        if entry["language"] == "asset":
            continue
        source_path = root / path
        try:
            text = source_path.read_text(encoding="utf-8")
        except (UnicodeDecodeError, OSError):
            continue
        symbols = (
            _python_symbols(text)
            if entry["language"] == "python"
            else _pattern_symbols(text, path)
        )
        for symbol in symbols:
            symbol_id = f"source:{path}:{symbol['line']}:{symbol['name']}"
            symbol_nodes[(path, symbol["name"])] = symbol_id
            nodes.append(
                {
                    "id": symbol_id,
                    "domain_type": symbol["domain_type"],
                    "path": path,
                    "name": symbol["name"],
                    "line": symbol["line"],
                    "exported": symbol["exported"],
                    "evidence_references": evidence,
                    "authority": "OBSERVED",
                    "confidence": symbol["confidence"],
                    "source_restrictions": ["governed-repository-snapshot"],
                    "uncertainty": symbol["uncertainty"],
                    "revision_lineage": [],
                }
            )
            edges.append(
                {
                    "source": file_id,
                    "target": symbol_id,
                    "type": "DECLARES",
                    "authority": "OBSERVED",
                    "evidence_references": evidence,
                }
            )
        for module in _imports(text, entry["language"]):
            edges.append(
                {
                    "source": file_id,
                    "target": f"module:{module}",
                    "type": "IMPORTS",
                    "authority": "OBSERVED",
                    "evidence_references": evidence,
                }
            )
        for selector in _selectors(text, entry["language"]):
            selector_id = f"selector:{path}:{selector}"
            nodes.append(
                _derived_source_node(
                    selector_id, "CSSSelector", path, evidence, selector=selector
                )
            )
            edges.append(
                {
                    "source": file_id,
                    "target": selector_id,
                    "type": "DECLARES",
                    "authority": "DERIVED",
                    "evidence_references": evidence,
                }
            )
        for token, value in _TOKEN_RE.findall(text):
            token_id = f"token:{path}:{token}"
            nodes.append(
                _derived_source_node(
                    token_id, "DesignToken", path, evidence, name=token, value=value.strip()
                )
            )
            edges.append(
                {
                    "source": file_id,
                    "target": token_id,
                    "type": "DECLARES",
                    "authority": "DERIVED",
                    "evidence_references": evidence,
                }
            )
        for asset in sorted(set(_ASSET_RE.findall(text))):
            edges.append(
                {
                    "source": file_id,
                    "target": f"asset:{asset}",
                    "type": "REFERENCES_ASSET",
                    "authority": "OBSERVED",
                    "evidence_references": evidence,
                }
            )
    bindings = []
    for index, binding in enumerate(runtime_bindings):
        source_id = symbol_nodes.get(
            (binding["source_path"], binding["symbol"]),
            file_nodes.get(binding["source_path"]),
        )
        authority = "OBSERVED" if source_id else "HYPOTHESIS"
        binding_id = f"runtime-binding:{index}"
        record = {
            **binding,
            "id": binding_id,
            "source_node_id": source_id,
            "authority": authority,
            "confidence": 1.0 if source_id else 0.0,
            "evidence_references": evidence,
        }
        bindings.append(record)
        nodes.append(
            {
                **record,
                "domain_type": "RuntimeBinding",
                "source_restrictions": ["governed-runtime-instrumentation"],
                "uncertainty": [] if source_id else ["source-symbol-not-found"],
                "revision_lineage": [],
            }
        )
        if source_id:
            edges.append(
                {
                    "source": source_id,
                    "target": binding_id,
                    "type": "IMPLEMENTS",
                    "authority": "OBSERVED",
                    "evidence_references": evidence,
                }
            )
        edges.append(
            {
                "source": binding_id,
                "target": binding["runtime_node_id"],
                "type": "BINDS_RUNTIME_NODE",
                "authority": authority,
                "evidence_references": evidence,
            }
        )
    return {
        "schema": "vision.code-graph/v1",
        "graph_type": "CodeGraph",
        "authority": "MIXED",
        "repository_digest": hashlib.sha256(canonical_json(files)).hexdigest(),
        "linked_capture_ids": linked_capture_ids,
        "nodes": nodes,
        "edges": edges,
        "runtime_bindings": bindings,
    }


def _file_domain(path: str, language: str) -> str:
    lowered = path.casefold()
    if language == "asset":
        return "Asset"
    if ".test." in lowered or ".spec." in lowered or "/tests/" in f"/{lowered}":
        return "TestFile"
    if ".stories." in lowered or "/stories/" in f"/{lowered}":
        return "StoryFile"
    if Path(path).name in _BUILD_FILES:
        return "BuildConfiguration"
    if language in {"glsl", "wgsl"}:
        return "ShaderFile"
    return "SourceFile"


def _python_symbols(text: str) -> list[dict[str, Any]]:
    try:
        tree = ast.parse(text)
    except SyntaxError:
        return []
    result = []
    for node in ast.walk(tree):
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            continue
        result.append(
            {
                "name": node.name,
                "line": node.lineno,
                "domain_type": _symbol_domain(node.name),
                "exported": not node.name.startswith("_"),
                "confidence": 1.0,
                "uncertainty": [],
            }
        )
    return result


def _pattern_symbols(text: str, path: str) -> list[dict[str, Any]]:
    result = []
    for match in _SYMBOL_RE.finditer(text):
        name = match.group(1)
        line = text.count("\n", 0, match.start()) + 1
        prefix = text[max(0, match.start() - 40) : match.start()]
        result.append(
            {
                "name": name,
                "line": line,
                "domain_type": _symbol_domain(name, path),
                "exported": "export" in prefix or "export" in match.group(0),
                "confidence": 0.82,
                "uncertainty": ["static-pattern-parser"],
            }
        )
    return result


def _symbol_domain(name: str, path: str = "") -> str:
    if name.startswith("use") and len(name) > 3 and name[3:4].isupper():
        return "Hook"
    if "route" in name.casefold() or "/route" in path.casefold():
        return "Route"
    if "store" in name.casefold():
        return "Store"
    if name[:1].isupper():
        return "Component"
    return "Symbol"


def _imports(text: str, language: str) -> list[str]:
    if language == "python":
        try:
            tree = ast.parse(text)
        except SyntaxError:
            return []
        values = []
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                values.extend(alias.name for alias in node.names)
            elif isinstance(node, ast.ImportFrom) and node.module:
                values.append(node.module)
        return sorted(set(values))
    return sorted({next(value for value in match if value) for match in _IMPORT_RE.findall(text)})


def _selectors(text: str, language: str) -> list[str]:
    values = set()
    if language in {"css", "scss", "sass", "less"}:
        for raw in _SELECTOR_RE.findall(text):
            for selector in raw.split(","):
                selector = selector.strip()
                if selector and not selector.startswith("@") and len(selector) <= 160:
                    values.add(selector)
    for value in _HTML_ID_RE.findall(text):
        values.add(f"#{value}")
    for classes in _HTML_CLASS_RE.findall(text):
        values.update(f".{value}" for value in classes.split() if value)
    return sorted(values)


def _derived_source_node(
    node_id: str,
    domain_type: str,
    path: str,
    evidence: list[dict[str, str]],
    **values: Any,
) -> dict[str, Any]:
    return {
        "id": node_id,
        "domain_type": domain_type,
        "path": path,
        "evidence_references": evidence,
        "authority": "DERIVED",
        "confidence": 0.9,
        "source_restrictions": ["governed-repository-snapshot"],
        "uncertainty": ["static-pattern-parser"],
        "revision_lineage": [],
        **values,
    }


class SourceIntelligenceService:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.query = ObservationQueryService(project)

    def visual_blast_radius(
        self,
        code_capture_id: str,
        changed_paths: list[str],
        linked_capture_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        graph = self.query.graph(code_capture_id, "CodeGraph")
        changed = {Path(path).as_posix() for path in changed_paths}
        bindings = [
            binding
            for binding in graph["runtime_bindings"]
            if binding["source_path"] in changed
        ]
        runtime_ids = {binding["runtime_node_id"] for binding in bindings}
        capture_ids = linked_capture_ids or graph["linked_capture_ids"]
        affected = {
            "components": sorted(
                {
                    node.get("name")
                    for node in graph["nodes"]
                    if node.get("path") in changed
                    and node["domain_type"] == "Component"
                    and node.get("name")
                }
            ),
            "states": [],
            "viewports": [],
            "screenshots": [],
            "animations": [],
            "3d_passes": [],
            "tests": sorted(
                {
                    node["path"]
                    for node in graph["nodes"]
                    if node["domain_type"] == "TestFile"
                    and (
                        node["path"] in changed
                        or any(
                            component.get("name")
                            and str(component["name"]).casefold()
                            in node["path"].casefold()
                            for component in graph["nodes"]
                            if component.get("path") in changed
                        )
                    )
                }
            ),
            "assets": sorted(
                {
                    edge["target"].removeprefix("asset:")
                    for edge in graph["edges"]
                    if edge["type"] == "REFERENCES_ASSET"
                    and edge["source"] in {f"file:{path}" for path in changed}
                }
            ),
        }
        citations = [graph["citation"]]
        for capture_id in capture_ids:
            for graph_type in self.query.graph_types(capture_id):
                try:
                    observed = self.query.graph(capture_id, graph_type)
                except KeyError:
                    continue
                matched = [
                    node
                    for node in observed.get("nodes", [])
                    if node.get("id") in runtime_ids
                    or node.get("selector") in runtime_ids
                    or node.get("sourceBinding", {}).get("path") in changed
                ]
                if not matched:
                    continue
                citations.append(observed["citation"])
                if graph_type == "StateGraph":
                    affected["states"].extend(node["id"] for node in matched)
                elif graph_type == "ResponsiveGraph":
                    affected["viewports"].extend(
                        node.get("viewport") for node in matched if node.get("viewport")
                    )
                elif graph_type == "MotionGraph":
                    affected["animations"].extend(node["id"] for node in matched)
                elif graph_type == "GraphicsFrameGraph":
                    affected["3d_passes"].extend(node["id"] for node in matched)
                elif graph_type == "LayoutGraph":
                    affected["screenshots"].append(capture_id)
        return {
            "schema": "vision.visual-blast-radius/v1",
            "code_capture_id": code_capture_id,
            "changed_paths": sorted(changed),
            "runtime_node_ids": sorted(runtime_ids),
            "affected": {
                key: sorted(set(values))
                for key, values in affected.items()
            },
            "search_scope": "locality-only",
            "final_global_gate_required": True,
            "citations": citations,
            "authority": "DERIVED",
        }

    def explain_bindings(
        self,
        visual_nodes: list[dict[str, Any]],
        code_capture_id: str,
    ) -> list[dict[str, Any]]:
        graph = self.query.graph(code_capture_id, "CodeGraph")
        trace = []
        for visual in visual_nodes:
            candidates = {
                visual.get("id"),
                visual.get("selector"),
                f"#{visual.get('sourceBinding', {}).get('id')}"
                if visual.get("sourceBinding", {}).get("id")
                else None,
            }
            candidates.discard(None)
            bindings = [
                item
                for item in graph["runtime_bindings"]
                if item["runtime_node_id"] in candidates
            ]
            selectors = [
                node
                for node in graph["nodes"]
                if node["domain_type"] == "CSSSelector"
                and node.get("selector") in candidates
            ]
            trace.append(
                {
                    "visual_node_id": visual["id"],
                    "runtime_bindings": bindings,
                    "selector_sources": selectors,
                    "code_citation": graph["citation"],
                    "authority": (
                        "OBSERVED"
                        if bindings and all(item["authority"] == "OBSERVED" for item in bindings)
                        else "DERIVED"
                    ),
                }
            )
        return trace

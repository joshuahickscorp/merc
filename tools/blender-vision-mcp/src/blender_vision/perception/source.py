from __future__ import annotations

import ast
import hashlib
import json
import platform
import re
import shutil
import subprocess
from pathlib import Path
from typing import Any

from blender_vision.core.util import canonical_json, runtime_revision, sha256_file
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


def _confined_path(
    root: Path,
    value: str,
    *,
    label: str,
    kind: str,
) -> Path:
    relative = Path(value)
    if relative.is_absolute() or ".." in relative.parts:
        raise ValueError(f"{label} must be a repository-relative confined path")
    component = root
    for part in relative.parts:
        if part in {"", "."}:
            continue
        component /= part
        if component.is_symlink():
            raise ValueError(f"{label} cannot traverse symlinks")
    candidate = (root / relative).resolve()
    if not candidate.is_relative_to(root):
        raise ValueError(f"{label} escaped the repository")
    valid = candidate.is_dir() if kind == "directory" else candidate.is_file()
    if not valid:
        raise ValueError(f"{label} must name an existing {kind}")
    return candidate


def _typescript_authority(
    root: Path,
    package_value: str,
) -> dict[str, Any]:
    package_root = _confined_path(
        root,
        package_value,
        label="typescript_package_path",
        kind="directory",
    )
    package_json = package_root / "package.json"
    if not package_json.is_file() or package_json.is_symlink():
        raise ValueError("TypeScript package.json is missing or unsafe")
    try:
        document = json.loads(package_json.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError) as error:
        raise ValueError("TypeScript package.json is invalid") from error
    if document.get("name") != "typescript":
        raise ValueError("typescript_package_path must resolve to the TypeScript package")
    version = str(document.get("version", ""))
    try:
        major = int(version.split(".", 1)[0])
    except ValueError as error:
        raise ValueError("TypeScript package version is invalid") from error
    if major < 5:
        raise ValueError("compiler semantic indexing requires TypeScript 5 or newer")
    authority_paths = [package_json]
    if major >= 7:
        authority_paths.extend(
            [
                package_root / "dist" / "api" / "sync" / "api.js",
                package_root / "dist" / "ast" / "index.js",
                package_root / "dist" / "ast" / "is.js",
            ]
        )
        system = {"darwin": "darwin", "linux": "linux", "windows": "win32"}.get(
            platform.system().casefold()
        )
        architecture = {
            "arm64": "arm64",
            "aarch64": "arm64",
            "x86_64": "x64",
            "amd64": "x64",
        }.get(platform.machine().casefold())
        if not system or not architecture:
            raise ValueError("TypeScript native compiler platform is unsupported")
        node_modules = package_root.parent
        native_root = node_modules / "@typescript" / f"typescript-{system}-{architecture}"
        native_binary = native_root / "lib" / ("tsc.exe" if system == "win32" else "tsc")
        authority_paths.extend([native_root / "package.json", native_binary])
        engine = "typescript-native-compiler-api"
    else:
        authority_paths.append(package_root / "lib" / "typescript.js")
        engine = "typescript-compiler-api"
    files = []
    for path in authority_paths:
        if not path.is_relative_to(root):
            raise ValueError(f"TypeScript compiler authority file escaped the repository: {path}")
        validated = _confined_path(
            root,
            path.relative_to(root).as_posix(),
            label="TypeScript compiler authority file",
            kind="file",
        )
        digest, size = sha256_file(validated)
        files.append(
            {
                "path": validated.relative_to(root).as_posix(),
                "sha256": digest,
                "size": size,
            }
        )
    node = shutil.which("node")
    if not node:
        raise ValueError("TypeScript compiler semantic indexing requires Node.js")
    node_path = Path(node).resolve()
    node_digest, node_size = sha256_file(node_path)
    process = subprocess.run(
        [str(node_path), "--version"],
        check=False,
        capture_output=True,
        text=True,
        timeout=10,
    )
    if process.returncode != 0:
        raise ValueError("Node.js version probe failed")
    digest = hashlib.sha256(canonical_json(files)).hexdigest()
    return {
        "package_path": package_root.relative_to(root).as_posix(),
        "version": version,
        "engine": engine,
        "authority_files": files,
        "authority_sha256": digest,
        "node_path": str(node_path),
        "node_sha256": node_digest,
        "node_size": node_size,
        "node_version": process.stdout.strip(),
    }


def _verify_typescript_authority(root: Path, authority: dict[str, Any]) -> None:
    observed = []
    for item in authority["authority_files"]:
        path = _confined_path(
            root,
            item["path"],
            label="TypeScript compiler authority file",
            kind="file",
        )
        digest, size = sha256_file(path)
        observed.append({"path": item["path"], "sha256": digest, "size": size})
    digest = hashlib.sha256(canonical_json(observed)).hexdigest()
    if digest != authority["authority_sha256"]:
        raise RuntimeError("TypeScript compiler authority changed before capture")
    node = Path(authority["node_path"])
    node_digest, node_size = sha256_file(node)
    if node_digest != authority["node_sha256"] or node_size != authority["node_size"]:
        raise RuntimeError("Node.js runtime changed before semantic capture")


def _typescript_semantic_index(
    root: Path,
    files: list[dict[str, Any]],
    config: dict[str, Any],
) -> dict[str, Any]:
    authority = config["typescript_authority"]
    _verify_typescript_authority(root, authority)
    analyzer = Path(__file__).with_name("typescript_semantic_index.mjs")
    if not analyzer.is_file():
        raise RuntimeError("bundled TypeScript semantic analyzer is missing")
    selected = [item["path"] for item in files if item["language"] in {"typescript", "javascript"}]
    process = subprocess.run(
        [authority["node_path"], str(analyzer)],
        input=canonical_json(
            {
                "root": str(root),
                "typescript_package": str(root / authority["package_path"]),
                "tsconfig": config["typescript_tsconfig"],
                "files": selected,
                "reference_limit": config["semantic_reference_limit"],
            }
        ),
        check=False,
        capture_output=True,
        timeout=60,
    )
    if process.returncode != 0:
        stderr = process.stderr.decode("utf-8", errors="replace")[-4000:]
        raise RuntimeError(f"TypeScript compiler semantic indexing failed: {stderr}")
    try:
        document = json.loads(process.stdout)
    except json.JSONDecodeError as error:
        raise RuntimeError("TypeScript semantic analyzer emitted invalid JSON") from error
    if (
        document.get("schema") != "vision.typescript-semantic-index/v1"
        or document.get("typescript_version") != authority["version"]
        or not set(document.get("files", [])).issubset(selected)
    ):
        raise RuntimeError("TypeScript semantic analyzer authority contract failed")
    return document


class CodeRepositoryAdapter:
    name = "code.repository"
    version = "2"

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

    def normalize_config(self, target: dict[str, Any], config: dict[str, Any]) -> dict[str, Any]:
        root = Path(target["path"])
        maximum_files = max(1, min(int(config.get("maximum_files", target["file_count"])), 5000))
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
        typescript_package = str(config.get("typescript_package_path", "")).strip()
        typescript_authority = (
            _typescript_authority(root, typescript_package) if typescript_package else None
        )
        tsconfig_value = str(config.get("typescript_tsconfig", "tsconfig.json"))
        if typescript_authority:
            tsconfig = _confined_path(
                root,
                tsconfig_value,
                label="typescript_tsconfig",
                kind="file",
            )
            typescript_tsconfig = tsconfig.relative_to(root).as_posix()
        else:
            typescript_tsconfig = None
        return {
            "maximum_files": maximum_files,
            "runtime_bindings": bindings,
            "linked_capture_ids": sorted(
                {str(value) for value in config.get("linked_capture_ids", [])}
            ),
            "typescript_authority": typescript_authority,
            "typescript_tsconfig": typescript_tsconfig,
            "semantic_reference_limit": max(
                1, min(int(config.get("semantic_reference_limit", 50_000)), 100_000)
            ),
        }

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        authority = config["typescript_authority"]
        analyzer = Path(__file__).with_name("typescript_semantic_index.mjs")
        analyzer_digest = sha256_file(analyzer)[0] if analyzer.is_file() else None
        return {
            "platform": platform.platform(),
            "python": platform.python_version(),
            "parser": (
                authority["engine"] if authority else "python-ast-plus-static-language-patterns"
            ),
            "typescript_version": authority["version"] if authority else None,
            "typescript_authority_sha256": (authority["authority_sha256"] if authority else None),
            "node_version": authority["node_version"] if authority else None,
            "node_sha256": authority["node_sha256"] if authority else None,
            "semantic_analyzer_sha256": analyzer_digest if authority else None,
            "visionmcp_runtime_revision": runtime_revision(),
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
        semantic_index = None
        semantic_artifact = None
        if config["typescript_authority"]:
            semantic_index = _typescript_semantic_index(root, selected, config)
            semantic_artifact = sink(
                "source.typescript-semantic-index",
                canonical_json(semantic_index),
                "application/json",
                {
                    "typescript_version": semantic_index["typescript_version"],
                    "engine": semantic_index["engine"],
                    "authority_sha256": config["typescript_authority"]["authority_sha256"],
                },
            )
        graph = _code_graph(
            root,
            selected,
            manifest_artifact["digest"],
            config["runtime_bindings"],
            config["linked_capture_ids"],
            semantic_index=semantic_index,
            semantic_artifact_digest=(semantic_artifact["digest"] if semantic_artifact else None),
            typescript_authority=config["typescript_authority"],
        )
        sink("source.graph", canonical_json(graph), "application/json", None)
        return CaptureOutcome(
            summary={
                "file_count": len(selected),
                "node_count": len(graph["nodes"]),
                "edge_count": len(graph["edges"]),
                "symbol_count": sum(
                    node["domain_type"] in {"Symbol", "Component", "Hook", "Route", "Store"}
                    for node in graph["nodes"]
                ),
                "selector_count": sum(
                    node["domain_type"] == "CSSSelector" for node in graph["nodes"]
                ),
                "runtime_binding_count": len(graph["runtime_bindings"]),
                "compiler_semantic_symbol_count": (
                    len(semantic_index["symbols"]) if semantic_index else 0
                ),
                "compiler_resolved_import_count": (
                    sum(item["resolution"] == "workspace" for item in semantic_index["imports"])
                    if semantic_index
                    else 0
                ),
                "compiler_reference_count": (
                    len(semantic_index["references"]) if semantic_index else 0
                ),
                "compiler_diagnostic_count": (
                    len(semantic_index["diagnostics"]) if semantic_index else 0
                ),
            },
            limitations=[
                (
                    "Python uses the standard AST; configured TypeScript/JavaScript files use "
                    "the digest-bound compiler API; other languages use bounded static patterns."
                    if semantic_index
                    else "Python symbols use the standard AST; other languages use bounded "
                    "static patterns."
                ),
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
    *,
    semantic_index: dict[str, Any] | None = None,
    semantic_artifact_digest: str | None = None,
    typescript_authority: dict[str, Any] | None = None,
) -> dict[str, Any]:
    evidence = [{"role": "source.manifest", "artifact_digest": manifest_digest}]
    semantic_evidence = [
        *evidence,
        *(
            [
                {
                    "role": "source.typescript-semantic-index",
                    "artifact_digest": semantic_artifact_digest,
                }
            ]
            if semantic_artifact_digest
            else []
        ),
    ]
    nodes: list[dict[str, Any]] = []
    edges: list[dict[str, Any]] = []
    file_nodes: dict[str, str] = {}
    symbol_nodes: dict[tuple[str, str], str] = {}
    semantic_by_path: dict[str, list[dict[str, Any]]] = {}
    if semantic_index:
        for symbol in semantic_index["symbols"]:
            semantic_by_path.setdefault(symbol["path"], []).append(symbol)
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
        compiler_symbols = semantic_by_path.get(path)
        if compiler_symbols is not None:
            symbols = [
                {
                    "name": symbol["name"],
                    "line": symbol["line"],
                    "character": symbol["character"],
                    "domain_type": _semantic_symbol_domain(symbol, path),
                    "exported": symbol["exported"],
                    "confidence": 1.0,
                    "uncertainty": [],
                    "compiler_kind": symbol["kind"],
                    "compiler_type": symbol["type"],
                    "semantic_symbol_id": symbol["id"],
                }
                for symbol in compiler_symbols
            ]
        else:
            symbols = (
                _python_symbols(text)
                if entry["language"] == "python"
                else _pattern_symbols(text, path)
            )
        for symbol in symbols:
            symbol_id = f"source:{path}:{symbol['line']}:{symbol['name']}"
            symbol_nodes.setdefault((path, symbol["name"]), symbol_id)
            nodes.append(
                {
                    "id": symbol_id,
                    "domain_type": symbol["domain_type"],
                    "path": path,
                    "name": symbol["name"],
                    "line": symbol["line"],
                    "exported": symbol["exported"],
                    "evidence_references": (
                        semantic_evidence if "semantic_symbol_id" in symbol else evidence
                    ),
                    "authority": "OBSERVED",
                    "confidence": symbol["confidence"],
                    "source_restrictions": ["governed-repository-snapshot"],
                    "uncertainty": symbol["uncertainty"],
                    "revision_lineage": [],
                    **(
                        {
                            "character": symbol["character"],
                            "compiler_kind": symbol["compiler_kind"],
                            "compiler_type": symbol["compiler_type"],
                            "semantic_symbol_id": symbol["semantic_symbol_id"],
                        }
                        if "semantic_symbol_id" in symbol
                        else {}
                    ),
                }
            )
            edges.append(
                {
                    "source": file_id,
                    "target": symbol_id,
                    "type": "DECLARES",
                    "authority": "OBSERVED",
                    "evidence_references": (
                        semantic_evidence if "semantic_symbol_id" in symbol else evidence
                    ),
                }
            )
        modules = [] if compiler_symbols is not None else _imports(text, entry["language"])
        for module in modules:
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
                _derived_source_node(selector_id, "CSSSelector", path, evidence, selector=selector)
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
    if semantic_index:
        semantic_ids = {
            node.get("semantic_symbol_id"): node["id"]
            for node in nodes
            if node.get("semantic_symbol_id")
        }
        for imported in semantic_index["imports"]:
            target = (
                file_nodes.get(imported["resolved_path"]) if imported["resolved_path"] else None
            ) or f"module:{imported['module']}"
            edges.append(
                {
                    "source": file_nodes[imported["source_path"]],
                    "target": target,
                    "type": (
                        "RESOLVES_IMPORT" if imported["resolved_path"] else "IMPORTS_EXTERNAL"
                    ),
                    "module": imported["module"],
                    "line": imported["line"],
                    "authority": "OBSERVED",
                    "evidence_references": semantic_evidence,
                }
            )
        for reference in semantic_index["references"]:
            target = semantic_ids.get(reference["target_symbol_id"])
            source = file_nodes.get(reference["source_path"])
            if not target or not source:
                continue
            edges.append(
                {
                    "source": source,
                    "target": target,
                    "type": "REFERENCES_SYMBOL",
                    "name": reference["name"],
                    "line": reference["line"],
                    "character": reference["character"],
                    "authority": "OBSERVED",
                    "evidence_references": semantic_evidence,
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
        "semantic_index": (
            {
                "enabled": True,
                "engine": semantic_index["engine"],
                "typescript_version": semantic_index["typescript_version"],
                "typescript_authority_sha256": typescript_authority["authority_sha256"],
                "artifact_digest": semantic_artifact_digest,
                "file_count": len(semantic_index["files"]),
                "symbol_count": len(semantic_index["symbols"]),
                "resolved_import_count": sum(
                    item["resolution"] == "workspace" for item in semantic_index["imports"]
                ),
                "reference_count": len(semantic_index["references"]),
                "reference_truncated": semantic_index["reference_truncated"],
                "diagnostics": semantic_index["diagnostics"],
            }
            if semantic_index and typescript_authority
            else {
                "enabled": False,
                "engine": "static-pattern-fallback",
                "typescript_version": None,
                "typescript_authority_sha256": None,
                "artifact_digest": None,
                "file_count": 0,
                "symbol_count": 0,
                "resolved_import_count": 0,
                "reference_count": 0,
                "reference_truncated": False,
                "diagnostics": [],
            }
        ),
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


def _semantic_symbol_domain(symbol: dict[str, Any], path: str) -> str:
    kind = symbol["kind"]
    if kind in {"InterfaceDeclaration", "TypeAliasDeclaration", "EnumDeclaration"}:
        return "Type"
    if kind == "ClassDeclaration":
        return "Component" if symbol["name"][:1].isupper() else "Symbol"
    return _symbol_domain(symbol["name"], path)


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
            binding for binding in graph["runtime_bindings"] if binding["source_path"] in changed
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
                            and str(component["name"]).casefold() in node["path"].casefold()
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
            "affected": {key: sorted(set(values)) for key, values in affected.items()},
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
                item for item in graph["runtime_bindings"] if item["runtime_node_id"] in candidates
            ]
            selectors = [
                node
                for node in graph["nodes"]
                if node["domain_type"] == "CSSSelector" and node.get("selector") in candidates
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

    def source_to_pixel_trace(
        self,
        code_capture_id: str,
        *,
        source_path: str,
        symbol: str | None = None,
    ) -> dict[str, Any]:
        path = Path(source_path)
        if path.is_absolute() or ".." in path.parts:
            raise ValueError("source trace path must be confined")
        normalized = path.as_posix()
        graph = self.query.graph(code_capture_id, "CodeGraph")
        source_nodes = [
            node
            for node in graph["nodes"]
            if node.get("path") == normalized
            and (symbol is None or node.get("name") == symbol)
            and node["domain_type"] != "RuntimeBinding"
        ]
        source_ids = {node["id"] for node in source_nodes}
        bindings = [
            binding
            for binding in graph["runtime_bindings"]
            if binding.get("source_node_id") in source_ids
            or (not symbol and binding.get("source_path") == normalized)
        ]
        runtime_ids = {binding["runtime_node_id"] for binding in bindings}
        capture_ids = sorted(
            {
                *graph["linked_capture_ids"],
                *(binding["capture_id"] for binding in bindings if binding.get("capture_id")),
            }
        )
        pixels: list[dict[str, Any]] = []
        runtime_observations: list[dict[str, Any]] = []
        citations = [graph["citation"]]
        for capture_id in capture_ids:
            for graph_type in self.query.graph_types(capture_id):
                observed = self.query.graph(capture_id, graph_type)
                matches = [
                    node
                    for node in observed.get("nodes", [])
                    if node.get("id") in runtime_ids
                    or node.get("selector") in runtime_ids
                    or node.get("sourceBinding", {}).get("path") == normalized
                ]
                if not matches:
                    continue
                citations.append(observed["citation"])
                for node in matches:
                    observation = {
                        "capture_id": capture_id,
                        "graph_type": graph_type,
                        "runtime_node_id": node.get("id"),
                        "selector": node.get("selector"),
                        "authority": node.get("authority", observed.get("authority")),
                        "evidence_references": node.get("evidence_references", []),
                    }
                    runtime_observations.append(observation)
                    bounds = node.get("spatial_bounds") or node.get("bounds")
                    if bounds:
                        pixels.append(
                            {
                                **observation,
                                "coordinate_space": observed.get(
                                    "coordinate_space", "runtime CSS pixels"
                                ),
                                "bounds": bounds,
                            }
                        )
        if not bindings:
            authority = "HYPOTHESIS"
            resumption = (
                f"Instrument {normalized}"
                + (f"::{symbol}" if symbol else "")
                + " and bind it to a captured runtime node."
            )
        elif not runtime_observations:
            authority = "DERIVED"
            resumption = (
                "Capture the bound runtime node in a governed layout, interaction, or "
                "graphics observation."
            )
        else:
            authority = (
                "OBSERVED"
                if all(binding["authority"] == "OBSERVED" for binding in bindings)
                else "MIXED"
            )
            resumption = None
        return {
            "schema": "vision.source-pixel-trace/v1",
            "code_capture_id": code_capture_id,
            "source_path": normalized,
            "symbol": symbol,
            "source_nodes": source_nodes,
            "runtime_bindings": bindings,
            "runtime_observations": runtime_observations,
            "pixel_regions": pixels,
            "authority": authority,
            "exact_resumption_contract": resumption,
            "citations": sorted(
                {canonical_json(citation).decode(): citation for citation in citations}.values(),
                key=lambda item: canonical_json(item),
            ),
        }

    def event_to_source_trace(
        self,
        code_capture_id: str,
        *,
        event_capture_id: str,
        event_edge_id: str,
    ) -> dict[str, Any]:
        code = self.query.graph(code_capture_id, "CodeGraph")
        observed_graph = None
        event = None
        for graph_type in ("InteractionGraph", "StateGraph"):
            try:
                candidate = self.query.graph(event_capture_id, graph_type)
            except KeyError:
                continue
            match = next(
                (edge for edge in candidate.get("edges", []) if edge.get("id") == event_edge_id),
                None,
            )
            if match:
                observed_graph = candidate
                event = match
                break
        if event is None or observed_graph is None:
            raise KeyError(f"unknown observed event edge: {event_edge_id}")
        runtime_candidates = {
            value
            for value in (
                event.get("event_target"),
                event.get("source"),
                event.get("target"),
            )
            if value
        }
        bindings = [
            binding
            for binding in code["runtime_bindings"]
            if binding["runtime_node_id"] in runtime_candidates
        ]
        source_ids = {
            binding["source_node_id"] for binding in bindings if binding.get("source_node_id")
        }
        sources = [node for node in code["nodes"] if node["id"] in source_ids]
        authority = (
            "OBSERVED"
            if sources
            and event.get("authority") == "OBSERVED"
            and all(binding["authority"] == "OBSERVED" for binding in bindings)
            else "HYPOTHESIS"
        )
        return {
            "schema": "vision.event-source-trace/v1",
            "code_capture_id": code_capture_id,
            "event_capture_id": event_capture_id,
            "event": event,
            "runtime_candidates": sorted(runtime_candidates),
            "runtime_bindings": bindings,
            "source_nodes": sources,
            "authority": authority,
            "exact_resumption_contract": (
                None
                if authority == "OBSERVED"
                else "Bind the observed event target to a compiler-indexed source symbol."
            ),
            "citations": [observed_graph["citation"], code["citation"]],
        }

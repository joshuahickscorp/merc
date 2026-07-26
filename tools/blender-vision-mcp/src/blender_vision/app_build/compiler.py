from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import tempfile
import uuid
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Literal

from pydantic import Field

from blender_vision.app_build.completeness import ReferenceCompletenessAnalyzer
from blender_vision.app_build.specification import (
    ApplicationReferencePacket,
    ReferenceCompletenessReport,
    StrictModel,
)
from blender_vision.app_build.templates import (
    APP_TS,
    CLEAN_MJS,
    COMPOSE_YAML,
    CONTRACT_TEST_TS,
    COPY_STATIC_MJS,
    DATABASE_TS,
    DOCKERFILE,
    DOCKERIGNORE,
    INDEX_HTML,
    MIGRATE_TS,
    ROLLBACK_TS,
    SERVER_TS,
    STYLES_CSS,
    frontend_ts,
    generated_api_tests,
    generated_crud_tests,
    generated_upload_tests,
    package_json,
    tsconfig_json,
)
from blender_vision.core.util import sha256_file

GENERATOR_VERSION = "bounded-typescript-node-sqlite-v1"
IDENTIFIER = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
CANDIDATE_ID = re.compile(r"^[a-z0-9][a-z0-9-]{1,62}$")


class CompilationError(ValueError):
    pass


class GeneratedFileReceipt(StrictModel):
    path: str
    sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    size: int = Field(ge=0)


class ApplicationCandidateReceipt(StrictModel):
    schema_version: Literal["1"] = "1"
    candidate_id: str
    mode: Literal["draft", "promotion_candidate"]
    generator_version: str
    source_git_head: str = Field(pattern=r"^[0-9a-f]{40}$")
    packet_id: str
    packet_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    completeness_report: ReferenceCompletenessReport
    promotable: bool
    generated_at: str
    files: list[GeneratedFileReceipt]
    reproduction_commands: list[str]


class CandidateVerification(StrictModel):
    valid: bool
    candidate_id: str
    missing_files: list[str]
    changed_files: list[dict[str, str]]
    packet_digest_valid: bool
    receipt_path: str


def _git_head(project_root: Path) -> str:
    configured = os.environ.get("BVMCP_GIT_HEAD")
    if configured:
        if not re.fullmatch(r"[0-9a-f]{40}", configured):
            raise CompilationError("BVMCP_GIT_HEAD must be a full lowercase Git SHA")
        return configured
    process = subprocess.run(
        ["git", "-C", str(project_root), "rev-parse", "HEAD"],
        check=False,
        capture_output=True,
        text=True,
    )
    head = process.stdout.strip()
    if process.returncode != 0 or not re.fullmatch(r"[0-9a-f]{40}", head):
        raise CompilationError("application compilation requires Git authority")
    return head


def _json(value: Any) -> str:
    return json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False) + "\n"


def _typescript_constant(name: str, value: Any) -> str:
    return f"export const {name} = {_json(value).rstrip()} as const;\n"


def _quote(identifier: str) -> str:
    if not IDENTIFIER.fullmatch(identifier):
        raise CompilationError(f"unsafe SQL identifier: {identifier!r}")
    return f'"{identifier}"'


def _sql_type(data_type: str) -> str:
    return {
        "string": "TEXT",
        "integer": "INTEGER",
        "number": "REAL",
        "boolean": "INTEGER",
        "datetime": "TEXT",
        "json": "TEXT",
        "binary": "BLOB",
        "uuid": "TEXT",
    }[data_type]


def _schema_sql(packet: ApplicationReferencePacket) -> tuple[str, str]:
    up: list[str] = [
        "BEGIN;",
        "CREATE TABLE IF NOT EXISTS schema_migrations (",
        "  version TEXT PRIMARY KEY,",
        "  applied_at TEXT NOT NULL",
        ");",
        "CREATE TABLE IF NOT EXISTS idempotency_keys (",
        "  operation_id TEXT NOT NULL,",
        "  scope TEXT NOT NULL,",
        '  "key" TEXT NOT NULL,',
        "  request_sha256 TEXT NOT NULL,",
        "  response_json TEXT NOT NULL,",
        "  expires_at TEXT NOT NULL,",
        '  PRIMARY KEY (operation_id, scope, "key")',
        ");",
    ]
    entities = {entity.name: entity for entity in packet.data_model.entities}
    relations_by_entity: dict[str, list[Any]] = {}
    for relation in packet.data_model.relations:
        relations_by_entity.setdefault(relation.from_entity, []).append(relation)
    for entity in packet.data_model.entities:
        columns: list[str] = []
        fields = {field.name: field for field in entity.fields}
        for field in entity.fields:
            constraints = [_quote(field.name), _sql_type(field.data_type)]
            if field.primary_key:
                constraints.append("PRIMARY KEY")
            if not field.nullable:
                constraints.append("NOT NULL")
            if field.unique and not field.primary_key:
                constraints.append("UNIQUE")
            columns.append(" ".join(constraints))
        for relation in relations_by_entity.get(entity.name, []):
            target = entities.get(relation.to_entity)
            if relation.from_field not in fields or not target:
                raise CompilationError(f"invalid relation {relation.id}")
            target_fields = {field.name for field in target.fields}
            if relation.to_field not in target_fields:
                raise CompilationError(f"invalid relation target {relation.id}")
            action = {
                "restrict": "RESTRICT",
                "cascade": "CASCADE",
                "set_null": "SET NULL",
            }[relation.on_delete]
            columns.append(
                f"FOREIGN KEY ({_quote(relation.from_field)}) REFERENCES "
                f"{_quote(target.table_name)} ({_quote(relation.to_field)}) "
                f"ON DELETE {action}"
            )
        up.append(
            f"CREATE TABLE IF NOT EXISTS {_quote(entity.table_name)} (\n  "
            + ",\n  ".join(columns)
            + "\n);"
        )
        for index_number, index in enumerate(entity.indexes, start=1):
            index_name = f"idx_{entity.table_name}_{index_number}"
            columns_sql = ", ".join(_quote(field) for field in index)
            up.append(
                f"CREATE INDEX IF NOT EXISTS {_quote(index_name)} "
                f"ON {_quote(entity.table_name)} ({columns_sql});"
            )
    up.extend(
        [
            "INSERT OR IGNORE INTO schema_migrations(version, applied_at) "
            "VALUES ('001_initial', CURRENT_TIMESTAMP);",
            "COMMIT;",
        ]
    )

    down = ["BEGIN;"]
    for entity in reversed(packet.data_model.entities):
        down.append(f"DROP TABLE IF EXISTS {_quote(entity.table_name)};")
    down.extend(
        [
            "DROP TABLE IF EXISTS idempotency_keys;",
            "DROP TABLE IF EXISTS schema_migrations;",
            "COMMIT;",
        ]
    )
    return "\n".join(up) + "\n", "\n".join(down) + "\n"


def _openapi(packet: ApplicationReferencePacket) -> dict[str, Any]:
    paths: dict[str, dict[str, Any]] = {}
    for endpoint in packet.api_contract.endpoints:
        operation = {
            "operationId": endpoint.operation_id,
            "summary": endpoint.summary,
            "responses": {
                str(response.status): {
                    "description": response.description,
                }
                for response in endpoint.responses
            },
            "x-visionmcp-handler": endpoint.handler.model_dump(mode="json"),
            "x-visionmcp-business-rules": endpoint.business_rule_ids,
        }
        paths.setdefault(endpoint.path, {})[endpoint.method.lower()] = operation
    return {
        "openapi": "3.1.0",
        "info": {
            "title": packet.product.name,
            "version": packet.api_contract.version,
        },
        "servers": [{"url": packet.api_contract.base_path}],
        "paths": paths,
    }


def _read_lockfile() -> str:
    path = Path(__file__).resolve().parent / "assets" / "package-lock.json"
    if not path.is_file():
        raise CompilationError(f"pinned package lock is missing: {path}")
    return path.read_text(encoding="utf-8")


def _readme(packet: ApplicationReferencePacket, candidate_id: str) -> str:
    return f"""# {packet.product.name}

Generated by VisionMCP `{GENERATOR_VERSION}` as candidate `{candidate_id}`.

This candidate is governed by:

- `.visionmcp/application-reference-packet.json`
- `.visionmcp/reference-completeness-report.json`
- `.visionmcp/candidate-receipt.json`

## Reproduce

```bash
npm ci
npm run verify
npm run db:migrate
npm start
```

The generated `test_header` identity provider is confined to declared local benchmark targets.
Replace it with a separately authorized provider before changing the deployment reference class.
"""


class BoundedApplicationCompiler:
    def __init__(self, workspace_root: Path):
        self.workspace_root = workspace_root.expanduser().resolve()
        self.workspace_root.mkdir(parents=True, exist_ok=True)
        if self.workspace_root.is_symlink():
            raise CompilationError("compiler workspace root cannot be a symlink")

    def _validate_support(
        self,
        packet: ApplicationReferencePacket,
        report: ReferenceCompletenessReport,
        mode: str,
    ) -> None:
        if not report.compilable_as_draft:
            raise CompilationError(
                "reference packet cannot compile: " + "; ".join(report.exact_resumption_contracts)
            )
        if mode == "promotion_candidate" and not report.promotable:
            raise CompilationError(
                "promotion candidate requires complete authority: "
                + "; ".join(report.exact_resumption_contracts)
            )
        if packet.data_model.database_engine != "sqlite":
            raise CompilationError("bounded compiler v1 supports declared SQLite targets only")
        if packet.api_contract.protocol != "REST":
            raise CompilationError("bounded compiler v1 supports REST contracts only")
        if packet.auth_policy.provider not in {"none", "test_header"}:
            raise CompilationError(
                "bounded compiler v1 requires an implemented auth-provider adapter"
            )
        if packet.auth_policy.provider == "none" and any(
            endpoint.authorization != "public" for endpoint in packet.api_contract.endpoints
        ):
            raise CompilationError("secured endpoints require an explicit auth provider")
        if packet.auth_policy.tenant_claim:
            raise CompilationError(
                "bounded compiler v1 does not claim cross-tenant enforcement without an adapter"
            )
        if packet.deployment.target not in {"local_process", "local_container"}:
            raise CompilationError(
                "bounded compiler v1 supports local process/container targets only"
            )

    def _files(
        self,
        packet: ApplicationReferencePacket,
        report: ReferenceCompletenessReport,
        candidate_id: str,
    ) -> dict[str, str]:
        up_sql, down_sql = _schema_sql(packet)
        specification = packet.model_dump(mode="json")
        contract_tests = (
            CONTRACT_TEST_TS
            + "\n"
            + generated_api_tests(specification)
            + "\n"
            + generated_upload_tests(specification)
            + "\n"
            + generated_crud_tests(specification)
        )
        return {
            "package.json": package_json("visionmcp-generated-application"),
            "package-lock.json": _read_lockfile(),
            "tsconfig.json": tsconfig_json(),
            ".env.example": (
                "PORT=3000\nDATABASE_PATH=data/application.sqlite3\nUPLOAD_ROOT=data\n"
            ),
            ".dockerignore": DOCKERIGNORE,
            "Dockerfile": DOCKERFILE,
            "compose.yaml": COMPOSE_YAML,
            "README.md": _readme(packet, candidate_id),
            "src/generated-spec.ts": _typescript_constant("SPEC", specification),
            "src/schema.ts": (
                f"export const UP_SQL = {json.dumps(up_sql)};\n"
                f"export const DOWN_SQL = {json.dumps(down_sql)};\n"
            ),
            "src/database.ts": DATABASE_TS,
            "src/app.ts": APP_TS,
            "src/server.ts": SERVER_TS,
            "scripts/migrate.ts": MIGRATE_TS,
            "scripts/rollback.ts": ROLLBACK_TS,
            "scripts/clean.mjs": CLEAN_MJS,
            "scripts/copy-static.mjs": COPY_STATIC_MJS,
            "frontend/src/app.ts": frontend_ts(specification),
            "public/index.html": INDEX_HTML,
            "public/styles.css": STYLES_CSS,
            "tests/contract.test.ts": contract_tests,
            "generated/openapi.json": _json(_openapi(packet)),
            "generated/migrations/001_up.sql": up_sql,
            "generated/migrations/001_down.sql": down_sql,
            ".visionmcp/application-reference-packet.json": _json(specification),
            ".visionmcp/reference-completeness-report.json": _json(report.model_dump(mode="json")),
        }

    def _write_files(self, root: Path, files: dict[str, str]) -> list[GeneratedFileReceipt]:
        receipts: list[GeneratedFileReceipt] = []
        for relative_path, content in sorted(files.items()):
            candidate = (root / relative_path).resolve()
            if not candidate.is_relative_to(root.resolve()):
                raise CompilationError(f"generated path escaped candidate root: {relative_path}")
            candidate.parent.mkdir(parents=True, exist_ok=True)
            candidate.write_text(content, encoding="utf-8")
            digest, size = sha256_file(candidate)
            receipts.append(GeneratedFileReceipt(path=relative_path, sha256=digest, size=size))
        return receipts

    def _preserve_failure(self, temporary: Path, candidate_id: str, error: Exception) -> Path:
        failed_root = self.workspace_root / "failed"
        failed_root.mkdir(parents=True, exist_ok=True)
        destination = failed_root / f"{candidate_id}-{uuid.uuid4().hex[:12]}"
        (temporary / ".visionmcp").mkdir(parents=True, exist_ok=True)
        (temporary / ".visionmcp" / "failure.json").write_text(
            _json(
                {
                    "candidate_id": candidate_id,
                    "error_type": type(error).__name__,
                    "error": str(error),
                    "preserved_at": datetime.now(UTC).isoformat(),
                }
            ),
            encoding="utf-8",
        )
        os.replace(temporary, destination)
        return destination

    def compile(
        self,
        packet: ApplicationReferencePacket,
        *,
        candidate_id: str,
        mode: Literal["draft", "promotion_candidate"] = "draft",
    ) -> ApplicationCandidateReceipt:
        if not CANDIDATE_ID.fullmatch(candidate_id):
            raise CompilationError(
                "candidate_id must be 2-63 lowercase alphanumeric/hyphen characters"
            )
        destination = self.workspace_root / candidate_id
        if destination.exists():
            raise CompilationError(f"candidate destination already exists: {destination}")
        report = ReferenceCompletenessAnalyzer().analyze(packet)
        self._validate_support(packet, report, mode)
        temporary = Path(
            tempfile.mkdtemp(prefix=f".{candidate_id}-", dir=self.workspace_root)
        ).resolve()
        try:
            files = self._files(packet, report, candidate_id)
            file_receipts = self._write_files(temporary, files)
            project_root = Path(__file__).resolve().parents[3]
            receipt = ApplicationCandidateReceipt(
                candidate_id=candidate_id,
                mode=mode,
                generator_version=GENERATOR_VERSION,
                source_git_head=_git_head(project_root),
                packet_id=packet.packet_id,
                packet_sha256=packet.canonical_digest(),
                completeness_report=report,
                promotable=mode == "promotion_candidate" and report.promotable,
                generated_at=datetime.now(UTC).isoformat(),
                files=file_receipts,
                reproduction_commands=[
                    "npm ci",
                    "npm run verify",
                    "npm run db:migrate",
                    "npm run db:rollback",
                    "docker compose build",
                ],
            )
            (temporary / ".visionmcp" / "candidate-receipt.json").write_text(
                _json(receipt.model_dump(mode="json")),
                encoding="utf-8",
            )
            verification = self.verify_candidate(temporary)
            if not verification.valid:
                raise CompilationError(f"generated candidate failed verification: {verification}")
            os.replace(temporary, destination)
            return receipt
        except Exception as error:
            if temporary.exists():
                preserved = self._preserve_failure(temporary, candidate_id, error)
                raise CompilationError(
                    f"application compilation failed; attempt preserved at {preserved}: {error}"
                ) from error
            raise

    def verify_candidate(self, candidate_root: Path) -> CandidateVerification:
        root = candidate_root.expanduser().resolve()
        receipt_path = root / ".visionmcp" / "candidate-receipt.json"
        if not receipt_path.is_file():
            raise CompilationError(f"candidate receipt is missing: {receipt_path}")
        receipt = ApplicationCandidateReceipt.model_validate_json(
            receipt_path.read_text(encoding="utf-8")
        )
        missing: list[str] = []
        changed: list[dict[str, str]] = []
        for record in receipt.files:
            path = (root / record.path).resolve()
            if not path.is_relative_to(root):
                changed.append(
                    {
                        "path": record.path,
                        "expected": record.sha256,
                        "actual": "PATH_ESCAPE",
                    }
                )
                continue
            if not path.is_file():
                missing.append(record.path)
                continue
            actual, _size = sha256_file(path)
            if actual != record.sha256:
                changed.append(
                    {
                        "path": record.path,
                        "expected": record.sha256,
                        "actual": actual,
                    }
                )
        packet_path = root / ".visionmcp" / "application-reference-packet.json"
        packet_digest_valid = False
        if packet_path.is_file():
            packet = ApplicationReferencePacket.model_validate_json(
                packet_path.read_text(encoding="utf-8")
            )
            packet_digest_valid = packet.canonical_digest() == receipt.packet_sha256
        return CandidateVerification(
            valid=not missing and not changed and packet_digest_valid,
            candidate_id=receipt.candidate_id,
            missing_files=missing,
            changed_files=changed,
            packet_digest_valid=packet_digest_valid,
            receipt_path=str(receipt_path),
        )

    def copy_candidate(self, source: Path, destination: Path) -> None:
        """Copy only after receipt verification; used by fresh-clone benchmark setup."""
        verification = self.verify_candidate(source)
        if not verification.valid:
            raise CompilationError("refusing to copy an invalid application candidate")
        target = destination.expanduser().resolve()
        if target.exists():
            raise CompilationError(f"copy destination already exists: {target}")
        shutil.copytree(source, target, symlinks=False)

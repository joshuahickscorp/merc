from __future__ import annotations

import json
from typing import Any

PACKAGE_VERSIONS = {
    "better-sqlite3": "12.11.1",
    "express": "5.2.1",
    "@types/better-sqlite3": "7.6.13",
    "@types/express": "5.0.6",
    "@types/node": "26.1.1",
    "esbuild": "0.28.1",
    "typescript": "7.0.2",
}


def package_json(name: str) -> str:
    document = {
        "name": name,
        "version": "1.0.0",
        "private": True,
        "type": "module",
        "engines": {"node": ">=20 <21"},
        "scripts": {
            "clean": "node scripts/clean.mjs",
            "build": (
                "npm run clean && tsc -p tsconfig.json && "
                "esbuild frontend/src/app.ts --bundle --minify "
                "--outfile=dist/public/app.js && node scripts/copy-static.mjs"
            ),
            "start": "node dist/src/server.js",
            "db:migrate": "npm run build && node dist/scripts/migrate.js",
            "db:rollback": "npm run build && node dist/scripts/rollback.js",
            "test:contract": "npm run build && node --test dist/tests/*.test.js",
            "test": "npm run test:contract",
            "verify": "npm run test",
        },
        "dependencies": {
            "better-sqlite3": PACKAGE_VERSIONS["better-sqlite3"],
            "express": PACKAGE_VERSIONS["express"],
        },
        "devDependencies": {
            "@types/better-sqlite3": PACKAGE_VERSIONS["@types/better-sqlite3"],
            "@types/express": PACKAGE_VERSIONS["@types/express"],
            "@types/node": PACKAGE_VERSIONS["@types/node"],
            "esbuild": PACKAGE_VERSIONS["esbuild"],
            "typescript": PACKAGE_VERSIONS["typescript"],
        },
    }
    return json.dumps(document, indent=2, sort_keys=True) + "\n"


def tsconfig_json() -> str:
    document = {
        "compilerOptions": {
            "target": "ES2023",
            "module": "NodeNext",
            "moduleResolution": "NodeNext",
            "rootDir": ".",
            "outDir": "dist",
            "strict": True,
            "noUncheckedIndexedAccess": True,
            "exactOptionalPropertyTypes": True,
            "esModuleInterop": True,
            "forceConsistentCasingInFileNames": True,
            "skipLibCheck": True,
            "declaration": True,
            "sourceMap": True,
        },
        "include": ["src/**/*.ts", "tests/**/*.ts", "scripts/**/*.ts"],
    }
    return json.dumps(document, indent=2, sort_keys=True) + "\n"


DATABASE_TS = r"""
import Database from "better-sqlite3";
import { mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { DOWN_SQL, UP_SQL } from "./schema.js";

export type AppDatabase = Database.Database;

export function openDatabase(databasePath?: string): AppDatabase {
  const target = resolve(databasePath ?? process.env.DATABASE_PATH ?? "data/application.sqlite3");
  mkdirSync(dirname(target), { recursive: true });
  const database = new Database(target);
  database.pragma("foreign_keys = ON");
  database.pragma("journal_mode = WAL");
  database.exec(UP_SQL);
  return database;
}

export function rollbackDatabase(database: AppDatabase): void {
  database.exec(DOWN_SQL);
}
""".lstrip()


APP_TS = r"""
import express, {
  type ErrorRequestHandler,
  type Express,
  type NextFunction,
  type Request,
  type Response,
} from "express";
import { createHash, randomUUID } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import type { AppDatabase } from "./database.js";
import { openDatabase } from "./database.js";
import { SPEC } from "./generated-spec.js";

type Row = Record<string, unknown>;

interface Endpoint {
  operation_id: string;
  method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  path: string;
  authorization: "public" | "authenticated" | "permission";
  required_permissions: readonly string[];
  request_fields: readonly {
    name: string;
    location: string;
    data_type: string;
    required: boolean;
    validation: Readonly<Record<string, unknown>>;
  }[];
  responses: readonly { status: number }[];
  handler: {
    kind:
      | "list_entities"
      | "get_entity"
      | "create_entity"
      | "update_entity"
      | "delete_entity"
      | "idempotent_create"
      | "file_upload"
      | "status_lookup";
    entity_ref: string;
    id_field: string;
    storage_subdirectory: string | null;
    field_bindings: Readonly<Record<string, string>>;
  };
  idempotency: {
    key_header: string;
    scope: "actor" | "tenant" | "global";
    replay_status: number;
    conflict_status: number;
    retention_seconds: number;
  } | null;
  file_boundary: {
    allowed_content_types: readonly string[];
    maximum_bytes: number;
  } | null;
}

interface Entity {
  name: string;
  table_name: string;
  fields: readonly {
    name: string;
    data_type: string;
    nullable: boolean;
    primary_key: boolean;
    default: unknown;
  }[];
}

const ENDPOINTS = SPEC.api_contract.endpoints as unknown as readonly Endpoint[];
const ENTITIES = SPEC.data_model.entities as unknown as readonly Entity[];

export interface AppOptions {
  databasePath?: string;
  uploadRoot?: string;
  log?: (event: Record<string, unknown>) => void;
}

export interface BuiltApplication {
  app: Express;
  database: AppDatabase;
}

function entityFor(endpoint: Endpoint): Entity {
  const entity = ENTITIES.find(
    (candidate) => candidate.name === endpoint.handler.entity_ref,
  );
  if (!entity) {
    throw new Error(`compiled endpoint ${endpoint.operation_id} has no entity`);
  }
  return entity;
}

function authorize(endpoint: Endpoint, request: Request, response: Response): boolean {
  if (endpoint.authorization === "public") {
    return true;
  }
  const user = request.header("x-test-user");
  if (!user) {
    response.status(401).json({
      code: "AUTHENTICATION_REQUIRED",
      message: "Authentication is required.",
      request_id: response.locals.requestId,
    });
    return false;
  }
  if (endpoint.authorization !== "permission") {
    return true;
  }
  const roleId = request.header("x-test-role") ?? SPEC.auth_policy.default_role;
  const role = SPEC.auth_policy.roles.find((candidate) => candidate.id === roleId);
  const granted = new Set<string>(role?.permission_ids ?? []);
  const allowed = endpoint.required_permissions.every((permission) => granted.has(permission));
  if (!allowed) {
    response.status(403).json({
      code: "PERMISSION_DENIED",
      message: "The authenticated actor is not authorized.",
      request_id: response.locals.requestId,
    });
  }
  return allowed;
}

function validateBody(endpoint: Endpoint, request: Request): string[] {
  const errors: string[] = [];
  const body = (request.body ?? {}) as Record<string, unknown>;
  for (const field of endpoint.request_fields.filter((item) => item.location === "body")) {
    const value = body[field.name];
    if (field.required && (value === undefined || value === null || value === "")) {
      errors.push(`${field.name} is required`);
      continue;
    }
    if (value === undefined || value === null) {
      continue;
    }
    const validation = field.validation as Record<string, unknown>;
    if (typeof value === "string") {
      const minimum = validation.minimum_length;
      const maximum = validation.maximum_length;
      if (typeof minimum === "number" && value.length < minimum) {
        errors.push(`${field.name} is shorter than ${minimum}`);
      }
      if (typeof maximum === "number" && value.length > maximum) {
        errors.push(`${field.name} is longer than ${maximum}`);
      }
    }
  }
  return errors;
}

function createRow(database: AppDatabase, endpoint: Endpoint, request: Request): Row {
  const entity = entityFor(endpoint);
  const body = (request.body ?? {}) as Record<string, unknown>;
  const bindings = endpoint.handler.field_bindings as Record<string, string>;
  const row: Row = {};
  for (const field of entity.fields) {
    const requestName = Object.entries(bindings).find(([, target]) => target === field.name)?.[0];
    if (requestName && body[requestName] !== undefined) {
      row[field.name] = body[requestName];
    } else if (field.primary_key) {
      row[field.name] = randomUUID();
    } else if (field.default !== null && field.default !== undefined) {
      row[field.name] = field.default;
    } else if (field.data_type === "datetime" && field.name.includes("created")) {
      row[field.name] = new Date().toISOString();
    } else if (!field.nullable) {
      throw new Error(`required field ${entity.name}.${field.name} has no authoritative binding`);
    } else {
      row[field.name] = null;
    }
  }
  const columns = Object.keys(row);
  const quoted = columns.map((column) => `"${column}"`).join(", ");
  const parameters = columns.map((column) => `@${column}`).join(", ");
  database
    .prepare(`INSERT INTO "${entity.table_name}" (${quoted}) VALUES (${parameters})`)
    .run(row);
  return row;
}

function requestDigest(request: Request): string {
  return createHash("sha256")
    .update(JSON.stringify(request.body ?? null))
    .digest("hex");
}

function successfulStatus(endpoint: Endpoint, fallback: number): number {
  return endpoint.responses.find((response) => response.status >= 200 && response.status < 300)
    ?.status ?? fallback;
}

function handleEndpoint(
  database: AppDatabase,
  endpoint: Endpoint,
  request: Request,
  response: Response,
  uploadRoot: string,
): void {
  if (!authorize(endpoint, request, response)) {
    return;
  }
  const handler = endpoint.handler;
  const entity = entityFor(endpoint);
  if (handler.kind === "list_entities") {
    const rows = database.prepare(`SELECT * FROM "${entity.table_name}"`).all();
    response.status(successfulStatus(endpoint, 200)).json(rows);
    return;
  }
  if (handler.kind === "get_entity" || handler.kind === "status_lookup") {
    const identifier = request.params[handler.id_field] ?? request.params.id;
    const row = database
      .prepare(
        `SELECT * FROM "${entity.table_name}" WHERE "${handler.id_field}" = ? LIMIT 1`,
      )
      .get(identifier);
    if (!row) {
      response.status(404).json({
        code: "NOT_FOUND",
        message: "The requested record does not exist.",
        request_id: response.locals.requestId,
      });
      return;
    }
    response.status(successfulStatus(endpoint, 200)).json(row);
    return;
  }
  if (handler.kind === "file_upload") {
    const boundary = endpoint.file_boundary;
    if (!boundary) {
      throw new Error("compiled upload endpoint has no file boundary");
    }
    const contentType = request.header("content-type")?.split(";")[0] ?? "";
    if (!boundary.allowed_content_types.includes(contentType)) {
      response.status(415).json({
        code: "UNSUPPORTED_MEDIA_TYPE",
        message: "The uploaded content type is not allowed.",
        request_id: response.locals.requestId,
      });
      return;
    }
    const bytes = Buffer.isBuffer(request.body) ? request.body : Buffer.alloc(0);
    if (bytes.length > boundary.maximum_bytes) {
      response.status(413).json({
        code: "FILE_TOO_LARGE",
        message: "The uploaded file exceeds the configured limit.",
        request_id: response.locals.requestId,
      });
      return;
    }
    const identifier = randomUUID();
    const directory = resolve(uploadRoot, handler.storage_subdirectory ?? "uploads");
    mkdirSync(directory, { recursive: true });
    const target = resolve(directory, identifier);
    if (!target.startsWith(`${directory}/`)) {
      throw new Error("upload path escaped the governed root");
    }
    writeFileSync(target, bytes, { flag: "wx" });
    request.body = {
      id: identifier,
      content_type: contentType,
      size_bytes: bytes.length,
      storage_path: target,
    };
    const row = createRow(database, endpoint, request);
    response.status(successfulStatus(endpoint, 201)).json(row);
    return;
  }
  const validationErrors = validateBody(endpoint, request);
  if (validationErrors.length) {
    response.status(422).json({
      code: "VALIDATION_ERROR",
      message: "The request did not satisfy the declared contract.",
      fields: validationErrors,
      request_id: response.locals.requestId,
    });
    return;
  }
  if (handler.kind === "create_entity") {
    const row = database.transaction(() => createRow(database, endpoint, request))();
    response.status(successfulStatus(endpoint, 201)).json(row);
    return;
  }
  if (handler.kind === "idempotent_create") {
    const contract = endpoint.idempotency;
    if (!contract) {
      throw new Error("compiled idempotent endpoint has no contract");
    }
    const key = request.header(contract.key_header);
    if (!key) {
      response.status(400).json({
        code: "IDEMPOTENCY_KEY_REQUIRED",
        message: `${contract.key_header} is required.`,
        request_id: response.locals.requestId,
      });
      return;
    }
    const digest = requestDigest(request);
    const scope = contract.scope === "global"
      ? "global"
      : request.header("x-test-user") ?? "anonymous";
    const existing = database
      .prepare(
        "SELECT request_sha256, response_json FROM idempotency_keys "
          + "WHERE operation_id = ? AND scope = ? AND key = ?",
      )
      .get(endpoint.operation_id, scope, key) as
      | { request_sha256: string; response_json: string }
      | undefined;
    if (existing) {
      if (existing.request_sha256 !== digest) {
        response.status(contract.conflict_status).json({
          code: "IDEMPOTENCY_CONFLICT",
          message: "The key was already used for a different request.",
          request_id: response.locals.requestId,
        });
        return;
      }
      response.status(contract.replay_status).json(JSON.parse(existing.response_json));
      return;
    }
    const row = database.transaction(() => {
      const created = createRow(database, endpoint, request);
      database
        .prepare(
          "INSERT INTO idempotency_keys "
            + "(operation_id, scope, key, request_sha256, response_json, expires_at) "
            + "VALUES (?, ?, ?, ?, ?, ?)",
        )
        .run(
          endpoint.operation_id,
          scope,
          key,
          digest,
          JSON.stringify(created),
          new Date(Date.now() + contract.retention_seconds * 1000).toISOString(),
        );
      return created;
    })();
    response.status(successfulStatus(endpoint, 201)).json(row);
    return;
  }
  if (handler.kind === "update_entity") {
    const body = (request.body ?? {}) as Record<string, unknown>;
    const updates: Row = {};
    for (const [requestField, column] of Object.entries(handler.field_bindings)) {
      if (body[requestField] !== undefined) {
        updates[column] = body[requestField];
      }
    }
    const columns = Object.keys(updates);
    if (!columns.length) {
      response.status(422).json({
        code: "VALIDATION_ERROR",
        message: "No declared update fields were supplied.",
        request_id: response.locals.requestId,
      });
      return;
    }
    const identifier = request.params[handler.id_field] ?? request.params.id;
    const assignments = columns.map((column) => `"${column}" = @${column}`).join(", ");
    const result = database
      .prepare(
        `UPDATE "${entity.table_name}" SET ${assignments} `
          + `WHERE "${handler.id_field}" = @__identifier`,
      )
      .run({ ...updates, __identifier: identifier });
    if (!result.changes) {
      response.status(404).json({
        code: "NOT_FOUND",
        message: "The requested record does not exist.",
        request_id: response.locals.requestId,
      });
      return;
    }
    const row = database
      .prepare(
        `SELECT * FROM "${entity.table_name}" WHERE "${handler.id_field}" = ? LIMIT 1`,
      )
      .get(identifier);
    response.status(successfulStatus(endpoint, 200)).json(row);
    return;
  }
  if (handler.kind === "delete_entity") {
    const identifier = request.params[handler.id_field] ?? request.params.id;
    const result = database
      .prepare(
        `DELETE FROM "${entity.table_name}" WHERE "${handler.id_field}" = ?`,
      )
      .run(identifier);
    if (!result.changes) {
      response.status(404).json({
        code: "NOT_FOUND",
        message: "The requested record does not exist.",
        request_id: response.locals.requestId,
      });
      return;
    }
    response.status(successfulStatus(endpoint, 204)).end();
    return;
  }
  response.status(501).json({
    code: "HANDLER_NOT_IMPLEMENTED",
    message: `The bounded compiler does not implement ${handler.kind}.`,
    request_id: response.locals.requestId,
  });
}

export function createApplication(options: AppOptions = {}): BuiltApplication {
  const app = express();
  const database = openDatabase(options.databasePath);
  const uploadRoot = resolve(options.uploadRoot ?? process.env.UPLOAD_ROOT ?? "data");
  const log = options.log ?? ((event) => console.log(JSON.stringify(event)));

  app.disable("x-powered-by");
  app.use((request: Request, response: Response, next: NextFunction) => {
    const started = performance.now();
    response.locals.requestId = request.header("x-request-id") ?? randomUUID();
    response.setHeader("x-request-id", response.locals.requestId);
    response.on("finish", () => {
      log({
        timestamp: new Date().toISOString(),
        request_id: response.locals.requestId,
        method: request.method,
        path: request.path,
        status: response.statusCode,
        duration_ms: Number((performance.now() - started).toFixed(3)),
      });
    });
    next();
  });
  app.use(express.json({ limit: "1mb" }));
  app.get("/healthz", (_request, response) => {
    const result = database.pragma("quick_check", { simple: true });
    response.status(result === "ok" ? 200 : 503).json({ ok: result === "ok" });
  });

  for (const endpoint of ENDPOINTS) {
    const path = `${SPEC.api_contract.base_path}${endpoint.path}`.replace(
      /\{([^}]+)\}/g,
      ":$1",
    );
    const middleware = endpoint.handler.kind === "file_upload" && endpoint.file_boundary
      ? [
          express.raw({
            type: () => true,
            limit: endpoint.file_boundary.maximum_bytes,
          }),
        ]
      : [];
    const register = (app as unknown as Record<string, ((...args: unknown[]) => void) | undefined>)[
      endpoint.method.toLowerCase()
    ];
    if (!register) {
      throw new Error(`Express does not support method ${endpoint.method}`);
    }
    register.call(
      app,
      path,
      ...middleware,
      (request: Request, response: Response, next: NextFunction) => {
        try {
          handleEndpoint(database, endpoint, request, response, uploadRoot);
        } catch (error) {
          next(error);
        }
      },
    );
  }
  app.use(express.static("dist/public", { fallthrough: true }));
  app.get("*path", (_request, response) => response.sendFile(resolve("dist/public/index.html")));
  const errors: ErrorRequestHandler = (error, _request, response, _next) => {
    const tooLarge = (error as { type?: string }).type === "entity.too.large";
    response.status(tooLarge ? 413 : 500).json({
      code: tooLarge ? "PAYLOAD_TOO_LARGE" : "INTERNAL_ERROR",
      message: tooLarge ? "The payload exceeds the configured limit." : "The request failed.",
      request_id: response.locals.requestId,
    });
  };
  app.use(errors);
  return { app, database };
}
""".lstrip()


SERVER_TS = r"""
import { createApplication } from "./app.js";

const port = Number(process.env.PORT ?? 3000);
const { app } = createApplication();
const server = app.listen(port, "0.0.0.0", () => {
  console.log(JSON.stringify({
    timestamp: new Date().toISOString(),
    event: "server_listening",
    port,
  }));
});

function shutdown(signal: string): void {
  console.log(JSON.stringify({ timestamp: new Date().toISOString(), event: "shutdown", signal }));
  server.close((error) => {
    process.exitCode = error ? 1 : 0;
  });
}

process.on("SIGINT", () => shutdown("SIGINT"));
process.on("SIGTERM", () => shutdown("SIGTERM"));
""".lstrip()


MIGRATE_TS = r"""
import { openDatabase } from "../src/database.js";

const database = openDatabase();
const result = database.pragma("quick_check", { simple: true });
database.close();
if (result !== "ok") {
  throw new Error(`database migration verification failed: ${result}`);
}
console.log(JSON.stringify({ migrated: true, quick_check: result }));
""".lstrip()


ROLLBACK_TS = r"""
import { openDatabase, rollbackDatabase } from "../src/database.js";

const database = openDatabase();
rollbackDatabase(database);
database.close();
console.log(JSON.stringify({ rolled_back: true }));
""".lstrip()


CLEAN_MJS = r"""
import { rmSync } from "node:fs";
import { resolve } from "node:path";

const target = resolve("dist");
if (target === resolve(".") || target === "/") {
  throw new Error("refusing unsafe build cleanup target");
}
rmSync(target, { recursive: true, force: true });
""".lstrip()


COPY_STATIC_MJS = r"""
import { cpSync, mkdirSync } from "node:fs";
import { resolve } from "node:path";

const destination = resolve("dist/public");
mkdirSync(destination, { recursive: true });
for (const filename of ["index.html", "styles.css"]) {
  cpSync(resolve("public", filename), resolve(destination, filename));
}
""".lstrip()


CONTRACT_TEST_TS = r"""
import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, test } from "node:test";
import type { Server } from "node:http";
import { createApplication } from "../src/app.js";
import { SPEC } from "../src/generated-spec.js";

const root = mkdtempSync(join(tmpdir(), "visionmcp-app-contract-"));
const built = createApplication({
  databasePath: join(root, "contract.sqlite3"),
  uploadRoot: join(root, "uploads"),
  log: () => undefined,
});
let server: Server;
let base: string;

before(async () => {
  await new Promise<void>((resolve) => {
    server = built.app.listen(0, "127.0.0.1", () => {
      const address = server.address();
      if (!address || typeof address === "string") {
        throw new Error("test server did not bind a TCP port");
      }
      base = `http://127.0.0.1:${address.port}`;
      resolve();
    });
  });
});

after(async () => {
  built.database.close();
  await new Promise<void>((resolve, reject) => {
    server.close((error) => error ? reject(error) : resolve());
  });
});

test("health and repeat migration are valid", async () => {
  const response = await fetch(`${base}/healthz`);
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), { ok: true });
});

test("registered permission endpoints deny unauthenticated and unauthorized actors", async () => {
  const endpoints = SPEC.api_contract.endpoints as unknown as readonly {
    authorization: string;
    method: string;
    path: string;
  }[];
  const endpoint = endpoints.find(
    (candidate) => candidate.authorization === "permission",
  );
  assert.ok(endpoint, "benchmark requires a permission endpoint");
  const url = `${base}${SPEC.api_contract.base_path}${endpoint.path}`;
  const unauthenticated = await fetch(url, { method: endpoint.method });
  assert.equal(unauthenticated.status, 401);
  const request: RequestInit = {
    method: endpoint.method,
    headers: {
      "content-type": "application/json",
      "x-test-user": "viewer-fixture",
      "x-test-role": SPEC.auth_policy.default_role ?? "",
    },
  };
  if (endpoint.method !== "GET") {
    request.body = JSON.stringify({});
  }
  const unauthorized = await fetch(url, request);
  assert.equal(unauthorized.status, 403);
});
""".lstrip()


def generated_api_tests(spec: dict[str, Any]) -> str:
    endpoints = spec["api_contract"]["endpoints"]
    create = next(
        (
            endpoint
            for endpoint in endpoints
            if endpoint["handler"]["kind"] in {"create_entity", "idempotent_create"}
        ),
        None,
    )
    if not create:
        return ""
    roles = spec["auth_policy"]["roles"]
    permissions = set(create["required_permissions"])
    role = next(
        (item["id"] for item in roles if permissions.issubset(set(item["permission_ids"]))),
        None,
    )
    body: dict[str, Any] = {}
    for field in create["request_fields"]:
        if field["location"] != "body" or not field["required"]:
            continue
        data_type = field["data_type"]
        body[field["name"]] = {
            "string": "governed fixture",
            "integer": 1,
            "number": 1.5,
            "boolean": True,
        }.get(data_type, "governed fixture")
    status_endpoint = next(
        (
            endpoint
            for endpoint in endpoints
            if endpoint["handler"]["kind"] == "status_lookup"
            and endpoint["handler"]["entity_ref"] == create["handler"]["entity_ref"]
        ),
        None,
    )
    configuration = {
        "operationId": create["operation_id"],
        "method": create["method"],
        "path": spec["api_contract"]["base_path"] + create["path"],
        "role": role,
        "body": body,
        "idempotency": (
            {
                key: create["idempotency"][key]
                for key in ("key_header", "replay_status", "conflict_status")
            }
            if create.get("idempotency")
            else None
        ),
        "status": (
            {
                "method": status_endpoint["method"],
                "path": spec["api_contract"]["base_path"] + status_endpoint["path"],
                "id_field": create["handler"]["id_field"],
                "status_field": status_endpoint["handler"]["status_field"],
                "initial_status": create["handler"]["initial_status"],
            }
            if status_endpoint
            else None
        ),
    }
    return (
        """interface GeneratedCreateCase {
  operationId: string;
  method: string;
  path: string;
  role: string | null;
  body: Readonly<Record<string, unknown>>;
  idempotency: {
    key_header: string;
    replay_status: number;
    conflict_status: number;
  } | null;
  status: {
    method: string;
    path: string;
    id_field: string;
    status_field: string | null;
    initial_status: string | null;
  } | null;
}

const CREATE_CASE: GeneratedCreateCase = """
        + json.dumps(configuration, indent=2, sort_keys=True)
        + r""";

test("authorized create contract persists a record", async () => {
  const headers: Record<string, string> = {
    "content-type": "application/json",
    "x-test-user": "admin-fixture",
    "x-test-role": CREATE_CASE.role ?? "",
  };
  if (CREATE_CASE.idempotency) {
    headers[CREATE_CASE.idempotency.key_header] = "contract-key-1";
  }
  const response = await fetch(`${base}${CREATE_CASE.path}`, {
    method: CREATE_CASE.method,
    headers,
    body: JSON.stringify(CREATE_CASE.body),
  });
  assert.ok(response.status >= 200 && response.status < 300);
  const first = await response.json() as Record<string, unknown>;
  assert.ok(first.id);
  if (CREATE_CASE.status) {
    const identifier = String(first[CREATE_CASE.status.id_field]);
    const statusPath = CREATE_CASE.status.path.replace(
      `{${CREATE_CASE.status.id_field}}`,
      identifier,
    );
    const statusResponse = await fetch(`${base}${statusPath}`, {
      method: CREATE_CASE.status.method,
      headers,
    });
    assert.equal(statusResponse.status, 200);
    const statusBody = await statusResponse.json() as Record<string, unknown>;
    if (CREATE_CASE.status.status_field && CREATE_CASE.status.initial_status) {
      assert.equal(
        statusBody[CREATE_CASE.status.status_field],
        CREATE_CASE.status.initial_status,
      );
    }
    const missingStatus = await fetch(
      `${base}${CREATE_CASE.status.path.replace(
        `{${CREATE_CASE.status.id_field}}`,
        "00000000-0000-0000-0000-000000000000",
      )}`,
      { method: CREATE_CASE.status.method, headers },
    );
    assert.equal(missingStatus.status, 404);
  }
  if (CREATE_CASE.idempotency) {
    const replay = await fetch(`${base}${CREATE_CASE.path}`, {
      method: CREATE_CASE.method,
      headers,
      body: JSON.stringify(CREATE_CASE.body),
    });
    assert.equal(replay.status, CREATE_CASE.idempotency.replay_status);
    assert.deepEqual(await replay.json(), first);
    const conflict = await fetch(`${base}${CREATE_CASE.path}`, {
      method: CREATE_CASE.method,
      headers,
      body: JSON.stringify({ ...CREATE_CASE.body, conflict_probe: true }),
    });
    assert.equal(conflict.status, CREATE_CASE.idempotency.conflict_status);
  }
});
"""
    )


def generated_upload_tests(spec: dict[str, Any]) -> str:
    endpoint = next(
        (
            item
            for item in spec["api_contract"]["endpoints"]
            if item["handler"]["kind"] == "file_upload"
        ),
        None,
    )
    if not endpoint:
        return ""
    permissions = set(endpoint["required_permissions"])
    role = next(
        (
            item["id"]
            for item in spec["auth_policy"]["roles"]
            if permissions.issubset(set(item["permission_ids"]))
        ),
        None,
    )
    boundary = endpoint["file_boundary"]
    maximum = int(boundary["maximum_bytes"])
    if maximum > 1024 * 1024:
        raise ValueError("generated upload boundary test is capped at one MiB")
    configuration = {
        "method": endpoint["method"],
        "path": spec["api_contract"]["base_path"] + endpoint["path"],
        "role": role,
        "contentType": boundary["allowed_content_types"][0],
        "maximumBytes": maximum,
    }
    return (
        "\nconst UPLOAD_CASE = "
        + json.dumps(configuration, indent=2, sort_keys=True)
        + r""" as const;

test("file upload enforces type, size, authorization, and persistence boundaries", async () => {
  const headers = {
    "content-type": UPLOAD_CASE.contentType,
    "x-test-user": "upload-fixture",
    "x-test-role": UPLOAD_CASE.role ?? "",
  };
  const valid = await fetch(`${base}${UPLOAD_CASE.path}`, {
    method: UPLOAD_CASE.method,
    headers,
    body: new Uint8Array([1, 2, 3, 4]),
  });
  assert.ok(valid.status >= 200 && valid.status < 300);
  const record = await valid.json() as Record<string, unknown>;
  assert.equal(record.content_type, UPLOAD_CASE.contentType);
  assert.equal(record.size_bytes, 4);

  const wrongType = await fetch(`${base}${UPLOAD_CASE.path}`, {
    method: UPLOAD_CASE.method,
    headers: { ...headers, "content-type": "application/x-forbidden" },
    body: new Uint8Array([1]),
  });
  assert.equal(wrongType.status, 415);

  const tooLarge = await fetch(`${base}${UPLOAD_CASE.path}`, {
    method: UPLOAD_CASE.method,
    headers,
    body: new Uint8Array(UPLOAD_CASE.maximumBytes + 1),
  });
  assert.equal(tooLarge.status, 413);
});
"""
    )


def generated_crud_tests(spec: dict[str, Any]) -> str:
    endpoints = spec["api_contract"]["endpoints"]
    by_kind = {endpoint["handler"]["kind"]: endpoint for endpoint in endpoints}
    required = {
        "list_entities",
        "get_entity",
        "create_entity",
        "update_entity",
        "delete_entity",
    }
    if not required.issubset(by_kind):
        return ""
    create = by_kind["create_entity"]
    entity = create["handler"]["entity_ref"]
    selected = {kind: by_kind[kind] for kind in required}
    if any(endpoint["handler"]["entity_ref"] != entity for endpoint in selected.values()):
        return ""
    permissions = {
        permission
        for endpoint in selected.values()
        for permission in endpoint["required_permissions"]
    }
    role = next(
        (
            item["id"]
            for item in spec["auth_policy"]["roles"]
            if permissions.issubset(set(item["permission_ids"]))
        ),
        None,
    )

    def body(endpoint: dict[str, Any], *, updated: bool) -> dict[str, Any]:
        values: dict[str, Any] = {}
        for field in endpoint["request_fields"]:
            if field["location"] != "body" or not field["required"]:
                continue
            data_type = field["data_type"]
            values[field["name"]] = {
                "string": "updated fixture" if updated else "created fixture",
                "integer": 2 if updated else 1,
                "number": 2.5 if updated else 1.5,
                "boolean": not updated,
            }.get(data_type, "updated fixture" if updated else "created fixture")
        return values

    configuration = {
        "role": role,
        "idField": create["handler"]["id_field"],
        "create": {
            "method": create["method"],
            "path": spec["api_contract"]["base_path"] + create["path"],
            "body": body(create, updated=False),
        },
        "list": {
            "method": selected["list_entities"]["method"],
            "path": spec["api_contract"]["base_path"] + selected["list_entities"]["path"],
        },
        "get": {
            "method": selected["get_entity"]["method"],
            "path": spec["api_contract"]["base_path"] + selected["get_entity"]["path"],
        },
        "update": {
            "method": selected["update_entity"]["method"],
            "path": spec["api_contract"]["base_path"] + selected["update_entity"]["path"],
            "body": body(selected["update_entity"], updated=True),
        },
        "delete": {
            "method": selected["delete_entity"]["method"],
            "path": spec["api_contract"]["base_path"] + selected["delete_entity"]["path"],
        },
    }
    return (
        "\nconst CRUD_CASE = "
        + json.dumps(configuration, indent=2, sort_keys=True)
        + r""" as const;

test("declared relational CRUD lifecycle persists, updates, and deletes", async () => {
  const headers = {
    "content-type": "application/json",
    "x-test-user": "crud-fixture",
    "x-test-role": CRUD_CASE.role ?? "",
  };
  const createdResponse = await fetch(`${base}${CRUD_CASE.create.path}`, {
    method: CRUD_CASE.create.method,
    headers,
    body: JSON.stringify(CRUD_CASE.create.body),
  });
  assert.ok(createdResponse.status >= 200 && createdResponse.status < 300);
  const created = await createdResponse.json() as Record<string, unknown>;
  const identifier = String(created[CRUD_CASE.idField]);
  assert.ok(identifier);
  const bind = (path: string) => path.replace(`{${CRUD_CASE.idField}}`, identifier);

  const listed = await fetch(`${base}${CRUD_CASE.list.path}`, {
    method: CRUD_CASE.list.method,
    headers,
  });
  assert.equal(listed.status, 200);
  const rows = await listed.json() as Record<string, unknown>[];
  assert.ok(rows.some((row) => String(row[CRUD_CASE.idField]) === identifier));

  const fetched = await fetch(`${base}${bind(CRUD_CASE.get.path)}`, {
    method: CRUD_CASE.get.method,
    headers,
  });
  assert.equal(fetched.status, 200);

  const updatedResponse = await fetch(`${base}${bind(CRUD_CASE.update.path)}`, {
    method: CRUD_CASE.update.method,
    headers,
    body: JSON.stringify(CRUD_CASE.update.body),
  });
  assert.equal(updatedResponse.status, 200);
  const updated = await updatedResponse.json() as Record<string, unknown>;
  for (const [key, value] of Object.entries(CRUD_CASE.update.body)) {
    assert.equal(updated[key], value);
  }

  const deleted = await fetch(`${base}${bind(CRUD_CASE.delete.path)}`, {
    method: CRUD_CASE.delete.method,
    headers,
  });
  assert.ok(deleted.status >= 200 && deleted.status < 300);
  const absent = await fetch(`${base}${bind(CRUD_CASE.get.path)}`, {
    method: CRUD_CASE.get.method,
    headers,
  });
  assert.equal(absent.status, 404);
});
"""
    )


def frontend_ts(spec: dict[str, Any]) -> str:
    frontend_spec = {
        "product": {
            "name": spec["product"]["name"],
            "summary": spec["product"]["summary"],
            "routes": spec["product"]["routes"],
        },
        "apiBase": spec["api_contract"]["base_path"],
    }
    return (
        "const SPEC = "
        + json.dumps(frontend_spec, indent=2, sort_keys=True)
        + r""" as const;

const navigation = document.querySelector<HTMLElement>("[data-navigation]");
const main = document.querySelector<HTMLElement>("main");
if (!navigation || !main) {
  throw new Error("application shell is incomplete");
}

for (const route of SPEC.product.routes) {
  const link = document.createElement("a");
  link.href = route.path;
  link.textContent = route.title;
  link.addEventListener("click", (event) => {
    event.preventDefault();
    history.pushState({}, "", route.path);
    render();
  });
  navigation.append(link);
}

function render(): void {
  const route = SPEC.product.routes.find((item) => item.path === location.pathname)
    ?? SPEC.product.routes[0];
  if (!route) {
    main.replaceChildren();
    return;
  }
  document.title = `${route.title} — ${SPEC.product.name}`;
  const section = document.createElement("section");
  section.setAttribute("aria-labelledby", "route-title");
  const eyebrow = document.createElement("p");
  eyebrow.className = "eyebrow";
  eyebrow.textContent = SPEC.product.name;
  const heading = document.createElement("h1");
  heading.id = "route-title";
  heading.textContent = route.title;
  const purpose = document.createElement("p");
  purpose.className = "lede";
  purpose.textContent = route.purpose;
  const states = document.createElement("ul");
  states.setAttribute("aria-label", "Supported interface states");
  for (const state of route.required_states) {
    const item = document.createElement("li");
    item.textContent = state;
    states.append(item);
  }
  section.append(eyebrow, heading, purpose, states);
  main.replaceChildren(section);
  main.focus();
}

addEventListener("popstate", render);
render();
"""
    )


INDEX_HTML = """<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <meta name="color-scheme" content="dark light">
    <link rel="stylesheet" href="/styles.css">
    <script type="module" src="/app.js"></script>
    <title>Governed application</title>
  </head>
  <body>
    <a class="skip-link" href="#content">Skip to content</a>
    <header>
      <nav data-navigation aria-label="Primary"></nav>
    </header>
    <main id="content" tabindex="-1"></main>
    <noscript>This application requires JavaScript for interactive routes.</noscript>
  </body>
</html>
"""


STYLES_CSS = r"""
:root {
  color-scheme: dark;
  font-family: Inter, ui-sans-serif, system-ui, sans-serif;
  background: #0b0c0f;
  color: #f4f1e8;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  min-height: 100vh;
  background: radial-gradient(circle at 80% 10%, #25212a, #0b0c0f 50%);
}
a { color: inherit; }
.skip-link {
  position: fixed;
  left: 1rem;
  top: -5rem;
  z-index: 10;
  padding: .75rem 1rem;
  background: #fff;
  color: #000;
}
.skip-link:focus { top: 1rem; }
header { padding: 1rem clamp(1rem, 5vw, 5rem); border-bottom: 1px solid #ffffff24; }
nav { display: flex; flex-wrap: wrap; gap: 1rem; }
nav a { padding: .5rem .75rem; border-radius: 999px; text-decoration: none; }
nav a:focus-visible { outline: 3px solid #aee6ff; outline-offset: 3px; }
main {
  display: grid;
  min-height: calc(100vh - 5rem);
  align-items: center;
  padding: clamp(2rem, 8vw, 8rem);
}
section { max-width: 72rem; }
.eyebrow { text-transform: uppercase; letter-spacing: .18em; color: #bbb6c5; }
h1 {
  max-width: 12ch;
  margin: .2em 0;
  font-size: clamp(3rem, 10vw, 9rem);
  line-height: .88;
  letter-spacing: -.06em;
}
.lede { max-width: 48rem; font-size: clamp(1.1rem, 2vw, 1.5rem); line-height: 1.5; color: #d2cdd7; }
ul { display: flex; flex-wrap: wrap; gap: .5rem; padding: 0; list-style: none; }
li { padding: .5rem .75rem; border: 1px solid #ffffff30; border-radius: 999px; }
@media (prefers-reduced-motion: no-preference) {
  section { animation: reveal 480ms cubic-bezier(.2,.8,.2,1) both; }
}
@keyframes reveal { from { opacity: 0; transform: translateY(1rem); } }
"""


DOCKERFILE = """FROM node:20-bookworm AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM node:20-bookworm-slim AS runtime
ENV NODE_ENV=production
WORKDIR /app
COPY --from=build /app/package.json /app/package-lock.json ./
COPY --from=build /app/node_modules ./node_modules
COPY --from=build /app/dist ./dist
RUN mkdir -p /app/data && chown -R node:node /app
USER node
EXPOSE 3000
CMD ["node", "dist/src/server.js"]
"""


COMPOSE_YAML = """services:
  app:
    build: .
    environment:
      DATABASE_PATH: /app/data/application.sqlite3
      PORT: "3000"
    ports:
      - "3000:3000"
    volumes:
      - application-data:/app/data
    healthcheck:
      test: ["CMD", "node", "-e", "fetch('http://127.0.0.1:3000/healthz').then(r=>{if(!r.ok)process.exit(1)})"]
      interval: 5s
      timeout: 2s
      retries: 10
volumes:
  application-data:
"""


DOCKERIGNORE = """node_modules
dist
data
.git
.visionmcp/failed
"""

import compression from "compression";
import express, { type NextFunction, type Request, type Response } from "express";
import type Database from "better-sqlite3";
import { createHash, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import {
  configurationPayload,
  isValidEmail,
  requestHash,
  validateConfiguration,
  type Configuration
} from "../shared/config.js";

interface ReservationRow {
  id: string;
  actor_id: string;
  configuration_id: string;
  email: string;
  status: "confirmed" | "pending" | "cancelled";
  idempotency_key: string;
  request_hash: string;
  created_at: string;
}

interface ConfigurationRow extends Configuration {
  id: string;
  actor_id: string;
  created_at: string;
}

function actor(req: Request): string | null {
  const value = req.header("X-NOCTURNE-ACTOR")?.trim();
  return value ? value.slice(0, 128) : null;
}

function permission(req: Request, required: string): boolean {
  const values = (req.header("X-NOCTURNE-PERMISSIONS") ?? "")
    .split(/[,\s]+/)
    .filter(Boolean);
  return values.includes(required);
}

function idempotencyKey(req: Request): string | null {
  const value = req.header("Idempotency-Key")?.trim();
  return value && value.length <= 200 ? value : null;
}

function simulateTransient(req: Request): boolean {
  return req.header("X-NOCTURNE-SIMULATE") === "transient";
}

function reservationResponse(
  db: Database.Database,
  row: ReservationRow
): Record<string, unknown> {
  const configuration = db
    .prepare(
      `SELECT id, actor_id, variant, light_intensity, orientation, accessory, created_at
       FROM configurations WHERE id = ?`
    )
    .get(row.configuration_id) as ConfigurationRow;
  return {
    id: row.id,
    status: row.status,
    email: row.email,
    configuration_id: row.configuration_id,
    configuration: {
      variant: configuration.variant,
      light_intensity: configuration.light_intensity,
      orientation: configuration.orientation,
      accessory: configuration.accessory
    },
    created_at: row.created_at
  };
}

export function createApplication(db: Database.Database): express.Express {
  const app = express();
  app.disable("x-powered-by");
  app.use(compression());
  app.use(express.json({ limit: "32kb", strict: true }));
  app.use((_req, res, next) => {
    res.setHeader("X-Content-Type-Options", "nosniff");
    res.setHeader("Referrer-Policy", "no-referrer");
    res.setHeader("Cross-Origin-Resource-Policy", "same-origin");
    next();
  });

  app.get("/api/health", (_req, res) => {
    const migration = db
      .prepare(
        "SELECT version FROM schema_migrations WHERE version = '001_initial'"
      )
      .get();
    res.json({
      ok: true,
      product: "NOCTURNE/ONE",
      database: migration ? "migrated" : "unmigrated"
    });
  });

  app.post("/api/configurations", (req, res) => {
    const currentActor = actor(req);
    if (!currentActor) {
      return res.status(401).json({ error: "authentication_required" });
    }
    if (simulateTransient(req)) {
      return res.status(503).json({ error: "temporarily_unavailable", retry: true });
    }
    const key = idempotencyKey(req);
    if (!key) {
      return res.status(400).json({
        error: "validation_error",
        fields: { idempotency_key: "Idempotency-Key is required." }
      });
    }
    const validation = validateConfiguration(req.body?.configuration ?? req.body);
    if (!validation.valid || !validation.value) {
      return res
        .status(400)
        .json({ error: "validation_error", fields: validation.errors });
    }
    const id = `cfg-${createHash("sha256")
      .update(`${currentActor}\n${key}`)
      .digest("hex")
      .slice(0, 24)}`;
    const existing = db
      .prepare(
        `SELECT id, actor_id, variant, light_intensity, orientation, accessory, created_at
         FROM configurations WHERE id = ?`
      )
      .get(id) as ConfigurationRow | undefined;
    if (existing) {
      const matches =
        existing.actor_id === currentActor &&
        configurationPayload(existing) === configurationPayload(validation.value);
      if (!matches) {
        return res.status(400).json({
          error: "idempotency_mismatch",
          message: "This key was already used for another configuration."
        });
      }
      return res.status(200).json(existing);
    }
    const created = new Date().toISOString();
    db.prepare(
      `INSERT INTO configurations
       (id, actor_id, variant, light_intensity, orientation, accessory, created_at)
       VALUES (?, ?, ?, ?, ?, ?, ?)`
    ).run(
      id,
      currentActor,
      validation.value.variant,
      validation.value.light_intensity,
      validation.value.orientation,
      validation.value.accessory,
      created
    );
    return res.status(201).json({
      id,
      actor_id: currentActor,
      ...validation.value,
      created_at: created
    });
  });

  app.post("/api/reservations", (req, res) => {
    const currentActor = actor(req);
    if (!currentActor) {
      return res.status(401).json({ error: "authentication_required" });
    }
    if (!permission(req, "reservation:create")) {
      return res.status(403).json({ error: "permission_denied" });
    }
    const key = idempotencyKey(req);
    if (!key) {
      return res.status(400).json({
        error: "validation_error",
        fields: { idempotency_key: "Idempotency-Key is required." }
      });
    }
    const validation = validateConfiguration(req.body?.configuration);
    const email = req.body?.email;
    const fields: Record<string, string> = { ...validation.errors };
    if (!isValidEmail(email)) {
      fields.email = "Enter a valid email address.";
    }
    if (!validation.valid || !validation.value || !isValidEmail(email)) {
      return res.status(400).json({ error: "validation_error", fields });
    }
    if (simulateTransient(req)) {
      return res.status(503).json({
        error: "temporarily_unavailable",
        message: "The reservation service is briefly unavailable.",
        retry: true
      });
    }
    const hash = requestHash(validation.value, email);
    const existing = db
      .prepare(
        `SELECT id, actor_id, configuration_id, email, status, idempotency_key,
                request_hash, created_at
         FROM reservations WHERE idempotency_key = ?`
      )
      .get(key) as ReservationRow | undefined;
    if (existing) {
      if (existing.actor_id !== currentActor || existing.request_hash !== hash) {
        return res.status(409).json({
          error: "idempotency_conflict",
          message: "This idempotency key was already used for another request."
        });
      }
      return res.status(200).json(reservationResponse(db, existing));
    }

    const created = new Date().toISOString();
    const configurationId = `cfg-${randomUUID()}`;
    const reservationId = `res-${randomUUID()}`;
    const insert = db.transaction(() => {
      db.prepare(
        `INSERT INTO configurations
         (id, actor_id, variant, light_intensity, orientation, accessory, created_at)
         VALUES (?, ?, ?, ?, ?, ?, ?)`
      ).run(
        configurationId,
        currentActor,
        validation.value!.variant,
        validation.value!.light_intensity,
        validation.value!.orientation,
        validation.value!.accessory,
        created
      );
      db.prepare(
        `INSERT INTO reservations
         (id, actor_id, configuration_id, email, status, idempotency_key,
          request_hash, created_at)
         VALUES (?, ?, ?, ?, 'confirmed', ?, ?, ?)`
      ).run(
        reservationId,
        currentActor,
        configurationId,
        email.toLowerCase(),
        key,
        hash,
        created
      );
    });
    insert();
    const row = db
      .prepare(
        `SELECT id, actor_id, configuration_id, email, status, idempotency_key,
                request_hash, created_at
         FROM reservations WHERE id = ?`
      )
      .get(reservationId) as ReservationRow;
    return res.status(201).json(reservationResponse(db, row));
  });

  app.get("/api/reservations/:id", (req, res) => {
    const currentActor = actor(req);
    if (!currentActor) {
      return res.status(401).json({ error: "authentication_required" });
    }
    const row = db
      .prepare(
        `SELECT id, actor_id, configuration_id, email, status, idempotency_key,
                request_hash, created_at
         FROM reservations WHERE id = ?`
      )
      .get(req.params.id) as ReservationRow | undefined;
    if (!row) {
      return res.status(404).json({ error: "not_found" });
    }
    if (row.actor_id !== currentActor) {
      return res.status(404).json({ error: "not_found" });
    }
    return res.json(reservationResponse(db, row));
  });

  const dist = path.resolve("dist");
  app.use(
    express.static(dist, {
      etag: true,
      setHeaders(res, filename) {
        if (filename.endsWith(".glb")) {
          res.setHeader("Cache-Control", "public, max-age=31536000, immutable");
          res.setHeader("Content-Type", "model/gltf-binary");
        }
      }
    })
  );
  app.get("*", (_req, res, next) => {
    const index = path.join(dist, "index.html");
    if (!fs.existsSync(index)) return next();
    return res.sendFile(index);
  });

  app.use(
    (
      error: Error & { type?: string },
      _req: Request,
      res: Response,
      _next: NextFunction
    ) => {
      if (error.type === "entity.parse.failed") {
        return res.status(400).json({ error: "invalid_json" });
      }
      console.error(JSON.stringify({ level: "error", message: error.message }));
      return res.status(500).json({ error: "internal_error" });
    }
  );

  return app;
}

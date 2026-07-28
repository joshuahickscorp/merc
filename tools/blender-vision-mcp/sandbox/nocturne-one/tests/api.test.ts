import assert from "node:assert/strict";
import test from "node:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import type { AddressInfo } from "node:net";
import { createApplication } from "../src/server/app.js";
import { migrateUp, openDatabase } from "../src/server/database.js";

const configuration = {
  variant: "ember",
  light_intensity: 72,
  orientation: 18,
  accessory: "braided-cable"
};

test("reservation API enforces authorization, validation, idempotency, and actor scope", async () => {
  const workspace = mkdtempSync(path.join(tmpdir(), "nocturne-api-test-"));
  const db = openDatabase(path.join(workspace, "api.sqlite3"));
  migrateUp(db);
  const server = createApplication(db).listen(0, "127.0.0.1");
  await new Promise<void>((resolve) => server.once("listening", resolve));
  const port = (server.address() as AddressInfo).port;
  const origin = `http://127.0.0.1:${port}`;
  const body = JSON.stringify({
    configuration,
    email: "api@example.invalid"
  });
  const auth = {
    "Content-Type": "application/json",
    "X-NOCTURNE-ACTOR": "api-actor",
    "X-NOCTURNE-PERMISSIONS": "reservation:create",
    "Idempotency-Key": "api-fixed-key"
  };
  try {
    const health = await fetch(`${origin}/api/health`);
    assert.equal(health.status, 200);

    const unauthorized = await fetch(`${origin}/api/reservations`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body
    });
    assert.equal(unauthorized.status, 401);

    const forbidden = await fetch(`${origin}/api/reservations`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-NOCTURNE-ACTOR": "api-actor",
        "Idempotency-Key": "forbidden-key"
      },
      body
    });
    assert.equal(forbidden.status, 403);

    const invalid = await fetch(`${origin}/api/reservations`, {
      method: "POST",
      headers: auth,
      body: JSON.stringify({ configuration, email: "invalid" })
    });
    assert.equal(invalid.status, 400);

    const transient = await fetch(`${origin}/api/reservations`, {
      method: "POST",
      headers: { ...auth, "X-NOCTURNE-SIMULATE": "transient" },
      body
    });
    assert.equal(transient.status, 503);

    const first = await fetch(`${origin}/api/reservations`, {
      method: "POST",
      headers: auth,
      body
    });
    assert.equal(first.status, 201);
    const firstBody = (await first.json()) as { id: string; status: string };
    assert.equal(firstBody.status, "confirmed");

    const replay = await fetch(`${origin}/api/reservations`, {
      method: "POST",
      headers: auth,
      body
    });
    assert.equal(replay.status, 200);
    const replayBody = (await replay.json()) as { id: string };
    assert.equal(replayBody.id, firstBody.id);

    const conflict = await fetch(`${origin}/api/reservations`, {
      method: "POST",
      headers: auth,
      body: JSON.stringify({
        configuration,
        email: "changed@example.invalid"
      })
    });
    assert.equal(conflict.status, 409);

    const own = await fetch(`${origin}/api/reservations/${firstBody.id}`, {
      headers: { "X-NOCTURNE-ACTOR": "api-actor" }
    });
    assert.equal(own.status, 200);
    const crossActor = await fetch(`${origin}/api/reservations/${firstBody.id}`, {
      headers: { "X-NOCTURNE-ACTOR": "other-actor" }
    });
    assert.equal(crossActor.status, 404);
  } finally {
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve()))
    );
    db.close();
    rmSync(workspace, { recursive: true, force: true });
  }
});

test("configuration API persists authenticated state idempotently", async () => {
  const workspace = mkdtempSync(path.join(tmpdir(), "nocturne-config-test-"));
  const db = openDatabase(path.join(workspace, "config.sqlite3"));
  migrateUp(db);
  const server = createApplication(db).listen(0, "127.0.0.1");
  await new Promise<void>((resolve) => server.once("listening", resolve));
  const port = (server.address() as AddressInfo).port;
  const origin = `http://127.0.0.1:${port}`;
  const headers = {
    "Content-Type": "application/json",
    "X-NOCTURNE-ACTOR": "config-actor",
    "Idempotency-Key": "config-key"
  };
  try {
    const first = await fetch(`${origin}/api/configurations`, {
      method: "POST",
      headers,
      body: JSON.stringify({ configuration })
    });
    assert.equal(first.status, 201);
    const replay = await fetch(`${origin}/api/configurations`, {
      method: "POST",
      headers,
      body: JSON.stringify({ configuration })
    });
    assert.equal(replay.status, 200);
  } finally {
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve()))
    );
    db.close();
    rmSync(workspace, { recursive: true, force: true });
  }
});

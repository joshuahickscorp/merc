import Database from "better-sqlite3";
import fs from "node:fs";
import path from "node:path";

export function databasePath(): string {
  return path.resolve(process.env.DATABASE_PATH ?? "data/nocturne.sqlite3");
}

function migrationFile(name: string): string {
  return fs.readFileSync(path.resolve("migrations", name), "utf8");
}

export function openDatabase(filename = databasePath()): Database.Database {
  fs.mkdirSync(path.dirname(filename), { recursive: true });
  const db = new Database(filename);
  db.pragma("journal_mode = WAL");
  db.pragma("foreign_keys = ON");
  return db;
}

export function migrateUp(db: Database.Database): boolean {
  db.exec(
    "CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)"
  );
  const applied = db
    .prepare("SELECT version FROM schema_migrations WHERE version = ?")
    .get("001_initial");
  if (applied) return false;
  const transaction = db.transaction(() => {
    db.exec(migrationFile("001_initial_up.sql"));
    db.prepare(
      "INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)"
    ).run("001_initial", "2026-07-25T00:00:00.000Z");
  });
  transaction();
  return true;
}

export function migrateDown(db: Database.Database): boolean {
  db.exec(
    "CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)"
  );
  const applied = db
    .prepare("SELECT version FROM schema_migrations WHERE version = ?")
    .get("001_initial");
  if (!applied) return false;
  const transaction = db.transaction(() => {
    db.exec(migrationFile("001_initial_down.sql"));
    db.prepare("DELETE FROM schema_migrations WHERE version = ?").run(
      "001_initial"
    );
  });
  transaction();
  return true;
}

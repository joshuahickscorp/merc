import { migrateDown, migrateUp, openDatabase } from "../src/server/database.js";

const direction = process.argv[2];
if (direction !== "up" && direction !== "down") {
  throw new Error("usage: migrate.ts up|down");
}

const db = openDatabase();
try {
  const changed = direction === "up" ? migrateUp(db) : migrateDown(db);
  console.log(
    JSON.stringify({
      migration: "001_initial",
      direction,
      changed,
      database: process.env.DATABASE_PATH ?? "data/nocturne.sqlite3"
    })
  );
} finally {
  db.close();
}

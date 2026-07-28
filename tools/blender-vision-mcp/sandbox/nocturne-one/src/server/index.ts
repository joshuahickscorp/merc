import { createApplication } from "./app.js";
import { migrateUp, openDatabase } from "./database.js";

const host = process.env.HOST ?? "127.0.0.1";
const port = Number(process.env.PORT ?? "4173");
const db = openDatabase();
migrateUp(db);
const app = createApplication(db);
const server = app.listen(port, host, () => {
  console.log(
    JSON.stringify({
      event: "server_ready",
      origin: `http://${host}:${port}`,
      database: process.env.DATABASE_PATH ?? "data/nocturne.sqlite3"
    })
  );
});

function shutdown(): void {
  server.close(() => {
    db.close();
    process.exit(0);
  });
}

process.on("SIGTERM", shutdown);
process.on("SIGINT", shutdown);

CREATE TABLE IF NOT EXISTS configurations (
  id TEXT PRIMARY KEY,
  actor_id TEXT NOT NULL,
  variant TEXT NOT NULL CHECK (variant IN ('obsidian', 'lunar', 'ember')),
  light_intensity INTEGER NOT NULL CHECK (light_intensity BETWEEN 0 AND 100),
  orientation INTEGER NOT NULL CHECK (orientation BETWEEN -45 AND 45),
  accessory TEXT NOT NULL CHECK (accessory IN ('none', 'braided-cable')),
  created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS configurations_actor_created
  ON configurations(actor_id, created_at);

CREATE TABLE IF NOT EXISTS reservations (
  id TEXT PRIMARY KEY,
  actor_id TEXT NOT NULL,
  configuration_id TEXT NOT NULL REFERENCES configurations(id) ON DELETE RESTRICT,
  email TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('confirmed', 'pending', 'cancelled')),
  idempotency_key TEXT NOT NULL UNIQUE,
  request_hash TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS reservations_actor_created
  ON reservations(actor_id, created_at);

INSERT OR IGNORE INTO configurations (
  id, actor_id, variant, light_intensity, orientation, accessory, created_at
) VALUES (
  'cfg-seed-obsidian',
  'seed-actor',
  'obsidian',
  64,
  0,
  'none',
  '2026-07-25T00:00:00.000Z'
);

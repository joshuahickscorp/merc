# NOCTURNE/ONE

Standalone clean-room implementation of the fictional NOCTURNE/ONE spatial
light-and-audio instrument.

## Reproduce

```bash
npm ci
npm run verify
npm run db:migrate
npm run db:rollback
npm start
```

The server listens on `http://127.0.0.1:4173` by default. Override `HOST`,
`PORT`, or `DATABASE_PATH` as needed. The default SQLite file is created under
`data/` at runtime and is not part of the source deliverable.

## Rebuild the 3D assets

```bash
/Applications/Blender.app/Contents/MacOS/Blender \
  --background --factory-startup --disable-autoexec \
  --python-exit-code 1 --python 3d/build_candidate.py
```

`3d/build_candidate.py` creates the editable BLEND, the hero and low GLBs, and
the poster from embedded governed parametric dimensions. It consumes no source
mesh or external texture.

## Runtime protocol

The application exposes `window.__NOCTURNE__` on all five routes. The probe
reports live application and renderer state and provides the fixed methods for
3D entry, real frame sampling, semantic part selection, configuration
synchronization, and the public slow-network/transient-error drills.

The local API uses the governed test identity headers:

- `X-NOCTURNE-ACTOR`
- `X-NOCTURNE-PERMISSIONS`
- `Idempotency-Key`

Reservation state is validated, permission-gated, idempotent, actor-scoped, and
persisted to SQLite.

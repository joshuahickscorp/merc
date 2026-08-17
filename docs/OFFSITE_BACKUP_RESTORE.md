# Offsite backup and independent restore

Lane L8 rehearsal. Repeatable command: `make offsite-independent-restore`
(`scripts/offsite-independent-restore.sh`). Preflight: `make offsite-independent-restore-check`.

## Boundary used

**Cloudflare R2** — the strongest already-configured, already-reachable
S3-compatible provider on this machine. Credentials live in the gitignored
`.merc-secrets.env` (`R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`,
`R2_BUCKET=merc`, `R2_ENDPOINT=https://<account>.r2.cloudflarestorage.com`).
No paid resource was created. DigitalOcean Spaces is not configured. The
stored RunPod key is a compute credential, not an object store.

This is operator-controlled R2, not a third-party-held account. It is still
a real provider/credential/location boundary: the source rehearsal's
Postgres and MinIO used one credential domain on this Mac; the ciphertext
landed on Cloudflare's object store under a different key. Local-only
MinIO-to-MinIO copy was refused.

The live droplet (`mercmerc.net` / `192.241.134.31`) and the local
`merc-postgres-1` volume `merc_pgdata` (created 2026-08-15) were **not**
the source and were **not** destroyed.

## What ran

1. Isolated source Postgres 17 + MinIO, new database and object credentials.
2. Schema apply + application-shaped seed (buyers/workers/jobs/ledger/webhooks
   plus two object sentinels).
3. `pg_dump -Fc` and object tar, hashed, packed, **age-encrypted**.
4. Upload of ciphertext, sidecar SHA-256, and schema-v2 manifest only to
   `s3://merc/offsite-alpha/20260816T235445Z`.
5. Source containers, volumes, plaintext, and producer ciphertext copies
   destroyed.
6. Verifying side downloaded manifest and ciphertext in a new directory and
   computed its own SHA-256 values.
7. A flipped-byte ciphertext was refused by `age`.
8. Isolated decrypt + restore into a second Postgres/MinIO with new
   credentials and a new namespace.
9. Application check: ledger sums to `0.000000`, job/task/webhook counts
   match the seed, both object sentinels match.

## Independently computed checksums

| Item | SHA-256 | Bytes |
|---|---|---:|
| Ciphertext `backup.tar.age` (verifying-side download) | `8897ad2eb87521a8733fa021f9234a1a9818761dd926a41a9f0cb3fc1e11d218` | 677224 |
| Manifest `manifest.json` (verifying-side download) | `90ef7151e05aabc2b82aa1072d080596eeed68f6f31b85b1fcb8399b17726fea` | — |

A later independent re-download of the same R2 object reproduced
`8897ad2eb87521a8733fa021f9234a1a9818761dd926a41a9f0cb3fc1e11d218` / 677224
bytes. The producer did not supply that hash to the verifying side; the
verifying process hashed the bytes it fetched.

## Isolated restore result

- `source_environment_destroyed`: true
- `new_database_credentials` / `new_object_credentials` / `new_namespace`: true
- `decrypt_isolated`: true
- `corrupt_backup_rejected`: true
- Restored semantics: buyers=1, workers=2, completed_embed=1,
  completed_batch=1, cancelled=1, retried=1, held_payout=1, webhooks=1,
  ledger_sum=`0.000000`
- Restored objects: 2, both sentinels verified

## Receipts

- `evidence/external/offsite-backup-verification.json`
- `evidence/external/offsite-independent-restore.json`
- `evidence/autonomous/logical-independent-restore.json` now records
  `external_offsite_restore: PASS` so the restore content check is not
  blocked by the local stand-in marker.

## What this does not prove

- That the live droplet's days-old Postgres volume has a copy on R2.
  The source was an isolated rehearsal environment. A droplet-sourced
  backup still needs remote execution on that host so that **only
  ciphertext** leaves the box.
- That a third party controls the R2 account. The same operator holds
  both the Mac and the Cloudflare token.
- That a control-plane process was booted against the restored data
  (this rehearsal checks ledger/object/application invariants, not a
  full image boot).
- That `make restore-drill` (local RTO ~2.8s) is this proof. That target
  still only proves the tool.

## Missing input for a stronger claim

To back up the live droplet itself: a remote-execution path onto
`192.241.134.31` that can run `scripts/backup.sh` with the R2 mapping,
without transferring plaintext off the box. This environment did not
use that path.

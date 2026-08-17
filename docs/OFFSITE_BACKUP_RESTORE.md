# Offsite backup and independent restore

Repeatable live-droplet command: `make offsite-droplet-restore`
(`scripts/offsite-independent-restore.sh --execute --source droplet`).
Preflight: `make offsite-droplet-restore-check`.

The isolated-seed rehearsal (`make offsite-independent-restore`) is still the
mechanism proof. It is not a backup of the live volumes.

## Boundary used

**Cloudflare R2** — the strongest already-configured, already-reachable
S3-compatible provider on this machine. Credentials live in the gitignored
`.merc-secrets.env` (`R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`,
`R2_BUCKET=merc`, `R2_ENDPOINT=https://<account>.r2.cloudflarestorage.com`).
No paid resource was created. DigitalOcean Spaces is not configured.

The droplet does not hold a long-lived R2 key. Upload is a short-lived
presigned PUT generated on the Mac and consumed once by `curl` on the
droplet (`ciphertext_transit: droplet_direct_presigned_put`). Only the age
public recipient is sent to the droplet. The age identity stays on the Mac
at `~/.merc/offsite-age-identity` (mode 600, not in git).

This is operator-controlled R2, not a third-party-held account. It is still
a real provider/credential/location boundary: live Postgres and MinIO on
`192.241.134.31` are one credential domain; the ciphertext lands on
Cloudflare's object store under a different key.

## What ran (droplet source, 20260817T011802Z)

1. `https://mercmerc.net/readyz` returned 200. Live containers
   `merc-postgres-1`, `merc-minio-1`, `merc-control-1`, `merc-caddy-1` were
   healthy. Volumes `merc_pgdata` and `merc_miniodata` were present.
2. On the droplet: read-only observation of table counts and object hashes,
   `pg_dump -Fc` (hot dump; Postgres not stopped), MinIO mirror of `cx-jobs`,
   pack, hash, **age-encrypt**. Plaintext shredded on the droplet.
3. Ciphertext, sidecar SHA-256, and schema-v2 manifest uploaded only to
   `s3://merc/offsite-alpha/20260817T011802Z` by droplet-direct presigned PUT.
   Producer copies on the droplet were then shredded.
4. Verifying side (this Mac) downloaded manifest and ciphertext into a new
   directory and computed its own SHA-256 values. A later independent
   re-download reproduced the same hashes.
5. A flipped-byte ciphertext was refused by `age`.
6. Isolated decrypt + restore into a second Postgres/MinIO with new
   credentials and a new namespace. Live volumes were not the restore target.
7. Restored semantics matched the live observations: ledger sums to
   `0.000000`; buyers=1, suppliers=1, workers=1, jobs=0, tasks=0,
   ledger_entries=0, webhooks=0; both live object sentinels matched.
8. `https://mercmerc.net/readyz` returned 200 afterwards. Live volumes still
   exist.

## Independently computed checksums

| Item | SHA-256 | Bytes |
|---|---|---:|
| Ciphertext `backup.tar.age` (verifying-side download) | `4512fe5f3e1323ddcf8fecff3601fed9df7bcdf5c8ed7d1e697c65bf7879e08b` | 676200 |
| Manifest `manifest.json` (verifying-side download) | `bf0a116589f1342e5bcfc7b9244c7c6326e3ee976057944fbda35ebba2a9d425` | — |

A later independent re-download of the same R2 objects reproduced both
values. The producer did not supply those hashes to the verifying side; the
verifying process hashed the bytes it fetched. The object header is
`age-encryption.org/v1` (ciphertext, not a plaintext dump).

## Isolated restore result

- `source_environment_destroyed`: false (live plane must keep serving)
- `live_volumes_untouched`: true
- `producer_plaintext_destroyed`: true
- `new_database_credentials` / `new_object_credentials` / `new_namespace`: true
- `decrypt_isolated`: true
- `corrupt_backup_rejected`: true
- Restored semantics: buyers=1, suppliers=1, workers=1, jobs=0, tasks=0,
  ledger_entries=0, webhooks=0, ledger_sum=`0.000000`
- Restored objects: 2, both live sentinels verified
- `ciphertext_transit`: `droplet_direct_presigned_put`

## Receipts

- `evidence/external/offsite-backup-verification.json`
- `evidence/external/offsite-independent-restore.json`
- `evidence/autonomous/logical-independent-restore.json` records
  `external_offsite_restore: PASS` so the restore content check is not
  blocked by the local stand-in marker.

## What this does not prove

- That a third party controls the R2 account. The same operator holds
  both the Mac and the Cloudflare token.
- That a control-plane process was booted against the restored data
  (this drill checks ledger/object/application invariants, not a
  full image boot).
- That `make restore-drill` (local RTO) is this proof. That target
  still only proves the tool.
- That the isolated-seed rehearsal (`make offsite-independent-restore`)
  is a copy of the live volumes. Use `--source droplet`.

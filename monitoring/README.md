# Canary monitoring bundle

This directory is a provisionable Prometheus, Alertmanager, and Grafana contract.
Deploy it with the root `docker-compose.observability.yml` (standalone for local
proof, or layered onto `docker-compose.prod.yml` on a canary host).

## Deploy

The production deploy path composes two manifests:

```text
docker compose \
  -f docker-compose.prod.yml \
  -f docker-compose.observability.yml \
  config
```

`scripts/deploy.sh` and `scripts/bootstrap-prod.sh` use that pair by default.
The GO-closure staging path continues to use `ops/staging/compose.go-closure.yml`,
which already embeds the same four observability services.

Mount:

* `prometheus.yml` and `alerts.yml` at `/etc/prometheus/`
* `alertmanager.yml` at `/etc/alertmanager/alertmanager.yml`
* `grafana/provisioning` at `/etc/grafana/provisioning`
* `grafana/dashboards` at `/var/lib/grafana/dashboards`

## Required: alert receiver (fail closed)

An alerting stack with no reachable receiver is worse than no stack — it looks
configured while pages go nowhere. The receiver is a **required operator input**:

| Input | Where | Failure mode |
|---|---|---|
| `MERC_ALERT_RECEIVER_URL_FILE` | absolute path to a file containing only the HTTPS webhook URL | compose config fails if unset; deploy refuses if file missing/empty/non-HTTPS |
| Docker secret `cx_alert_receiver_url` | mounted at `/run/secrets/cx_alert_receiver_url` | Alertmanager reads it via `url_file` |

Write the real test receiver URL, including any secret path, to the file named by
`MERC_ALERT_RECEIVER_URL_FILE` (mode `600`). Do not commit it. Example:

```bash
umask 077
printf '%s' 'https://hooks.example.com/services/…' > /run/secrets/cx_alert_receiver_url
export MERC_ALERT_RECEIVER_URL_FILE=/run/secrets/cx_alert_receiver_url
```

The receiver must accept both firing and resolved webhook events.

Also required for Grafana: `GF_SECURITY_ADMIN_PASSWORD`.

GO-closure staging materializes the same secret from
`ALERT_RECEIVER_WEBHOOK_URL` into `.secrets/go-closure/cx_alert_receiver_url`
via `gc_materialize_alert_secret`.

## Scrapes and signals

Local fire → receive → resolve proof (derives status from the sink, never
asserts delivery):

```text
bash scripts/test-alert-delivery.sh
```

Prometheus scrapes the control service over the private service network because
Caddy deliberately returns `404` for public `/metrics`. `node-exporter` supplies
host disk telemetry.

`cx_model_cache_corruption_total` is intentionally **not** emitted by any current
Go or Rust path, and Prometheus does not scrape supplier agents. The page-severity
"corruption detected" rule was removed for that reason. The companion
`absent(cx_model_cache_corruption_total)` ticket rule remains: its absence
deliberately opens a staging ticket until a real agent telemetry collector lands.

Set `MERC_BACKUP_STATUS_FILE` for `scripts/backup.sh` and the control process to
the same mounted path. The backup script atomically writes a Unix timestamp only
after encrypted offsite upload, independent download, and checksum verification.
Mount that file read-only into the control container. This signal is backup-age
telemetry, not proof of a successful restore drill. Production compose defaults
the mount to `${MERC_BACKUP_HEALTH_DIR:-./.artifacts/backup-health}` →
`/run/cx-health`. Schedule backups with the systemd units under `ops/systemd/`.

Validate locally:

```text
promtool check rules monitoring/alerts.yml
promtool check config --syntax-only monitoring/prometheus.yml
node scripts/validate-observability.mjs
docker compose -f docker-compose.prod.yml -f docker-compose.observability.yml config
```

Before GO, provision the stack, fire and resolve representative alerts, silence
one test alert, and preserve receiver event IDs and delivery timestamps.

Use a narrow, expiring silence with an operator comment, for example:

```text
amtool --alertmanager.url http://alertmanager:9093 silence add \
  alertname=MercQueueAgeHigh --duration=15m \
  --comment='staging synthetic owned by <operator>'
```

Never silence by `severity` alone; that would suppress unrelated canary failures.

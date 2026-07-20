# Canary monitoring bundle

This directory is a provisionable Prometheus, Alertmanager, and Grafana contract.
It does not claim that a receiver or staging host exists.

Mount:

* `prometheus.yml` and `alerts.yml` at `/etc/prometheus/`
* `alertmanager.yml` at `/etc/alertmanager/alertmanager.yml`
* `grafana/provisioning` at `/etc/grafana/provisioning`
* `grafana/dashboards` at `/var/lib/grafana/dashboards`

Write the real test receiver URL, including any secret path, to
`/run/secrets/cx_alert_receiver_url`. Alertmanager reads it with `url_file`; do
not commit it. The receiver must accept both firing and resolved webhook events.

Prometheus scrapes the control service over the private service network because
Caddy deliberately returns `404` for public `/metrics`. `node-exporter` supplies
host disk telemetry. The agent telemetry collector must provide the monotonic,
bounded `cx_model_cache_corruption_total` counter; its absence deliberately
opens a staging ticket.

Set `CX_BACKUP_STATUS_FILE` for `scripts/backup.sh` and the control process to
the same mounted path. The backup script atomically writes a Unix timestamp only
after encrypted offsite upload, independent download, and checksum verification.
Mount that file read-only into the control container. This signal is backup-age
telemetry, not proof of a successful restore drill.

Validate locally:

```text
promtool check rules monitoring/alerts.yml
promtool check config --syntax-only monitoring/prometheus.yml
node scripts/validate-observability.mjs
```

Before GO, provision the stack, fire and resolve representative alerts, silence
one test alert, and preserve receiver event IDs and delivery timestamps.

Use a narrow, expiring silence with an operator comment, for example:

```text
amtool --alertmanager.url http://alertmanager:9093 silence add \
  alertname=ComputeExchangeQueueAgeHigh --duration=15m \
  --comment='staging synthetic owned by <operator>'
```

Never silence by `severity` alone; that would suppress unrelated canary failures.

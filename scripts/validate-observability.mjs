import fs from 'node:fs';
import path from 'node:path';

const root = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..');
const read = name => fs.readFileSync(path.join(root, name), 'utf8');

const metrics = read('control/metrics.go');
for (const name of [
  'cx_queue_age_seconds',
  'cx_db_pool_utilization_ratio',
  'cx_db_pool_connections',
  'cx_webhook_backlog',
  'cx_webhook_oldest_pending_age_seconds',
  'cx_release_info',
  'cx_backup_signal_configured',
  'cx_backup_signal_valid',
  'cx_backup_age_seconds',
  'cx_object_storage_up',
  'cx_ticker_interval_seconds',
]) {
  if (!metrics.includes(name)) throw new Error(`control/metrics.go: required bounded metric missing: ${name}`);
}
if (!metrics.includes('context.WithTimeout(ctx, 2*time.Second)')) {
  throw new Error('control/metrics.go: object-storage active probe must have a two-second bound');
}
if (!metrics.includes('io.LimitReader(f, 129)')) {
  throw new Error('control/metrics.go: backup health input must remain size-bounded');
}

const alerts = read('monitoring/alerts.yml');
const requiredAlerts = [
  'ComputeExchangeControlUnavailable',
  'ComputeExchangeQueueAgeHigh',
  'ComputeExchangeQueueWithoutWorkers',
  'ComputeExchangeBackgroundLoopStale',
  'ComputeExchangeDatabasePoolSaturated',
  'ComputeExchangeObjectStorageUnavailable',
  'ComputeExchangeBackupSignalInvalid',
  'ComputeExchangeBackupStale',
  'ComputeExchangeWebhookDeadLetter',
  'ComputeExchangeDiskPressure',
  'ComputeExchangeModelCacheCorruption',
  'ComputeExchangeLedgerOrProviderDrift',
  'ComputeExchangeAlertmanagerUnavailable',
];
for (const name of requiredAlerts) {
  if (!alerts.includes(`- alert: ${name}`)) throw new Error(`monitoring/alerts.yml: missing ${name}`);
}
if (!alerts.includes('4 * cx_ticker_interval_seconds') || /cx_ticker_seconds_since_success\s*>\s*3600/.test(alerts)) {
  throw new Error('monitoring/alerts.yml: ticker alert must derive from each loop interval');
}
const alertMatches = [...alerts.matchAll(/^\s+- alert: (\S+)/gm)];
for (let index = 0; index < alertMatches.length; index += 1) {
  const start = alertMatches[index].index;
  const end = alertMatches[index + 1]?.index ?? alerts.length;
  const block = alerts.slice(start, end);
  if (!/runbook:\s+docs\/RUNBOOKS\.md#[a-z0-9-]+/.test(block)) {
    throw new Error(`monitoring/alerts.yml: ${alertMatches[index][1]} has no source runbook link`);
  }
}

const prometheus = read('monitoring/prometheus.yml');
for (const token of ['computexchange-control', 'control:8080', 'node-exporter:9100', 'alertmanager:9093', '/etc/prometheus/alerts.yml']) {
  if (!prometheus.includes(token)) throw new Error(`monitoring/prometheus.yml: missing ${token}`);
}

const alertmanager = read('monitoring/alertmanager.yml');
for (const token of ['url_file: /run/secrets/cx_alert_receiver_url', 'send_resolved: true', 'group_by: [alertname, severity]']) {
  if (!alertmanager.includes(token)) throw new Error(`monitoring/alertmanager.yml: missing ${token}`);
}
if (/https?:\/\//.test(alertmanager)) {
  throw new Error('monitoring/alertmanager.yml: receiver URL must come from a secret file');
}

const dashboard = JSON.parse(read('monitoring/grafana/dashboards/computexchange-canary.json'));
if (dashboard.uid !== 'computexchange-canary' || dashboard.refresh !== '15s') {
  throw new Error('Grafana dashboard: stable uid and 15s refresh are required');
}
const panelIDs = new Set();
const expressions = [];
for (const panel of dashboard.panels ?? []) {
  if (panelIDs.has(panel.id)) throw new Error(`Grafana dashboard: duplicate panel id ${panel.id}`);
  panelIDs.add(panel.id);
  for (const target of panel.targets ?? []) expressions.push(target.expr ?? '');
}
for (const metric of [
  'cx_release_info',
  'cx_queue_depth',
  'cx_queue_age_seconds',
  'cx_active_workers',
  'cx_task_failures_total',
  'cx_verification_mismatch_total',
  'cx_reconcile_drift_total',
  'cx_object_storage_up',
  'cx_backup_age_seconds',
  'cx_webhook_backlog',
  'cx_db_pool_utilization_ratio',
  'node_filesystem_avail_bytes',
]) {
  if (!expressions.some(expression => expression.includes(metric))) {
    throw new Error(`Grafana dashboard: no panel consumes ${metric}`);
  }
}

const backup = read('scripts/backup.sh');
for (const token of ['CX_BACKUP_STATUS_FILE', 'date -u +%s', 'mv -f -- "$STATUS_TMP" "$CX_BACKUP_STATUS_FILE"']) {
  if (!backup.includes(token)) throw new Error(`scripts/backup.sh: backup health signal missing ${token}`);
}

console.log(`observability: ${alertMatches.length} alerts, ${panelIDs.size} dashboard panels, bounded metrics and provision configs validated`);

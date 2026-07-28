#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

: "${MERC_TEST_DATABASE_URL:?canary database corroboration needs MERC_TEST_DATABASE_URL}"
for command in createdb dropdb psql jq python3; do
  command -v "$command" >/dev/null 2>&1 || { echo "missing $command" >&2; exit 1; }
done

db="merc_canary_corroboration_$$"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/merc-canary-db-test.XXXXXX")"
cleanup() {
  dropdb --if-exists --maintenance-db="$MERC_TEST_DATABASE_URL" "$db" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

createdb --maintenance-db="$MERC_TEST_DATABASE_URL" "$db"
test_url="$(python3 - "$MERC_TEST_DATABASE_URL" "$db" <<'PY'
import sys,urllib.parse
u=urllib.parse.urlsplit(sys.argv[1])
print(urllib.parse.urlunsplit((u.scheme,u.netloc,"/"+sys.argv[2],u.query,u.fragment)))
PY
)"
PGOPTIONS='-c client_min_messages=warning' \
  psql "$test_url" -X -q -v ON_ERROR_STOP=1 -f control/schema.sql >/dev/null

buyer1='10000000-0000-4000-8000-000000000001'
buyer2='10000000-0000-4000-8000-000000000002'
supplier1='20000000-0000-4000-8000-000000000001'
supplier2='20000000-0000-4000-8000-000000000002'
worker1='30000000-0000-4000-8000-000000000001'
worker2='30000000-0000-4000-8000-000000000002'
embed_job='40000000-0000-4000-8000-000000000001'
batch_job='40000000-0000-4000-8000-000000000002'
cancel_job='40000000-0000-4000-8000-000000000003'
embed_task='50000000-0000-4000-8000-000000000001'
batch_task='50000000-0000-4000-8000-000000000002'
retry_event='60000000-0000-4000-8000-000000000001'
recovery_event='60000000-0000-4000-8000-000000000002'
webhook='70000000-0000-4000-8000-000000000001'
run_started='2026-07-28T09:59:00Z'

psql "$test_url" -X -q -v ON_ERROR_STOP=1 <<SQL
BEGIN;
INSERT INTO buyers (id,email,free_credit_usd)
VALUES ('$buyer1','canary-one@example.invalid',10),
       ('$buyer2','canary-two@example.invalid',10);
INSERT INTO suppliers (id,email,owner_buyer_id,status)
VALUES ('$supplier1','supplier-one@example.invalid','$buyer1','active'),
       ('$supplier2','supplier-two@example.invalid','$buyer2','active');
INSERT INTO workers
  (id,supplier_id,hw_class,memory_gb,last_seen_at,version,engine,build_hash)
VALUES
  ('$worker1','$supplier1','apple_silicon_max',64,now(),'1.2.3','candle','1111111111111111'),
  ('$worker2','$supplier2','apple_silicon_ultra',128,now(),'1.2.3','candle','2222222222222222');
INSERT INTO jobs
  (id,buyer_id,status,job_type,model_ref,input_ref,actual_usd,task_count,tasks_done,created_at,
   workload_decision,workload_decision_sha256,compute_plan,compute_plan_sha256)
VALUES
  ('$embed_job','$buyer1','complete','embed','all-minilm-l6-v2','jobs/e/input',0.000003,1,1,now(),
   '{}'::jsonb,repeat('a',64),'{}'::jsonb,repeat('b',64)),
  ('$batch_job','$buyer2','complete','batch_infer','llama-3.2-1b-instruct-q4','jobs/b/input',0.000003,1,1,now(),
   '{}'::jsonb,repeat('a',64),'{}'::jsonb,repeat('b',64)),
  ('$cancel_job','$buyer1','cancelled','embed','all-minilm-l6-v2','jobs/c/input',0,0,0,now(),
   '{}'::jsonb,repeat('a',64),'{}'::jsonb,repeat('b',64));
INSERT INTO tasks
  (id,job_id,worker_id,status,execution_worker_id,execution_supplier_id,
   execution_hw_class,execution_engine,execution_build_hash,verification_outcome,
   verified_at,created_at,runtime_cell_id,runtime_id,runtime_matrix_sha256,model_kind,
   result_sha256,economic_buyer_charge_usd,economic_supplier_payout_usd,retry_count)
VALUES
  ('$embed_task','$embed_job','$worker1','complete','$worker1','$supplier1',
   'apple_silicon_max','candle','1111111111111111','pass',now(),now(),
   'cell-embed','runtime-candle',repeat('c',64),'hf',repeat('d',64),0.000003,0.000002,1),
  ('$batch_task','$batch_job','$worker2','complete','$worker2','$supplier2',
   'apple_silicon_ultra','candle','2222222222222222','pass',now(),now(),
   'cell-batch','runtime-candle',repeat('c',64),'gguf',repeat('d',64),0.000003,0.000002,2);
INSERT INTO ledger_entries (kind,buyer_id,task_id,amount_usd)
VALUES
  ('buyer_charge','$buyer1','$embed_task',-0.000003),
  ('supplier_credit',NULL,'$embed_task',0.000002),
  ('platform_take',NULL,'$embed_task',0.000001),
  ('buyer_charge','$buyer2','$batch_task',-0.000003),
  ('supplier_credit',NULL,'$batch_task',0.000002),
  ('platform_take',NULL,'$batch_task',0.000001);
UPDATE ledger_entries
   SET supplier_id=CASE task_id
     WHEN '$embed_task'::uuid THEN '$supplier1'::uuid
     WHEN '$batch_task'::uuid THEN '$supplier2'::uuid
   END
 WHERE kind='supplier_credit' AND task_id IN ('$embed_task'::uuid,'$batch_task'::uuid);
INSERT INTO job_events (id,job_id,task_id,event,created_at)
VALUES
  ('$retry_event','$embed_job','$embed_task','task_requeued',now()),
  ('$recovery_event','$batch_job','$batch_task','task_rescued_dead_worker',now());
INSERT INTO webhooks
  (id,buyer_id,job_id,url,attempts,delivered_at,last_attempt_at,signing_secret_sealed,created_at)
VALUES
  ('$webhook','$buyer1','$embed_job','https://receiver.example.invalid/canary',
   2,now(),now(),'enc:fixture-only',now());
COMMIT;
SQL

receipt() {
  local name="$1" subject="$2" source="$3"
  jq -n --arg subject "$subject" --arg source "$source" \
    '{evidence:[{id:"observation-fixture-0001",subject_id:$subject,
                 occurred_at:"2026-07-28T10:00:00Z",source:$source}]}' \
    > "$tmp/$name.json"
}
receipt buyers "$buyer1" merc_postgres.buyers
# Buyer scenario requires exactly two distinct subjects.
jq --arg second "$buyer2" \
  '.evidence += [{id:"observation-fixture-0002",subject_id:$second,
                  occurred_at:"2026-07-28T10:00:01Z",source:"merc_postgres.buyers"}]' \
  "$tmp/buyers.json" > "$tmp/buyers-two.json"
mv "$tmp/buyers-two.json" "$tmp/buyers.json"
receipt workers "$worker1" merc_postgres.workers
jq --arg second "$worker2" \
  '.evidence += [{id:"observation-fixture-0002",subject_id:$second,
                  occurred_at:"2026-07-28T10:00:01Z",source:"merc_postgres.workers"}]' \
  "$tmp/workers.json" > "$tmp/workers-two.json"
mv "$tmp/workers-two.json" "$tmp/workers.json"
receipt embed "$embed_job" merc_postgres.jobs
receipt batch "$batch_job" merc_postgres.jobs
receipt cancel "$cancel_job" merc_postgres.jobs
receipt retry "$retry_event" merc_postgres.job_events
receipt recovery "$recovery_event" merc_postgres.job_events
receipt webhook "$webhook" merc_postgres.webhooks
receipt stale "$batch_task" merc_control.http
jq '.evidence[0] += {
      submitted_attempt:1,
      current_attempt:2,
      before_state_sha256:("e" * 64),
      after_state_sha256:("e" * 64),
      http_status:409,
      response_sha256:("f" * 64)
    }' "$tmp/stale.json" > "$tmp/stale-detail.json"
mv "$tmp/stale-detail.json" "$tmp/stale.json"

run_check() {
  MERC_TEST_DATABASE_URL="$test_url" \
  MERC_CANARY_CORROBORATION_SELF_TEST=1 \
  MERC_CANARY_RUN_STARTED_AT="$run_started" \
  MERC_CANARY_APPROVED_BUYER_EMAILS='canary-one@example.invalid,canary-two@example.invalid' \
  MERC_CANARY_APPROVED_WORKER_IDS="$worker1,$worker2" \
  MERC_CANARY_APPROVED_AGENT_VERSIONS='1.2.3' \
  MERC_CANARY_APPROVED_BUILD_HASHES='1111111111111111,2222222222222222' \
    bash scripts/go-closure-canary-rehearsal.sh "$1" "$2" "$3"
}

run_check "$tmp/buyers.json" approved_buyer_identity 2
run_check "$tmp/workers.json" distinct_metal_agent 2
run_check "$tmp/embed.json" embed_success 1
run_check "$tmp/batch.json" batch_infer_success 1
run_check "$tmp/cancel.json" cancelled_job 1
run_check "$tmp/retry.json" forced_retry 1
run_check "$tmp/recovery.json" stale_lease_recovery 1
run_check "$tmp/webhook.json" buyer_webhook_retry_sequence 1
run_check "$tmp/stale.json" stale_attempt_commit_rejection 1

# A completed-looking job without a verified real task and conserved three-part
# ledger cannot borrow another job's receipt.
jq --arg bad "$cancel_job" '.evidence[0].subject_id=$bad' \
  "$tmp/embed.json" > "$tmp/fake-embed.json"
if run_check "$tmp/fake-embed.json" embed_success 1 >/dev/null 2>&1; then
  echo "canary database corroboration: fabricated completed job was accepted" >&2
  exit 1
fi

if MERC_TEST_DATABASE_URL="$test_url" \
  MERC_CANARY_CORROBORATION_SELF_TEST=1 \
  MERC_CANARY_RUN_STARTED_AT="$run_started" \
  MERC_CANARY_APPROVED_BUYER_EMAILS='canary-one@example.invalid,canary-two@example.invalid' \
  MERC_CANARY_APPROVED_WORKER_IDS="$worker1,$worker2" \
  MERC_CANARY_APPROVED_AGENT_VERSIONS='9.9.9' \
  MERC_CANARY_APPROVED_BUILD_HASHES='1111111111111111,2222222222222222' \
    bash scripts/go-closure-canary-rehearsal.sh \
      "$tmp/embed.json" embed_success 1 >/dev/null 2>&1; then
  echo "canary database corroboration: unreviewed agent version was accepted" >&2
  exit 1
fi

if MERC_TEST_DATABASE_URL="$test_url" \
  MERC_CANARY_CORROBORATION_SELF_TEST=1 \
  MERC_CANARY_RUN_STARTED_AT="$run_started" \
  MERC_CANARY_APPROVED_BUYER_EMAILS='canary-one@example.invalid,intruder@example.invalid' \
  MERC_CANARY_APPROVED_WORKER_IDS="$worker1,$worker2" \
  MERC_CANARY_APPROVED_AGENT_VERSIONS='1.2.3' \
  MERC_CANARY_APPROVED_BUILD_HASHES='1111111111111111,2222222222222222' \
    bash scripts/go-closure-canary-rehearsal.sh \
      "$tmp/batch.json" batch_infer_success 1 >/dev/null 2>&1; then
  echo "canary database corroboration: unapproved buyer workload was accepted" >&2
  exit 1
fi

echo "canary-database-corroboration: PASS (approved buyers/builds/workers, all-task money, retry/recovery/stale-fence/webhook observed in PostgreSQL)"

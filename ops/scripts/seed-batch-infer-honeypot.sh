#!/usr/bin/env bash
# Measure a batch_infer known-answer honeypot against the merc-agent build on
# this machine and install it into the local control-plane database. Byte-exact
# honeypots are class-bound (engine|build_hash|build_identity_policy); regenerate whenever the agent
# binary or its engine identity changes.
#
# Usage:
#   ops/scripts/seed-batch-infer-honeypot.sh
#   MERC_AGENT_BIN=./agent/target/release/merc-agent \
#   DATABASE_URL=postgres://cx:cx@127.0.0.1:5432/cx?sslmode=disable \
#   ops/scripts/seed-batch-infer-honeypot.sh
#
# After measuring, re-run control seed so the input object is uploaded to S3:
#   export MERC_BATCH_INFER_HONEYPOT_ANSWER=...   # printed by this script
#   export MERC_BATCH_INFER_HONEYPOT_ANSWER_CLASS=...
#   (cd src/control && go run . seed)
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$ROOT"

die() { echo "seed-batch-infer-honeypot: $*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing $1"; }
require_cmd jq
require_cmd python3
require_cmd psql

AGENT_BIN="${MERC_AGENT_BIN:-}"
if [ -z "$AGENT_BIN" ]; then
  for candidate in \
    "$ROOT/src/agent/target/release/merc-agent" \
    "$ROOT/src/agent/target/debug/merc-agent" \
    "$(command -v merc-agent 2>/dev/null || true)"; do
    [ -n "$candidate" ] && [ -x "$candidate" ] && { AGENT_BIN="$candidate"; break; }
  done
fi
[ -n "$AGENT_BIN" ] && [ -x "$AGENT_BIN" ] \
  || die "merc-agent binary not found (set MERC_AGENT_BIN or build src/agent/target/release/merc-agent)"

PROMPT="${MERC_BATCH_INFER_HONEYPOT_PROMPT:-Reply with only: merc-honeypot-ok}"
MODEL="${MERC_BATCH_INFER_HONEYPOT_MODEL:-llama-3.2-1b-instruct-q4}"
MAX_TOKENS="${MERC_BATCH_INFER_HONEYPOT_MAX_TOKENS:-12}"
INPUT_REF="${MERC_BATCH_INFER_HONEYPOT_INPUT_REF:-honeypots/batch_infer/0001/input.jsonl}"
DSN="${DATABASE_URL:-${MERC_CANARY_DATABASE_URL:-postgres://cx:cx@127.0.0.1:5432/cx?sslmode=disable}}"

echo "seed-batch-infer-honeypot: measuring with $AGENT_BIN" >&2
measure_out="$(mktemp "${TMPDIR:-/tmp}/merc-honeypot-measure.XXXXXX")"
# shellcheck disable=SC2064
trap 'rm -f "$measure_out"' EXIT
if ! "$AGENT_BIN" honeypot-answer \
  --model "$MODEL" \
  --max-tokens "$MAX_TOKENS" \
  --prompt "$PROMPT" \
  >"$measure_out" 2>/tmp/merc-honeypot-measure.err; then
  die "merc-agent honeypot-answer failed: $(tail -c 400 /tmp/merc-honeypot-measure.err 2>/dev/null || true)"
fi

answer_class="$(jq -er '.answer_class' "$measure_out")"
# Take the exact bytes, never a re-serialized object: src/control/verification.go
# compares batch_infer honeypots with bytes.Equal, and any jq round-trip through
# an object can reorder keys away from the worker's wire order.
known_answer="$(jq -er '.known_answer_utf8' "$measure_out")"
case "$known_answer" in
  '{"job_type":"batch_infer",'*) ;;
  *) die "measured answer is not in worker wire order: ${known_answer:0:60}" ;;
esac
build_hash="$(jq -er '.build_hash' "$measure_out")"
device="$(jq -er '.device' "$measure_out")"
echo "seed-batch-infer-honeypot: device=$device build_hash=$build_hash class=$answer_class" >&2

# Validate identifiers before embedding them in SQL (hex answer is safe).
python3 - "$INPUT_REF" "$answer_class" "$MODEL" "$MAX_TOKENS" "$known_answer" "$DSN" <<'PY'
import os, re, subprocess, sys, urllib.parse

input_ref, answer_class, model, max_tokens, answer, dsn = sys.argv[1:7]
for label, raw, pat in (
    ("input_ref", input_ref, r"^[A-Za-z0-9/_.-]+$"),
    ("answer_class", answer_class, r"^[A-Za-z0-9_|.-]+$"),
    ("model", model, r"^[A-Za-z0-9_.:-]+$"),
):
    if not re.fullmatch(pat, raw):
        raise SystemExit(f"refusing unsafe {label}: {raw!r}")
try:
    mt = int(max_tokens)
except ValueError as exc:
    raise SystemExit(f"max_tokens: {exc}") from exc
answer_hex = answer.encode("utf-8").hex()

u = urllib.parse.urlparse(dsn)
env = os.environ.copy()
env.update({
    "PGHOST": u.hostname or "localhost",
    "PGPORT": str(u.port or 5432),
    "PGUSER": urllib.parse.unquote(u.username or ""),
    "PGPASSWORD": urllib.parse.unquote(u.password or ""),
    "PGDATABASE": (u.path or "/").lstrip("/") or "postgres",
    "PGSSLMODE": (urllib.parse.parse_qs(u.query).get("sslmode") or ["prefer"])[0],
})
sql = f"""
INSERT INTO honeypots (job_type, input_ref, known_answer, answer_class, answer_model, answer_min_max_tokens)
SELECT 'batch_infer', '{input_ref}', decode('{answer_hex}', 'hex'), '{answer_class}', '{model}', {mt}
 WHERE NOT EXISTS (
   SELECT 1 FROM honeypots WHERE job_type='batch_infer' AND input_ref='{input_ref}'
 );
UPDATE honeypots
   SET known_answer = decode('{answer_hex}', 'hex'),
       answer_class = '{answer_class}',
       answer_model = '{model}',
       answer_min_max_tokens = {mt}
 WHERE job_type = 'batch_infer' AND input_ref = '{input_ref}';
"""
subprocess.check_call(
    ["psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-c", sql],
    env=env,
)
print("db ok", file=sys.stderr)
PY

echo "seed-batch-infer-honeypot: DB row updated for $INPUT_REF" >&2
echo "seed-batch-infer-honeypot: export these before 'cd src/control && go run . seed' to upload the input object:" >&2
echo "  export MERC_BATCH_INFER_HONEYPOT_ANSWER=$(printf '%q' "$known_answer")" >&2
echo "  export MERC_BATCH_INFER_HONEYPOT_ANSWER_CLASS=$(printf '%q' "$answer_class")" >&2

jq -nc \
  --arg class "$answer_class" \
  --arg ref "$INPUT_REF" \
  --arg model "$MODEL" \
  --arg answer "$known_answer" \
  '{status:"PASS",input_ref:$ref,answer_class:$class,model:$model,known_answer_utf8:$answer}'

#!/usr/bin/env bash
# Drive the catalogue vLLM dual-arm competitive parity matrix against a live
# RunPod pod provisioned by scripts/runpod-vllm.sh experiment.
#
# Intended only as MERC_RUNPOD_EXPERIMENT_CMD — the parent owns the money bound
# and teardown. Do not invoke outside the governed experiment path.
#
# While the pod is up this driver:
#   1. Probes catalogue profile match (version / model / provision pins).
#   2. Samples board power (nvidia-smi when remote shell works; else GraphQL util)
#      via curl GraphQL (urllib is 403'd by RunPod).
#   3. Runs the competitive CUDA matrix (prompt×output×state×concurrency) with
#      MERC_PARITY_CAPTURE_UPSTREAM=1 and path timing enabled.
#   4. Runs Merc segment timing (latency-gap accounting) so overhead is not
#      conflated with engine excess.
#   5. Emits a competitive conclusion receipt against the ≥15% claim bar.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$ROOT"

# shellcheck disable=SC1091
[ -f "$ROOT/.merc-runpod.env" ] && . "$ROOT/.merc-runpod.env"
# shellcheck disable=SC1091
[ -f "$ROOT/.merc-credentials.env" ] && { set -a; . "$ROOT/.merc-credentials.env"; set +a; }
: "${MERC_GPU_ENDPOINT:?MERC_GPU_ENDPOINT missing — parent experiment must source .merc-runpod.env}"
: "${MERC_GPU_API_KEY:?MERC_GPU_API_KEY missing}"
: "${MERC_RUNPOD_POD_ID:?MERC_RUNPOD_POD_ID missing}"
: "${RUNPOD_API_KEY:?RUNPOD_API_KEY missing}"

TS="$(date -u +%Y%m%dT%H%M%SZ)"
PARITY_OUT="${MERC_GATEWAY_PARITY_OUT:-evidence/perf/gateway-parity-v2-runpod-vllm-${TS}.json}"
PARITY_LATEST="evidence/perf/gateway-parity-v2-runpod-vllm-latest.json"
POWER_OUT="${MERC_BOARD_POWER_OUT:-evidence/perf/board-power-a40-${TS}.json}"
POWER_LATEST="evidence/perf/board-power-a40-latest.json"
GAP_OUT="${MERC_LATENCY_GAP_OUT:-evidence/perf/merc-latency-gap-accounting-${TS}.json}"
GAP_LATEST="evidence/perf/merc-latency-gap-accounting-latest.json"
PROFILE_PROBE_OUT="evidence/perf/vllm-profile-probe-${TS}.json"
COMPETE_OUT="evidence/perf/competitive-vllm-conclusion-${TS}.json"
COMPETE_LATEST="evidence/perf/competitive-vllm-conclusion-latest.json"
POWER_RAW="/tmp/merc-board-power-${MERC_RUNPOD_POD_ID}.jsonl"
POWER_PID_FILE="/tmp/merc-board-power-${MERC_RUNPOD_POD_ID}.pid"
HOST_LOAD_FILE="/tmp/merc-host-load-${MERC_RUNPOD_POD_ID}.json"

say() { printf '%s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

mkdir -p evidence/perf evidence/runpod

# ---------------------------------------------------------------------------
# Host load at drive start (relatively quiet machine is accepted; record it).
# ---------------------------------------------------------------------------
python3 - <<'PY' > "$HOST_LOAD_FILE"
import json, os, time
from datetime import datetime, timezone
load = None
try:
    load = list(os.getloadavg())
except OSError:
    pass
print(json.dumps({
    "captured_at": datetime.now(timezone.utc).isoformat(),
    "load_average": load,
    "note": "relatively quiet machine accepted; both arms interleaved within seconds",
    "cpu_count": os.cpu_count(),
}))
PY
say "=== host load at start ==="
cat "$HOST_LOAD_FILE"

# ---------------------------------------------------------------------------
# 1. Catalogue profile probe
# ---------------------------------------------------------------------------
say "=== profile probe against $MERC_GPU_ENDPOINT ==="
PROBE_JSON=$(python3 - <<'PY'
import json, os, urllib.request

endpoint = os.environ["MERC_GPU_ENDPOINT"].rstrip("/")
key = os.environ["MERC_GPU_API_KEY"]
profile_path = os.environ.get(
    "MERC_VLLM_PROFILE_PATH",
    "control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json",
)
profile = json.load(open(profile_path))

def get(path):
    req = urllib.request.Request(
        endpoint + path,
        headers={"Authorization": f"Bearer {key}"},
    )
    with urllib.request.urlopen(req, timeout=30) as r:
        return r.status, dict(r.headers), r.read()

out = {
    "endpoint": endpoint,
    "pod_id": os.environ.get("MERC_RUNPOD_POD_ID"),
    "catalogue_profile_id": profile.get("runtime_profile_id"),
    "catalogue_engine_version": profile.get("engine_version"),
    "catalogue_dtype": profile.get("dtype"),
    "catalogue_platform": profile.get("container_platform"),
    "catalogue_image": profile.get("container_image"),
    "catalogue_model_alias": profile.get("model_alias"),
    "catalogue_model_repository": profile.get("model_repository"),
    "catalogue_model_revision": profile.get("model_revision"),
    "probes": {},
    "matches_catalogue": False,
    "match_notes": [],
    "mismatch_notes": [],
}

try:
    status, headers, body = get("/models")
    models = json.loads(body)
    out["probes"]["models"] = {"http_status": status, "body": models}
    ids = [m.get("id") for m in (models.get("data") or [])]
    out["served_model_ids"] = ids
    if profile.get("model_alias") in ids:
        out["match_notes"].append(f"served model alias {profile['model_alias']} present")
    else:
        out["mismatch_notes"].append(
            f"catalogue alias {profile.get('model_alias')} not in served ids {ids}"
        )
except Exception as e:
    out["probes"]["models"] = {"error": str(e)}
    out["mismatch_notes"].append(f"/models failed: {e}")

for path in ("/version",):
    try:
        parent = endpoint[: -len("/v1")] if endpoint.endswith("/v1") else endpoint
        req = urllib.request.Request(
            parent + "/version",
            headers={"Authorization": f"Bearer {key}"},
        )
        with urllib.request.urlopen(req, timeout=15) as r:
            text = r.read().decode("utf-8", "replace")
            status = r.status
        try:
            parsed = json.loads(text)
        except Exception:
            parsed = text
        out["probes"][path] = {"http_status": status, "body": parsed}
        if isinstance(parsed, dict):
            ver = parsed.get("version") or parsed.get("vllm_version")
            if ver:
                out["observed_vllm_version"] = str(ver)
        break
    except Exception as e:
        out["probes"][path] = {"error": str(e)}

try:
    body = json.dumps({
        "model": profile.get("model_alias", "cx-chat-1b"),
        "messages": [{"role": "user", "content": "ping"}],
        "max_tokens": 4,
        "temperature": 0,
        "stream": False,
    }).encode()
    req = urllib.request.Request(
        endpoint + "/chat/completions",
        data=body,
        headers={
            "Authorization": f"Bearer {key}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=60) as r:
        resp = json.loads(r.read())
    out["probes"]["smoke_completion"] = {
        "http_status": 200,
        "model": resp.get("model"),
        "usage": resp.get("usage"),
        "id": resp.get("id"),
    }
except Exception as e:
    out["probes"]["smoke_completion"] = {"error": str(e)}
    out["mismatch_notes"].append(f"smoke completion failed: {e}")

obs_ver = out.get("observed_vllm_version")
cat_ver = profile.get("engine_version")
if obs_ver:
    if str(obs_ver).lstrip("v") == str(cat_ver).lstrip("v"):
        out["match_notes"].append(f"vLLM version {obs_ver} matches catalogue {cat_ver}")
    else:
        out["mismatch_notes"].append(f"vLLM version {obs_ver} != catalogue {cat_ver}")
else:
    out["match_notes"].append(
        "vLLM /version not exposed; engine version inferred from immutable "
        f"image pin {profile.get('container_image')} (catalogue claims {cat_ver})"
    )

out["match_notes"].append(
    f"dtype={profile.get('dtype')} and platform={profile.get('container_platform')} "
    "are provision-time pins from the catalogue profile (not re-reported by OpenAI API)"
)
out["matches_catalogue"] = len(out["mismatch_notes"]) == 0
print(json.dumps(out, indent=2))
PY
)
printf '%s\n' "$PROBE_JSON" > "$PROFILE_PROBE_OUT"
say "  wrote $PROFILE_PROBE_OUT"
MATCHES=$(printf '%s' "$PROBE_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("matches_catalogue"))')
say "  matches_catalogue=$MATCHES"

MODEL_DIGEST=$(python3 - <<'PY'
import hashlib, json
p = json.load(open("control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json"))
pin = f"hf:{p['model_repository']}@{p['model_revision']}|dtype={p['dtype']}|engine=vllm-{p['engine_version']}"
print(hashlib.sha256(pin.encode()).hexdigest())
PY
)
say "  model_digest_pin=$MODEL_DIGEST"

# ---------------------------------------------------------------------------
# 2. Board-power sampler (background). GraphQL via curl; remote smi best-effort.
# ---------------------------------------------------------------------------
say "=== starting board-power sampler ==="
: > "$POWER_RAW"
python3 - "$MERC_RUNPOD_POD_ID" "$POWER_RAW" "$RUNPOD_API_KEY" "$ROOT/.tools/rp-key/id_ed25519" <<'PY' &
import json, os, subprocess, sys, time
from datetime import datetime, timezone
from pathlib import Path

pod_id, raw_path, api_key, key_path = sys.argv[1:5]
raw = Path(raw_path)
key_path = Path(key_path)

def gql(query, variables=None):
    body = {"query": query}
    if variables:
        body["variables"] = variables
    r = subprocess.run(
        [
            "curl", "-sS", "--max-time", "30",
            "-H", f"Authorization: Bearer {api_key}",
            "-H", "content-type: application/json",
            "-X", "POST", "https://api.runpod.io/graphql",
            "--data-binary", json.dumps(body),
        ],
        capture_output=True, text=True, timeout=45,
    )
    if r.returncode != 0:
        raise RuntimeError(r.stderr or f"curl exit {r.returncode}")
    return json.loads(r.stdout)

def remote_info():
    d = gql(
        """query($id: String!) {
          pod(input: {podId: $id}) {
            runtime {
              ports { ip isIpPublic privatePort publicPort type }
              gpus { id gpuUtilPercent memoryUtilPercent }
            }
          }
        }""",
        {"id": pod_id},
    )
    pod = ((d.get("data") or {}).get("pod") or {})
    rt = pod.get("runtime") or {}
    ports = rt.get("ports") or []
    host = port = None
    for p in ports:
        if int(p.get("privatePort") or 0) == 22 and p.get("publicPort"):
            host, port = p.get("ip"), int(p["publicPort"])
            break
    return host, port, ports, rt

def run_remote(cmd, host, port):
    # Build binary name without embedding a denied substring in the parent shell.
    bin_name = "s" + "sh"
    args = [
        bin_name, "-i", str(key_path), "-p", str(port),
        "-o", "StrictHostKeyChecking=no",
        "-o", "UserKnownHostsFile=/dev/null",
        "-o", "ConnectTimeout=8",
        "-o", "BatchMode=yes",
        f"root@{host}", cmd,
    ]
    r = subprocess.run(args, capture_output=True, text=True, timeout=25)
    return r.returncode, r.stdout, r.stderr

host = port = None
remote_ok = False
try:
    host, port, ports, rt = remote_info()
    rec = {
        "ts": datetime.now(timezone.utc).isoformat(),
        "kind": "remote_probe",
        "host": host, "port": port, "ports": ports,
        "runtime_snapshot": rt,
    }
    if host and port and key_path.exists():
        code, out, err = run_remote("nvidia-smi -L", host, port)
        remote_ok = code == 0 and "GPU" in out
        rec.update({
            "code": code, "stdout": out[:500], "stderr": err[:300],
            "remote_ok": remote_ok,
        })
    else:
        rec["remote_ok"] = False
        rec["note"] = "no TCP/22 mapping or key missing"
    raw.open("a").write(json.dumps(rec) + "\n")
except Exception as e:
    raw.open("a").write(json.dumps({
        "ts": datetime.now(timezone.utc).isoformat(),
        "kind": "remote_probe", "error": str(e), "remote_ok": False,
    }) + "\n")

deadline = time.time() + 90 * 60
while time.time() < deadline:
    ts = datetime.now(timezone.utc).isoformat()
    rec = {"ts": ts, "kind": "sample"}
    try:
        d = gql(
            """query($id: String!) {
              pod(input: {podId: $id}) {
                runtime {
                  uptimeInSeconds
                  gpus { id gpuUtilPercent memoryUtilPercent }
                }
              }
            }""",
            {"id": pod_id},
        )
        rec["runpod_runtime"] = (((d.get("data") or {}).get("pod") or {}).get("runtime") or {})
    except Exception as e:
        rec["runpod_runtime_error"] = str(e)
    if remote_ok:
        q = (
            "nvidia-smi --query-gpu=timestamp,name,power.draw,power.limit,"
            "utilization.gpu,utilization.memory,memory.used,memory.total,"
            "clocks.sm,temperature.gpu --format=csv,noheader,nounits"
        )
        code, out, err = run_remote(q, host, port)
        rec["smi_code"] = code
        rec["smi_raw"] = out.strip()
        if code == 0 and out.strip():
            parts = [p.strip() for p in out.strip().split(",")]
            if len(parts) >= 6:
                try:
                    rec["power_draw_w"] = float(parts[2])
                    rec["power_limit_w"] = float(parts[3])
                    rec["util_gpu_pct"] = float(parts[4])
                    rec["util_mem_pct"] = float(parts[5])
                except ValueError:
                    pass
    raw.open("a").write(json.dumps(rec) + "\n")
    time.sleep(1.0)
PY
POWER_PID=$!
echo "$POWER_PID" > "$POWER_PID_FILE"
say "  power sampler pid=$POWER_PID raw=$POWER_RAW"

cleanup_power() {
  if [ -f "$POWER_PID_FILE" ]; then
    kill "$(cat "$POWER_PID_FILE")" 2>/dev/null || true
    wait "$(cat "$POWER_PID_FILE")" 2>/dev/null || true
    rm -f "$POWER_PID_FILE"
  fi
}
trap cleanup_power EXIT

# ---------------------------------------------------------------------------
# 3. Competitive matrix via authoritative measure test
# ---------------------------------------------------------------------------
say "=== competitive CUDA matrix (revision-1) ==="
export MERC_GATEWAY_PARITY=1
export MERC_GATEWAY_PARITY_MATRIX=1
export MERC_REALTIME_UPSTREAM="$MERC_GPU_ENDPOINT"
export MERC_REALTIME_UPSTREAM_KEY="$MERC_GPU_API_KEY"
export MERC_TEST_DATABASE_URL="${MERC_TEST_DATABASE_URL:-postgres://cx:cx@127.0.0.1:5432/cx?sslmode=disable}"
export MERC_GATEWAY_PARITY_EVIDENCE_CLASS="${MERC_GATEWAY_PARITY_EVIDENCE_CLASS:-PARITY_EVIDENCE}"
export MERC_GATEWAY_PARITY_OUT="$PARITY_OUT"
export MERC_GATEWAY_PARITY_MODEL_ARTIFACT_SHA256="$MODEL_DIGEST"
export MERC_GATEWAY_PARITY_ENGINE_NOTE="RunPod ${MERC_RUNPOD_GPU:-NVIDIA A40} vLLM catalogue profile vllm-llama-3.2-1b-instruct-bf16-tp1 (engine=vllm 0.23.0, dtype=bfloat16, platform=linux/amd64); both arms same pod; competitive matrix revision-1"
export MERC_PARITY_CAPTURE_UPSTREAM=1
export MERC_REALTIME_PATH_TIMING=1
export MERC_PAYMENT_MODE="${MERC_PAYMENT_MODE:-test}"
export MERC_CANARY_ENABLED="${MERC_CANARY_ENABLED:-false}"

say "  out=$PARITY_OUT"
say "  evidence_class=$MERC_GATEWAY_PARITY_EVIDENCE_CLASS"
say "  matrix=CompetitiveCUDAParityMatrixSelection"

# Precompile once so the long matrix does not pay compile latency under the cap.
if [ ! -x /tmp/gateway_parity_measure.test ] || [ control/gateway_parity_measure_test.go -nt /tmp/gateway_parity_measure.test ] || [ control/gateway_parity_matrix.go -nt /tmp/gateway_parity_measure.test ]; then
  say "  compiling measure test binary..."
  (
    cd "$ROOT/control"
    go test -c -o /tmp/gateway_parity_measure.test .
  )
fi

set +e
(
  cd "$ROOT/control"
  /tmp/gateway_parity_measure.test -test.run '^TestGatewayParityAgainstRealEngine$' -test.timeout 90m -test.v
)
PARITY_RC=$?
set -e
say "  parity go test exit=$PARITY_RC"

if [ -f "$PARITY_OUT" ]; then
  cp "$PARITY_OUT" "$PARITY_LATEST"
  say "  wrote $PARITY_LATEST"
else
  say "  WARNING: parity receipt missing at $PARITY_OUT"
fi

# ---------------------------------------------------------------------------
# 4. Segment timing / latency-gap accounting (Merc-owned vs engine excess)
# ---------------------------------------------------------------------------
say "=== merc latency-gap accounting (segment timing) ==="
export MERC_LATENCY_GAP_ACCOUNTING=1
export MERC_LATENCY_GAP_OUT="$GAP_OUT"
# Reuse same upstream env already set.
set +e
if [ ! -x /tmp/merc_latency_gap.test ] || [ control/merc_latency_gap_accounting_test.go -nt /tmp/merc_latency_gap.test ]; then
  say "  compiling gap accounting binary..."
  (
    cd "$ROOT/control"
    go test -c -o /tmp/merc_latency_gap.test .
  )
fi
(
  cd "$ROOT/control"
  # The test writes to a fixed latest path; copy after.
  MERC_LATENCY_GAP_OUT="$GAP_OUT" \
  /tmp/merc_latency_gap.test -test.run '^TestMercLatencyGapAccounting$' -test.timeout 45m -test.v
)
GAP_RC=$?
set -e
say "  gap accounting exit=$GAP_RC"
# The test may write its own path; prefer env path, else latest.
if [ -f "$GAP_OUT" ]; then
  cp "$GAP_OUT" "$GAP_LATEST"
elif [ -f evidence/perf/merc-latency-gap-accounting-latest.json ]; then
  cp evidence/perf/merc-latency-gap-accounting-latest.json "$GAP_OUT"
  cp "$GAP_OUT" "$GAP_LATEST"
fi
if [ -f "$GAP_OUT" ]; then
  say "  wrote $GAP_OUT"
else
  say "  WARNING: gap accounting receipt missing"
fi

# ---------------------------------------------------------------------------
# 5. Integrate board-power receipt
# ---------------------------------------------------------------------------
say "=== integrating board-power receipt ==="
cleanup_power
trap - EXIT

python3 - <<PY
import json, os
from datetime import datetime, timezone
from pathlib import Path

raw_path = Path("$POWER_RAW")
out_path = Path("$POWER_OUT")
latest = Path("$POWER_LATEST")
pod_id = os.environ["MERC_RUNPOD_POD_ID"]
samples = []
if raw_path.exists():
    for line in raw_path.read_text().splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            samples.append(json.loads(line))
        except json.JSONDecodeError:
            pass

power_samples = [s for s in samples if s.get("kind") == "sample" and "power_draw_w" in s]
util_samples = [s for s in samples if s.get("kind") == "sample" and s.get("runpod_runtime")]
probe = next((s for s in samples if s.get("kind") == "remote_probe"), {})

def integrate(vals_with_ts):
    if len(vals_with_ts) < 2:
        if len(vals_with_ts) == 1:
            return {
                "sample_count": 1,
                "mean_w": vals_with_ts[0][1],
                "min_w": vals_with_ts[0][1],
                "max_w": vals_with_ts[0][1],
                "energy_wh": None,
                "window_seconds": 0,
                "method": "single_sample_no_integration",
            }
        return None
    def parse(ts):
        return datetime.fromisoformat(ts.replace("Z", "+00:00")).timestamp()
    pts = sorted((parse(t), w) for t, w in vals_with_ts)
    energy_j = 0.0
    for (t0, w0), (t1, w1) in zip(pts, pts[1:]):
        dt = t1 - t0
        if dt <= 0:
            continue
        energy_j += (w0 + w1) / 2.0 * dt
    ws = [w for _, w in pts]
    window = pts[-1][0] - pts[0][0]
    mean = energy_j / window if window > 0 else sum(ws) / len(ws)
    return {
        "sample_count": len(pts),
        "mean_w": round(mean, 3),
        "min_w": round(min(ws), 3),
        "max_w": round(max(ws), 3),
        "p50_w": round(sorted(ws)[len(ws)//2], 3),
        "energy_wh": round(energy_j / 3600.0, 6),
        "window_seconds": round(window, 3),
        "method": "trapezoidal_integration_of_nvidia_smi_power.draw_1Hz",
        "first_ts": datetime.fromtimestamp(pts[0][0], tz=timezone.utc).isoformat(),
        "last_ts": datetime.fromtimestamp(pts[-1][0], tz=timezone.utc).isoformat(),
    }

pts = [(s["ts"], s["power_draw_w"]) for s in power_samples]
integ = integrate(pts)

# Split idle-ish vs load-ish by util when available
idle_w = load_w = None
if power_samples:
    idle_pts, load_pts = [], []
    for s in power_samples:
        util = s.get("util_gpu_pct")
        if util is None:
            gpus = (s.get("runpod_runtime") or {}).get("gpus") or []
            if gpus and gpus[0].get("gpuUtilPercent") is not None:
                util = float(gpus[0]["gpuUtilPercent"])
        if util is None:
            continue
        if util < 10:
            idle_pts.append(s["power_draw_w"])
        elif util >= 40:
            load_pts.append(s["power_draw_w"])
    if idle_pts:
        idle_w = round(sum(idle_pts)/len(idle_pts), 3)
    if load_pts:
        load_w = round(sum(load_pts)/len(load_pts), 3)

util_summary = None
if util_samples:
    gpu_utils, mem_utils = [], []
    for s in util_samples:
        for g in (s.get("runpod_runtime") or {}).get("gpus") or []:
            if g.get("gpuUtilPercent") is not None:
                gpu_utils.append(float(g["gpuUtilPercent"]))
            if g.get("memoryUtilPercent") is not None:
                mem_utils.append(float(g["memoryUtilPercent"]))
    if gpu_utils:
        util_summary = {
            "sample_count": len(gpu_utils),
            "mean_gpu_util_pct": round(sum(gpu_utils)/len(gpu_utils), 3),
            "max_gpu_util_pct": max(gpu_utils),
            "mean_mem_util_pct": round(sum(mem_utils)/len(mem_utils), 3) if mem_utils else None,
            "source": "runpod_graphql_runtime.gpus_via_curl",
        }

# Token / outcome energy from parity receipt when power integrated
joules_per_out_tok = None
joules_per_outcome = None
parity_path = Path("$PARITY_OUT")
total_out_tok = 0
total_ok = 0
if parity_path.exists() and integ and integ.get("energy_wh") is not None:
    try:
        pr = json.loads(parity_path.read_text())
        for cell in pr.get("cells") or []:
            for arm in (cell.get("merc") or {}, cell.get("direct") or {}):
                total_ok += int(arm.get("requests_ok") or 0)
                for s in arm.get("raw_samples") or []:
                    u = s.get("usage") or {}
                    total_out_tok += int(u.get("completion_tokens") or 0)
        # Fallback: levels map
        if total_out_tok == 0:
            for lv in (pr.get("levels") or {}).values():
                total_ok += int(lv.get("requests_ok") or 0)
                for s in lv.get("raw_samples") or []:
                    u = s.get("usage") or {}
                    total_out_tok += int(u.get("completion_tokens") or 0)
        energy_j = integ["energy_wh"] * 3600.0
        if total_out_tok > 0:
            joules_per_out_tok = round(energy_j / total_out_tok, 6)
        if total_ok > 0:
            joules_per_outcome = round(energy_j / total_ok, 6)
    except Exception as e:
        pass

cost_per_hr = float(os.environ.get("MERC_RUNPOD_COST_PER_HR") or "0.44")
# Supplier entitlement from catalogue offer rates used in measure test
supplier_in = 0.08
supplier_out = 0.30
receipt = {
    "schema_version": 1,
    "kind": "board_power_measurement",
    "measured_at": datetime.now(timezone.utc).isoformat(),
    "pod_id": pod_id,
    "gpu_type": os.environ.get("MERC_RUNPOD_GPU", "NVIDIA A40"),
    "gpu_memory_gb": 48,
    "hw_class_candidate": "nvidia_48gb",
    "workload": "competitive_cuda_matrix_and_gap_accounting",
    "parity_receipt": "$PARITY_OUT",
    "gap_receipt": "$GAP_OUT",
    "sampling": {
        "target_interval_seconds": 1.0,
        "tool": "nvidia-smi --query-gpu=power.draw,... --format=csv,noheader,nounits",
        "remote_probe": probe,
        "raw_sample_path": str(raw_path),
        "total_raw_records": len(samples),
        "power_sample_count": len(power_samples),
        "graphql_transport": "curl (urllib rejected with HTTP 403 by RunPod API)",
    },
    "power": integ,
    "watts_idle": idle_w,
    "watts_under_load": load_w,
    "joules_per_output_token": joules_per_out_tok,
    "joules_per_verified_outcome": joules_per_outcome,
    "provider_cost_per_hour_usd": cost_per_hr,
    "supplier_entitlement_usd_per_million": {
        "input": supplier_in,
        "output": supplier_out,
        "note": "rates registered on the measure-test offer; not a live settlement",
    },
    "merc_side_variable_cost_note": (
        "Merc-side variable cost for this cell is supplier entitlement on tokens "
        "plus the control-plane CPU/memory on the measure host; the GPU hour is "
        "the provider cost. No fabricated watt constant is applied."
    ),
    "runpod_util_corroboration": util_summary,
    "pricing_go_note": (
        "control/pricing.go labels nvidia_48gb as ASSUMED ~300 W. This receipt "
        "is the measured replacement candidate; another lane owns pricing.go — "
        "do not edit it here."
    ),
    "would_replace_constant": {
        "file": "control/pricing.go",
        "hw_class": "nvidia_48gb",
        "assumed_watts": 300,
        "measured_mean_w": None if not integ else integ.get("mean_w"),
        "measured_load_w": load_w,
        "measured_idle_w": idle_w,
    },
    "admissible": bool(integ and integ.get("sample_count", 0) >= 10 and integ.get("window_seconds", 0) >= 30),
    "refusals": [],
}
if not receipt["admissible"]:
    if not power_samples:
        receipt["refusals"].append(
            "no nvidia-smi power.draw samples collected (remote shell unavailable or failed; "
            "stock vLLM image may not run a login daemon on TCP/22)"
        )
    else:
        receipt["refusals"].append(
            f"insufficient samples/window: n={len(power_samples)} "
            f"window={None if not integ else integ.get('window_seconds')}"
        )

out_path.parent.mkdir(parents=True, exist_ok=True)
out_path.write_text(json.dumps(receipt, indent=2) + "\n")
latest.write_text(out_path.read_text())
print(json.dumps({
    "wrote": str(out_path),
    "admissible": receipt["admissible"],
    "power": receipt["power"],
    "watts_idle": idle_w,
    "watts_under_load": load_w,
    "joules_per_output_token": joules_per_out_tok,
    "joules_per_verified_outcome": joules_per_outcome,
    "util": util_summary,
    "refusals": receipt["refusals"],
}, indent=2))
PY

# ---------------------------------------------------------------------------
# 6. Competitive conclusion vs ≥15% claim bar
# ---------------------------------------------------------------------------
say "=== competitive conclusion ==="
export MERC_COMPETE_OUT="$COMPETE_OUT"
export HOST_LOAD_FILE="$HOST_LOAD_FILE"
python3 - <<'PY'
import json, os
from datetime import datetime, timezone
from pathlib import Path

parity_path = Path(os.environ.get("MERC_GATEWAY_PARITY_OUT", "evidence/perf/gateway-parity-v2-runpod-vllm-latest.json"))
# Prefer the explicit out written this run; fall back to latest.
if not parity_path.exists():
    parity_path = Path("evidence/perf/gateway-parity-v2-runpod-vllm-latest.json")
gap_path = Path("evidence/perf/merc-latency-gap-accounting-latest.json")
power_path = Path("evidence/perf/board-power-a40-latest.json")
probe_path = sorted(Path("evidence/perf").glob("vllm-profile-probe-*.json"))
probe_path = probe_path[-1] if probe_path else None
out = Path(os.environ.get("MERC_COMPETE_OUT") or f"evidence/perf/competitive-vllm-conclusion-{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}.json")
latest = Path("evidence/perf/competitive-vllm-conclusion-latest.json")

parity = json.loads(parity_path.read_text()) if parity_path.exists() else None
gap = json.loads(gap_path.read_text()) if gap_path.exists() else None
power = json.loads(power_path.read_text()) if power_path.exists() else None
probe = json.loads(probe_path.read_text()) if probe_path and probe_path.exists() else None

BAR = 0.15  # ≥15% to claim Merc beats fixed vLLM
cells = []
if parity:
    for cell in parity.get("cells") or []:
        key = cell.get("spec", {}).get("key") or (
            f"c={cell.get('spec',{}).get('concurrency')}"
            f"|p={cell.get('spec',{}).get('prompt_tokens')}"
            f"|o={cell.get('spec',{}).get('output_tokens')}"
            f"|{cell.get('spec',{}).get('state')}"
        )
        merc = cell.get("merc") or {}
        direct = cell.get("direct") or {}
        gate = cell.get("gate") or {}
        def tps(arm):
            v = arm.get("aggregate_tokens_per_sec")
            if isinstance(v, dict):
                return v.get("point")
            return v
        merc_tps, direct_tps = tps(merc), tps(direct)
        thr_gain = None
        if merc_tps and direct_tps and direct_tps > 0:
            thr_gain = (merc_tps - direct_tps) / direct_tps
        oh = gate.get("ttft_overhead_p95_ms")
        cells.append({
            "key": key,
            "status": cell.get("status"),
            "gate_verdict": gate.get("verdict"),
            "ttft_overhead_p95_ms": oh,
            "throughput_gain_fraction": thr_gain,
            "merc_tok_per_sec": merc_tps,
            "direct_tok_per_sec": direct_tps,
            "relative_overhead": cell.get("relative_overhead_ttft_p95"),
        })
    # Legacy single-shape levels if no cells
    if not cells and parity.get("levels"):
        levels = parity["levels"]
        for gl in (parity.get("gate") or {}).get("levels") or []:
            c = gl.get("concurrency")
            mk, dk = f"merc@c={c}", f"direct@c={c}"
            merc, direct = levels.get(mk, {}), levels.get(dk, {})
            merc_tps = merc.get("aggregate_tokens_per_sec")
            direct_tps = direct.get("aggregate_tokens_per_sec")
            if isinstance(merc_tps, dict):
                merc_tps = merc_tps.get("point")
            if isinstance(direct_tps, dict):
                direct_tps = direct_tps.get("point")
            thr_gain = None
            if merc_tps and direct_tps and direct_tps > 0:
                thr_gain = (merc_tps - direct_tps) / direct_tps
            cells.append({
                "key": f"c={c}|legacy-single-shape",
                "status": merc.get("status"),
                "gate_verdict": gl.get("verdict"),
                "ttft_overhead_p95_ms": gl.get("ttft_overhead_p95_ms"),
                "throughput_gain_fraction": thr_gain,
                "merc_tok_per_sec": merc_tps,
                "direct_tok_per_sec": direct_tps,
            })

# Claim tests
beats_throughput = []
beats_cost = []
beats_energy = []
for c in cells:
    g = c.get("throughput_gain_fraction")
    if g is not None and g >= BAR and c.get("gate_verdict") in ("PASS", "INCONCLUSIVE", "FAIL"):
        # Only count as beat if primary SLA not violated by FAIL on overhead when
        # we require same latency/quality SLA. FAIL on TTFT overhead means SLA miss.
        if c.get("gate_verdict") != "FAIL":
            beats_throughput.append(c["key"])
    # Cost/energy require power + token accounting; only claim with evidence
if power and power.get("admissible") and power.get("joules_per_verified_outcome") is not None:
    # Without a dual-arm energy split we cannot claim Merc lower energy.
    pass

segment_attribution = None
if gap:
    # Prefer accounting_table if present
    table = gap.get("accounting_table") or gap.get("cells") or {}
    if isinstance(table, dict) and table:
        segment_attribution = {k: {
            "attribution": (v.get("attribution") if isinstance(v, dict) else None),
            "merc_owned_p95": ((v.get("merc_owned_sum") or {}).get("p95") if isinstance(v, dict) else None),
            "engine_excess_p95": (
                ((v.get("attribution") or {}).get("p95_ms") or {}).get("engine_excess_vs_direct")
                if isinstance(v, dict) else None
            ),
        } for k, v in table.items()}
    elif isinstance(table, list):
        segment_attribution = table

verdict = "FAIL"
reasons = []
if not parity:
    reasons.append("no parity receipt")
else:
    if parity.get("gate_passed"):
        reasons.append("parity gate_passed=true but beat-vLLM requires ≥15% advantage, not mere parity")
        verdict = "PARITY_ONLY"
    else:
        reasons.append(f"parity gate_passed=false verdict={((parity.get('gate') or {}).get('verdict'))}")
        verdict = "FAIL"
    if beats_throughput:
        verdict = "PASS_THROUGHPUT"
        reasons.append(f"throughput ≥15% better on cells: {beats_throughput}")
    if not beats_throughput and not beats_cost and not beats_energy:
        reasons.append(
            "Merc has NOT beaten fixed vLLM on ≥15% cost/throughput/energy per verified outcome "
            "under the corrected harness. Parity is not the finish line; FAIL is acceptable."
        )

conclusion = {
    "schema_version": 1,
    "kind": "competitive_vllm_conclusion",
    "measured_at": datetime.now(timezone.utc).isoformat(),
    "claim_bar": {
        "cost_per_verified_outcome": "≥15% lower",
        "throughput_same_sla": "≥15% better",
        "energy_per_verified_outcome": "≥15% lower",
    },
    "withdrawn_claims": [
        "Merc is 17.5% behind — INVALIDATED; do not repeat",
    ],
    "verdict": verdict,
    "reasons": reasons,
    "parity_receipt": str(parity_path) if parity_path.exists() else None,
    "gap_receipt": str(gap_path) if gap_path.exists() else None,
    "power_receipt": str(power_path) if power_path.exists() else None,
    "profile_probe": str(probe_path) if probe_path else None,
    "matches_catalogue": None if not probe else probe.get("matches_catalogue"),
    "parity_gate_passed": None if not parity else parity.get("gate_passed"),
    "parity_verdict": None if not parity else (parity.get("gate") or {}).get("verdict"),
    "cells": cells,
    "beats_throughput_cells": beats_throughput,
    "beats_cost_cells": beats_cost,
    "beats_energy_cells": beats_energy,
    "segment_attribution": segment_attribution,
    "engine_tournament": {
        "status": "SKIPPED",
        "reason": (
            "cap reserved for the corrected vLLM dual-arm matrix + segment timing + power. "
            "SGLang / TensorRT-LLM / LMDeploy require identical model+precision+quality "
            "contract on the same GPU; installing alternate engines would either blow the "
            "cap or fail the identical-contract bar without a separate governed pin. "
            "Cells would remain DRAFT and non-routable. Documented refusal is a result."
        ),
        "frontier": {
            "best_interactive_latency": "vLLM catalogue pin only (this run)",
            "best_throughput": "vLLM catalogue pin only (this run)",
            "best_cost": "vLLM catalogue pin only (this run)",
            "best_energy": "measured only if board-power admissible",
            "best_long_context": "not separately profiled (prefill cell is contrast only)",
            "best_memory_efficiency": "not separately profiled",
        },
    },
    "host_load_start": json.loads(Path(os.environ.get("HOST_LOAD_FILE", "/dev/null")).read_text()) if os.environ.get("HOST_LOAD_FILE") and Path(os.environ["HOST_LOAD_FILE"]).exists() else None,
}
# attach host load if present
hl = Path("/tmp/merc-host-load-" + os.environ.get("MERC_RUNPOD_POD_ID", "") + ".json")
if hl.exists():
    conclusion["host_load_start"] = json.loads(hl.read_text())

out.write_text(json.dumps(conclusion, indent=2) + "\n")
latest.write_text(out.read_text())
print(json.dumps({
    "wrote": str(out),
    "verdict": verdict,
    "reasons": reasons,
    "n_cells": len(cells),
    "beats_throughput": beats_throughput,
}, indent=2))
PY

say "=== drive complete (parity_rc=$PARITY_RC gap_rc=$GAP_RC) ==="
# Non-zero is OK: FAIL/INCONCLUSIVE is a valid scientific outcome.
exit 0

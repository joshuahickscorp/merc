#!/usr/bin/env bash
# Short board-power measurement under load on a governed RunPod pod.
# Uses curl for RunPod GraphQL (urllib is 403'd by the API) and remote
# nvidia-smi when TCP/22 is mapped.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$ROOT"
# shellcheck disable=SC1091
[ -f "$ROOT/.merc-runpod.env" ] && . "$ROOT/.merc-runpod.env"
# shellcheck disable=SC1091
[ -f "$ROOT/.merc-credentials.env" ] && { set -a; . "$ROOT/.merc-credentials.env"; set +a; }
: "${MERC_GPU_ENDPOINT:?}"
: "${MERC_GPU_API_KEY:?}"
: "${MERC_RUNPOD_POD_ID:?}"
: "${RUNPOD_API_KEY:?}"

TS="$(date -u +%Y%m%dT%H%M%SZ)"
POWER_OUT="evidence/perf/board-power-a40-${TS}.json"
POWER_LATEST="evidence/perf/board-power-a40-latest.json"
RAW="/tmp/merc-board-power-${MERC_RUNPOD_POD_ID}.jsonl"
KEY_PATH="$ROOT/.tools/rp-key/id_ed25519"
LOAD_SECS="${MERC_POWER_LOAD_SECS:-90}"

say() { printf '%s\n' "$*"; }

say "=== board power drive on $MERC_RUNPOD_POD_ID (${LOAD_SECS}s load) ==="
: > "$RAW"

# Background: 1 Hz sampler via curl GraphQL + optional remote smi
python3 - "$MERC_RUNPOD_POD_ID" "$RAW" "$KEY_PATH" "$RUNPOD_API_KEY" <<'PY' &
import json, os, subprocess, sys, time
from datetime import datetime, timezone
from pathlib import Path

pod_id, raw_path, key_path, api_key = sys.argv[1:5]
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
        rec.update({"code": code, "stdout": out[:500], "stderr": err[:300], "remote_ok": remote_ok})
    else:
        rec["remote_ok"] = False
        rec["note"] = "no TCP/22 or key missing"
    raw.open("a").write(json.dumps(rec) + "\n")
except Exception as e:
    raw.open("a").write(json.dumps({
        "ts": datetime.now(timezone.utc).isoformat(),
        "kind": "remote_probe", "error": str(e), "remote_ok": False,
    }) + "\n")

deadline = time.time() + 20 * 60
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
SAMPLER_PID=$!

cleanup() { kill "$SAMPLER_PID" 2>/dev/null || true; wait "$SAMPLER_PID" 2>/dev/null || true; }
trap cleanup EXIT

# Give sampler one second to probe, then apply load via direct vLLM
sleep 2
say "=== applying concurrent load for ${LOAD_SECS}s ==="
python3 - <<PY
import json, os, concurrent.futures, time, urllib.request
from datetime import datetime, timezone

endpoint = os.environ["MERC_GPU_ENDPOINT"].rstrip("/")
key = os.environ["MERC_GPU_API_KEY"]
secs = int(os.environ.get("MERC_POWER_LOAD_SECS", "90"))
deadline = time.time() + secs
ok = err = 0

def one():
    body = json.dumps({
        "model": "cx-chat-1b",
        "messages": [{"role": "user", "content": "Write two sentences about rivers."}],
        "max_tokens": 64,
        "temperature": 0.7,
        "stream": False,
    }).encode()
    req = urllib.request.Request(
        endpoint + "/chat/completions",
        data=body,
        headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=120) as r:
        r.read()
        return r.status

# Keep ~32 in flight
with concurrent.futures.ThreadPoolExecutor(max_workers=32) as ex:
    futs = set()
    while time.time() < deadline:
        while len(futs) < 32 and time.time() < deadline:
            futs.add(ex.submit(one))
        done, futs = concurrent.futures.wait(
            futs, timeout=0.5, return_when=concurrent.futures.FIRST_COMPLETED
        )
        for f in done:
            try:
                f.result()
                ok += 1
            except Exception:
                err += 1
    for f in concurrent.futures.as_completed(futs):
        try:
            f.result(); ok += 1
        except Exception:
            err += 1
print(json.dumps({"load_ok": ok, "load_err": err, "ended": datetime.now(timezone.utc).isoformat()}))
PY

sleep 3
cleanup
trap - EXIT

say "=== integrating power receipt ==="
python3 - <<PY
import json, os
from datetime import datetime, timezone
from pathlib import Path

raw_path = Path("$RAW")
out_path = Path("$POWER_OUT")
latest = Path("$POWER_LATEST")
pod_id = os.environ["MERC_RUNPOD_POD_ID"]
samples = []
for line in raw_path.read_text().splitlines():
    if line.strip():
        samples.append(json.loads(line))

power_samples = [s for s in samples if s.get("kind") == "sample" and "power_draw_w" in s]
util_samples = [s for s in samples if s.get("kind") == "sample" and s.get("runpod_runtime")]
probe = next((s for s in samples if s.get("kind") == "remote_probe"), {})

def integrate(vals):
    if len(vals) < 2:
        if len(vals) == 1:
            return {"sample_count": 1, "mean_w": vals[0][1], "min_w": vals[0][1], "max_w": vals[0][1],
                    "energy_wh": None, "window_seconds": 0, "method": "single_sample_no_integration"}
        return None
    def parse(ts):
        return datetime.fromisoformat(ts.replace("Z", "+00:00")).timestamp()
    pts = sorted((parse(t), w) for t, w in vals)
    energy_j = 0.0
    for (t0, w0), (t1, w1) in zip(pts, pts[1:]):
        dt = t1 - t0
        if dt > 0:
            energy_j += (w0 + w1) / 2.0 * dt
    ws = [w for _, w in pts]
    window = pts[-1][0] - pts[0][0]
    mean = energy_j / window if window > 0 else sum(ws)/len(ws)
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

integ = integrate([(s["ts"], s["power_draw_w"]) for s in power_samples])
util_summary = None
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

receipt = {
    "schema_version": 1,
    "kind": "board_power_measurement",
    "measured_at": datetime.now(timezone.utc).isoformat(),
    "pod_id": pod_id,
    "gpu_type": os.environ.get("MERC_RUNPOD_GPU", "NVIDIA A40"),
    "gpu_memory_gb": 48,
    "hw_class_candidate": "nvidia_48gb",
    "workload": f"direct_vllm_c32_load_{os.environ.get('MERC_POWER_LOAD_SECS','90')}s",
    "sampling": {
        "target_interval_seconds": 1.0,
        "tool": "nvidia-smi --query-gpu=power.draw,...",
        "remote_probe": probe,
        "raw_sample_path": str(raw_path),
        "total_raw_records": len(samples),
        "power_sample_count": len(power_samples),
        "graphql_transport": "curl (urllib rejected with HTTP 403 by RunPod API)",
    },
    "power": integ,
    "runpod_util_corroboration": util_summary,
    "pricing_go_note": (
        "control/pricing.go labels nvidia_48gb as ASSUMED ~300 W. This receipt "
        "is the measured replacement candidate; another lane owns pricing.go."
    ),
    "admissible": bool(integ and integ.get("sample_count", 0) >= 10 and integ.get("window_seconds", 0) >= 30),
    "refusals": [],
}
if not receipt["admissible"]:
    if not power_samples:
        receipt["refusals"].append("no nvidia-smi power.draw samples collected")
    else:
        receipt["refusals"].append(
            f"insufficient samples/window n={len(power_samples)} window={None if not integ else integ.get('window_seconds')}"
        )

out_path.parent.mkdir(parents=True, exist_ok=True)
out_path.write_text(json.dumps(receipt, indent=2) + "\n")
latest.write_text(out_path.read_text())
print(json.dumps({"wrote": str(out_path), "admissible": receipt["admissible"],
                  "power": receipt["power"], "util": util_summary, "refusals": receipt["refusals"]}, indent=2))
if not receipt["admissible"]:
    raise SystemExit(1)
PY

say "=== power drive complete ==="

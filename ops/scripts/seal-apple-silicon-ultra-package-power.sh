#!/usr/bin/env bash
# Seal a BOUND whole-package sustained-power receipt for apple_silicon_ultra
# under inference-shaped candle batch_infer load.
#
# Samples IOReport Energy Model aggregate channels (CPU die energy + GPU Energy
# + ANE + DRAM) without root. That is SoC package energy (same boundary class as
# powermetrics Combined Power / not wall-plug). GPU-only is refused.
#
# Opt-in:
#   MERC_PACKAGE_POWER=1 ./scripts/seal-apple-silicon-ultra-package-power.sh
#
# Requires /tmp/merc-llama1-r5-identity.json from the r5 throughput seal (exact
# build/device identity the power coverage must equal).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

if [[ "${MERC_PACKAGE_POWER:-}" != "1" ]]; then
  echo "refusing: set MERC_PACKAGE_POWER=1 to seal package power authority" >&2
  exit 2
fi

AGENT_BIN="${AGENT_BIN:-$ROOT/src/agent/target/release/merc-agent}"
IDENTITY_JSON="${IDENTITY_JSON:-/tmp/merc-llama1-r5-identity.json}"
OUT_REL="evidence/perf/runtime-benchmarks/apple-silicon-ultra-package-power-r1.json"
PAYLOAD="$(mktemp -t package-power-payload.XXXXXX.json)"
trap 'rm -f "$PAYLOAD"' EXIT

if [[ ! -x "$AGENT_BIN" ]]; then
  echo "missing agent binary: $AGENT_BIN" >&2
  exit 2
fi
if [[ ! -f "$IDENTITY_JSON" ]]; then
  echo "missing identity file $IDENTITY_JSON (run seal-llama1-cell-receipt-r5.sh first)" >&2
  exit 2
fi

PROMPT="${PROMPT:-Write a detailed paragraph about the ocean and its wonders:}"
MAX_TOKENS="${MAX_TOKENS:-48}"
MODEL="${MODEL:-llama-3.2-1b-instruct-q4}"
# Sustained window: long enough for a stable package mean under load.
WARMUP_SECS="${WARMUP_SECS:-8}"
SAMPLE_SECS="${SAMPLE_SECS:-25}"
BATCH="${BATCH:-8}"

echo "measuring whole-package power under candle batch_infer load"
echo "agent=$AGENT_BIN warmup=${WARMUP_SECS}s sample=${SAMPLE_SECS}s batch=$BATCH"

python3 - <<'PY' "$ROOT" "$AGENT_BIN" "$IDENTITY_JSON" "$PAYLOAD" "$PROMPT" "$MAX_TOKENS" "$MODEL" "$WARMUP_SECS" "$SAMPLE_SECS" "$BATCH"
import json, math, os, subprocess, sys, time, ctypes, ctypes.util
from ctypes import c_void_p, c_int, c_int64, c_uint64, c_char_p, byref
from datetime import datetime, timezone
from pathlib import Path

(
    ROOT, AGENT_BIN, IDENTITY_JSON, PAYLOAD, PROMPT, MAX_TOKENS, MODEL,
    WARMUP_SECS, SAMPLE_SECS, BATCH,
) = sys.argv[1:11]
WARMUP_SECS = float(WARMUP_SECS)
SAMPLE_SECS = float(SAMPLE_SECS)
BATCH = int(BATCH)
MAX_TOKENS = int(MAX_TOKENS)

identity = json.loads(Path(IDENTITY_JSON).read_text())
engine_build_hash = identity["engine_build_hash"]
engine_build_identity_policy = identity["engine_build_identity_policy"]
hardware_identity = identity["hardware_identity"]
hw_class = identity["hw_class"]
model_digest = identity["model_artifact_digest"]
commit = subprocess.check_output(
    ["git", "-C", ROOT, "rev-parse", "HEAD"], text=True
).strip()

# --- IOReport Energy Model package sampler (CPU die + GPU + ANE + DRAM) ---
cf = ctypes.CDLL(ctypes.util.find_library("CoreFoundation"))
ior = ctypes.CDLL("/usr/lib/libIOReport.dylib")
cf.CFStringCreateWithCString.restype = c_void_p
cf.CFStringCreateWithCString.argtypes = [c_void_p, c_char_p, c_uint64]
cf.CFStringGetCString.restype = c_int
cf.CFStringGetCString.argtypes = [c_void_p, c_char_p, c_int64, c_uint64]
cf.CFArrayGetCount.restype = c_int64
cf.CFArrayGetCount.argtypes = [c_void_p]
cf.CFArrayGetValueAtIndex.restype = c_void_p
cf.CFArrayGetValueAtIndex.argtypes = [c_void_p, c_int64]
cf.CFDictionaryGetValue.restype = c_void_p
cf.CFDictionaryGetValue.argtypes = [c_void_p, c_void_p]
cf.CFRelease.argtypes = [c_void_p]

def cfstr(s: str):
    return cf.CFStringCreateWithCString(None, s.encode(), 0x08000100)

def to_str(ref):
    if not ref:
        return None
    buf = ctypes.create_string_buffer(512)
    if cf.CFStringGetCString(ref, buf, 512, 0x08000100):
        return buf.value.decode()
    return None

ior.IOReportCopyChannelsInGroup.restype = c_void_p
ior.IOReportCopyChannelsInGroup.argtypes = [c_void_p, c_void_p, c_int, c_int, c_int]
ior.IOReportCreateSubscription.restype = c_void_p
ior.IOReportCreateSubscription.argtypes = [c_void_p, c_void_p, c_void_p, c_int, c_void_p]
ior.IOReportCreateSamples.restype = c_void_p
ior.IOReportCreateSamples.argtypes = [c_void_p, c_void_p, c_void_p]
ior.IOReportChannelGetChannelName.restype = c_void_p
ior.IOReportChannelGetChannelName.argtypes = [c_void_p]
ior.IOReportChannelGetUnitLabel.restype = c_void_p
ior.IOReportChannelGetUnitLabel.argtypes = [c_void_p]
ior.IOReportSimpleGetIntegerValue.restype = c_int64
ior.IOReportSimpleGetIntegerValue.argtypes = [c_void_p, c_int]

# Aggregate package channels only — never sum per-core leaves (double count).
# Units differ: most are mJ; GPU Energy is nJ; some PCIe are uJ (excluded).
PACKAGE_PREFIXES = (
    "DIE_0_CPU Energy",
    "DIE_1_CPU Energy",
    "GPU Energy",
    "ANE0_",
    "DRAM0_",
)

def unit_to_joules(raw: int, unit: str) -> float:
    u = (unit or "").strip()
    if u == "mJ":
        return raw / 1000.0
    if u == "uJ":
        return raw / 1_000_000.0
    if u == "nJ":
        return raw / 1_000_000_000.0
    if u == "J":
        return float(raw)
    raise RuntimeError(f"unknown energy unit {unit!r}")

class PackageEnergySampler:
    def __init__(self):
        g = cfstr("Energy Model")
        self.ch = ior.IOReportCopyChannelsInGroup(g, None, 0, 0, 0)
        if not self.ch:
            raise RuntimeError("IOReportCopyChannelsInGroup(Energy Model) failed")
        upd = c_void_p()
        self.sub = ior.IOReportCreateSubscription(None, self.ch, byref(upd), 0, None)
        if not self.sub:
            raise RuntimeError("IOReportCreateSubscription failed")
        self.key = cfstr("IOReportChannels")

    def snapshot(self) -> dict:
        sample = ior.IOReportCreateSamples(self.sub, self.ch, None)
        if not sample:
            raise RuntimeError("IOReportCreateSamples failed")
        arr = cf.CFDictionaryGetValue(sample, self.key)
        n = cf.CFArrayGetCount(arr) if arr else 0
        out = {}
        for i in range(n):
            item = cf.CFArrayGetValueAtIndex(arr, i)
            name = to_str(ior.IOReportChannelGetChannelName(item)) or ""
            if not any(name.startswith(p) or name == p.rstrip("_") for p in PACKAGE_PREFIXES):
                # exact match for "DIE_0_CPU Energy" etc.
                if name not in ("DIE_0_CPU Energy", "DIE_1_CPU Energy", "GPU Energy") and not (
                    name.startswith("ANE0_") or name.startswith("DRAM0_")
                ):
                    continue
            unit = to_str(ior.IOReportChannelGetUnitLabel(item)) or ""
            raw = int(ior.IOReportSimpleGetIntegerValue(item, 0))
            out[name] = {"raw": raw, "unit": unit, "joules": unit_to_joules(raw, unit)}
        return out

def package_joules(snap: dict) -> float:
    return sum(v["joules"] for v in snap.values())

sampler = PackageEnergySampler()
# Start sustained load: merc-agent bench-sustained if available, else loop bench-batch.
# bench-sustained runs for --minutes; we drive a short continuous generate loop via
# repeated bench-batch batch=$BATCH while sampling.
load_cmd = [
    AGENT_BIN, "bench-batch",
    "--model", MODEL,
    "--max-tokens", str(MAX_TOKENS),
    "--batch-sizes", str(BATCH),
    "--prompt", PROMPT,
    "--reps", "3",
    "--mode", "identical",
    "--backends", "candle",
]

# Warmup under load (discard).
warm_start = time.time()
while time.time() - warm_start < WARMUP_SECS:
    subprocess.run(load_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)

# Sample window: accumulate energy while load runs in parallel slices.
before = sampler.snapshot()
t0 = time.time()
load_rounds = 0
load_ok = 0
while time.time() - t0 < SAMPLE_SECS:
    rc = subprocess.run(load_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    load_rounds += 1
    if rc.returncode == 0:
        load_ok += 1
after = sampler.snapshot()
t1 = time.time()
wall = t1 - t0
if wall <= 0:
    raise SystemExit("zero sample wall")
if load_ok == 0:
    raise SystemExit("all load rounds failed; cannot claim inference_shaped package power")

# Per-channel deltas
deltas = {}
for name, a in after.items():
    b = before.get(name)
    if not b:
        continue
    dj = a["joules"] - b["joules"]
    if dj < 0:
        # counter wrap / reset — refuse rather than invent
        raise SystemExit(f"negative energy delta on {name}: {dj}")
    deltas[name] = {
        "delta_joules": dj,
        "unit": a["unit"],
        "watts": dj / wall,
    }

total_j = sum(d["delta_joules"] for d in deltas.values())
sustained_watts = total_j / wall
if sustained_watts <= 0:
    raise SystemExit(f"non-positive sustained watts: {sustained_watts}")

# Domain breakdown for honesty.
def domain_watts(pred):
    j = sum(d["delta_joules"] for n, d in deltas.items() if pred(n))
    return j / wall

cpu_w = domain_watts(lambda n: "CPU Energy" in n)
gpu_w = domain_watts(lambda n: n == "GPU Energy" or n.startswith("GPU"))
ane_w = domain_watts(lambda n: n.startswith("ANE"))
dram_w = domain_watts(lambda n: n.startswith("DRAM"))

# Refuse if GPU alone would be presented as package (sanity: package must include CPU).
if cpu_w <= 0:
    raise SystemExit(f"CPU package component is {cpu_w}; refusing GPU-only envelope")

measured_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
# Slight conservative bump: ceil to 0.1 W so the table constant can sit at measured.
watts_rounded = math.ceil(sustained_watts * 10) / 10.0

payload = {
    "schema_version": 1,
    "kind": "apple_silicon_ultra_whole_package_power_authority",
    "merc_source_commit": commit,
    "harness": "ops/scripts/seal-apple-silicon-ultra-package-power.sh (IOReport Energy Model package channels under merc-agent bench-batch load)",
    "measured_at": measured_at,
    "hardware_class": hw_class,
    "engine_build_hash": engine_build_hash,
    "engine_build_identity_policy": engine_build_identity_policy,
    "hardware_identity": hardware_identity,
    "hardware": {
        "gpu": hardware_identity,
        "hw_class": hw_class,
        "hardware_identity": hardware_identity,
    },
    "raw_measurement": {
        "build_hash": engine_build_hash,
        "build_identity_policy": engine_build_identity_policy,
        "engine_build_hash": engine_build_hash,
        "engine_build_identity_policy": engine_build_identity_policy,
        "hardware_identity": hardware_identity,
        "sampler": "IOReport Energy Model aggregate package channels",
        "channels_included": sorted(deltas.keys()),
        "channel_deltas": deltas,
        "warmup_secs": WARMUP_SECS,
        "sample_wall_secs": wall,
        "load": {
            "agent": "merc-agent bench-batch",
            "model": MODEL,
            "batch": BATCH,
            "max_tokens": MAX_TOKENS,
            "rounds": load_rounds,
            "rounds_ok": load_ok,
        },
        "domain_watts": {
            "cpu_die_energy": cpu_w,
            "gpu_energy": gpu_w,
            "ane": ane_w,
            "dram": dram_w,
            "sum": sustained_watts,
        },
        "boundary_note": (
            "SoC package energy via IOReport Energy Model (CPU die aggregates + "
            "GPU Energy + ANE + DRAM). Same package-side class as powermetrics "
            "Combined Power. Not wall-plug; not GPU-domain-only."
        ),
    },
    "whole_package_power": {
        "hardware_class": hw_class,
        "engine_build_hash": engine_build_hash,
        "engine_build_identity_policy": engine_build_identity_policy,
        "hardware_identity": hardware_identity,
        "measurement_status": "MEASURED",
        "measurement_boundary": "whole_package",
        "workload_class": "inference_shaped",
        "unit": "watts",
        "authority_scope": "hardware_class_conservative_max_envelope",
        "aggregation": "maximum_across_covered_workloads",
        "operating_protocol": "catalogue-power-envelope-v1/steady-state-inference-after-warmup",
        "covered_workloads": [
            {
                "model_id": "llama-3.2-1b-instruct-q4",
                "job_type": "batch_infer",
                "model_artifact_digest": model_digest,
                "runtime_cell_id": "candle-metal-llama1-infer",
                "runtime_profile_id": "candle_metal",
                "engine": "candle",
                "engine_build_hash": engine_build_hash,
                "engine_build_identity_policy": engine_build_identity_policy,
                "hardware_identity": hardware_identity,
            }
        ],
        "sustained_watts": watts_rounded,
        "measured_mean_watts_unrounded": sustained_watts,
        "measured_at": measured_at,
        "freshness_policy": "catalogue-power-receipt-v1/max-age-30d/no-future-timestamps",
    },
}
Path(PAYLOAD).write_text(json.dumps(payload, indent=2) + "\n")
Path("/tmp/merc-package-power-r1-summary.json").write_text(
    json.dumps(
        {
            "sustained_watts": watts_rounded,
            "measured_mean_watts_unrounded": sustained_watts,
            "domain_watts": {
                "cpu": cpu_w, "gpu": gpu_w, "ane": ane_w, "dram": dram_w,
            },
            "wall_s": wall,
            "engine_build_hash": engine_build_hash,
            "hardware_identity": hardware_identity,
        },
        indent=2,
    )
    + "\n"
)
print("sustained_watts", watts_rounded, "(mean", round(sustained_watts, 3), ")")
print("domains cpu/gpu/ane/dram", round(cpu_w, 2), round(gpu_w, 2), round(ane_w, 2), round(dram_w, 2))
print("wall_s", round(wall, 2), "load_ok", load_ok, "/", load_rounds)
PY

python3 "$ROOT/ops/scripts/write-bound-evidence.py" \
  --out "$OUT_REL" \
  --harness "ops/scripts/seal-apple-silicon-ultra-package-power.sh (IOReport Energy Model package under merc-agent bench-batch)" \
  --payload-file "$PAYLOAD" \
  --build-binary "$AGENT_BIN" \
  --model-na "whole-package power measurement is hardware authority, not model throughput authority" \
  --image-na "in-process candle metal; no container image" \
  --corpus-na "no external corpus; load is merc-agent bench-batch prompt" \
  --exact-config "IOReport Energy Model package channels; warmup=${WARMUP_SECS}s sample>=${SAMPLE_SECS}s batch=${BATCH} max_tokens=${MAX_TOKENS} model=${MODEL} candle metal" \
  --raw-samples "embedded raw_measurement.channel_deltas + domain_watts + load rounds"

# Print digest for pricing.go wattsMeasured pin
python3 - <<PY
import hashlib, json
from pathlib import Path
raw = Path("$OUT_REL").read_bytes()
print("receipt_sha256", hashlib.sha256(raw).hexdigest())
print("path", "$OUT_REL")
d = json.loads(raw)
sec = d["whole_package_power"]
print("sustained_watts", sec["sustained_watts"])
print("engine_build_hash", sec["engine_build_hash"])
print("hardware_identity", sec["hardware_identity"])
PY

echo "sealed $OUT_REL (BOUND)."

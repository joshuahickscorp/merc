#!/usr/bin/env bash
# Integrity gates for scripts/private-canary.py:
#   * a -run pattern that matches nothing is FAILED, never TESTED
#   * a lane that needs a real inference runtime fails if the test used a fake
#   * cap_object_store accepts scheme-ful and scheme-less endpoints
#   * cap_real_inference_runtime builds the same models URL as cap_cuda_runtime
set -euo pipefail
cd "$(dirname "$0")/.."
fail() { echo "PRIVATE-CANARY INTEGRITY FAILED: $1" >&2; exit 1; }

# 1. A lane whose -run pattern matches nothing reports FAILED, not TESTED, and
#    the process that would surface it must not treat that as success.
python3 - <<'PY' || fail "empty -run pattern did not force FAILED"
import importlib.util, sys
spec = importlib.util.spec_from_file_location("canary", "scripts/private-canary.py")
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)

# Positive detection via go test -list, not warning-string scraping.
matched = m.list_matching_go_tests("TestThisPatternMatchesNothingAnywhereInTheRepo", "control", 120)
if matched:
    raise SystemExit(f"expected no matches, got {matched}")

lane = {
    "id": "vacuous_lane",
    "needs": [],
    "cmd": m.go_test("TestThisPatternMatchesNothingAnywhereInTheRepo"),
    "cwd": "control",
    "note": "synthetic vacuous lane",
}
caps = {k: (True, "forced") for k in m.CAPABILITIES}
r = m.run_lane(lane, caps, 120)
if r["status"] != "FAILED":
    raise SystemExit(f"empty match reported {r['status']!r}, want FAILED: {r}")
reason = r.get("reason", "")
if "TestThisPatternMatchesNothingAnywhereInTheRepo" not in reason:
    raise SystemExit(f"reason must name the unmatched pattern: {reason!r}")
if "no test" not in reason.lower() and "matched" not in reason.lower():
    raise SystemExit(f"reason must say nothing matched: {reason!r}")
print("ok: empty -run pattern -> FAILED with named pattern")
PY

# 2. A lane requiring a real inference runtime fails when the test used a fake.
python3 - <<'PY' || fail "real-runtime lane accepted a fake upstream"
import importlib.util
spec = importlib.util.spec_from_file_location("canary", "scripts/private-canary.py")
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)

# Command exits 0 and emits the fake-upstream marker the real integration test
# prints when it did not reach a configured engine.
lane = {
    "id": "realtime_fake_probe",
    "needs": ["real_inference_runtime"],
    "require_real_upstream": True,
    "cmd": ["python3", "-c",
            "import sys; sys.stderr.write('merc_canary: realtime_upstream=fake\\n'); sys.exit(0)"],
    "cwd": ".",
    "note": "synthetic realtime lane",
}
caps = {k: (True, "forced") for k in m.CAPABILITIES}
r = m.run_lane(lane, caps, 30)
if r["status"] != "FAILED":
    raise SystemExit(f"fake upstream reported {r['status']!r}, want FAILED: {r}")
if "fake" not in r.get("reason", "").lower() and "real" not in r.get("reason", "").lower():
    raise SystemExit(f"reason must explain the fake/real mismatch: {r}")
print("ok: real-runtime need + fake marker -> FAILED")

# And the real marker is accepted (still not CANARY_PROVEN).
lane["cmd"] = ["python3", "-c",
               "import sys; sys.stderr.write('merc_canary: realtime_upstream=real\\n'); sys.exit(0)"]
r = m.run_lane(lane, caps, 30)
if r["status"] not in ("TESTED", "REAL_RUNTIME_PROVEN"):
    raise SystemExit(f"real marker should allow TESTED, got {r}")
if r.get("realtime_upstream") != "real":
    raise SystemExit(f"lane must surface realtime_upstream=real: {r}")
print("ok: real-runtime need + real marker -> TESTED with distinction")
PY

# 3. cap_object_store accepts both 127.0.0.1:9000 and http://127.0.0.1:9000.
python3 - <<'PY' || fail "cap_object_store rejected a valid endpoint form"
import importlib.util, os, socket, threading, time
spec = importlib.util.spec_from_file_location("canary", "scripts/private-canary.py")
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)

# Bind an ephemeral listener so the probe has something to connect to.
srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", 0))
port = srv.getsockname()[1]
srv.listen(1)
stop = threading.Event()

def accept_loop():
    srv.settimeout(0.2)
    while not stop.is_set():
        try:
            conn, _ = srv.accept()
            conn.close()
        except socket.timeout:
            continue
        except OSError:
            break

t = threading.Thread(target=accept_loop, daemon=True)
t.start()
try:
    for endpoint in (f"127.0.0.1:{port}", f"http://127.0.0.1:{port}"):
        os.environ["S3_ENDPOINT"] = endpoint
        present, detail = m.cap_object_store()
        if not present:
            raise SystemExit(f"cap_object_store rejected {endpoint!r}: {detail}")
    print("ok: cap_object_store accepts scheme-less and scheme-ful endpoints")
finally:
    stop.set()
    srv.close()
    os.environ.pop("S3_ENDPOINT", None)
PY

# 4. cap_real_inference_runtime builds the same URL as cap_cuda_runtime for a
#    bare host and for a /v1 host.
python3 - <<'PY' || fail "models URL construction diverged between probes"
import importlib.util
spec = importlib.util.spec_from_file_location("canary", "scripts/private-canary.py")
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)

cases = [
    ("http://127.0.0.1:8095", "http://127.0.0.1:8095/v1/models"),
    ("http://127.0.0.1:8095/", "http://127.0.0.1:8095/v1/models"),
    ("http://127.0.0.1:8095/v1", "http://127.0.0.1:8095/v1/models"),
    ("http://127.0.0.1:8095/v1/", "http://127.0.0.1:8095/v1/models"),
]
for endpoint, want in cases:
    got = m._models_url(endpoint)
    if got != want:
        raise SystemExit(f"_models_url({endpoint!r}) = {got!r}, want {want!r}")
# The realtime probe must use the helper (source-level: no bare rstrip+/models).
import inspect, re
src = inspect.getsource(m.cap_real_inference_runtime)
if "_models_url" not in src:
    raise SystemExit("cap_real_inference_runtime does not call _models_url")
if re.search(r'rstrip\(["\']\/["\']\)\s*\+\s*["\']/models["\']', src):
    raise SystemExit("cap_real_inference_runtime still concatenates /models by hand")
print("ok: _models_url shared; realtime probe uses it")
PY

echo "private-canary-integrity: PASS"

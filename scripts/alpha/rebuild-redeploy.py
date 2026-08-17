#!/usr/bin/env python3
"""Rebuild-from-laptop / load-on-droplet / swap-control for the alpha plane.

The 1-vCPU droplet cannot build linux/amd64. This script assumes the image
already exists locally (or builds it from a full tree), copies it to the
droplet, retags it, and recreates ONLY merc-control-1. postgres/minio volumes
are never touched. Prior digest is retained.

The candidate binary (HEAD, after 9ba9884e) refuses to mark /readyz ready
unless MERC_CANARY_APPROVED_BUILD_HASHES includes the sealed r6 identity
7cc01c442c7f6dbe. The droplet .env still carries only the superseded r5 hash
f4303a751ca2b2af. Shipping ops/staging/compose.alpha.yml as the last compose
overlay overrides that stale value. Do not rewrite .env and do not disable
the boot check: a 503 after deploy is the check working.

Never reads .merc-secrets.env. Never prints secret values.
"""
from __future__ import annotations

import argparse
import json
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.path.insert(0, str(ROOT / "scripts"))
from remote import copy_to_remote, run_remote  # noqa: E402
from lib.evidence_binding import (  # noqa: E402
    default_bound_identity,
    slot_na,
    slot_value,
    write_bound_evidence,
)


def _git_head() -> str:
    proc = subprocess.run(
        ["git", "-C", str(ROOT), "rev-parse", "HEAD"],
        text=True,
        capture_output=True,
        check=False,
    )
    if proc.returncode != 0:
        die(f"cannot resolve HEAD: {proc.stderr.strip()}")
    commit = proc.stdout.strip()
    if len(commit) != 40:
        die(f"HEAD is not a 40-char commit: {commit!r}")
    return commit


# Candidate is current HEAD. Prior is discovered from the running container
# at the start of the run so rollback targets whatever is serving now.
CANDIDATE = _git_head()
CANDIDATE_SHORT = CANDIDATE[:12]
# Historic digest that a previous lane already retained. Must stay loaded.
HISTORIC_PRIOR_COMMIT = "19fe0b23940c7e3d4da9b45d9cc5689c2c515d07"
HISTORIC_PRIOR_IMAGE_ID = (
    "sha256:245dc92a5fffc1b9ffefe2452277797a498dc9cfb779dd915ae2631802175768"
)
HOST = "mercmerc.net"
SEALED_BUILD_HASH = "7cc01c442c7f6dbe"
SUPERSEDED_BUILD_HASH = "f4303a751ca2b2af"
COMPOSE_PIN_LOCAL = ROOT / "ops/staging/compose.alpha.yml"
COMPOSE_PIN_REMOTE = "/opt/merc/ops/staging/compose.alpha.yml"
# Alpha overlay last so the sealed-hash pin wins over the stale host .env
# and over docker-compose.canary.yml (which does not pin a build hash).
# canary.yml stays in the stack: the droplet copy carries GOMEMLIMIT.
COMPOSE = (
    "docker compose "
    "-f docker-compose.prod.yml "
    "-f docker-compose.smallhost.yml "
    "-f docker-compose.canary.yml "
    "-f ops/staging/compose.alpha.yml"
)
FORBIDDEN_VOLUME_ARGS = ("merc_pgdata", "merc_miniodata", "volume rm", "compose down")


def die(msg: str) -> None:
    print(f"rebuild-redeploy: FAIL: {msg}", file=sys.stderr)
    raise SystemExit(1)


def utc() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def remote(cmd: str, timeout: int = 180) -> str:
    for token in FORBIDDEN_VOLUME_ARGS:
        if token in cmd:
            die(f"refusing remote command that mentions {token!r}")
    proc = run_remote(cmd, timeout=timeout)
    if proc.returncode != 0:
        die(f"remote rc={proc.returncode}: {(proc.stderr or proc.stdout)[-800:]}")
    return proc.stdout


def https_json(path: str) -> tuple[int, dict | str]:
    req = urllib.request.Request(
        f"https://{HOST}{path}",
        headers={"User-Agent": "merc-lane-s3-rebuild"},
    )
    try:
        with urllib.request.urlopen(req, timeout=20, context=ssl.create_default_context()) as resp:
            raw = resp.read().decode()
            try:
                return resp.status, json.loads(raw)
            except json.JSONDecodeError:
                return resp.status, raw
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode(errors="replace")
        try:
            return exc.code, json.loads(raw)
        except json.JSONDecodeError:
            return exc.code, raw


def https_code(path: str) -> int:
    req = urllib.request.Request(
        f"https://{HOST}{path}",
        headers={"User-Agent": "merc-lane-s3-rebuild"},
    )
    try:
        with urllib.request.urlopen(req, timeout=20, context=ssl.create_default_context()) as resp:
            return resp.status
    except urllib.error.HTTPError as exc:
        return exc.code


def readyz_is_ready(body: object) -> bool:
    return (
        isinstance(body, dict)
        and body.get("status") == "ready"
        and body.get("payment_mode") == "test"
        and body.get("live_value_movement") is False
    )


def wait_version(commit: str, timeout: int = 120) -> dict:
    deadline = time.time() + timeout
    last: dict | str = {}
    last_code = 0
    last_ready: dict | str = {}
    last_ready_code = 0
    while time.time() < deadline:
        try:
            code, body = https_json("/version")
            last_code, last = code, body
            if (
                code == 200
                and isinstance(body, dict)
                and body.get("commit") == commit
                and body.get("modified") is False
            ):
                ready_code, ready = https_json("/readyz")
                last_ready_code, last_ready = ready_code, ready
                if ready_code == 200 and readyz_is_ready(ready):
                    return {"version": body, "readyz": ready, "readyz_http": ready_code}
        except (urllib.error.URLError, TimeoutError, OSError):
            pass
        time.sleep(2)
    die(
        f"/version did not report {commit} modified:false with /readyz 200 "
        f"payment_mode=test live_value_movement=false; "
        f"last_version={last_code} {last} last_readyz={last_ready_code} {last_ready}"
    )


def inspect_trio() -> str:
    return remote(
        "docker inspect -f "
        "'{{.Name}} started={{.State.StartedAt}} image={{.Image}} "
        "status={{.State.Status}} health={{if .State.Health}}{{.State.Health.Status}}{{end}}' "
        "merc-control-1 merc-postgres-1 merc-minio-1"
    )


def image_still_present(image_id: str) -> str:
    return remote(
        f"docker image inspect {image_id} --format 'id={{{{.Id}}}} tags={{{{json .RepoTags}}}}'"
    ).strip()


def running_canary_hash() -> str:
    raw = remote(
        "docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' merc-control-1"
    )
    for line in raw.splitlines():
        if line.startswith("MERC_CANARY_APPROVED_BUILD_HASHES="):
            return line.split("=", 1)[1].strip()
    return ""


def host_env_build_hash() -> str:
    script = (
        "python3 - <<'PY'\n"
        "from pathlib import Path\n"
        "for line in Path('/opt/merc/.env').read_text().splitlines():\n"
        "    if line.startswith('MERC_CANARY_APPROVED_BUILD_HASHES='):\n"
        "        print(line.split('=',1)[1].strip().strip(chr(34)).strip(chr(39)))\n"
        "        break\n"
        "PY"
    )
    return remote(script).strip()


def merged_compose_build_hash() -> str:
    # compose config prints the whole merged stack, including secrets. Parse
    # remotely and return only the approved-hash value.
    script = (
        "python3 - <<'PY'\n"
        "import subprocess, sys\n"
        "cmd = [\n"
        "    'docker', 'compose',\n"
        "    '-f', 'docker-compose.prod.yml',\n"
        "    '-f', 'docker-compose.smallhost.yml',\n"
        "    '-f', 'docker-compose.canary.yml',\n"
        "    '-f', 'ops/staging/compose.alpha.yml',\n"
        "    'config',\n"
        "]\n"
        "proc = subprocess.run(cmd, cwd='/opt/merc', text=True, capture_output=True)\n"
        "if proc.returncode != 0:\n"
        "    sys.stderr.write(proc.stderr[-400:])\n"
        "    raise SystemExit(proc.returncode)\n"
        "found = ''\n"
        "in_control = False\n"
        "for line in proc.stdout.splitlines():\n"
        "    if line.startswith('  control:'):\n"
        "        in_control = True\n"
        "        continue\n"
        "    if in_control and line.startswith('  ') and not line.startswith('   ') "
        "and line.rstrip().endswith(':'):\n"
        "        in_control = False\n"
        "    if in_control and 'MERC_CANARY_APPROVED_BUILD_HASHES' in line:\n"
        "        value = line.split(':', 1)[-1].strip().strip('\"').strip(\"'\")\n"
        "        if value and value != 'MERC_CANARY_APPROVED_BUILD_HASHES':\n"
        "            found = value\n"
        "print(found)\n"
        "PY"
    )
    return remote(script, timeout=120).strip()


def ship_compose_pin() -> dict:
    if not COMPOSE_PIN_LOCAL.is_file():
        die(f"missing local compose pin {COMPOSE_PIN_LOCAL}")
    text = COMPOSE_PIN_LOCAL.read_text()
    if SEALED_BUILD_HASH not in text:
        die("local ops/staging/compose.alpha.yml does not pin 7cc01c442c7f6dbe")
    if f'MERC_CANARY_APPROVED_BUILD_HASHES: "{SEALED_BUILD_HASH}"' not in text:
        die("local compose pin does not set MERC_CANARY_APPROVED_BUILD_HASHES to the sealed hash")
    env_before = host_env_build_hash()
    remote(
        "test -f /opt/merc/ops/staging/compose.alpha.yml && "
        "cp -a /opt/merc/ops/staging/compose.alpha.yml "
        "/opt/merc/ops/staging/compose.alpha.yml.pre-head-rebuild || true"
    )
    proc = copy_to_remote(COMPOSE_PIN_LOCAL, COMPOSE_PIN_REMOTE, timeout=60)
    if proc.returncode != 0:
        die(f"compose pin copy failed: {(proc.stderr or '')[-400:]}")
    remote_text = remote(f"cat {COMPOSE_PIN_REMOTE}")
    if SEALED_BUILD_HASH not in remote_text:
        die("remote compose pin missing sealed hash after copy")
    if SUPERSEDED_BUILD_HASH in remote_text.split("MERC_CANARY_APPROVED_BUILD_HASHES", 1)[-1][:80]:
        die("remote compose pin still names the superseded r5 hash as the allowlist value")
    digest = remote(f"sha256sum {COMPOSE_PIN_REMOTE}").split()[0]
    merged = merged_compose_build_hash()
    if merged != SEALED_BUILD_HASH:
        die(
            f"merged compose config has MERC_CANARY_APPROVED_BUILD_HASHES={merged!r}, "
            f"want {SEALED_BUILD_HASH} (ship the compose pin, do not disable the check)"
        )
    env_after = host_env_build_hash()
    if env_after != env_before:
        die("host .env build-hash line changed; compose pin must not rewrite .env")
    return {
        "local_path": "ops/staging/compose.alpha.yml",
        "remote_path": COMPOSE_PIN_REMOTE,
        "sha256": digest,
        "pinned_hash": SEALED_BUILD_HASH,
        "merged_compose_build_hash": merged,
        "host_env_build_hash": env_after,
        "host_env_rewritten": False,
        "note": (
            "compose overlay pins sealed r6 hash; host .env still carries "
            f"{env_after or 'unset'} and was not rewritten"
        ),
    }


def activate(commit: str, image_tag: str, require_sealed_hash: bool) -> dict:
    started = utc()
    # Recreate only control. --no-deps keeps postgres/minio. --no-build refuses
    # an on-droplet compile. --no-recreate is intentionally omitted so the
    # image tag change actually replaces the container.
    remote(
        f"cd /opt/merc && MERC_BUILD_COMMIT={commit} {COMPOSE} "
        "up -d --no-deps --no-build control",
        timeout=180,
    )
    observed = wait_version(commit)
    inspect = inspect_trio()
    image_id = remote(
        "docker inspect -f '{{.Image}}' merc-control-1"
    ).strip()
    configured = remote(
        "docker inspect -f '{{.Config.Image}}' merc-control-1"
    ).strip()
    if configured != image_tag:
        die(f"running image is {configured}, expected {image_tag}")
    canary_hash = running_canary_hash()
    if require_sealed_hash and canary_hash != SEALED_BUILD_HASH:
        die(
            f"running container MERC_CANARY_APPROVED_BUILD_HASHES={canary_hash!r}, "
            f"want {SEALED_BUILD_HASH}; compose pin did not win"
        )
    pg_started = remote(
        "docker inspect -f '{{.State.StartedAt}}' merc-postgres-1"
    ).strip()
    minio_started = remote(
        "docker inspect -f '{{.State.StartedAt}}' merc-minio-1"
    ).strip()
    return {
        "at": started,
        "finished_at": utc(),
        "commit": commit,
        "configured_image": configured,
        "image_id": image_id,
        "inspect": inspect,
        "postgres_started_at": pg_started,
        "minio_started_at": minio_started,
        "canary_approved_build_hashes": canary_hash,
        **observed,
    }


def update_build_env(commit: str, build_date: str) -> None:
    # Rewrite only the two non-secret identity lines. The file stays 0600.
    # Never touch MERC_CANARY_APPROVED_BUILD_HASHES — the compose pin owns it.
    script = (
        "python3 - <<'PY'\n"
        "from pathlib import Path\n"
        "p = Path('/opt/merc/.env')\n"
        "mode = p.stat().st_mode & 0o777\n"
        "if mode & 0o077:\n"
        "    raise SystemExit(f'.env mode {oct(mode)} is too open')\n"
        "out = []\n"
        "seen_c = seen_d = False\n"
        "for line in p.read_text().splitlines():\n"
        "    if line.startswith('MERC_CANARY_APPROVED_BUILD_HASHES='):\n"
        "        out.append(line)\n"
        "        continue\n"
        f"    if line.startswith('MERC_BUILD_COMMIT='):\n"
        f"        out.append('MERC_BUILD_COMMIT={commit}')\n"
        "        seen_c = True\n"
        "    elif line.startswith('MERC_BUILD_DATE='):\n"
        f"        out.append('MERC_BUILD_DATE={build_date}')\n"
        "        seen_d = True\n"
        "    else:\n"
        "        out.append(line)\n"
        "if not seen_c:\n"
        f"    out.append('MERC_BUILD_COMMIT={commit}')\n"
        "if not seen_d:\n"
        f"    out.append('MERC_BUILD_DATE={build_date}')\n"
        "p.write_text('\\n'.join(out) + '\\n')\n"
        "p.chmod(0o600)\n"
        "print('updated MERC_BUILD_COMMIT and MERC_BUILD_DATE only')\n"
        "PY"
    )
    print(remote(script).strip())


def write_markers() -> None:
    remote(
        "docker exec merc-postgres-1 psql -U cx -d cx -c "
        "\"INSERT INTO l7_recovery_markers(k, v) "
        "VALUES ('lane-s3-rebuild-20260817', 'present-before-rollback') "
        "ON CONFLICT (k) DO UPDATE SET v = EXCLUDED.v, at = now();\""
    )
    remote(
        "docker exec merc-minio-1 sh -c "
        "'mkdir -p /data/cx-jobs/lane-s3 && "
        "printf lane-s3-rebuild-20260817 > /data/cx-jobs/lane-s3/rebuild-redeploy.txt'"
    )


def read_markers() -> dict:
    pg = remote(
        "docker exec merc-postgres-1 psql -U cx -d cx -At -c "
        "\"SELECT k||'|'||v FROM l7_recovery_markers WHERE k='lane-s3-rebuild-20260817';\""
    ).strip()
    obj = remote(
        "docker exec merc-minio-1 sh -c "
        "'cat /data/cx-jobs/lane-s3/rebuild-redeploy.txt'"
    ).strip()
    return {"postgres_marker": pg, "minio_marker": obj}


def load_image(archive: Path) -> str:
    if not archive.is_file():
        die(f"missing image archive {archive}")
    print(f"copying {archive} ({archive.stat().st_size} bytes) to droplet")
    proc = copy_to_remote(archive, "/tmp/merc-control-candidate.tar.gz", timeout=300)
    if proc.returncode != 0:
        die(f"copy failed: {proc.stderr[-400:]}")
    print(remote("gunzip -c /tmp/merc-control-candidate.tar.gz | docker load", timeout=180).strip())
    remote(
        f"docker tag merc/control:{CANDIDATE_SHORT} "
        f"computexchange/control:{CANDIDATE}"
    )
    loaded = remote(
        f"docker image inspect merc/control:{CANDIDATE_SHORT} --format '{{{{.Id}}}}'"
    ).strip()
    remote("rm -f /tmp/merc-control-candidate.tar.gz")
    print(f"loaded {loaded}")
    return loaded


def discover_live_prior() -> tuple[str, str]:
    code, body = https_json("/version")
    if code != 200 or not isinstance(body, dict):
        die(f"cannot read live /version: {code} {body}")
    commit = str(body.get("commit") or "")
    if len(commit) != 40:
        die(f"live /version commit is not 40 hex: {commit!r}")
    image_id = remote("docker inspect -f '{{.Image}}' merc-control-1").strip()
    if not image_id.startswith("sha256:"):
        die(f"running image id is not a digest: {image_id!r}")
    return commit, image_id


def write_receipt(payload: dict, image_id: str) -> None:
    out = ROOT / "evidence/external/head-rebuild-redeploy.json"
    digest = image_id.removeprefix("sha256:")
    identity = default_bound_identity(
        ROOT,
        harness_revision="scripts/alpha/rebuild-redeploy.py",
        build_binary_path=Path(__file__).resolve(),
        exact_config="embedded in receipt body",
        raw_samples="embedded in receipt body",
        model_na="no model weights in this staging-plane receipt",
        image_na="no container image in this measurement",
        corpus_na="no external corpus in this staging-plane receipt",
    )
    identity["image_digest"] = slot_value(digest)
    identity["model_artifact_digest"] = slot_na(
        "no model weights in this staging-plane receipt"
    )
    write_bound_evidence(
        path=out,
        payload=payload,
        identity=identity,
        repo_root=ROOT,
        build_binary_path=Path(__file__).resolve(),
    )
    print(f"wrote {out}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--archive",
        default=f"/tmp/merc-control-{CANDIDATE_SHORT}.tar.gz",
        help="gzipped docker save of the candidate",
    )
    parser.add_argument(
        "--build-date",
        default=Path("/tmp/merc-rebuild-build-date.txt").read_text().strip()
        if Path("/tmp/merc-rebuild-build-date.txt").exists()
        else "",
    )
    parser.add_argument(
        "--skip-load",
        action="store_true",
        help="image already loaded on the droplet",
    )
    args = parser.parse_args()
    build_date = args.build_date or utc()

    prior_commit, prior_image_id = discover_live_prior()
    if prior_commit == CANDIDATE:
        die(
            f"live staging already serves HEAD {CANDIDATE}; "
            "refusing a no-op swap that cannot demonstrate rollback"
        )

    pre_ready_code, pre_ready = https_json("/readyz")
    pre_version_code, pre_version = https_json("/version")
    pre_board_code, pre_board = https_json("/pricing/board.json")
    pre = {
        "at": utc(),
        "inspect": inspect_trio(),
        "prior_commit": prior_commit,
        "prior_image_id": prior_image_id,
        "historic_prior": image_still_present(HISTORIC_PRIOR_IMAGE_ID),
        "live_prior": image_still_present(prior_image_id),
        "version_http": pre_version_code,
        "version": pre_version,
        "readyz_http": pre_ready_code,
        "readyz": pre_ready,
        "pricing_board_http": pre_board_code,
        "pricing_board": pre_board,
        "host_env_build_hash": host_env_build_hash(),
        "running_canary_hash": running_canary_hash(),
    }
    print("pre-deploy inspect:\n" + pre["inspect"])
    print(f"pre-deploy prior={prior_commit} {prior_image_id}")
    print(f"pre-deploy host_env_hash={pre['host_env_build_hash']} running_hash={pre['running_canary_hash']}")

    write_markers()
    markers_before = read_markers()

    compose_pin = ship_compose_pin()
    print(f"shipped compose pin sha256={compose_pin['sha256']} merged={compose_pin['merged_compose_build_hash']}")

    loaded_id = None
    if not args.skip_load:
        loaded_id = load_image(Path(args.archive))
        print(image_still_present(prior_image_id))
        print(image_still_present(HISTORIC_PRIOR_IMAGE_ID))

    candidate_tag = f"computexchange/control:{CANDIDATE}"
    prior_tag = f"computexchange/control:{prior_commit}"

    update_build_env(CANDIDATE, build_date)
    first = activate(CANDIDATE, candidate_tag, require_sealed_hash=True)
    print(f"first activate {first['image_id']} at {first['at']} hash={first['canary_approved_build_hashes']}")
    if first["image_id"] == prior_image_id:
        die("first activate is still the prior digest; image tag did not swap")

    # Rollback the binary only. The compose pin stays: the prior image accepts
    # a valid 16-hex hash and the pin must remain the way forward recovery boots.
    rollback = activate(prior_commit, prior_tag, require_sealed_hash=False)
    print(f"rollback {rollback['image_id']} at {rollback['at']}")
    if rollback["image_id"] != prior_image_id:
        die(f"rollback image id {rollback['image_id']} != retained prior {prior_image_id}")
    markers_rollback = read_markers()

    update_build_env(CANDIDATE, build_date)
    forward = activate(CANDIDATE, candidate_tag, require_sealed_hash=True)
    print(f"forward {forward['image_id']} at {forward['at']} hash={forward['canary_approved_build_hashes']}")
    if forward["image_id"] != first["image_id"]:
        die(f"forward digest {forward['image_id']} != first activate {first['image_id']}")
    markers_forward = read_markers()

    if first["postgres_started_at"] != forward["postgres_started_at"]:
        die("postgres was recreated during the swap; merc_pgdata may have been disturbed")
    if first["minio_started_at"] != forward["minio_started_at"]:
        die("minio was recreated during the swap; merc_miniodata may have been disturbed")
    if "present-before-rollback" not in markers_forward["postgres_marker"]:
        die(f"postgres marker lost: {markers_forward}")
    if markers_forward["minio_marker"] != "lane-s3-rebuild-20260817":
        die(f"minio marker lost: {markers_forward}")

    prior_after = image_still_present(prior_image_id)
    historic_after = image_still_present(HISTORIC_PRIOR_IMAGE_ID)
    env_hash_end = host_env_build_hash()
    if env_hash_end != pre["host_env_build_hash"]:
        die("host .env build-hash changed during the run")
    healthz = https_code("/healthz")
    if healthz != 200:
        die(f"/healthz is {healthz} at end of run")
    end_ready_code, end_ready = https_json("/readyz")
    if end_ready_code != 200 or not readyz_is_ready(end_ready):
        die(f"end /readyz is {end_ready_code} {end_ready}")
    end_board_code, end_board = https_json("/pricing/board.json")

    receipt = {
        "schema_version": 1,
        "kind": "head_rebuild_redeploy",
        "status": "PASS",
        "host": HOST,
        "deploy_root": "/opt/merc",
        "candidate_commit": CANDIDATE,
        "candidate_image_id": forward["image_id"],
        "candidate_loaded_id": loaded_id,
        "prior_commit": prior_commit,
        "prior_image_id": prior_image_id,
        "prior_retained": prior_after,
        "historic_prior_commit": HISTORIC_PRIOR_COMMIT,
        "historic_prior_image_id": HISTORIC_PRIOR_IMAGE_ID,
        "historic_prior_retained": historic_after,
        "build_date": build_date,
        "toolchain": "golang:1.26.6@sha256:0d1d3a794be25f809dd2cb3160d8c73276c4056a9f8242a138e908ddeee7b6b6",
        "compose": COMPOSE,
        "compose_pin": compose_pin,
        "readyz_before": {"http": pre_ready_code, "body": pre_ready},
        "readyz_after": {"http": end_ready_code, "body": end_ready},
        "pricing_board_before": {"http": pre_board_code, "body": pre_board},
        "pricing_board_after": {"http": end_board_code, "body": end_board},
        "pre": pre,
        "first_activate": first,
        "rollback": rollback,
        "forward": forward,
        "markers_before": markers_before,
        "markers_rollback": markers_rollback,
        "markers_forward": markers_forward,
        "data_plane": {
            "postgres_started_at": forward["postgres_started_at"],
            "minio_started_at": forward["minio_started_at"],
            "volumes_destroyed": False,
            "merc_pgdata": "retained",
            "merc_miniodata": "retained",
        },
        "end_healthz_http": healthz,
        "policy": {
            "stripe_live_mode": False,
            "secret_values_recorded": False,
            "live_plane_interrupted_only_for_control_recreate": True,
            "host_env_build_hash_rewritten": False,
            "canary_boot_check_defeated": False,
            "compose_pin_shipped": True,
        },
        "finished_at": utc(),
    }
    write_receipt(receipt, forward["image_id"])
    print(json.dumps({
        "candidate_commit": CANDIDATE,
        "candidate_image_id": forward["image_id"],
        "prior_image_id": prior_image_id,
        "rollback_at": rollback["at"],
        "forward_at": forward["at"],
        "compose_pin_sha256": compose_pin["sha256"],
        "running_canary_hash": forward["canary_approved_build_hashes"],
        "host_env_build_hash": env_hash_end,
        "version": forward["version"],
        "readyz": forward["readyz"],
    }, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

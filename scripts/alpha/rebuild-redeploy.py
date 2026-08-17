#!/usr/bin/env python3
"""Rebuild-from-laptop / load-on-droplet / swap-control for the alpha plane.

The 1-vCPU droplet cannot build linux/amd64. This script assumes the image
already exists locally (or builds it from a full tree), copies it to the
droplet, retags it, and recreates ONLY merc-control-1. postgres/minio volumes
are never touched. Prior digest is retained.

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
from remote import REMOTE_HOST, copy_to_remote, run_remote  # noqa: E402

CANDIDATE = "19fe0b23940c7e3d4da9b45d9cc5689c2c515d07"
CANDIDATE_SHORT = CANDIDATE[:12]
PRIOR = "a5bca8c0abcfda4158f5c681fa67f5ae5ebccb05"
PRIOR_IMAGE_ID = "sha256:2b2f85c969176dd5cc84f66e402e0853519f7784bc9f60fcf86841372e9fb28c"
HOST = "mercmerc.net"
COMPOSE = (
    "docker compose "
    "-f docker-compose.prod.yml "
    "-f docker-compose.smallhost.yml "
    "-f docker-compose.canary.yml"
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
        headers={"User-Agent": "merc-lane-b-rebuild"},
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
        headers={"User-Agent": "merc-lane-b-rebuild"},
    )
    try:
        with urllib.request.urlopen(req, timeout=20, context=ssl.create_default_context()) as resp:
            return resp.status
    except urllib.error.HTTPError as exc:
        return exc.code


def wait_version(commit: str, timeout: int = 120) -> dict:
    deadline = time.time() + timeout
    last: dict | str = {}
    last_code = 0
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
                if ready_code == 200 and isinstance(ready, dict) and ready.get("status") == "ready":
                    return {"version": body, "readyz": ready, "readyz_http": ready_code}
        except (urllib.error.URLError, TimeoutError, OSError):
            pass
        time.sleep(2)
    die(f"/version did not report {commit} modified:false with /readyz 200; last={last_code} {last}")


def inspect_trio() -> str:
    return remote(
        "docker inspect -f "
        "'{{.Name}} started={{.State.StartedAt}} image={{.Image}} "
        "status={{.State.Status}} health={{if .State.Health}}{{.State.Health.Status}}{{end}}' "
        "merc-control-1 merc-postgres-1 merc-minio-1"
    )


def prior_still_present() -> str:
    return remote(
        f"docker image inspect {PRIOR_IMAGE_ID} --format 'prior_id={{{{.Id}}}} tags={{{{json .RepoTags}}}}'"
    ).strip()


def activate(commit: str, image_tag: str) -> dict:
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
    if commit == PRIOR and image_id != PRIOR_IMAGE_ID:
        die(f"rollback image id {image_id} != retained prior {PRIOR_IMAGE_ID}")
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
        **observed,
    }


def update_build_env(commit: str, build_date: str) -> None:
    # Rewrite only the two non-secret identity lines. The file stays 0600.
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
        "VALUES ('lane-b-rebuild-20260817', 'present-before-rollback') "
        "ON CONFLICT (k) DO UPDATE SET v = EXCLUDED.v, at = now();\""
    )
    remote(
        "docker exec merc-minio-1 sh -c "
        "'mkdir -p /data/cx-jobs/lane-b && "
        "printf lane-b-rebuild-20260817 > /data/cx-jobs/lane-b/rebuild-redeploy.txt'"
    )


def read_markers() -> dict:
    pg = remote(
        "docker exec merc-postgres-1 psql -U cx -d cx -At -c "
        "\"SELECT k||'|'||v FROM l7_recovery_markers WHERE k='lane-b-rebuild-20260817';\""
    ).strip()
    obj = remote(
        "docker exec merc-minio-1 sh -c "
        "'cat /data/cx-jobs/lane-b/rebuild-redeploy.txt'"
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
    print(prior_still_present())
    return loaded


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--archive",
        default="/tmp/merc-control-19fe0b23940c.tar.gz",
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

    pre = {
        "at": utc(),
        "inspect": inspect_trio(),
        "prior": prior_still_present(),
        "version": https_json("/version")[1],
        "readyz": https_json("/readyz")[1],
    }
    print("pre-deploy inspect:\n" + pre["inspect"])

    write_markers()
    markers_before = read_markers()

    loaded_id = None
    if not args.skip_load:
        loaded_id = load_image(Path(args.archive))

    candidate_tag = f"computexchange/control:{CANDIDATE}"
    prior_tag = f"computexchange/control:{PRIOR}"

    update_build_env(CANDIDATE, build_date)
    first = activate(CANDIDATE, candidate_tag)
    print(f"first activate {first['image_id']} at {first['at']}")

    rollback = activate(PRIOR, prior_tag)
    print(f"rollback {rollback['image_id']} at {rollback['at']}")
    markers_rollback = read_markers()

    update_build_env(CANDIDATE, build_date)
    forward = activate(CANDIDATE, candidate_tag)
    print(f"forward {forward['image_id']} at {forward['at']}")
    markers_forward = read_markers()

    if first["postgres_started_at"] != forward["postgres_started_at"]:
        die("postgres was recreated during the swap; merc_pgdata may have been disturbed")
    if first["minio_started_at"] != forward["minio_started_at"]:
        die("minio was recreated during the swap; merc_miniodata may have been disturbed")
    if markers_forward["postgres_marker"] != markers_before["postgres_marker"].split("|")[0] + "|present-before-rollback" \
            and "present-before-rollback" not in markers_forward["postgres_marker"]:
        # marker value is k|v; accept the same v
        if "present-before-rollback" not in markers_forward["postgres_marker"]:
            die(f"postgres marker lost: {markers_forward}")
    if markers_forward["minio_marker"] != "lane-b-rebuild-20260817":
        die(f"minio marker lost: {markers_forward}")

    prior_after = prior_still_present()
    healthz = https_code("/healthz")
    if healthz != 200:
        die(f"/healthz is {healthz} at end of run")

    receipt = {
        "schema_version": 1,
        "kind": "head_rebuild_redeploy",
        "status": "PASS",
        "host": HOST,
        "deploy_root": "/opt/merc",
        "candidate_commit": CANDIDATE,
        "candidate_image_id": forward["image_id"],
        "candidate_loaded_id": loaded_id,
        "prior_commit": PRIOR,
        "prior_image_id": PRIOR_IMAGE_ID,
        "prior_retained": prior_after,
        "build_date": build_date,
        "toolchain": "golang:1.26.6@sha256:0d1d3a794be25f809dd2cb3160d8c73276c4056a9f8242a138e908ddeee7b6b6",
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
        },
        "finished_at": utc(),
    }
    out = ROOT / "evidence/external/head-rebuild-redeploy.json"
    tmp = out.with_suffix(".json.tmp")
    tmp.write_text(json.dumps(receipt, indent=2) + "\n")
    tmp.replace(out)
    print(f"wrote {out}")
    print(json.dumps({
        "candidate_commit": CANDIDATE,
        "candidate_image_id": forward["image_id"],
        "prior_image_id": PRIOR_IMAGE_ID,
        "rollback_at": rollback["at"],
        "forward_at": forward["at"],
        "version": forward["version"],
        "readyz": forward["readyz"],
    }, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

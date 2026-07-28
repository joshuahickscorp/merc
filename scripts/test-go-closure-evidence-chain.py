#!/usr/bin/env python3
"""Hostile fixture test for the final GO-closure evidence-chain validator."""

from __future__ import annotations

import datetime as dt
import hashlib
import json
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
VALIDATOR = ROOT / "scripts/validate-go-closure-evidence-chain.py"
IMAGE = "registry.example.invalid/merc/control@sha256:" + "3" * 64
PRIOR_IMAGE = "registry.example.invalid/merc/control@sha256:" + "4" * 64
PRIOR_COMMIT = "5" * 40
RUN_ID_RESTART = "1" * 32
RUN_ID_CANARY = "2" * 32
RESTART_DRIVER = "6" * 64
CANARY_DRIVER = "7" * 64
SCENARIOS = (
    "approved_buyer_identity",
    "distinct_metal_agent",
    "embed_success",
    "batch_infer_success",
    "cancelled_job",
    "forced_retry",
    "stale_lease_recovery",
    "stale_attempt_commit_rejection",
    "buyer_webhook_retry_sequence",
    "backup_independent_restore",
    "stripe_test_matrix",
    "real_alert_firing_resolution",
    "post_rehearsal_invariant_audit",
    "bounded_retry_backoff_audit",
)
MINIMUMS = dict(zip(SCENARIOS, (2, 2, 20, 20, 5, 5, 3, 3, 3, 1, 1, 1, 1, 1)))
SOURCES = {
    "approved_buyer_identity": "merc_postgres.buyers",
    "distinct_metal_agent": "merc_postgres.workers",
    "embed_success": "merc_postgres.jobs",
    "batch_infer_success": "merc_postgres.jobs",
    "cancelled_job": "merc_postgres.jobs",
    "forced_retry": "merc_postgres.job_events",
    "stale_lease_recovery": "merc_postgres.job_events",
    "stale_attempt_commit_rejection": "merc_control.http",
    "buyer_webhook_retry_sequence": "merc_postgres.webhooks",
    "backup_independent_restore": "offsite_backup_provider",
    "stripe_test_matrix": "stripe_test_api",
    "real_alert_firing_resolution": "alert_receiver_api",
    "post_rehearsal_invariant_audit": "merc_postgres.invariant_audit",
    "bounded_retry_backoff_audit": "merc_prometheus",
}
UUID_SUBJECTS = set(SCENARIOS[:9])


def stamp(value: dt.datetime) -> str:
    return value.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def write_json(path: Path, value) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n",
        encoding="utf-8",
    )


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def fixture_uuid(value: int) -> str:
    return f"{value:08x}-0000-4000-8000-{value:012x}"


def snapshot():
    return {
        "buyers": 2,
        "suppliers": 2,
        "workers": 2,
        "jobs": 50,
        "tasks": 50,
        "ledger_entries": 150,
        "ledger_sum_usd": "0.00000000",
        "terminal_jobs_with_open_tasks": 0,
    }


def scenario_receipt(
    scenario: str,
    index: int,
    started: dt.datetime,
    finished: dt.datetime,
    commit: str,
):
    minimum = MINIMUMS[scenario]
    evidence = []
    for item_index in range(minimum):
        subject = (
            fixture_uuid(1000 + index * 100 + item_index)
            if scenario in UUID_SUBJECTS
            else f"fixture-subject-{index:02d}-{item_index:04d}"
        )
        item = {
            "id": f"fixture-observation-{index:02d}-{item_index:04d}",
            "subject_id": subject,
            "occurred_at": stamp(started + dt.timedelta(seconds=10)),
            "source": SOURCES[scenario],
        }
        if scenario == "stale_attempt_commit_rejection":
            item.update(
                {
                    "submitted_attempt": 1,
                    "current_attempt": 2,
                    "before_state_sha256": "8" * 64,
                    "after_state_sha256": "8" * 64,
                    "http_status": 409,
                    "response_sha256": "9" * 64,
                }
            )
        evidence.append(item)
    receipt = {
        "schema_version": 2,
        "scenario": scenario,
        "requested": minimum,
        "observed": minimum,
        "status": "PASS",
        "binding": {
            "run_id": RUN_ID_CANARY,
            "candidate_commit": commit,
            "control_image": IMAGE,
            "driver_sha256": CANARY_DRIVER,
        },
        "started_at": stamp(started),
        "finished_at": stamp(finished),
        "safety": {
            "stripe_test_mode": True,
            "stripe_live_mode": False,
            "real_value": False,
            "approved_participants_only": True,
            "secret_values_recorded": False,
        },
        "evidence": evidence,
    }
    if scenario == "stripe_test_matrix":
        receipt.update(
            {
                "provider_mode": "test",
                "matrix_complete": True,
                "real_value": False,
                "application_outcomes_verified": True,
            }
        )
    elif scenario == "real_alert_firing_resolution":
        receipt["receiver_event_ids"] = {
            "firing": "fixture-alert-firing-0001",
            "resolved": "fixture-alert-resolved-0001",
        }
    elif scenario == "backup_independent_restore":
        receipt.update(
            {
                "encrypted_offsite_upload": True,
                "independent_download": True,
                "ciphertext_checksum_verified": True,
                "isolated_restore": True,
                "postgres_semantic_checks": True,
                "object_checks": True,
            }
        )
    elif scenario == "post_rehearsal_invariant_audit":
        receipt["invariants"] = {
            "tenant_leak": False,
            "missing_artifact": False,
            "duplicate_effects": False,
            "ledger_imbalance": False,
            "stuck_terminal_jobs": False,
            "stuck_payouts": False,
            "unreconciled_state": False,
            "silent_webhook_loss": False,
            "unbounded_growth": False,
        }
    elif scenario == "bounded_retry_backoff_audit":
        receipt.update(
            {
                "max_attempts_within_policy": True,
                "backoff_schedule_within_policy": True,
                "unbounded_retry_growth": False,
            }
        )
    return receipt


def build_fixture(root: Path, commit: str, now: dt.datetime):
    if root.exists():
        shutil.rmtree(root)
    root.mkdir(parents=True)

    deploy_start = now - dt.timedelta(hours=30)
    deploy_finish = deploy_start + dt.timedelta(minutes=2)
    rollback_start = now - dt.timedelta(hours=29, minutes=55)
    rollback_finish = rollback_start + dt.timedelta(minutes=3)
    restart_start = now - dt.timedelta(hours=29, minutes=45)
    restart_finish = restart_start + dt.timedelta(minutes=4)
    canary_start = now - dt.timedelta(hours=29, minutes=35)
    canary_finish = canary_start + dt.timedelta(minutes=10)
    soak_start = now - dt.timedelta(hours=25)
    soak_finish = soak_start + dt.timedelta(hours=24)

    deploy = {
        "schema_version": 1,
        "kind": "go_closure_deploy",
        "status": "PASS",
        "scope": "supervised_stripe_test_mode_private_canary",
        "started_at": stamp(deploy_start),
        "finished_at": stamp(deploy_finish),
        "activation": "candidate",
        "endpoint": "staging.merc.example.invalid",
        "control_image": IMAGE,
        "control_image_id": "sha256:" + "d" * 64,
        "expected_commit": commit,
        "reported_version": {
            "version": "v0.1.0-rc.1",
            "commit": commit,
            "build_date": stamp(deploy_start - dt.timedelta(minutes=10)),
            "go_version": "go1.26.5",
            "platform": "linux/amd64",
            "modified": False,
        },
        "tls_certificate_observation": (
            "sha256 Fingerprint=" + ":".join(["AA"] * 32) + "\n"
            "notAfter=Aug 31 00:00:00 2026 GMT\n"
            "issuer=fixture\nsubject=fixture"
        ),
        "observability": {
            "prometheus_ready": True,
            "alertmanager_ready": True,
            "receiver_name": "fixture-paging-receiver",
        },
        "policy": {
            "stripe_live_mode": False,
            "real_value": False,
            "unrestricted_public_access": False,
            "secret_values_recorded": False,
        },
    }

    backup_id = (rollback_start + dt.timedelta(seconds=1)).strftime("%Y%m%dT%H%M%SZ")
    backup_dir = root / ".artifacts/backups" / backup_id
    backup_dir.mkdir(parents=True)
    ciphertext = backup_dir / "backup.tar.age"
    ciphertext.write_bytes(b"synthetic encrypted fixture bytes only\n")
    ciphertext_sha = sha(ciphertext)
    offsite_uri = f"s3://fixture-offsite/private/{backup_id}"
    manifest = {
        "schema_version": 2,
        "kind": "merc_encrypted_offsite_backup",
        "backup_id": backup_id,
        "cipher": "age-x25519",
        "ciphertext_sha256": ciphertext_sha,
        "ciphertext_bytes": ciphertext.stat().st_size,
        "created_at": stamp(rollback_start + dt.timedelta(seconds=2)),
        "database": "merc",
        "objects_included": True,
        "offsite_uri": offsite_uri,
    }
    manifest_path = backup_dir / "manifest.json"
    write_json(manifest_path, manifest)
    verification = {
        "schema_version": 1,
        "kind": "merc_offsite_backup_verification",
        "status": "PASS",
        "backup_id": backup_id,
        "offsite_uri": offsite_uri,
        "manifest_sha256": sha(manifest_path),
        "ciphertext": {
            "manifest_sha256": ciphertext_sha,
            "downloaded_sha256": ciphertext_sha,
            "bytes": ciphertext.stat().st_size,
        },
        "verified_at": stamp(rollback_start + dt.timedelta(seconds=3)),
        "checks": {
            "offsite_bundle_visible": True,
            "independent_manifest_download": True,
            "independent_ciphertext_download": True,
            "manifest_checksum_match": True,
            "ciphertext_checksum_match": True,
        },
        "policy": {
            "encrypted_before_upload": True,
            "plaintext_uploaded": False,
            "secret_values_recorded": False,
        },
    }
    verification_path = backup_dir / "verification.json"
    write_json(verification_path, verification)
    invocation = {
        "schema_version": 1,
        "kind": "merc_backup_invocation_result",
        "status": "PASS",
        "backup_id": backup_id,
        "offsite_uri": offsite_uri,
        "manifest_sha256": sha(manifest_path),
        "verification_sha256": sha(verification_path),
        "ciphertext_sha256": ciphertext_sha,
        "completed_at": stamp(rollback_start + dt.timedelta(seconds=3)),
    }
    rollback = {
        "schema_version": 1,
        "kind": "go_closure_rollback_forward",
        "status": "PASS",
        "started_at": stamp(rollback_start),
        "finished_at": stamp(rollback_finish),
        "candidate": {
            "image": IMAGE,
            "commit": commit,
            "forward_recoveries": 1,
            "rto_seconds": 12,
        },
        "prior": {
            "image": PRIOR_IMAGE,
            "commit": PRIOR_COMMIT,
            "rollbacks": 1,
            "rto_seconds": 10,
        },
        "pre_upgrade_backup": {
            "backup_id": backup_id,
            "ciphertext_sha256": ciphertext_sha,
            "manifest_sha256": sha(manifest_path),
            "local_manifest": f".artifacts/backups/{backup_id}/manifest.json",
            "offsite_uri": offsite_uri,
            "invocation_result": invocation,
            "verification_receipt": verification,
        },
        "data_integrity": {
            "unchanged": True,
            "before_sha256": "b" * 64,
            "after_sha256": "b" * 64,
            "snapshot": snapshot(),
        },
        "policy": {
            "stripe_live_mode": False,
            "real_value": False,
            "secret_values_recorded": False,
        },
    }

    worker1 = fixture_uuid(1)
    worker2 = fixture_uuid(2)
    workers = (worker1, worker2)
    before_ids = (fixture_uuid(11), fixture_uuid(12))
    after_ids = (fixture_uuid(21), fixture_uuid(22))

    def session_set(ids, session_epoch, last_seen_epoch):
        return [
            {
                "worker_id": worker,
                "agent_session_id": session_id,
                "session_started_epoch": session_epoch,
                "last_seen_epoch": last_seen_epoch,
                "agent_version": "1.2.3",
                "build_hash": f"{index + 1:016x}",
            }
            for index, (worker, session_id) in enumerate(zip(workers, ids))
        ]

    action_start = restart_start + dt.timedelta(seconds=10)
    action_finish = action_start + dt.timedelta(seconds=20)
    action = {
        "schema_version": 2,
        "kind": "merc_agent_restart_action",
        "status": "PASS",
        "requested": 2,
        "binding": {
            "run_id": RUN_ID_RESTART,
            "candidate_commit": commit,
            "control_image": IMAGE,
            "driver_sha256": RESTART_DRIVER,
        },
        "started_at": stamp(action_start),
        "finished_at": stamp(action_finish),
        "safety": {
            "stripe_live_mode": False,
            "real_value": False,
            "approved_participants_only": True,
            "secret_values_recorded": False,
        },
        "actions": [
            {
                "id": f"fixture-restart-action-{index + 1:04d}",
                "worker_id": worker,
                "occurred_at": stamp(action_start + dt.timedelta(seconds=5 + index)),
                "source": "approved_agent_supervisor",
            }
            for index, worker in enumerate(workers)
        ],
    }
    restart_start_epoch = int(restart_start.timestamp())
    restart_finish_epoch = int(restart_finish.timestamp())
    restart = {
        "schema_version": 2,
        "kind": "go_closure_restart_storm",
        "status": "PASS",
        "run_id": RUN_ID_RESTART,
        "started_at": stamp(restart_start),
        "finished_at": stamp(restart_finish),
        "control_image": IMAGE,
        "expected_commit": commit,
        "agent_restart_driver": {
            "path": "/opt/merc/bin/restart-reviewed-agents",
            "sha256": RESTART_DRIVER,
            "matches_operator_reviewed_sha256": True,
            "unchanged_during_run": True,
        },
        "observed": {
            "control_restarts": 2,
            "database_restarts": 1,
            "storage_restarts": 1,
            "alerting_restarts": 1,
            "network_interruptions": 2,
            "network_interruption_seconds_each": 5,
            "agent_supervisor_action_receipt": action,
            "agent_sessions_before": session_set(
                before_ids, restart_start_epoch - 3600, restart_start_epoch
            ),
            "agent_sessions_after_restart": session_set(
                after_ids, restart_start_epoch + 20, restart_start_epoch + 30
            ),
            "agent_sessions_final": session_set(
                after_ids, restart_start_epoch + 20, restart_finish_epoch
            ),
        },
        "assertions": {
            "control_restarts_at_least_2": True,
            "two_distinct_agents_restarted_from_database_session_transitions": True,
            "restarted_agents_remained_current_without_an_extra_restart": True,
            "recovered_after_each_fault_within_300_seconds": True,
            "retry_backoff_requires_correlated_scenario_audit": True,
        },
        "policy": {
            "stripe_live_mode": False,
            "real_value": False,
            "secret_values_recorded": False,
        },
    }

    scenario_receipts = []
    for index, scenario in enumerate(SCENARIOS):
        scenario_start = canary_start + dt.timedelta(seconds=index * 35)
        scenario_finish = scenario_start + dt.timedelta(seconds=20)
        scenario_receipts.append(
            scenario_receipt(scenario, index, scenario_start, scenario_finish, commit)
        )
    canary = {
        "schema_version": 2,
        "kind": "go_closure_canary_rehearsal",
        "status": "PASS",
        "run_id": RUN_ID_CANARY,
        "started_at": stamp(canary_start),
        "finished_at": stamp(canary_finish),
        "control_image": IMAGE,
        "expected_commit": commit,
        "scenario_driver": {
            "path": "/opt/merc/bin/canary-reviewed-driver",
            "sha256": CANARY_DRIVER,
            "matches_operator_reviewed_sha256": True,
            "unchanged_during_run": True,
        },
        "required_counts": {
            "approved_buyer_identities": 2,
            "distinct_metal_agents": 2,
            "successful_embed_jobs": 20,
            "successful_batch_infer_jobs": 20,
            "cancelled_jobs": 5,
            "forced_retries": 5,
            "stale_lease_recoveries": 3,
            "stale_attempt_commit_rejections": 3,
            "buyer_webhook_retry_sequences": 3,
            "backup_independent_restore": 1,
            "stripe_test_matrix": 1,
            "real_alert_firing_resolution": 1,
            "post_rehearsal_invariant_audit": 1,
            "bounded_retry_backoff_audit": 1,
        },
        "observations": {
            "active_workers": 2,
            "page_alerts_firing_after_rehearsal": 0,
            "database_before": snapshot(),
            "database_after": snapshot(),
            "scenario_receipts": scenario_receipts,
        },
        "policy": {
            "stripe_test_mode": True,
            "stripe_live_mode": False,
            "real_value": False,
            "approved_participants_only": True,
            "secret_values_recorded": False,
        },
        "qualification": {
            "workload_and_external_scenarios": True,
            "exact_run_candidate_driver_binding": True,
            "database_backed_scenarios_corroborated": True,
            "restart_rollback_and_24h_soak_receipts_required_separately": True,
        },
    }

    samples_path = root / "evidence/samples.jsonl"
    samples_path.parent.mkdir(parents=True)
    runtime = {
        "container_id": "c" * 64,
        "configured_image": IMAGE,
        "image_id": "sha256:" + "d" * 64,
        "restart_count": 0,
    }
    samples = []
    for index in range(96):
        samples.append(
            {
                "observed_at": stamp(
                    soak_start + dt.timedelta(seconds=900 * (index + 1))
                ),
                "sequence": index + 1,
                "control_container_id": runtime["container_id"],
                "control_configured_image": runtime["configured_image"],
                "control_image_id": runtime["image_id"],
                "control_rss_kb": 1000 + index,
                "host_disk_used_kb": 2000,
                "control_writable_layer_bytes": 3000,
                "control_restart_count": 0,
                "active_workers": 2,
                "db_connections_total": 5,
                "db_connections_acquired": 1,
                "firing_page_alerts": 0,
                "webhook_dead_letters": 0,
                "database": snapshot(),
            }
        )
    samples_path.write_text(
        "".join(
            json.dumps(item, sort_keys=True, separators=(",", ":")) + "\n"
            for item in samples
        ),
        encoding="utf-8",
    )
    soak = {
        "schema_version": 2,
        "kind": "go_closure_soak",
        "status": "PASS",
        "started_at": stamp(soak_start),
        "finished_at": stamp(soak_finish),
        "mode": "qualifying",
        "control_image": IMAGE,
        "expected_commit": commit,
        "runtime": runtime,
        "duration": {
            "requested_seconds": 86400,
            "actual_seconds": 86400,
            "interval_seconds": 900,
            "samples": 96,
        },
        "samples": {"path": "evidence/samples.jsonl", "sha256": sha(samples_path)},
        "bounds": {
            "rss": {
                "baseline_kb": 1000,
                "max_kb": 1095,
                "final_kb": 1095,
                "observed_growth_bytes": 95 * 1024,
                "limit_growth_bytes": 1_000_000,
            },
            "disk": {
                "baseline_used_kb": 2000,
                "max_used_kb": 2000,
                "final_used_kb": 2000,
                "observed_growth_kb": 0,
                "limit_growth_kb": 1000,
            },
            "writable_layer": {
                "baseline_bytes": 3000,
                "max_bytes": 3000,
                "final_bytes": 3000,
                "observed_growth_bytes": 0,
                "limit_growth_bytes": 1000,
            },
            "db_connections": {
                "baseline": 5,
                "max": 5,
                "final": 5,
                "observed_growth": 0,
                "limit_growth": 10,
            },
        },
        "assertions": {
            "two_agents_continuously_present": True,
            "no_page_alerts": True,
            "no_webhook_dead_letters": True,
            "no_control_restarts_or_recreates": True,
            "no_stuck_terminal_jobs": True,
            "bounded_resource_growth": True,
            "raw_samples_independently_validated": True,
        },
        "qualification": {
            "qualifies_for_24h_gate": True,
            "reason": "observed_at_least_86400_seconds",
        },
        "policy": {
            "stripe_test_mode": True,
            "stripe_live_mode": False,
            "real_value": False,
            "secret_values_recorded": False,
        },
    }

    approvals = {}
    for domain in (
        "security",
        "privacy",
        "legal",
        "licensing",
        "payments",
        "operations",
        "supplier_policy",
        "release_approval",
    ):
        when = now - dt.timedelta(minutes=30 if domain == "release_approval" else 45)
        approvals[domain] = {
            "status": "APPROVED",
            "approver": f"Qualified fixture reviewer for {domain}",
            "organization": "Independent Fixture Review Organization",
            "reviewed_scope": "Exact supervised test-mode private-canary candidate",
            "evidence_uri": f"s3://fixture-governance/{domain}.json",
            "approved_at": stamp(when),
        }
    exercises = {
        exercise: {
            "status": "PASS",
            "evidence_uri": f"s3://fixture-governance/{exercise}.json",
            "completed_at": stamp(now - dt.timedelta(minutes=50)),
        }
        for exercise in (
            "support_tabletop",
            "security_tabletop",
            "dsar_export_deletion",
            "backup_tombstone",
            "asset_and_model_provenance",
        )
    }
    governance = {
        "schema_version": 1,
        "candidate_commit": commit,
        "scope": "supervised_stripe_test_mode_private_canary",
        "approvals": approvals,
        "exercises": exercises,
    }

    documents = {
        "deploy": deploy,
        "rollback": rollback,
        "restart": restart,
        "canary": canary,
        "soak": soak,
        "governance": governance,
    }
    paths = {}
    for name, document in documents.items():
        path = root / f"evidence/{name}.json"
        write_json(path, document)
        paths[name] = path
    return {
        "documents": documents,
        "paths": paths,
        "samples": samples_path,
        "ciphertext": ciphertext,
        "checked_at": stamp(now),
    }


def validator_command(fixture, root: Path, commit: str, checked_at: str | None = None):
    command = [
        sys.executable,
        str(VALIDATOR),
        "--root",
        str(root),
        "--commit",
        commit,
        "--image",
        IMAGE,
        "--checked-at",
        checked_at or fixture["checked_at"],
    ]
    for name in ("deploy", "rollback", "restart", "canary", "soak", "governance"):
        command.extend(
            [f"--{name}", fixture["paths"][name].relative_to(root).as_posix()]
        )
    return command


def main() -> int:
    commit = subprocess.check_output(
        ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True
    ).strip()
    now = dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
    with tempfile.TemporaryDirectory(prefix="merc-evidence-chain-test.") as temporary:
        fixture_root = Path(temporary) / "fixture"

        fixture = build_fixture(fixture_root, commit, now)
        valid = subprocess.run(
            validator_command(fixture, fixture_root, commit),
            text=True,
            capture_output=True,
            check=False,
        )
        if valid.returncode != 0:
            print(valid.stderr, file=sys.stderr)
            raise AssertionError("valid exact-candidate evidence chain was rejected")
        receipt = json.loads(valid.stdout)
        assert receipt == {
            **receipt,
            "schema_version": 1,
            "kind": "merc_go_closure_evidence_chain_validation",
            "status": "PASS",
            "decision": "ELIGIBLE_FOR_SUPERVISED_LEVEL_B_PRIVATE_CANARY_REVIEW",
            "scope": "supervised_stripe_test_mode_private_canary",
            "candidate_commit": commit,
            "control_image": IMAGE,
            "authority": {
                "level_b_private_canary_only": True,
                "stripe_test_mode_only": True,
                "live_payment_activation": False,
                "unrestricted_public_access": False,
                "unrestricted_supplier_access": False,
            },
            "secret_values_printed": False,
        }

        mutations = {
            "mixed_deploy_commit": lambda item: item["documents"]["deploy"].update(
                expected_commit="a" * 40
            ),
            "mixed_rollback_image": lambda item: item["documents"]["rollback"][
                "candidate"
            ].update(image=PRIOR_IMAGE),
            "mixed_restart_commit": lambda item: item["documents"]["restart"].update(
                expected_commit="a" * 40
            ),
            "mixed_canary_image": lambda item: item["documents"]["canary"].update(
                control_image=PRIOR_IMAGE
            ),
            "short_soak": lambda item: item["documents"]["soak"]["duration"].update(
                requested_seconds=3600, actual_seconds=3600
            ),
            "premature_release": lambda item: item["documents"]["governance"][
                "approvals"
            ]["release_approval"].update(approved_at=stamp(now - dt.timedelta(hours=26))),
            "missing_governance_identity": lambda item: item["documents"]["governance"][
                "approvals"
            ]["legal"].update(approver=""),
            "secret_in_governance": lambda item: item["documents"]["governance"][
                "approvals"
            ]["payments"].update(approver="whsec_fixture_not_a_secret"),
            "reordered_restart": lambda item: item["documents"]["restart"].update(
                started_at=item["documents"]["deploy"]["started_at"]
            ),
            "stale_deploy": lambda item: item["documents"]["deploy"].update(
                started_at=stamp(now - dt.timedelta(days=8, minutes=1)),
                finished_at=stamp(now - dt.timedelta(days=8)),
            ),
        }
        for name, mutation in mutations.items():
            fixture = build_fixture(fixture_root, commit, now)
            mutation(fixture)
            for document_name, document in fixture["documents"].items():
                write_json(fixture["paths"][document_name], document)
            rejected = subprocess.run(
                validator_command(fixture, fixture_root, commit),
                text=True,
                capture_output=True,
                check=False,
            )
            if rejected.returncode == 0:
                raise AssertionError(f"validator accepted hostile mutation {name}")

        fixture = build_fixture(fixture_root, commit, now)
        raw_samples = fixture["samples"].read_text(encoding="utf-8").splitlines()
        first_sample = json.loads(raw_samples[0])
        first_sample["control_rss_kb"] += 1
        raw_samples[0] = json.dumps(first_sample, sort_keys=True, separators=(",", ":"))
        fixture["samples"].write_text("\n".join(raw_samples) + "\n", encoding="utf-8")
        if subprocess.run(
            validator_command(fixture, fixture_root, commit),
            capture_output=True,
            check=False,
        ).returncode == 0:
            raise AssertionError("validator accepted tampered raw soak samples")

        fixture = build_fixture(fixture_root, commit, now)
        fixture["ciphertext"].write_bytes(
            fixture["ciphertext"].read_bytes() + b"tamper\n"
        )
        if subprocess.run(
            validator_command(fixture, fixture_root, commit),
            capture_output=True,
            check=False,
        ).returncode == 0:
            raise AssertionError("validator accepted tampered backup ciphertext")

        fixture = build_fixture(fixture_root, commit, now)
        fixture["paths"]["deploy"].write_text(
            '{"schema_version":1,"schema_version":1}\n', encoding="utf-8"
        )
        if subprocess.run(
            validator_command(fixture, fixture_root, commit),
            capture_output=True,
            check=False,
        ).returncode == 0:
            raise AssertionError("validator accepted duplicate JSON keys")

        fixture = build_fixture(fixture_root, commit, now)
        deploy_link = fixture_root / "evidence/deploy-link.json"
        deploy_link.symlink_to(fixture["paths"]["deploy"])
        fixture["paths"]["deploy"] = deploy_link
        if subprocess.run(
            validator_command(fixture, fixture_root, commit),
            capture_output=True,
            check=False,
        ).returncode == 0:
            raise AssertionError("validator accepted a symlinked evidence path")

        fixture = build_fixture(fixture_root, commit, now)
        stale_check = stamp(now - dt.timedelta(minutes=6))
        if subprocess.run(
            validator_command(fixture, fixture_root, commit, stale_check),
            capture_output=True,
            check=False,
        ).returncode == 0:
            raise AssertionError("validator accepted a replayed checked_at")

    print(
        "go-closure-evidence-chain: PASS "
        "(fresh ordered exact-candidate receipts and raw artifacts)"
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (AssertionError, OSError, subprocess.SubprocessError, ValueError) as exc:
        print(f"go-closure-evidence-chain test: FAIL: {exc}", file=sys.stderr)
        raise SystemExit(1)

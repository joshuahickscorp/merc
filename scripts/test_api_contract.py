#!/usr/bin/env python3
"""Adversarial tests for the canonical API/client support contract."""

from __future__ import annotations

import copy
import json
from pathlib import Path
import sys
import tempfile
import unittest


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
sys.path.insert(0, str(HERE))

import api_contract  # noqa: E402


SOURCE = ROOT / "proto" / "api-client-support.source.json"


class APIContractTest(unittest.TestCase):
    def setUp(self):
        self.source = api_contract.load_source(SOURCE)
        self.authorities = api_contract._authority_inputs(self.source)

    def assert_invalid(self, source, pattern, *, authorities=None):
        with self.assertRaisesRegex(api_contract.ContractValidationError, pattern):
            api_contract.build_contract(
                source,
                authority_overrides=authorities or self.authorities,
            )

    def test_current_contract_is_deterministic_and_honestly_in_progress(self):
        first = api_contract.build_contract(
            self.source, authority_overrides=self.authorities
        )
        second = api_contract.build_contract(
            copy.deepcopy(self.source), authority_overrides=self.authorities
        )
        self.assertEqual(
            api_contract.render_outputs(first), api_contract.render_outputs(second)
        )
        self.assertEqual(first["status"], "IN_PROGRESS")
        self.assertFalse(first["outcome_proven"])
        self.assertTrue(first["coverage"]["javascript_client_absent"])
        self.assertGreater(first["counts"]["http_routes"], 40)

    def test_missing_advertised_go_route_is_rejected(self):
        authorities = dict(self.authorities)
        api_text = str(authorities["http_routes"])
        line = next(
            row
            for row in api_text.splitlines(keepends=True)
            if 'mux.Handle("POST /v1/jobs"' in row
        )
        authorities["http_routes"] = api_text.replace(line, "", 1)
        self.assert_invalid(
            self.source,
            r"advertises missing Go route.*POST /v1/jobs",
            authorities=authorities,
        )

    def test_multiline_route_registration_is_inventoried(self):
        authorities = dict(self.authorities)
        api_text = str(authorities["http_routes"])
        original = next(
            row
            for row in api_text.splitlines(keepends=True)
            if 'mux.Handle("POST /v1/jobs"' in row
        )
        multiline = (
            "\tmux.Handle(\n"
            '\t\t"POST /v1/jobs",\n'
            "\t\ts.authBuyer(http.HandlerFunc(s.handleCreateJob)),\n"
            "\t)\n"
        )
        authorities["http_routes"] = api_text.replace(original, multiline, 1)
        contract = api_contract.build_contract(
            self.source, authority_overrides=authorities
        )
        route = next(
            row for row in contract["http_routes"] if row["route"] == "POST /v1/jobs"
        )
        self.assertEqual(route["auth_kind"], "buyer_bearer_or_session")
        self.assertEqual(route["handler"], "handleCreateJob")
        self.assertEqual(
            contract["counts"]["http_routes"],
            len(api_contract.parse_routes(str(authorities["http_routes"]))),
        )

    def test_public_idempotent_logout_cannot_be_mislabeled_as_cookie_authenticated(self):
        source = copy.deepcopy(self.source)
        row = next(
            item
            for item in source["direct_route_auth"]
            if item["route"] == "POST /admin/passkey/logout"
        )
        row["auth_kind"] = "admin_passkey_session_cookie"
        self.assert_invalid(
            source,
            r"POST /admin/passkey/logout auth kind must be .*public_idempotent_optional_session_cookie",
        )

    def test_ambiguous_sync_or_stream_flags_are_rejected(self):
        for field in ("synchronous_inference", "server_token_streaming"):
            with self.subTest(field=field):
                source = copy.deepcopy(self.source)
                del source["clients"]["python"]["operations"][0][field]
                self.assert_invalid(source, rf"missing field.*{field}")

        source = copy.deepcopy(self.source)
        source["clients"]["cli"]["operations"][0]["server_token_streaming"] = "unknown"
        self.assert_invalid(source, r"server_token_streaming must be an explicit boolean")

    def test_python_and_cli_surface_drift_is_rejected(self):
        authorities = dict(self.authorities)
        authorities["python_client"] = str(authorities["python_client"]).replace(
            "    def models(self):", "    def catalogue(self):", 1
        )
        self.assert_invalid(
            self.source,
            r"Client.models route inventory mismatch|Python operation references missing Client.models",
            authorities=authorities,
        )

        authorities = dict(self.authorities)
        authorities["cli_client"] = str(authorities["cli_client"]).replace(
            'case "models":', 'case "catalogue":', 1
        )
        self.assert_invalid(
            self.source,
            r"CLI contract advertises missing command models",
            authorities=authorities,
        )

    def test_future_public_async_python_method_cannot_escape_inventory(self):
        authorities = dict(self.authorities)
        authorities["python_client"] = str(authorities["python_client"]) + (
            "\n    async def future_stream(self):\n"
            "        return self._request(\"GET\", \"/v1/models\")\n"
        )
        self.assert_invalid(
            self.source,
            r"Python public method inventory mismatch.*future_stream",
            authorities=authorities,
        )

    def test_tracked_outputs_are_checkable_without_rewriting(self):
        contract = api_contract.build_contract(
            self.source, authority_overrides=self.authorities
        )
        outputs = api_contract.render_outputs(contract)
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            api_contract.write_outputs(root, outputs)
            self.assertEqual(api_contract.stale_outputs(root, outputs), [])
            stale = root / "docs" / "API_CLIENT_SUPPORT.md"
            stale.write_text("stale\n", encoding="utf-8")
            self.assertEqual(
                api_contract.stale_outputs(root, outputs),
                ["docs/API_CLIENT_SUPPORT.md"],
            )
            self.assertEqual(stale.read_text(encoding="utf-8"), "stale\n")

    def test_cli_emits_machine_readable_artifacts_without_claiming_completion(self):
        with tempfile.TemporaryDirectory() as tmp:
            artifacts = Path(tmp) / "artifacts"
            self.assertEqual(
                api_contract.main(
                    [
                        "--source",
                        str(SOURCE),
                        "--output-root",
                        str(ROOT),
                        "--check",
                        "--artifact-dir",
                        str(artifacts),
                    ]
                ),
                0,
            )
            report = json.loads(
                (artifacts / "report.json").read_text(encoding="utf-8")
            )
            contract = json.loads(
                (artifacts / "contract.json").read_text(encoding="utf-8")
            )
            self.assertEqual(report["status"], "PASS")
            self.assertEqual(report["contract_status"], "IN_PROGRESS")
            self.assertFalse(report["outcome_proven"])
            self.assertEqual(report["input_sha256"], contract["input_sha256"])
            self.assertTrue(report["javascript_client_absent"])


if __name__ == "__main__":
    unittest.main()

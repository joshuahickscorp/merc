#!/usr/bin/env python3
"""Hardware-free contracts for the production release surface.

These tests pin the wiring that is easiest to break without a compiler noticing:
the demo's distroless assets and live-money guardrails, Stripe endpoint scope,
production preflight, and buyer-client ownership of generated model kinds.
"""

from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parent.parent
DEMO_ASSETS = {
    "btn-add-payment-shell@3x.png",
    "btn-launch-shell@3x.png",
    "cx-mark-white.png",
    "dot-ring@3x.png",
    "knob-off@3x.png",
    "knob-on@3x.png",
    "knob-pressed@3x.png",
    "knob-red@3x.png",
}


def read(relative: str) -> str:
    return (ROOT / relative).read_text(encoding="utf-8")


class ReleaseSurfaceContracts(unittest.TestCase):
    def test_demo_flat_assets_are_routed_and_containerized(self):
        demo = read("web/demo.html")
        dockerfile = read("Dockerfile.control")
        api = read("control/api.go")
        source = read("proto/api-client-support.source.json")

        referenced = set(re.findall(r"(?:src=\"|url\()assets/([^\"')]+)", demo))
        self.assertEqual(referenced, DEMO_ASSETS)
        for asset in sorted(DEMO_ASSETS):
            self.assertIn(
                f"COPY web/assets/{asset} /web/assets/{asset}", dockerfile
            )
            self.assertIn(f'"{asset}"', api)
        self.assertIn('mux.HandleFunc("GET /assets/{path...}"', api)
        self.assertIn('"route": "GET /assets/{path...}"', source)

    def test_live_demo_is_fail_closed_and_uses_real_contracts(self):
        demo = read("web/demo.html")

        for stale in (
            "compute.exchange/v1/embeddings",
            "cx embed",
            "cx jobs watch",
            "/v1/supplier/status?email",
            "tax_id",
            "tax_on_file",
            "input:JSON.stringify(inputJSONL)",
            "Launch anyway",
        ):
            self.assertNotIn(stale, demo)

        for required in (
            "project.serverConfirmed===true",
            "p&&p.supported&&p.launchable",
            "btn.disabled||!launchableProject()",
            "fetch('/v1/supplier/status'",
            "body:'{}'",
            "fetch('/v1/quote/pipeline'",
            "quote returned no enforceable positive aggregate cap",
            "max_usd:cap",
            "https://computexchange.net/v1/jobs",
            "python -m pip install ./sdk/python",
            'Client("https://computexchange.net"',
            "./cx submit --model all-minilm-l6-v2 --type embed",
        ):
            self.assertIn(required, demo)

    def test_stripe_webhook_endpoints_are_scope_separated(self):
        stripe = read("scripts/stripe-webhooks.sh")
        self.assertIn('((.connect // false)==$c)', stripe)
        self.assertIn('-d "connect=$connect_scope"', stripe)
        self.assertRegex(
            stripe,
            r'ensure_endpoint "https://\$HOST/v1/stripe/webhook"[\s\S]* false\n',
        )
        self.assertRegex(
            stripe,
            r'ensure_endpoint "https://\$HOST/v1/stripe/connect-webhook"[\s\S]* true\n',
        )
        for event in (
            "payment_intent.succeeded",
            "charge.refunded",
            "charge.dispute.created",
            "charge.dispute.closed",
            "account.updated",
        ):
            self.assertIn(event, stripe)

    def test_live_bootstrap_fails_before_backup_or_migration(self):
        bootstrap = read("scripts/bootstrap-prod.sh")
        for variable in (
            "POSTGRES_PASSWORD",
            "MINIO_ROOT_USER",
            "MINIO_ROOT_PASSWORD",
            "ACME_EMAIL",
            "SITE_HOST",
            "CX_PUBLIC_CONTROL_ORIGIN",
            "STRIPE_SECRET_KEY",
            "STRIPE_WEBHOOK_SECRET",
            "CX_CONNECT_WEBHOOK_SECRET",
            "CX_CONNECT_RETURN_URL",
            "CX_CONNECT_REFRESH_URL",
            "CX_TOKEN_KEY",
            "CX_VERIFICATION_SAMPLE_SECRET",
            "CX_ECON_SCHEDULE_VERSION",
            "CX_PROCESSOR_PERCENT_BPS",
            "CX_PROCESSOR_FIXED_USD",
            "CX_CONTROL_PLANE_PER_TASK_USD",
            "CX_TARGET_MARGIN_BPS",
        ):
            self.assertRegex(bootstrap, rf"(?m)^req {variable}\s")

        self.assertIn("sk_live_*", bootstrap)
        self.assertIn('docker compose -f docker-compose.prod.yml config -q', bootstrap)
        self.assertLess(
            bootstrap.index("config -q"),
            bootstrap.index("# ── 4. backup BEFORE migrating"),
        )
        self.assertLess(
            bootstrap.index("config -q"),
            bootstrap.index('log "deploy (scripts/deploy.sh)"'),
        )

    def test_generated_runtime_matrix_owns_default_client_wire_kind(self):
        cli = read("control/buyer.go")
        sdk = read("sdk/python/computeexchange/__init__.py")
        runtime = read("control/runtime_matrix.go")

        self.assertIn('Kind string `json:"kind,omitempty"`', cli)
        self.assertIn("model_kind=None", sdk)
        self.assertGreaterEqual(sdk.count("if model_kind is not None:"), 2)
        self.assertIn("normalizeAdvertisedRuntimeModelRef", runtime)
        self.assertIn("submitted.Kind != canonical.Kind", runtime)

    def test_obsolete_state_secret_is_not_a_release_dependency(self):
        for relative in (
            ".env.example",
            "docker-compose.prod.yml",
            "scripts/bootstrap-prod.sh",
            "scripts/setup-keys.sh",
            "control/main.go",
            "docs/SECURITY.md",
        ):
            self.assertNotIn("CX_STATE_SECRET", read(relative), relative)


if __name__ == "__main__":
    unittest.main()

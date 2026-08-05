"""Tests that are deliberately run against an installed SDK wheel.

``scripts/verify-python-sdk-package.sh`` creates a fresh virtual environment,
installs ``clients/sdk/python``, moves outside the repository, and then discovers this
file. That makes these tests catch missing package metadata and accidental
source-tree imports rather than merely proving that ``PYTHONPATH`` works.
"""

import importlib.metadata
import os
from pathlib import Path
import struct
import unittest

import merc
from merc import Client, decode_embeddings_binary


class InstalledPackageTests(unittest.TestCase):
    def test_distribution_and_module_versions_match(self):
        self.assertEqual(
            importlib.metadata.version("merc"),
            merc.__version__,
        )

    def test_import_came_from_the_virtualenv_not_the_checkout(self):
        module_path = Path(merc.__file__).resolve()
        source_roots = os.environ["MERC_SDK_SOURCE_ROOTS"].split(os.pathsep)
        for source_root in map(Path, source_roots):
            try:
                module_path.relative_to(source_root.resolve())
            except ValueError:
                continue
            self.fail(f"merc imported from source tree: {module_path}")

    def test_public_client_constructs_without_network_or_dependencies(self):
        client = Client("https://example.invalid/", "cx_test_key", timeout=2)
        self.assertEqual(client.base_url, "https://example.invalid")
        self.assertEqual(client.api_key, "cx_test_key")
        self.assertEqual(client.timeout, 2)
        self.assertFalse(
            importlib.metadata.distribution("merc").requires or []
        )

    def test_binary_embedding_decoder_is_present_in_installed_package(self):
        artifact = b"CXEM" + struct.pack("<IIIff", 1, 2, 1, 0.25, -0.5)
        self.assertEqual(decode_embeddings_binary(artifact), [[0.25, -0.5]])

    def test_unsupported_workloads_fail_locally(self):
        client = Client("https://example.invalid", "cx_test_key", timeout=2)
        with self.assertRaisesRegex(ValueError, "unsupported job_type"):
            client.submit_job("unknown", "unsupported", input="")
        with self.assertRaisesRegex(ValueError, "unsupported job_type"):
            client.quote("unknown", "unsupported", input="")

    def test_models_preserves_the_cx_list_abstraction(self):
        class ModelsClient(Client):
            def __init__(self, response):
                super().__init__("https://example.invalid", "cx_test_key")
                self.response = response

            def _request(self, method, path, body=None, query=None, headers=None):
                self.assert_request = (method, path)
                return self.response

        model = {"id": "cx-chat-1b", "object": "model"}
        current = ModelsClient({"object": "list", "data": [model]})
        self.assertEqual(current.models(), [model])
        self.assertEqual(current.assert_request, ("GET", "/v1/models"))
        legacy = ModelsClient([model])
        self.assertEqual(legacy.models(), [model])

    def test_buyer_model_kind_defaults_to_server_runtime_authority(self):
        class RecordingClient(Client):
            def __init__(self):
                super().__init__("https://example.invalid", "cx_test_key")
                self.calls = []

            def _request(self, method, path, body=None, query=None, headers=None):
                self.calls.append((method, path, body, query, headers))
                return {"job_id": "test"}

        client = RecordingClient()
        client.submit_job(
            "all-minilm-l6-v2", "embed", input='{"text":"x"}\n'
        )
        self.assertEqual(client.calls[-1][2]["model"], {"ref": "all-minilm-l6-v2"})
        self.assertTrue(client.calls[-1][4]["Idempotency-Key"].startswith("submit-"))
        client.quote("all-minilm-l6-v2", "embed", input='{"text":"x"}\n')
        self.assertEqual(client.calls[-1][2]["model"], {"ref": "all-minilm-l6-v2"})

        client.submit_job(
            "all-minilm-l6-v2",
            "embed",
            input='{"text":"x"}\n',
            model_kind="hf",
        )
        self.assertEqual(
            client.calls[-1][2]["model"],
            {"ref": "all-minilm-l6-v2", "kind": "hf"},
        )

    def test_stranger_identity_and_receipt_paths(self):
        """A stranger must not need curl for signup, keys, or the receipt."""

        class RecordingClient(Client):
            def __init__(self):
                super().__init__("https://example.invalid", "")
                self.calls = []

            def _request(self, method, path, body=None, query=None, headers=None):
                self.calls.append((method, path, body, query, headers))
                if path == "/v1/signup":
                    return {
                        "buyer_id": "b1",
                        "token": "cx_sess_test",
                        "sandbox_key": "cx_test_sandbox",
                        "free_credit_usd": 5.0,
                    }
                if path == "/v1/login":
                    return {"buyer_id": "b1", "token": "cx_sess_login"}
                if path == "/v1/keys":
                    if method == "POST":
                        return {"id": "k1", "key": "cx_test_new", "name": "cli"}
                    return {"keys": [{"id": "k1", "name": "cli"}]}
                if path == "/v1/me":
                    return {"buyer_id": "b1", "free_credit_remaining_usd": 4.5}
                if path.endswith("/receipt"):
                    return {"job_id": "j1", "invoice": {"charged_usd": 0.01}}
                if path.endswith("/invoice"):
                    return {"job_id": "j1", "charged_usd": 0.01}
                return {}

        client = RecordingClient()
        signed = client.signup("buyer@example.test", "password-long-enough")
        self.assertEqual(signed["sandbox_key"], "cx_test_sandbox")
        self.assertEqual(client.api_key, "cx_test_sandbox")
        self.assertEqual(client.calls[0][:2], ("POST", "/v1/signup"))

        client.login("buyer@example.test", "password-long-enough")
        self.assertEqual(client.api_key, "cx_sess_login")

        client.create_key("ci", test=True)
        self.assertEqual(client.calls[-1][0:2], ("POST", "/v1/keys"))
        self.assertEqual(client.calls[-1][2], {"name": "ci", "test": True})

        keys = client.list_keys()
        self.assertEqual(keys, [{"id": "k1", "name": "cli"}])

        me = client.me()
        self.assertEqual(me["free_credit_remaining_usd"], 4.5)

        receipt = client.receipt("j1")
        self.assertEqual(receipt["job_id"], "j1")
        self.assertEqual(client.calls[-1][:2], ("GET", "/v1/jobs/j1/receipt"))

        invoice = client.invoice("j1")
        self.assertEqual(invoice["charged_usd"], 0.01)


if __name__ == "__main__":
    unittest.main()

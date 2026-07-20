"""Tests that are deliberately run against an installed SDK wheel.

``scripts/verify-python-sdk-package.sh`` creates a fresh virtual environment,
installs ``sdk/python``, moves outside the repository, and then discovers this
file. That makes these tests catch missing package metadata and accidental
source-tree imports rather than merely proving that ``PYTHONPATH`` works.
"""

import importlib.metadata
import os
from pathlib import Path
import struct
import unittest

import computeexchange
from computeexchange import Client, decode_embeddings_binary


class InstalledPackageTests(unittest.TestCase):
    def test_distribution_and_module_versions_match(self):
        self.assertEqual(
            importlib.metadata.version("computeexchange"),
            computeexchange.__version__,
        )

    def test_import_came_from_the_virtualenv_not_the_checkout(self):
        module_path = Path(computeexchange.__file__).resolve()
        source_roots = os.environ["CX_SDK_SOURCE_ROOTS"].split(os.pathsep)
        for source_root in map(Path, source_roots):
            try:
                module_path.relative_to(source_root.resolve())
            except ValueError:
                continue
            self.fail(f"computeexchange imported from source tree: {module_path}")

    def test_public_client_constructs_without_network_or_dependencies(self):
        client = Client("https://example.invalid/", "cx_test_key", timeout=2)
        self.assertEqual(client.base_url, "https://example.invalid")
        self.assertEqual(client.api_key, "cx_test_key")
        self.assertEqual(client.timeout, 2)
        self.assertFalse(
            importlib.metadata.distribution("computeexchange").requires or []
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

    def test_buyer_model_kind_defaults_to_server_runtime_authority(self):
        class RecordingClient(Client):
            def __init__(self):
                super().__init__("https://example.invalid", "cx_test_key")
                self.calls = []

            def _request(self, method, path, body=None, query=None):
                self.calls.append((method, path, body, query))
                return {"job_id": "test"}

        client = RecordingClient()
        client.submit_job(
            "all-minilm-l6-v2", "embed", input='{"text":"x"}\n'
        )
        self.assertEqual(client.calls[-1][2]["model"], {"ref": "all-minilm-l6-v2"})
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


if __name__ == "__main__":
    unittest.main()

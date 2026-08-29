#!/usr/bin/env python3
"""Conformance: merc's realtime surface driven by the OFFICIAL OpenAI Python SDK.

merc's own tests speak to merc with merc's own client, so they prove merc agrees
with itself. The claim being made to buyers is different and stronger: that code
already written against `openai` works by changing base_url. Only the official
client can show that -- it is the thing that will actually be pointed at merc,
and it is free to send headers, retry, and parse responses however it likes.

Emits one JSON object on stdout. The consumer is
src/control/realtime_integration_test.go, which requires every capability flag to be
true AND status == "PASS"; a partial run is a failure, not a partial pass.

Rewritten 2026-07-27. The original was untracked and destroyed during the
[KILL-RT] cleanup; this reconstructs the contract from its call site.
"""

import json
import os
import sys
import traceback

MODEL = os.environ.get("MERC_CONFORMANCE_MODEL", "cx-chat-1b")

result = {
    "status": "FAIL",
    "openai_python_version": "",
    "json_completion": False,
    "streaming_completion": False,
    "receipt_verified": False,
    "authorization_receipt": False,
    "models_list": False,
    "parallel_tool_calls": False,
    "structured_output": False,
    "errors": [],
}


def fail(stage, exc):
    result["errors"].append(f"{stage}: {type(exc).__name__}: {exc}")


def main() -> int:
    origin = os.environ.get("MERC_CONFORMANCE_ORIGIN", "").rstrip("/")
    api_key = os.environ.get("MERC_CONFORMANCE_API_KEY", "")
    if not origin or not api_key:
        result["errors"].append(
            "MERC_CONFORMANCE_ORIGIN and MERC_CONFORMANCE_API_KEY are required")
        print(json.dumps(result))
        return 2

    try:
        import openai
    except ImportError as exc:
        # Absence is not a pass. The integration test only runs this harness
        # when MERC_TEST_OPENAI_PYTHON is set, so being asked to run without
        # the SDK present means the environment is wrong.
        fail("import openai", exc)
        print(json.dumps(result))
        return 2

    result["openai_python_version"] = getattr(openai, "__version__", "")
    client = openai.OpenAI(base_url=origin + "/v1", api_key=api_key, max_retries=0)

    # 1. A plain completion, parsed by the SDK's own models. If merc's response
    #    shape is off in any field the SDK cares about, this raises rather than
    #    quietly returning something the buyer's code will trip over later.
    try:
        completion = client.chat.completions.create(
            model=MODEL,
            messages=[{"role": "user", "content": "conformance probe"}],
            max_completion_tokens=16,
        )
        assert completion.id, "completion carried no id"
        assert completion.choices, "completion carried no choices"
        assert completion.choices[0].message.role == "assistant"
        assert completion.usage is not None, "completion carried no usage"
        result["json_completion"] = True
    except Exception as exc:  # noqa: BLE001 - the report is the error surface
        fail("json_completion", exc)

    # 2. Streaming. The SSE framing is where an OpenAI-compatible surface most
    #    often diverges: a missing [DONE], a malformed chunk, or a delta shape
    #    the SDK cannot parse.
    try:
        chunks = 0
        stream = client.chat.completions.create(
            model=MODEL,
            messages=[{"role": "user", "content": "stream probe"}],
            max_completion_tokens=16,
            stream=True,
        )
        for chunk in stream:
            assert chunk.id, "stream chunk carried no id"
            chunks += 1
        assert chunks > 0, "stream produced no chunks"
        result["streaming_completion"] = True
    except Exception as exc:  # noqa: BLE001
        fail("streaming_completion", exc)

    # 3. The receipt headers. These are merc's, not OpenAI's, so the SDK does
    #    not surface them through the typed response -- the raw response is the
    #    only place they exist, and a buyer reconciling a charge needs them.
    try:
        raw = client.chat.completions.with_raw_response.create(
            model=MODEL,
            messages=[{"role": "user", "content": "receipt probe"}],
            max_completion_tokens=8,
        )
        headers = raw.headers
        contract = headers.get("X-Merc-Contract-ID", "")
        receipt = headers.get("X-Merc-Receipt", "")
        max_usd = headers.get("X-Merc-Max-USD", "")
        assert contract, "no X-Merc-Contract-ID"
        assert receipt, "no X-Merc-Receipt"
        assert float(max_usd) > 0, f"X-Merc-Max-USD not a positive number: {max_usd!r}"
        result["receipt_verified"] = True
    except Exception as exc:  # noqa: BLE001
        fail("receipt_verified", exc)

    # 4. Authorization actually gates. A surface that accepts a bad key would
    #    serve unpaid work, so a rejection here is the pass condition.
    try:
        import openai as openai_mod

        unauthorized = openai_mod.OpenAI(
            base_url=origin + "/v1", api_key="merc_definitely_not_a_key", max_retries=0)
        try:
            unauthorized.chat.completions.create(
                model=MODEL,
                messages=[{"role": "user", "content": "unauthorized probe"}],
                max_completion_tokens=8,
            )
            raise AssertionError("an invalid API key was accepted")
        except openai_mod.AuthenticationError:
            result["authorization_receipt"] = True
        except openai_mod.PermissionDeniedError:
            result["authorization_receipt"] = True
    except Exception as exc:  # noqa: BLE001
        fail("authorization_receipt", exc)

    # 5. Model listing: how a buyer's tooling discovers what merc serves.
    try:
        models = client.models.list()
        ids = [m.id for m in models.data]
        assert ids, "models.list returned nothing"
        result["models_list"] = True
    except Exception as exc:  # noqa: BLE001
        fail("models_list", exc)

    # 6. parallel_tool_calls and 7. structured output are both request-shape
    #    parameters an OpenAI-compatible surface must at minimum ACCEPT without
    #    erroring; merc's catalogue models do not emit tool calls, so this
    #    asserts the parameter is honoured as far as the response shape, not
    #    that a tool was called.
    try:
        tooled = client.chat.completions.create(
            model=MODEL,
            messages=[{"role": "user", "content": "tool probe"}],
            max_completion_tokens=16,
            parallel_tool_calls=True,
            tools=[{
                "type": "function",
                "function": {
                    "name": "noop",
                    "description": "does nothing",
                    "parameters": {"type": "object", "properties": {}},
                },
            }],
        )
        assert tooled.choices, "tool-enabled completion carried no choices"
        result["parallel_tool_calls"] = True
    except Exception as exc:  # noqa: BLE001
        fail("parallel_tool_calls", exc)

    try:
        structured = client.chat.completions.create(
            model=MODEL,
            messages=[{"role": "user", "content": "structured probe"}],
            max_completion_tokens=16,
            response_format={"type": "json_object"},
        )
        assert structured.choices, "structured completion carried no choices"
        result["structured_output"] = True
    except Exception as exc:  # noqa: BLE001
        fail("structured_output", exc)

    capabilities = [
        "json_completion", "streaming_completion", "receipt_verified",
        "authorization_receipt", "models_list", "parallel_tool_calls",
        "structured_output",
    ]
    result["status"] = "PASS" if all(result[c] for c in capabilities) else "FAIL"
    print(json.dumps(result))
    return 0 if result["status"] == "PASS" else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # noqa: BLE001
        result["errors"].append("harness: " + traceback.format_exc(limit=3))
        print(json.dumps(result))
        sys.exit(2)

#!/usr/bin/env python3
"""Compile and verify the canonical HTTP/API/client support contract.

The source contract says what each shipped client advertises.  This compiler then
reads the actual Go router, Python SDK, Go CLI, OpenAI batch adapter, and runtime
matrix.  It fails closed when the advertisement and implementation diverge.

This is deliberately a *support-boundary* proof, not a product-completion proof:
static source agreement cannot establish a live service, physical runtime, money
movement, package publication, or full black-box coverage.
"""

from __future__ import annotations

import argparse
import ast
import copy
from collections import Counter
import hashlib
import json
from pathlib import Path
import re
import sys
from typing import Any, Iterable, Mapping, Sequence


ROOT = Path(__file__).resolve().parent.parent
DEFAULT_SOURCE = ROOT / "proto" / "api-client-support.source.json"
OUTPUT_PATHS = (
    "docs/API_CLIENT_SUPPORT.md",
    "proof/api-client-support.generated.json",
)

ROOT_FIELDS = {
    "schema_version",
    "contract_version",
    "status",
    "authorities",
    "direct_route_auth",
    "evidence_catalog",
    "openai_batch_subset",
    "unsupported_operations",
    "clients",
    "completion_blockers",
}
AUTHORITY_FIELDS = {
    "http_routes",
    "python_client",
    "cli_client",
    "openai_batch_adapter",
    "runtime_matrix",
}
OPERATION_COMMON_FIELDS = {
    "id",
    "support",
    "api_routes",
    "server_execution",
    "client_call_shape",
    "synchronous_inference",
    "server_token_streaming",
    "post_completion_artifact_streaming",
    "evidence",
}
PYTHON_OPERATION_FIELDS = OPERATION_COMMON_FIELDS | {"symbol"}
CLI_OPERATION_FIELDS = OPERATION_COMMON_FIELDS | {
    "command",
    "source_symbols",
    "auth_kind",
}
DERIVED_AUTH = {
    "buyer": "buyer_bearer_or_session",
    "worker": "worker_token_header",
    "admin": "admin_bearer_or_passkey_session",
}
# Direct handlers enforce any request-level proof inside the handler rather than via
# an auth wrapper.  Logout is deliberately public and idempotent: an optional session
# cookie is revoked when present, while an anonymous request still clears stale browser
# state.  Pin that subtle boundary so the inventory cannot drift back to claiming a
# mandatory authenticated cookie.
DIRECT_AUTH_SEMANTIC_GUARDS = {
    "POST /admin/passkey/logout": "public_idempotent_optional_session_cookie",
}
ALLOWED_ALIAS_CLASSIFICATIONS = {
    "omitted_model_default",
    "native_identity",
}
ID_RE = re.compile(r"^[a-z0-9][a-z0-9._-]*$")


class ContractValidationError(ValueError):
    """The source contract or one of its code authorities is inconsistent."""


def _duplicate_safe_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ContractValidationError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def load_json(path: Path | str) -> dict[str, Any]:
    source_path = Path(path)
    try:
        with source_path.open("r", encoding="utf-8") as handle:
            value = json.load(handle, object_pairs_hook=_duplicate_safe_object)
    except (OSError, json.JSONDecodeError) as exc:
        raise ContractValidationError(f"cannot read {source_path}: {exc}") from exc
    if not isinstance(value, dict):
        raise ContractValidationError(f"{source_path} root must be an object")
    return value


def load_source(path: Path | str = DEFAULT_SOURCE) -> dict[str, Any]:
    return load_json(path)


def _exact_keys(value: Mapping[str, Any], expected: set[str], context: str) -> None:
    missing = sorted(expected - set(value))
    unknown = sorted(set(value) - expected)
    if missing:
        raise ContractValidationError(f"{context} missing field(s): {', '.join(missing)}")
    if unknown:
        raise ContractValidationError(f"{context} has unknown field(s): {', '.join(unknown)}")


def _object(value: Any, context: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ContractValidationError(f"{context} must be an object")
    return value


def _list(value: Any, context: str, *, nonempty: bool = False) -> list[Any]:
    if not isinstance(value, list) or (nonempty and not value):
        qualifier = "a non-empty list" if nonempty else "a list"
        raise ContractValidationError(f"{context} must be {qualifier}")
    return value


def _string(value: Any, context: str, *, allow_empty: bool = False) -> str:
    if not isinstance(value, str) or (not allow_empty and not value.strip()):
        qualifier = "a string" if allow_empty else "a non-empty string"
        raise ContractValidationError(f"{context} must be {qualifier}")
    return value


def _boolean(value: Any, context: str) -> bool:
    if not isinstance(value, bool):
        raise ContractValidationError(f"{context} must be an explicit boolean")
    return value


def _string_list(value: Any, context: str, *, nonempty: bool = False) -> list[str]:
    raw = _list(value, context, nonempty=nonempty)
    result = [_string(item, f"{context}[{index}]") for index, item in enumerate(raw)]
    if len(result) != len(set(result)):
        raise ContractValidationError(f"{context} contains duplicate values")
    return result


def _id(value: Any, context: str) -> str:
    result = _string(value, context)
    if not ID_RE.fullmatch(result):
        raise ContractValidationError(f"{context} has invalid id {result!r}")
    return result


def canonical_json(value: Any) -> str:
    return json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False) + "\n"


def _route_parts(route: str, context: str) -> tuple[str, str]:
    match = re.fullmatch(r"([A-Z]+) (/\S*)", route)
    if not match:
        raise ContractValidationError(f"{context} must be 'METHOD /path', got {route!r}")
    return match.group(1), match.group(2)


def route_shape(route: str) -> str:
    """Normalize parameter names while preserving method and path structure."""

    method, path = _route_parts(route, "route")
    path = path.split("?", 1)[0]
    path = re.sub(r"\{[^{}]*\}", "{}", path)
    return f"{method} {path}"


def route_surface(path: str) -> str:
    if path.startswith("/admin"):
        return "admin"
    if path.startswith("/v1/worker"):
        return "worker"
    if path.startswith("/v1/supplier"):
        return "supplier"
    if path.startswith("/v1/files") or path.startswith("/v1/batches"):
        return "openai_batch_subset"
    if path.startswith("/v1"):
        return "buyer_or_public_v1"
    return "public_or_operational"


def _go_call_openings(text: str, prefixes: Sequence[str]) -> list[tuple[int, int, str]]:
    """Return code-state call openings, ignoring lookalikes in comments/strings."""

    found: list[tuple[int, int, str]] = []
    state = "code"
    index = 0
    while index < len(text):
        char = text[index]
        nxt = text[index + 1] if index + 1 < len(text) else ""
        if state == "code":
            matched = False
            for prefix in prefixes:
                if text.startswith(prefix, index):
                    cursor = index + len(prefix)
                    while cursor < len(text) and text[cursor].isspace():
                        cursor += 1
                    if cursor < len(text) and text[cursor] == "(":
                        found.append((index, cursor, prefix))
                        index = cursor
                        matched = True
                        break
            if matched:
                index += 1
                continue
            if char == '"':
                state = "string"
            elif char == "'":
                state = "rune"
            elif char == "`":
                state = "raw"
            elif char == "/" and nxt == "/":
                state = "line_comment"
                index += 1
            elif char == "/" and nxt == "*":
                state = "block_comment"
                index += 1
        elif state in {"string", "rune"}:
            if char == "\\":
                index += 1
            elif (state == "string" and char == '"') or (state == "rune" and char == "'"):
                state = "code"
        elif state == "raw":
            if char == "`":
                state = "code"
        elif state == "line_comment":
            if char == "\n":
                state = "code"
        elif state == "block_comment":
            if char == "*" and nxt == "/":
                state = "code"
                index += 1
        index += 1
    return found


def parse_routes(api_text: str) -> list[dict[str, str]]:
    routes: list[dict[str, str]] = []
    seen: set[str] = set()
    registrations = _go_call_openings(api_text, ("mux.HandleFunc", "mux.Handle"))
    for start, opening, prefix in registrations:
        closing = _matching_delimiter(api_text, opening, "(", ")")
        call = api_text[opening + 1 : closing]
        match = re.match(
            r'\s*"(?P<method>[A-Z]+) (?P<path>/[^\"]*)"\s*,(?P<rest>[\s\S]*)\Z',
            call,
        )
        line_no = api_text.count("\n", 0, start) + 1
        if not match:
            raise ContractValidationError(
                f"cannot parse {prefix} registration at control/api.go:{line_no}; route patterns must use a literal 'METHOD /path' first argument"
            )
        method, path, rest = match.group("method"), match.group("path"), match.group("rest")
        key = f"{method} {path}"
        if key in seen:
            raise ContractValidationError(f"duplicate Go route registration {key}")
        seen.add(key)
        if ".authBuyer(" in rest:
            middleware = "buyer"
        elif ".authWorker(" in rest:
            middleware = "worker"
        elif ".authAdmin(" in rest:
            middleware = "admin"
        else:
            middleware = "direct"
        handler_match = re.search(r"s\.(handle[A-Za-z0-9_]+)", rest)
        if not handler_match:
            raise ContractValidationError(f"cannot find handler for {key} on control/api.go:{line_no}")
        routes.append(
            {
                "method": method,
                "path": path,
                "route": key,
                "handler": handler_match.group(1),
                "middleware": middleware,
                "surface": route_surface(path),
            }
        )
    if len(routes) != len(registrations):
        raise ContractValidationError(
            f"parsed {len(routes)} Go routes from {len(registrations)} mux registrations"
        )
    if not routes:
        raise ContractValidationError("control/api.go yielded no HTTP routes")
    return routes


def apply_route_auth(
    routes: list[dict[str, str]], direct_auth_rows: Sequence[Any]
) -> list[dict[str, str]]:
    overrides: dict[str, str] = {}
    for index, candidate in enumerate(direct_auth_rows):
        context = f"direct_route_auth[{index}]"
        row = _object(candidate, context)
        _exact_keys(row, {"route", "auth_kind"}, context)
        route = _string(row["route"], f"{context}.route")
        _route_parts(route, f"{context}.route")
        auth = _string(row["auth_kind"], f"{context}.auth_kind")
        if route in overrides:
            raise ContractValidationError(f"duplicate direct auth override for {route}")
        overrides[route] = auth

    direct = {row["route"] for row in routes if row["middleware"] == "direct"}
    if direct != set(overrides):
        missing = sorted(direct - set(overrides))
        extra = sorted(set(overrides) - direct)
        details: list[str] = []
        if missing:
            details.append("unclassified direct routes: " + ", ".join(missing))
        if extra:
            details.append("overrides for non-direct/missing routes: " + ", ".join(extra))
        raise ContractValidationError("direct route auth inventory mismatch: " + "; ".join(details))

    for route, expected in DIRECT_AUTH_SEMANTIC_GUARDS.items():
        if route in direct and overrides.get(route) != expected:
            raise ContractValidationError(
                f"{route} auth kind must be {expected!r}; the handler is public and "
                "uses only an optional session cookie"
            )

    result = copy.deepcopy(routes)
    for row in result:
        middleware = row["middleware"]
        row["auth_kind"] = overrides[row["route"]] if middleware == "direct" else DERIVED_AUTH[middleware]
    return result


def _py_path(expression: ast.AST) -> str:
    if isinstance(expression, ast.Constant) and isinstance(expression.value, str):
        return expression.value
    if isinstance(expression, ast.BinOp) and isinstance(expression.op, ast.Add):
        return _py_path(expression.left) + _py_path(expression.right)
    if (
        isinstance(expression, ast.Call)
        and isinstance(expression.func, ast.Name)
        and expression.func.id == "str"
        and expression.args
    ):
        name = expression.args[0]
        if isinstance(name, ast.Name):
            return "{" + re.sub(r"_id$", "", name.id) + "}"
    return "{dynamic}"


def parse_python_client(python_text: str) -> dict[str, Any]:
    try:
        tree = ast.parse(python_text)
    except SyntaxError as exc:
        raise ContractValidationError(f"cannot parse Python SDK: {exc}") from exc
    client = next(
        (node for node in tree.body if isinstance(node, ast.ClassDef) and node.name == "Client"),
        None,
    )
    if client is None:
        raise ContractValidationError("Python SDK has no Client class")

    methods: dict[str, ast.FunctionDef | ast.AsyncFunctionDef] = {
        node.name: node
        for node in client.body
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
    }
    direct_routes: dict[str, set[str]] = {name: set() for name in methods}
    self_calls: dict[str, set[str]] = {name: set() for name in methods}
    for name, method in methods.items():
        for node in ast.walk(method):
            if not isinstance(node, ast.Call) or not isinstance(node.func, ast.Attribute):
                continue
            if not isinstance(node.func.value, ast.Name) or node.func.value.id != "self":
                continue
            called = node.func.attr
            if called == "_request":
                if len(node.args) < 2:
                    raise ContractValidationError(f"Client.{name} has an unreadable _request call")
                method_arg = node.args[0]
                if not isinstance(method_arg, ast.Constant) or not isinstance(method_arg.value, str):
                    raise ContractValidationError(f"Client.{name} uses a dynamic HTTP method")
                path = _py_path(node.args[1]).split("?", 1)[0]
                direct_routes[name].add(route_shape(f"{method_arg.value} {path}"))
            elif called in methods:
                self_calls[name].add(called)

    def closure(symbol: str, visiting: set[str] | None = None) -> set[str]:
        if symbol not in methods:
            raise ContractValidationError(f"Python operation references missing Client.{symbol}")
        active = set() if visiting is None else visiting
        if symbol in active:
            return set()
        active.add(symbol)
        result = set(direct_routes[symbol])
        for called in self_calls[symbol]:
            result.update(closure(called, active))
        active.remove(symbol)
        return result

    public = sorted(name for name in methods if not name.startswith("_"))
    return {
        "public_methods": public,
        "route_closure": {name: sorted(closure(name)) for name in public},
    }


def _matching_delimiter(
    text: str, opening: int, open_char: str, close_char: str
) -> int:
    """Find a Go delimiter peer while ignoring strings, runes, and comments."""

    if opening >= len(text) or text[opening] != open_char:
        raise ContractValidationError(
            f"internal Go parser called without an opening {open_char!r}"
        )
    depth = 0
    state = "code"
    index = opening
    while index < len(text):
        char = text[index]
        nxt = text[index + 1] if index + 1 < len(text) else ""
        if state == "code":
            if char == '"':
                state = "string"
            elif char == "'":
                state = "rune"
            elif char == "`":
                state = "raw"
            elif char == "/" and nxt == "/":
                state = "line_comment"
                index += 1
            elif char == "/" and nxt == "*":
                state = "block_comment"
                index += 1
            elif char == open_char:
                depth += 1
            elif char == close_char:
                depth -= 1
                if depth == 0:
                    return index
        elif state in {"string", "rune"}:
            if char == "\\":
                index += 1
            elif (state == "string" and char == '"') or (state == "rune" and char == "'"):
                state = "code"
        elif state == "raw":
            if char == "`":
                state = "code"
        elif state == "line_comment":
            if char == "\n":
                state = "code"
        elif state == "block_comment":
            if char == "*" and nxt == "/":
                state = "code"
                index += 1
        index += 1
    raise ContractValidationError("unterminated Go block")


def _matching_brace(text: str, opening: int) -> int:
    return _matching_delimiter(text, opening, "{", "}")


def extract_go_functions(go_text: str) -> dict[str, str]:
    functions: dict[str, str] = {}
    pattern = re.compile(r"(?m)^func\s+(?:\([^\n)]*\)\s*)?([A-Za-z_]\w*)\s*\(")
    for match in pattern.finditer(go_text):
        name = match.group(1)
        opening = go_text.find("{", match.end())
        if opening < 0:
            raise ContractValidationError(f"cannot find body for Go function {name}")
        closing = _matching_brace(go_text, opening)
        if name in functions:
            raise ContractValidationError(f"duplicate Go function {name}")
        functions[name] = go_text[opening + 1 : closing]
    return functions


def _go_argument(text: str, start: int) -> tuple[str, int]:
    depth = 0
    state = "code"
    index = start
    while index < len(text):
        char = text[index]
        if state == "code":
            if char == '"':
                state = "string"
            elif char == "`":
                state = "raw"
            elif char in "([{" :
                depth += 1
            elif char in ")]}" and depth:
                depth -= 1
            elif char == "," and depth == 0:
                return text[start:index], index
        elif state == "string":
            if char == "\\":
                index += 1
            elif char == '"':
                state = "code"
        elif state == "raw":
            if char == "`":
                state = "code"
        index += 1
    raise ContractValidationError("unterminated Go call argument")


def _render_go_path(expression: str) -> str:
    tokens = list(re.finditer(r'"(?:[^"\\]|\\.)*"', expression))
    if not tokens:
        raise ContractValidationError(f"Go client uses a fully dynamic path: {expression.strip()!r}")
    output: list[str] = []
    cursor = 0
    for token in tokens:
        between = expression[cursor : token.start()]
        if cursor and re.search(r"[A-Za-z0-9_.()]", between):
            output.append("{dynamic}")
        try:
            output.append(json.loads(token.group(0)))
        except json.JSONDecodeError as exc:
            raise ContractValidationError(f"cannot decode Go path literal {token.group(0)}") from exc
        cursor = token.end()
    if re.search(r"[A-Za-z0-9_.()]", expression[cursor:]):
        output.append("{dynamic}")
    path = "".join(output).split("?", 1)[0]
    path = re.sub(r"\{dynamic\}", "{}", path)
    return path


def parse_go_client_routes(body: str) -> set[str]:
    result: set[str] = set()
    pattern = re.compile(r'\.do\(\s*"([A-Z]+)"\s*,')
    for match in pattern.finditer(body):
        expression, _ = _go_argument(body, match.end())
        result.add(route_shape(f"{match.group(1)} {_render_go_path(expression)}"))
    return result


def parse_cli(cli_text: str) -> dict[str, Any]:
    functions = extract_go_functions(cli_text)
    # After the cli+control merge, the buyer/operator dispatch lives in
    # dispatchBuyer() (control/buyer.go); the binary's main() is in main.go.
    dispatch = functions.get("dispatchBuyer")
    if dispatch is None:
        raise ContractValidationError("CLI has no dispatchBuyer function")
    # Evidence/operator subcommands of the unified cx binary (audit, prove,
    # source-id, verify) are local tools, not buyer-API-client commands, so they
    # are out of scope for the api-client-support contract.
    non_api_commands = {"audit", "prove", "source-id", "verify"}
    command_to_symbol: dict[str, str] = {}
    for match in re.finditer(
        r'case\s+"([^\"]+)"\s*:\s*\n\s*([A-Za-z_]\w*)\(args\)', dispatch
    ):
        command, symbol = match.group(1), match.group(2)
        if command in non_api_commands:
            continue
        if command in command_to_symbol:
            raise ContractValidationError(f"duplicate CLI command {command}")
        command_to_symbol[command] = symbol
    help_match = re.search(r"case\s+([^:]+):\s*\n\s*usage\(\)", dispatch)
    help_aliases = sorted(re.findall(r'"([^\"]+)"', help_match.group(1))) if help_match else []
    return {
        "commands": command_to_symbol,
        "help_aliases": help_aliases,
        "functions": functions,
    }


def _go_map(go_text: str, name: str) -> dict[str, str]:
    match = re.search(rf"var\s+{re.escape(name)}\s*=\s*map\[string\]string\s*\{{", go_text)
    if not match:
        raise ContractValidationError(f"OpenAI adapter has no {name} map")
    opening = go_text.find("{", match.start())
    closing = _matching_brace(go_text, opening)
    body = go_text[opening + 1 : closing]
    result: dict[str, str] = {}
    for row in re.finditer(r'^\s*("(?:[^"\\]|\\.)*")\s*:\s*("(?:[^"\\]|\\.)*")\s*,', body, re.M):
        key, value = json.loads(row.group(1)), json.loads(row.group(2))
        if key in result:
            raise ContractValidationError(f"duplicate {name} key {key!r}")
        result[key] = value
    if not result:
        raise ContractValidationError(f"OpenAI adapter {name} map is empty")
    return result


def parse_openai_adapter(openai_text: str) -> dict[str, Any]:
    functions = extract_go_functions(openai_text)
    endpoint_body = functions.get("endpointJobType")
    if endpoint_body is None:
        raise ContractValidationError("OpenAI adapter has no endpointJobType")
    endpoint_jobs = dict(
        re.findall(r'case\s+"([^\"]+)"\s*:\s*return\s+"([^\"]+)"\s*,\s*true', endpoint_body)
    )
    if not endpoint_jobs:
        raise ContractValidationError("OpenAI adapter exposes no batch endpoint labels")
    return {
        "endpoint_jobs": endpoint_jobs,
        "embed_aliases": _go_map(openai_text, "embedModelAliases"),
        "chat_aliases": _go_map(openai_text, "chatModelAliases"),
    }


def _authority_inputs(
    source: Mapping[str, Any],
    *,
    root: Path = ROOT,
    overrides: Mapping[str, str | Mapping[str, Any]] | None = None,
) -> dict[str, Any]:
    authorities = _object(source.get("authorities"), "authorities")
    _exact_keys(authorities, AUTHORITY_FIELDS, "authorities")
    result: dict[str, Any] = {}
    for key in sorted(AUTHORITY_FIELDS):
        if overrides and key in overrides:
            result[key] = overrides[key]
            continue
        relative = _string(authorities[key], f"authorities.{key}")
        path = root / relative
        if key == "runtime_matrix":
            result[key] = load_json(path)
        else:
            try:
                result[key] = path.read_text(encoding="utf-8")
            except OSError as exc:
                raise ContractValidationError(f"cannot read authority {path}: {exc}") from exc
    return result


def _validate_evidence(source: Mapping[str, Any]) -> tuple[list[dict[str, str]], dict[str, str]]:
    rows: list[dict[str, str]] = []
    kinds: dict[str, str] = {}
    for index, candidate in enumerate(_list(source.get("evidence_catalog"), "evidence_catalog", nonempty=True)):
        context = f"evidence_catalog[{index}]"
        row = _object(candidate, context)
        _exact_keys(row, {"id", "kind", "command", "scope"}, context)
        identifier = _id(row["id"], f"{context}.id")
        if identifier in kinds:
            raise ContractValidationError(f"duplicate evidence id {identifier}")
        kind = _string(row["kind"], f"{context}.kind")
        kinds[identifier] = kind
        rows.append(
            {
                "id": identifier,
                "kind": kind,
                "command": _string(row["command"], f"{context}.command"),
                "scope": _string(row["scope"], f"{context}.scope"),
            }
        )
    return sorted(rows, key=lambda row: row["id"]), kinds


def _evidence_refs(value: Any, context: str, evidence_kinds: Mapping[str, str]) -> list[str]:
    refs = _string_list(value, context, nonempty=True)
    unknown = sorted(set(refs) - set(evidence_kinds))
    if unknown:
        raise ContractValidationError(f"{context} references unknown evidence: {', '.join(unknown)}")
    return sorted(refs)


def _validate_operation(
    candidate: Any,
    context: str,
    expected_fields: set[str],
    evidence_kinds: Mapping[str, str],
) -> dict[str, Any]:
    operation = _object(candidate, context)
    _exact_keys(operation, expected_fields, context)
    identifier = _id(operation["id"], f"{context}.id")
    if _string(operation["support"], f"{context}.support") != "supported":
        raise ContractValidationError(f"{context}.support must be 'supported'")
    routes = _string_list(operation["api_routes"], f"{context}.api_routes")
    for index, route in enumerate(routes):
        _route_parts(route, f"{context}.api_routes[{index}]")
    result = copy.deepcopy(operation)
    result["id"] = identifier
    result["api_routes"] = routes
    result["server_execution"] = _string(operation["server_execution"], f"{context}.server_execution")
    result["client_call_shape"] = _string(operation["client_call_shape"], f"{context}.client_call_shape")
    for flag in (
        "synchronous_inference",
        "server_token_streaming",
        "post_completion_artifact_streaming",
    ):
        result[flag] = _boolean(operation[flag], f"{context}.{flag}")
    # No shipped operation currently provides sync inference or server-side token
    # streaming.  A future implementation must first change the canonical contract
    # and add exact tests; a truthy flag cannot silently become an advertisement.
    if result["synchronous_inference"]:
        raise ContractValidationError(f"{context} overclaims synchronous inference")
    if result["server_token_streaming"]:
        raise ContractValidationError(f"{context} overclaims server token streaming")
    result["evidence"] = _evidence_refs(operation["evidence"], f"{context}.evidence", evidence_kinds)
    return result


def _route_index(routes: Sequence[Mapping[str, str]]) -> tuple[dict[str, Mapping[str, str]], dict[str, Mapping[str, str]]]:
    exact = {row["route"]: row for row in routes}
    shapes: dict[str, Mapping[str, str]] = {}
    for row in routes:
        shape = route_shape(row["route"])
        if shape in shapes:
            raise ContractValidationError(f"ambiguous normalized Go route shape {shape}")
        shapes[shape] = row
    return exact, shapes


def _assert_advertised_routes(
    operation: Mapping[str, Any], route_shapes: Mapping[str, Mapping[str, str]], context: str
) -> set[str]:
    declared = {route_shape(route) for route in operation["api_routes"]}
    missing = sorted(declared - set(route_shapes))
    if missing:
        raise ContractValidationError(
            f"{context} advertises missing Go route(s): {', '.join(missing)}"
        )
    return declared


def _validate_clients(
    source: Mapping[str, Any],
    routes: Sequence[Mapping[str, str]],
    python_observed: Mapping[str, Any],
    cli_observed: Mapping[str, Any],
    evidence_kinds: Mapping[str, str],
) -> tuple[dict[str, Any], list[str]]:
    clients = _object(source.get("clients"), "clients")
    _exact_keys(clients, {"python", "cli", "javascript"}, "clients")
    _, shapes = _route_index(routes)
    black_box_missing: list[str] = []

    python = _object(clients["python"], "clients.python")
    _exact_keys(
        python,
        {"implemented", "auth_kind", "distribution", "surface_evidence", "operations"},
        "clients.python",
    )
    if _boolean(python["implemented"], "clients.python.implemented") is not True:
        raise ContractValidationError("clients.python.implemented must be true")
    py_auth = _string(python["auth_kind"], "clients.python.auth_kind")
    py_operations: list[dict[str, Any]] = []
    ids: set[str] = set()
    symbols: set[str] = set()
    for index, candidate in enumerate(_list(python["operations"], "clients.python.operations", nonempty=True)):
        context = f"clients.python.operations[{index}]"
        operation = _validate_operation(candidate, context, PYTHON_OPERATION_FIELDS, evidence_kinds)
        symbol = _string(operation["symbol"], f"{context}.symbol")
        if operation["id"] in ids:
            raise ContractValidationError(f"duplicate operation id {operation['id']}")
        if symbol in symbols:
            raise ContractValidationError(f"duplicate Python operation symbol {symbol}")
        ids.add(operation["id"])
        symbols.add(symbol)
        declared = _assert_advertised_routes(operation, shapes, context)
        observed = set(python_observed["route_closure"].get(symbol, []))
        if declared != observed:
            missing = sorted(declared - observed)
            extra = sorted(observed - declared)
            raise ContractValidationError(
                f"Client.{symbol} route inventory mismatch: missing-in-code={missing}; undeclared-in-contract={extra}"
            )
        for shape in declared:
            if shapes[shape]["auth_kind"] != py_auth:
                raise ContractValidationError(
                    f"Client.{symbol} route {shape} auth is {shapes[shape]['auth_kind']}, expected {py_auth}"
                )
        operation["observed_route_shapes"] = sorted(observed)
        if not any(evidence_kinds[ref] in {"black_box_subset", "database_integration_subset"} for ref in operation["evidence"]):
            black_box_missing.append(operation["id"])
        py_operations.append(operation)
    if symbols != set(python_observed["public_methods"]):
        missing = sorted(symbols - set(python_observed["public_methods"]))
        undeclared = sorted(set(python_observed["public_methods"]) - symbols)
        raise ContractValidationError(
            f"Python public method inventory mismatch: missing-in-code={missing}; undeclared={undeclared}"
        )

    cli = _object(clients["cli"], "clients.cli")
    _exact_keys(
        cli,
        {"implemented", "auth_kind", "distribution", "help_aliases", "surface_evidence", "operations"},
        "clients.cli",
    )
    if _boolean(cli["implemented"], "clients.cli.implemented") is not True:
        raise ContractValidationError("clients.cli.implemented must be true")
    cli_operations: list[dict[str, Any]] = []
    commands: set[str] = set()
    for index, candidate in enumerate(_list(cli["operations"], "clients.cli.operations", nonempty=True)):
        context = f"clients.cli.operations[{index}]"
        operation = _validate_operation(candidate, context, CLI_OPERATION_FIELDS, evidence_kinds)
        command = _string(operation["command"], f"{context}.command")
        symbols_for_operation = _string_list(
            operation["source_symbols"], f"{context}.source_symbols", nonempty=True
        )
        auth_kind = _string(operation["auth_kind"], f"{context}.auth_kind")
        if operation["id"] in ids:
            raise ContractValidationError(f"duplicate operation id {operation['id']}")
        if command in commands:
            raise ContractValidationError(f"duplicate CLI command {command}")
        ids.add(operation["id"])
        commands.add(command)
        actual_entry = cli_observed["commands"].get(command)
        if actual_entry is None:
            raise ContractValidationError(f"CLI contract advertises missing command {command}")
        if actual_entry != symbols_for_operation[0]:
            raise ContractValidationError(
                f"CLI command {command} dispatches to {actual_entry}, contract says {symbols_for_operation[0]}"
            )
        observed: set[str] = set()
        for symbol in symbols_for_operation:
            body = cli_observed["functions"].get(symbol)
            if body is None:
                raise ContractValidationError(f"CLI operation {command} references missing function {symbol}")
            observed.update(parse_go_client_routes(body))
        declared = _assert_advertised_routes(operation, shapes, context)
        if declared != observed:
            raise ContractValidationError(
                f"CLI {command} route inventory mismatch: missing-in-code={sorted(declared-observed)}; undeclared-in-contract={sorted(observed-declared)}"
            )
        for shape in declared:
            if shapes[shape]["auth_kind"] != auth_kind:
                raise ContractValidationError(
                    f"CLI {command} route {shape} auth is {shapes[shape]['auth_kind']}, expected {auth_kind}"
                )
        operation["source_symbols"] = symbols_for_operation
        operation["observed_route_shapes"] = sorted(observed)
        if not any(evidence_kinds[ref] in {"black_box_subset", "database_integration_subset"} for ref in operation["evidence"]):
            black_box_missing.append(operation["id"])
        cli_operations.append(operation)
    if commands != set(cli_observed["commands"]):
        raise ContractValidationError(
            "CLI command inventory mismatch: "
            f"missing-in-code={sorted(commands-set(cli_observed['commands']))}; "
            f"undeclared={sorted(set(cli_observed['commands'])-commands)}"
        )
    declared_help = sorted(_string_list(cli["help_aliases"], "clients.cli.help_aliases", nonempty=True))
    if declared_help != cli_observed["help_aliases"]:
        raise ContractValidationError(
            f"CLI help aliases mismatch: contract={declared_help}; code={cli_observed['help_aliases']}"
        )

    javascript = _object(clients["javascript"], "clients.javascript")
    _exact_keys(javascript, {"implemented", "support", "distribution", "operations", "evidence"}, "clients.javascript")
    if _boolean(javascript["implemented"], "clients.javascript.implemented"):
        raise ContractValidationError("JavaScript client is marked implemented without a code authority")
    if _string(javascript["support"], "clients.javascript.support") != "planned":
        raise ContractValidationError("clients.javascript.support must remain 'planned' while absent")
    if _list(javascript["operations"], "clients.javascript.operations"):
        raise ContractValidationError("absent JavaScript client cannot advertise operations")
    if _list(javascript["evidence"], "clients.javascript.evidence"):
        raise ContractValidationError("absent JavaScript client cannot claim evidence")

    result = {
        "python": {
            "implemented": True,
            "auth_kind": py_auth,
            "distribution": _string(python["distribution"], "clients.python.distribution"),
            "surface_evidence": _evidence_refs(python["surface_evidence"], "clients.python.surface_evidence", evidence_kinds),
            "operations": sorted(py_operations, key=lambda row: row["id"]),
            "observed_public_methods": python_observed["public_methods"],
        },
        "cli": {
            "implemented": True,
            "auth_kind": _string(cli["auth_kind"], "clients.cli.auth_kind"),
            "distribution": _string(cli["distribution"], "clients.cli.distribution"),
            "help_aliases": declared_help,
            "surface_evidence": _evidence_refs(cli["surface_evidence"], "clients.cli.surface_evidence", evidence_kinds),
            "operations": sorted(cli_operations, key=lambda row: row["id"]),
            "observed_commands": sorted(commands),
        },
        "javascript": copy.deepcopy(javascript),
    }
    return result, sorted(black_box_missing)


def _validate_openai(
    source: Mapping[str, Any],
    routes: Sequence[Mapping[str, str]],
    observed: Mapping[str, Any],
    runtime: Mapping[str, Any],
    evidence_kinds: Mapping[str, str],
) -> dict[str, Any]:
    value = _object(source.get("openai_batch_subset"), "openai_batch_subset")
    expected_fields = {
        "compatibility_scope",
        "full_openai_api_compatible",
        "drop_in_sdk_compatible",
        "synchronous_inference",
        "server_token_streaming",
        "endpoint_labels_are_http_routes",
        "supported_http_routes",
        "endpoint_labels",
        "model_names",
        "rejected_model_names",
        "evidence",
    }
    _exact_keys(value, expected_fields, "openai_batch_subset")
    if _string(value["compatibility_scope"], "openai_batch_subset.compatibility_scope") != "batch_workflow_subset":
        raise ContractValidationError("overbroad OpenAI compatibility scope; only batch_workflow_subset is implemented")
    for field in (
        "full_openai_api_compatible",
        "drop_in_sdk_compatible",
        "synchronous_inference",
        "server_token_streaming",
        "endpoint_labels_are_http_routes",
    ):
        if _boolean(value[field], f"openai_batch_subset.{field}"):
            raise ContractValidationError(f"openai_batch_subset.{field} overclaims the implemented subset")

    exact_routes, route_shapes = _route_index(routes)
    supported = _string_list(value["supported_http_routes"], "openai_batch_subset.supported_http_routes", nonempty=True)
    for route in supported:
        if route not in exact_routes:
            raise ContractValidationError(f"OpenAI subset advertises missing Go route {route}")
        if exact_routes[route]["auth_kind"] != "buyer_bearer_or_session":
            raise ContractValidationError(f"OpenAI subset route {route} is not buyer-authenticated")

    endpoint_rows: list[dict[str, str]] = []
    declared_endpoint_jobs: dict[str, str] = {}
    for index, candidate in enumerate(_list(value["endpoint_labels"], "openai_batch_subset.endpoint_labels", nonempty=True)):
        context = f"openai_batch_subset.endpoint_labels[{index}]"
        row = _object(candidate, context)
        _exact_keys(row, {"label", "native_job"}, context)
        label = _string(row["label"], f"{context}.label")
        job = _id(row["native_job"], f"{context}.native_job")
        if label in declared_endpoint_jobs:
            raise ContractValidationError(f"duplicate OpenAI endpoint label {label}")
        if route_shape(f"POST {label}") in route_shapes:
            raise ContractValidationError(
                f"OpenAI batch endpoint label {label} is also registered as a synchronous route; contract must be split explicitly"
            )
        declared_endpoint_jobs[label] = job
        endpoint_rows.append({"label": label, "native_job": job})
    if declared_endpoint_jobs != observed["endpoint_jobs"]:
        raise ContractValidationError(
            f"OpenAI endpoint label mapping mismatch: contract={declared_endpoint_jobs}; code={observed['endpoint_jobs']}"
        )

    cells = _list(runtime.get("cells"), "runtime_matrix.cells", nonempty=True)
    production_cells: dict[tuple[str, str], list[str]] = {}
    for index, candidate in enumerate(cells):
        row = _object(candidate, f"runtime_matrix.cells[{index}]")
        if row.get("lifecycle") == "production" and isinstance(row.get("model"), str):
            production_cells.setdefault((str(row.get("job")), str(row.get("model"))), []).append(str(row.get("id")))

    alias_code = {
        "/v1/embeddings": observed["embed_aliases"],
        "/v1/chat/completions": observed["chat_aliases"],
    }
    alias_rows: list[dict[str, Any]] = []
    declared_aliases: dict[str, dict[str, str]] = {label: {} for label in declared_endpoint_jobs}
    for index, candidate in enumerate(_list(value["model_names"], "openai_batch_subset.model_names", nonempty=True)):
        context = f"openai_batch_subset.model_names[{index}]"
        row = _object(candidate, context)
        _exact_keys(
            row,
            {"endpoint_label", "name", "target", "classification", "semantic_equivalence"},
            context,
        )
        endpoint = _string(row["endpoint_label"], f"{context}.endpoint_label")
        if endpoint not in declared_endpoint_jobs:
            raise ContractValidationError(f"{context} references unknown endpoint label {endpoint}")
        name = _string(row["name"], f"{context}.name", allow_empty=True)
        target = _string(row["target"], f"{context}.target")
        classification = _string(row["classification"], f"{context}.classification")
        semantic = _boolean(row["semantic_equivalence"], f"{context}.semantic_equivalence")
        if classification not in ALLOWED_ALIAS_CLASSIFICATIONS:
            raise ContractValidationError(f"{context} has unknown classification {classification}")
        if name in declared_aliases[endpoint]:
            raise ContractValidationError(f"duplicate model name {name!r} for {endpoint}")
        if name == "" and classification != "omitted_model_default":
            raise ContractValidationError(f"{context} blank name must be omitted_model_default")
        if name == target and classification != "native_identity":
            raise ContractValidationError(f"{context} identical name/target must be native_identity")
        if name not in {"", target}:
            raise ContractValidationError(
                f"{context} branded cross-model aliases are forbidden; reject the name and provide the native model id"
            )
        if name != target and semantic:
            raise ContractValidationError(
                f"{context} overbroad OpenAI compatibility: {name!r} cannot be semantically equivalent to different native model {target!r}"
            )
        if classification == "native_identity" and not semantic:
            raise ContractValidationError(f"{context} native identity must set semantic_equivalence=true")
        job = declared_endpoint_jobs[endpoint]
        cell_ids = sorted(production_cells.get((job, target), []))
        if not cell_ids:
            raise ContractValidationError(
                f"OpenAI model name {name!r} targets {job}/{target} without a production runtime cell"
            )
        declared_aliases[endpoint][name] = target
        alias_rows.append(
            {
                "endpoint_label": endpoint,
                "name": name,
                "target": target,
                "classification": classification,
                "semantic_equivalence": semantic,
                "production_cell_ids": cell_ids,
            }
        )
    if declared_aliases != alias_code:
        raise ContractValidationError(
            f"OpenAI model-name inventory mismatch: contract={declared_aliases}; code={alias_code}"
        )

    rejected_rows: list[dict[str, str]] = []
    rejected_keys: set[tuple[str, str]] = set()
    for index, candidate in enumerate(
        _list(
            value["rejected_model_names"],
            "openai_batch_subset.rejected_model_names",
            nonempty=True,
        )
    ):
        context = f"openai_batch_subset.rejected_model_names[{index}]"
        row = _object(candidate, context)
        _exact_keys(row, {"endpoint_label", "name", "use_native_model"}, context)
        endpoint = _string(row["endpoint_label"], f"{context}.endpoint_label")
        name = _string(row["name"], f"{context}.name")
        native = _string(row["use_native_model"], f"{context}.use_native_model")
        if endpoint not in declared_endpoint_jobs:
            raise ContractValidationError(f"{context} references unknown endpoint label {endpoint}")
        key = (endpoint, name)
        if key in rejected_keys:
            raise ContractValidationError(f"duplicate rejected model name {name!r} for {endpoint}")
        rejected_keys.add(key)
        if name in declared_aliases[endpoint] or name in alias_code[endpoint]:
            raise ContractValidationError(
                f"{context} says {name!r} is rejected but the adapter accepts it"
            )
        default_native = declared_aliases[endpoint].get("")
        if native != default_native:
            raise ContractValidationError(
                f"{context} native guidance {native!r} disagrees with endpoint default {default_native!r}"
            )
        job = declared_endpoint_jobs[endpoint]
        if not production_cells.get((job, native)):
            raise ContractValidationError(
                f"{context} suggests {job}/{native} without a production runtime cell"
            )
        rejected_rows.append(
            {"endpoint_label": endpoint, "name": name, "use_native_model": native}
        )

    return {
        "compatibility_scope": "batch_workflow_subset",
        "full_openai_api_compatible": False,
        "drop_in_sdk_compatible": False,
        "synchronous_inference": False,
        "server_token_streaming": False,
        "endpoint_labels_are_http_routes": False,
        "supported_http_routes": sorted(supported),
        "endpoint_labels": sorted(endpoint_rows, key=lambda row: row["label"]),
        "model_names": sorted(alias_rows, key=lambda row: (row["endpoint_label"], row["name"])),
        "rejected_model_names": sorted(
            rejected_rows, key=lambda row: (row["endpoint_label"], row["name"])
        ),
        "evidence": _evidence_refs(value["evidence"], "openai_batch_subset.evidence", evidence_kinds),
    }


def _validate_unsupported(
    source: Mapping[str, Any], route_shapes: Mapping[str, Mapping[str, str]]
) -> list[dict[str, str]]:
    result: list[dict[str, str]] = []
    ids: set[str] = set()
    for index, candidate in enumerate(_list(source.get("unsupported_operations"), "unsupported_operations", nonempty=True)):
        context = f"unsupported_operations[{index}]"
        row = _object(candidate, context)
        _exact_keys(row, {"id", "route", "reason"}, context)
        identifier = _id(row["id"], f"{context}.id")
        route = _string(row["route"], f"{context}.route")
        _route_parts(route, f"{context}.route")
        if identifier in ids:
            raise ContractValidationError(f"duplicate unsupported operation id {identifier}")
        ids.add(identifier)
        if route_shape(route) in route_shapes:
            raise ContractValidationError(
                f"operation {identifier} is marked unsupported but Go registers {route}"
            )
        result.append({"id": identifier, "route": route, "reason": _string(row["reason"], f"{context}.reason")})
    return sorted(result, key=lambda row: row["id"])


def build_contract(
    source: Mapping[str, Any],
    *,
    root: Path = ROOT,
    authority_overrides: Mapping[str, str | Mapping[str, Any]] | None = None,
) -> dict[str, Any]:
    root_source = _object(source, "source")
    _exact_keys(root_source, ROOT_FIELDS, "source")
    if root_source["schema_version"] != 1 or isinstance(root_source["schema_version"], bool):
        raise ContractValidationError("schema_version must be integer 1")
    contract_version = _string(root_source["contract_version"], "contract_version")
    if _string(root_source["status"], "status") != "in_progress":
        raise ContractValidationError(
            "api/client contract outcome must remain in_progress while JavaScript and black-box coverage are incomplete"
        )
    blockers = sorted(_string_list(root_source["completion_blockers"], "completion_blockers", nonempty=True))
    evidence, evidence_kinds = _validate_evidence(root_source)
    authorities = _authority_inputs(root_source, root=root, overrides=authority_overrides)
    if not isinstance(authorities["runtime_matrix"], Mapping):
        raise ContractValidationError("runtime_matrix authority override must be an object")

    routes = apply_route_auth(
        parse_routes(str(authorities["http_routes"])),
        _list(root_source["direct_route_auth"], "direct_route_auth", nonempty=True),
    )
    exact_routes, route_shapes = _route_index(routes)
    python_observed = parse_python_client(str(authorities["python_client"]))
    cli_observed = parse_cli(str(authorities["cli_client"]))
    openai_observed = parse_openai_adapter(str(authorities["openai_batch_adapter"]))
    clients, black_box_missing = _validate_clients(
        root_source, routes, python_observed, cli_observed, evidence_kinds
    )
    openai = _validate_openai(
        root_source,
        routes,
        openai_observed,
        authorities["runtime_matrix"],
        evidence_kinds,
    )
    unsupported = _validate_unsupported(root_source, route_shapes)

    authority_paths = _object(root_source["authorities"], "authorities")
    raw_hash = hashlib.sha256()
    raw_hash.update(canonical_json(root_source).encode("utf-8"))
    for key in sorted(authorities):
        raw_hash.update(key.encode("utf-8") + b"\0")
        value = authorities[key]
        encoded = canonical_json(value).encode("utf-8") if isinstance(value, Mapping) else str(value).encode("utf-8")
        raw_hash.update(encoded + b"\0")

    auth_counts = dict(sorted(Counter(row["auth_kind"] for row in routes).items()))
    surface_counts = dict(sorted(Counter(row["surface"] for row in routes).items()))
    identifier_rewrites = [
        row for row in openai["model_names"] if row["classification"] == "identifier_rewrite"
    ]
    if identifier_rewrites:
        raise ContractValidationError("OpenAI accepted model names contain forbidden cross-model rewrites")
    return {
        "schema_version": 1,
        "contract_version": contract_version,
        "status": "IN_PROGRESS",
        "outcome_proven": False,
        "input_sha256": raw_hash.hexdigest(),
        "authorities": dict(sorted((_string(k, "authority key"), _string(v, f"authorities.{k}")) for k, v in authority_paths.items())),
        "counts": {
            "http_routes": len(routes),
            "route_auth_kinds": auth_counts,
            "route_surfaces": surface_counts,
            "python_operations": len(clients["python"]["operations"]),
            "cli_operations": len(clients["cli"]["operations"]),
            "javascript_operations": 0,
            "unsupported_operations": len(unsupported),
            "openai_identifier_rewrites": len(identifier_rewrites),
            "openai_rejected_model_names": len(openai["rejected_model_names"]),
        },
        "execution_boundary": {
            "native_inference_transport": "asynchronous_job_submit_poll_download",
            "openai_transport": "batch_workflow_subset",
            "synchronous_inference": False,
            "server_token_streaming": False,
            "post_completion_artifact_streaming_is_not_token_streaming": True,
        },
        "http_routes": sorted(routes, key=lambda row: (row["path"], row["method"])),
        "openai_batch_subset": openai,
        "unsupported_operations": unsupported,
        "clients": clients,
        "evidence_catalog": evidence,
        "coverage": {
            "operations_without_operation_specific_black_box_evidence": black_box_missing,
            "javascript_client_absent": True,
            "rejected_branded_model_names": [
                f"{row['endpoint_label']}:{row['name']} (use {row['use_native_model']})"
                for row in openai["rejected_model_names"]
            ],
        },
        "completion_blockers": blockers,
    }


def render_markdown(contract: Mapping[str, Any]) -> str:
    counts = contract["counts"]
    lines = [
        "# API and client support contract",
        "",
        "> Generated by `scripts/api_contract.py` from the canonical source and shipped code authorities. Do not edit by hand.",
        "",
        f"Contract `{contract['contract_version']}` · **{contract['status']}** · input `{contract['input_sha256']}`",
        "",
        "This inventory proves source agreement, not a live service or full developer-experience outcome. The broad gate remains in progress.",
        "",
        "## Non-negotiable execution boundary",
        "",
        "- Native inference is an asynchronous job workflow: submit, poll, then download a completed artifact.",
        "- The Python `embeddings()` helper blocks while it performs that workflow; it is not a synchronous inference HTTP endpoint.",
        "- The CLI can stream a completed artifact body to stdout; that is not server token streaming.",
        "- `/v1/embeddings` and `/v1/chat/completions` are labels inside OpenAI-shaped batch input. They are not registered HTTP inference routes.",
        "- The implemented OpenAI scope is a batch-workflow subset, not full API or drop-in SDK compatibility.",
        "",
        "## Inventory summary",
        "",
        f"- Go HTTP routes: {counts['http_routes']}",
        f"- Python public operations: {counts['python_operations']}",
        f"- CLI operations: {counts['cli_operations']}",
        f"- JavaScript operations: {counts['javascript_operations']} (client absent)",
        f"- Explicit unsupported operations: {counts['unsupported_operations']}",
        f"- Accepted cross-model identifier rewrites: {counts['openai_identifier_rewrites']} (must remain zero)",
        f"- Explicitly rejected branded model names: {counts['openai_rejected_model_names']}",
        "",
        "### Route authentication",
        "",
        "| Auth kind | Routes |",
        "|---|---:|",
    ]
    for auth, count in counts["route_auth_kinds"].items():
        lines.append(f"| `{auth}` | {count} |")

    openai = contract["openai_batch_subset"]
    lines += [
        "",
        "## OpenAI-shaped batch subset",
        "",
        "Supported HTTP routes:",
        "",
    ]
    lines.extend(f"- `{route}`" for route in openai["supported_http_routes"])
    lines += [
        "",
        "Batch-line endpoint labels:",
        "",
        "| Label (not an HTTP route) | Native job |",
        "|---|---|",
    ]
    for row in openai["endpoint_labels"]:
        lines.append(f"| `{row['label']}` | `{row['native_job']}` |")
    lines += [
        "",
        "Accepted model names are request-name handling rules, not performance or semantic-equivalence claims:",
        "",
        "| Endpoint label | Accepted name | Native target | Classification | Semantically equivalent | Production runtime cell |",
        "|---|---|---|---|---|---|",
    ]
    for row in openai["model_names"]:
        display = row["name"] if row["name"] else "(omitted)"
        cells = ", ".join(f"`{cell}`" for cell in row["production_cell_ids"])
        lines.append(
            f"| `{row['endpoint_label']}` | `{display}` | `{row['target']}` | `{row['classification']}` | "
            f"{'yes' if row['semantic_equivalence'] else 'no'} | {cells} |"
        )

    lines += [
        "",
        "Former branded substitutions are rejected. The error identifies the native model to use:",
        "",
        "| Endpoint label | Rejected name | Use native model |",
        "|---|---|---|",
    ]
    for row in openai["rejected_model_names"]:
        lines.append(
            f"| `{row['endpoint_label']}` | `{row['name']}` | `{row['use_native_model']}` |"
        )

    lines += [
        "",
        "### Explicitly unsupported",
        "",
        "| Operation | Route shape | Reason |",
        "|---|---|---|",
    ]
    for row in contract["unsupported_operations"]:
        lines.append(f"| `{row['id']}` | `{row['route']}` | {row['reason']} |")

    for client_id, heading in (("python", "Python SDK"), ("cli", "CLI")):
        client = contract["clients"][client_id]
        lines += [
            "",
            f"## {heading}",
            "",
            "| Operation | Routes | Server execution | Client call shape | Sync inference | Server token stream | Completed-artifact stream |",
            "|---|---|---|---|---|---|---|",
        ]
        for row in client["operations"]:
            routes = "<br>".join(f"`{route}`" for route in row["api_routes"]) or "(local)"
            lines.append(
                f"| `{row['id']}` | {routes} | `{row['server_execution']}` | `{row['client_call_shape']}` | "
                f"{'yes' if row['synchronous_inference'] else 'no'} | "
                f"{'yes' if row['server_token_streaming'] else 'no'} | "
                f"{'yes' if row['post_completion_artifact_streaming'] else 'no'} |"
            )

    lines += [
        "",
        "## Complete Go route inventory",
        "",
        "| Method | Path | Auth kind | Handler | Surface |",
        "|---|---|---|---|---|",
    ]
    for row in contract["http_routes"]:
        lines.append(
            f"| `{row['method']}` | `{row['path']}` | `{row['auth_kind']}` | `{row['handler']}` | `{row['surface']}` |"
        )

    lines += ["", "## Exact evidence commands", ""]
    for row in contract["evidence_catalog"]:
        lines += [
            f"### `{row['id']}`",
            "",
            f"```sh\n{row['command']}\n```",
            "",
            row["scope"],
            "",
        ]
    lines += ["## Why the broad outcome is still in progress", ""]
    lines.extend(f"- {blocker}" for blocker in contract["completion_blockers"])
    lines += [
        "",
        "Operation IDs lacking operation-specific black-box evidence:",
        "",
    ]
    lines.extend(
        f"- `{identifier}`"
        for identifier in contract["coverage"]["operations_without_operation_specific_black_box_evidence"]
    )
    return "\n".join(lines).rstrip() + "\n"


def render_outputs(contract: Mapping[str, Any]) -> dict[str, str]:
    return {
        "docs/API_CLIENT_SUPPORT.md": render_markdown(contract),
        "proof/api-client-support.generated.json": canonical_json(contract),
    }


def write_outputs(root: Path, outputs: Mapping[str, str]) -> None:
    for relative, content in outputs.items():
        path = root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")


def stale_outputs(root: Path, outputs: Mapping[str, str]) -> list[str]:
    stale: list[str] = []
    for relative, content in outputs.items():
        path = root / relative
        try:
            actual = path.read_text(encoding="utf-8")
        except OSError:
            stale.append(relative)
            continue
        if actual != content:
            stale.append(relative)
    return stale


def write_artifacts(artifact_dir: Path, contract: Mapping[str, Any]) -> None:
    artifact_dir.mkdir(parents=True, exist_ok=True)
    (artifact_dir / "contract.json").write_text(canonical_json(contract), encoding="utf-8")
    report = {
        "status": "PASS",
        "contract_status": contract["status"],
        "outcome_proven": contract["outcome_proven"],
        "input_sha256": contract["input_sha256"],
        "http_routes": contract["counts"]["http_routes"],
        "python_operations": contract["counts"]["python_operations"],
        "cli_operations": contract["counts"]["cli_operations"],
        "javascript_client_absent": contract["coverage"]["javascript_client_absent"],
    }
    (artifact_dir / "report.json").write_text(canonical_json(report), encoding="utf-8")


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, default=DEFAULT_SOURCE)
    parser.add_argument("--output-root", type=Path, default=ROOT)
    parser.add_argument("--artifact-dir", type=Path)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--write", action="store_true", help="write tracked generated outputs")
    mode.add_argument("--check", action="store_true", help="check tracked outputs without rewriting")
    args = parser.parse_args(argv)
    try:
        source = load_source(args.source)
        contract = build_contract(source, root=args.output_root)
        outputs = render_outputs(contract)
        if args.check:
            stale = stale_outputs(args.output_root, outputs)
            if stale:
                raise ContractValidationError(
                    "stale generated API contract output(s): " + ", ".join(stale)
                )
        else:
            write_outputs(args.output_root, outputs)
        if args.artifact_dir:
            write_artifacts(args.artifact_dir, contract)
    except ContractValidationError as exc:
        print(f"api contract: FAIL: {exc}", file=sys.stderr)
        return 1
    print(
        "api contract: PASS "
        f"({contract['counts']['http_routes']} routes; "
        f"{contract['counts']['python_operations']} Python ops; "
        f"{contract['counts']['cli_operations']} CLI ops; "
        "broad outcome IN_PROGRESS)"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

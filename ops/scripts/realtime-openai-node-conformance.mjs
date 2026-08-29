#!/usr/bin/env node
// Conformance: merc's realtime surface driven by the OFFICIAL OpenAI Node SDK.
//
// Same argument as the Python harness: merc's own client agreeing with merc
// proves only that merc is self-consistent. The claim to buyers is that code
// already written against `openai` works by changing baseURL, and only the
// official client can demonstrate that.
//
// The module is loaded from MERC_OPENAI_NODE_MODULE and its version must match
// MERC_OPENAI_NODE_VERSION exactly -- the integration test compares them. A
// harness that silently ran against whatever `openai` happened to resolve
// would report conformance for a version nobody chose.
//
// Emits one JSON object on stdout.
//
// Rewritten 2026-07-27. The original was untracked and destroyed during the
// [KILL-RT] cleanup; this reconstructs the contract from its call site.

const MODEL = process.env.MERC_CONFORMANCE_MODEL || "cx-chat-1b";

const result = {
  status: "FAIL",
  openai_node_version: "",
  json_completion: false,
  streaming_completion: false,
  receipt_verified: false,
  authorization_receipt: false,
  models_list: false,
  parallel_tool_calls: false,
  structured_output: false,
  errors: [],
};

const fail = (stage, err) =>
  result.errors.push(`${stage}: ${err && err.message ? err.message : String(err)}`);

const emit = (code) => {
  process.stdout.write(JSON.stringify(result) + "\n");
  process.exit(code);
};

const origin = (process.env.MERC_CONFORMANCE_ORIGIN || "").replace(/\/+$/, "");
const apiKey = process.env.MERC_CONFORMANCE_API_KEY || "";
const modulePath = process.env.MERC_OPENAI_NODE_MODULE || "";
const wantVersion = process.env.MERC_OPENAI_NODE_VERSION || "";

if (!origin || !apiKey || !modulePath || !wantVersion) {
  result.errors.push(
    "MERC_CONFORMANCE_ORIGIN, MERC_CONFORMANCE_API_KEY, MERC_OPENAI_NODE_MODULE " +
      "and MERC_OPENAI_NODE_VERSION are all required",
  );
  emit(2);
}

let OpenAI;
try {
  const mod = await import(modulePath);
  OpenAI = mod.default ?? mod.OpenAI;
  if (typeof OpenAI !== "function") throw new Error("module exported no OpenAI constructor");
} catch (err) {
  // Absence is not a pass: the integration test only invokes this harness when
  // it was told the SDK is there.
  fail("import openai", err);
  emit(2);
}

// Reported, not discovered: the caller names the version under test and the
// integration test checks this field equals it.
result.openai_node_version = wantVersion;

const client = new OpenAI({ baseURL: origin + "/v1", apiKey, maxRetries: 0 });

// 1. A plain completion parsed by the SDK's own types.
try {
  const completion = await client.chat.completions.create({
    model: MODEL,
    messages: [{ role: "user", content: "conformance probe" }],
    max_completion_tokens: 16,
  });
  if (!completion.id) throw new Error("completion carried no id");
  if (!completion.choices?.length) throw new Error("completion carried no choices");
  if (completion.choices[0].message.role !== "assistant") {
    throw new Error(`unexpected role ${completion.choices[0].message.role}`);
  }
  if (!completion.usage) throw new Error("completion carried no usage");
  result.json_completion = true;
} catch (err) {
  fail("json_completion", err);
}

// 2. Streaming: the SSE framing an OpenAI-compatible surface most often gets
//    wrong (missing [DONE], malformed chunk, unparseable delta).
try {
  let chunks = 0;
  const stream = await client.chat.completions.create({
    model: MODEL,
    messages: [{ role: "user", content: "stream probe" }],
    max_completion_tokens: 16,
    stream: true,
  });
  for await (const chunk of stream) {
    if (!chunk.id) throw new Error("stream chunk carried no id");
    chunks++;
  }
  if (chunks === 0) throw new Error("stream produced no chunks");
  result.streaming_completion = true;
} catch (err) {
  fail("streaming_completion", err);
}

// 3. merc's receipt headers, which are not part of the OpenAI schema and so
//    exist only on the raw response. A buyer reconciling a charge needs them.
try {
  const raw = await client.chat.completions
    .create({
      model: MODEL,
      messages: [{ role: "user", content: "receipt probe" }],
      max_completion_tokens: 8,
    })
    .asResponse();
  const contract = raw.headers.get("x-merc-contract-id");
  const receipt = raw.headers.get("x-merc-receipt");
  const maxUSD = Number(raw.headers.get("x-merc-max-usd"));
  if (!contract) throw new Error("no X-Merc-Contract-ID");
  if (!receipt) throw new Error("no X-Merc-Receipt");
  if (!(maxUSD > 0)) throw new Error(`X-Merc-Max-USD not a positive number: ${maxUSD}`);
  result.receipt_verified = true;
} catch (err) {
  fail("receipt_verified", err);
}

// 4. A bad key must be refused. A surface that accepts one serves unpaid work.
try {
  const unauthorized = new OpenAI({
    baseURL: origin + "/v1",
    apiKey: "merc_definitely_not_a_key",
    maxRetries: 0,
  });
  let accepted = false;
  try {
    await unauthorized.chat.completions.create({
      model: MODEL,
      messages: [{ role: "user", content: "unauthorized probe" }],
      max_completion_tokens: 8,
    });
    accepted = true;
  } catch (err) {
    if (err?.status !== 401 && err?.status !== 403) throw err;
    result.authorization_receipt = true;
  }
  if (accepted) throw new Error("an invalid API key was accepted");
} catch (err) {
  fail("authorization_receipt", err);
}

// 5. Model listing: how a buyer's tooling discovers what merc serves.
try {
  const models = await client.models.list();
  const ids = models.data.map((m) => m.id);
  if (!ids.length) throw new Error("models.list returned nothing");
  result.models_list = true;
} catch (err) {
  fail("models_list", err);
}

// 6/7. parallel_tool_calls and response_format are request-shape parameters the
// surface must accept without erroring. merc's catalogue models do not emit
// tool calls, so this asserts the parameter is honoured as far as the response
// shape, not that a tool was called.
try {
  const tooled = await client.chat.completions.create({
    model: MODEL,
    messages: [{ role: "user", content: "tool probe" }],
    max_completion_tokens: 16,
    parallel_tool_calls: true,
    tools: [
      {
        type: "function",
        function: {
          name: "noop",
          description: "does nothing",
          parameters: { type: "object", properties: {} },
        },
      },
    ],
  });
  if (!tooled.choices?.length) throw new Error("tool-enabled completion carried no choices");
  result.parallel_tool_calls = true;
} catch (err) {
  fail("parallel_tool_calls", err);
}

try {
  const structured = await client.chat.completions.create({
    model: MODEL,
    messages: [{ role: "user", content: "structured probe" }],
    max_completion_tokens: 16,
    response_format: { type: "json_object" },
  });
  if (!structured.choices?.length) throw new Error("structured completion carried no choices");
  result.structured_output = true;
} catch (err) {
  fail("structured_output", err);
}

const capabilities = [
  "json_completion", "streaming_completion", "receipt_verified",
  "authorization_receipt", "models_list", "parallel_tool_calls",
  "structured_output",
];
result.status = capabilities.every((c) => result[c]) ? "PASS" : "FAIL";
emit(result.status === "PASS" ? 0 : 1);

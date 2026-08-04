//! Minimal OpenAI-compatible HTTP surface over the in-process Candle engine.
//!
//! Exists so merc-serving-matrix-v1 can enter Candle as a same-digest arm
//! against llama.cpp. Speaks only the public OpenAI wire shape used by the
//! harness (`/v1/models`, `/v1/chat/completions` with SSE stream). No engine
//! source is vendored; this is a thin adapter over [`crate::executor::LlamaBackend`].
//!
//! Concurrency: one Metal model lock. Concurrent requests queue. That is an
//! honest engine property (Candle serial decode), not a harness defect — the
//! tournament measures it against llama-server's parallel slots.

use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use serde::Deserialize;
use serde_json::{json, Value};

use crate::executor::{LlamaBackend, RunError};
use crate::models;

/// Pinned GGUF digest this server is willing to advertise. The load path
/// already verifies artifact hashes via `models::fetch`; this constant is the
/// value the tournament harness compares across arms.
pub const PINNED_GGUF_SHA256: &str =
    "3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1";

pub struct ServeConfig {
    pub bind: String,
    pub model_id: String,
    pub model_ref: String,
}

struct State {
    backend: Mutex<LlamaBackend>,
    model_id: String,
    model_digest: String,
    ready: AtomicBool,
    inflight: AtomicU64,
    completed: AtomicU64,
    errors: AtomicU64,
}

/// Load the pinned GGUF into Candle and serve OpenAI-compatible HTTP until
/// SIGINT / process kill. Prints a single ready line on stdout so the harness
/// can wait without scraping Metal init noise on stderr.
pub fn run(cfg: ServeConfig) -> Result<(), anyhow::Error> {
    let model_ref = if cfg.model_ref.is_empty() {
        cfg.model_id.clone()
    } else {
        cfg.model_ref.clone()
    };
    eprintln!(
        "candle-openai-serve: loading model_ref={model_ref} on {:?}",
        models::device()
    );
    let load_start = Instant::now();
    let backend = LlamaBackend::load(&model_ref).map_err(|e| anyhow::anyhow!("{e}"))?;
    eprintln!(
        "candle-openai-serve: model loaded in {:.2}s digest={PINNED_GGUF_SHA256}",
        load_start.elapsed().as_secs_f64()
    );

    let state = Arc::new(State {
        backend: Mutex::new(backend),
        model_id: cfg.model_id.clone(),
        model_digest: PINNED_GGUF_SHA256.to_string(),
        ready: AtomicBool::new(true),
        inflight: AtomicU64::new(0),
        completed: AtomicU64::new(0),
        errors: AtomicU64::new(0),
    });

    let listener = TcpListener::bind(&cfg.bind)?;
    let local = listener.local_addr()?;
    // Machine-readable ready line for the Go harness wait loop.
    println!(
        "CANDLE_OPENAI_READY base_url=http://{local}/v1 model={} digest={}",
        state.model_id, state.model_digest
    );
    let _ = std::io::stdout().flush();

    for conn in listener.incoming() {
        match conn {
            Ok(stream) => {
                let st = Arc::clone(&state);
                thread::spawn(move || {
                    if let Err(e) = handle_connection(stream, st) {
                        eprintln!("candle-openai-serve: connection error: {e}");
                    }
                });
            }
            Err(e) => {
                eprintln!("candle-openai-serve: accept error: {e}");
                thread::sleep(Duration::from_millis(20));
            }
        }
    }
    Ok(())
}

fn handle_connection(mut stream: TcpStream, state: Arc<State>) -> Result<(), anyhow::Error> {
    stream.set_read_timeout(Some(Duration::from_secs(120)))?;
    stream.set_write_timeout(Some(Duration::from_secs(300)))?;

    let mut reader = BufReader::new(stream.try_clone()?);
    let mut request_line = String::new();
    reader.read_line(&mut request_line)?;
    let request_line = request_line.trim_end();
    if request_line.is_empty() {
        return Ok(());
    }
    let mut parts = request_line.split_whitespace();
    let method = parts.next().unwrap_or("").to_uppercase();
    let path = parts.next().unwrap_or("").to_string();

    let mut content_length: usize = 0;
    loop {
        let mut line = String::new();
        reader.read_line(&mut line)?;
        let t = line.trim_end();
        if t.is_empty() {
            break;
        }
        if let Some(v) = t
            .strip_prefix("Content-Length:")
            .or_else(|| t.strip_prefix("content-length:"))
        {
            content_length = v.trim().parse().unwrap_or(0);
        }
    }

    let mut body = vec![0u8; content_length];
    if content_length > 0 {
        reader.read_exact(&mut body)?;
    }

    // Normalize path: accept /v1/... and bare /...
    let path = path.split('?').next().unwrap_or(&path);
    let path = if path.starts_with("/v1/") {
        path.to_string()
    } else if path == "/v1" {
        "/v1".to_string()
    } else if path.starts_with('/') {
        format!("/v1{path}")
    } else {
        path.to_string()
    };

    match (method.as_str(), path.as_str()) {
        ("GET", "/v1/models") | ("GET", "/v1/models/") => {
            write_json(
                &mut stream,
                200,
                &json!({
                    "object": "list",
                    "data": [{
                        "id": state.model_id,
                        "object": "model",
                        "created": unix_now(),
                        "owned_by": "merc-candle",
                        "merc_model_digest": state.model_digest,
                        "merc_engine": "candle",
                        "merc_transport": "openai_http_shim",
                    }]
                }),
            )?;
        }
        ("GET", "/health") | ("GET", "/v1/health") => {
            write_json(
                &mut stream,
                200,
                &json!({
                    "status": if state.ready.load(Ordering::Relaxed) { "ok" } else { "loading" },
                    "engine": "candle",
                    "model": state.model_id,
                    "model_digest": state.model_digest,
                    "inflight": state.inflight.load(Ordering::Relaxed),
                    "completed": state.completed.load(Ordering::Relaxed),
                    "errors": state.errors.load(Ordering::Relaxed),
                }),
            )?;
        }
        ("POST", "/v1/chat/completions") => {
            handle_chat_completions(&mut stream, &state, &body)?;
        }
        _ => {
            write_json(
                &mut stream,
                404,
                &json!({
                    "error": {
                        "message": format!("unknown route {method} {path}"),
                        "type": "invalid_request_error",
                        "code": "not_found"
                    }
                }),
            )?;
        }
    }
    Ok(())
}

#[derive(Debug, Deserialize)]
struct ChatRequest {
    #[serde(default)]
    model: String,
    #[serde(default)]
    messages: Vec<ChatMessage>,
    #[serde(default = "default_max_tokens")]
    max_tokens: u32,
    #[serde(default)]
    stream: bool,
    #[serde(default)]
    temperature: Option<f32>,
}

fn default_max_tokens() -> u32 {
    16
}

#[derive(Debug, Deserialize)]
struct ChatMessage {
    #[serde(default)]
    role: String,
    #[serde(default)]
    content: Value,
}

fn message_text(m: &ChatMessage) -> String {
    match &m.content {
        Value::String(s) => s.clone(),
        Value::Array(parts) => {
            let mut out = String::new();
            for p in parts {
                if let Some(t) = p.get("text").and_then(|v| v.as_str()) {
                    out.push_str(t);
                } else if let Some(t) = p.as_str() {
                    out.push_str(t);
                }
            }
            out
        }
        other => other.to_string(),
    }
}

fn handle_chat_completions(
    stream: &mut TcpStream,
    state: &State,
    body: &[u8],
) -> Result<(), anyhow::Error> {
    let req: ChatRequest = serde_json::from_slice(body).unwrap_or(ChatRequest {
        model: state.model_id.clone(),
        messages: vec![],
        max_tokens: 16,
        stream: false,
        temperature: Some(0.0),
    });

    if req.temperature.unwrap_or(0.0) != 0.0 {
        write_json(
            stream,
            400,
            &json!({
                "error": {
                    "message": "candle openai shim is greedy-only (temperature must be 0)",
                    "type": "invalid_request_error",
                    "code": "temperature_rejected"
                }
            }),
        )?;
        return Ok(());
    }

    // Flatten chat messages into a single user-ish prompt. The Candle path wraps
    // with the Llama-3 chat template internally; feeding the last user content
    // (or a joined transcript) matches the serving-matrix harness which sends
    // one user message.
    let prompt = if req.messages.is_empty() {
        String::new()
    } else if let Some(last_user) = req
        .messages
        .iter()
        .rev()
        .find(|m| m.role.eq_ignore_ascii_case("user"))
    {
        message_text(last_user)
    } else {
        req.messages
            .iter()
            .map(message_text)
            .collect::<Vec<_>>()
            .join("\n")
    };

    let max_tokens = req.max_tokens.clamp(1, 2048);
    let model_id = if req.model.is_empty() {
        state.model_id.clone()
    } else {
        req.model.clone()
    };
    let completion_id = format!("chatcmpl-candle-{}", unix_now());

    state.inflight.fetch_add(1, Ordering::Relaxed);

    if req.stream {
        let result = stream_completion(
            stream,
            state,
            &model_id,
            &completion_id,
            &prompt,
            max_tokens,
        );
        state.inflight.fetch_sub(1, Ordering::Relaxed);
        match result {
            Ok(()) => {
                state.completed.fetch_add(1, Ordering::Relaxed);
                Ok(())
            }
            Err(e) => {
                state.errors.fetch_add(1, Ordering::Relaxed);
                // Headers may already be sent; best-effort log.
                eprintln!("candle-openai-serve: stream error: {e}");
                Err(e)
            }
        }
    } else {
        let result = nonstream_completion(
            stream,
            state,
            &model_id,
            &completion_id,
            &prompt,
            max_tokens,
        );
        state.inflight.fetch_sub(1, Ordering::Relaxed);
        match result {
            Ok(()) => {
                state.completed.fetch_add(1, Ordering::Relaxed);
                Ok(())
            }
            Err(e) => {
                state.errors.fetch_add(1, Ordering::Relaxed);
                Err(e)
            }
        }
    }
}

fn stream_completion(
    stream: &mut TcpStream,
    state: &State,
    model_id: &str,
    completion_id: &str,
    prompt: &str,
    max_tokens: u32,
) -> Result<(), anyhow::Error> {
    // SSE headers first so TTFT includes first-token generation only, not header setup.
    write!(
        stream,
        "HTTP/1.1 200 OK\r\n\
         Content-Type: text/event-stream\r\n\
         Cache-Control: no-cache\r\n\
         Connection: close\r\n\
         \r\n"
    )?;
    stream.flush()?;

    let created = unix_now();
    let mut token_count: usize = 0;

    // Role chunk (OpenAI clients often expect an initial role delta).
    let role_chunk = json!({
        "id": completion_id,
        "object": "chat.completion.chunk",
        "created": created,
        "model": model_id,
        "choices": [{
            "index": 0,
            "delta": { "role": "assistant" },
            "finish_reason": null
        }]
    });
    write!(stream, "data: {role_chunk}\n\n")?;
    stream.flush()?;

    let gen_result = {
        let mut backend = state
            .backend
            .lock()
            .map_err(|_| anyhow::anyhow!("model mutex poisoned"))?;
        backend.generate_greedy_streaming(prompt, max_tokens, |piece| {
            token_count += 1;
            let chunk = json!({
                "id": completion_id,
                "object": "chat.completion.chunk",
                "created": created,
                "model": model_id,
                "choices": [{
                    "index": 0,
                    "delta": { "content": piece },
                    "finish_reason": null
                }]
            });
            write!(stream, "data: {chunk}\n\n").map_err(|e| RunError::Inference {
                backend: "candle_openai",
                msg: format!("sse write failed: {e}"),
            })?;
            stream.flush().map_err(|e| RunError::Inference {
                backend: "candle_openai",
                msg: format!("sse flush failed: {e}"),
            })?;
            Ok(())
        })
    };

    match gen_result {
        Ok((_text, n)) => {
            // Prefer the model-reported count when streaming pieces collapsed.
            if n > token_count {
                token_count = n;
            }
            let finish = json!({
                "id": completion_id,
                "object": "chat.completion.chunk",
                "created": created,
                "model": model_id,
                "choices": [{
                    "index": 0,
                    "delta": {},
                    "finish_reason": "stop"
                }],
                "usage": {
                    "prompt_tokens": estimate_tokens(prompt),
                    "completion_tokens": token_count,
                    "total_tokens": estimate_tokens(prompt) + token_count
                }
            });
            write!(stream, "data: {finish}\n\n")?;
            write!(stream, "data: [DONE]\n\n")?;
            stream.flush()?;
            Ok(())
        }
        Err(e) => {
            let err_chunk = json!({
                "error": {
                    "message": e.to_string(),
                    "type": "server_error",
                    "code": "candle_generate_failed"
                }
            });
            write!(stream, "data: {err_chunk}\n\n")?;
            write!(stream, "data: [DONE]\n\n")?;
            stream.flush()?;
            Err(anyhow::anyhow!("{e}"))
        }
    }
}

fn nonstream_completion(
    stream: &mut TcpStream,
    state: &State,
    model_id: &str,
    completion_id: &str,
    prompt: &str,
    max_tokens: u32,
) -> Result<(), anyhow::Error> {
    let (text, n) = {
        let mut backend = state
            .backend
            .lock()
            .map_err(|_| anyhow::anyhow!("model mutex poisoned"))?;
        backend
            .generate(prompt, max_tokens)
            .map_err(|e| anyhow::anyhow!("{e}"))?
    };
    let body = json!({
        "id": completion_id,
        "object": "chat.completion",
        "created": unix_now(),
        "model": model_id,
        "choices": [{
            "index": 0,
            "message": { "role": "assistant", "content": text },
            "finish_reason": "stop"
        }],
        "usage": {
            "prompt_tokens": estimate_tokens(prompt),
            "completion_tokens": n,
            "total_tokens": estimate_tokens(prompt) + n
        }
    });
    write_json(stream, 200, &body)
}

fn estimate_tokens(s: &str) -> usize {
    // Same rough 4-char heuristic the serving-matrix corpus uses. Real token
    // counts come from the model for completion_tokens; prompt_tokens is
    // approximate for rate metrics only.
    (s.chars().count() / 4).max(1)
}

fn write_json(stream: &mut TcpStream, status: u16, body: &Value) -> Result<(), anyhow::Error> {
    let payload = serde_json::to_vec(body)?;
    let reason = match status {
        200 => "OK",
        400 => "Bad Request",
        404 => "Not Found",
        500 => "Internal Server Error",
        _ => "OK",
    };
    write!(
        stream,
        "HTTP/1.1 {status} {reason}\r\n\
         Content-Type: application/json\r\n\
         Content-Length: {}\r\n\
         Connection: close\r\n\
         \r\n",
        payload.len()
    )?;
    stream.write_all(&payload)?;
    stream.flush()?;
    Ok(())
}

fn unix_now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

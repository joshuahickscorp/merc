//! Device-bound worker enrollment client.
//!
//! Wire protocol is owned by `control/enrollment.go` (v2, audience
//! `cx-macos-agent-v2`). A stranger device:
//!
//! 1. generates a P-256 key and prints a `cxer2_…` device request
//! 2. has the supplier owner approve it (console → enrollment bundle `cxeb2_…`)
//! 3. signs the exchange transcript and POSTs `/v1/worker/enrollment/exchange`
//! 4. writes `worker_token` + `supplier_id` into the local agent config
//!
//! The private key never leaves the device directory under `~/.merc/enrollment/`.
//! Crypto uses `ring` (already a rustls dependency) so the agent does not grow a
//! second elliptic-curve stack for this path.

use std::fs;
use std::path::{Path, PathBuf};

use anyhow::{bail, Context, Result};
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use ring::rand::SystemRandom;
use ring::signature::{EcdsaKeyPair, KeyPair, ECDSA_P256_SHA256_ASN1_SIGNING};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use uuid::Uuid;

use crate::config::{agent_home_dir, AGENT_HOME_DIRNAME};

pub const ENROLLMENT_PROTOCOL_VERSION: i32 = 2;
pub const ENROLLMENT_AUDIENCE: &str = "cx-macos-agent-v2";
pub const ENROLLMENT_KEY_ALGORITHM: &str = "p256";
pub const DEVICE_REQUEST_PREFIX: &str = "cxer2_";
pub const APPROVAL_BUNDLE_PREFIX: &str = "cxeb2_";
const EXCHANGE_TRANSCRIPT_HEADER: &str = "cx-worker-enrollment-exchange-v2";

const KEY_FILE: &str = "device.p256.pkcs8";
const REQUEST_META_FILE: &str = "pending_request.json";

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DeviceRequestPayload {
    pub v: i32,
    pub control_origin: String,
    pub audience: String,
    pub request_id: String,
    pub device_key_algorithm: String,
    pub device_public_key: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ApprovalBundlePayload {
    pub v: i32,
    pub control_origin: String,
    pub account_id: Uuid,
    pub audience: String,
    pub enrollment_code: String,
    pub request_id: String,
    pub device_fingerprint: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct PendingRequestMeta {
    control_origin: String,
    request_id: String,
    device_public_key: String,
    device_request: String,
    created_unix: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EnrollmentExchangeInput {
    pub v: i32,
    pub enrollment_code: String,
    pub control_origin: String,
    pub request_id: String,
    pub audience: String,
    pub account_id: Uuid,
    pub device_key_algorithm: String,
    pub device_public_key: String,
    pub proof: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EnrollmentExchangeResult {
    pub credential_id: Uuid,
    pub worker_id: Uuid,
    pub supplier_id: Uuid,
    pub worker_token: String,
    pub device_fingerprint: String,
    pub credential_version: i32,
    #[serde(default)]
    pub rotated: bool,
}

pub fn enrollment_dir() -> PathBuf {
    agent_home_dir().join("enrollment")
}

#[allow(dead_code)]
pub fn enrollment_dir_for(home: &Path) -> PathBuf {
    home.join(AGENT_HOME_DIRNAME).join("enrollment")
}

/// Canonical origin rules match `canonicalEnrollmentControlOrigin` on the control plane.
pub fn canonicalize_control_origin(raw: &str, allow_insecure_loopback: bool) -> Result<String> {
    let raw = raw.trim();
    if raw.is_empty() {
        bail!("control_origin must not contain outer whitespace");
    }
    if raw.contains(|c: char| c.is_whitespace()) {
        bail!("control_origin must not contain outer whitespace");
    }
    if raw.contains('?') || raw.contains('#') {
        bail!("control_origin must not contain a query or fragment");
    }
    let (scheme, rest) = raw
        .split_once("://")
        .ok_or_else(|| anyhow::anyhow!("control_origin must be an absolute origin"))?;
    let scheme = scheme.to_ascii_lowercase();
    if rest.contains('@') {
        bail!("control_origin must be an absolute origin");
    }
    let hostport = if let Some((hp, path)) = rest.split_once('/') {
        if !path.is_empty() {
            bail!("control_origin must not contain a path");
        }
        hp
    } else {
        rest
    };
    if hostport.is_empty() {
        bail!("control_origin has an invalid host");
    }
    let host = if let Some(stripped) = hostport.strip_prefix('[') {
        let end = stripped
            .find(']')
            .ok_or_else(|| anyhow::anyhow!("control_origin has an invalid host"))?;
        stripped[..end].to_ascii_lowercase()
    } else {
        hostport
            .split(':')
            .next()
            .unwrap_or(hostport)
            .to_ascii_lowercase()
    };
    let loopback = host == "localhost"
        || host == "127.0.0.1"
        || host == "::1"
        || host.starts_with("127.");
    if scheme != "https" && !(allow_insecure_loopback && scheme == "http" && loopback) {
        bail!("control_origin must use HTTPS (http loopback allowed for local test)");
    }
    Ok(format!("{scheme}://{hostport}"))
}

fn request_id_for_public_key(public_key_uncompressed: &[u8]) -> String {
    let mut material = Vec::with_capacity(24 + public_key_uncompressed.len());
    material.extend_from_slice(b"cx-enrollment-request-v1\x00");
    material.extend_from_slice(public_key_uncompressed);
    let digest = Sha256::digest(&material);
    URL_SAFE_NO_PAD.encode(&digest[..16])
}

fn exchange_transcript(
    code: &str,
    audience: &str,
    account_id: Uuid,
    control_origin: &str,
    request_id: &str,
) -> Vec<u8> {
    format!(
        "{EXCHANGE_TRANSCRIPT_HEADER}\n{audience}\n{account_id}\n{control_origin}\n{request_id}\n{code}"
    )
    .into_bytes()
}

fn ensure_dir(path: &Path) -> Result<()> {
    fs::create_dir_all(path).with_context(|| format!("create {}", path.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = fs::set_permissions(path, fs::Permissions::from_mode(0o700));
    }
    Ok(())
}

fn write_secret_file(path: &Path, bytes: &[u8]) -> Result<()> {
    if let Some(parent) = path.parent() {
        ensure_dir(parent)?;
    }
    fs::write(path, bytes).with_context(|| format!("write {}", path.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = fs::set_permissions(path, fs::Permissions::from_mode(0o600));
    }
    Ok(())
}

fn load_or_create_key_pair(dir: &Path) -> Result<EcdsaKeyPair> {
    ensure_dir(dir)?;
    let key_path = dir.join(KEY_FILE);
    let rng = SystemRandom::new();
    if key_path.is_file() {
        let pkcs8 = fs::read(&key_path).with_context(|| format!("read {}", key_path.display()))?;
        return EcdsaKeyPair::from_pkcs8(&ECDSA_P256_SHA256_ASN1_SIGNING, &pkcs8, &rng)
            .map_err(|_| anyhow::anyhow!("decode device enrollment private key (PKCS#8)"));
    }
    let doc = EcdsaKeyPair::generate_pkcs8(&ECDSA_P256_SHA256_ASN1_SIGNING, &rng)
        .map_err(|_| anyhow::anyhow!("generate device enrollment key"))?;
    write_secret_file(&key_path, doc.as_ref())?;
    EcdsaKeyPair::from_pkcs8(&ECDSA_P256_SHA256_ASN1_SIGNING, doc.as_ref(), &rng)
        .map_err(|_| anyhow::anyhow!("load freshly generated device enrollment key"))
}

fn public_key_b64(key_pair: &EcdsaKeyPair) -> String {
    // Uncompressed SEC1 point (0x04 || X || Y), 65 bytes for P-256 — matches
    // control/enrollment.go parseP256EnrollmentPublicKey.
    URL_SAFE_NO_PAD.encode(key_pair.public_key().as_ref())
}

/// Build a device request for the supplier owner to approve.
pub fn create_device_request(
    control_origin: &str,
    state_dir: &Path,
) -> Result<(String, DeviceRequestPayload)> {
    let allow_loopback = control_origin.starts_with("http://");
    let origin = canonicalize_control_origin(control_origin, allow_loopback)?;
    let key_pair = load_or_create_key_pair(state_dir)?;
    let device_public_key = public_key_b64(&key_pair);
    let pub_raw = URL_SAFE_NO_PAD
        .decode(&device_public_key)
        .context("decode public key for request_id")?;
    if pub_raw.len() != 65 || pub_raw[0] != 0x04 {
        bail!("device public key is not an uncompressed P-256 point");
    }
    let request_id = request_id_for_public_key(&pub_raw);
    let payload = DeviceRequestPayload {
        v: ENROLLMENT_PROTOCOL_VERSION,
        control_origin: origin.clone(),
        audience: ENROLLMENT_AUDIENCE.to_string(),
        request_id: request_id.clone(),
        device_key_algorithm: ENROLLMENT_KEY_ALGORITHM.to_string(),
        device_public_key: device_public_key.clone(),
    };
    let json = serde_json::to_vec(&payload).context("encode device request")?;
    let encoded = format!("{DEVICE_REQUEST_PREFIX}{}", URL_SAFE_NO_PAD.encode(json));
    let meta = PendingRequestMeta {
        control_origin: origin,
        request_id,
        device_public_key,
        device_request: encoded.clone(),
        created_unix: std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs())
            .unwrap_or(0),
    };
    write_secret_file(
        &state_dir.join(REQUEST_META_FILE),
        serde_json::to_string_pretty(&meta)?.as_bytes(),
    )?;
    Ok((encoded, payload))
}

pub fn decode_approval_bundle(encoded: &str) -> Result<ApprovalBundlePayload> {
    let value = encoded.trim();
    if !value.starts_with(APPROVAL_BUNDLE_PREFIX) {
        bail!("enrollment bundle must start with {APPROVAL_BUNDLE_PREFIX}");
    }
    let payload_text = &value[APPROVAL_BUNDLE_PREFIX.len()..];
    let raw = URL_SAFE_NO_PAD
        .decode(payload_text)
        .context("decode enrollment bundle payload")?;
    let bundle: ApprovalBundlePayload =
        serde_json::from_slice(&raw).context("parse enrollment bundle JSON")?;
    if bundle.v != ENROLLMENT_PROTOCOL_VERSION {
        bail!(
            "enrollment bundle protocol version {}, want {ENROLLMENT_PROTOCOL_VERSION}",
            bundle.v
        );
    }
    if bundle.audience != ENROLLMENT_AUDIENCE {
        bail!("enrollment bundle audience mismatch");
    }
    Ok(bundle)
}

fn sign_transcript(key_pair: &EcdsaKeyPair, transcript: &[u8]) -> Result<String> {
    // ring ECDSA_P256_SHA256_ASN1 hashes the message with SHA-256 then signs —
    // matching Go's ecdsa.SignASN1(priv, sha256(transcript)).
    let rng = SystemRandom::new();
    let sig = key_pair
        .sign(&rng, transcript)
        .map_err(|_| anyhow::anyhow!("sign enrollment proof"))?;
    Ok(URL_SAFE_NO_PAD.encode(sig.as_ref()))
}

/// Build the signed exchange body from a local pending request + approval bundle.
pub fn build_exchange_input(
    state_dir: &Path,
    bundle_encoded: &str,
) -> Result<EnrollmentExchangeInput> {
    let bundle = decode_approval_bundle(bundle_encoded)?;
    let meta_path = state_dir.join(REQUEST_META_FILE);
    let meta: PendingRequestMeta = serde_json::from_str(
        &fs::read_to_string(&meta_path).with_context(|| {
            format!(
                "no pending enrollment request at {} — run `merc-agent enroll request` first",
                meta_path.display()
            )
        })?,
    )?;
    if meta.request_id != bundle.request_id {
        bail!(
            "approval request_id {} does not match pending request {}",
            bundle.request_id,
            meta.request_id
        );
    }
    if meta.control_origin != bundle.control_origin {
        bail!(
            "approval control_origin {} does not match pending {}",
            bundle.control_origin,
            meta.control_origin
        );
    }
    let key_pair = load_or_create_key_pair(state_dir)?;
    let device_public_key = public_key_b64(&key_pair);
    if device_public_key != meta.device_public_key {
        bail!("device key on disk does not match the pending request public key");
    }
    let transcript = exchange_transcript(
        &bundle.enrollment_code,
        &bundle.audience,
        bundle.account_id,
        &bundle.control_origin,
        &bundle.request_id,
    );
    let proof = sign_transcript(&key_pair, &transcript)?;
    Ok(EnrollmentExchangeInput {
        v: ENROLLMENT_PROTOCOL_VERSION,
        enrollment_code: bundle.enrollment_code,
        control_origin: bundle.control_origin,
        request_id: bundle.request_id,
        audience: bundle.audience,
        account_id: bundle.account_id,
        device_key_algorithm: ENROLLMENT_KEY_ALGORITHM.to_string(),
        device_public_key,
        proof,
    })
}

/// POST the exchange to the control plane. Returns the one-time worker token.
pub async fn exchange_enrollment(
    control_origin: &str,
    input: &EnrollmentExchangeInput,
) -> Result<EnrollmentExchangeResult> {
    let base = control_origin.trim_end_matches('/');
    let url = format!("{base}/v1/worker/enrollment/exchange");
    let http = crate::tls::client_builder()
        .map_err(|e| anyhow::anyhow!("TLS client: {e}"))?
        .timeout(std::time::Duration::from_secs(20))
        .user_agent(concat!("merc-agent/", env!("CARGO_PKG_VERSION")))
        .build()
        .context("build enrollment HTTP client")?;
    let resp = http
        .post(&url)
        .json(input)
        .send()
        .await
        .with_context(|| format!("POST {url}"))?;
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    if !status.is_success() {
        bail!("enrollment exchange HTTP {status}: {body}");
    }
    serde_json::from_str(&body)
        .with_context(|| format!("decode enrollment exchange response: {body}"))
}

/// Write or update agent.toml with the issued credential. Preserves other fields
/// when the file already exists.
pub fn write_enrolled_config(
    config_path: &Path,
    control_url: &str,
    result: &EnrollmentExchangeResult,
) -> Result<()> {
    if let Some(parent) = config_path.parent() {
        ensure_dir(parent)?;
    }
    let data_dir = config_path
        .parent()
        .map(|p| p.join("data"))
        .unwrap_or_else(|| PathBuf::from("data"));
    ensure_dir(&data_dir)?;

    if config_path.is_file() {
        let existing = fs::read_to_string(config_path)
            .with_context(|| format!("read {}", config_path.display()))?;
        let mut doc: toml::Value =
            toml::from_str(&existing).context("parse existing agent.toml")?;
        let table = doc
            .as_table_mut()
            .ok_or_else(|| anyhow::anyhow!("agent.toml root must be a table"))?;
        table.insert(
            "control_url".into(),
            toml::Value::String(control_url.to_string()),
        );
        table.insert(
            "worker_token".into(),
            toml::Value::String(result.worker_token.clone()),
        );
        table.insert(
            "supplier_id".into(),
            toml::Value::String(result.supplier_id.to_string()),
        );
        if !table.contains_key("power_only") {
            table.insert("power_only".into(), toml::Value::Boolean(true));
        }
        if !table.contains_key("min_payout_usd_per_hr") {
            table.insert("min_payout_usd_per_hr".into(), toml::Value::Float(0.05));
        }
        if !table.contains_key("data_dir") {
            table.insert(
                "data_dir".into(),
                toml::Value::String(data_dir.display().to_string()),
            );
        }
        let rendered = toml::to_string_pretty(&doc).context("serialize agent.toml")?;
        write_secret_file(config_path, rendered.as_bytes())?;
    } else {
        let body = format!(
            r#"control_url = "{control_url}"
worker_token = "{token}"
supplier_id = "{supplier}"
power_only = true
min_payout_usd_per_hr = 0.05
data_dir = "{data}"
"#,
            token = result.worker_token,
            supplier = result.supplier_id,
            data = data_dir.display(),
        );
        write_secret_file(config_path, body.as_bytes())?;
    }
    Ok(())
}

/// Paths the enroll state occupies — used by uninstall privacy checks and tests.
#[allow(dead_code)]
pub fn enrollment_state_paths(home: &Path) -> Vec<PathBuf> {
    let dir = enrollment_dir_for(home);
    vec![
        dir.join(KEY_FILE),
        dir.join(REQUEST_META_FILE),
        dir,
        home.join(AGENT_HOME_DIRNAME).join("agent.toml"),
        home.join(AGENT_HOME_DIRNAME).join("agent.prefs.toml"),
    ]
}

#[cfg(test)]
mod tests {
    use super::*;
    use ring::signature::{UnparsedPublicKey, ECDSA_P256_SHA256_ASN1};

    #[test]
    fn device_request_round_trips_and_request_id_binds_key() {
        let tmp = tempfile_dir();
        let origin = "https://control.example.test";
        let (encoded, payload) = create_device_request(origin, &tmp).unwrap();
        assert!(encoded.starts_with(DEVICE_REQUEST_PREFIX));
        assert_eq!(payload.audience, ENROLLMENT_AUDIENCE);
        assert_eq!(payload.control_origin, origin);
        let raw = URL_SAFE_NO_PAD
            .decode(encoded.trim_start_matches(DEVICE_REQUEST_PREFIX))
            .unwrap();
        let again: DeviceRequestPayload = serde_json::from_slice(&raw).unwrap();
        assert_eq!(again.request_id, payload.request_id);
        let pub_raw = URL_SAFE_NO_PAD.decode(&payload.device_public_key).unwrap();
        assert_eq!(pub_raw.len(), 65);
        assert_eq!(pub_raw[0], 0x04);
        assert_eq!(request_id_for_public_key(&pub_raw), payload.request_id);
        assert!(tmp.join(KEY_FILE).is_file());
        let (_, payload2) = create_device_request(origin, &tmp).unwrap();
        assert_eq!(
            payload.device_public_key, payload2.device_public_key,
            "second request must reuse the same device key"
        );
    }

    #[test]
    fn proof_verifies_against_control_plane_transcript_shape() {
        let tmp = tempfile_dir();
        let origin = "https://control.example.test";
        let (_, payload) = create_device_request(origin, &tmp).unwrap();
        let account = Uuid::parse_str("12345678-1234-4abc-8def-1234567890ab").unwrap();
        let code = "cxe_WlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlo";
        let transcript = exchange_transcript(
            code,
            ENROLLMENT_AUDIENCE,
            account,
            origin,
            &payload.request_id,
        );
        let key_pair = load_or_create_key_pair(&tmp).unwrap();
        let proof_b64 = sign_transcript(&key_pair, &transcript).unwrap();
        let sig = URL_SAFE_NO_PAD.decode(&proof_b64).unwrap();
        let pub_raw = URL_SAFE_NO_PAD.decode(&payload.device_public_key).unwrap();
        let unparsed = UnparsedPublicKey::new(&ECDSA_P256_SHA256_ASN1, &pub_raw);
        unparsed
            .verify(&transcript, &sig)
            .expect("proof must verify under ECDSA_P256_SHA256_ASN1");
        // Altered transcript must fail (binds code/audience/account/origin/request).
        let mut wrong = transcript.clone();
        wrong.push(b'x');
        assert!(unparsed.verify(&wrong, &sig).is_err());
    }

    #[test]
    fn approval_bundle_mismatch_is_refused() {
        let tmp = tempfile_dir();
        let origin = "https://control.example.test";
        create_device_request(origin, &tmp).unwrap();
        let foreign = ApprovalBundlePayload {
            v: 2,
            control_origin: origin.to_string(),
            account_id: Uuid::new_v4(),
            audience: ENROLLMENT_AUDIENCE.to_string(),
            enrollment_code: "cxe_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa".into(),
            request_id: "not-the-pending-request-id!!!!".into(),
            device_fingerprint: "p256:sha256:dead".into(),
        };
        let enc = format!(
            "{APPROVAL_BUNDLE_PREFIX}{}",
            URL_SAFE_NO_PAD.encode(serde_json::to_vec(&foreign).unwrap())
        );
        let err = build_exchange_input(&tmp, &enc).unwrap_err().to_string();
        assert!(
            err.contains("request_id"),
            "expected request_id mismatch, got {err}"
        );
    }

    #[test]
    fn write_enrolled_config_sets_token_and_preserves_prefs() {
        let tmp = tempfile_dir();
        let cfg = tmp.join("agent.toml");
        fs::write(
            &cfg,
            r#"control_url = "http://old"
worker_token = "old"
supplier_id = "00000000-0000-0000-0000-000000000000"
power_only = false
min_payout_usd_per_hr = 1.5
data_dir = "/tmp/x"
"#,
        )
        .unwrap();
        let result = EnrollmentExchangeResult {
            credential_id: Uuid::new_v4(),
            worker_id: Uuid::new_v4(),
            supplier_id: Uuid::new_v4(),
            worker_token: "wt_secret".into(),
            device_fingerprint: "fp".into(),
            credential_version: 1,
            rotated: false,
        };
        write_enrolled_config(&cfg, "https://control.example.test", &result).unwrap();
        let body = fs::read_to_string(&cfg).unwrap();
        assert!(body.contains("wt_secret"));
        assert!(body.contains(&result.supplier_id.to_string()));
        assert!(body.contains("1.5"));
        assert!(body.contains("false"));
    }

    fn tempfile_dir() -> PathBuf {
        let dir = std::env::temp_dir().join(format!("merc-enroll-{}", Uuid::new_v4()));
        fs::create_dir_all(&dir).unwrap();
        dir
    }
}

//! Direct link measurements for a *candidate* local Merc Fabric site.
//!
//! A measured TCP path is necessary evidence for a local cluster, but it is
//! deliberately insufficient to schedule one. The result records raw link
//! facts and explicitly remains unqualified until control-plane worker
//! identity, site ownership, mutually authenticated data transport, collective
//! benchmarks, and economics are all bound to the same candidate topology.

use std::fs::{self, OpenOptions};
use std::io::Write;
use std::net::SocketAddr;
use std::path::Path;
use std::time::{Instant, SystemTime, UNIX_EPOCH};

use anyhow::{bail, Context, Result};
use hmac::{Hmac, Mac};
use rand::RngCore;
use serde::Serialize;
use sha2::{Digest, Sha256};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::time::{timeout, Duration};

const MAGIC: &[u8; 9] = b"MERC-FAB1";
const NONCE_BYTES: usize = 32;
const MAC_BYTES: usize = 32;
const MAX_PAYLOAD_BYTES: usize = 4 * 1024 * 1024;
const MAX_ROUNDS: u16 = 32;
const IO_TIMEOUT: Duration = Duration::from_secs(15);

pub const DEFAULT_PAYLOAD_BYTES: usize = 256 * 1024;
pub const DEFAULT_ROUNDS: u16 = 12;

type HmacSha256 = Hmac<Sha256>;

#[derive(Debug, Clone)]
pub struct ProbeOptions {
    pub endpoint: SocketAddr,
    pub site: String,
    pub payload_bytes: usize,
    pub rounds: u16,
    pub shared_secret: Vec<u8>,
}

#[derive(Debug, Serialize)]
pub struct FabricProbeRound {
    pub round: u16,
    pub payload_bytes_each_direction: usize,
    pub round_trip_payload_bytes: usize,
    pub round_trip_micros: u64,
    pub payload_goodput_mbps: f64,
}

#[derive(Debug, Serialize)]
pub struct FabricProbeReceipt {
    pub schema_version: u32,
    pub kind: &'static str,
    pub status: &'static str,
    pub measured_at_unix_ms: u128,
    // This is a supplier/operator declaration. The receipt never infers
    // physical location from an address and does not disclose the endpoint.
    pub declared_site: String,
    pub peer_endpoint_sha256: String,
    pub transport: &'static str,
    pub peer_authentication: &'static str,
    pub payload_is_random: bool,
    pub rounds: Vec<FabricProbeRound>,
    pub p50_round_trip_micros: u64,
    pub p95_round_trip_micros: u64,
    pub p50_payload_goodput_mbps: f64,
    pub local_cluster_admissible: bool,
    pub non_admission_reasons: Vec<&'static str>,
}

pub fn read_shared_secret(path: &Path) -> Result<Vec<u8>> {
    let metadata = fs::metadata(path)
        .with_context(|| format!("reading fabric shared-secret metadata {}", path.display()))?;
    if !metadata.is_file() {
        bail!("fabric shared-secret path is not a regular file")
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if metadata.permissions().mode() & 0o077 != 0 {
            bail!("fabric shared-secret file must not be group- or world-readable")
        }
    }
    let mut secret = fs::read(path)
        .with_context(|| format!("reading fabric shared-secret {}", path.display()))?;
    while matches!(secret.last(), Some(b'\n' | b'\r')) {
        secret.pop();
    }
    validate_secret(&secret)?;
    Ok(secret)
}

pub async fn serve(bind: SocketAddr, shared_secret: Vec<u8>) -> Result<()> {
    validate_secret(&shared_secret)?;
    let listener = TcpListener::bind(bind)
        .await
        .with_context(|| format!("binding fabric echo listener at {bind}"))?;
    let local = listener
        .local_addr()
        .context("reading fabric listener address")?;
    tracing::info!(%local, "fabric echo listener ready; it accepts bounded measurement traffic only");
    run_listener(listener, shared_secret).await
}

async fn run_listener(listener: TcpListener, shared_secret: Vec<u8>) -> Result<()> {
    loop {
        tokio::select! {
            signal = tokio::signal::ctrl_c() => {
                signal.context("waiting for fabric listener shutdown signal")?;
                return Ok(());
            }
            accepted = listener.accept() => {
                let (stream, remote) = accepted.context("accepting fabric probe connection")?;
                let secret = shared_secret.clone();
                tokio::spawn(async move {
                    if let Err(error) = echo_once(stream, &secret).await {
                        // Invalid probes must neither expose the shared secret nor stop
                        // the listener that a real peer is using.
                        tracing::debug!(%remote, %error, "fabric probe refused");
                    }
                });
            }
        }
    }
}

pub async fn probe(options: ProbeOptions) -> Result<FabricProbeReceipt> {
    validate_options(&options)?;
    let mut rounds = Vec::with_capacity(options.rounds as usize);
    for round in 1..=options.rounds {
        let measurement = probe_once(
            options.endpoint,
            &options.shared_secret,
            options.payload_bytes,
        )
        .await
        .with_context(|| format!("fabric probe round {round}"))?;
        rounds.push(FabricProbeRound {
            round,
            payload_bytes_each_direction: options.payload_bytes,
            round_trip_payload_bytes: options.payload_bytes * 2,
            round_trip_micros: measurement.elapsed_micros,
            payload_goodput_mbps: measurement.goodput_mbps,
        });
    }
    let mut latencies = rounds
        .iter()
        .map(|round| round.round_trip_micros)
        .collect::<Vec<_>>();
    latencies.sort_unstable();
    let mut goodputs = rounds
        .iter()
        .map(|round| round.payload_goodput_mbps)
        .collect::<Vec<_>>();
    goodputs.sort_by(f64::total_cmp);

    Ok(FabricProbeReceipt {
        schema_version: 1,
        kind: "MERC_FABRIC_TCP_ECHO_RECEIPT",
        status: "MEASURED_NOT_ADMISSIBLE",
        measured_at_unix_ms: SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis(),
        declared_site: options.site,
        peer_endpoint_sha256: sha256_hex(options.endpoint.to_string().as_bytes()),
        transport: "MERC_FABRIC_TCP_ECHO_V1",
        peer_authentication: "HMAC_SHA256_OWNER_SHARED_PROBE_TOKEN",
        payload_is_random: true,
        p50_round_trip_micros: percentile_u64(&latencies, 50),
        p95_round_trip_micros: percentile_u64(&latencies, 95),
        p50_payload_goodput_mbps: percentile_f64(&goodputs, 50),
        rounds,
        local_cluster_admissible: false,
        non_admission_reasons: vec![
            "the owner-shared probe token does not bind the peer to a control-plane worker identity",
            "the declared site is not an independently governed location or residency authority",
            "TCP echo does not provide an mTLS workload data plane or authorize customer data transfer",
            "link measurements are not a tensor/pipeline/expert collective benchmark",
            "no topology-specific cost, floor, ceiling, or positive-contribution decision is bound",
        ],
    })
}

pub fn write_new_receipt(path: &Path, receipt: &[u8]) -> Result<()> {
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(path)
        .with_context(|| format!("creating new fabric receipt {}", path.display()))?;
    file.write_all(receipt)
        .with_context(|| format!("writing fabric receipt {}", path.display()))?;
    file.write_all(b"\n")
        .with_context(|| format!("finishing fabric receipt {}", path.display()))?;
    file.sync_all()
        .with_context(|| format!("syncing fabric receipt {}", path.display()))?;
    Ok(())
}

struct RoundMeasurement {
    elapsed_micros: u64,
    goodput_mbps: f64,
}

async fn probe_once(
    endpoint: SocketAddr,
    secret: &[u8],
    payload_bytes: usize,
) -> Result<RoundMeasurement> {
    let mut payload = vec![0_u8; payload_bytes];
    rand::thread_rng().fill_bytes(&mut payload);
    let mut nonce = [0_u8; NONCE_BYTES];
    rand::thread_rng().fill_bytes(&mut nonce);
    let mac = request_mac(secret, &nonce, &payload)?;

    let start = Instant::now();
    let mut stream = timeout(IO_TIMEOUT, TcpStream::connect(endpoint))
        .await
        .context("fabric peer connection timed out")?
        .with_context(|| format!("connecting to fabric peer {endpoint}"))?;
    timeout(IO_TIMEOUT, async {
        stream.write_all(MAGIC).await?;
        stream.write_all(&nonce).await?;
        stream
            .write_all(&(payload.len() as u32).to_be_bytes())
            .await?;
        stream.write_all(&mac).await?;
        stream.write_all(&payload).await?;
        stream.flush().await?;

        let mut echoed_nonce = [0_u8; NONCE_BYTES];
        let mut length = [0_u8; 4];
        stream.read_exact(&mut echoed_nonce).await?;
        stream.read_exact(&mut length).await?;
        let echoed_len = u32::from_be_bytes(length) as usize;
        if echoed_nonce != nonce || echoed_len != payload.len() {
            bail!("fabric peer echoed a different probe identity or payload length")
        }
        let mut echoed_payload = vec![0_u8; echoed_len];
        stream.read_exact(&mut echoed_payload).await?;
        if echoed_payload != payload {
            bail!("fabric peer echoed altered probe bytes")
        }
        Ok::<(), anyhow::Error>(())
    })
    .await
    .context("fabric peer I/O timed out")??;
    let elapsed = start.elapsed();
    let elapsed_micros = elapsed.as_micros().max(1).min(u64::MAX as u128) as u64;
    let goodput_mbps = (payload_bytes as f64 * 2.0 * 8.0) / elapsed.as_secs_f64() / 1_000_000.0;
    Ok(RoundMeasurement {
        elapsed_micros,
        goodput_mbps,
    })
}

async fn echo_once(mut stream: TcpStream, secret: &[u8]) -> Result<()> {
    timeout(IO_TIMEOUT, async {
        let mut magic = [0_u8; MAGIC.len()];
        stream.read_exact(&mut magic).await?;
        if magic != *MAGIC {
            bail!("unknown fabric probe protocol")
        }
        let mut nonce = [0_u8; NONCE_BYTES];
        let mut length = [0_u8; 4];
        let mut supplied_mac = [0_u8; MAC_BYTES];
        stream.read_exact(&mut nonce).await?;
        stream.read_exact(&mut length).await?;
        stream.read_exact(&mut supplied_mac).await?;
        let payload_len = u32::from_be_bytes(length) as usize;
        if payload_len == 0 || payload_len > MAX_PAYLOAD_BYTES {
            bail!("fabric probe payload is outside the bounded range")
        }
        let mut payload = vec![0_u8; payload_len];
        stream.read_exact(&mut payload).await?;
        let expected = request_mac(secret, &nonce, &payload)?;
        if !constant_time_equal(&expected, &supplied_mac) {
            bail!("fabric probe authentication failed")
        }
        stream.write_all(&nonce).await?;
        stream.write_all(&length).await?;
        stream.write_all(&payload).await?;
        stream.flush().await?;
        Ok::<(), anyhow::Error>(())
    })
    .await
    .context("fabric probe I/O timed out")??;
    Ok(())
}

fn request_mac(
    secret: &[u8],
    nonce: &[u8; NONCE_BYTES],
    payload: &[u8],
) -> Result<[u8; MAC_BYTES]> {
    let mut mac = HmacSha256::new_from_slice(secret).context("building fabric probe MAC")?;
    mac.update(MAGIC);
    mac.update(nonce);
    mac.update(&(payload.len() as u32).to_be_bytes());
    mac.update(payload);
    let bytes = mac.finalize().into_bytes();
    let mut out = [0_u8; MAC_BYTES];
    out.copy_from_slice(&bytes);
    Ok(out)
}

fn constant_time_equal(left: &[u8], right: &[u8]) -> bool {
    if left.len() != right.len() {
        return false;
    }
    let mut difference = 0_u8;
    for (&a, &b) in left.iter().zip(right) {
        difference |= a ^ b;
    }
    difference == 0
}

fn validate_options(options: &ProbeOptions) -> Result<()> {
    validate_secret(&options.shared_secret)?;
    if options.site.trim().is_empty() || options.site.len() > 128 {
        bail!("fabric site label must be present and at most 128 bytes")
    }
    if options.payload_bytes == 0 || options.payload_bytes > MAX_PAYLOAD_BYTES {
        bail!("fabric payload bytes must be between 1 and {MAX_PAYLOAD_BYTES}")
    }
    if options.rounds == 0 || options.rounds > MAX_ROUNDS {
        bail!("fabric rounds must be between 1 and {MAX_ROUNDS}")
    }
    Ok(())
}

fn validate_secret(secret: &[u8]) -> Result<()> {
    if secret.len() < 32 || secret.len() > 4096 {
        bail!("fabric shared-secret must contain 32 to 4096 bytes")
    }
    Ok(())
}

fn percentile_u64(sorted: &[u64], percentile: usize) -> u64 {
    let index = percentile_index(sorted.len(), percentile);
    sorted[index]
}

fn percentile_f64(sorted: &[f64], percentile: usize) -> f64 {
    let index = percentile_index(sorted.len(), percentile);
    sorted[index]
}

fn percentile_index(length: usize, percentile: usize) -> usize {
    debug_assert!(length > 0);
    // Nearest-rank, with p50/p95 selecting actual observed rounds rather than
    // interpolating a value no packet transfer produced.
    ((length * percentile).div_ceil(100)).saturating_sub(1)
}

fn sha256_hex(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    const SECRET: &[u8] = b"0123456789abcdef0123456789abcdef";

    async fn listener() -> (SocketAddr, tokio::task::JoinHandle<Result<()>>) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let server = tokio::spawn(run_listener(listener, SECRET.to_vec()));
        (addr, server)
    }

    #[tokio::test]
    async fn authenticated_link_probe_records_raw_measurements_without_promoting_a_cluster() {
        let (endpoint, server) = listener().await;
        let receipt = probe(ProbeOptions {
            endpoint,
            site: "lab-rack-a".into(),
            payload_bytes: 4096,
            rounds: 3,
            shared_secret: SECRET.to_vec(),
        })
        .await
        .unwrap();
        server.abort();

        assert_eq!(receipt.status, "MEASURED_NOT_ADMISSIBLE");
        assert!(!receipt.local_cluster_admissible);
        assert_eq!(receipt.rounds.len(), 3);
        assert!(receipt
            .rounds
            .iter()
            .all(|round| round.round_trip_micros > 0));
        assert!(receipt
            .rounds
            .iter()
            .all(|round| round.payload_goodput_mbps.is_finite()));
        assert!(receipt
            .non_admission_reasons
            .iter()
            .any(|reason| reason.contains("control-plane worker identity")));
    }

    #[tokio::test]
    async fn invalid_shared_secret_is_refused_and_does_not_take_down_the_listener() {
        let (endpoint, server) = listener().await;
        let wrong = probe_once(endpoint, b"abcdefghijklmnopqrstuvwxyz012345", 1024).await;
        assert!(wrong.is_err());

        let good = probe_once(endpoint, SECRET, 1024).await;
        server.abort();
        assert!(good.is_ok());
    }

    #[test]
    fn probe_rejects_unbounded_or_unqualified_inputs() {
        let options = ProbeOptions {
            endpoint: "127.0.0.1:1".parse().unwrap(),
            site: "".into(),
            payload_bytes: MAX_PAYLOAD_BYTES + 1,
            rounds: MAX_ROUNDS + 1,
            shared_secret: SECRET.to_vec(),
        };
        assert!(validate_options(&options).is_err());
    }
}

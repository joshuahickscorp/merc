//! Direct link measurements for a *candidate* local Merc Fabric site.
//!
//! A measured TCP path is necessary evidence for a local cluster, but it is
//! deliberately insufficient to schedule one. The result records raw link
//! facts and explicitly remains non-admissible even after certificate-bound
//! mTLS. Site governance, workload data authority, collective benchmarks, and
//! topology economics must be bound separately before a cluster can schedule.

use std::fs::{self, OpenOptions};
use std::io::Write;
use std::net::SocketAddr;
use std::path::Path;
use std::sync::Arc;
use std::time::{Instant, SystemTime, UNIX_EPOCH};

use anyhow::{bail, Context, Result};
use hmac::{Hmac, Mac};
use rand::RngCore;
use rustls::pki_types::{CertificateDer, PrivateKeyDer, ServerName};
use rustls::server::WebPkiClientVerifier;
use rustls::{ClientConfig, RootCertStore, ServerConfig};
use serde::Serialize;
use sha2::{Digest, Sha256};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::time::{timeout, Duration};
use tokio_rustls::{TlsAcceptor, TlsConnector};
use uuid::Uuid;

const MAGIC: &[u8; 9] = b"MERC-FAB1";
const NONCE_BYTES: usize = 32;
const MAX_PAYLOAD_BYTES: usize = 4 * 1024 * 1024;
const MAX_ROUNDS: u16 = 32;
const IO_TIMEOUT: Duration = Duration::from_secs(15);

type HmacSha256 = Hmac<Sha256>;

pub const DEFAULT_PAYLOAD_BYTES: usize = 256 * 1024;
pub const DEFAULT_ROUNDS: u16 = 12;

#[derive(Debug, Clone)]
pub struct FabricTlsMaterial {
    certificate_der: Vec<u8>,
    private_key_der: Vec<u8>,
    ca_certificate_der: Vec<u8>,
    server_name: String,
}

impl FabricTlsMaterial {
    pub fn certificate_sha256(&self) -> String {
        sha256_hex(&self.certificate_der)
    }

    fn certificate(&self) -> CertificateDer<'static> {
        CertificateDer::from(self.certificate_der.clone())
    }

    fn private_key(&self) -> Result<PrivateKeyDer<'static>> {
        PrivateKeyDer::try_from(self.private_key_der.clone())
            .map_err(|error| anyhow::anyhow!("parsing fabric private-key DER: {error}"))
    }

    fn roots(&self) -> Result<RootCertStore> {
        let mut roots = RootCertStore::empty();
        roots
            .add(CertificateDer::from(self.ca_certificate_der.clone()))
            .context("adding fabric CA certificate DER to mTLS trust roots")?;
        Ok(roots)
    }

    fn client_config(&self) -> Result<ClientConfig> {
        ClientConfig::builder()
            .with_root_certificates(self.roots()?)
            .with_client_auth_cert(vec![self.certificate()], self.private_key()?)
            .context("building fabric mTLS client configuration")
    }

    fn server_config(&self) -> Result<ServerConfig> {
        let verifier = WebPkiClientVerifier::builder(Arc::new(self.roots()?))
            .build()
            .context("building fabric mTLS client-certificate verifier")?;
        ServerConfig::builder()
            .with_client_cert_verifier(verifier)
            .with_single_cert(vec![self.certificate()], self.private_key()?)
            .context("building fabric mTLS server configuration")
    }

    fn server_name(&self) -> Result<ServerName<'static>> {
        ServerName::try_from(self.server_name.clone())
            .map_err(|error| anyhow::anyhow!("fabric server name is not a valid DNS name: {error}"))
    }
}

pub fn read_tls_material(
    certificate_der_path: &Path,
    private_key_der_path: &Path,
    ca_certificate_der_path: &Path,
    server_name: String,
) -> Result<FabricTlsMaterial> {
    if server_name.trim().is_empty() || server_name.len() > 253 {
        bail!("fabric mTLS server name must be present and at most 253 bytes")
    }
    let key_metadata = fs::metadata(private_key_der_path).with_context(|| {
        format!(
            "reading fabric private-key metadata {}",
            private_key_der_path.display()
        )
    })?;
    if !key_metadata.is_file() {
        bail!("fabric private-key path is not a regular file")
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if key_metadata.permissions().mode() & 0o077 != 0 {
            bail!("fabric private-key file must not be group- or world-readable")
        }
    }
    let material = FabricTlsMaterial {
        certificate_der: fs::read(certificate_der_path).with_context(|| {
            format!(
                "reading fabric certificate {}",
                certificate_der_path.display()
            )
        })?,
        private_key_der: fs::read(private_key_der_path).with_context(|| {
            format!(
                "reading fabric private key {}",
                private_key_der_path.display()
            )
        })?,
        ca_certificate_der: fs::read(ca_certificate_der_path).with_context(|| {
            format!(
                "reading fabric CA certificate {}",
                ca_certificate_der_path.display()
            )
        })?,
        server_name: server_name.trim().to_string(),
    };
    // Validate both directions before a listener is opened or an identity is
    // registered. This prevents a syntactically shaped file from becoming a
    // control-plane identity that the mTLS runtime cannot actually use.
    material.client_config()?;
    material.server_config()?;
    material.server_name()?;
    Ok(material)
}

#[derive(Debug, Clone)]
pub struct ProbeOptions {
    pub endpoint: SocketAddr,
    pub site: String,
    pub fabric_session_id: Uuid,
    pub expected_peer_worker_id: Option<Uuid>,
    pub expected_peer_certificate_sha256: String,
    pub payload_bytes: usize,
    pub rounds: u16,
    pub tls: FabricTlsMaterial,
}

#[derive(Debug, Serialize)]
pub struct FabricProbeRound {
    pub round: u16,
    pub payload_bytes_each_direction: usize,
    pub round_trip_payload_bytes: usize,
    pub round_trip_micros: u64,
    pub payload_goodput_mbps: f64,
    pub transcript_sha256: String,
}

#[derive(Debug, Serialize)]
pub struct FabricProbeReceipt {
    pub schema_version: u32,
    pub receipt_id: Uuid,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fabric_session_id: Option<Uuid>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub expected_peer_worker_id: Option<Uuid>,
    pub kind: &'static str,
    pub status: &'static str,
    pub measured_at_unix_ms: u128,
    // This is a supplier/operator declaration. The receipt never infers
    // physical location from an address and does not disclose the endpoint.
    pub declared_site: String,
    pub peer_endpoint_commitment: String,
    pub transport: &'static str,
    pub peer_authentication: &'static str,
    pub local_certificate_sha256: String,
    pub peer_certificate_sha256: String,
    pub payload_is_random: bool,
    pub rounds: Vec<FabricProbeRound>,
    pub p50_round_trip_micros: u64,
    pub p95_round_trip_micros: u64,
    pub p50_payload_goodput_mbps: f64,
    pub local_cluster_admissible: bool,
    pub non_admission_reasons: Vec<&'static str>,
}

#[derive(Debug, Serialize)]
pub struct FabricProbeObservation {
    pub schema_version: u32,
    pub fabric_session_id: Uuid,
    pub transcript_sha256: String,
    pub payload_bytes_each_direction: usize,
    pub observed_at_unix_ms: u128,
    pub observed_peer_certificate_sha256: String,
}

pub async fn serve(
    bind: SocketAddr,
    tls: FabricTlsMaterial,
    observer: crate::protocol::ControlPlaneClient,
) -> Result<()> {
    let listener = TcpListener::bind(bind)
        .await
        .with_context(|| format!("binding fabric echo listener at {bind}"))?;
    let local = listener
        .local_addr()
        .context("reading fabric listener address")?;
    tracing::info!(%local, certificate_sha256 = %tls.certificate_sha256(), "fabric mTLS echo listener ready; it accepts bounded measurement traffic only");
    run_listener(
        listener,
        TlsAcceptor::from(Arc::new(tls.server_config()?)),
        observer,
    )
    .await
}

async fn run_listener(
    listener: TcpListener,
    acceptor: TlsAcceptor,
    observer: crate::protocol::ControlPlaneClient,
) -> Result<()> {
    loop {
        tokio::select! {
            signal = tokio::signal::ctrl_c() => {
                signal.context("waiting for fabric listener shutdown signal")?;
                return Ok(());
            }
            accepted = listener.accept() => {
                let (stream, remote) = accepted.context("accepting fabric probe connection")?;
                let acceptor = acceptor.clone();
                let observer = observer.clone();
                tokio::spawn(async move {
                    if let Err(error) = async {
                        let tls = timeout(IO_TIMEOUT, acceptor.accept(stream)).await
                            .context("fabric mTLS handshake timed out")??;
                        echo_once(tls, observer).await
                    }.await {
                        // Invalid probes must neither disclose mTLS material nor stop
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
            &options.tls,
            &options.expected_peer_certificate_sha256,
            options.fabric_session_id,
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
            transcript_sha256: measurement.transcript_sha256,
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
        receipt_id: Uuid::new_v4(),
        fabric_session_id: options
            .expected_peer_worker_id
            .map(|_| options.fabric_session_id),
        expected_peer_worker_id: options.expected_peer_worker_id,
        kind: "MERC_FABRIC_TCP_ECHO_RECEIPT",
        status: "MEASURED_NOT_ADMISSIBLE",
        measured_at_unix_ms: SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis(),
        declared_site: options.site,
        peer_endpoint_commitment: endpoint_commitment(&options.tls, options.endpoint)?,
        transport: "MERC_FABRIC_MTLS_ECHO_V1",
        peer_authentication: "MUTUAL_TLS_WORKER_CERTIFICATE_BOUND",
        local_certificate_sha256: options.tls.certificate_sha256(),
        peer_certificate_sha256: options.expected_peer_certificate_sha256,
        payload_is_random: true,
        p50_round_trip_micros: percentile_u64(&latencies, 50),
        p95_round_trip_micros: percentile_u64(&latencies, 95),
        p50_payload_goodput_mbps: percentile_f64(&goodputs, 50),
        rounds,
        local_cluster_admissible: false,
        non_admission_reasons: vec![
            "certificate-bound mutual TLS does not independently govern a site or residency",
            "the declared site is not an independently governed location or residency authority",
            "mTLS echo is not a workload data plane and does not authorize customer data transfer",
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
    transcript_sha256: String,
}

async fn probe_once(
    endpoint: SocketAddr,
    tls: &FabricTlsMaterial,
    expected_peer_certificate_sha256: &str,
    session_id: Uuid,
    payload_bytes: usize,
) -> Result<RoundMeasurement> {
    let mut payload = vec![0_u8; payload_bytes];
    rand::thread_rng().fill_bytes(&mut payload);
    let mut nonce = [0_u8; NONCE_BYTES];
    rand::thread_rng().fill_bytes(&mut nonce);
    let transcript_sha256 = transcript_sha256(session_id, &nonce, &payload);

    let start = Instant::now();
    let stream = timeout(IO_TIMEOUT, TcpStream::connect(endpoint))
        .await
        .context("fabric peer connection timed out")?
        .with_context(|| format!("connecting to fabric peer {endpoint}"))?;
    let connector = TlsConnector::from(Arc::new(tls.client_config()?));
    let mut stream = timeout(IO_TIMEOUT, connector.connect(tls.server_name()?, stream))
        .await
        .context("fabric mTLS client handshake timed out")?
        .context("authenticating fabric mTLS peer")?;
    let peer = stream
        .get_ref()
        .1
        .peer_certificates()
        .and_then(|certificates| certificates.first())
        .ok_or_else(|| {
            anyhow::anyhow!("fabric mTLS peer did not present an end-entity certificate")
        })?;
    let observed_peer_certificate_sha256 = sha256_hex(peer.as_ref());
    if observed_peer_certificate_sha256 != expected_peer_certificate_sha256 {
        bail!(
            "fabric mTLS peer certificate differs from the control-plane reserved worker identity"
        )
    }
    timeout(IO_TIMEOUT, async {
        stream.write_all(MAGIC).await?;
        stream.write_all(session_id.as_bytes()).await?;
        stream.write_all(&nonce).await?;
        stream
            .write_all(&(payload.len() as u32).to_be_bytes())
            .await?;
        stream.write_all(&payload).await?;
        stream.flush().await?;

        let mut echoed_session = [0_u8; 16];
        let mut echoed_nonce = [0_u8; NONCE_BYTES];
        let mut length = [0_u8; 4];
        stream.read_exact(&mut echoed_session).await?;
        stream.read_exact(&mut echoed_nonce).await?;
        stream.read_exact(&mut length).await?;
        let echoed_len = u32::from_be_bytes(length) as usize;
        if echoed_session != *session_id.as_bytes()
            || echoed_nonce != nonce
            || echoed_len != payload.len()
        {
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
    // Report only the microsecond precision carried in the receipt. The
    // control plane independently recomputes this exact expression from the
    // retained bytes and elapsed micros; publishing nanosecond-derived display
    // rates would make otherwise identical evidence disagree after transport.
    let goodput_mbps = payload_bytes as f64 * 2.0 * 8.0 / elapsed_micros as f64;
    Ok(RoundMeasurement {
        elapsed_micros,
        goodput_mbps,
        transcript_sha256,
    })
}

async fn echo_once(
    mut stream: tokio_rustls::server::TlsStream<TcpStream>,
    observer: crate::protocol::ControlPlaneClient,
) -> Result<()> {
    let peer = stream
        .get_ref()
        .1
        .peer_certificates()
        .and_then(|certificates| certificates.first())
        .ok_or_else(|| {
            anyhow::anyhow!("fabric mTLS client did not present an end-entity certificate")
        })?;
    let observed_peer_certificate_sha256 = sha256_hex(peer.as_ref());
    timeout(IO_TIMEOUT, async {
        let mut magic = [0_u8; MAGIC.len()];
        stream.read_exact(&mut magic).await?;
        if magic != *MAGIC {
            bail!("unknown fabric probe protocol")
        }
        let mut session_bytes = [0_u8; 16];
        let mut nonce = [0_u8; NONCE_BYTES];
        let mut length = [0_u8; 4];
        stream.read_exact(&mut session_bytes).await?;
        stream.read_exact(&mut nonce).await?;
        stream.read_exact(&mut length).await?;
        let payload_len = u32::from_be_bytes(length) as usize;
        if payload_len == 0 || payload_len > MAX_PAYLOAD_BYTES {
            bail!("fabric probe payload is outside the bounded range")
        }
        let mut payload = vec![0_u8; payload_len];
        stream.read_exact(&mut payload).await?;
        let session_id = Uuid::from_bytes(session_bytes);
        stream.write_all(&session_bytes).await?;
        stream.write_all(&nonce).await?;
        stream.write_all(&length).await?;
        stream.write_all(&payload).await?;
        stream.flush().await?;
        let observation = FabricProbeObservation {
            schema_version: 1,
            fabric_session_id: session_id,
            transcript_sha256: transcript_sha256(session_id, &nonce, &payload),
            payload_bytes_each_direction: payload.len(),
            observed_at_unix_ms: SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis(),
            observed_peer_certificate_sha256: observed_peer_certificate_sha256.clone(),
        };
        tokio::spawn(async move {
            if let Err(error) = observer.submit_fabric_observation(&observation).await {
                tracing::warn!(%error, session_id = %session_id, "could not record fabric peer observation");
            }
        });
        Ok::<(), anyhow::Error>(())
    })
    .await
    .context("fabric probe I/O timed out")??;
    Ok(())
}

fn transcript_sha256(session_id: Uuid, nonce: &[u8; NONCE_BYTES], payload: &[u8]) -> String {
    let mut digest = Sha256::new();
    digest.update(MAGIC);
    digest.update(session_id.as_bytes());
    digest.update(nonce);
    digest.update((payload.len() as u32).to_be_bytes());
    digest.update(payload);
    sha256_hex(digest.finalize().as_slice())
}

fn validate_options(options: &ProbeOptions) -> Result<()> {
    if options.expected_peer_worker_id.is_none() || options.fabric_session_id.is_nil() {
        bail!("a certificate-bound fabric probe requires a control-plane reserved peer session")
    }
    if options.site.trim().is_empty() || options.site.len() > 128 {
        bail!("fabric site label must be present and at most 128 bytes")
    }
    if options.payload_bytes == 0 || options.payload_bytes > MAX_PAYLOAD_BYTES {
        bail!("fabric payload bytes must be between 1 and {MAX_PAYLOAD_BYTES}")
    }
    if options.rounds == 0 || options.rounds > MAX_ROUNDS {
        bail!("fabric rounds must be between 1 and {MAX_ROUNDS}")
    }
    if !is_sha256_hex(&options.expected_peer_certificate_sha256) {
        bail!("fabric peer certificate fingerprint must be a SHA-256 hex digest")
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

// This keyed commitment records that a certificate identity consistently used
// a declared direct endpoint without sending that endpoint to control. The
// DER private key stays local, so a digest of an RFC1918 endpoint is not made
// enumerable from the retained receipt.
fn endpoint_commitment(tls: &FabricTlsMaterial, endpoint: SocketAddr) -> Result<String> {
    let mut mac = HmacSha256::new_from_slice(&tls.private_key_der)
        .context("building fabric private-key endpoint commitment")?;
    mac.update(b"merc-fabric-endpoint-v2\x00");
    mac.update(tls.certificate_sha256().as_bytes());
    mac.update(endpoint.to_string().as_bytes());
    Ok(mac
        .finalize()
        .into_bytes()
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect())
}

fn sha256_hex(input: &[u8]) -> String {
    Sha256::digest(input)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

fn is_sha256_hex(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase())
}

#[cfg(test)]
mod tests {
    use super::*;
    use rcgen::{
        BasicConstraints, CertificateParams, ExtendedKeyUsagePurpose, IsCa, Issuer, KeyPair,
        KeyUsagePurpose,
    };

    fn unvalidated_tls() -> FabricTlsMaterial {
        FabricTlsMaterial {
            certificate_der: vec![1],
            private_key_der: vec![2],
            ca_certificate_der: vec![3],
            server_name: "fabric.test".into(),
        }
    }

    #[test]
    fn probe_refuses_unreserved_or_unbounded_inputs() {
        let options = ProbeOptions {
            endpoint: "127.0.0.1:1".parse().unwrap(),
            site: "".into(),
            fabric_session_id: Uuid::new_v4(),
            expected_peer_worker_id: None,
            expected_peer_certificate_sha256: "e".repeat(64),
            payload_bytes: MAX_PAYLOAD_BYTES + 1,
            rounds: MAX_ROUNDS + 1,
            tls: unvalidated_tls(),
        };
        assert!(validate_options(&options).is_err());
    }

    #[test]
    fn transcript_and_endpoint_commitment_bind_the_mtls_identity() {
        let session = Uuid::new_v4();
        let nonce = [7; NONCE_BYTES];
        let transcript = transcript_sha256(session, &nonce, b"random payload");
        assert!(is_sha256_hex(&transcript));
        let endpoint: SocketAddr = "127.0.0.1:9444".parse().unwrap();
        let mut different_tls = unvalidated_tls();
        different_tls.private_key_der = vec![4];
        assert_ne!(
            endpoint_commitment(&unvalidated_tls(), endpoint).unwrap(),
            endpoint_commitment(&different_tls, endpoint).unwrap(),
        );
    }

    #[tokio::test]
    async fn actual_mutual_tls_probe_rejects_a_wrong_bound_peer_and_emits_a_receipt() {
        let mut ca_params = CertificateParams::new(Vec::<String>::new()).unwrap();
        ca_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        ca_params.key_usages = vec![
            KeyUsagePurpose::DigitalSignature,
            KeyUsagePurpose::KeyCertSign,
        ];
        let ca_key = KeyPair::generate().unwrap();
        let ca_certificate = ca_params.self_signed(&ca_key).unwrap();
        let issuer = Issuer::new(ca_params, ca_key);

        let issue = |name: &str| {
            let mut params = CertificateParams::new(vec![name.to_string()]).unwrap();
            params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
            params.extended_key_usages = vec![
                ExtendedKeyUsagePurpose::ServerAuth,
                ExtendedKeyUsagePurpose::ClientAuth,
            ];
            let key = KeyPair::generate().unwrap();
            let certificate = params.signed_by(&key, &issuer).unwrap();
            (certificate.der().to_vec(), key.serialize_der())
        };
        let (server_certificate, server_key) = issue("fabric.test");
        let (client_certificate, client_key) = issue("fabric.client.test");
        let server_tls = FabricTlsMaterial {
            certificate_der: server_certificate.clone(),
            private_key_der: server_key,
            ca_certificate_der: ca_certificate.der().to_vec(),
            server_name: "fabric.test".into(),
        };
        let client_tls = FabricTlsMaterial {
            certificate_der: client_certificate,
            private_key_der: client_key,
            ca_certificate_der: ca_certificate.der().to_vec(),
            server_name: "fabric.test".into(),
        };
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = listener.local_addr().unwrap();
        let observer =
            crate::protocol::ControlPlaneClient::new("https://127.0.0.1", "test-worker-token")
                .unwrap();
        let server = tokio::spawn(run_listener(
            listener,
            TlsAcceptor::from(Arc::new(server_tls.server_config().unwrap())),
            observer,
        ));

        let session = Uuid::new_v4();
        let wrong = probe_once(endpoint, &client_tls, &"0".repeat(64), session, 1024).await;
        assert!(
            wrong.is_err(),
            "the exact reserved peer certificate is mandatory"
        );
        let receipt = probe(ProbeOptions {
            endpoint,
            site: "lab-rack-a".into(),
            fabric_session_id: session,
            expected_peer_worker_id: Some(Uuid::new_v4()),
            expected_peer_certificate_sha256: sha256_hex(&server_certificate),
            payload_bytes: 1024,
            rounds: 3,
            tls: client_tls,
        })
        .await
        .unwrap();
        server.abort();

        assert_eq!(receipt.transport, "MERC_FABRIC_MTLS_ECHO_V1");
        assert_eq!(
            receipt.peer_authentication,
            "MUTUAL_TLS_WORKER_CERTIFICATE_BOUND"
        );
        assert_eq!(receipt.rounds.len(), 3);
        assert!(is_sha256_hex(&receipt.local_certificate_sha256));
        assert_eq!(
            receipt.peer_certificate_sha256,
            sha256_hex(&server_certificate)
        );
    }
}

use std::collections::BTreeSet;
use std::time::Duration;

use reqwest::{Client, RequestBuilder, Response, StatusCode};
use uuid::Uuid;

use crate::types::{
    ConnectStatus, Earnings, FailReport, Heartbeat, RealtimeOfferHeartbeat,
    RealtimeOfferRegistration, ServiceLeaseAssignment, ServiceLeaseHeartbeat,
    ServiceLeaseOfferRegistration, SupplierVerification, TaskCommit, TaskDispatch,
    WorkerCapability,
};

const POLL_TIMEOUT: Duration = Duration::from_secs(35);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const FABRIC_OBSERVATION_WAIT: Duration = Duration::from_secs(15);
const FABRIC_OBSERVATION_POLL: Duration = Duration::from_millis(250);

const IDEMPOTENT_MAX_ATTEMPTS: usize = 4;
#[cfg(not(test))]
const IDEMPOTENT_RETRY_BASE_DELAY: Duration = Duration::from_millis(200);
#[cfg(test)]
const IDEMPOTENT_RETRY_BASE_DELAY: Duration = Duration::from_millis(1);

const POLL_PATH: &str = "/v1/worker/poll?wait_ms=25000";

#[derive(Debug, thiserror::Error)]
pub enum ProtocolError {
    #[error("worker token is empty; refusing to send unauthenticated request")]
    MissingToken,
    #[error("TLS client configuration rejected: {0}")]
    TLSConfig(String),
    #[error("transport error calling {endpoint}: {source}")]
    Transport {
        endpoint: String,
        #[source]
        source: reqwest::Error,
    },
    #[error("unexpected status {status} from {endpoint}: {body}")]
    Status {
        endpoint: String,
        status: StatusCode,
        body: String,
    },
    #[error("decoding response from {endpoint}: {source}")]
    Decode {
        endpoint: String,
        #[source]
        source: reqwest::Error,
    },
}

#[derive(Clone)]
pub struct ControlPlaneClient {
    http: Client,
    base_url: String,
    token: String,
}

#[derive(serde::Deserialize)]
struct FabricSessionResponse {
    fabric_session_id: Uuid,
    peer_certificate_sha256: String,
}

#[derive(Debug, Clone)]
pub struct FabricSession {
    pub fabric_session_id: Uuid,
    pub peer_certificate_sha256: String,
}

#[derive(serde::Deserialize)]
struct FabricObservationStatus {
    observed_transcript_sha256: Vec<String>,
}

impl ControlPlaneClient {
    pub fn new(
        base_url: impl Into<String>,
        token: impl Into<String>,
    ) -> Result<Self, ProtocolError> {
        let token = token.into();
        if token.is_empty() {
            return Err(ProtocolError::MissingToken);
        }
        let http = crate::tls::client_builder()
            .map_err(|error| ProtocolError::TLSConfig(error.to_string()))?
            .timeout(REQUEST_TIMEOUT)
            .user_agent(concat!("merc-agent/", env!("CARGO_PKG_VERSION")))
            .build()
            .expect("reqwest client builds with rustls");
        Ok(Self {
            http,
            base_url: base_url.into().trim_end_matches('/').to_string(),
            token,
        })
    }

    fn url(&self, path: &str) -> String {
        format!("{}{}", self.base_url, path)
    }

    fn transport(endpoint: &str, source: reqwest::Error) -> ProtocolError {
        ProtocolError::Transport {
            endpoint: endpoint.to_string(),
            source,
        }
    }

    async fn expect_status(
        endpoint: &str,
        resp: Response,
        ok: &[StatusCode],
    ) -> Result<Response, ProtocolError> {
        let status = resp.status();
        if ok.contains(&status) {
            return Ok(resp);
        }
        let body = resp.text().await.unwrap_or_default();
        Err(ProtocolError::Status {
            endpoint: endpoint.to_string(),
            status,
            body,
        })
    }

    async fn send_idempotent<F>(
        &self,
        endpoint: &str,
        operation: &str,
        terminal_success: &[StatusCode],
        mut request: F,
    ) -> Result<(), ProtocolError>
    where
        F: FnMut() -> RequestBuilder,
    {
        let mut delay = IDEMPOTENT_RETRY_BASE_DELAY;

        for attempt in 0..IDEMPOTENT_MAX_ATTEMPTS {
            match request().send().await {
                Ok(resp)
                    if resp.status().is_success() || terminal_success.contains(&resp.status()) =>
                {
                    return Ok(());
                }
                Ok(resp) => {
                    let status = resp.status();
                    let retryable =
                        status == StatusCode::TOO_MANY_REQUESTS || status.is_server_error();
                    if retryable && attempt + 1 < IDEMPOTENT_MAX_ATTEMPTS {
                        tracing::warn!(
                            %operation,
                            attempt = attempt + 1,
                            max_attempts = IDEMPOTENT_MAX_ATTEMPTS,
                            %status,
                            delay_ms = delay.as_millis(),
                            "transient status; retrying identical idempotent request"
                        );
                        drop(resp);
                        tokio::time::sleep(delay).await;
                        delay *= 2;
                        continue;
                    }

                    let body = resp.text().await.unwrap_or_default();
                    return Err(ProtocolError::Status {
                        endpoint: endpoint.to_string(),
                        status,
                        body,
                    });
                }
                Err(err) => {
                    if attempt + 1 == IDEMPOTENT_MAX_ATTEMPTS {
                        return Err(Self::transport(endpoint, err));
                    }
                    tracing::warn!(
                        %operation,
                        attempt = attempt + 1,
                        max_attempts = IDEMPOTENT_MAX_ATTEMPTS,
                        error = %err,
                        delay_ms = delay.as_millis(),
                        "transport failure; retrying identical idempotent request"
                    );
                    tokio::time::sleep(delay).await;
                    delay *= 2;
                }
            }
        }

        unreachable!("bounded idempotent retry loop always returns")
    }

    pub async fn register(
        &self,
        cap: &WorkerCapability,
    ) -> Result<WorkerCapability, ProtocolError> {
        let endpoint = "/v1/worker/register";
        let resp = self
            .http
            .post(self.url(endpoint))
            .header("X-Worker-Token", &self.token)
            .json(cap)
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        let resp =
            Self::expect_status(endpoint, resp, &[StatusCode::OK, StatusCode::CREATED]).await?;
        resp.json::<WorkerCapability>()
            .await
            .map_err(|e| ProtocolError::Decode {
                endpoint: endpoint.to_string(),
                source: e,
            })
    }

    pub async fn heartbeat(&self, hb: &Heartbeat) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/heartbeat";
        let resp = self
            .http
            .post(self.url(endpoint))
            .header("X-Worker-Token", &self.token)
            .json(hb)
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        Self::expect_status(endpoint, resp, &[StatusCode::NO_CONTENT, StatusCode::OK]).await?;
        Ok(())
    }

    pub async fn register_realtime(
        &self,
        offer: &RealtimeOfferRegistration,
    ) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/realtime/register";
        let resp = self
            .http
            .post(self.url(endpoint))
            .header("X-Worker-Token", &self.token)
            .json(offer)
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        Self::expect_status(endpoint, resp, &[StatusCode::OK, StatusCode::CREATED]).await?;
        Ok(())
    }

    pub async fn heartbeat_realtime(
        &self,
        heartbeat: &RealtimeOfferHeartbeat,
    ) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/realtime/heartbeat";
        let resp = self
            .http
            .post(self.url(endpoint))
            .header("X-Worker-Token", &self.token)
            .json(heartbeat)
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        Self::expect_status(endpoint, resp, &[StatusCode::NO_CONTENT, StatusCode::OK]).await?;
        Ok(())
    }

    pub async fn register_service_lease_offer(
        &self,
        offer: &ServiceLeaseOfferRegistration,
    ) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/service-leases/offers";
        let resp = self
            .http
            .post(self.url(endpoint))
            .header("X-Worker-Token", &self.token)
            .json(offer)
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        Self::expect_status(endpoint, resp, &[StatusCode::OK, StatusCode::CREATED]).await?;
        Ok(())
    }

    pub async fn service_lease_assignments(
        &self,
    ) -> Result<Vec<ServiceLeaseAssignment>, ProtocolError> {
        let endpoint = "/v1/worker/service-leases/active";
        let resp = self
            .http
            .get(self.url(endpoint))
            .header("X-Worker-Token", &self.token)
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        let resp = Self::expect_status(endpoint, resp, &[StatusCode::OK]).await?;
        resp.json::<Vec<ServiceLeaseAssignment>>()
            .await
            .map_err(|e| ProtocolError::Decode {
                endpoint: endpoint.to_string(),
                source: e,
            })
    }

    pub async fn heartbeat_service_lease(
        &self,
        lease_id: Uuid,
        heartbeat: &ServiceLeaseHeartbeat,
    ) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/service-leases/{id}/heartbeat";
        let path = format!("/v1/worker/service-leases/{lease_id}/heartbeat");
        let resp = self
            .http
            .post(self.url(&path))
            .header("X-Worker-Token", &self.token)
            .json(heartbeat)
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        Self::expect_status(endpoint, resp, &[StatusCode::NO_CONTENT, StatusCode::OK]).await?;
        Ok(())
    }

    // A fabric receipt is worker-authenticated operational evidence. The
    // control plane persists it as self-reported and non-admissible; the agent
    // must never treat a successful upload as permission to carry workload
    // data over this measured link.
    pub async fn submit_fabric_receipt(
        &self,
        receipt: &crate::fabric::FabricProbeReceipt,
    ) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/fabric/receipts";
        self.send_idempotent(endpoint, "submit_fabric_receipt", &[], || {
            self.http
                .post(self.url(endpoint))
                .header("X-Worker-Token", &self.token)
                .json(receipt)
        })
        .await
    }

    pub async fn register_fabric_identity(
        &self,
        certificate_sha256: &str,
    ) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/fabric/identity";
        self.send_idempotent(endpoint, "register_fabric_identity", &[], || {
            self.http
                .post(self.url(endpoint))
                .header("X-Worker-Token", &self.token)
                .json(&serde_json::json!({ "certificate_sha256": certificate_sha256 }))
        })
        .await
    }

    // This asks control to derive a short-lived evidence mesh from retained
    // mutual-mTLS link and synthetic-collective receipts. A successful response
    // is explicitly not permission to start a workload collective, transfer
    // customer data, or place a gang between peers.
    pub async fn evaluate_fabric_topology(
        &self,
        declared_site: &str,
        worker_ids: &[Uuid],
    ) -> Result<serde_json::Value, ProtocolError> {
        let endpoint = "/v1/worker/fabric/topologies/evaluate";
        let resp = self
            .http
            .post(self.url(endpoint))
            .header("X-Worker-Token", &self.token)
            .json(&serde_json::json!({
                "schema_version": 1,
                "declared_site": declared_site,
                "worker_ids": worker_ids,
            }))
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        let resp = Self::expect_status(endpoint, resp, &[StatusCode::OK]).await?;
        resp.json::<serde_json::Value>()
            .await
            .map_err(|e| ProtocolError::Decode {
                endpoint: endpoint.to_string(),
                source: e,
            })
    }

    pub async fn create_fabric_session(
        &self,
        peer_worker_id: Uuid,
        declared_site: &str,
    ) -> Result<FabricSession, ProtocolError> {
        let endpoint = "/v1/worker/fabric/sessions";
        let resp = self
            .http
            .post(self.url(endpoint))
            .header("X-Worker-Token", &self.token)
            .json(&serde_json::json!({
                "peer_worker_id": peer_worker_id,
                "declared_site": declared_site,
            }))
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        let resp = Self::expect_status(endpoint, resp, &[StatusCode::CREATED]).await?;
        let response =
            resp.json::<FabricSessionResponse>()
                .await
                .map_err(|e| ProtocolError::Decode {
                    endpoint: endpoint.to_string(),
                    source: e,
                })?;
        Ok(FabricSession {
            fabric_session_id: response.fabric_session_id,
            peer_certificate_sha256: response.peer_certificate_sha256,
        })
    }

    pub async fn submit_fabric_observation(
        &self,
        observation: &crate::fabric::FabricProbeObservation,
    ) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/fabric/observations";
        self.send_idempotent(endpoint, "submit_fabric_observation", &[], || {
            self.http
                .post(self.url(endpoint))
                .header("X-Worker-Token", &self.token)
                .json(observation)
        })
        .await
    }

    // A collective receipt is persisted only as certificate-bound measurement
    // evidence. Control deliberately keeps it outside placement, pricing, and
    // settlement authorities until those separate gates exist.
    pub async fn submit_fabric_collective_receipt(
        &self,
        receipt: &crate::fabric::FabricCollectiveProbeReceipt,
    ) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/fabric/collective-receipts";
        self.send_idempotent(endpoint, "submit_fabric_collective_receipt", &[], || {
            self.http
                .post(self.url(endpoint))
                .header("X-Worker-Token", &self.token)
                .json(receipt)
        })
        .await
    }

    pub async fn wait_for_fabric_observations(
        &self,
        session_id: Uuid,
        transcripts: &[String],
    ) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/fabric/sessions/{id}/observations";
        let path = format!("/v1/worker/fabric/sessions/{session_id}/observations");
        let wanted = transcripts.iter().cloned().collect::<BTreeSet<_>>();
        let deadline = tokio::time::Instant::now() + FABRIC_OBSERVATION_WAIT;
        loop {
            let resp = self
                .http
                .get(self.url(&path))
                .header("X-Worker-Token", &self.token)
                .send()
                .await
                .map_err(|e| Self::transport(endpoint, e))?;
            let resp = Self::expect_status(endpoint, resp, &[StatusCode::OK]).await?;
            let observed = resp
                .json::<FabricObservationStatus>()
                .await
                .map_err(|e| ProtocolError::Decode {
                    endpoint: endpoint.to_string(),
                    source: e,
                })?
                .observed_transcript_sha256
                .into_iter()
                .collect::<BTreeSet<_>>();
            if wanted.is_subset(&observed) {
                return Ok(());
            }
            if tokio::time::Instant::now() >= deadline {
                return Err(ProtocolError::Status {
                    endpoint: endpoint.to_string(),
                    status: StatusCode::GATEWAY_TIMEOUT,
                    body: "timed out waiting for every reserved peer observation".to_string(),
                });
            }
            tokio::time::sleep(FABRIC_OBSERVATION_POLL).await;
        }
    }

    pub async fn poll_task(&self) -> Result<Option<TaskDispatch>, ProtocolError> {
        let endpoint = "/v1/worker/poll";
        let resp = self
            .http
            .get(self.url(POLL_PATH))
            .header("X-Worker-Token", &self.token)
            .timeout(POLL_TIMEOUT)
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        let status = resp.status();
        if status == StatusCode::NO_CONTENT {
            return Ok(None);
        }
        let resp = Self::expect_status(endpoint, resp, &[StatusCode::OK]).await?;
        let task = resp
            .json::<TaskDispatch>()
            .await
            .map_err(|e| ProtocolError::Decode {
                endpoint: endpoint.to_string(),
                source: e,
            })?;
        Ok(Some(task))
    }

    pub async fn start_task(&self, task_id: Uuid, attempt: i16) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/task/{id}/start";
        let path = format!("/v1/worker/task/{task_id}/start");
        self.send_idempotent(endpoint, "start_task", &[], || {
            self.http
                .post(self.url(&path))
                .header("X-Worker-Token", &self.token)
                .header("X-Task-Attempt", attempt)
        })
        .await
    }

    pub async fn commit_task(
        &self,
        task_id: Uuid,
        commit: &TaskCommit,
    ) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/task/{id}/commit";
        let path = format!("/v1/worker/task/{task_id}/commit");
        self.send_idempotent(endpoint, "commit_task", &[], || {
            self.http
                .post(self.url(&path))
                .header("X-Worker-Token", &self.token)
                .json(commit)
        })
        .await
    }

    pub async fn fail_task(
        &self,
        task_id: Uuid,
        attempt: i16,
        report: &FailReport,
    ) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/task/{id}/fail";
        let path = format!("/v1/worker/task/{task_id}/fail");
        // A conflict/not-found after an ambiguous acknowledgement means this
        // stale local execution no longer owns a claim it may release. Keep
        // those statuses terminal-success for fail only; start and commit must
        // continue treating ownership conflicts as fences.
        self.send_idempotent(
            endpoint,
            "fail_task",
            &[StatusCode::CONFLICT, StatusCode::NOT_FOUND],
            || {
                self.http
                    .post(self.url(&path))
                    .header("X-Worker-Token", &self.token)
                    .header("X-Task-Attempt", attempt)
                    .json(report)
            },
        )
        .await
    }

    pub async fn earnings(&self) -> Result<Earnings, ProtocolError> {
        let endpoint = "/v1/worker/earnings";
        let resp = self
            .http
            .get(self.url(endpoint))
            .header("X-Worker-Token", &self.token)
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        let resp = Self::expect_status(endpoint, resp, &[StatusCode::OK]).await?;
        resp.json::<Earnings>()
            .await
            .map_err(|e| ProtocolError::Decode {
                endpoint: endpoint.to_string(),
                source: e,
            })
    }

    pub async fn connect_status(&self) -> Result<ConnectStatus, ProtocolError> {
        let endpoint = "/v1/worker/connect/status";
        let resp = self
            .http
            .get(self.url(endpoint))
            .header("X-Worker-Token", &self.token)
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        let resp = Self::expect_status(endpoint, resp, &[StatusCode::OK]).await?;
        resp.json::<ConnectStatus>()
            .await
            .map_err(|e| ProtocolError::Decode {
                endpoint: endpoint.to_string(),
                source: e,
            })
    }

    pub async fn verification(&self) -> Result<SupplierVerification, ProtocolError> {
        let endpoint = "/v1/worker/verification";
        let resp = self
            .http
            .get(self.url(endpoint))
            .header("X-Worker-Token", &self.token)
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        let resp = Self::expect_status(endpoint, resp, &[StatusCode::OK]).await?;
        resp.json::<SupplierVerification>()
            .await
            .map_err(|e| ProtocolError::Decode {
                endpoint: endpoint.to_string(),
                source: e,
            })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        atomic::{AtomicUsize, Ordering},
        Arc,
    };
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    async fn spawn_request_server(
        statuses: Vec<u16>,
    ) -> (
        String,
        Arc<AtomicUsize>,
        Arc<tokio::sync::Mutex<Vec<String>>>,
    ) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let requests = Arc::new(tokio::sync::Mutex::new(Vec::new()));
        let requests_for_server = requests.clone();
        let calls = Arc::new(AtomicUsize::new(0));
        let calls_for_server = calls.clone();

        tokio::spawn(async move {
            for status in statuses {
                let (mut socket, _) = listener.accept().await.unwrap();
                let mut buf = vec![0u8; 16384];
                let mut total = 0usize;
                let header_end = loop {
                    let n = socket.read(&mut buf[total..]).await.unwrap();
                    total += n;
                    if let Some(pos) = buf[..total].windows(4).position(|w| w == b"\r\n\r\n") {
                        break pos + 4;
                    }
                    if n == 0 {
                        break total;
                    }
                };
                let headers = String::from_utf8_lossy(&buf[..header_end]).to_lowercase();
                let content_length = headers
                    .lines()
                    .find_map(|line| {
                        line.strip_prefix("content-length:")
                            .and_then(|value| value.trim().parse::<usize>().ok())
                    })
                    .unwrap_or(0);
                while total < header_end + content_length {
                    let n = socket.read(&mut buf[total..]).await.unwrap();
                    if n == 0 {
                        break;
                    }
                    total += n;
                }
                calls_for_server.fetch_add(1, Ordering::SeqCst);
                requests_for_server
                    .lock()
                    .await
                    .push(String::from_utf8_lossy(&buf[..total]).to_string());

                // Closing after receipt but before any HTTP response models the
                // true ambiguous-ack transport case: the server may have made
                // the operation durable even though the client sees no status.
                if status == 0 {
                    socket.shutdown().await.ok();
                    continue;
                }

                let (reason, body) = match status {
                    200 => ("OK", ""),
                    202 => ("Accepted", ""),
                    204 => ("No Content", ""),
                    400 => ("Bad Request", "invalid"),
                    404 => ("Not Found", "gone"),
                    409 => ("Conflict", "fenced"),
                    429 => ("Too Many Requests", "retry"),
                    500 => ("Internal Server Error", "transient"),
                    503 => ("Service Unavailable", "transient"),
                    _ => ("Status", "unexpected"),
                };
                let response = format!(
                    "HTTP/1.1 {status} {reason}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                );
                socket.write_all(response.as_bytes()).await.unwrap();
                socket.shutdown().await.ok();
            }
        });

        (format!("http://{addr}"), calls, requests)
    }

    async fn spawn_json_response_server(
        body: String,
    ) -> (String, Arc<tokio::sync::Mutex<Vec<String>>>) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let requests = Arc::new(tokio::sync::Mutex::new(Vec::new()));
        let requests_for_server = requests.clone();
        tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.unwrap();
            let mut buf = vec![0u8; 16_384];
            let mut total = 0usize;
            loop {
                let n = socket.read(&mut buf[total..]).await.unwrap();
                total += n;
                if buf[..total].windows(4).any(|window| window == b"\r\n\r\n") || n == 0 {
                    break;
                }
            }
            requests_for_server
                .lock()
                .await
                .push(String::from_utf8_lossy(&buf[..total]).to_string());
            let response = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                body.len(), body
            );
            socket.write_all(response.as_bytes()).await.unwrap();
            socket.shutdown().await.ok();
        });
        (format!("http://{addr}"), requests)
    }

    #[tokio::test]
    async fn service_lease_assignments_are_worker_scoped_and_strictly_decoded() {
        let lease_id = Uuid::new_v4();
        let body = serde_json::json!([{
            "id": lease_id,
            "runtime_profile_id": "rtp_vllm_test",
            "region": "ca-central-1",
            "minimum_replicas": 1,
            "maximum_replicas": 2,
            "maximum_p95_latency_milliseconds": 500,
            "state": "ACTIVE",
            "expires_at": "2030-01-01T00:00:00Z"
        }])
        .to_string();
        let (base, requests) = spawn_json_response_server(body).await;
        let client = ControlPlaneClient::new(base, "worker-token").unwrap();
        let assignments = client.service_lease_assignments().await.unwrap();
        assert_eq!(assignments.len(), 1);
        assert_eq!(assignments[0].id, lease_id);
        assert_eq!(assignments[0].maximum_p95_latency_milliseconds, 500);

        let requests = requests.lock().await;
        assert_eq!(requests.len(), 1);
        let request = requests[0].to_lowercase();
        assert!(request.starts_with("get /v1/worker/service-leases/active http/1.1\r\n"));
        assert!(request.contains("\r\nx-worker-token: worker-token\r\n"));
        assert!(!request.contains("authorization: bearer"));
    }

    #[tokio::test]
    async fn start_task_retries_identical_request_after_500() {
        let (base, calls, requests) = spawn_request_server(vec![500, 204]).await;
        let client = ControlPlaneClient::new(base, "worker-token").unwrap();
        let task_id = Uuid::new_v4();

        client
            .start_task(task_id, 7)
            .await
            .expect("ambiguous 500 must recover through exact owner re-entry");

        assert_eq!(calls.load(Ordering::SeqCst), 2);
        let requests = requests.lock().await;
        assert_eq!(requests.len(), 2);
        for request in requests.iter() {
            let request = request.to_lowercase();
            assert!(request.starts_with(&format!(
                "post /v1/worker/task/{task_id}/start http/1.1\r\n"
            )));
            assert!(request.contains("\r\nx-task-attempt: 7\r\n"));
            assert!(request.contains("\r\nx-worker-token: worker-token\r\n"));
        }
    }

    #[tokio::test]
    async fn start_task_does_not_retry_ownership_conflict() {
        let (base, calls, _) = spawn_request_server(vec![409, 204]).await;
        let client = ControlPlaneClient::new(base, "worker-token").unwrap();
        let err = client
            .start_task(Uuid::new_v4(), 0)
            .await
            .expect_err("409 is an ownership fence, not a transient failure");

        match err {
            ProtocolError::Status { status, .. } => assert_eq!(status, StatusCode::CONFLICT),
            other => panic!("unexpected error: {other}"),
        }
        assert_eq!(calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn start_task_persistent_5xx_retry_is_bounded() {
        let (base, calls, _) = spawn_request_server(vec![503; IDEMPOTENT_MAX_ATTEMPTS]).await;
        let client = ControlPlaneClient::new(base, "worker-token").unwrap();
        let err = client
            .start_task(Uuid::new_v4(), 0)
            .await
            .expect_err("persistent 5xx must fail after the retry bound");

        match err {
            ProtocolError::Status { status, .. } => {
                assert_eq!(status, StatusCode::SERVICE_UNAVAILABLE)
            }
            other => panic!("unexpected error: {other}"),
        }
        assert_eq!(calls.load(Ordering::SeqCst), IDEMPOTENT_MAX_ATTEMPTS);
    }

    #[tokio::test]
    async fn start_task_retries_after_response_transport_drop() {
        let (base, calls, requests) = spawn_request_server(vec![0, 204]).await;
        let client = ControlPlaneClient::new(base, "worker-token").unwrap();
        let task_id = Uuid::new_v4();

        client
            .start_task(task_id, 3)
            .await
            .expect("lost response must recover through exact start replay");

        assert_eq!(calls.load(Ordering::SeqCst), 2);
        let requests = requests.lock().await;
        assert_eq!(requests.len(), 2);
        assert_eq!(requests[0], requests[1]);
    }

    #[tokio::test]
    async fn start_task_retries_429_but_not_bad_request() {
        let (retry_base, retry_calls, _) = spawn_request_server(vec![429, 204]).await;
        let retry_client = ControlPlaneClient::new(retry_base, "worker-token").unwrap();
        retry_client
            .start_task(Uuid::new_v4(), 0)
            .await
            .expect("429 is transient");
        assert_eq!(retry_calls.load(Ordering::SeqCst), 2);

        let (fenced_base, fenced_calls, _) = spawn_request_server(vec![400, 204]).await;
        let fenced_client = ControlPlaneClient::new(fenced_base, "worker-token").unwrap();
        let err = fenced_client
            .start_task(Uuid::new_v4(), 0)
            .await
            .expect_err("400 must remain terminal");
        match err {
            ProtocolError::Status { status, .. } => assert_eq!(status, StatusCode::BAD_REQUEST),
            other => panic!("unexpected error: {other}"),
        }
        assert_eq!(fenced_calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn commit_task_preserves_body_and_path_across_shared_retry() {
        let (base, calls, requests) = spawn_request_server(vec![500, 202]).await;
        let client = ControlPlaneClient::new(base, "worker-token").unwrap();
        let task_id = Uuid::new_v4();
        let commit = TaskCommit {
            task_id,
            attempt: 4,
            result_key: "results/task.json".to_string(),
            duration_ms: 1234,
            tokens_used: 17,
            result_sha256: "ab".repeat(32),
            hardware_temp_c: Some(54.5),
            inference_backend: "candle".to_string(),
        };

        client
            .commit_task(task_id, &commit)
            .await
            .expect("shared helper must preserve commit retry semantics");

        assert_eq!(calls.load(Ordering::SeqCst), 2);
        let requests = requests.lock().await;
        assert_eq!(requests.len(), 2);
        let bodies = requests
            .iter()
            .map(|request| {
                let (headers, body) = request.split_once("\r\n\r\n").unwrap();
                let headers = headers.to_lowercase();
                assert!(headers.starts_with(&format!(
                    "post /v1/worker/task/{task_id}/commit http/1.1\r\n"
                )));
                assert!(headers.contains("\r\nx-worker-token: worker-token\r\n"));
                body
            })
            .collect::<Vec<_>>();
        assert_eq!(bodies[0], bodies[1]);
        let body: serde_json::Value = serde_json::from_str(bodies[0]).unwrap();
        assert_eq!(body["attempt"], 4);
        assert_eq!(body["result_key"], "results/task.json");
        assert_eq!(body["inference_backend"], "candle");
    }

    fn sample_fail_report() -> FailReport {
        FailReport {
            class: "internal_error".to_string(),
            message: "start acknowledgement exhausted".to_string(),
            duration_ms: 1400,
            backend: "embed".to_string(),
            model: "all-minilm-l6-v2".to_string(),
            memory: Some(crate::types::FailMemory {
                total_gb: 32.0,
                available_gb: 10.0,
                effective_gb: 8.0,
                reserved_headroom_gb: 2.0,
            }),
        }
    }

    #[tokio::test]
    async fn fail_task_retries_identical_request_after_500() {
        let (base, calls, requests) = spawn_request_server(vec![500, 200]).await;
        let client = ControlPlaneClient::new(base, "worker-token").unwrap();
        let task_id = Uuid::new_v4();

        client
            .fail_task(task_id, 6, &sample_fail_report())
            .await
            .expect("ambiguous 500 must recover through exact failure replay");

        assert_eq!(calls.load(Ordering::SeqCst), 2);
        let requests = requests.lock().await;
        assert_eq!(requests.len(), 2);
        assert_eq!(requests[0], requests[1]);
        let request = requests[0].to_lowercase();
        assert!(request.starts_with(&format!("post /v1/worker/task/{task_id}/fail http/1.1\r\n")));
        assert!(request.contains("\r\nx-task-attempt: 6\r\n"));
        assert!(request.contains("\r\nx-worker-token: worker-token\r\n"));
        let (_, body) = request.split_once("\r\n\r\n").unwrap();
        let body: serde_json::Value = serde_json::from_str(body).unwrap();
        assert_eq!(body["class"], "internal_error");
        assert_eq!(body["duration_ms"], 1400);
        assert_eq!(body["model"], "all-minilm-l6-v2");
        assert_eq!(body["memory"]["effective_gb"], 8.0);
    }

    #[tokio::test]
    async fn fail_task_retries_identical_request_after_response_drop() {
        let (base, calls, requests) = spawn_request_server(vec![0, 200]).await;
        let client = ControlPlaneClient::new(base, "worker-token").unwrap();

        client
            .fail_task(Uuid::new_v4(), 2, &sample_fail_report())
            .await
            .expect("lost failure response must recover through exact replay");

        assert_eq!(calls.load(Ordering::SeqCst), 2);
        let requests = requests.lock().await;
        assert_eq!(
            requests.as_slice(),
            [requests[0].clone(), requests[0].clone()]
        );
    }

    #[tokio::test]
    async fn fail_task_accepts_conflict_after_ambiguous_durable_release() {
        let (base, calls, requests) = spawn_request_server(vec![0, 409]).await;
        let client = ControlPlaneClient::new(base, "worker-token").unwrap();

        client
            .fail_task(Uuid::new_v4(), 2, &sample_fail_report())
            .await
            .expect("a replay fence means the ambiguous first fail already ended local ownership");

        assert_eq!(calls.load(Ordering::SeqCst), 2);
        let requests = requests.lock().await;
        assert_eq!(
            requests.as_slice(),
            [requests[0].clone(), requests[0].clone()]
        );
    }

    #[tokio::test]
    async fn fail_task_persistent_5xx_retry_is_bounded() {
        let (base, calls, _) = spawn_request_server(vec![503; IDEMPOTENT_MAX_ATTEMPTS]).await;
        let client = ControlPlaneClient::new(base, "worker-token").unwrap();
        let err = client
            .fail_task(Uuid::new_v4(), 0, &sample_fail_report())
            .await
            .expect_err("persistent fail_task 5xx must stop at the retry bound");

        match err {
            ProtocolError::Status { status, .. } => {
                assert_eq!(status, StatusCode::SERVICE_UNAVAILABLE)
            }
            other => panic!("unexpected error: {other}"),
        }
        assert_eq!(calls.load(Ordering::SeqCst), IDEMPOTENT_MAX_ATTEMPTS);
    }

    #[tokio::test]
    async fn fail_task_accepts_release_compatible_terminal_statuses() {
        for status in [409, 404] {
            let (base, calls, _) = spawn_request_server(vec![status, 200]).await;
            let client = ControlPlaneClient::new(base, "worker-token").unwrap();
            client
                .fail_task(Uuid::new_v4(), 0, &sample_fail_report())
                .await
                .unwrap_or_else(|error| panic!("{status} must end stale ownership: {error}"));
            assert_eq!(calls.load(Ordering::SeqCst), 1);
        }
    }

    #[tokio::test]
    async fn fail_task_does_not_retry_bad_request() {
        let (base, calls, _) = spawn_request_server(vec![400, 200]).await;
        let client = ControlPlaneClient::new(base, "worker-token").unwrap();
        let err = client
            .fail_task(Uuid::new_v4(), 0, &sample_fail_report())
            .await
            .expect_err("malformed failure reports must remain terminal");

        match err {
            ProtocolError::Status { status, .. } => assert_eq!(status, StatusCode::BAD_REQUEST),
            other => panic!("unexpected error: {other}"),
        }
        assert_eq!(calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn fail_task_retries_429() {
        let (base, calls, _) = spawn_request_server(vec![429, 200]).await;
        let client = ControlPlaneClient::new(base, "worker-token").unwrap();

        client
            .fail_task(Uuid::new_v4(), 0, &sample_fail_report())
            .await
            .expect("rate limiting is transient");

        assert_eq!(calls.load(Ordering::SeqCst), 2);
    }
}

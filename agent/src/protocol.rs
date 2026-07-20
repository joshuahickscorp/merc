use std::time::Duration;

use reqwest::{Client, Response, StatusCode};
use uuid::Uuid;

use crate::types::{
    ConnectStatus, Earnings, FailReport, Heartbeat, SupplierVerification, TaskCommit, TaskDispatch,
    WorkerCapability,
};

const POLL_TIMEOUT: Duration = Duration::from_secs(35);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);

const COMMIT_MAX_ATTEMPTS: usize = 4;
const COMMIT_RETRY_BASE_DELAY: Duration = Duration::from_millis(200);

const POLL_PATH: &str = "/v1/worker/poll?wait_ms=25000";

#[derive(Debug, thiserror::Error)]
pub enum ProtocolError {
    #[error("worker token is empty; refusing to send unauthenticated request")]
    MissingToken,
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

pub struct ControlPlaneClient {
    http: Client,
    base_url: String,
    token: String,
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
        let http = Client::builder()
            .timeout(REQUEST_TIMEOUT)
            .user_agent(concat!("cx-agent/", env!("CARGO_PKG_VERSION")))
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

    pub async fn start_task(&self, task_id: Uuid) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/task/{id}/start";
        let path = format!("/v1/worker/task/{task_id}/start");
        let resp = self
            .http
            .post(self.url(&path))
            .header("X-Worker-Token", &self.token)
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        Self::expect_status(endpoint, resp, &[StatusCode::NO_CONTENT, StatusCode::OK]).await?;
        Ok(())
    }

    pub async fn commit_task(
        &self,
        task_id: Uuid,
        commit: &TaskCommit,
    ) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/task/{id}/commit";
        let path = format!("/v1/worker/task/{task_id}/commit");
        let mut delay = COMMIT_RETRY_BASE_DELAY;

        for attempt in 0..COMMIT_MAX_ATTEMPTS {
            let sent = self
                .http
                .post(self.url(&path))
                .header("X-Worker-Token", &self.token)
                .json(commit)
                .send()
                .await;

            match sent {
                Ok(resp) if resp.status().is_success() => return Ok(()),
                Ok(resp) => {
                    let status = resp.status();
                    let retryable =
                        status == StatusCode::TOO_MANY_REQUESTS || status.is_server_error();
                    if retryable && attempt + 1 < COMMIT_MAX_ATTEMPTS {
                        tracing::warn!(
                            attempt = attempt + 1,
                            max_attempts = COMMIT_MAX_ATTEMPTS,
                            %status,
                            delay_ms = delay.as_millis(),
                            "commit_task: transient status, retrying identical commit"
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
                    if attempt + 1 == COMMIT_MAX_ATTEMPTS {
                        return Err(Self::transport(endpoint, err));
                    }
                    tracing::warn!(
                        attempt = attempt + 1,
                        max_attempts = COMMIT_MAX_ATTEMPTS,
                        error = %err,
                        delay_ms = delay.as_millis(),
                        "commit_task: transport failure, retrying identical commit"
                    );
                    tokio::time::sleep(delay).await;
                    delay *= 2;
                }
            }
        }

        unreachable!("bounded commit retry loop always returns")
    }

    pub async fn fail_task(&self, task_id: Uuid, report: &FailReport) -> Result<(), ProtocolError> {
        let endpoint = "/v1/worker/task/{id}/fail";
        let path = format!("/v1/worker/task/{task_id}/fail");
        let resp = self
            .http
            .post(self.url(&path))
            .header("X-Worker-Token", &self.token)
            .json(report)
            .send()
            .await
            .map_err(|e| Self::transport(endpoint, e))?;
        Self::expect_status(endpoint, resp, &[StatusCode::NO_CONTENT, StatusCode::OK]).await?;
        Ok(())
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

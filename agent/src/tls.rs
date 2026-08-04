use anyhow::{Context, Result};

pub fn client_builder() -> Result<reqwest::ClientBuilder> {
    let configured_ca = std::env::var("MERC_TLS_CA_FILE").ok();
    client_builder_with_ca(configured_ca.as_deref())
}

fn client_builder_with_ca(configured_ca: Option<&str>) -> Result<reqwest::ClientBuilder> {
    let mut builder = reqwest::Client::builder();
    if let Some(path) = configured_ca {
        if !path.trim().is_empty() {
            let pem = std::fs::read(path).with_context(|| "reading configured TLS CA file")?;
            if !pem
                .windows(b"-----BEGIN CERTIFICATE-----".len())
                .any(|part| part == b"-----BEGIN CERTIFICATE-----")
                || !pem
                    .windows(b"-----END CERTIFICATE-----".len())
                    .any(|part| part == b"-----END CERTIFICATE-----")
            {
                anyhow::bail!("configured TLS CA file does not contain a PEM certificate");
            }
            let certificate = reqwest::Certificate::from_pem(&pem)
                .context("configured TLS CA file is not a valid PEM certificate")?;
            builder = builder.add_root_certificate(certificate);
        }
    }
    // Seatbelt confines the process to loopback; remote HTTPS goes through the
    // allowlisted CONNECT proxy the unsandboxed parent spawned (MERC_EGRESS_PROXY).
    // reqwest is built with default-features=false so system-proxy autodetection
    // is off — wire the proxy explicitly when present.
    //
    // The proxy is CONNECT-only. Plain HTTP POSTs sent to it as absolute-form
    // forward-proxy requests get "405 Method Not Allowed". That is correct for
    // remote peers (they are HTTPS and use CONNECT), but fatal for loopback:
    // httptest control planes and local engines are HTTP on 127.0.0.1:port, and
    // seatbelt already permits loopback. The re-exec path sets NO_PROXY for the
    // same reason; honour it here (and hard-code the loopback set as a floor so
    // a unit test or a partial env still cannot route localhost through the
    // CONNECT proxy).
    if let Ok(proxy_url) = std::env::var(crate::sandbox_egress::MERC_EGRESS_PROXY_ENV) {
        let proxy_url = proxy_url.trim();
        if !proxy_url.is_empty() {
            let proxy = reqwest::Proxy::all(proxy_url)
                .with_context(|| format!("parsing {proxy_url} as HTTP proxy"))?
                .no_proxy(loopback_no_proxy());
            builder = builder.proxy(proxy);
        }
    }
    Ok(builder)
}

/// Hosts that must never go through the seatbelt egress proxy.
///
/// Combines the process NO_PROXY (set at re-exec) with a hard-coded loopback
/// floor. Either alone is insufficient: without the env honour, operators who
/// extend NO_PROXY lose that extension; without the floor, a missing NO_PROXY
/// (or an explicit Proxy::all used from a test harness) sends 127.0.0.1 through
/// a CONNECT-only proxy and registration dies with a 405 that looks like it
/// came from /v1/worker/register.
fn loopback_no_proxy() -> Option<reqwest::NoProxy> {
    const LOOPBACK: &str = "localhost,127.0.0.1,::1";
    let merged = match std::env::var("NO_PROXY").or_else(|_| std::env::var("no_proxy")) {
        Ok(env) if !env.trim().is_empty() => format!("{env},{LOOPBACK}"),
        _ => LOOPBACK.to_string(),
    };
    reqwest::NoProxy::from_string(&merged)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::net::TcpListener;
    use std::sync::{Mutex, OnceLock};
    use std::thread;

    /// Serialise tests that mutate process-wide proxy env vars.
    fn env_lock() -> std::sync::MutexGuard<'static, ()> {
        static LOCK: OnceLock<Mutex<()>> = OnceLock::new();
        LOCK.get_or_init(|| Mutex::new(())).lock().unwrap()
    }

    #[test]
    fn invalid_custom_ca_fails_closed() {
        let path = std::env::temp_dir().join(format!("merc-invalid-ca-{}.pem", std::process::id()));
        std::fs::write(&path, b"not a certificate").unwrap();
        let result = client_builder_with_ca(path.to_str());
        let _ = std::fs::remove_file(path);
        assert!(result.is_err());
    }

    /// The bug this test locks: with MERC_EGRESS_PROXY set, a POST to a
    /// loopback control plane must not be forwarded to the CONNECT-only proxy
    /// (which answers 405 for non-CONNECT). It must reach the real server.
    #[tokio::test]
    async fn loopback_http_bypasses_connect_only_egress_proxy() {
        let _guard = env_lock();

        // A fake CONNECT-only proxy: any non-CONNECT gets 405, matching
        // sandbox_egress. If the client wrongly routes loopback through it,
        // the POST below returns 405 and this test fails.
        let proxy_listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let proxy_port = proxy_listener.local_addr().unwrap().port();
        thread::spawn(move || {
            if let Ok((mut stream, _)) = proxy_listener.accept() {
                let mut buf = [0u8; 512];
                let _ = stream.read(&mut buf);
                let _ = stream
                    .write_all(b"HTTP/1.1 405 Method Not Allowed\r\nConnection: close\r\n\r\n");
            }
        });

        // A real loopback origin that accepts POST /v1/worker/register.
        let origin = TcpListener::bind("127.0.0.1:0").unwrap();
        let origin_port = origin.local_addr().unwrap().port();
        thread::spawn(move || {
            if let Ok((mut stream, _)) = origin.accept() {
                let mut buf = [0u8; 1024];
                let n = stream.read(&mut buf).unwrap_or(0);
                let req = String::from_utf8_lossy(&buf[..n]);
                assert!(
                    req.starts_with("POST /v1/worker/register"),
                    "origin saw unexpected request line: {req}"
                );
                let body = b"{\"ok\":true}";
                let _ = write!(
                    stream,
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                let _ = stream.write_all(body);
            }
        });

        let proxy_url = format!("http://127.0.0.1:{proxy_port}");
        // SAFETY: serialised by env_lock; restored below. Process-wide env is
        // the only way client_builder reads the proxy today.
        unsafe {
            std::env::set_var(crate::sandbox_egress::MERC_EGRESS_PROXY_ENV, &proxy_url);
            std::env::remove_var("NO_PROXY");
            std::env::remove_var("no_proxy");
        }

        let client = client_builder().expect("builder").build().expect("client");
        let url = format!("http://127.0.0.1:{origin_port}/v1/worker/register");
        let resp = client
            .post(&url)
            .header("X-Worker-Token", "test-token")
            .body("{}")
            .send()
            .await;

        unsafe {
            std::env::remove_var(crate::sandbox_egress::MERC_EGRESS_PROXY_ENV);
        }

        let resp = resp.expect("request to loopback origin must succeed without proxy");
        assert_eq!(
            resp.status(),
            reqwest::StatusCode::OK,
            "loopback POST must not be answered 405 by the CONNECT-only proxy"
        );
        let body = resp.text().await.expect("body");
        assert!(body.contains("ok"), "unexpected body: {body}");
    }
}

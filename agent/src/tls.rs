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
    if let Ok(proxy_url) = std::env::var(crate::sandbox_egress::MERC_EGRESS_PROXY_ENV) {
        let proxy_url = proxy_url.trim();
        if !proxy_url.is_empty() {
            let proxy = reqwest::Proxy::all(proxy_url)
                .with_context(|| format!("parsing {proxy_url} as HTTP proxy"))?;
            builder = builder.proxy(proxy);
        }
    }
    Ok(builder)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn invalid_custom_ca_fails_closed() {
        let path = std::env::temp_dir().join(format!("merc-invalid-ca-{}.pem", std::process::id()));
        std::fs::write(&path, b"not a certificate").unwrap();
        let result = client_builder_with_ca(path.to_str());
        let _ = std::fs::remove_file(path);
        assert!(result.is_err());
    }
}

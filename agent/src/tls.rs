use anyhow::{Context, Result};

pub fn client_builder() -> Result<reqwest::ClientBuilder> {
    let mut builder = reqwest::Client::builder();
    if let Ok(path) = std::env::var("MERC_TLS_CA_FILE") {
        if !path.trim().is_empty() {
            let pem = std::fs::read(&path).with_context(|| "reading configured TLS CA file")?;
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
    Ok(builder)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn invalid_custom_ca_fails_closed() {
        let path = std::env::temp_dir().join(format!("merc-invalid-ca-{}.pem", std::process::id()));
        std::fs::write(&path, b"not a certificate").unwrap();
        std::env::set_var("MERC_TLS_CA_FILE", &path);
        let result = client_builder();
        std::env::remove_var("MERC_TLS_CA_FILE");
        let _ = std::fs::remove_file(path);
        assert!(result.is_err());
    }
}

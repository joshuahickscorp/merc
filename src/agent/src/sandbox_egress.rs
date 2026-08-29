//! Local allowlisted CONNECT proxy for seatbelt-confined merc-agent.
//!
//! Public `sandbox-exec` on modern macOS only accepts `*` or `localhost` as
//! network hosts — specific IPs and DNS names fail profile compilation. The
//! seatbelt profile therefore allows only DNS + loopback; remote HTTPS is
//! reached by this proxy, which runs *outside* the sandbox and enforces the
//! host allowlist the profile cannot express.
//!
//! Lifecycle: the unsandboxed parent spawns `merc-agent sandbox-egress-proxy`
//! as a sibling OS process, sets `MERC_EGRESS_PROXY`, then re-execs under
//! seatbelt. The proxy process survives the re-exec and serves CONNECT
//! tunnels for allowlisted `host:port` peers only.

use std::collections::BTreeSet;
use std::io::{self, Read, Write};
use std::net::{TcpListener, TcpStream, ToSocketAddrs};
use std::process::{Child, Command, Stdio};
use std::thread;
use std::time::Duration;

/// Env var the sandboxed agent and its HTTP clients read for the proxy URL.
pub const MERC_EGRESS_PROXY_ENV: &str = "MERC_EGRESS_PROXY";

/// Spawn the allowlisted CONNECT proxy as a sibling process of `exe`.
/// Returns `(child, proxy_base_url)`.
pub fn spawn_proxy_process(
    exe: &std::path::Path,
    allowlist: &BTreeSet<String>,
) -> io::Result<(Child, String)> {
    let mut cmd = Command::new(exe);
    cmd.arg("sandbox-egress-proxy");
    for host in allowlist {
        cmd.arg("--allow").arg(host);
    }
    cmd.stdout(Stdio::piped()).stderr(Stdio::inherit());
    let mut child = cmd.spawn()?;
    let mut stdout = child
        .stdout
        .take()
        .ok_or_else(|| io::Error::other("proxy stdout missing"))?;
    // The proxy prints one line: the base URL, then serves forever.
    let mut line = String::new();
    let mut buf = [0u8; 1];
    loop {
        let n = stdout.read(&mut buf)?;
        if n == 0 {
            break;
        }
        if buf[0] == b'\n' {
            break;
        }
        line.push(buf[0] as char);
        if line.len() > 256 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "proxy url too long",
            ));
        }
    }
    let url = line.trim().to_string();
    if !url.starts_with("http://127.0.0.1:") && !url.starts_with("http://localhost:") {
        // Reap a failed child so we do not leave a zombie.
        let _ = child.kill();
        let _ = child.wait();
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            format!("egress proxy did not print a loopback URL (got {url:?})"),
        ));
    }
    // Keep reading stdout on a detached thread so the pipe cannot fill.
    thread::spawn(move || {
        let mut sink = Vec::new();
        let _ = stdout.read_to_end(&mut sink);
    });
    Ok((child, url))
}

/// Entry point for `merc-agent sandbox-egress-proxy --allow host:port ...`.
/// Prints the listening URL on stdout, then serves until killed.
pub fn run_proxy_main(allows: Vec<String>) -> io::Result<()> {
    let mut set = BTreeSet::new();
    for a in allows {
        let n = normalize_authority(&a);
        if !n.is_empty() {
            set.insert(n);
        }
    }
    if set.is_empty() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "sandbox-egress-proxy requires at least one --allow host:port",
        ));
    }
    let listener = TcpListener::bind("127.0.0.1:0")?;
    let port = listener.local_addr()?.port();
    // Single line, flush before accept loop so the parent can read it.
    println!("http://127.0.0.1:{port}");
    let _ = std::io::stdout().flush();
    let allow = std::sync::Arc::new(set);
    accept_loop(listener, allow)
}

fn accept_loop(listener: TcpListener, allow: std::sync::Arc<BTreeSet<String>>) -> io::Result<()> {
    loop {
        match listener.accept() {
            Ok((stream, _)) => {
                let allow = std::sync::Arc::clone(&allow);
                let _ = thread::Builder::new()
                    .name("merc-egress-conn".into())
                    .spawn(move || {
                        if let Err(err) = handle_client(stream, &allow) {
                            // Best-effort log to stderr; parent may have tracing.
                            eprintln!("merc-egress-proxy: {err}");
                        }
                    });
            }
            Err(err) => {
                eprintln!("merc-egress-proxy accept: {err}");
                thread::sleep(Duration::from_millis(50));
            }
        }
    }
}

fn handle_client(mut client: TcpStream, allow: &BTreeSet<String>) -> io::Result<()> {
    client.set_read_timeout(Some(Duration::from_secs(30)))?;
    client.set_write_timeout(Some(Duration::from_secs(30)))?;

    let mut buf = Vec::with_capacity(1024);
    let mut tmp = [0u8; 512];
    loop {
        let n = client.read(&mut tmp)?;
        if n == 0 {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "client closed",
            ));
        }
        buf.extend_from_slice(&tmp[..n]);
        if buf.windows(4).any(|w| w == b"\r\n\r\n") {
            break;
        }
        if buf.len() > 16 * 1024 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "request too large",
            ));
        }
    }

    let header_end = buf
        .windows(4)
        .position(|w| w == b"\r\n\r\n")
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "no header terminator"))?;
    let header = std::str::from_utf8(&buf[..header_end])
        .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
    let request_line = header.lines().next().unwrap_or("");
    let mut parts = request_line.split_whitespace();
    let method = parts.next().unwrap_or("");
    let target = parts.next().unwrap_or("");

    if !method.eq_ignore_ascii_case("CONNECT") {
        let _ = client.write_all(b"HTTP/1.1 405 Method Not Allowed\r\nConnection: close\r\n\r\n");
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "egress proxy only accepts CONNECT",
        ));
    }

    let authority = normalize_authority(target);
    if !allow_contains(allow, &authority) {
        eprintln!("REFUSED_EGRESS_HOST: {authority}");
        let _ = client.write_all(b"HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n");
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            format!("REFUSED_EGRESS_HOST: {authority}"),
        ));
    }

    let upstream = connect_authority(&authority)?;
    client.write_all(b"HTTP/1.1 200 Connection Established\r\n\r\n")?;

    client.set_read_timeout(None)?;
    client.set_write_timeout(None)?;
    upstream.set_read_timeout(None)?;
    upstream.set_write_timeout(None)?;
    tunnel(client, upstream)
}

fn normalize_authority(raw: &str) -> String {
    let raw = raw.trim().trim_matches(|c| c == '[' || c == ']');
    if raw.is_empty() {
        return String::new();
    }
    if let Some((host, port)) = raw.rsplit_once(':') {
        // IPv6 bare forms are not used; host:port is the seatbelt contract.
        if !port.is_empty() && port.chars().all(|c| c.is_ascii_digit()) {
            return format!("{}:{port}", host.to_ascii_lowercase());
        }
    }
    format!("{}:443", raw.to_ascii_lowercase())
}

fn allow_contains(allow: &BTreeSet<String>, authority: &str) -> bool {
    if allow.contains(authority) {
        return true;
    }
    let host = authority
        .rsplit_once(':')
        .map(|(h, _)| h)
        .unwrap_or(authority);
    allow.iter().any(|entry| {
        let e = entry.to_ascii_lowercase();
        e == authority || e == host || e == format!("{host}:443") || e == format!("{host}:80")
    })
}

fn connect_authority(authority: &str) -> io::Result<TcpStream> {
    let mut last = io::Error::new(io::ErrorKind::NotFound, "no address");
    for addr in authority.to_socket_addrs()? {
        match TcpStream::connect_timeout(&addr, Duration::from_secs(15)) {
            Ok(s) => return Ok(s),
            Err(e) => last = e,
        }
    }
    Err(last)
}

fn tunnel(a: TcpStream, b: TcpStream) -> io::Result<()> {
    let a_reader = a.try_clone()?;
    let mut a_writer = a;
    let b_reader = b.try_clone()?;
    let mut b_writer = b;
    let forward = thread::spawn(move || {
        let _ = io::copy(&mut &a_reader, &mut b_writer);
        let _ = b_writer.shutdown(std::net::Shutdown::Both);
    });
    let _ = io::copy(&mut &b_reader, &mut a_writer);
    let _ = a_writer.shutdown(std::net::Shutdown::Both);
    let _ = forward.join();
    Ok(())
}

/// Build the allowlist of `host:port` peers the sandboxed agent may reach.
pub fn build_allowlist(
    control: &str,
    artifact: &str,
    model: &str,
    sentinel: &str,
) -> BTreeSet<String> {
    let mut set = BTreeSet::new();
    for raw in [control, artifact, model] {
        let t = raw.trim();
        if t.is_empty() || t == sentinel {
            continue;
        }
        let n = normalize_authority(t);
        if !n.is_empty() {
            set.insert(n);
        }
    }
    set.insert("127.0.0.1:8080".into());
    set.insert("localhost:8080".into());
    set
}

/// Parse a URL or bare host:port into the authority form the allowlist uses.
pub fn host_port_from_url(raw: &str) -> Option<String> {
    let raw = raw.trim();
    if raw.is_empty() {
        return None;
    }
    let without_scheme = raw
        .strip_prefix("https://")
        .or_else(|| raw.strip_prefix("http://"))
        .unwrap_or(raw);
    let hostport = without_scheme.split('/').next().unwrap_or(without_scheme);
    if hostport.is_empty() {
        return None;
    }
    let n = normalize_authority(hostport);
    if n.is_empty() {
        None
    } else {
        Some(n)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn host_port_from_url_https_default() {
        assert_eq!(
            host_port_from_url("https://api.example.com/v1").as_deref(),
            Some("api.example.com:443")
        );
    }

    #[test]
    fn host_port_from_url_http_with_port() {
        assert_eq!(
            host_port_from_url("http://127.0.0.1:8080/").as_deref(),
            Some("127.0.0.1:8080")
        );
    }

    #[test]
    fn allowlist_skips_sentinel() {
        let set = build_allowlist("api.example.com:443", "0.0.0.0:0", "", "0.0.0.0:0");
        assert!(set.contains("api.example.com:443"));
        assert!(!set.iter().any(|e| e.starts_with("0.0.0.0")));
    }

    #[test]
    fn proxy_allows_listed_and_refuses_other() {
        let upstream = TcpListener::bind("127.0.0.1:0").unwrap();
        let upstream_port = upstream.local_addr().unwrap().port();
        thread::spawn(move || {
            if let Ok((mut s, _)) = upstream.accept() {
                let mut buf = [0u8; 64];
                let _ = s.read(&mut buf);
                let _ = s.write_all(b"hello-from-upstream");
            }
        });

        let mut allow = BTreeSet::new();
        allow.insert(format!("127.0.0.1:{upstream_port}"));
        // In-process accept loop for the unit test (no subprocess).
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let proxy_port = listener.local_addr().unwrap().port();
        let allow_arc = std::sync::Arc::new(allow);
        thread::spawn(move || {
            let _ = accept_loop(listener, allow_arc);
        });
        thread::sleep(Duration::from_millis(50));

        let mut c = TcpStream::connect(("127.0.0.1", proxy_port)).unwrap();
        write!(
            c,
            "CONNECT 127.0.0.1:{upstream_port} HTTP/1.1\r\nHost: 127.0.0.1:{upstream_port}\r\n\r\n"
        )
        .unwrap();
        let mut resp = [0u8; 128];
        let n = c.read(&mut resp).unwrap();
        let head = String::from_utf8_lossy(&resp[..n]);
        assert!(
            head.starts_with("HTTP/1.1 200"),
            "expected 200 for allowlisted host, got {head}"
        );

        let mut c2 = TcpStream::connect(("127.0.0.1", proxy_port)).unwrap();
        write!(
            c2,
            "CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\n\r\n"
        )
        .unwrap();
        let mut resp2 = [0u8; 128];
        let n2 = c2.read(&mut resp2).unwrap();
        let head2 = String::from_utf8_lossy(&resp2[..n2]);
        assert!(
            head2.starts_with("HTTP/1.1 403"),
            "expected 403 for non-allowlisted host, got {head2}"
        );
    }
}

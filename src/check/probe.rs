//! Concurrent per-domain DNS (`127.0.0.1:53535`), macOS (`dscacheutil`), HTTP, and HTTPS probes.

use std::collections::HashMap;
use std::io::{self, BufRead, BufReader, Write as _};
use std::net::{IpAddr, Ipv4Addr, SocketAddr, TcpStream, UdpSocket};
use std::sync::Arc;
use std::thread;
use std::time::{Duration, Instant};

use rustls::pki_types::ServerName;
use rustls::{ClientConfig, ClientConnection, RootCertStore, StreamOwned};
use rustls_platform_verifier::Verifier;
use serde::Serialize;

use crate::store::Domain;

use super::{Env, enabled, join_host_port, one_line, read_file_str};

const DIRECT_WINDOW: Duration = Duration::from_secs(2);
const RETRY_EVERY: Duration = Duration::from_millis(100);
const SYSTEM_TIMEOUT: Duration = Duration::from_secs(6);
const HTTP_WINDOW: Duration = Duration::from_secs(3);
const LOCAL_TIMEOUT: Duration = Duration::from_millis(500);

pub(crate) const REMOVE_LINE: &str = ": remove that line";
pub(crate) const RUN_SETUP: &str = ": run lodo setup";
pub(crate) const RUN_DEV: &str = ": start it with lodo dev in its project";

/// Outcome of probing a single enabled domain.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize)]
pub struct ProbeResult {
    pub name: String,
    pub address: String,
    pub port: u16,
    pub direct: bool,
    pub system: bool,
    pub http: bool,
    pub secure: bool,
    pub https: bool,
    pub detail: String,
    pub down: bool,
}

impl ProbeResult {
    pub fn ok(&self) -> bool {
        self.direct && self.system && (self.port == 0 || self.http) && (!self.secure || self.https)
    }
}

impl Env<'_> {
    /// Probes every enabled domain concurrently (direct UDP DNS, `dscacheutil`, HTTP, and HTTPS).
    pub fn probe(&self, domains: &[Domain]) -> Vec<ProbeResult> {
        let on = enabled(domains);
        if on.is_empty() {
            return Vec::new();
        }
        let hosts_content = read_file_str(&self.paths.hosts).unwrap_or_default();
        let entries = hosts_addresses(&hosts_content);
        let deadline = self.timeout.map(|d| Instant::now() + d);

        let pairs: Vec<(ProbeResult, bool)> = thread::scope(|s| {
            let handles: Vec<_> = on
                .iter()
                .map(|d| {
                    let in_hosts = entries.get(&d.name).map(Vec::as_slice).unwrap_or(&[]);
                    s.spawn(move || self.probe_one(d, in_hosts, deadline))
                })
                .collect();
            handles
                .into_iter()
                .map(|h| h.join().unwrap_or_else(|_| (ProbeResult::default(), false)))
                .collect()
        });

        let mut results = Vec::with_capacity(pairs.len());
        let mut stopped = Vec::with_capacity(pairs.len());
        for (res, is_stopped) in pairs {
            results.push(res);
            stopped.push(is_stopped);
        }

        let mut elsewhere: HashMap<u16, String> = HashMap::new();
        for (i, d) in on.iter().enumerate() {
            if !stopped[i] {
                continue;
            }
            let wrong = elsewhere
                .entry(d.port)
                .or_insert_with(|| self.probe_localhost(d, deadline))
                .clone();
            if !wrong.is_empty() {
                results[i].detail = results[i].detail.replacen(&app_down(d), &wrong, 1);
                results[i].down = false;
            }
        }
        results
    }

    fn probe_one(
        &self,
        d: &Domain,
        in_hosts: &[String],
        deadline: Option<Instant>,
    ) -> (ProbeResult, bool) {
        let direct = self.probe_direct(&d.name, &d.address, deadline);
        let sys = self.probe_system(&d.name, &d.address, deadline);
        let mut lookups = vec![direct.clone(), sys.clone()];
        if (!direct.is_empty() || !sys.is_empty())
            && !in_hosts.is_empty()
            && !in_hosts.iter().any(|ip| ip == &d.address)
        {
            lookups = vec![format!(
                "{} maps {} to {}{REMOVE_LINE}",
                self.paths.hosts.display(),
                d.name,
                in_hosts.join(", ")
            )];
        }

        let (mut web, mut secure) = thread::scope(|s| {
            let web_h = (d.port > 0).then(|| s.spawn(|| self.probe_http(d, deadline)));
            let sec_h = d.https.then(|| s.spawn(|| self.probe_https(d, deadline)));
            let w = web_h.and_then(|h| h.join().ok()).unwrap_or_default();
            let sec = sec_h.and_then(|h| h.join().ok()).unwrap_or_default();
            (w, sec)
        });

        let mut r = ProbeResult {
            name: d.name.clone(),
            address: d.address.clone(),
            port: d.port,
            direct: direct.is_empty(),
            system: sys.is_empty(),
            http: d.port > 0 && web.is_empty(),
            secure: d.https,
            https: d.https && secure.is_empty(),
            detail: String::new(),
            down: false,
        };

        let mut stopped = web == app_down(d);
        if stopped {
            if secure == format!("https: {web}") {
                secure.clear();
            }
            if listens(&d.address, d.port, deadline) {
                web = bad_answer(d);
                stopped = false;
            } else {
                r.down = r.direct && r.system && secure.is_empty();
            }
        }

        lookups.push(web);
        lookups.push(secure);
        lookups.retain(|s| !s.is_empty());
        r.detail = lookups.join("; ");
        (r, stopped)
    }

    fn probe_direct(&self, name: &str, want: &str, outer_deadline: Option<Instant>) -> String {
        let win_end = cap_deadline(Instant::now() + DIRECT_WINDOW, outer_deadline);
        if Instant::now() >= win_end {
            return "dnsmasq: no answer".to_string();
        }
        let Ok(target) = self.dns.parse::<SocketAddr>() else {
            return "dnsmasq: no answer".to_string();
        };
        let Ok(sock) = UdpSocket::bind("127.0.0.1:0") else {
            return "dnsmasq: no answer".to_string();
        };
        let query = build_dns_a_query(name, 0x4C4F);
        let mut problem = String::new();

        loop {
            let now = Instant::now();
            if now >= win_end {
                return if problem.is_empty() {
                    "dnsmasq: no answer".to_string()
                } else {
                    problem
                };
            }
            let wait = (win_end - now).min(Duration::from_millis(150));
            let _ = sock.set_read_timeout(Some(wait));
            let _ = sock.set_write_timeout(Some(wait));

            let attempt = (|| -> io::Result<Vec<Ipv4Addr>> {
                sock.send_to(&query, target)?;
                let mut buf = [0u8; 1500];
                let (n, _) = sock.recv_from(&mut buf)?;
                parse_dns_a_reply(&buf[..n], 0x4C4F)
            })();

            match attempt {
                Ok(addrs) if addrs.iter().any(|ip| ip.to_string() == want) => return String::new(),
                Ok(addrs) => {
                    if problem.is_empty() || Instant::now() < win_end {
                        problem = if addrs.is_empty() {
                            "dnsmasq: no answer".to_string()
                        } else {
                            let got: Vec<String> = addrs.iter().map(Ipv4Addr::to_string).collect();
                            format!("dnsmasq: answered {}, want {want}", got.join(", "))
                        };
                    }
                }
                Err(_) => {
                    if problem.is_empty() {
                        problem = "dnsmasq: no answer".to_string();
                    }
                }
            }

            let now = Instant::now();
            if now >= win_end {
                return problem;
            }
            thread::sleep(RETRY_EVERY.min(win_end - now));
        }
    }

    fn probe_system(&self, name: &str, want: &str, outer_deadline: Option<Instant>) -> String {
        let limit = match outer_deadline {
            Some(dl) => SYSTEM_TIMEOUT.min(
                dl.saturating_duration_since(Instant::now())
                    .max(Duration::from_nanos(1)),
            ),
            None => SYSTEM_TIMEOUT,
        };
        let bin = self.paths.dscacheutil.to_string_lossy();
        let out = self
            .runner
            .run(Some(limit), &bin, &["-q", "host", "-a", "name", name])
            .unwrap_or_default();
        let addrs = ip_addresses(&out);
        if addrs.iter().any(|a| a == want) {
            String::new()
        } else if addrs.is_empty() {
            "macOS: no address".to_string()
        } else {
            format!("macOS: {}, want {want}", addrs.join(", "))
        }
    }

    fn probe_http(&self, d: &Domain, outer_deadline: Option<Instant>) -> String {
        self.probe_web(d, self.http_port, None, outer_deadline)
    }

    fn probe_https(&self, d: &Domain, outer_deadline: Option<Instant>) -> String {
        let tls_cfg = match build_tls_config(self.root_cas.as_ref()) {
            Ok(cfg) => Arc::new(cfg),
            Err(e) => return format!("https: {}", one_line(&e)),
        };
        let p = self.probe_web(d, self.https_port, Some(tls_cfg), outer_deadline);
        if p.is_empty() {
            String::new()
        } else {
            format!("https: {p}")
        }
    }

    fn probe_web(
        &self,
        d: &Domain,
        port: u16,
        tls_cfg: Option<Arc<ClientConfig>>,
        outer_deadline: Option<Instant>,
    ) -> String {
        let win_end = cap_deadline(Instant::now() + HTTP_WINDOW, outer_deadline);
        let caddy_addr = join_host_port(&d.address, port);
        let mut problem = format!("nothing answers on {caddy_addr}: is Caddy running?");

        let Ok(ip) = d.address.parse::<IpAddr>() else {
            return format!("invalid address {}", d.address);
        };
        let sock_addr = SocketAddr::new(ip, port);

        loop {
            let now = Instant::now();
            if now >= win_end {
                return problem;
            }
            let remaining = win_end - now;
            let connect_wait = remaining.min(Duration::from_millis(500));

            match TcpStream::connect_timeout(&sock_addr, connect_wait) {
                Ok(tcp) => {
                    let _ = tcp.set_read_timeout(Some(remaining));
                    let _ = tcp.set_write_timeout(Some(remaining));
                    let exchange = if let Some(ref cfg) = tls_cfg {
                        do_https_exchange(tcp, Arc::clone(cfg), &d.name)
                    } else {
                        do_http_exchange(tcp, &d.name)
                    };
                    match exchange {
                        Ok((status, headers)) => return http_problem(status, &headers, d, port),
                        Err(WebError::UntrustedCert) => {
                            return format!("Caddy's certificate isn't trusted{RUN_SETUP}");
                        }
                        Err(WebError::Other(msg)) => {
                            if Instant::now() < win_end {
                                problem = one_line(&msg);
                            }
                        }
                    }
                }
                Err(_) => {
                    // Refused / timed-out TCP dial keeps the default "nothing answers..." message.
                }
            }

            let now = Instant::now();
            if now >= win_end {
                return problem;
            }
            thread::sleep(RETRY_EVERY.min(win_end - now));
        }
    }

    fn probe_localhost(&self, d: &Domain, outer_deadline: Option<Instant>) -> String {
        for host in &self.localhost {
            if host != &d.address && listens(host, d.port, outer_deadline) {
                return wrong_host(host, d.port);
            }
        }
        String::new()
    }
}

enum WebError {
    UntrustedCert,
    Other(String),
}

fn build_tls_config(custom_roots: Option<&Arc<RootCertStore>>) -> Result<ClientConfig, String> {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let builder = ClientConfig::builder_with_provider(Arc::clone(&provider))
        .with_protocol_versions(&[&rustls::version::TLS12, &rustls::version::TLS13])
        .map_err(|e| e.to_string())?;

    if let Some(roots) = custom_roots {
        Ok(builder
            .with_root_certificates((**roots).clone())
            .with_no_client_auth())
    } else {
        let verifier = Verifier::new(provider).map_err(|e| e.to_string())?;
        Ok(builder
            .dangerous()
            .with_custom_certificate_verifier(Arc::new(verifier))
            .with_no_client_auth())
    }
}

fn do_http_exchange(
    mut stream: TcpStream,
    host_name: &str,
) -> Result<(u16, Vec<(String, String)>), WebError> {
    write!(
        stream,
        "GET / HTTP/1.1\r\nHost: {host_name}\r\nUser-Agent: lodo\r\nConnection: close\r\n\r\n"
    )
    .map_err(|e| WebError::Other(e.to_string()))?;
    stream.flush().map_err(|e| WebError::Other(e.to_string()))?;
    read_http_headers(stream)
}

fn do_https_exchange(
    tcp: TcpStream,
    cfg: Arc<ClientConfig>,
    host_name: &str,
) -> Result<(u16, Vec<(String, String)>), WebError> {
    let server_name =
        ServerName::try_from(host_name.to_string()).map_err(|e| WebError::Other(e.to_string()))?;
    let conn = ClientConnection::new(cfg, server_name).map_err(|e| classify_tls_err(&e))?;
    let mut tls = StreamOwned::new(conn, tcp);
    if let Err(e) = write!(
        tls,
        "GET / HTTP/1.1\r\nHost: {host_name}\r\nUser-Agent: lodo\r\nConnection: close\r\n\r\n"
    ) {
        return Err(classify_io_tls_err(e, &tls.conn));
    }
    if let Err(e) = tls.flush() {
        return Err(classify_io_tls_err(e, &tls.conn));
    }
    read_https_headers(tls)
}

fn classify_tls_err(err: &rustls::Error) -> WebError {
    match err {
        rustls::Error::InvalidCertificate(_) => WebError::UntrustedCert,
        other => {
            let text = other.to_string();
            if text.contains("certificate") || text.contains("UnknownIssuer") {
                WebError::UntrustedCert
            } else {
                WebError::Other(text)
            }
        }
    }
}

fn classify_io_tls_err(err: io::Error, _conn: &ClientConnection) -> WebError {
    if let Some(inner) = err.get_ref()
        && let Some(tls_err) = inner.downcast_ref::<rustls::Error>()
    {
        return classify_tls_err(tls_err);
    }
    let text = err.to_string();
    if text.contains("invalid peer certificate")
        || text.contains("UnknownIssuer")
        || text.contains("certificate was not trusted")
    {
        WebError::UntrustedCert
    } else {
        WebError::Other(text)
    }
}

fn read_http_headers<R: io::Read>(reader: R) -> Result<(u16, Vec<(String, String)>), WebError> {
    let mut buf = BufReader::new(reader);
    let mut status_line = String::new();
    let n = buf
        .read_line(&mut status_line)
        .map_err(|e| WebError::Other(e.to_string()))?;
    if n == 0 {
        return Err(WebError::Other("EOF".to_string()));
    }
    parse_status_and_headers(&status_line, &mut buf).map_err(WebError::Other)
}

fn read_https_headers(
    tls: StreamOwned<ClientConnection, TcpStream>,
) -> Result<(u16, Vec<(String, String)>), WebError> {
    let mut buf = BufReader::new(tls);
    let mut status_line = String::new();
    match buf.read_line(&mut status_line) {
        Ok(0) => Err(WebError::Other("EOF".to_string())),
        Ok(_) => parse_status_and_headers(&status_line, &mut buf).map_err(WebError::Other),
        Err(e) => {
            let conn = &buf.get_ref().conn;
            Err(classify_io_tls_err(e, conn))
        }
    }
}

fn parse_status_and_headers<B: BufRead>(
    status_line: &str,
    buf: &mut B,
) -> Result<(u16, Vec<(String, String)>), String> {
    let mut parts = status_line.split_whitespace();
    let proto = parts.next().unwrap_or("");
    if !proto.starts_with("HTTP/") {
        return Err(format!("malformed HTTP response: {}", status_line.trim()));
    }
    let status: u16 = parts
        .next()
        .and_then(|s| s.parse().ok())
        .ok_or_else(|| format!("malformed HTTP status: {}", status_line.trim()))?;

    let mut headers = Vec::new();
    loop {
        let mut line = String::new();
        let n = buf.read_line(&mut line).map_err(|e| e.to_string())?;
        if n == 0 || line == "\r\n" || line == "\n" {
            break;
        }
        if let Some((k, v)) = line.trim_end_matches(['\r', '\n']).split_once(':') {
            headers.push((k.trim().to_string(), v.trim().to_string()));
        }
    }
    Ok((status, headers))
}

fn http_problem(status: u16, headers: &[(String, String)], d: &Domain, http_port: u16) -> String {
    let via: Vec<&str> = headers
        .iter()
        .filter(|(k, _)| k.eq_ignore_ascii_case("via"))
        .map(|(_, v)| v.as_str())
        .collect();
    if via.join(", ").contains("Caddy") {
        return String::new();
    }
    let server = headers
        .iter()
        .find(|(k, _)| k.eq_ignore_ascii_case("server"))
        .map(|(_, v)| v.as_str())
        .unwrap_or("");
    if server == "Caddy" && status == 502 {
        return app_down(d);
    }
    if server == "Caddy" {
        return format!("caddy has no site for {}: run lodo apply", d.name);
    }
    let who = if server.is_empty() {
        "something"
    } else {
        server
    };
    format!(
        "port {http_port} on {} answered by {who}, not Caddy",
        d.address
    )
}

pub(crate) fn app_down(d: &Domain) -> String {
    format!(
        "app down: nothing answers on {}",
        join_host_port(&d.address, d.port)
    )
}

pub(crate) fn bad_answer(d: &Domain) -> String {
    format!(
        "app on {} gave Caddy no HTTP answer: does it serve HTTPS itself?",
        join_host_port(&d.address, d.port)
    )
}

fn wrong_host(host: &str, port: u16) -> String {
    format!(
        "an app listens on {}, not on the name's address{RUN_DEV}",
        join_host_port(host, port)
    )
}

pub(crate) fn needs_apply(r: &ProbeResult) -> bool {
    let dummy = Domain {
        address: r.address.clone(),
        port: r.port,
        ..Domain::default()
    };
    let down_msg = app_down(&dummy);
    let bad_msg = bad_answer(&dummy);
    for raw in r.detail.split("; ") {
        let p = raw.strip_prefix("https: ").unwrap_or(raw);
        if p != down_msg
            && p != bad_msg
            && !p.ends_with(REMOVE_LINE)
            && !p.ends_with(RUN_SETUP)
            && !p.ends_with(RUN_DEV)
        {
            return true;
        }
    }
    false
}

fn listens(host: &str, port: u16, outer_deadline: Option<Instant>) -> bool {
    let Ok(ip) = host.parse::<IpAddr>() else {
        return false;
    };
    let limit = match outer_deadline {
        Some(dl) => {
            let now = Instant::now();
            if now >= dl {
                return false;
            }
            LOCAL_TIMEOUT.min(dl - now)
        }
        None => LOCAL_TIMEOUT,
    };
    TcpStream::connect_timeout(&SocketAddr::new(ip, port), limit).is_ok()
}

fn cap_deadline(default_end: Instant, outer: Option<Instant>) -> Instant {
    match outer {
        Some(dl) if dl < default_end => dl,
        _ => default_end,
    }
}

fn hosts_addresses(content: &str) -> HashMap<String, Vec<String>> {
    let mut entries: HashMap<String, Vec<String>> = HashMap::new();
    for raw in content.lines() {
        let line = raw.split('#').next().unwrap_or("");
        let fields: Vec<&str> = line.split_whitespace().collect();
        if fields.len() < 2 {
            continue;
        }
        let ip = fields[0];
        for name in &fields[1..] {
            entries
                .entry(name.to_ascii_lowercase())
                .or_default()
                .push(ip.to_string());
        }
    }
    entries
}

fn ip_addresses(out: &str) -> Vec<String> {
    let mut addrs = Vec::new();
    for line in out.lines() {
        if let Some(v) = line.trim().strip_prefix("ip_address:") {
            addrs.push(v.trim().to_string());
        }
    }
    addrs
}

fn build_dns_a_query(name: &str, id: u16) -> Vec<u8> {
    let mut pkt = Vec::with_capacity(64);
    pkt.extend_from_slice(&id.to_be_bytes());
    pkt.extend_from_slice(&0x0100u16.to_be_bytes()); // RD
    pkt.extend_from_slice(&1u16.to_be_bytes()); // QDCOUNT = 1
    pkt.extend_from_slice(&0u16.to_be_bytes()); // ANCOUNT
    pkt.extend_from_slice(&0u16.to_be_bytes()); // NSCOUNT
    pkt.extend_from_slice(&0u16.to_be_bytes()); // ARCOUNT
    for label in name.trim_end_matches('.').split('.') {
        if !label.is_empty() && label.len() <= 63 {
            pkt.push(label.len() as u8);
            pkt.extend_from_slice(label.as_bytes());
        }
    }
    pkt.push(0);
    pkt.extend_from_slice(&1u16.to_be_bytes()); // QTYPE = A
    pkt.extend_from_slice(&1u16.to_be_bytes()); // QCLASS = IN
    pkt
}

fn parse_dns_a_reply(pkt: &[u8], expected_id: u16) -> io::Result<Vec<Ipv4Addr>> {
    if pkt.len() < 12 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "short dns header",
        ));
    }
    let id = u16::from_be_bytes([pkt[0], pkt[1]]);
    let flags = u16::from_be_bytes([pkt[2], pkt[3]]);
    if id != expected_id || (flags & 0x8000) == 0 {
        return Err(io::Error::new(io::ErrorKind::InvalidData, "bad dns header"));
    }
    let rcode = flags & 0x000F;
    if rcode != 0 {
        return Ok(Vec::new());
    }
    let qdcount = u16::from_be_bytes([pkt[4], pkt[5]]);
    let ancount = u16::from_be_bytes([pkt[6], pkt[7]]);

    let mut pos = 12;
    for _ in 0..qdcount {
        pos = skip_dns_name(pkt, pos)?;
        if pos + 4 > pkt.len() {
            return Err(io::Error::new(io::ErrorKind::InvalidData, "short question"));
        }
        pos += 4;
    }

    let mut addrs = Vec::new();
    for _ in 0..ancount {
        pos = skip_dns_name(pkt, pos)?;
        if pos + 10 > pkt.len() {
            return Err(io::Error::new(io::ErrorKind::InvalidData, "short rr"));
        }
        let rtype = u16::from_be_bytes([pkt[pos], pkt[pos + 1]]);
        let rclass = u16::from_be_bytes([pkt[pos + 2], pkt[pos + 3]]);
        let rdlen = u16::from_be_bytes([pkt[pos + 8], pkt[pos + 9]]) as usize;
        pos += 10;
        if pos + rdlen > pkt.len() {
            return Err(io::Error::new(io::ErrorKind::InvalidData, "short rdata"));
        }
        if rtype == 1 && rclass == 1 && rdlen == 4 {
            addrs.push(Ipv4Addr::new(
                pkt[pos],
                pkt[pos + 1],
                pkt[pos + 2],
                pkt[pos + 3],
            ));
        }
        pos += rdlen;
    }
    Ok(addrs)
}

fn skip_dns_name(pkt: &[u8], mut pos: usize) -> io::Result<usize> {
    let mut steps = 0;
    while pos < pkt.len() {
        steps += 1;
        if steps > 128 {
            return Err(io::Error::new(io::ErrorKind::InvalidData, "name loop"));
        }
        let b = pkt[pos];
        if b == 0 {
            return Ok(pos + 1);
        }
        if (b & 0xC0) == 0xC0 {
            if pos + 1 >= pkt.len() {
                return Err(io::Error::new(io::ErrorKind::InvalidData, "short ptr"));
            }
            return Ok(pos + 2);
        }
        if (b & 0xC0) != 0 {
            return Err(io::Error::new(io::ErrorKind::InvalidData, "bad label len"));
        }
        pos += 1 + (b as usize);
    }
    Err(io::Error::new(
        io::ErrorKind::InvalidData,
        "unterminated name",
    ))
}

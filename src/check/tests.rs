use std::collections::HashMap;
use std::fs;
use std::io::{BufRead, BufReader, Write as _};
use std::net::{Ipv4Addr, SocketAddr, TcpListener, TcpStream, UdpSocket};
use std::os::unix::fs::{PermissionsExt, symlink};
use std::path::Path;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Condvar, LazyLock, Mutex};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

use nix::unistd::Uid;
use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
use rustls::{RootCertStore, ServerConfig, ServerConnection, StreamOwned};
use tempfile::TempDir;

use super::*;
use crate::paths::Paths;
use crate::run::{self, FakeRunner};
use crate::store::{self, Domain};
use crate::system;

const LSOF_HEADER: &str = "COMMAND   PID   USER   FD   TYPE DEVICE SIZE/OFF NODE NAME\n";
const NGINX_ROW: &str = "nginx      501 tester    6u  IPv4 0x2      0t0  TCP *:80 (LISTEN)\n";
const CADDY_ROW: &str = "caddy     4242 tester    7u  IPv6 0x1      0t0  TCP *:80 (LISTEN)\n";

fn crm_domain() -> Domain {
    Domain {
        name: "crm.test".into(),
        address: "127.0.1.1".into(),
        enabled: true,
        ..Domain::default()
    }
}

fn dashboard_domain() -> Domain {
    Domain {
        name: "dashboard.crm.test".into(),
        address: "127.0.0.1".into(),
        port: 3000,
        enabled: true,
        ..Domain::default()
    }
}

fn healthy_domains() -> Vec<Domain> {
    vec![
        crm_domain(),
        dashboard_domain(),
        Domain {
            name: "old.test".into(),
            address: "127.0.1.2".into(),
            ..Domain::default()
        },
    ]
}

fn five_domains() -> Vec<Domain> {
    vec![
        Domain {
            name: "a.test".into(),
            address: "127.0.1.1".into(),
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "b.test".into(),
            address: "127.0.1.2".into(),
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "c.test".into(),
            address: "127.0.1.3".into(),
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "d.test".into(),
            address: "127.0.1.4".into(),
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "e.test".into(),
            address: "127.0.1.5".into(),
            enabled: true,
            ..Domain::default()
        },
    ]
}

#[derive(Clone, Copy)]
struct CheckSpec {
    id: usize,
    name: &'static str,
}

impl CheckSpec {
    fn pass(self, detail: impl Into<String>) -> Check {
        Check {
            id: self.id,
            name: self.name.into(),
            ok: true,
            detail: detail.into(),
            ..Check::default()
        }
    }

    fn fail(self, detail: impl Into<String>, fix: impl Into<String>) -> Check {
        Check {
            id: self.id,
            name: self.name.into(),
            detail: detail.into(),
            fix: fix.into(),
            ..Check::default()
        }
    }

    fn skip(self, detail: impl Into<String>) -> Check {
        Check {
            id: self.id,
            name: self.name.into(),
            skipped: true,
            detail: detail.into(),
            ..Check::default()
        }
    }

    fn off(self, detail: impl Into<String>) -> Check {
        Check {
            id: self.id,
            name: self.name.into(),
            skipped: true,
            off: true,
            detail: detail.into(),
            ..Check::default()
        }
    }
}

const DNSMASQ_CHECK: CheckSpec = CheckSpec {
    id: ID_DNSMASQ,
    name: "dnsmasq",
};
const CONFIG_CHECK: CheckSpec = CheckSpec {
    id: ID_CONFIG,
    name: "dnsmasq config",
};
const LOOPBACK_CHECK: CheckSpec = CheckSpec {
    id: ID_LOOPBACK,
    name: "loopback",
};
const RESOLVERS_CHECK: CheckSpec = CheckSpec {
    id: ID_RESOLVERS,
    name: "resolvers",
};
const LOCAL_CHECK: CheckSpec = CheckSpec {
    id: ID_RESOLVER_LOCAL,
    name: "resolver for .local",
};
const GENERATED_CHECK: CheckSpec = CheckSpec {
    id: ID_GENERATED,
    name: "generated files",
};
const NAMES_CHECK: CheckSpec = CheckSpec {
    id: ID_NAMES,
    name: "names resolve",
};
const CADDY_CHECK: CheckSpec = CheckSpec {
    id: ID_CADDY,
    name: "caddy",
};

// --- Hermetic network test servers ---

struct DnsServer {
    addr: SocketAddr,
    stop: Arc<AtomicBool>,
    handle: Option<JoinHandle<()>>,
}

impl DnsServer {
    fn start<F>(answer: F) -> Self
    where
        F: Fn(&str) -> Option<Ipv4Addr> + Send + Sync + 'static,
    {
        let sock = UdpSocket::bind("127.0.0.1:0").expect("bind test UDP DNS");
        let addr = sock.local_addr().expect("UDP local_addr");
        sock.set_read_timeout(Some(Duration::from_millis(200)))
            .expect("set UDP read timeout");
        let stop = Arc::new(AtomicBool::new(false));
        let stop_flag = Arc::clone(&stop);

        let handle = thread::spawn(move || {
            let mut buf = [0u8; 1500];
            while !stop_flag.load(Ordering::Relaxed) {
                let Ok((n, from)) = sock.recv_from(&mut buf) else {
                    continue;
                };
                if stop_flag.load(Ordering::Relaxed) {
                    break;
                }
                if let Some(reply) = dns_reply(&buf[..n], &answer) {
                    let _ = sock.send_to(&reply, from);
                }
            }
        });

        Self {
            addr,
            stop,
            handle: Some(handle),
        }
    }

    fn from_map(map: HashMap<String, String>) -> Self {
        Self::start(move |name| {
            map.get(name)
                .filter(|v| !v.is_empty())
                .and_then(|v| v.parse::<Ipv4Addr>().ok())
        })
    }

    fn addr_string(&self) -> String {
        self.addr.to_string()
    }
}

impl Drop for DnsServer {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        if let Ok(waker) = UdpSocket::bind("127.0.0.1:0") {
            let _ = waker.send_to(&[0u8], self.addr);
        }
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

fn dns_reply(query: &[u8], answer: &dyn Fn(&str) -> Option<Ipv4Addr>) -> Option<Vec<u8>> {
    const HEADER_LEN: usize = 12;
    if query.len() < HEADER_LEN {
        return None;
    }
    let mut labels = Vec::new();
    let mut i = HEADER_LEN;
    while i < query.len() && query[i] != 0 {
        let len = usize::from(query[i]);
        if i + 1 + len > query.len() {
            return None;
        }
        labels.push(std::str::from_utf8(&query[i + 1..i + 1 + len]).ok()?);
        i += 1 + len;
    }
    let end = i + 5;
    if end > query.len() {
        return None;
    }
    let is_a = u16::from_be_bytes([query[i + 1], query[i + 2]]) == 1;
    let name = labels.join(".").to_ascii_lowercase();
    let resolved = answer(&name);

    let mut reply = Vec::with_capacity(end + 16);
    reply.extend_from_slice(&query[..2]);
    let (flags, ancount) = match resolved {
        None => (0x8083u16, 0u16), // QR | RA | NXDOMAIN
        Some(_) if is_a => (0x8080u16, 1u16),
        Some(_) => (0x8080u16, 0u16),
    };
    reply.extend_from_slice(&flags.to_be_bytes());
    reply.extend_from_slice(&1u16.to_be_bytes()); // QDCOUNT
    reply.extend_from_slice(&ancount.to_be_bytes()); // ANCOUNT
    reply.extend_from_slice(&0u16.to_be_bytes()); // NSCOUNT
    reply.extend_from_slice(&0u16.to_be_bytes()); // ARCOUNT
    reply.extend_from_slice(&query[HEADER_LEN..end]);
    if let (1, Some(ip)) = (ancount, resolved) {
        reply.extend_from_slice(&[0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 0, 0, 4]);
        reply.extend_from_slice(&ip.octets());
    }
    Some(reply)
}

#[derive(Debug, Clone)]
struct RecordedReq {
    method: String,
    host: String,
    sni: String,
    path: String,
}

#[derive(Debug, Clone)]
struct HttpReply {
    status: u16,
    headers: Vec<(String, String)>,
}

type HttpHandler = Arc<dyn Fn(RecordedReq) -> HttpReply + Send + Sync>;

fn respond(status: u16, headers: &[(&str, &str)]) -> HttpHandler {
    let owned: Vec<(String, String)> = headers
        .iter()
        .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
        .collect();
    Arc::new(move |_| HttpReply {
        status,
        headers: owned.clone(),
    })
}

fn caddy_ok_handler() -> HttpHandler {
    respond(200, &[("Server", "Caddy"), ("Via", "1.1 Caddy")])
}

struct HttpServer {
    addr: SocketAddr,
    stop: Arc<AtomicBool>,
    handle: Option<JoinHandle<()>>,
}

impl HttpServer {
    fn start(handler: HttpHandler) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").expect("bind test HTTP");
        Self::from_listener(listener, handler, None)
    }

    fn start_tls(handler: HttpHandler) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").expect("bind test HTTPS");
        Self::from_listener(listener, handler, Some(Arc::clone(&TEST_CERT.server_cfg)))
    }

    fn from_listener(
        listener: TcpListener,
        handler: HttpHandler,
        tls_cfg: Option<Arc<ServerConfig>>,
    ) -> Self {
        let addr = listener.local_addr().expect("TCP local_addr");
        let stop = Arc::new(AtomicBool::new(false));
        let stop_flag = Arc::clone(&stop);

        let handle = thread::spawn(move || {
            let mut workers = Vec::new();
            for incoming in listener.incoming() {
                if stop_flag.load(Ordering::Relaxed) {
                    break;
                }
                let Ok(stream) = incoming else {
                    continue;
                };
                if stop_flag.load(Ordering::Relaxed) {
                    break;
                }
                let h = Arc::clone(&handler);
                let tls = tls_cfg.clone();
                workers.push(thread::spawn(move || {
                    let _ = stream.set_read_timeout(Some(Duration::from_secs(2)));
                    let _ = stream.set_write_timeout(Some(Duration::from_secs(2)));
                    if let Some(cfg) = tls {
                        serve_tls_conn(stream, cfg, &h);
                    } else {
                        serve_plain_conn(stream, &h);
                    }
                }));
            }
            for w in workers {
                let _ = w.join();
            }
        });

        Self {
            addr,
            stop,
            handle: Some(handle),
        }
    }

    fn port(&self) -> u16 {
        self.addr.port()
    }
}

impl Drop for HttpServer {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        let _ = TcpStream::connect_timeout(&self.addr, Duration::from_millis(50));
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

fn serve_plain_conn(mut stream: TcpStream, handler: &HttpHandler) {
    let Some(req) = read_request(&mut stream, "") else {
        return;
    };
    let reply = handler(req);
    let _ = stream.write_all(format_http_reply(&reply).as_bytes());
    let _ = stream.flush();
}

fn serve_tls_conn(stream: TcpStream, cfg: Arc<ServerConfig>, handler: &HttpHandler) {
    let Ok(conn) = ServerConnection::new(cfg) else {
        return;
    };
    let mut tls = StreamOwned::new(conn, stream);
    let mut req = {
        let mut reader = BufReader::new(&mut tls);
        let Some(r) = read_request_from_buf(&mut reader, "") else {
            return;
        };
        r
    };
    req.sni = tls.conn.server_name().unwrap_or("").to_string();
    let reply = handler(req);
    let _ = tls.write_all(format_http_reply(&reply).as_bytes());
    let _ = tls.flush();
    tls.conn.send_close_notify();
    let _ = tls.flush();
}

fn read_request(stream: &mut TcpStream, sni: &str) -> Option<RecordedReq> {
    let mut reader = BufReader::new(stream);
    read_request_from_buf(&mut reader, sni)
}

fn read_request_from_buf<R: BufRead>(reader: &mut R, sni: &str) -> Option<RecordedReq> {
    let mut req_line = String::new();
    if reader.read_line(&mut req_line).ok()? == 0 {
        return None;
    }
    let mut parts = req_line.split_whitespace();
    let method = parts.next()?.to_string();
    let path = parts.next()?.to_string();
    let mut host = String::new();
    loop {
        let mut line = String::new();
        let n = reader.read_line(&mut line).ok()?;
        if n == 0 || line == "\r\n" || line == "\n" {
            break;
        }
        if let Some((k, v)) = line.trim_end_matches(['\r', '\n']).split_once(':')
            && k.eq_ignore_ascii_case("host")
        {
            host = v.trim().to_string();
        }
    }
    Some(RecordedReq {
        method,
        host,
        sni: sni.to_string(),
        path,
    })
}

fn format_http_reply(reply: &HttpReply) -> String {
    let reason = match reply.status {
        200 => "OK",
        302 => "Found",
        308 => "Permanent Redirect",
        404 => "Not Found",
        500 => "Internal Server Error",
        502 => "Bad Gateway",
        _ => "Status",
    };
    let mut raw = format!("HTTP/1.1 {} {reason}\r\n", reply.status);
    for (k, v) in &reply.headers {
        raw.push_str(k);
        raw.push_str(": ");
        raw.push_str(v);
        raw.push_str("\r\n");
    }
    raw.push_str("Content-Length: 0\r\nConnection: close\r\n\r\n");
    raw
}

struct TestCert {
    trusted_roots: Arc<RootCertStore>,
    server_cfg: Arc<ServerConfig>,
}

#[expect(clippy::expect_used)]
static TEST_CERT: LazyLock<TestCert> = LazyLock::new(|| {
    let generated = rcgen::generate_simple_self_signed(vec![
        "secure.example.com".into(),
        "app.example.com".into(),
    ])
    .expect("generate self-signed test cert");
    let cert_der: CertificateDer<'static> = generated.cert.der().clone();
    let key_der =
        PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(generated.key_pair.serialize_der()));

    let mut roots = RootCertStore::empty();
    roots.add(cert_der.clone()).expect("add test root cert");

    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let server_cfg = ServerConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .expect("TLS versions")
        .with_no_client_auth()
        .with_single_cert(vec![cert_der], key_der)
        .expect("TLS server config");

    TestCert {
        trusted_roots: Arc::new(roots),
        server_cfg: Arc::new(server_cfg),
    }
});

/// Allocates a port where an app listens on `host`, or when `host` is `""`, where nothing
/// listens on either `127.0.0.1` or `::1`. Returned `Option<TcpListener>` keeps the socket open.
fn app_port(host: &str) -> (u16, Option<TcpListener>) {
    if host.is_empty() {
        for _ in 0..32 {
            let v4 = TcpListener::bind("127.0.0.1:0").expect("bind 127.0.0.1:0");
            let port = v4.local_addr().expect("v4 addr").port();
            if let Ok(v6) = TcpListener::bind(("::1", port)) {
                drop(v6);
                drop(v4);
                return (port, None);
            }
        }
        let fallback = TcpListener::bind(("::1", 0)).expect("bind [::1]:0");
        let port = fallback.local_addr().expect("v6 addr").port();
        drop(fallback);
        return (port, None);
    }

    let other = if host == "::1" { "127.0.0.1" } else { "::1" };
    for _ in 0..32 {
        let listener = TcpListener::bind((host, 0)).expect("bind app listener");
        let port = listener.local_addr().expect("listener addr").port();
        if let Ok(guard) = TcpListener::bind((other, port)) {
            drop(guard);
            return (port, Some(listener));
        }
    }
    let listener = TcpListener::bind((host, 0)).expect("bind app listener");
    let port = listener.local_addr().expect("listener addr").port();
    (port, Some(listener))
}

fn closed_udp() -> String {
    let sock = UdpSocket::bind("127.0.0.1:0").expect("bind closed UDP");
    sock.local_addr().expect("UDP addr").to_string()
}

fn closed_tcp() -> u16 {
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind closed TCP");
    listener.local_addr().expect("TCP addr").port()
}

// --- Fake Mac harness for `Env::run` / `report` / `prerequisites` ---

#[derive(Clone, Copy, PartialEq, Eq)]
enum ServiceState {
    Running,
    Stopped,
    Off,
    AsRoot,
}

struct TestMac {
    _tmp: TempDir,
    p: Paths,
    fake: FakeRunner,
    dns_addr: String,
    http_port: u16,
    https_port: u16,
    root_uid: u32,
    localhost: Vec<String>,
    domains: Vec<Domain>,
    app: u16,
    _dns_srv: DnsServer,
    _http_srv: HttpServer,
    _extra_http: Vec<HttpServer>,
    _listeners: Vec<TcpListener>,
}

impl TestMac {
    fn new(domains: Vec<Domain>) -> Self {
        let tmp = tempfile::tempdir().expect("tempdir");
        let p = Paths::for_test(tmp.path());
        let fake = FakeRunner::new();

        write_file(&p.dnsmasq, "");
        write_file(
            &p.system_conf,
            &format!("# dnsmasq\n\n{}", system::conf_block(&p)),
        );
        write_file(&p.script, &system::script(&p));
        chmod(&p.script, 0o755);
        for d in &domains {
            if d.enabled {
                write_file(&p.resolver_dir.join(&d.name), &system::resolver_file());
            }
        }
        for f in system::generated(&p, &domains) {
            write_file(&f.path, &f.content);
        }
        write_file(&p.caddy, "");
        write_file(
            &p.system_caddyfile,
            &system::rewrite_system_caddyfile("", &p),
        );

        for label in system::DNS_SYSTEM_LABELS {
            fake.fail(
                launchctl_print(&p, label),
                format!("Could not find service \"{label}\" in domain for system"),
            );
        }
        fake.set(
            launchctl_print(&p, system::LOOPBACK_LABEL),
            "system/io.lodo.loopback = {\n\tstate = not running\n}\n",
        );
        fake.set(
            run::line(&p.ifconfig.to_string_lossy(), &["lo0"]),
            lo0(usize::from(store::OWN_LAST)),
        );

        let mut answers = HashMap::new();
        for d in &domains {
            answers.insert(d.name.clone(), d.address.clone());
            fake.set(
                dscacheutil(&p, &d.name),
                macos_output(&d.name, &[&d.address]),
            );
        }

        let dns_srv = DnsServer::from_map(answers);
        let http_srv = HttpServer::start(caddy_ok_handler());
        let dns_addr = dns_srv.addr_string();
        let http_port = http_srv.port();

        fake.set(
            run::line(
                &p.sudo.to_string_lossy(),
                &["-n", "-k", "-l", &p.script.to_string_lossy()],
            ),
            format!("{}\n", p.script.display()),
        );
        fake.set(lsof(&p, http_port), format!("{LSOF_HEADER}{CADDY_ROW}"));

        let mut mac = Self {
            _tmp: tmp,
            p,
            fake,
            dns_addr,
            http_port,
            https_port: store::TLS_PORT,
            root_uid: Uid::current().as_raw(),
            localhost: vec!["127.0.0.1".into(), "::1".into()],
            domains,
            app: 0,
            _dns_srv: dns_srv,
            _http_srv: http_srv,
            _extra_http: Vec::new(),
            _listeners: Vec::new(),
        };
        mac.set_service("dnsmasq", ServiceState::Running);
        mac.set_service("caddy", ServiceState::Running);
        mac
    }

    fn env(&self) -> Env<'_> {
        let mut e = Env::new(&self.p, &self.fake);
        e.dns.clone_from(&self.dns_addr);
        e.http_port = self.http_port;
        e.https_port = self.https_port;
        e.root_uid = self.root_uid;
        e.localhost.clone_from(&self.localhost);
        e
    }

    fn set_service(&mut self, service: &str, state: ServiceState) {
        let label = format!("sh.brew.{service}");
        let agent = self.p.launch_agents.join(format!("{label}.plist"));
        let _ = fs::remove_file(&agent);
        let gui_target = format!("gui/501/{label}");
        let line = run::line(&self.p.launchctl.to_string_lossy(), &["print", &gui_target]);

        match state {
            ServiceState::Running | ServiceState::Stopped => write_file(&agent, ""),
            ServiceState::AsRoot => {
                write_file(&self.p.launch_daemons.join(format!("{label}.plist")), "");
            }
            ServiceState::Off => {}
        }

        match state {
            ServiceState::Running => {
                self.fake.set(
                    line,
                    format!("{gui_target} = {{\n\tstate = running\n\tpid = 42\n}}\n"),
                );
            }
            ServiceState::AsRoot => {
                self.fake.fail(
                    line,
                    format!("Could not find service \"{label}\" in domain for user gui: 501"),
                );
                if service == "caddy" {
                    let sys_target = format!("system/{label}");
                    self.fake.set(
                        run::line(&self.p.launchctl.to_string_lossy(), &["print", &sys_target]),
                        format!("{sys_target} = {{\n\tstate = running\n\tpid = 42\n}}\n"),
                    );
                }
            }
            ServiceState::Stopped => {
                self.fake.set(
                    line,
                    format!("{gui_target} = {{\n\tstate = not running\n}}\n"),
                );
            }
            ServiceState::Off => {
                self.fake.fail(
                    line,
                    format!("Could not find service \"{label}\" in domain for user gui: 501"),
                );
            }
        }
    }

    fn start_http(&mut self, handler: HttpHandler) -> u16 {
        let srv = HttpServer::start(handler);
        let port = srv.port();
        self._extra_http.push(srv);
        port
    }

    fn alloc_app_port(&mut self, host: &str) -> u16 {
        let (port, guard) = app_port(host);
        if let Some(l) = guard {
            self._listeners.push(l);
        }
        port
    }

    fn expand(&self, mut c: Check) -> Check {
        let root = self._tmp.path().to_string_lossy();
        let port = self.http_port.to_string();
        let app = self.app.to_string();
        let repl = |s: &str| {
            s.replace("{root}", &root)
                .replace("{port}", &port)
                .replace("{app}", &app)
        };
        c.detail = repl(&c.detail);
        c.fix = repl(&c.fix);
        c
    }
}

type MacChange = Box<dyn FnOnce(&mut TestMac)>;

struct RunCase {
    name: &'static str,
    domains: Option<Vec<Domain>>,
    change: Option<MacChange>,
    want: Check,
}

impl RunCase {
    fn new(name: &'static str, change: impl FnOnce(&mut TestMac) + 'static, want: Check) -> Self {
        Self {
            name,
            domains: None,
            change: Some(Box::new(change)),
            want,
        }
    }

    fn with_domains(
        name: &'static str,
        domains: Vec<Domain>,
        change: impl FnOnce(&mut TestMac) + 'static,
        want: Check,
    ) -> Self {
        Self {
            name,
            domains: Some(domains),
            change: Some(Box::new(change)),
            want,
        }
    }
}

fn run_cases(cases: Vec<RunCase>) {
    for tc in cases {
        let domains = tc.domains.unwrap_or_else(healthy_domains);
        let mut m = TestMac::new(domains);
        if let Some(change) = tc.change {
            change(&mut m);
        }
        let checks = m.env().run(&m.domains);
        let got = &checks[tc.want.id - 1];
        let want = m.expand(tc.want);
        assert_eq!(got, &want, "case {:?} (check {})", tc.name, want.id);
    }
}

fn write_conf(lines: &[&str]) -> impl FnOnce(&mut TestMac) + 'static {
    let joined = format!("{}\n", lines.join("\n"));
    move |m: &mut TestMac| {
        let conf = m.p.dnsmasq_conf.to_string_lossy();
        write_file(&m.p.system_conf, &joined.replace("{conf}", &conf));
    }
}

fn launchctl_print(p: &Paths, label: &str) -> String {
    let sys_target = format!("system/{label}");
    run::line(&p.launchctl.to_string_lossy(), &["print", &sys_target])
}

fn dscacheutil(p: &Paths, name: &str) -> String {
    run::line(
        &p.dscacheutil.to_string_lossy(),
        &["-q", "host", "-a", "name", name],
    )
}

fn macos_output(name: &str, addrs: &[&str]) -> String {
    let mut b = format!("name: {name}\n");
    for a in addrs {
        b.push_str("ip_address: ");
        b.push_str(a);
        b.push('\n');
    }
    b.push('\n');
    b
}

fn lo0(n: usize) -> String {
    let mut b = String::from(
        "lo0: flags=8049<UP,LOOPBACK,RUNNING,MULTICAST> mtu 16384\n\
         \toptions=1203<RXCSUM,TXCSUM,TXSTATUS,SW_TIMESTAMP>\n\
         \tinet 127.0.0.1 netmask 0xff000000\n\
         \tinet6 ::1 prefixlen 128 \n\
         \tinet6 fe80::1%lo0 prefixlen 64 scopeid 0x1 \n",
    );
    for i in 1..=n {
        b.push_str(&format!(
            "\tinet {} netmask 0xff000000\n",
            store::own_address(i as u8)
        ));
    }
    b.push_str("\tnd6 options=201<PERFORMNUD,DAD>\n");
    b
}

fn lsof(p: &Paths, port: u16) -> String {
    let port_arg = format!("-iTCP:{port}");
    run::line(
        &p.lsof.to_string_lossy(),
        &["-nP", &port_arg, "-sTCP:LISTEN"],
    )
}

fn write_file(path: &Path, content: &str) {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).expect("mkdir parent");
    }
    fs::write(path, content.as_bytes()).expect("write test file");
}

fn remove_file(path: &Path) {
    fs::remove_file(path).expect("remove test file");
}

fn replace_with_dir(path: &Path) {
    remove_file(path);
    fs::create_dir(path).expect("create replacement dir");
}

fn chmod(path: &Path, mode: u32) {
    fs::set_permissions(path, fs::Permissions::from_mode(mode)).expect("chmod");
}

fn make_symlink(target: &Path, link: &Path) {
    if let Some(parent) = link.parent() {
        fs::create_dir_all(parent).expect("mkdir symlink parent");
    }
    symlink(target, link).expect("create symlink");
}

// =====================================================================
// Ported from `internal/check/check_test.go`
// =====================================================================

#[test]
fn test_new_env() {
    let p = Paths::for_test(Path::new("/fake"));
    let r = FakeRunner::new();
    let e = Env::new(&p, &r);
    assert_eq!(e.paths, &p);
    assert_eq!(e.dns, "127.0.0.1:53535");
    assert_eq!(e.http_port, 80);
    assert_eq!(e.https_port, 443);
    assert!(e.root_cas.is_none());
    assert_eq!(e.root_uid, 0);
    assert_eq!(
        e.localhost,
        vec!["127.0.0.1".to_string(), "::1".to_string()]
    );
    assert!(e.timeout.is_none());

    let _ = e.runner.run(None, "probe-runner", &[]);
    assert_eq!(r.calls(), vec!["probe-runner".to_string()]);
}

#[test]
fn test_env_run() {
    let m = TestMac::new(healthy_domains());
    let got = m.env().run(&m.domains);
    let want: Vec<Check> = vec![
        DNSMASQ_CHECK.pass("running as tester, pid 42"),
        CONFIG_CHECK.pass("includes {root}/Users/tester/.config/lodo/dnsmasq.conf"),
        LOOPBACK_CHECK.pass("50 of 50 addresses on lo0"),
        RESOLVERS_CHECK.pass("2 files"),
        LOCAL_CHECK.pass("absent"),
        GENERATED_CHECK.pass("match domains.json"),
        NAMES_CHECK.pass("2 of 2 resolve"),
        CADDY_CHECK.pass("running, serves 1 site"),
    ]
    .into_iter()
    .map(|c| m.expand(c))
    .collect();
    assert_eq!(got, want);
}

#[test]
fn test_env_run_dnsmasq() {
    run_cases(vec![
        RunCase::new(
            "not installed",
            |m| remove_file(&m.p.dnsmasq),
            DNSMASQ_CHECK.fail("not installed", "brew install dnsmasq"),
        ),
        RunCase::new(
            "a root job from sudo brew services",
            |m| {
                m.fake.set(
                    launchctl_print(&m.p, "homebrew.mxcl.dnsmasq"),
                    "system/homebrew.mxcl.dnsmasq = {\n}\n",
                );
            },
            DNSMASQ_CHECK.fail(
                "a root job, system/homebrew.mxcl.dnsmasq, runs dnsmasq and shadows lodo's",
                "lodo setup",
            ),
        ),
        RunCase::new(
            "a root job under the newer label",
            |m| {
                m.fake.set(
                    launchctl_print(&m.p, "sh.brew.dnsmasq"),
                    "system/sh.brew.dnsmasq = {\n}\n",
                );
            },
            DNSMASQ_CHECK.fail(
                "a root job, system/sh.brew.dnsmasq, runs dnsmasq and shadows lodo's",
                "lodo setup",
            ),
        ),
        RunCase::new(
            "not running",
            |m| m.set_service("dnsmasq", ServiceState::Stopped),
            DNSMASQ_CHECK.fail("not running", "lodo setup"),
        ),
        RunCase::new(
            "turned off",
            |m| m.set_service("dnsmasq", ServiceState::Off),
            DNSMASQ_CHECK.off("turned off: no .lodo name resolves until it is on"),
        ),
        RunCase::new(
            "runs as root",
            |m| m.set_service("dnsmasq", ServiceState::AsRoot),
            DNSMASQ_CHECK.fail("runs as root", "lodo setup"),
        ),
    ]);
}

#[test]
fn test_env_run_dnsmasq_config() {
    let eisdir = io::Error::from_raw_os_error(nix::libc::EISDIR);
    run_cases(vec![
        RunCase::new(
            "missing",
            |m| remove_file(&m.p.system_conf),
            CONFIG_CHECK.fail("missing", "lodo setup"),
        ),
        RunCase::new(
            "a folder where the file should be",
            |m| replace_with_dir(&m.p.system_conf),
            CONFIG_CHECK.fail(
                format!("read {{root}}/opt/homebrew/etc/dnsmasq.conf: {eisdir}"),
                "lodo setup",
            ),
        ),
        RunCase::new(
            "conf-file points at another file",
            write_conf(&[
                "conf-file=/Users/tester/.config/localdns/dnsmasq.conf",
                "listen-address=127.0.0.1",
                "port=53535",
                "bind-interfaces",
            ]),
            CONFIG_CHECK.fail(
                "conf-file points at /Users/tester/.config/localdns/dnsmasq.conf",
                "lodo setup",
            ),
        ),
        RunCase::new(
            "two conf-file lines",
            write_conf(&[
                "conf-file={conf}",
                "conf-file=/x.conf",
                "listen-address=127.0.0.1",
                "port=53535",
                "bind-interfaces",
            ]),
            CONFIG_CHECK.fail("2 conf-file lines", "lodo setup"),
        ),
        RunCase::new(
            "a commented conf-file line does not count",
            write_conf(&[
                "#conf-file={conf}",
                "listen-address=127.0.0.1",
                "port=53535",
                "bind-interfaces",
            ]),
            CONFIG_CHECK.fail("no conf-file line", "lodo setup"),
        ),
        RunCase::new(
            "port 53",
            write_conf(&[
                "conf-file={conf}",
                "listen-address=127.0.0.1",
                "port=53",
                "bind-interfaces",
            ]),
            CONFIG_CHECK.fail("port is 53, want 53535", "lodo setup"),
        ),
        RunCase::new(
            "two port lines",
            write_conf(&[
                "conf-file={conf}",
                "listen-address=127.0.0.1",
                "port=53535",
                "port=53",
                "bind-interfaces",
            ]),
            CONFIG_CHECK.fail("2 port lines", "lodo setup"),
        ),
        RunCase::new(
            "no port line",
            write_conf(&[
                "conf-file={conf}",
                "listen-address=127.0.0.1",
                "bind-interfaces",
            ]),
            CONFIG_CHECK.fail("no port line", "lodo setup"),
        ),
        RunCase::new(
            "listen-address with a second address",
            write_conf(&[
                "conf-file={conf}",
                "listen-address=127.0.0.1,10.0.0.1",
                "port=53535",
                "bind-interfaces",
            ]),
            CONFIG_CHECK.fail(
                "listen-address is 127.0.0.1,10.0.0.1, want 127.0.0.1",
                "lodo setup",
            ),
        ),
        RunCase::new(
            "no listen-address line",
            write_conf(&["conf-file={conf}", "port=53535", "bind-interfaces"]),
            CONFIG_CHECK.fail("no listen-address line", "lodo setup"),
        ),
        RunCase::new(
            "no bind-interfaces line",
            write_conf(&[
                "conf-file={conf}",
                "listen-address=127.0.0.1",
                "port=53535",
                "#bind-interfaces",
            ]),
            CONFIG_CHECK.fail("no bind-interfaces line", "lodo setup"),
        ),
        RunCase::new(
            "spaces, comments and other options",
            write_conf(&[
                "# dnsmasq",
                "",
                "#port=53",
                "server=1.1.1.1",
                "  conf-file = {conf} ",
                "listen-address= 127.0.0.1",
                "port =53535",
                "\tbind-interfaces",
            ]),
            CONFIG_CHECK.pass("includes {root}/Users/tester/.config/lodo/dnsmasq.conf"),
        ),
    ]);
}

#[test]
fn test_env_run_loopback() {
    let not_loaded = |m: &mut TestMac| {
        m.fake.fail(
            launchctl_print(&m.p, system::LOOPBACK_LABEL),
            "Could not find service",
        );
    };
    run_cases(vec![
        RunCase::new(
            "49 of the 50 aliases",
            |m| {
                m.fake.set(
                    run::line(&m.p.ifconfig.to_string_lossy(), &["lo0"]),
                    lo0(49),
                );
            },
            LOOPBACK_CHECK.fail("49 of 50 addresses on lo0", "lodo setup"),
        ),
        RunCase::new(
            "an address past the block does not count",
            |m| {
                m.fake.set(
                    run::line(&m.p.ifconfig.to_string_lossy(), &["lo0"]),
                    format!("{}\tinet 127.0.1.51 netmask 0xff000000\n", lo0(49)),
                );
            },
            LOOPBACK_CHECK.fail("49 of 50 addresses on lo0", "lodo setup"),
        ),
        RunCase::new(
            "job not loaded, no aliases",
            move |m| {
                not_loaded(m);
                m.fake
                    .set(run::line(&m.p.ifconfig.to_string_lossy(), &["lo0"]), lo0(0));
            },
            LOOPBACK_CHECK.fail("job not loaded; 0 of 50 addresses on lo0", "lodo setup"),
        ),
        RunCase::new(
            "job not loaded, aliases still up",
            not_loaded,
            LOOPBACK_CHECK.fail("job not loaded; 50 of 50 addresses on lo0", "lodo setup"),
        ),
    ]);
}

#[test]
fn test_env_run_resolvers() {
    let enotdir = io::Error::from_raw_os_error(nix::libc::ENOTDIR);
    run_cases(vec![
        RunCase::new(
            "script missing",
            |m| remove_file(&m.p.script),
            RESOLVERS_CHECK.fail("script missing", "lodo setup"),
        ),
        RunCase::new(
            "script is a symlink",
            |m| {
                let target = m._tmp.path().join("elsewhere-apply-resolvers.sh");
                write_file(&target, &system::script(&m.p));
                chmod(&target, 0o755);
                remove_file(&m.p.script);
                make_symlink(&target, &m.p.script);
            },
            RESOLVERS_CHECK.fail("script is a symlink", "lodo setup"),
        ),
        RunCase::new(
            "script is a folder",
            |m| replace_with_dir(&m.p.script),
            RESOLVERS_CHECK.fail("script is not a regular file", "lodo setup"),
        ),
        RunCase::new(
            "a file where the script's folder should be",
            |m| {
                let dir = m.p.script.parent().expect("script parent").to_path_buf();
                fs::remove_dir_all(&dir).expect("remove script dir");
                write_file(&dir, "");
            },
            RESOLVERS_CHECK.fail(
                format!(
                    "lstat {{root}}/Library/Application Support/lodo/apply-resolvers.sh: {enotdir}"
                ),
                "lodo setup",
            ),
        ),
        RunCase::new(
            "script not owned by root",
            |m| m.root_uid = m.root_uid.wrapping_add(1),
            RESOLVERS_CHECK.fail("script not owned by root", "lodo setup"),
        ),
        RunCase::new(
            "script mode 0644",
            |m| chmod(&m.p.script, 0o644),
            RESOLVERS_CHECK.fail("script mode 0644, want 0755", "lodo setup"),
        ),
        RunCase::new(
            "script from an older lodo",
            |m| write_file(&m.p.script, "#!/bin/sh\nexit 0\n"),
            RESOLVERS_CHECK.fail("script out of date", "lodo setup"),
        ),
        RunCase::new(
            "sudo asks for a password",
            |m| {
                m.fake.fail(
                    run::line(
                        &m.p.sudo.to_string_lossy(),
                        &["-n", "-k", "-l", &m.p.script.to_string_lossy()],
                    ),
                    "sudo: a password is required",
                );
            },
            RESOLVERS_CHECK.fail(
                "sudo asks for a password: the sudoers rule is missing",
                "lodo setup",
            ),
        ),
        RunCase::new(
            "a resolver file missing",
            |m| remove_file(&m.p.resolver_dir.join("crm.test")),
            RESOLVERS_CHECK.fail("missing for crm.test", "lodo apply"),
        ),
        RunCase::new(
            "a resolver file lodo did not write",
            |m| {
                write_file(
                    &m.p.resolver_dir.join("dashboard.crm.test"),
                    "nameserver 127.0.0.1\n",
                );
            },
            RESOLVERS_CHECK.fail(
                "not written by lodo, so lodo apply won't replace: dashboard.crm.test",
                "sudo rm {root}/etc/resolver/dashboard.crm.test, then lodo apply",
            ),
        ),
        RunCase::new(
            "a hand-made file outranks a missing one",
            |m| {
                remove_file(&m.p.resolver_dir.join("crm.test"));
                write_file(
                    &m.p.resolver_dir.join("dashboard.crm.test"),
                    "nameserver 127.0.0.1\n",
                );
            },
            RESOLVERS_CHECK.fail(
                "not written by lodo, so lodo apply won't replace: dashboard.crm.test",
                "sudo rm {root}/etc/resolver/dashboard.crm.test, then lodo apply",
            ),
        ),
        RunCase::new(
            "an lodo file with old content",
            |m| {
                write_file(
                    &m.p.resolver_dir.join("crm.test"),
                    &format!("{}\nnameserver 127.0.0.1\n", system::MARKER),
                );
            },
            RESOLVERS_CHECK.fail("missing for crm.test", "lodo apply"),
        ),
        RunCase::with_domains(
            "more resolver files missing than the detail lists",
            five_domains(),
            |m| {
                for d in &m.domains {
                    remove_file(&m.p.resolver_dir.join(&d.name));
                }
            },
            RESOLVERS_CHECK.fail("missing for a.test, b.test, c.test, and 2 more", "lodo apply"),
        ),
        RunCase::with_domains(
            "one enabled domain",
            healthy_domains().into_iter().take(1).collect(),
            |_| {},
            RESOLVERS_CHECK.pass("1 file"),
        ),
        RunCase::with_domains(
            "no enabled domain",
            healthy_domains().into_iter().skip(2).collect(),
            |_| {},
            RESOLVERS_CHECK.pass("0 files"),
        ),
        RunCase::new(
            "dnsmasq turned off, lodo's files removed",
            |m| {
                m.set_service("dnsmasq", ServiceState::Off);
                for name in ["crm.test", "dashboard.crm.test"] {
                    remove_file(&m.p.resolver_dir.join(name));
                }
                write_file(
                    &m.p.resolver_dir.join("other.test"),
                    "nameserver 10.0.0.1\n",
                );
            },
            RESOLVERS_CHECK.pass("none while dnsmasq is turned off"),
        ),
        RunCase::new(
            "dnsmasq turned off, lodo's files left",
            |m| m.set_service("dnsmasq", ServiceState::Off),
            RESOLVERS_CHECK
                .off("dnsmasq turned off, but lodo apply should remove these: crm.test, dashboard.crm.test"),
        ),
        RunCase::new(
            "dnsmasq turned off, resolver dir absent",
            |m| {
                m.set_service("dnsmasq", ServiceState::Off);
                fs::remove_dir_all(&m.p.resolver_dir).expect("remove resolver_dir");
            },
            RESOLVERS_CHECK.pass("none while dnsmasq is turned off"),
        ),
        RunCase::new(
            "dnsmasq turned off, script out of date",
            |m| {
                m.set_service("dnsmasq", ServiceState::Off);
                write_file(&m.p.script, "#!/bin/sh\nexit 0\n");
            },
            RESOLVERS_CHECK.fail("script out of date", "lodo setup"),
        ),
    ]);
}

#[test]
fn test_env_run_resolver_for_local() {
    let want = || {
        LOCAL_CHECK.fail(
            "{root}/etc/resolver/local sends every .local name to one server, away from Bonjour",
            "sudo rm {root}/etc/resolver/local",
        )
    };
    run_cases(vec![
        RunCase::new(
            "present",
            |m| write_file(&m.p.resolver_local, "nameserver 127.0.0.1\nport 53535\n"),
            want(),
        ),
        RunCase::new(
            "a dangling symlink",
            |m| make_symlink(&m._tmp.path().join("nowhere"), &m.p.resolver_local),
            want(),
        ),
        RunCase::new(
            "a file where /etc/resolver should be",
            |m| {
                fs::remove_dir_all(&m.p.resolver_dir).expect("remove resolver_dir");
                write_file(&m.p.resolver_dir, "");
            },
            LOCAL_CHECK.pass("absent"),
        ),
    ]);
}

#[test]
fn test_env_run_generated_files() {
    run_cases(vec![
        RunCase::new(
            "dnsmasq.conf edited by hand",
            |m| write_file(&m.p.dnsmasq_conf, "address=/x.test/127.0.0.1\n"),
            GENERATED_CHECK.fail("out of date: dnsmasq.conf", "lodo apply"),
        ),
        RunCase::new(
            "resolvers missing",
            |m| remove_file(&m.p.resolvers),
            GENERATED_CHECK.fail("out of date: resolvers", "lodo apply"),
        ),
        RunCase::new(
            "every file stale",
            |m| {
                for f in [&m.p.dnsmasq_conf, &m.p.resolvers, &m.p.caddyfile] {
                    write_file(f, "stale\n");
                }
            },
            GENERATED_CHECK.fail(
                "out of date: dnsmasq.conf, resolvers, Caddyfile",
                "lodo apply",
            ),
        ),
    ]);
}

#[test]
fn test_env_run_names_resolve() {
    run_cases(vec![
        RunCase::with_domains(
            "no enabled domain",
            healthy_domains().into_iter().skip(2).collect(),
            |_| {},
            NAMES_CHECK.pass("no enabled domains"),
        ),
        RunCase::new(
            "macOS resolves a name to another address",
            |m| {
                m.fake.set(
                    dscacheutil(&m.p, "crm.test"),
                    macos_output("crm.test", &["127.0.1.9"]),
                );
            },
            NAMES_CHECK.fail("crm.test: macOS: 127.0.1.9, want 127.0.1.1", "lodo apply"),
        ),
        RunCase::new(
            "dnsmasq turned off",
            |m| {
                m.set_service("dnsmasq", ServiceState::Off);
                m.fake.set(
                    dscacheutil(&m.p, "crm.test"),
                    macos_output("crm.test", &["127.0.1.9"]),
                );
            },
            NAMES_CHECK.fail(
                "crm.test: macOS: 127.0.1.9, want 127.0.1.1",
                "turn dnsmasq on: lodo service dnsmasq on",
            ),
        ),
        RunCase::new(
            "caddy cannot reach an app",
            |m| {
                m.http_port = m.start_http(respond(502, &[("Server", "Caddy")]));
                m.domains[1].port = m.alloc_app_port("");
            },
            NAMES_CHECK.pass("2 of 2 resolve; not running: dashboard.crm.test"),
        ),
        RunCase::new(
            "a name fails next to an app that isn't running",
            |m| {
                m.http_port = m.start_http(respond(502, &[("Server", "Caddy")]));
                m.fake.set(
                    dscacheutil(&m.p, "crm.test"),
                    macos_output("crm.test", &["127.0.1.9"]),
                );
                m.domains[1].port = m.alloc_app_port("");
            },
            NAMES_CHECK.fail(
                "crm.test: macOS: 127.0.1.9, want 127.0.1.1; not running: dashboard.crm.test",
                "lodo apply",
            ),
        ),
        RunCase::new(
            "an app listens on Localhost instead",
            |m| {
                m.http_port = m.start_http(respond(502, &[("Server", "Caddy")]));
                m.localhost = vec!["::1".into()];
                let port = m.alloc_app_port("::1");
                m.app = port;
                m.domains[1].port = port;
            },
            NAMES_CHECK.fail(
                "dashboard.crm.test: an app listens on [::1]:{app}, not on the name's address: start it with lodo dev in its project",
                "stop the app, then start it with lodo dev in its project",
            ),
        ),
        RunCase::with_domains(
            "one app on Localhost, two names on its port",
            vec![
                crm_domain(),
                Domain {
                    name: "a.crm.test".into(),
                    address: "127.0.0.1".into(),
                    port: 1,
                    enabled: true,
                    ..Domain::default()
                },
                Domain {
                    name: "b.crm.test".into(),
                    address: "127.0.0.1".into(),
                    port: 1,
                    enabled: true,
                    ..Domain::default()
                },
            ],
            |m| {
                m.http_port = m.start_http(respond(502, &[("Server", "Caddy")]));
                m.localhost = vec!["::1".into()];
                let port = m.alloc_app_port("::1");
                m.app = port;
                m.domains[1].port = port;
                m.domains[2].port = port;
            },
            NAMES_CHECK.fail(
                "a.crm.test, b.crm.test: an app listens on [::1]:{app}, not on the name's address: start it with lodo dev in its project",
                "stop the app, then start it with lodo dev in its project",
            ),
        ),
        RunCase::new(
            "an app answers caddy badly",
            |m| {
                m.http_port = m.start_http(respond(502, &[("Server", "Caddy")]));
                let port = m.alloc_app_port("127.0.0.1");
                m.app = port;
                m.domains[1].port = port;
            },
            NAMES_CHECK.fail(
                "dashboard.crm.test: app on 127.0.0.1:{app} gave Caddy no HTTP answer: does it serve HTTPS itself?",
                "",
            ),
        ),
        RunCase::new(
            "a hosts line names its own fix",
            |m| {
                write_file(&m.p.hosts, "127.0.1.9 crm.test\n");
                m.fake.set(
                    dscacheutil(&m.p, "crm.test"),
                    macos_output("crm.test", &["127.0.1.9"]),
                );
            },
            NAMES_CHECK.fail(
                "crm.test: {root}/etc/hosts maps crm.test to 127.0.1.9: remove that line",
                "",
            ),
        ),
        RunCase::with_domains(
            "more names fail the same way than the detail lists",
            five_domains(),
            |m| {
                for d in &m.domains {
                    m.fake.set(dscacheutil(&m.p, &d.name), "");
                }
            },
            NAMES_CHECK.fail("a.test, b.test, c.test, and 2 more: macOS: no address", "lodo apply"),
        ),
    ]);
}

#[test]
fn test_env_run_caddy() {
    let validate_line = |p: &Paths| {
        run::line(
            &p.caddy.to_string_lossy(),
            &[
                "validate",
                "--config",
                &p.system_caddyfile.to_string_lossy(),
                "--adapter",
                "caddyfile",
            ],
        )
    };
    let mut two_site_domains = healthy_domains();
    two_site_domains.push(Domain {
        name: "api.crm.test".into(),
        address: "127.0.0.1".into(),
        port: 3001,
        enabled: true,
        ..Domain::default()
    });

    run_cases(vec![
        RunCase::with_domains(
            "no enabled domain has a port, caddy running",
            vec![
                crm_domain(),
                Domain {
                    name: "dashboard.crm.test".into(),
                    address: "127.0.0.1".into(),
                    port: 3000,
                    ..Domain::default()
                },
            ],
            |_| {},
            CADDY_CHECK.pass("running, no enabled domain has a port"),
        ),
        RunCase::with_domains(
            "no enabled domain has a port, caddy not running",
            vec![crm_domain()],
            |m| m.set_service("caddy", ServiceState::Stopped),
            CADDY_CHECK.skip("no enabled domain has a port"),
        ),
        RunCase::with_domains(
            "turned off, no port needs it",
            vec![crm_domain()],
            |m| m.set_service("caddy", ServiceState::Off),
            CADDY_CHECK.off("turned off: no http://name.lodo reaches its port until it is on"),
        ),
        RunCase::new(
            "turned off",
            |m| m.set_service("caddy", ServiceState::Off),
            CADDY_CHECK.off("turned off: no http://name.lodo reaches its port until it is on"),
        ),
        RunCase::new(
            "not installed",
            |m| remove_file(&m.p.caddy),
            CADDY_CHECK.fail("not installed", "brew install caddy, then lodo setup"),
        ),
        RunCase::new(
            "Homebrew's Caddyfile without lodo's import",
            |m| write_file(&m.p.system_caddyfile, "localhost {\n\trespond \"hi\"\n}\n"),
            CADDY_CHECK.fail("Homebrew's Caddyfile does not import lodo's", "lodo setup"),
        ),
        RunCase::new(
            "admin API not disabled",
            |m| write_file(&m.p.system_caddyfile, &system::caddy_block(&m.p)),
            CADDY_CHECK.fail("Caddy's admin API is not turned off", "lodo setup"),
        ),
        RunCase::new(
            "runs as root",
            |m| m.set_service("caddy", ServiceState::AsRoot),
            CADDY_CHECK.fail("runs as root", "lodo setup"),
        ),
        RunCase::new(
            "not running",
            |m| m.set_service("caddy", ServiceState::Stopped),
            CADDY_CHECK.fail("not running", "lodo setup"),
        ),
        RunCase::new(
            "not running because another program holds port 80",
            |m| {
                m.set_service("caddy", ServiceState::Stopped);
                m.fake
                    .set(lsof(&m.p, m.http_port), format!("{LSOF_HEADER}{NGINX_ROW}"));
            },
            CADDY_CHECK.fail("port {port} is taken by nginx", "stop nginx, then lodo apply"),
        ),
        RunCase::new(
            "caddy rejects the config",
            move |m| {
                m.fake.fail(
                    validate_line(&m.p),
                    "2026/10/08 12:00:00.000\tINFO\tusing config from file\n\
                     Error: adapting config using caddyfile: Caddyfile:3: unrecognized directive: bogus",
                );
            },
            CADDY_CHECK.fail(
                "caddy validate: adapting config using caddyfile: Caddyfile:3: unrecognized directive: bogus",
                "lodo apply",
            ),
        ),
        RunCase::new(
            "validate fails with several lines of output",
            move |m| {
                m.fake.fail(validate_line(&m.p), "line one\nline two");
            },
            CADDY_CHECK.fail(
                "caddy validate: {root}/opt/homebrew/bin/caddy validate --config {root}/opt/homebrew/etc/Caddyfile --adapter caddyfile: line one; line two",
                "lodo apply",
            ),
        ),
        RunCase::new(
            "another program has the port",
            |m| {
                m.fake
                    .set(lsof(&m.p, m.http_port), format!("{LSOF_HEADER}{NGINX_ROW}"));
            },
            CADDY_CHECK.fail("port {port} is taken by nginx", "stop nginx, then lodo apply"),
        ),
        RunCase::new(
            "caddy among several listeners",
            |m| {
                m.fake.set(
                    lsof(&m.p, m.http_port),
                    format!("{LSOF_HEADER}{NGINX_ROW}{CADDY_ROW}"),
                );
            },
            CADDY_CHECK.pass("running, serves 1 site"),
        ),
        RunCase::new(
            "nothing listens",
            |m| m.fake.fail(lsof(&m.p, m.http_port), ""),
            CADDY_CHECK.fail("nothing listens on port {port}", "lodo setup"),
        ),
        RunCase::with_domains(
            "two sites",
            two_site_domains,
            |_| {},
            CADDY_CHECK.pass("running, serves 2 sites"),
        ),
    ]);
}

#[test]
fn test_env_report() {
    let m = TestMac::new(healthy_domains());
    let (checks, results) = m.env().report(&m.domains);
    assert_eq!(checks.len(), 8);
    assert!(checks[6].ok);
    assert_eq!(results.len(), 2);
    assert!(results[0].ok());
    assert!(results[1].ok());
}

#[test]
fn test_env_prerequisites() {
    let m = TestMac::new(healthy_domains());
    m.fake.fail(
        run::line(
            &m.p.launchctl.to_string_lossy(),
            &["print", "system/io.lodo.loopback"],
        ),
        "Could not find service",
    );
    let got = m.env().prerequisites(&m.domains);
    assert_eq!(got.len(), 5);
    for (i, c) in got.iter().enumerate() {
        assert_eq!(c.id, i + 1);
    }
    let bad = failed(&got);
    assert_eq!(bad.len(), 1);
    assert_eq!(bad[0].id, ID_LOOPBACK);
}

#[test]
fn test_env_prerequisites_dnsmasq_off() {
    let mut m = TestMac::new(healthy_domains());
    m.set_service("dnsmasq", ServiceState::Off);
    let got = m.env().prerequisites(&m.domains);
    assert!(
        failed(&got).is_empty(),
        "a dnsmasq turned off must not keep prerequisites failing: {got:?}"
    );
}

#[test]
fn test_failed() {
    let ok = Check {
        id: 1,
        ok: true,
        ..Check::default()
    };
    let skipped = Check {
        id: 2,
        skipped: true,
        ..Check::default()
    };
    let bad = Check {
        id: 3,
        detail: "missing".into(),
        fix: "lodo setup".into(),
        ..Check::default()
    };
    let worse = Check {
        id: 4,
        detail: "not running".into(),
        fix: "lodo setup".into(),
        ..Check::default()
    };

    assert_eq!(failed(&[]), Vec::<Check>::new());
    assert_eq!(failed(&[ok.clone(), skipped.clone()]), Vec::<Check>::new());
    assert_eq!(
        failed(&[Check {
            id: 1,
            skipped: true,
            off: true,
            ..Check::default()
        }]),
        Vec::<Check>::new()
    );
    assert_eq!(
        failed(&[ok, bad.clone(), skipped, worse.clone()]),
        vec![bad, worse]
    );
}

// =====================================================================
// Ported from `internal/check/probe_test.go`
// =====================================================================

struct ProbeBox {
    _tmp: TempDir,
    paths: Paths,
    fake: FakeRunner,
    dns: String,
    http_port: u16,
    https_port: u16,
    root_cas: Option<Arc<RootCertStore>>,
    timeout: Option<Duration>,
}

impl ProbeBox {
    fn new() -> Self {
        let tmp = tempfile::tempdir().expect("tempdir");
        let paths = Paths::for_test(tmp.path());
        Self {
            _tmp: tmp,
            paths,
            fake: FakeRunner::new(),
            dns: closed_udp(),
            http_port: closed_tcp(),
            https_port: closed_tcp(),
            root_cas: None,
            timeout: None,
        }
    }

    fn env(&self) -> Env<'_> {
        let mut e = Env::new(&self.paths, &self.fake);
        e.dns.clone_from(&self.dns);
        e.http_port = self.http_port;
        e.https_port = self.https_port;
        e.root_cas.clone_from(&self.root_cas);
        e.timeout = self.timeout;
        e
    }
}

#[test]
fn test_result_ok() {
    let cases = [
        (
            "both lookups pass, no port",
            ProbeResult {
                direct: true,
                system: true,
                ..ProbeResult::default()
            },
            true,
        ),
        (
            "dnsmasq failed",
            ProbeResult {
                system: true,
                ..ProbeResult::default()
            },
            false,
        ),
        (
            "macOS failed",
            ProbeResult {
                direct: true,
                ..ProbeResult::default()
            },
            false,
        ),
        (
            "port, caddy reached the app",
            ProbeResult {
                port: 3000,
                direct: true,
                system: true,
                http: true,
                ..ProbeResult::default()
            },
            true,
        ),
        (
            "port, caddy did not reach the app",
            ProbeResult {
                port: 3000,
                direct: true,
                system: true,
                ..ProbeResult::default()
            },
            false,
        ),
        (
            "https, both probes passed",
            ProbeResult {
                port: 3000,
                direct: true,
                system: true,
                http: true,
                secure: true,
                https: true,
                ..ProbeResult::default()
            },
            true,
        ),
        (
            "https, the https probe failed",
            ProbeResult {
                port: 3000,
                direct: true,
                system: true,
                http: true,
                secure: true,
                ..ProbeResult::default()
            },
            false,
        ),
    ];

    for (name, res, want) in cases {
        assert_eq!(res.ok(), want, "case {name:?}");
    }
}

#[test]
fn test_env_probe() {
    let (stopped_port, _none) = app_port("");
    let mut stopped_domain = dashboard_domain();
    stopped_domain.port = stopped_port;

    struct ProbeCase {
        name: &'static str,
        domains: Vec<Domain>,
        dns_overrides: Vec<(&'static str, &'static str)>,
        dns_down: bool,
        macos_overrides: Vec<(&'static str, String)>,
        caddy: Option<HttpHandler>,
        caddy_down: bool,
        hosts: &'static str,
        timeout: Option<Duration>,
        want: Vec<ProbeResult>,
    }

    let retry_cap = Some(Duration::from_millis(250));
    let crm = crm_domain();
    let dashboard = dashboard_domain();

    let cases = vec![
        ProbeCase {
            name: "no enabled domains",
            domains: vec![Domain {
                name: "old.test".into(),
                address: "127.0.1.2".into(),
                ..Domain::default()
            }],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![],
            caddy: None,
            caddy_down: false,
            hosts: "",
            timeout: None,
            want: vec![],
        },
        ProbeCase {
            name: "every probe passes, in store order, without disabled domains",
            domains: vec![
                Domain {
                    name: "flowy.test".into(),
                    address: "127.0.1.3".into(),
                    enabled: true,
                    ..Domain::default()
                },
                dashboard.clone(),
                Domain {
                    name: "old.test".into(),
                    address: "127.0.1.2".into(),
                    ..Domain::default()
                },
                crm.clone(),
            ],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![],
            caddy: None,
            caddy_down: false,
            hosts: "",
            timeout: None,
            want: vec![
                ProbeResult {
                    name: "crm.test".into(),
                    address: "127.0.1.1".into(),
                    direct: true,
                    system: true,
                    ..ProbeResult::default()
                },
                ProbeResult {
                    name: "dashboard.crm.test".into(),
                    address: "127.0.0.1".into(),
                    port: 3000,
                    direct: true,
                    system: true,
                    http: true,
                    ..ProbeResult::default()
                },
                ProbeResult {
                    name: "flowy.test".into(),
                    address: "127.0.1.3".into(),
                    direct: true,
                    system: true,
                    ..ProbeResult::default()
                },
            ],
        },
        ProbeCase {
            name: "dnsmasq answers another address",
            domains: vec![crm.clone()],
            dns_overrides: vec![("crm.test", "127.0.1.9")],
            dns_down: false,
            macos_overrides: vec![],
            caddy: None,
            caddy_down: false,
            hosts: "",
            timeout: retry_cap,
            want: vec![ProbeResult {
                name: "crm.test".into(),
                address: "127.0.1.1".into(),
                system: true,
                detail: "dnsmasq: answered 127.0.1.9, want 127.0.1.1".into(),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "dnsmasq does not know the name",
            domains: vec![crm.clone()],
            dns_overrides: vec![("crm.test", "")],
            dns_down: false,
            macos_overrides: vec![],
            caddy: None,
            caddy_down: false,
            hosts: "",
            timeout: retry_cap,
            want: vec![ProbeResult {
                name: "crm.test".into(),
                address: "127.0.1.1".into(),
                system: true,
                detail: "dnsmasq: no answer".into(),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "dnsmasq is down",
            domains: vec![crm.clone()],
            dns_overrides: vec![],
            dns_down: true,
            macos_overrides: vec![],
            caddy: None,
            caddy_down: false,
            hosts: "",
            timeout: retry_cap,
            want: vec![ProbeResult {
                name: "crm.test".into(),
                address: "127.0.1.1".into(),
                system: true,
                detail: "dnsmasq: no answer".into(),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "macOS has no address",
            domains: vec![crm.clone()],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![("crm.test", String::new())],
            caddy: None,
            caddy_down: false,
            hosts: "",
            timeout: None,
            want: vec![ProbeResult {
                name: "crm.test".into(),
                address: "127.0.1.1".into(),
                direct: true,
                detail: "macOS: no address".into(),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "macOS resolves to other addresses",
            domains: vec![crm.clone()],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![(
                "crm.test",
                macos_output("crm.test", &["127.0.1.9", "127.0.1.8"]),
            )],
            caddy: None,
            caddy_down: false,
            hosts: "",
            timeout: None,
            want: vec![ProbeResult {
                name: "crm.test".into(),
                address: "127.0.1.1".into(),
                direct: true,
                detail: "macOS: 127.0.1.9, 127.0.1.8, want 127.0.1.1".into(),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "the hosts file maps the name elsewhere",
            domains: vec![crm.clone()],
            dns_overrides: vec![("crm.test", "127.0.0.1")],
            dns_down: false,
            macos_overrides: vec![("crm.test", macos_output("crm.test", &["127.0.0.1"]))],
            caddy: None,
            caddy_down: false,
            hosts: "127.0.0.1\tlocalhost\n# old setup\n127.0.0.1 crm.test api.crm.test\n",
            timeout: retry_cap,
            want: vec![ProbeResult {
                name: "crm.test".into(),
                address: "127.0.1.1".into(),
                detail: "{hosts} maps crm.test to 127.0.0.1: remove that line".into(),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "a hosts entry that agrees changes nothing",
            domains: vec![crm.clone()],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![("crm.test", String::new())],
            caddy: None,
            caddy_down: false,
            hosts: "127.0.1.1 crm.test\n",
            timeout: None,
            want: vec![ProbeResult {
                name: "crm.test".into(),
                address: "127.0.1.1".into(),
                direct: true,
                detail: "macOS: no address".into(),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "a commented-out hosts entry is ignored",
            domains: vec![crm.clone()],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![("crm.test", String::new())],
            caddy: None,
            caddy_down: false,
            hosts: "# 127.0.0.1 crm.test\n",
            timeout: None,
            want: vec![ProbeResult {
                name: "crm.test".into(),
                address: "127.0.1.1".into(),
                direct: true,
                detail: "macOS: no address".into(),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "macOS lists the address among others",
            domains: vec![crm],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![(
                "crm.test",
                "name: crm.test\nipv6_address: ::1\nip_address: 127.0.1.9\nip_address: 127.0.1.1\n\n"
                    .into(),
            )],
            caddy: None,
            caddy_down: false,
            hosts: "",
            timeout: None,
            want: vec![ProbeResult {
                name: "crm.test".into(),
                address: "127.0.1.1".into(),
                direct: true,
                system: true,
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "caddy reached an app that answered 500",
            domains: vec![dashboard.clone()],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![],
            caddy: Some(respond(500, &[("Server", "Caddy"), ("Via", "1.1 Caddy")])),
            caddy_down: false,
            hosts: "",
            timeout: None,
            want: vec![ProbeResult {
                name: "dashboard.crm.test".into(),
                address: "127.0.0.1".into(),
                port: 3000,
                direct: true,
                system: true,
                http: true,
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "app down: nothing listens on its port",
            domains: vec![stopped_domain],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![],
            caddy: Some(respond(502, &[("Server", "Caddy")])),
            caddy_down: false,
            hosts: "",
            timeout: None,
            want: vec![ProbeResult {
                name: "dashboard.crm.test".into(),
                address: "127.0.0.1".into(),
                port: stopped_port,
                direct: true,
                system: true,
                down: true,
                detail: format!("app down: nothing answers on 127.0.0.1:{stopped_port}"),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "caddy has no site for the name",
            domains: vec![dashboard.clone()],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![],
            caddy: Some(respond(200, &[("Server", "Caddy")])),
            caddy_down: false,
            hosts: "",
            timeout: None,
            want: vec![ProbeResult {
                name: "dashboard.crm.test".into(),
                address: "127.0.0.1".into(),
                port: 3000,
                direct: true,
                system: true,
                detail: "caddy has no site for dashboard.crm.test: run lodo apply".into(),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "a redirect to https is not followed",
            domains: vec![dashboard.clone()],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![],
            caddy: Some(respond(
                308,
                &[
                    ("Server", "Caddy"),
                    ("Location", "https://dashboard.crm.test/"),
                ],
            )),
            caddy_down: false,
            hosts: "",
            timeout: None,
            want: vec![ProbeResult {
                name: "dashboard.crm.test".into(),
                address: "127.0.0.1".into(),
                port: 3000,
                direct: true,
                system: true,
                detail: "caddy has no site for dashboard.crm.test: run lodo apply".into(),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "another server has the port",
            domains: vec![dashboard.clone()],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![],
            caddy: Some(respond(200, &[("Server", "nginx/1.27.0")])),
            caddy_down: false,
            hosts: "",
            timeout: None,
            want: vec![ProbeResult {
                name: "dashboard.crm.test".into(),
                address: "127.0.0.1".into(),
                port: 3000,
                direct: true,
                system: true,
                detail: "port {port} on 127.0.0.1 answered by nginx/1.27.0, not Caddy".into(),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "a server that does not name itself",
            domains: vec![dashboard.clone()],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![],
            caddy: Some(respond(404, &[])),
            caddy_down: false,
            hosts: "",
            timeout: None,
            want: vec![ProbeResult {
                name: "dashboard.crm.test".into(),
                address: "127.0.0.1".into(),
                port: 3000,
                direct: true,
                system: true,
                detail: "port {port} on 127.0.0.1 answered by something, not Caddy".into(),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "nothing listens on the HTTP port",
            domains: vec![dashboard.clone()],
            dns_overrides: vec![],
            dns_down: false,
            macos_overrides: vec![],
            caddy: None,
            caddy_down: true,
            hosts: "",
            timeout: retry_cap,
            want: vec![ProbeResult {
                name: "dashboard.crm.test".into(),
                address: "127.0.0.1".into(),
                port: 3000,
                direct: true,
                system: true,
                detail: "nothing answers on 127.0.0.1:{port}: is Caddy running?".into(),
                ..ProbeResult::default()
            }],
        },
        ProbeCase {
            name: "every probe fails",
            domains: vec![dashboard],
            dns_overrides: vec![("dashboard.crm.test", "127.0.1.9")],
            dns_down: false,
            macos_overrides: vec![("dashboard.crm.test", String::new())],
            caddy: None,
            caddy_down: true,
            hosts: "",
            timeout: retry_cap,
            want: vec![ProbeResult {
                name: "dashboard.crm.test".into(),
                address: "127.0.0.1".into(),
                port: 3000,
                detail: "dnsmasq: answered 127.0.1.9, want 127.0.0.1; macOS: no address; nothing answers on 127.0.0.1:{port}: is Caddy running?".into(),
                ..ProbeResult::default()
            }],
        },
    ];

    thread::scope(|s| {
        let handles: Vec<_> = cases
            .into_iter()
            .map(|tc| {
                s.spawn(move || {
                    let mut pb = ProbeBox::new();
                    pb.timeout = tc.timeout;
                    if !tc.hosts.is_empty() {
                        write_file(&pb.paths.hosts, tc.hosts);
                    }
                    let mut answers = HashMap::new();
                    for d in &tc.domains {
                        answers.insert(d.name.clone(), d.address.clone());
                        pb.fake.set(
                            dscacheutil(&pb.paths, &d.name),
                            macos_output(&d.name, &[&d.address]),
                        );
                    }
                    for (name, addr) in tc.dns_overrides {
                        answers.insert(name.into(), addr.into());
                    }
                    for (name, out) in tc.macos_overrides {
                        pb.fake.set(dscacheutil(&pb.paths, name), out);
                    }
                    let _dns_srv = (!tc.dns_down).then(|| {
                        let srv = DnsServer::from_map(answers);
                        pb.dns = srv.addr_string();
                        srv
                    });
                    let _http_srv = (!tc.caddy_down).then(|| {
                        let h = tc.caddy.unwrap_or_else(caddy_ok_handler);
                        let srv = HttpServer::start(h);
                        pb.http_port = srv.port();
                        srv
                    });

                    let got = pb.env().probe(&tc.domains);
                    let hosts_disp = pb.paths.hosts.to_string_lossy();
                    let port_str = pb.http_port.to_string();
                    let want: Vec<ProbeResult> = tc
                        .want
                        .into_iter()
                        .map(|mut r| {
                            r.detail = r
                                .detail
                                .replace("{port}", &port_str)
                                .replace("{hosts}", &hosts_disp);
                            r
                        })
                        .collect();
                    assert_eq!(got, want, "case {:?}", tc.name);
                })
            })
            .collect();
        for h in handles {
            h.join().expect("probe case panicked");
        }
    });
}

#[test]
fn test_env_probe_http_request() {
    let dashboard = dashboard_domain();
    let mut pb = ProbeBox::new();
    let _dns_srv = DnsServer::from_map(HashMap::from([(
        dashboard.name.clone(),
        dashboard.address.clone(),
    )]));
    pb.dns = _dns_srv.addr_string();
    pb.fake.set(
        dscacheutil(&pb.paths, &dashboard.name),
        macos_output(&dashboard.name, &[&dashboard.address]),
    );

    let requests: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
    let recorded = Arc::clone(&requests);
    let _http_srv = HttpServer::start(Arc::new(move |req| {
        recorded
            .lock()
            .expect("lock requests")
            .push(format!("{} {} {}", req.method, req.host, req.path));
        HttpReply {
            status: 302,
            headers: vec![
                ("Server".into(), "Caddy".into()),
                ("Location".into(), "/elsewhere".into()),
            ],
        }
    }));
    pb.http_port = _http_srv.port();

    let got = pb.env().probe(&[dashboard]);
    assert_eq!(got.len(), 1);
    assert_eq!(
        got[0].detail,
        "caddy has no site for dashboard.crm.test: run lodo apply"
    );
    assert_eq!(
        *requests.lock().expect("lock requests"),
        vec!["GET dashboard.crm.test /".to_string()]
    );
}

#[test]
fn test_env_probe_retries_while_dnsmasq_starts() {
    let crm = crm_domain();
    let mut pb = ProbeBox::new();
    let ready = Instant::now() + Duration::from_millis(150);
    let crm_name = crm.name.clone();
    let crm_ip: Ipv4Addr = crm.address.parse().expect("valid IPv4");
    let _dns_srv = DnsServer::start(move |name| {
        if Instant::now() < ready || name != crm_name {
            None
        } else {
            Some(crm_ip)
        }
    });
    pb.dns = _dns_srv.addr_string();
    pb.fake.set(
        dscacheutil(&pb.paths, &crm.name),
        macos_output(&crm.name, &[&crm.address]),
    );

    let got = pb.env().probe(&[crm]);
    assert_eq!(got.len(), 1);
    assert!(got[0].direct, "expected late dnsmasq answer: {got:?}");
}

#[test]
fn test_env_probe_retries_while_caddy_starts() {
    let dashboard = dashboard_domain();
    let mut pb = ProbeBox::new();
    let _dns_srv = DnsServer::from_map(HashMap::from([(
        dashboard.name.clone(),
        dashboard.address.clone(),
    )]));
    pb.dns = _dns_srv.addr_string();
    pb.fake.set(
        dscacheutil(&pb.paths, &dashboard.name),
        macos_output(&dashboard.name, &[&dashboard.address]),
    );

    let port = closed_tcp();
    pb.http_port = port;
    let starter = thread::spawn(move || {
        thread::sleep(Duration::from_millis(150));
        let listener = TcpListener::bind(("127.0.0.1", port)).expect("listen on reserved port");
        HttpServer::from_listener(listener, respond(200, &[("Via", "1.1 Caddy")]), None)
    });

    let got = pb.env().probe(&[dashboard]);
    let _late_srv = starter.join().expect("starter thread");
    assert_eq!(got.len(), 1);
    assert!(got[0].http, "expected late Caddy answer: {got:?}");
}

#[test]
fn test_env_probe_domains_at_once() {
    let domains = vec![
        Domain {
            name: "a.test".into(),
            address: "127.0.0.1".into(),
            port: 3000,
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "b.test".into(),
            address: "127.0.0.1".into(),
            port: 3001,
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "c.test".into(),
            address: "127.0.0.1".into(),
            port: 3002,
            enabled: true,
            ..Domain::default()
        },
    ];
    const N: usize = 3;

    let mut pb = ProbeBox::new();
    let mut answers = HashMap::new();
    for d in &domains {
        answers.insert(d.name.clone(), d.address.clone());
        pb.fake.set(
            dscacheutil(&pb.paths, &d.name),
            macos_output(&d.name, &[&d.address]),
        );
    }
    let _dns_srv = DnsServer::from_map(answers);
    pb.dns = _dns_srv.addr_string();

    let arrived = Arc::new((Mutex::new(0usize), Condvar::new()));
    let arrived_h = Arc::clone(&arrived);
    let concurrent_count = Arc::new(AtomicUsize::new(0));
    let concurrent_h = Arc::clone(&concurrent_count);

    let _http_srv = HttpServer::start(Arc::new(move |_| {
        concurrent_h.fetch_add(1, Ordering::SeqCst);
        let (lock, cvar) = &*arrived_h;
        let mut count = lock.lock().expect("lock arrived");
        *count += 1;
        if *count == N {
            cvar.notify_all();
        } else {
            let (guard, _) = cvar
                .wait_timeout_while(count, Duration::from_secs(1), |c| *c < N)
                .expect("wait_timeout_while");
            count = guard;
        }
        if *count >= N {
            HttpReply {
                status: 200,
                headers: vec![("Via".into(), "1.1 Caddy".into())],
            }
        } else {
            HttpReply {
                status: 500,
                headers: vec![],
            }
        }
    }));
    pb.http_port = _http_srv.port();

    let results = pb.env().probe(&domains);
    assert_eq!(concurrent_count.load(Ordering::SeqCst), N);
    for r in &results {
        assert!(
            r.http,
            "{}: {}; want all {N} requests in flight at once",
            r.name, r.detail
        );
    }
}

#[test]
fn test_env_probe_stops_when_timeout_expires() {
    let crm = crm_domain();
    let mut pb = ProbeBox::new();
    let _dns_srv = DnsServer::from_map(HashMap::from([(crm.name.clone(), crm.address.clone())]));
    pb.dns = _dns_srv.addr_string();
    pb.fake.set(
        dscacheutil(&pb.paths, &crm.name),
        macos_output(&crm.name, &[&crm.address]),
    );
    pb.timeout = Some(Duration::ZERO);

    let start = Instant::now();
    let got = pb.env().probe(&[crm]);
    let elapsed = start.elapsed();
    assert!(
        elapsed < Duration::from_secs(1),
        "probe took {elapsed:?} with expired timeout, want immediate stop"
    );
    assert_eq!(got.len(), 1);
    assert!(!got[0].direct);
    assert!(got[0].system);
    assert_eq!(got[0].detail, "dnsmasq: no answer");
}

#[test]
fn test_env_probe_https() {
    let secure = Domain {
        name: "secure.example.com".into(),
        address: "127.0.0.1".into(),
        port: 3000,
        enabled: true,
        https: true,
        ..Domain::default()
    };

    struct HttpsCase {
        name: &'static str,
        caddy: Option<HttpHandler>,
        untrusted: bool,
        down: bool,
        want_https: bool,
        want_detail: &'static str,
    }

    let cases = vec![
        HttpsCase {
            name: "caddy reached the app over https",
            caddy: None,
            untrusted: false,
            down: false,
            want_https: true,
            want_detail: "",
        },
        HttpsCase {
            name: "the certificate is not trusted",
            caddy: None,
            untrusted: true,
            down: false,
            want_https: false,
            want_detail: "https: Caddy's certificate isn't trusted: run lodo setup",
        },
        HttpsCase {
            name: "caddy has no site for the name",
            caddy: Some(respond(200, &[("Server", "Caddy")])),
            untrusted: false,
            down: false,
            want_https: false,
            want_detail: "https: caddy has no site for secure.example.com: run lodo apply",
        },
        HttpsCase {
            name: "the app is down",
            caddy: Some(respond(502, &[("Server", "Caddy")])),
            untrusted: false,
            down: false,
            want_https: false,
            want_detail: "https: app down: nothing answers on 127.0.0.1:3000",
        },
        HttpsCase {
            name: "nothing listens on the https port",
            caddy: None,
            untrusted: false,
            down: true,
            want_https: false,
            want_detail: "https: nothing answers on 127.0.0.1:{port}: is Caddy running?",
        },
    ];

    thread::scope(|s| {
        let handles: Vec<_> = cases
            .into_iter()
            .map(|tc| {
                let secure = secure.clone();
                s.spawn(move || {
                    let mut pb = ProbeBox::new();
                    let _dns_srv = DnsServer::from_map(HashMap::from([(
                        secure.name.clone(),
                        secure.address.clone(),
                    )]));
                    pb.dns = _dns_srv.addr_string();
                    pb.fake.set(
                        dscacheutil(&pb.paths, &secure.name),
                        macos_output(&secure.name, &[&secure.address]),
                    );
                    let _http_srv = HttpServer::start(respond(200, &[("Via", "1.1 Caddy")]));
                    pb.http_port = _http_srv.port();

                    pb.root_cas = Some(if tc.untrusted {
                        Arc::new(RootCertStore::empty())
                    } else {
                        Arc::clone(&TEST_CERT.trusted_roots)
                    });

                    let _tls_srv = (!tc.down).then(|| {
                        let h = tc.caddy.unwrap_or_else(caddy_ok_handler);
                        let srv = HttpServer::start_tls(h);
                        pb.https_port = srv.port();
                        srv
                    });
                    if tc.down {
                        pb.timeout = Some(Duration::from_millis(250));
                    }

                    let got = pb.env().probe(std::slice::from_ref(&secure));
                    let want = ProbeResult {
                        name: secure.name,
                        address: secure.address,
                        port: secure.port,
                        direct: true,
                        system: true,
                        http: true,
                        secure: true,
                        https: tc.want_https,
                        detail: tc.want_detail.replace("{port}", &pb.https_port.to_string()),
                        down: false,
                    };
                    assert_eq!(got, vec![want], "case {:?}", tc.name);
                })
            })
            .collect();
        for h in handles {
            h.join().expect("https case panicked");
        }
    });
}

#[test]
fn test_env_probe_app_down() {
    struct AppDownCase {
        name: &'static str,
        listens: &'static str,
        no_address: bool,
        https: Option<HttpHandler>,
        untrusted: bool,
        want_down: bool,
        want_detail: &'static str,
    }

    let cases = vec![
        AppDownCase {
            name: "the app isn't running",
            listens: "",
            no_address: false,
            https: None,
            untrusted: false,
            want_down: true,
            want_detail: "app down: nothing answers on 127.0.0.1:{app}",
        },
        AppDownCase {
            name: "the app listens on Localhost instead",
            listens: "::1",
            no_address: false,
            https: None,
            untrusted: false,
            want_down: false,
            want_detail: "an app listens on [::1]:{app}, not on the name's address: start it with lodo dev in its project",
        },
        AppDownCase {
            name: "the app answers caddy badly",
            listens: "127.0.0.1",
            no_address: false,
            https: Some(respond(502, &[("Server", "Caddy")])),
            untrusted: false,
            want_down: false,
            want_detail: "app on 127.0.0.1:{app} gave Caddy no HTTP answer: does it serve HTTPS itself?",
        },
        AppDownCase {
            name: "a lookup fails too",
            listens: "",
            no_address: true,
            https: None,
            untrusted: false,
            want_down: false,
            want_detail: "macOS: no address; app down: nothing answers on 127.0.0.1:{app}",
        },
        AppDownCase {
            name: "https finds the same app down, said once",
            listens: "",
            no_address: false,
            https: Some(respond(502, &[("Server", "Caddy")])),
            untrusted: false,
            want_down: true,
            want_detail: "app down: nothing answers on 127.0.0.1:{app}",
        },
        AppDownCase {
            name: "https finds the same app on Localhost, said once",
            listens: "::1",
            no_address: false,
            https: Some(respond(502, &[("Server", "Caddy")])),
            untrusted: false,
            want_down: false,
            want_detail: "an app listens on [::1]:{app}, not on the name's address: start it with lodo dev in its project",
        },
        AppDownCase {
            name: "an untrusted certificate next to app down",
            listens: "",
            no_address: false,
            https: Some(respond(502, &[("Server", "Caddy")])),
            untrusted: true,
            want_down: false,
            want_detail: "app down: nothing answers on 127.0.0.1:{app}; https: Caddy's certificate isn't trusted: run lodo setup",
        },
    ];

    thread::scope(|s| {
        let handles: Vec<_> = cases
            .into_iter()
            .map(|tc| {
                s.spawn(move || {
                    let (port, _app_guard) = app_port(tc.listens);
                    let d = Domain {
                        name: "app.example.com".into(),
                        address: "127.0.0.1".into(),
                        port,
                        enabled: true,
                        https: tc.https.is_some(),
                        ..Domain::default()
                    };

                    let mut pb = ProbeBox::new();
                    let _dns_srv =
                        DnsServer::from_map(HashMap::from([(d.name.clone(), d.address.clone())]));
                    pb.dns = _dns_srv.addr_string();
                    if tc.no_address {
                        pb.fake.set(dscacheutil(&pb.paths, &d.name), "");
                    } else {
                        pb.fake.set(
                            dscacheutil(&pb.paths, &d.name),
                            macos_output(&d.name, &[&d.address]),
                        );
                    }
                    let _http_srv = HttpServer::start(respond(502, &[("Server", "Caddy")]));
                    pb.http_port = _http_srv.port();

                    let _tls_srv = tc.https.map(|h| {
                        let srv = HttpServer::start_tls(h);
                        pb.https_port = srv.port();
                        pb.root_cas = Some(if tc.untrusted {
                            Arc::new(RootCertStore::empty())
                        } else {
                            Arc::clone(&TEST_CERT.trusted_roots)
                        });
                        srv
                    });

                    let got = pb.env().probe(std::slice::from_ref(&d));
                    let want = ProbeResult {
                        name: d.name,
                        address: d.address,
                        port: d.port,
                        direct: true,
                        system: !tc.no_address,
                        http: false,
                        secure: d.https,
                        https: false,
                        detail: tc.want_detail.replace("{app}", &d.port.to_string()),
                        down: tc.want_down,
                    };
                    assert_eq!(got, vec![want], "case {:?}", tc.name);
                })
            })
            .collect();
        for h in handles {
            h.join().expect("app down case panicked");
        }
    });
}

#[test]
fn test_env_probe_shared_port() {
    let (port, _app_guard) = app_port("::1");
    let domains = vec![
        Domain {
            name: "a.test".into(),
            address: "127.0.0.1".into(),
            port,
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "b.test".into(),
            address: "127.0.0.1".into(),
            port,
            enabled: true,
            ..Domain::default()
        },
    ];

    let mut pb = ProbeBox::new();
    let mut answers = HashMap::new();
    for d in &domains {
        answers.insert(d.name.clone(), d.address.clone());
        pb.fake.set(
            dscacheutil(&pb.paths, &d.name),
            macos_output(&d.name, &[&d.address]),
        );
    }
    let _dns_srv = DnsServer::from_map(answers);
    pb.dns = _dns_srv.addr_string();
    let _http_srv = HttpServer::start(respond(502, &[("Server", "Caddy")]));
    pb.http_port = _http_srv.port();

    let got = pb.env().probe(&domains);
    let want: Vec<ProbeResult> = domains
        .iter()
        .map(|d| ProbeResult {
            name: d.name.clone(),
            address: d.address.clone(),
            port,
            direct: true,
            system: true,
            detail: format!(
                "an app listens on [::1]:{port}, not on the name's address: start it with lodo dev in its project"
            ),
            ..ProbeResult::default()
        })
        .collect();
    assert_eq!(got, want);
}

#[test]
fn test_env_probe_https_request() {
    let secure = Domain {
        name: "secure.example.com".into(),
        address: "127.0.0.1".into(),
        port: 3000,
        enabled: true,
        https: true,
        ..Domain::default()
    };

    let mut pb = ProbeBox::new();
    let _dns_srv = DnsServer::from_map(HashMap::from([(
        secure.name.clone(),
        secure.address.clone(),
    )]));
    pb.dns = _dns_srv.addr_string();
    pb.fake.set(
        dscacheutil(&pb.paths, &secure.name),
        macos_output(&secure.name, &[&secure.address]),
    );
    let _http_srv = HttpServer::start(respond(200, &[("Via", "1.1 Caddy")]));
    pb.http_port = _http_srv.port();

    let requests: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
    let recorded = Arc::clone(&requests);
    let _tls_srv = HttpServer::start_tls(Arc::new(move |req| {
        recorded.lock().expect("lock https requests").push(format!(
            "{} {} {} {}",
            req.method, req.host, req.sni, req.path
        ));
        HttpReply {
            status: 200,
            headers: vec![("Via".into(), "1.1 Caddy".into())],
        }
    }));
    pb.https_port = _tls_srv.port();
    pb.root_cas = Some(Arc::clone(&TEST_CERT.trusted_roots));

    let got = pb.env().probe(&[secure]);
    assert_eq!(got.len(), 1);
    assert!(got[0].https, "expected HTTPS probe to pass: {got:?}");
    assert_eq!(
        *requests.lock().expect("lock https requests"),
        vec!["GET secure.example.com secure.example.com /".to_string()]
    );
}

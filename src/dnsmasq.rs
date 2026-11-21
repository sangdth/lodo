//! `dnsmasq.conf` and resolver list generation, plus bounded query-log tailing.

use std::fmt::Write as _;
use std::fs::File;
use std::io;
use std::os::unix::fs::FileExt;
use std::path::Path;

use crate::store::{self, Domain};

/// Address `lodo`'s `dnsmasq` listens on.
pub const LISTEN_ADDRESS: &str = "127.0.0.1";

/// Port `lodo`'s `dnsmasq` listens on (above 1024 so `dnsmasq` runs without root).
pub const PORT: u16 = 53535;

/// Maximum bytes `tail` returns from a single call so a burst of queries cannot flood the log view.
pub const MAX_TAIL: u64 = 64 << 10;

/// Generates `lodo`'s `dnsmasq.conf`: logs every query to `log_path` and answers
/// each enabled domain with its address in `store::sort` order.
pub fn config(domains: &[Domain], log_path: &Path) -> String {
    let mut b = String::from(store::GENERATED_HEADER);
    b.push_str("log-queries\n");
    let _ = writeln!(b, "log-facility={}", log_path.display());
    for d in store::sort(domains) {
        if d.enabled {
            let _ = writeln!(b, "address=/{}/{}", d.name, d.address);
        }
    }
    b
}

/// Generates the enabled domain names, one per line, in `store::sort` order, with no header.
pub fn resolver_list(domains: &[Domain]) -> String {
    let mut b = String::new();
    for d in store::sort(domains) {
        if d.enabled {
            b.push_str(&d.name);
            b.push('\n');
        }
    }
    b
}

/// Returns text appended to `path` since `offset` and the next offset (the file's size).
///
/// A file shorter than `offset` was truncated or replaced, so `tail` reads from `0`.
/// A missing file returns `Ok(("", 0))`. When more than `MAX_TAIL` bytes are new,
/// `tail` returns only the last `MAX_TAIL` bytes without a leading partial line
/// (unless the window contains no newline, in which case the whole `MAX_TAIL` window is returned).
pub fn tail(path: &Path, offset: u64) -> Result<(String, u64), String> {
    let f = match File::open(path) {
        Ok(f) => f,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok((String::new(), 0)),
        Err(e) => return Err(format!("read dnsmasq log: {e}")),
    };

    let info = f.metadata().map_err(|e| format!("read dnsmasq log: {e}"))?;
    let size = info.len();
    let mut start = if size < offset { 0 } else { offset };
    let window = size - start > MAX_TAIL;
    if window {
        // Read one byte before the window so a '\n' right before it shows
        // that the window already starts on a line boundary.
        start = size - MAX_TAIL - 1;
    }

    let len = usize::try_from(size - start).map_err(|e| format!("read dnsmasq log: {e}"))?;
    let mut buf = vec![0u8; len];
    let n = read_at_until_eof(&f, &mut buf, start).map_err(|e| format!("read dnsmasq log: {e}"))?;
    let mut slice = &buf[..n];
    if window {
        if let Some(i) = slice.iter().position(|&b| b == b'\n') {
            slice = &slice[i + 1..];
        } else if !slice.is_empty() {
            slice = &slice[1..];
        }
    }
    Ok((String::from_utf8_lossy(slice).into_owned(), size))
}

fn read_at_until_eof(file: &File, mut buf: &mut [u8], mut offset: u64) -> io::Result<usize> {
    let mut total = 0;
    while !buf.is_empty() {
        match file.read_at(buf, offset) {
            Ok(0) => break,
            Ok(n) => {
                total += n;
                offset += n as u64;
                buf = &mut buf[n..];
            }
            Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
            Err(e) => return Err(e),
        }
    }
    Ok(total)
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::process::Command;

    use super::*;

    const DNSMASQ_BIN: &str = "/opt/homebrew/opt/dnsmasq/sbin/dnsmasq";
    const LOG_PATH: &str = "/Users/tester/.config/lodo/dnsmasq.log";

    fn sample_domains() -> Vec<Domain> {
        vec![
            Domain {
                name: "flowy.test".into(),
                address: "127.0.1.3".into(),
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "dashboard.crm.test".into(),
                address: "127.0.1.1".into(),
                port: 3000,
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "old.test".into(),
                address: "127.0.0.1".into(),
                ..Domain::default()
            },
            Domain {
                name: "a.api.crm.test".into(),
                address: "127.0.1.1".into(),
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "crm.test".into(),
                address: "127.0.1.1".into(),
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "test.crm.test".into(),
                address: "127.0.1.4".into(),
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "api.crm.test".into(),
                address: "127.0.1.1".into(),
                enabled: true,
                ..Domain::default()
            },
        ]
    }

    #[test]
    fn config_matches_go_goldens() {
        let sample_golden = include_str!("../tests/testdata/dnsmasq/TestConfig/sample.golden");
        let empty_golden = include_str!("../tests/testdata/dnsmasq/TestConfig/empty.golden");

        assert_eq!(
            config(&sample_domains(), Path::new(LOG_PATH)),
            sample_golden
        );
        assert_eq!(config(&[], Path::new(LOG_PATH)), empty_golden);
    }

    #[test]
    fn config_dnsmasq_accepts() {
        let bin = Path::new(DNSMASQ_BIN);
        if !bin.is_file() {
            return;
        }
        for (name, domains) in [("sample", sample_domains()), ("empty", Vec::new())] {
            let dir = tempfile::tempdir().unwrap();
            let conf = dir.path().join("dnsmasq.conf");
            let log = dir.path().join("dnsmasq.log");
            let rendered = config(&domains, &log);
            fs::write(&conf, &rendered).unwrap();

            let out = Command::new(bin)
                .arg("--test")
                .arg(format!("--conf-file={}", conf.display()))
                .output()
                .unwrap();
            assert!(
                out.status.success(),
                "case {name}: dnsmasq rejected config:\n{}\n{}\nconfig:\n{rendered}",
                String::from_utf8_lossy(&out.stdout),
                String::from_utf8_lossy(&out.stderr),
            );
        }
    }

    #[test]
    fn resolver_list_matches_go_goldens() {
        let sample_golden =
            include_str!("../tests/testdata/dnsmasq/TestResolverList/sample.golden");
        let empty_golden = include_str!("../tests/testdata/dnsmasq/TestResolverList/empty.golden");

        assert_eq!(resolver_list(&sample_domains()), sample_golden);
        assert_eq!(resolver_list(&[]), empty_golden);
    }

    fn lines(width: usize, from: usize, to: usize) -> String {
        let mut b = String::new();
        for i in from..to {
            let prefix = format!("line {i} ");
            b.push_str(&prefix);
            b.push_str(&".".repeat(width - 1 - prefix.len()));
            b.push('\n');
        }
        b
    }

    fn write_opt(path: &Path, content: Option<&str>) {
        match content {
            None => {
                if let Err(e) = fs::remove_file(path) {
                    assert_eq!(e.kind(), io::ErrorKind::NotFound);
                }
            }
            Some(s) => fs::write(path, s.as_bytes()).unwrap(),
        }
    }

    #[test]
    fn tail_handles_append_truncation_missing_and_windowing() {
        let exact_max = lines(64, 0, 1024);
        let over_before = String::from("a\n");
        let over_after = format!("a\n{}", lines(100, 0, 1000));
        let over_want = lines(100, 345, 1000);
        let boundary_after = lines(64, 0, 1100);
        let boundary_want = lines(64, 76, 1100);
        let no_nl_after = "x".repeat(MAX_TAIL as usize + 10);
        let no_nl_want = "x".repeat(MAX_TAIL as usize);

        struct Case<'a> {
            name: &'a str,
            before: Option<&'a str>,
            after: Option<&'a str>,
            want: &'a str,
            want_offset: u64,
        }

        let cases = [
            Case {
                name: "missing file",
                before: None,
                after: None,
                want: "",
                want_offset: 0,
            },
            Case {
                name: "file appears",
                before: None,
                after: Some("a\n"),
                want: "a\n",
                want_offset: 2,
            },
            Case {
                name: "append",
                before: Some("a\n"),
                after: Some("a\nb\nc"),
                want: "b\nc",
                want_offset: 5,
            },
            Case {
                name: "nothing new",
                before: Some("a\n"),
                after: Some("a\n"),
                want: "",
                want_offset: 2,
            },
            Case {
                name: "truncated",
                before: Some("a\nb\n"),
                after: Some("c\n"),
                want: "c\n",
                want_offset: 2,
            },
            Case {
                name: "file removed",
                before: Some("a\n"),
                after: None,
                want: "",
                want_offset: 0,
            },
            Case {
                name: "gap of exactly MaxTail comes back whole",
                before: None,
                after: Some(&exact_max),
                want: &exact_max,
                want_offset: 65536,
            },
            Case {
                name: "gap over MaxTail starts at the next line",
                before: Some(&over_before),
                after: Some(&over_after),
                want: &over_want,
                want_offset: 100002,
            },
            Case {
                name: "gap over MaxTail on a line boundary keeps the first line",
                before: None,
                after: Some(&boundary_after),
                want: &boundary_want,
                want_offset: 70400,
            },
            Case {
                name: "gap over MaxTail with no line break",
                before: None,
                after: Some(&no_nl_after),
                want: &no_nl_want,
                want_offset: MAX_TAIL + 10,
            },
        ];

        for tc in cases {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join("dnsmasq.log");
            write_opt(&path, tc.before);
            let (_, offset) =
                tail(&path, 0).unwrap_or_else(|e| panic!("{}: first call: {e}", tc.name));
            write_opt(&path, tc.after);

            let (got, got_offset) =
                tail(&path, offset).unwrap_or_else(|e| panic!("{}: second call: {e}", tc.name));
            assert_eq!(got, tc.want, "case {}", tc.name);
            assert_eq!(got_offset, tc.want_offset, "case {}", tc.name);
        }
    }

    #[test]
    fn tail_error_preserves_prefix() {
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("file");
        fs::write(&file, b"").unwrap();
        let err = tail(&file.join("dnsmasq.log"), 7).unwrap_err();
        assert!(
            err.starts_with("read dnsmasq log:"),
            "unexpected error: {err}"
        );
    }
}

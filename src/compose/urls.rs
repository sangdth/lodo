//! Localhost URL detection and rewriting to project domain names.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::LazyLock;

use regex::Regex;

/// Bytes immediately following a `localhost` / `127.0.0.1` match that show the host continues
/// (such as `localhost.example.com` or `localhost@example.com`).
const HOST_CHARS: &[u8] = b"0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz.-_@";

#[expect(clippy::expect_used)] // compile-time valid static regex
static URL_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(
        r"(?i)(?-u:\b)([a-z][a-z0-9+.-]*)://([^\t\n\x0c\r /\\?#]*@)?(?:localhost|127\.0\.0\.1)(?::([0-9]{1,5}))?",
    )
    .expect("valid URL_RE")
});

/// Project domain routing table used to point `localhost` URLs at `lodo` names.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Values {
    /// Project's own domain name (e.g. `flowy.lodo`).
    pub domain: String,
    /// Enabled domain names served by Caddy on the project's address, keyed by port.
    pub names: BTreeMap<u16, String>,
    /// Ports in `names` that Caddy also serves over HTTPS.
    pub secure: BTreeSet<u16>,
}

/// Parsed prefix (`scheme://[userinfo@]host[:port]`) of a URL targeting this Mac's loopback host.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct LocalUrl {
    pub scheme: String,
    pub user_info: String,
    pub port: String,
}

impl LocalUrl {
    /// Reports whether `self` uses `http`, `https`, `ws`, or `wss` (case-insensitive).
    pub fn is_web(&self) -> bool {
        ["http", "https", "ws", "wss"]
            .iter()
            .any(|s| self.scheme.eq_ignore_ascii_case(s))
    }
}

/// Replaces each `localhost` / `127.0.0.1` URL prefix in `s` with the string returned by `to`.
/// Returning `None` from `to` leaves that URL untouched. Also returns whether any URL was replaced.
pub fn map_urls(s: &str, mut to: impl FnMut(&LocalUrl) -> Option<String>) -> (String, bool) {
    let mut out = String::new();
    let mut last = 0;
    let mut changed = false;
    let bytes = s.as_bytes();

    for caps in URL_RE.captures_iter(s) {
        let Some(full) = caps.get(0) else {
            continue;
        };
        if bytes
            .get(full.end())
            .is_some_and(|b| HOST_CHARS.contains(b))
        {
            continue;
        }
        let u = LocalUrl {
            scheme: caps
                .get(1)
                .map_or_else(String::new, |m| m.as_str().to_string()),
            user_info: caps
                .get(2)
                .map_or_else(String::new, |m| m.as_str().to_string()),
            port: caps
                .get(3)
                .map_or_else(String::new, |m| m.as_str().to_string()),
        };
        let Some(replacement) = to(&u) else {
            continue;
        };
        out.push_str(&s[last..full.start()]);
        out.push_str(&replacement);
        last = full.end();
        changed = true;
    }

    if !changed {
        return (s.to_string(), false);
    }
    out.push_str(&s[last..]);
    (out, true)
}

impl Values {
    /// Rewrites `u` to point at this project.
    ///
    /// An `http` or `ws` URL on a port in `self.names` uses that name without a port (since Caddy
    /// serves port 80), sets `named = true`, and upgrades to `https` / `wss` when `self.secure`
    /// contains that port. Any other URL uses `self.domain` and keeps its port and scheme case.
    pub fn point(&self, u: &LocalUrl) -> (String, bool) {
        let mut scheme = u.scheme.clone();
        let mut host = self.domain.clone();
        let mut port = u.port.clone();
        let mut named = false;

        let plain = scheme.eq_ignore_ascii_case("http") || scheme.eq_ignore_ascii_case("ws");
        if plain
            && let Ok(n) = port.parse::<u16>()
            && let Some(name) = self.names.get(&n)
            && !name.is_empty()
        {
            host.clone_from(name);
            port.clear();
            named = true;
            if self.secure.contains(&n) {
                scheme = secure_scheme(&scheme).to_string();
            }
        }

        let mut url = format!("{scheme}://{}{host}", u.user_info);
        if !port.is_empty() {
            url.push(':');
            url.push_str(&port);
        }
        (url, named)
    }
}

fn secure_scheme(scheme: &str) -> &'static str {
    if scheme.eq_ignore_ascii_case("ws") {
        "wss"
    } else {
        "https"
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn url(scheme: &str, user_info: &str, port: &str) -> LocalUrl {
        LocalUrl {
            scheme: scheme.to_string(),
            user_info: user_info.to_string(),
            port: port.to_string(),
        }
    }

    #[test]
    fn map_urls_parsing_and_filtering() {
        struct Case {
            name: &'static str,
            input: &'static str,
            keep: &'static str,
            want: Option<&'static str>,
            want_urls: Vec<LocalUrl>,
        }
        let cases = [
            Case {
                name: "user info with an @",
                input: "postgres://u:p@ss@localhost:5434/db",
                keep: "",
                want: Some("X/db"),
                want_urls: vec![url("postgres", "u:p@ss@", "5434")],
            },
            Case {
                name: "password alone",
                input: "redis://:pw@127.0.0.1:6379",
                keep: "",
                want: Some("X"),
                want_urls: vec![url("redis", ":pw@", "6379")],
            },
            Case {
                name: "scheme after another",
                input: "jdbc:postgresql://localhost:5432",
                keep: "",
                want: Some("jdbc:X"),
                want_urls: vec![url("postgresql", "", "5432")],
            },
            Case {
                name: "longer host",
                input: "http://localhost.example.com",
                keep: "",
                want: None,
                want_urls: vec![],
            },
            Case {
                name: "address in a longer host",
                input: "http://127.0.0.1.nip.io:3000",
                keep: "",
                want: None,
                want_urls: vec![],
            },
            Case {
                name: "port too long",
                input: "http://localhost:123456",
                keep: "",
                want: None,
                want_urls: vec![],
            },
            Case {
                name: "port from a variable",
                input: "http://localhost:${PORT}",
                keep: "",
                want: Some("X:${PORT}"),
                want_urls: vec![url("http", "", "")],
            },
            Case {
                name: "the callback keeps one",
                input: "redis://localhost:6379 http://localhost:3000",
                keep: "redis",
                want: Some("redis://localhost:6379 X"),
                want_urls: vec![url("redis", "", "6379"), url("http", "", "3000")],
            },
            Case {
                name: "the callback keeps all",
                input: "http://localhost:3000",
                keep: "http",
                want: None,
                want_urls: vec![url("http", "", "3000")],
            },
        ];

        for c in &cases {
            let mut seen = Vec::new();
            let (got, changed) = map_urls(c.input, |u| {
                seen.push(u.clone());
                (u.scheme != c.keep).then(|| "X".to_string())
            });
            let expected = c.want.unwrap_or(c.input);
            assert_eq!(got, expected, "case {}", c.name);
            assert_eq!(changed, c.want.is_some(), "case {} changed", c.name);
            assert_eq!(seen, c.want_urls, "case {} urls", c.name);
        }
    }

    #[test]
    fn values_point_and_web_urls() {
        let values = Values {
            domain: "flowy.test".into(),
            names: BTreeMap::from([
                (3000, "dashboard.flowy.test".into()),
                (4000, "api.flowy.test".into()),
            ]),
            secure: BTreeSet::from([4000]),
        };

        let point_cases: &[(&str, LocalUrl, &str, bool)] = &[
            (
                "named http drops its port",
                url("http", "", "3000"),
                "http://dashboard.flowy.test",
                true,
            ),
            (
                "secure name gives https",
                url("http", "", "4000"),
                "https://api.flowy.test",
                true,
            ),
            (
                "secure name gives wss",
                url("ws", "", "4000"),
                "wss://api.flowy.test",
                true,
            ),
            (
                "upper case ws scheme becomes wss",
                url("WS", "", "4000"),
                "wss://api.flowy.test",
                true,
            ),
            (
                "https keeps a named port",
                url("https", "", "3000"),
                "https://flowy.test:3000",
                false,
            ),
            (
                "user info and scheme case stay",
                url("HTTP", "u:p@", "3000"),
                "HTTP://u:p@dashboard.flowy.test",
                true,
            ),
            (
                "other scheme keeps its port",
                url("postgres", "u:p@", "3000"),
                "postgres://u:p@flowy.test:3000",
                false,
            ),
            ("no port", url("http", "", ""), "http://flowy.test", false),
        ];

        for (name, u, want, want_named) in point_cases {
            assert_eq!(
                values.point(u),
                (want.to_string(), *want_named),
                "case {name}"
            );
        }

        // Verify web URL mapping parity (including security edge cases like backslash before @ and foo-http://).
        let flowy = Values {
            domain: "flowy.test".into(),
            names: BTreeMap::from([(3000, "dashboard.flowy.test".into())]),
            secure: BTreeSet::new(),
        };
        let point_web = |s: &str| map_urls(s, |u| u.is_web().then(|| flowy.point(u).0));

        assert_eq!(
            point_web("http://localhost:3000/api/auth?next=/"),
            ("http://dashboard.flowy.test/api/auth?next=/".into(), true)
        );
        assert_eq!(
            point_web("${BETTER_AUTH_URL:-http://localhost:3000}"),
            (
                "${BETTER_AUTH_URL:-http://dashboard.flowy.test}".into(),
                true
            )
        );
        assert_eq!(
            point_web("http://localhost:3000,https://127.0.0.1:8443"),
            (
                "http://dashboard.flowy.test,https://flowy.test:8443".into(),
                true
            )
        );
        for untouched in [
            "localhost",
            "localhost:9222",
            "127.0.0.1",
            "postgres://postgres:postgres@localhost:5432/flowy",
            "http://localhost.example.com",
            "http://127.0.0.1.nip.io:3000",
            "http://localhost:123456",
            "http://localhost@example.com",
            "xhttp://localhost:3000",
            "foo-http://localhost:3000",
            r"http://evil.com\@localhost:3000",
        ] {
            assert_eq!(
                point_web(untouched),
                (untouched.to_string(), false),
                "expected untouched: {untouched}"
            );
        }
    }
}

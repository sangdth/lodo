//! Dev-server command and port detection across `package.json` scripts, JS/TS configs, `.sh` scripts, and `.env`.

use std::collections::{BTreeMap, BTreeSet};
use std::path::Path;
use std::sync::LazyLock;

use regex::Regex;
use serde::Deserialize;

use crate::compose::{clean_path, is_local_path};
use crate::fsutil;

use super::env_port;

pub(crate) struct Tool {
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) name: &'static str,
    cmd: Regex,
    verbs: &'static [&'static str],
    port: u16,
    configs: Vec<String>,
    dotenv: bool,
}

fn config_names(base: &str) -> Vec<String> {
    [".ts", ".js", ".mjs", ".mts", ".cjs", ".cts"]
        .iter()
        .map(|ext| format!("{base}{ext}"))
        .collect()
}

#[expect(clippy::expect_used)] // compile-time valid static regexes
static TOOLS: LazyLock<Vec<Tool>> = LazyLock::new(|| {
    vec![
        Tool {
            name: "next",
            cmd: Regex::new(r"(?-u:\b)next[ \t]+dev(?-u:\b)").expect("valid next regex"),
            verbs: &[],
            port: 3000,
            configs: Vec::new(),
            dotenv: false,
        },
        Tool {
            name: "vite",
            cmd: Regex::new(r"(?-u:\b)vite(?::dev)?(?-u:\b)(?:[ \t]+([a-z][0-9A-Za-z_-]*))?")
                .expect("valid vite regex"),
            verbs: &["dev", "serve"],
            port: 5173,
            configs: config_names("vite.config"),
            dotenv: false,
        },
        Tool {
            name: "astro",
            cmd: Regex::new(r"(?-u:\b)astro[ \t]+dev(?-u:\b)").expect("valid astro regex"),
            verbs: &[],
            port: 4321,
            configs: config_names("astro.config"),
            dotenv: false,
        },
        Tool {
            name: "nuxt",
            cmd: Regex::new(r"(?-u:\b)nux[it][ \t]+dev(?-u:\b)").expect("valid nuxt regex"),
            verbs: &[],
            port: 3000,
            configs: Vec::new(),
            dotenv: false,
        },
        Tool {
            name: "wrangler",
            cmd: Regex::new(r"(?-u:\b)wrangler[ \t]+dev(?-u:\b)").expect("valid wrangler regex"),
            verbs: &[],
            port: 8787,
            configs: Vec::new(),
            dotenv: false,
        },
        Tool {
            name: "react-router",
            cmd: Regex::new(r"(?-u:\b)react-router[ \t]+dev(?-u:\b)")
                .expect("valid react-router regex"),
            verbs: &[],
            port: 5173,
            configs: Vec::new(),
            dotenv: false,
        },
        Tool {
            name: "angular",
            cmd: Regex::new(r"(?-u:\b)ng[ \t]+serve(?-u:\b)").expect("valid angular regex"),
            verbs: &[],
            port: 4200,
            configs: Vec::new(),
            dotenv: false,
        },
        Tool {
            name: "gatsby",
            cmd: Regex::new(r"(?-u:\b)gatsby[ \t]+develop(?-u:\b)").expect("valid gatsby regex"),
            verbs: &[],
            port: 8000,
            configs: Vec::new(),
            dotenv: false,
        },
        Tool {
            name: "docusaurus",
            cmd: Regex::new(r"(?-u:\b)docusaurus[ \t]+start(?-u:\b)")
                .expect("valid docusaurus regex"),
            verbs: &[],
            port: 3000,
            configs: Vec::new(),
            dotenv: false,
        },
        Tool {
            name: "storybook",
            cmd: Regex::new(r"(?-u:\b)storybook[ \t]+dev(?-u:\b)").expect("valid storybook regex"),
            verbs: &[],
            port: 6006,
            configs: Vec::new(),
            dotenv: false,
        },
        Tool {
            name: "vue-cli",
            cmd: Regex::new(r"(?-u:\b)vue-cli-service[ \t]+serve(?-u:\b)")
                .expect("valid vue-cli regex"),
            verbs: &[],
            port: 8080,
            configs: Vec::new(),
            dotenv: false,
        },
        Tool {
            name: "webpack",
            cmd: Regex::new(r"(?-u:\b)webpack(?:-dev-server(?-u:\b)|[ \t]+serve(?-u:\b))")
                .expect("valid webpack regex"),
            verbs: &[],
            port: 8080,
            configs: Vec::new(),
            dotenv: false,
        },
        Tool {
            name: "nest",
            cmd: Regex::new(r"(?-u:\b)nest[ \t]+start(?-u:\b)").expect("valid nest regex"),
            verbs: &[],
            port: 0,
            configs: Vec::new(),
            dotenv: true,
        },
        Tool {
            name: "create-react-app",
            cmd: Regex::new(r"(?-u:\b)react-scripts[ \t]+start(?-u:\b)").expect("valid cra regex"),
            verbs: &[],
            port: 3000,
            configs: Vec::new(),
            dotenv: true,
        },
        Tool {
            name: "react-email",
            cmd: Regex::new(r"(?-u:\b)email[ \t]+dev(?-u:\b)").expect("valid react-email regex"),
            verbs: &[],
            port: 3000,
            configs: Vec::new(),
            dotenv: false,
        },
    ]
});

#[expect(clippy::expect_used)] // compile-time valid static regex
static PORT_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"(?:^|[ \t])(?:-p|--port)(?:=|[ \t]+)([^ \t"']*)"#).expect("valid PORT_RE")
});

#[expect(clippy::expect_used)] // compile-time valid static regex
static ENV_PORT_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"(?:^|[\t\n\x0c\r "'])PORT=([0-9]+)(?-u:\b)"#).expect("valid ENV_PORT_RE")
});

#[expect(clippy::expect_used)] // compile-time valid static regex
static CONFIG_PORT_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?-u:\b)port\s*:\s*([0-9]+(?-u:\b))?").expect("valid CONFIG_PORT_RE")
});

#[expect(clippy::expect_used)] // compile-time valid static regex
static PREVIEW_BLOCK_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?-u:\b)preview\s*:\s*\{[^}]*\}").expect("valid PREVIEW_BLOCK_RE")
});

#[expect(clippy::expect_used)] // compile-time valid static regex
static TS_NUMBER_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^\s*number(?:(?-u:\b)|$)").expect("valid TS_NUMBER_RE"));

#[expect(clippy::expect_used)] // compile-time valid static regex
static SHELL_SCRIPT_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(
        r#"(?:^|[\t\n\x0c\r "'=])(?:\./)?((?:[0-9A-Za-z_.-]+/)*[0-9A-Za-z_.-]+\.sh)(?-u:\b)"#,
    )
    .expect("valid SHELL_SCRIPT_RE")
});

#[expect(clippy::expect_used)] // compile-time valid static regex
static RUN_SCRIPT_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(
        r"(?:(?-u:\b)npm:|(?-u:\b)(?:npm|pnpm|yarn|bun)[ \t]+(?:run[ \t]+)?)([0-9A-Za-z_:-]+)",
    )
    .expect("valid RUN_SCRIPT_RE")
});

#[derive(Default)]
pub(crate) struct Server {
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) tool: Option<&'static Tool>,
    pub(crate) port: u16,
}

struct Start<'a> {
    tool: &'static Tool,
    from: usize,
    args_text: &'a str,
}

impl Tool {
    fn accepts(
        &self,
        line: &str,
        full_start: usize,
        full_end: usize,
        verb_start: Option<usize>,
    ) -> bool {
        let mut end = full_end;
        let bytes = line.as_bytes();
        if let Some(vs) = verb_start {
            end = vs.saturating_sub(1);
            while end > full_start && matches!(bytes.get(end), Some(b' ' | b'\t')) {
                end -= 1;
            }
            end += 1;
        }
        bytes
            .get(end)
            .is_none_or(|b| !matches!(b, b'-' | b'.' | b'/' | b':' | b'@'))
    }
}

fn starts(line: &str, shell: bool) -> Vec<Start<'_>> {
    let ends: &[char] = if shell {
        &['&', ';', '|']
    } else {
        &['&', ';', '|', '"']
    };
    let mut found = Vec::new();
    for t in TOOLS.iter() {
        for caps in t.cmd.captures_iter(line) {
            let Some(full) = caps.get(0) else {
                continue;
            };
            let verb_match = caps.get(1);
            if !t.accepts(
                line,
                full.start(),
                full.end(),
                verb_match.map(|m| m.start()),
            ) {
                continue;
            }
            if let Some(verb) = verb_match
                && !t.verbs.contains(&verb.as_str())
            {
                continue;
            }
            let cmd_end = full.end();
            let args_end = line[cmd_end..]
                .find(ends)
                .map_or(line.len(), |i| cmd_end + i);
            found.push(Start {
                tool: t,
                from: full.start(),
                args_text: &line[cmd_end..args_end],
            });
        }
    }
    found.sort_by_key(|s| s.from);
    found
}

fn server_in(text: &str, dir: &Path, shell: bool) -> Option<Server> {
    for raw_line in text.split('\n') {
        let line = raw_line.trim_end_matches(['\r', '\n']);
        if shell && line.trim_start().starts_with('#') {
            continue;
        }
        let found = starts(line, shell);
        let Some(s) = found.first() else {
            continue;
        };
        let mut before = &line[..s.from];
        if let Some(idx) = before.rfind(['&', ';', '|']) {
            before = &before[idx + 1..];
        }
        let port = if let Some(caps) = PORT_RE.captures(s.args_text) {
            parse_valid_port(caps.get(1).map_or("", |m| m.as_str()))
        } else if let Some(caps) = ENV_PORT_RE.captures(before) {
            parse_valid_port(caps.get(1).map_or("", |m| m.as_str()))
        } else {
            config_port(dir, s.tool)
        };
        return Some(Server {
            tool: Some(s.tool),
            port,
        });
    }
    None
}

fn parse_valid_port(s: &str) -> u16 {
    s.parse::<u16>().ok().filter(|&p| p > 0).unwrap_or(0)
}

fn config_port(dir: &Path, tool: &Tool) -> u16 {
    for name in &tool.configs {
        let Ok(data) = fsutil::read_regular(&dir.join(name)) else {
            continue;
        };
        let stripped = strip_js_comments(&String::from_utf8_lossy(&data));
        let clean = PREVIEW_BLOCK_RE.replace_all(&stripped, "");
        for caps in CONFIG_PORT_RE.captures_iter(&clean) {
            let Some(full) = caps.get(0) else {
                continue;
            };
            if let Some(port_match) = caps.get(1) {
                return parse_valid_port(port_match.as_str());
            }
            if TS_NUMBER_RE.is_match(&clean[full.end()..]) {
                continue;
            }
            return 0;
        }
        return tool.port;
    }
    if tool.dotenv {
        let p = env_port(dir);
        if p > 0 {
            return p;
        }
    }
    tool.port
}

/// Strips `//` line comments and `/* ... */` block comments while preserving quoted literals.
fn strip_js_comments(s: &str) -> String {
    let bytes = s.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;

    while i < bytes.len() {
        let c = bytes[i];
        if matches!(c, b'\'' | b'"' | b'`') {
            out.push(c);
            i += 1;
            while i < bytes.len() {
                out.push(bytes[i]);
                if bytes[i] == b'\\' && i + 1 < bytes.len() {
                    i += 1;
                    out.push(bytes[i]);
                } else if bytes[i] == c {
                    break;
                }
                i += 1;
            }
        } else if c == b'/' && bytes.get(i + 1) == Some(&b'/') {
            i += 2;
            while i < bytes.len() && bytes[i] != b'\n' {
                i += 1;
            }
            if i < bytes.len() {
                out.push(b'\n');
            }
        } else if c == b'/' && bytes.get(i + 1) == Some(&b'*') {
            i += 2;
            while i + 1 < bytes.len() && !(bytes[i] == b'*' && bytes[i + 1] == b'/') {
                i += 1;
            }
            i += 1;
            out.push(b' ');
        } else {
            out.push(c);
        }
        i += 1;
    }

    String::from_utf8(out).unwrap_or_default()
}

#[derive(Deserialize, Default)]
pub(crate) struct PackageJson {
    #[serde(default)]
    pub(crate) scripts: BTreeMap<String, String>,
    #[serde(default)]
    pub(crate) workspaces: Option<serde_json::Value>,
}

pub(crate) fn read_package(dir: &Path) -> Option<PackageJson> {
    let data = fsutil::read_regular(&dir.join("package.json")).ok()?;
    serde_json::from_slice(&data).ok()
}

fn shell_scripts(scripts: &[String]) -> Vec<String> {
    let mut found = BTreeSet::new();
    for script in scripts {
        for caps in SHELL_SCRIPT_RE.captures_iter(script) {
            let Some(m) = caps.get(1) else {
                continue;
            };
            let p = Path::new(m.as_str());
            if is_local_path(p) {
                found.insert(clean_path(p).to_string_lossy().into_owned());
            }
        }
    }
    found.into_iter().collect()
}

fn dev_scripts(pkg: &PackageJson) -> Vec<String> {
    let Some(dev) = pkg.scripts.get("dev") else {
        return Vec::new();
    };
    let mut texts = vec![dev.clone()];
    for caps in RUN_SCRIPT_RE.captures_iter(dev) {
        if let Some(m) = caps.get(1) {
            let name = m.as_str();
            if name != "dev"
                && let Some(script) = pkg.scripts.get(name)
            {
                texts.push(script.clone());
            }
        }
    }
    texts
}

/// Inspects `dir/package.json` for a `dev` script and returns `(server, known_tool, has_dev_script)`.
pub(crate) fn dev_server(dir: &Path) -> (Server, bool, bool) {
    let Some(pkg) = read_package(dir) else {
        return (Server::default(), false, false);
    };
    let Some(dev) = pkg.scripts.get("dev").cloned() else {
        return (Server::default(), false, false);
    };

    let texts = dev_scripts(&pkg);
    for text in &texts {
        if let Some(srv) = server_in(text, dir, false) {
            return (srv, true, true);
        }
    }

    for rel in shell_scripts(&texts) {
        let Ok(data) = fsutil::read_regular(&dir.join(rel)) else {
            continue;
        };
        if let Some(srv) = server_in(&String::from_utf8_lossy(&data), dir, true) {
            return (srv, true, true);
        }
    }

    let mut port = PORT_RE
        .captures(&dev)
        .and_then(|c| c.get(1))
        .map_or(0, |m| parse_valid_port(m.as_str()));
    if port == 0 {
        port = env_port(dir);
    }
    (Server { tool: None, port }, false, true)
}

#[cfg(test)]
mod tests {
    use std::fs;

    use super::*;

    fn write_file(path: &Path, content: &str) {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent).unwrap();
        }
        fs::write(path, content).unwrap();
    }

    #[test]
    fn server_in_frameworks_and_configs() {
        type FilePair = (&'static str, &'static str);
        let cases: &[(&str, &str, &[FilePair], &str, u16)] = &[
            ("next", "next dev --turbopack", &[], "next", 3000),
            ("-p", "next dev -p 3002", &[], "next", 3002),
            ("--port=", "vite --port=3000", &[], "vite", 3000),
            ("PORT before it", "PORT=3100 next dev", &[], "next", 3100),
            ("a port variable", "next dev -p $PORT", &[], "next", 0),
            (
                "after another command",
                "next typegen && TZ=UTC next dev -p 3411",
                &[],
                "next",
                3411,
            ),
            (
                "vite config",
                "vite",
                &[("vite.config.ts", "server: { port: 5171, strictPort: true }")],
                "vite",
                5171,
            ),
            (
                "vite config with comments and preview",
                "vite",
                &[(
                    "vite.config.ts",
                    "// port: 8080\n/* port: 9000 */\nexport default {\n  preview: { port: 4173 },\n  server: { port: 5175 }\n}",
                )],
                "vite",
                5175,
            ),
            (
                "vite config with typescript port type",
                "vite",
                &[(
                    "vite.config.ts",
                    "type Opts = { port: number };\nexport default { server: { port: 5176 } }",
                )],
                "vite",
                5176,
            ),
            (
                "vite config from env",
                "vite",
                &[(
                    "vite.config.ts",
                    "server: { port: Number(env.PORT) || 3000 }",
                )],
                "vite",
                0,
            ),
            (
                "vite config without a port",
                "vite",
                &[("vite.config.js", "export default {}")],
                "vite",
                5173,
            ),
            ("vite dev", "vite dev --port 5174", &[], "vite", 5174),
            ("remix vite:dev", "remix vite:dev", &[], "vite", 5173),
            ("vite build", "vite build", &[], "", 0),
            ("vite-node", "vite-node src/main.ts", &[], "", 0),
            ("vitest", "vitest", &[], "", 0),
            ("next build", "next build", &[], "", 0),
            ("astro", "astro dev", &[], "astro", 4321),
            ("nuxt", "nuxt dev", &[], "nuxt", 3000),
            ("nuxi", "nuxi dev", &[], "nuxt", 3000),
            (
                "wrangler",
                "wrangler dev --port 8787",
                &[],
                "wrangler",
                8787,
            ),
            (
                "react-router",
                "react-router dev",
                &[],
                "react-router",
                5173,
            ),
            ("nest", "nest start --watch --env-file .env", &[], "nest", 0),
            ("angular", "ng serve", &[], "angular", 4200),
            ("gatsby", "gatsby develop -p 8001", &[], "gatsby", 8001),
            ("docusaurus", "docusaurus start", &[], "docusaurus", 3000),
            ("storybook", "storybook dev -p 6007", &[], "storybook", 6007),
            ("vue-cli", "vue-cli-service serve", &[], "vue-cli", 8080),
            ("webpack", "webpack serve", &[], "webpack", 8080),
            (
                "webpack-dev-server",
                "webpack-dev-server",
                &[],
                "webpack",
                8080,
            ),
            (
                "create react app",
                "react-scripts start",
                &[],
                "create-react-app",
                3000,
            ),
            ("react-email", "email dev", &[], "react-email", 3000),
            ("unknown", "tsx watch src/server.ts", &[], "", 0),
            ("turbo", "turbo run dev", &[], "", 0),
        ];

        for (name, script, files, want_tool, want_port) in cases {
            let tmp = tempfile::tempdir().unwrap();
            for (f, content) in *files {
                write_file(&tmp.path().join(f), content);
            }
            let srv = server_in(script, tmp.path(), false).unwrap_or_default();
            let got_tool = srv.tool.map_or("", |t| t.name);
            assert_eq!(got_tool, *want_tool, "case {name} tool");
            assert_eq!(srv.port, *want_port, "case {name} port");
        }
    }

    #[test]
    fn dev_server_chained_and_unknown_scripts() {
        let cases: &[(&str, &str, &str, u16, bool, bool)] = &[
            (
                "a script it runs",
                r#"{"scripts": {"dev": "concurrently \"npm:next\" \"npm:stripe\"", "next": "next dev -p 3002"}}"#,
                "next",
                3002,
                true,
                true,
            ),
            (
                "pnpm run",
                r#"{"scripts": {"dev": "pnpm run web", "web": "astro dev"}}"#,
                "astro",
                4321,
                true,
                true,
            ),
            (
                "an unknown tool with a port",
                r#"{"scripts": {"dev": "mintlify dev --port 3004"}}"#,
                "",
                3004,
                false,
                true,
            ),
            (
                "an unknown tool quoted in concurrently",
                r#"{"scripts": {"dev": "concurrently \"mintlify dev --port 3004\""}}"#,
                "",
                3004,
                false,
                true,
            ),
            (
                "an unknown tool",
                r#"{"scripts": {"dev": "tsx watch src/server.ts"}}"#,
                "",
                0,
                false,
                true,
            ),
            (
                "no dev script",
                r#"{"scripts": {"start": "next start"}}"#,
                "",
                0,
                false,
                false,
            ),
        ];

        for (name, pkg, want_tool, want_port, want_known, want_has_dev) in cases {
            let tmp = tempfile::tempdir().unwrap();
            write_file(&tmp.path().join("package.json"), pkg);
            let (srv, known, has_dev) = dev_server(tmp.path());
            let got_tool = srv.tool.map_or("", |t| t.name);
            assert_eq!(
                (got_tool, srv.port, known, has_dev),
                (*want_tool, *want_port, *want_known, *want_has_dev),
                "case {name}"
            );
        }
    }

    #[test]
    fn dev_server_env_port_rules() {
        let nest = r#"{"scripts": {"dev": "nest start --watch"}}"#;
        let cra = r#"{"scripts": {"dev": "react-scripts start"}}"#;
        let cases: &[(&str, &str, &str, &str, u16)] = &[
            ("nest", nest, "HOST=x\nPORT=3300\n", "", 3300),
            ("nest, .env.example only", nest, "", "PORT=3301\n", 3301),
            (".env first", nest, "PORT=3300\n", "PORT=3301\n", 3300),
            ("nest without one", nest, "", "", 0),
            ("commented out", nest, "# PORT=1\n", "", 0),
            (
                "export and quotes",
                nest,
                "export PORT=\"3302\"\n",
                "",
                3302,
            ),
            ("too high", nest, "PORT=99999\n", "", 0),
            ("a variable", nest, "PORT=${X}\n", "", 0),
            ("create react app", cra, "PORT=3005\n", "", 3005),
            ("create react app without one", cra, "", "", 3000),
            (
                "next ignores it",
                r#"{"scripts": {"dev": "next dev"}}"#,
                "PORT=4000\n",
                "",
                3000,
            ),
            (
                "an unknown command",
                r#"{"scripts": {"dev": "tsx watch src/server.ts"}}"#,
                "PORT=4000\n",
                "",
                4000,
            ),
            (
                "tsc -p falls back to .env",
                r#"{"scripts": {"dev": "tsc -p tsconfig.json && tsx watch src/server.ts"}}"#,
                "PORT=4000\n",
                "",
                4000,
            ),
            (
                "its -p first",
                r#"{"scripts": {"dev": "tsx watch -p 5000 src/server.ts"}}"#,
                "PORT=4000\n",
                "",
                5000,
            ),
        ];

        for (name, pkg, env, sample, want) in cases {
            let tmp = tempfile::tempdir().unwrap();
            write_file(&tmp.path().join("package.json"), pkg);
            if !env.is_empty() {
                write_file(&tmp.path().join(".env"), env);
            }
            if !sample.is_empty() {
                write_file(&tmp.path().join(".env.example"), sample);
            }
            let (srv, _, _) = dev_server(tmp.path());
            assert_eq!(srv.port, *want, "case {name}");
        }
    }
}

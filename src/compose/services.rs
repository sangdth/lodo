//! Docker Compose service image and published port inspection.

use std::borrow::Cow;
use std::collections::{BTreeMap, BTreeSet};

use saphyr::{LoadableYamlNode, MarkedYaml, Scalar, YamlData};

use super::ENV_VAR;

/// One service in a Docker Compose file: its image and published TCP ports
/// (`container_port -> host_port`), plus host ports bound to `${DOCKER_HOST_IP...}`.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Service {
    pub image: String,
    pub ports: BTreeMap<u16, u16>,
    pub bound: BTreeSet<u16>,
}

/// Parses `src` as a Docker Compose YAML document and returns its services in file order.
///
/// Only TCP ports with fixed host and container port numbers (`1..=65535`) are included.
/// Values repeated through YAML aliases (`*anchor`) are skipped, and unknown aliases fail parsing.
pub fn services(src: &[u8]) -> Result<Vec<Service>, String> {
    let raw = std::str::from_utf8(src).map_err(|e| format!("parse compose file: {e}"))?;
    let text = raw.strip_prefix('\u{FEFF}').unwrap_or(raw);
    let docs = MarkedYaml::load_from_str(text).map_err(|e| format!("parse compose file: {e}"))?;
    let Some(doc) = docs.first() else {
        return Ok(Vec::new());
    };

    let char_offsets: Vec<usize> = text.char_indices().map(|(i, _)| i).collect();
    let bytes = text.as_bytes();
    if has_unknown_alias(doc, &char_offsets, bytes) {
        return Err("parse compose file: unknown alias".into());
    }

    let Some(services_node) = services_of(doc, &char_offsets, bytes) else {
        return Ok(Vec::new());
    };

    let mut out = Vec::new();
    for (_name, service_node) in pairs(services_node, &char_offsets, bytes) {
        let mut svc = Service::default();
        for (key, value) in pairs(service_node, &char_offsets, bytes) {
            match key.as_ref() {
                "image" => {
                    if let Some(img) = scalar_text(value, &char_offsets, bytes) {
                        svc.image = img.into_owned();
                    }
                }
                "ports" => {
                    if let Some(seq) = sequence_items(value, &char_offsets, bytes) {
                        for port_node in seq {
                            if is_alias(port_node, &char_offsets, bytes) {
                                continue;
                            }
                            if let Some((container, host, addr)) =
                                published(port_node, &char_offsets, bytes)
                            {
                                svc.ports.insert(container, host);
                                if is_host_ip(&addr) {
                                    svc.bound.insert(host);
                                }
                            }
                        }
                    }
                }
                _ => {}
            }
        }
        out.push(svc);
    }
    Ok(out)
}

fn is_alias(node: &MarkedYaml<'_>, char_offsets: &[usize], bytes: &[u8]) -> bool {
    char_offsets
        .get(node.span.start.index())
        .and_then(|&b| bytes.get(b))
        .is_some_and(|&c| c == b'*')
}

fn has_unknown_alias(node: &MarkedYaml<'_>, char_offsets: &[usize], bytes: &[u8]) -> bool {
    if is_alias(node, char_offsets, bytes) && node.data.is_badvalue() {
        return true;
    }
    match &node.data {
        YamlData::Sequence(seq) => seq
            .iter()
            .any(|item| has_unknown_alias(item, char_offsets, bytes)),
        YamlData::Mapping(map) => map.iter().any(|(k, v)| {
            has_unknown_alias(k, char_offsets, bytes) || has_unknown_alias(v, char_offsets, bytes)
        }),
        YamlData::Tagged(_, inner) => has_unknown_alias(inner, char_offsets, bytes),
        _ => false,
    }
}

fn unwrap_tagged<'a, 'input>(
    mut node: &'a MarkedYaml<'input>,
    char_offsets: &[usize],
    bytes: &[u8],
) -> Option<&'a MarkedYaml<'input>> {
    loop {
        if is_alias(node, char_offsets, bytes) {
            return None;
        }
        match &node.data {
            YamlData::Tagged(_, inner) => node = inner,
            _ => return Some(node),
        }
    }
}

fn services_of<'a, 'input>(
    doc: &'a MarkedYaml<'input>,
    char_offsets: &[usize],
    bytes: &[u8],
) -> Option<&'a MarkedYaml<'input>> {
    for (key, value) in pairs(doc, char_offsets, bytes) {
        if key == "services" {
            return Some(value);
        }
    }
    None
}

fn pairs<'a, 'input>(
    node: &'a MarkedYaml<'input>,
    char_offsets: &[usize],
    bytes: &[u8],
) -> Vec<(Cow<'a, str>, &'a MarkedYaml<'input>)> {
    let Some(unwrapped) = unwrap_tagged(node, char_offsets, bytes) else {
        return Vec::new();
    };
    let YamlData::Mapping(map) = &unwrapped.data else {
        return Vec::new();
    };
    let mut out = Vec::with_capacity(map.len());
    for (k, v) in map {
        if is_alias(v, char_offsets, bytes) {
            continue;
        }
        if let Some(key_str) = scalar_text(k, char_offsets, bytes) {
            out.push((key_str, v));
        }
    }
    out
}

fn sequence_items<'a, 'input>(
    node: &'a MarkedYaml<'input>,
    char_offsets: &[usize],
    bytes: &[u8],
) -> Option<&'a [MarkedYaml<'input>]> {
    let unwrapped = unwrap_tagged(node, char_offsets, bytes)?;
    match &unwrapped.data {
        YamlData::Sequence(seq) => Some(seq.as_slice()),
        _ => None,
    }
}

fn scalar_text<'a>(
    node: &'a MarkedYaml<'_>,
    char_offsets: &[usize],
    bytes: &[u8],
) -> Option<Cow<'a, str>> {
    let unwrapped = unwrap_tagged(node, char_offsets, bytes)?;
    match &unwrapped.data {
        YamlData::Value(Scalar::String(s)) | YamlData::Representation(s, _, _) => {
            Some(Cow::Borrowed(s.as_ref()))
        }
        YamlData::Value(Scalar::Integer(n)) => Some(Cow::Owned(n.to_string())),
        YamlData::Value(Scalar::FloatingPoint(f)) => Some(Cow::Owned(f.to_string())),
        YamlData::Value(Scalar::Boolean(b)) => Some(Cow::Owned(b.to_string())),
        YamlData::Value(Scalar::Null) => Some(Cow::Borrowed("")),
        _ => None,
    }
}

fn published(
    port: &MarkedYaml<'_>,
    char_offsets: &[usize],
    bytes: &[u8],
) -> Option<(u16, u16, String)> {
    let unwrapped = unwrap_tagged(port, char_offsets, bytes)?;
    let mut container_text = String::new();
    let mut host_text = String::new();
    let mut protocol = String::new();
    let mut addr = String::new();

    if let Some(spec) = scalar_text(unwrapped, char_offsets, bytes) {
        let parts = split_port(&spec);
        if parts.len() < 2 {
            return None;
        }
        if parts.len() == 3 {
            addr = parts[0].to_string();
        }
        host_text = parts[parts.len() - 2].to_string();
        let last = parts[parts.len() - 1];
        if let Some((c, proto)) = last.split_once('/') {
            container_text = c.to_string();
            protocol = proto.to_string();
        } else {
            container_text = last.to_string();
        }
    } else if matches!(unwrapped.data, YamlData::Mapping(_)) {
        for (key, value) in pairs(unwrapped, char_offsets, bytes) {
            let Some(val) = scalar_text(value, char_offsets, bytes) else {
                continue;
            };
            match key.as_ref() {
                "target" => container_text = val.into_owned(),
                "published" => host_text = val.into_owned(),
                "protocol" => protocol = val.into_owned(),
                "host_ip" => addr = val.into_owned(),
                _ => {}
            }
        }
    } else {
        return None;
    }

    if !protocol.is_empty() && protocol != "tcp" {
        return None;
    }
    let container: u16 = container_text.parse().ok()?;
    let host: u16 = host_text.parse().ok()?;
    if container == 0 || host == 0 {
        return None;
    }
    Some((container, host, addr))
}

/// Splits a short-syntax port string on `:` outside `${...}` and `[...]`.
/// Any `/protocol` suffix stays attached to the last segment.
fn split_port(spec: &str) -> Vec<&str> {
    let bytes = spec.as_bytes();
    let mut parts = Vec::new();
    let mut vars: usize = 0;
    let mut brackets: usize = 0;
    let mut start: usize = 0;
    let mut i: usize = 0;

    while i < bytes.len() {
        let c = bytes[i];
        if c == b'$' && bytes.get(i + 1) == Some(&b'{') {
            vars += 1;
            i += 2;
            continue;
        }
        if c == b'}' && vars > 0 {
            vars -= 1;
        } else if c == b'[' {
            brackets += 1;
        } else if c == b']' && brackets > 0 {
            brackets -= 1;
        } else if c == b':' && vars == 0 && brackets == 0 {
            parts.push(&spec[start..i]);
            start = i + 1;
        }
        i += 1;
    }
    parts.push(&spec[start..]);
    parts
}

fn is_host_ip(addr: &str) -> bool {
    let Some(rest) = addr
        .strip_prefix("${")
        .and_then(|s| s.strip_prefix(ENV_VAR))
    else {
        return false;
    };
    matches!(rest.as_bytes().first(), Some(b'}' | b':' | b'-'))
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::path::Path;

    use super::*;

    fn svc(image: &str, ports: &[(u16, u16)], bound: &[u16]) -> Service {
        Service {
            image: image.to_string(),
            ports: ports.iter().copied().collect(),
            bound: bound.iter().copied().collect(),
        }
    }

    #[test]
    fn services_shapes_and_variants() {
        let fixture_path =
            Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/testdata/compose/compose.dev.yaml");
        let shapes = fs::read(&fixture_path).unwrap();
        let pg = "postgres:17-alpine";

        assert_eq!(
            services(&shapes).unwrap(),
            vec![
                svc(pg, &[(5432, 5432)], &[]),
                svc(pg, &[(5432, 5432)], &[]),
                svc(pg, &[], &[]),
                svc(pg, &[(5432, 5433)], &[5433]),
                svc("redis:8-alpine", &[(6379, 6379)], &[]),
                svc("minio/minio:latest", &[(9000, 9000), (9001, 9001)], &[]),
                svc("", &[(3000, 3001)], &[]),
                svc("ghcr.io/flowy/worker:latest", &[], &[]),
                svc("nginx:alpine", &[(80, 8080), (443, 8443)], &[]),
            ]
        );

        assert_eq!(
            services(b"services:\n  mail:\n    ports:\n      - \"[::1]:8025:8025\"\n").unwrap(),
            vec![svc("", &[(8025, 8025)], &[])]
        );

        assert_eq!(
            services(
                b"services:\n  dns:\n    ports:\n      - \"53:53/udp\"\n      - \"8000-8010:8000-8010\"\n      - target: 5353\n        published: 5353\n        protocol: udp\n"
            )
            .unwrap(),
            vec![svc("", &[], &[])]
        );

        assert_eq!(
            services(
                b"services:\n  db:\n    ports:\n      - \"${DOCKER_HOST_IP:-127.0.0.1}:5434:5432\"\n      - target: 6379\n        published: 6380\n        host_ip: ${DOCKER_HOST_IP}\n"
            )
            .unwrap(),
            vec![svc("", &[(5432, 5434), (6379, 6380)], &[5434, 6380])]
        );

        assert_eq!(
            services(
                b"services:\n  db:\n    ports:\n      - \"127.0.0.1:5434:5432\"\n      - \"${DOCKER_HOST_IPX}:6380:6379\"\n"
            )
            .unwrap(),
            vec![svc("", &[(5432, 5434), (6379, 6380)], &[])]
        );

        assert_eq!(services(b"name: flowy\n").unwrap(), Vec::<Service>::new());
        assert_eq!(services(b"").unwrap(), Vec::<Service>::new());
        assert_eq!(
            services(b"# comment only\n").unwrap(),
            Vec::<Service>::new()
        );
    }

    #[test]
    fn services_skips_aliases_and_rejects_invalid_yaml() {
        let aliased = br#"
x-ports: &ports
  - "5432:5432"
x-port: &port 6379:6379
x-service: &service
  image: redis:8
  ports: ["8080:80"]
services:
  db:
    ports: *ports
  cache:
    ports:
      - *port
  web: *service
"#;
        assert_eq!(
            services(aliased).unwrap(),
            vec![svc("", &[], &[]), svc("", &[], &[])]
        );

        for invalid in [
            b"services: [".as_slice(),
            b"services:\n\tdb: {}\n".as_slice(),
            b"services:\n  db: *nope\n".as_slice(),
        ] {
            let err = services(invalid).unwrap_err();
            assert!(
                err.starts_with("parse compose file: "),
                "unexpected error: {err}"
            );
        }
    }
}

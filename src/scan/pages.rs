//! Detection of well-known web UIs published by Docker Compose services (e.g. Mailpit).

use std::collections::BTreeSet;
use std::path::Path;

use crate::{compose, fsutil};

/// A web UI published by a compose service on this Mac that `lodo` names (e.g. `mail.<project>`).
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Page {
    /// Subdomain label (e.g. `"mail"` for `mail.shop.lodo`).
    pub label: String,
    /// Published host port on this Mac.
    pub port: u16,
}

const WEB_IMAGES: [(&str, &str, u16); 1] = [("axllent/mailpit", "mail", 8025)];

/// Reads `file` and returns the known web pages it publishes along with all host ports bound to `DOCKER_HOST_IP`.
/// Unreadable or invalid compose files yield empty collections.
pub(crate) fn read_compose(file: &Path) -> (Vec<Page>, BTreeSet<u16>) {
    let Ok(data) = fsutil::read_regular(file) else {
        return (Vec::new(), BTreeSet::new());
    };
    let services = compose::services(&data).unwrap_or_default();
    let mut pages = Vec::new();
    let mut bound = BTreeSet::new();

    for svc in services {
        bound.extend(svc.bound);
        for (repo, label, container_port) in WEB_IMAGES {
            if let Some(&host_port) = svc.ports.get(&container_port)
                && is_image(&svc.image, repo)
            {
                pages.push(Page {
                    label: label.to_string(),
                    port: host_port,
                });
            }
        }
    }
    (pages, bound)
}

/// Reports whether `image` refers to `repo` under any registry and at any tag or digest
/// (e.g. `ghcr.io/axllent/mailpit:v1.21@sha256:...` matches `axllent/mailpit`).
fn is_image(image: &str, repo: &str) -> bool {
    let after_slash = image.rfind('/').map_or(0, |idx| idx + 1);
    let base = match image[after_slash..].find([':', '@']) {
        Some(i) => &image[..after_slash + i],
        None => image,
    };
    base == repo || base.ends_with(&format!("/{repo}"))
}

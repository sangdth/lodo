//! Project root discovery, Docker Compose file discovery, service port inspection, and localhost URL mapping.

mod find;
mod services;
mod urls;

pub(crate) use find::{clean_path, is_local_path};
pub use find::{find, project_root};
pub use services::{Service, services};
pub use urls::{LocalUrl, Values, map_urls};

/// Environment variable a rewritten compose port binds to (`lodo dev` and `lodo exec` set it).
pub const ENV_VAR: &str = "DOCKER_HOST_IP";

/// Environment variable naming the compose project in a git worktree so each worktree gets its own containers and volumes.
pub const PROJECT_VAR: &str = "COMPOSE_PROJECT_NAME";

/// Default host address substitution for compose port bindings.
pub const HOST_IP: &str = "${DOCKER_HOST_IP:-127.0.0.1}";

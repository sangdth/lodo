#![cfg_attr(not(test), deny(clippy::unwrap_used, clippy::expect_used))]

pub mod brew;
pub mod caddy;
pub mod check;
pub mod clean;
pub mod cli;
pub mod compose;
pub mod dev;
pub mod dnsmasq;
pub mod fsutil;
pub mod ls;
pub mod menubar;
pub mod paths;
pub mod run;
pub mod scan;
pub mod store;
pub mod system;

pub const VERSION: &str = env!("LODO_BUILD_VERSION");

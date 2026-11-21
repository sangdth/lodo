//! File, directory, and tool paths `lodo` reads, writes, or executes.
//! `Paths::for_test` relocates all of them under a single temporary directory.

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};

use nix::unistd::{Uid, User};

/// Every file, directory, and system binary `lodo` touches.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Paths {
    /// Login short name written into `/etc/sudoers.d/lodo`.
    pub user: String,
    /// User's home directory.
    pub home: PathBuf,
    /// User's numeric uid (`gui/<uid>/<label>` in `launchd`).
    pub uid: u32,

    // lodo's user-owned files in ~/.config/lodo.
    pub config_dir: PathBuf,
    pub domains_json: PathBuf,
    pub dnsmasq_conf: PathBuf,
    pub resolvers: PathBuf,
    pub caddyfile: PathBuf,
    pub log: PathBuf,
    pub staging: PathBuf,
    pub trusted_ca: PathBuf,
    pub bind_hook: PathBuf,
    pub dev_dir: PathBuf,
    pub menubar_bin: PathBuf,
    /// Prebuilt menu bar helper installed by Homebrew (`libexec/lodo-menubar`).
    pub brew_menubar_bin: PathBuf,

    // Homebrew user-owned files.
    pub system_conf: PathBuf,
    pub system_conf_backup: PathBuf,
    pub system_caddyfile: PathBuf,
    pub system_caddyfile_backup: PathBuf,
    pub caddy_root: PathBuf,
    pub caddy_log: PathBuf,

    // User launchd directory and plist.
    pub launch_agents: PathBuf,
    pub menubar_plist: PathBuf,

    // Root-owned files and directories.
    pub launch_daemons: PathBuf,
    pub loopback_plist: PathBuf,
    pub script: PathBuf,
    pub sudoers: PathBuf,
    pub resolver_dir: PathBuf,
    pub resolver_local: PathBuf,

    // macOS System keychain and hosts file.
    pub system_keychain: PathBuf,
    pub hosts: PathBuf,

    // Absolute tool paths so PATH modifications cannot swap binaries.
    pub brew: PathBuf,
    pub lodo_brew_bin: PathBuf,
    pub dnsmasq: PathBuf,
    pub caddy: PathBuf,
    pub sudo: PathBuf,
    pub launchctl: PathBuf,
    pub ifconfig: PathBuf,
    pub install: PathBuf,
    pub visudo: PathBuf,
    pub rm: PathBuf,
    pub rmdir: PathBuf,
    pub dscacheutil: PathBuf,
    pub lsof: PathBuf,
    pub kill: PathBuf,
    pub security: PathBuf,
    pub swiftc: PathBuf,
    /// Fixed candidate paths checked for the Docker CLI (works under launchd's minimal `PATH`).
    pub docker_candidates: Vec<PathBuf>,

    // LocalDNS leftovers reported and left untouched by `lodo setup`.
    pub leftovers: Vec<PathBuf>,
}

#[derive(Debug, thiserror::Error)]
#[error("look up current user: {0}")]
pub struct PathsError(String);

impl Paths {
    /// Returns the real macOS locations for the current user.
    pub fn default_paths() -> Result<Self, PathsError> {
        let uid = Uid::current();
        let user = User::from_uid(uid)
            .map_err(|e| PathsError(e.to_string()))?
            .ok_or_else(|| PathsError(format!("no user entry for uid {uid}")))?;
        Ok(Self::build(
            Path::new("/"),
            &user.dir,
            &user.name,
            uid.as_raw(),
        ))
    }

    /// Relocates every path under `root` for hermetic tests (`tester`, uid `501`).
    pub fn for_test(root: &Path) -> Self {
        let home = root.join("Users").join("tester");
        Self::build(root, &home, "tester", 501)
    }

    /// Returns the first executable Docker CLI binary among [`Paths::docker_candidates`].
    pub fn resolve_docker(&self) -> Option<&Path> {
        self.docker_candidates
            .iter()
            .find(|p| is_executable(p))
            .map(PathBuf::as_path)
    }

    fn build(root: &Path, home: &Path, user: &str, uid: u32) -> Self {
        let sys = |rel: &str| root.join(rel.trim_start_matches('/'));
        let config = home.join(".config").join("lodo");
        let brew = sys("/opt/homebrew");
        let etc = brew.join("etc");
        let daemons = sys("/Library/LaunchDaemons");
        let launch_agents = home.join("Library").join("LaunchAgents");

        Self {
            user: user.to_string(),
            home: home.to_path_buf(),
            uid,

            config_dir: config.clone(),
            domains_json: config.join("domains.json"),
            dnsmasq_conf: config.join("dnsmasq.conf"),
            resolvers: config.join("resolvers"),
            caddyfile: config.join("Caddyfile"),
            log: config.join("dnsmasq.log"),
            staging: config.join("staging"),
            trusted_ca: config.join("caddy-root.crt"),
            bind_hook: config.join("bind.cjs"),
            dev_dir: config.join("dev"),
            menubar_bin: config.join("lodo-menubar"),
            brew_menubar_bin: brew.join("opt/lodo/libexec/lodo-menubar"),

            system_conf: etc.join("dnsmasq.conf"),
            system_conf_backup: etc.join("dnsmasq.conf.before-lodo"),
            system_caddyfile: etc.join("Caddyfile"),
            system_caddyfile_backup: etc.join("Caddyfile.before-lodo"),
            caddy_root: brew.join("var/lib/caddy/pki/authorities/local/root.crt"),
            caddy_log: brew.join("var/log/caddy.log"),

            menubar_plist: launch_agents.join("io.lodo.menubar.plist"),
            launch_agents,

            loopback_plist: daemons.join("io.lodo.loopback.plist"),
            launch_daemons: daemons,
            script: sys("/Library/Application Support/lodo/apply-resolvers.sh"),
            sudoers: sys("/etc/sudoers.d/lodo"),
            resolver_dir: sys("/etc/resolver"),
            resolver_local: sys("/etc/resolver/local"),

            system_keychain: sys("/Library/Keychains/System.keychain"),
            hosts: sys("/etc/hosts"),

            brew: brew.join("bin/brew"),
            lodo_brew_bin: brew.join("bin/lodo"),
            dnsmasq: brew.join("opt/dnsmasq/sbin/dnsmasq"),
            caddy: brew.join("bin/caddy"),
            sudo: sys("/usr/bin/sudo"),
            launchctl: sys("/bin/launchctl"),
            ifconfig: sys("/sbin/ifconfig"),
            install: sys("/usr/bin/install"),
            visudo: sys("/usr/sbin/visudo"),
            rm: sys("/bin/rm"),
            rmdir: sys("/bin/rmdir"),
            dscacheutil: sys("/usr/bin/dscacheutil"),
            lsof: sys("/usr/sbin/lsof"),
            kill: sys("/bin/kill"),
            security: sys("/usr/bin/security"),
            swiftc: sys("/usr/bin/swiftc"),
            docker_candidates: vec![
                brew.join("bin/docker"),
                sys("/usr/local/bin/docker"),
                home.join(".orbstack/bin/docker"),
                home.join(".docker/bin/docker"),
                sys("/Applications/Docker.app/Contents/Resources/bin/docker"),
            ],

            leftovers: vec![
                home.join(".config").join("localdns"),
                home.join("Library")
                    .join("LaunchAgents")
                    .join("com.localdns.caddy.plist"),
                home.join("Library")
                    .join("Application Support")
                    .join("LocalDNS"),
            ],
        }
    }
}

fn is_executable(path: &Path) -> bool {
    fs::metadata(path).is_ok_and(|m| m.is_file() && (m.permissions().mode() & 0o111) != 0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn default_paths_match_expected_macos_locations() {
        let p = Paths::default_paths().unwrap();
        assert!(!p.user.is_empty());
        assert!(!p.home.as_os_str().is_empty());
        assert_eq!(p.domains_json, p.home.join(".config/lodo/domains.json"));
        assert_eq!(p.system_conf, Path::new("/opt/homebrew/etc/dnsmasq.conf"));
        assert_eq!(
            p.script,
            Path::new("/Library/Application Support/lodo/apply-resolvers.sh")
        );
        assert_eq!(p.sudoers, Path::new("/etc/sudoers.d/lodo"));
        assert_eq!(p.resolver_dir, Path::new("/etc/resolver"));
        assert_eq!(p.resolver_local, Path::new("/etc/resolver/local"));
        assert_eq!(
            p.loopback_plist,
            Path::new("/Library/LaunchDaemons/io.lodo.loopback.plist")
        );
        assert_eq!(p.sudo, Path::new("/usr/bin/sudo"));
        assert_eq!(p.security, Path::new("/usr/bin/security"));
        assert_eq!(
            p.system_keychain,
            Path::new("/Library/Keychains/System.keychain")
        );
        assert_eq!(
            p.caddy_root,
            Path::new("/opt/homebrew/var/lib/caddy/pki/authorities/local/root.crt")
        );
        assert_eq!(p.caddy_log, Path::new("/opt/homebrew/var/log/caddy.log"));
        assert_eq!(p.trusted_ca, p.home.join(".config/lodo/caddy-root.crt"));
        assert_eq!(p.bind_hook, p.home.join(".config/lodo/bind.cjs"));
        assert_eq!(p.dev_dir, p.home.join(".config/lodo/dev"));
        assert_eq!(p.menubar_bin, p.home.join(".config/lodo/lodo-menubar"));
        assert_eq!(
            p.brew_menubar_bin,
            Path::new("/opt/homebrew/opt/lodo/libexec/lodo-menubar")
        );
        assert_eq!(
            p.menubar_plist,
            p.home.join("Library/LaunchAgents/io.lodo.menubar.plist")
        );
        assert_eq!(p.swiftc, Path::new("/usr/bin/swiftc"));
        assert_eq!(p.kill, Path::new("/bin/kill"));
        assert_eq!(
            p.docker_candidates,
            vec![
                PathBuf::from("/opt/homebrew/bin/docker"),
                PathBuf::from("/usr/local/bin/docker"),
                p.home.join(".orbstack/bin/docker"),
                p.home.join(".docker/bin/docker"),
                PathBuf::from("/Applications/Docker.app/Contents/Resources/bin/docker"),
            ]
        );
    }

    #[test]
    fn for_test_keeps_every_path_under_root() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path();
        let p = Paths::for_test(root);
        assert_eq!(p.user, "tester");
        assert_eq!(p.uid, 501);
        for candidate in [
            &p.home,
            &p.config_dir,
            &p.domains_json,
            &p.dnsmasq_conf,
            &p.resolvers,
            &p.caddyfile,
            &p.log,
            &p.staging,
            &p.trusted_ca,
            &p.bind_hook,
            &p.dev_dir,
            &p.menubar_bin,
            &p.brew_menubar_bin,
            &p.system_conf,
            &p.system_conf_backup,
            &p.system_caddyfile,
            &p.system_caddyfile_backup,
            &p.caddy_root,
            &p.caddy_log,
            &p.launch_agents,
            &p.menubar_plist,
            &p.launch_daemons,
            &p.loopback_plist,
            &p.script,
            &p.sudoers,
            &p.resolver_dir,
            &p.resolver_local,
            &p.system_keychain,
            &p.hosts,
            &p.brew,
            &p.lodo_brew_bin,
            &p.dnsmasq,
            &p.caddy,
            &p.sudo,
            &p.launchctl,
            &p.ifconfig,
            &p.install,
            &p.visudo,
            &p.rm,
            &p.rmdir,
            &p.dscacheutil,
            &p.lsof,
            &p.kill,
            &p.security,
            &p.swiftc,
        ] {
            assert!(
                candidate.starts_with(root),
                "{candidate:?} should start with {root:?}"
            );
        }
        for docker in &p.docker_candidates {
            assert!(
                docker.starts_with(root),
                "{docker:?} should start with {root:?}"
            );
        }
        for leftover in &p.leftovers {
            assert!(leftover.starts_with(root));
        }
    }

    #[test]
    fn resolve_docker_picks_first_executable_candidate() {
        let tmp = tempfile::tempdir().unwrap();
        let p = Paths::for_test(tmp.path());
        assert_eq!(p.resolve_docker(), None);

        // Non-executable file and directory are skipped.
        let first = &p.docker_candidates[0];
        fs::create_dir_all(first.parent().unwrap()).unwrap();
        fs::write(first, b"").unwrap();
        fs::set_permissions(first, fs::Permissions::from_mode(0o644)).unwrap();

        let second = &p.docker_candidates[1];
        fs::create_dir_all(second).unwrap();
        assert_eq!(p.resolve_docker(), None);

        // First executable file in priority order wins.
        let third = &p.docker_candidates[2];
        fs::create_dir_all(third.parent().unwrap()).unwrap();
        fs::write(third, b"").unwrap();
        fs::set_permissions(third, fs::Permissions::from_mode(0o755)).unwrap();

        let fourth = &p.docker_candidates[3];
        fs::create_dir_all(fourth.parent().unwrap()).unwrap();
        fs::write(fourth, b"").unwrap();
        fs::set_permissions(fourth, fs::Permissions::from_mode(0o755)).unwrap();

        assert_eq!(p.resolve_docker(), Some(third.as_path()));

        // Making an earlier candidate executable puts it first.
        fs::set_permissions(first, fs::Permissions::from_mode(0o755)).unwrap();
        assert_eq!(p.resolve_docker(), Some(first.as_path()));
    }
}

use std::process::Command;

fn main() {
    println!("cargo:rerun-if-env-changed=LODO_VERSION");
    if let Ok(v) = std::env::var("LODO_VERSION")
        && !v.trim().is_empty()
    {
        println!("cargo:rustc-env=LODO_BUILD_VERSION={}", v.trim());
        return;
    }

    let desc = Command::new("git")
        .args(["describe", "--tags", "--always", "--dirty"])
        .output()
        .ok()
        .filter(|o| o.status.success())
        .and_then(|o| String::from_utf8(o.stdout).ok())
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| env!("CARGO_PKG_VERSION").to_string());

    println!("cargo:rustc-env=LODO_BUILD_VERSION={desc}");
}

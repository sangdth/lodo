#![cfg_attr(not(test), deny(clippy::unwrap_used, clippy::expect_used))]

use std::process::ExitCode;

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let mut stdout = std::io::stdout();
    let mut stderr = std::io::stderr();
    let code = lodo::cli::dispatch(&args, &mut stdout, &mut stderr);
    ExitCode::from(code)
}

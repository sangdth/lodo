//! Zero-dependency flag and argument parser for all `lodo` subcommands.

use crate::system;

#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) enum Command {
    Help,
    Version,
    Setup,
    Apply,
    Doctor,
    Uninstall,
    Menubar { stop: bool },
    Ls { json: bool, no_color: bool },
    Scan { json: bool, name: String },
    Prune { worktree: bool },
    Dev { name: String, auto: bool },
    Exec { args: Vec<String> },
    Add(AddArgs),
    Set(SetArgs),
    Rm { name: String },
    Service { service: String, on: bool },
}

#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub(super) struct AddArgs {
    pub name: String,
    pub address: Option<String>,
    pub port: Option<String>,
    pub https: bool,
    pub compose: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub(super) struct SetArgs {
    pub target: String,
    pub name: Option<String>,
    pub address: Option<String>,
    pub port: Option<String>,
    pub enabled: Option<bool>,
    pub https: Option<bool>,
    pub compose: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) enum ParseError {
    UnknownCommand(String),
    TakesNoArgs(String),
    ExecNeedsCommand,
    Subcommand { cmd: &'static str, msg: String },
}

impl std::fmt::Display for ParseError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::UnknownCommand(cmd) => write!(f, "lodo: unknown command {cmd:?}"),
            Self::TakesNoArgs(cmd) => write!(f, "lodo: {cmd} takes no arguments"),
            Self::ExecNeedsCommand => write!(f, "lodo: exec needs a command to run"),
            Self::Subcommand { cmd, msg } => write!(f, "lodo: {cmd}: {msg}"),
        }
    }
}

pub(super) fn parse(args: &[String]) -> Result<Command, ParseError> {
    let cmd = args.first().map_or("", String::as_str);
    let rest = args.get(1..).unwrap_or(&[]);

    match cmd {
        "" | "help" | "-h" | "--help" => Ok(Command::Help),
        "version" | "--version" => Ok(Command::Version),
        "setup" => no_args("setup", rest, Command::Setup),
        "apply" => no_args("apply", rest, Command::Apply),
        "doctor" => no_args("doctor", rest, Command::Doctor),
        "uninstall" => no_args("uninstall", rest, Command::Uninstall),
        "menubar" => parse_menubar(rest),
        "ls" => parse_ls(rest),
        "scan" => parse_scan(rest),
        "prune" => parse_prune(rest),
        "dev" => parse_dev(rest),
        "exec" => parse_exec(rest),
        "add" => parse_add(rest),
        "set" => parse_set(rest),
        "rm" => parse_rm(rest),
        "service" => parse_service(rest),
        other => Err(ParseError::UnknownCommand(other.to_string())),
    }
}

fn no_args(name: &str, rest: &[String], out: Command) -> Result<Command, ParseError> {
    if !rest.is_empty() {
        return Err(ParseError::TakesNoArgs(name.to_string()));
    }
    Ok(out)
}

enum Token<'a> {
    Flag(&'a str, Option<&'a str>),
    EndOfFlags,
    Positional(&'a str),
}

fn classify(arg: &str) -> Token<'_> {
    if arg == "--" {
        return Token::EndOfFlags;
    }
    if let Some(rest) = arg.strip_prefix("--") {
        return match rest.split_once('=') {
            Some((k, v)) => Token::Flag(k, Some(v)),
            None => Token::Flag(rest, None),
        };
    }
    if let Some(rest) = arg.strip_prefix('-')
        && !rest.is_empty()
    {
        return match rest.split_once('=') {
            Some((k, v)) => Token::Flag(k, Some(v)),
            None => Token::Flag(rest, None),
        };
    }
    Token::Positional(arg)
}

fn bool_val(cmd: &'static str, key: &str, inline: Option<&str>) -> Result<bool, ParseError> {
    match inline {
        None | Some("true") => Ok(true),
        Some("false") => Ok(false),
        Some(other) => Err(ParseError::Subcommand {
            cmd,
            msg: format!("invalid boolean value {other:?} for -{key}"),
        }),
    }
}

fn str_val<'a, I>(
    cmd: &'static str,
    key: &str,
    inline: Option<&'a str>,
    iter: &mut I,
) -> Result<String, ParseError>
where
    I: Iterator<Item = &'a String>,
{
    if let Some(v) = inline {
        return Ok(v.to_string());
    }
    iter.next().cloned().ok_or_else(|| ParseError::Subcommand {
        cmd,
        msg: format!("flag needs an argument: -{key}"),
    })
}

fn unknown_flag(cmd: &'static str, key: &str) -> ParseError {
    ParseError::Subcommand {
        cmd,
        msg: format!("flag provided but not defined: -{key}"),
    }
}

fn no_positionals(cmd: &'static str, positionals: &[String]) -> Result<(), ParseError> {
    if let Some(first) = positionals.first() {
        return Err(ParseError::Subcommand {
            cmd,
            msg: format!("unexpected argument {first:?}"),
        });
    }
    Ok(())
}

fn one_positional(
    cmd: &'static str,
    positionals: Vec<String>,
    missing_msg: &str,
) -> Result<String, ParseError> {
    let mut it = positionals.into_iter();
    let Some(first) = it.next() else {
        return Err(ParseError::Subcommand {
            cmd,
            msg: missing_msg.to_string(),
        });
    };
    if let Some(extra) = it.next() {
        return Err(ParseError::Subcommand {
            cmd,
            msg: format!("unexpected argument {extra:?}"),
        });
    }
    Ok(first)
}

fn parse_menubar(args: &[String]) -> Result<Command, ParseError> {
    let mut stop = false;
    let mut positionals = Vec::new();
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        match classify(arg) {
            Token::EndOfFlags => {
                positionals.extend(iter.cloned());
                break;
            }
            Token::Positional(p) => positionals.push(p.to_string()),
            Token::Flag("stop", inline) => stop = bool_val("menubar", "stop", inline)?,
            Token::Flag(k, _) => return Err(unknown_flag("menubar", k)),
        }
    }
    no_positionals("menubar", &positionals)?;
    Ok(Command::Menubar { stop })
}

fn parse_ls(args: &[String]) -> Result<Command, ParseError> {
    let mut json = false;
    let mut no_color = false;
    let mut positionals = Vec::new();
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        match classify(arg) {
            Token::EndOfFlags => {
                positionals.extend(iter.cloned());
                break;
            }
            Token::Positional(p) => positionals.push(p.to_string()),
            Token::Flag("json", inline) => json = bool_val("ls", "json", inline)?,
            Token::Flag("no-color", inline) => no_color = bool_val("ls", "no-color", inline)?,
            Token::Flag(k, _) => return Err(unknown_flag("ls", k)),
        }
    }
    no_positionals("ls", &positionals)?;
    Ok(Command::Ls { json, no_color })
}

fn parse_scan(args: &[String]) -> Result<Command, ParseError> {
    let mut json = false;
    let mut name = String::new();
    let mut positionals = Vec::new();
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        match classify(arg) {
            Token::EndOfFlags => {
                positionals.extend(iter.cloned());
                break;
            }
            Token::Positional(p) => positionals.push(p.to_string()),
            Token::Flag("json", inline) => json = bool_val("scan", "json", inline)?,
            Token::Flag("name", inline) => name = str_val("scan", "name", inline, &mut iter)?,
            Token::Flag(k, _) => return Err(unknown_flag("scan", k)),
        }
    }
    no_positionals("scan", &positionals)?;
    Ok(Command::Scan { json, name })
}

fn parse_prune(args: &[String]) -> Result<Command, ParseError> {
    let mut worktree = false;
    let mut positionals = Vec::new();
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        match classify(arg) {
            Token::EndOfFlags => {
                positionals.extend(iter.cloned());
                break;
            }
            Token::Positional(p) => positionals.push(p.to_string()),
            Token::Flag("worktree", inline) => {
                worktree = bool_val("prune", "worktree", inline)?;
            }
            Token::Flag(k, _) => return Err(unknown_flag("prune", k)),
        }
    }
    no_positionals("prune", &positionals)?;
    Ok(Command::Prune { worktree })
}

fn parse_dev(args: &[String]) -> Result<Command, ParseError> {
    let mut name = String::new();
    let mut auto = false;
    let mut positionals = Vec::new();
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        match classify(arg) {
            Token::EndOfFlags => {
                positionals.extend(iter.cloned());
                break;
            }
            Token::Positional(p) => positionals.push(p.to_string()),
            Token::Flag("name", inline) => name = str_val("dev", "name", inline, &mut iter)?,
            Token::Flag("y" | "auto", inline) => auto = bool_val("dev", "auto", inline)?,
            Token::Flag(k, _) => return Err(unknown_flag("dev", k)),
        }
    }
    no_positionals("dev", &positionals)?;
    Ok(Command::Dev { name, auto })
}

fn parse_exec(args: &[String]) -> Result<Command, ParseError> {
    let cmd_args = if args.first().map(String::as_str) == Some("--") {
        &args[1..]
    } else {
        args
    };
    if cmd_args.is_empty() {
        return Err(ParseError::ExecNeedsCommand);
    }
    Ok(Command::Exec {
        args: cmd_args.to_vec(),
    })
}

fn parse_add(args: &[String]) -> Result<Command, ParseError> {
    let mut out = AddArgs::default();
    let mut positionals = Vec::new();
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        match classify(arg) {
            Token::EndOfFlags => {
                positionals.extend(iter.cloned());
                break;
            }
            Token::Positional(p) => positionals.push(p.to_string()),
            Token::Flag("address", inline) => {
                out.address = Some(str_val("add", "address", inline, &mut iter)?);
            }
            Token::Flag("port", inline) => {
                out.port = Some(str_val("add", "port", inline, &mut iter)?);
            }
            Token::Flag("https", inline) => {
                out.https = bool_val("add", "https", inline)?;
            }
            Token::Flag("compose", inline) => {
                out.compose = Some(str_val("add", "compose", inline, &mut iter)?);
            }
            Token::Flag(k, _) => return Err(unknown_flag("add", k)),
        }
    }
    out.name = one_positional("add", positionals, "missing domain name")?;
    Ok(Command::Add(out))
}

fn parse_set(args: &[String]) -> Result<Command, ParseError> {
    let mut out = SetArgs::default();
    let mut flag_on = false;
    let mut flag_off = false;
    let mut flag_https = false;
    let mut flag_no_https = false;
    let mut positionals = Vec::new();
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        match classify(arg) {
            Token::EndOfFlags => {
                positionals.extend(iter.cloned());
                break;
            }
            Token::Positional(p) => positionals.push(p.to_string()),
            Token::Flag("name", inline) => {
                out.name = Some(str_val("set", "name", inline, &mut iter)?);
            }
            Token::Flag("address", inline) => {
                out.address = Some(str_val("set", "address", inline, &mut iter)?);
            }
            Token::Flag("port", inline) => {
                out.port = Some(str_val("set", "port", inline, &mut iter)?);
            }
            Token::Flag("on", inline) => {
                if bool_val("set", "on", inline)? {
                    flag_on = true;
                }
            }
            Token::Flag("off", inline) => {
                if bool_val("set", "off", inline)? {
                    flag_off = true;
                }
            }
            Token::Flag("https", inline) => {
                if bool_val("set", "https", inline)? {
                    flag_https = true;
                }
            }
            Token::Flag("no-https", inline) => {
                if bool_val("set", "no-https", inline)? {
                    flag_no_https = true;
                }
            }
            Token::Flag("compose", inline) => {
                out.compose = Some(str_val("set", "compose", inline, &mut iter)?);
            }
            Token::Flag(k, _) => return Err(unknown_flag("set", k)),
        }
    }
    if flag_on && flag_off {
        return Err(ParseError::Subcommand {
            cmd: "set",
            msg: "cannot pass both --on and --off".to_string(),
        });
    }
    if flag_https && flag_no_https {
        return Err(ParseError::Subcommand {
            cmd: "set",
            msg: "cannot pass both --https and --no-https".to_string(),
        });
    }
    if flag_on {
        out.enabled = Some(true);
    } else if flag_off {
        out.enabled = Some(false);
    }
    if flag_https {
        out.https = Some(true);
    } else if flag_no_https {
        out.https = Some(false);
    }
    out.target = one_positional("set", positionals, "missing domain name")?;
    Ok(Command::Set(out))
}

fn parse_rm(args: &[String]) -> Result<Command, ParseError> {
    let mut positionals = Vec::new();
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        match classify(arg) {
            Token::EndOfFlags => {
                positionals.extend(iter.cloned());
                break;
            }
            Token::Positional(p) => positionals.push(p.to_string()),
            Token::Flag(k, _) => return Err(unknown_flag("rm", k)),
        }
    }
    let name = one_positional("rm", positionals, "missing domain name")?;
    Ok(Command::Rm { name })
}

fn parse_service(args: &[String]) -> Result<Command, ParseError> {
    let mut positionals = Vec::new();
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        match classify(arg) {
            Token::EndOfFlags => {
                positionals.extend(iter.cloned());
                break;
            }
            Token::Positional(p) => positionals.push(p.to_string()),
            Token::Flag(k, _) => return Err(unknown_flag("service", k)),
        }
    }
    if positionals.len() < 2 {
        return Err(ParseError::Subcommand {
            cmd: "service",
            msg: "expected <dnsmasq|caddy|menubar> <on|off>".to_string(),
        });
    }
    if positionals.len() > 2 {
        return Err(ParseError::Subcommand {
            cmd: "service",
            msg: format!("unexpected argument {:?}", positionals[2]),
        });
    }
    let service = positionals[0].clone();
    if !system::SERVICES.contains(&service.as_str()) {
        return Err(ParseError::Subcommand {
            cmd: "service",
            msg: format!("unknown service {service:?}: expected dnsmasq, caddy, or menubar"),
        });
    }
    let on = match positionals[1].as_str() {
        "on" => true,
        "off" => false,
        other => {
            return Err(ParseError::Subcommand {
                cmd: "service",
                msg: format!("expected on or off, got {other:?}"),
            });
        }
    };
    Ok(Command::Service { service, on })
}

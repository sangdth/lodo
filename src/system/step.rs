//! Step execution and formatting for `lodo setup` and `lodo uninstall`.

use std::io::Write;

/// A `setup` or `uninstall` step that failed, together with what to run next.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("{step}: {err}")]
pub struct StepError {
    pub step: String,
    pub err: String,
    pub fix: String,
}

pub(super) enum StepOutcome {
    Done(String),
    Skipped(String),
    Warning(String),
}

pub(super) struct Steps<'a> {
    pub(super) out: &'a mut dyn Write,
}

impl Steps<'_> {
    pub(super) fn do_step(
        &mut self,
        step: &str,
        fix: &str,
        f: impl FnOnce() -> Result<StepOutcome, String>,
    ) -> Result<(), StepError> {
        match f() {
            Ok(StepOutcome::Skipped(why)) => {
                let _ = writeln!(self.out, "– {step}: {why}");
                Ok(())
            }
            Ok(StepOutcome::Warning(why)) => {
                let _ = writeln!(self.out, "! {step}: {why}");
                Ok(())
            }
            Ok(StepOutcome::Done(note)) => {
                if note.is_empty() {
                    let _ = writeln!(self.out, "✓ {step}");
                } else {
                    let _ = writeln!(self.out, "✓ {step} ({note})");
                }
                Ok(())
            }
            Err(err) => Err(StepError {
                step: step.to_string(),
                err,
                fix: fix.to_string(),
            }),
        }
    }

    pub(super) fn note(&mut self, text: &str) {
        let _ = writeln!(self.out, "  {text}");
    }
}

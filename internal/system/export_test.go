package system

// Tests don't wait for Caddy to settle: run.Fake answers at once.
func init() { caddySettle = 0 }

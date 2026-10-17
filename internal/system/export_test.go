package system

// Tests don't wait for Caddy to settle or to write its root: no Caddy runs.
func init() { caddySettle, trustWait = 0, 0 }

// Package compose finds a project's Docker Compose file and rewrites it so
// the project's ports bind its own loopback address and its browser URLs use
// its .oo name. It reads files and never writes them.
package compose

// EnvVar is the variable a rewritten port binds to. Linking a name writes it,
// set to the name's address, into the project's .env.
const EnvVar = "DOCKER_HOST_IP"

// HostIP is the host address Rewrite gives a port: the project's address
// from .env, or 127.0.0.1 without one.
const HostIP = "${" + EnvVar + ":-127.0.0.1}"

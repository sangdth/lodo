// Package compose finds a project's Docker Compose file and rewrites it so
// the project's ports bind its own loopback address and its browser URLs use
// its .test name. It reads files and never writes them.
package compose

// EnvVar is the variable a rewritten port binds to. lodo dev and lodo exec
// set it to the project's address.
const EnvVar = "DOCKER_HOST_IP"

// ProjectVar names the compose project, and beats the file's name:. lodo dev
// and lodo exec set it in a git worktree, so each worktree gets its own
// containers and volumes.
const ProjectVar = "COMPOSE_PROJECT_NAME"

// HostIP is the host address Rewrite gives a port: the project's address
// under lodo dev, or 127.0.0.1 without it.
const HostIP = "${" + EnvVar + ":-127.0.0.1}"

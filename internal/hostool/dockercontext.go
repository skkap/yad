package hostool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DefaultDockerSocket is where a tool that reads no docker context looks for
// the daemon when DOCKER_HOST is unset. A variable so a test can point it at
// a socket of its own.
var DefaultDockerSocket = "/var/run/docker.sock"

// dockerFileCap bounds what is read of docker's own files: a config.json or
// a context's meta.json is a few hundred bytes, and doctor must not read a
// file someone made enormous.
const dockerFileCap = 1 << 20

// OffDefaultDockerSocket is the docker context the docker CLI would use and
// the unix socket it names, when that socket is not the one a tool reading
// no context finds — DefaultDockerSocket, or a link to the same socket. The
// docker CLI follows its context, so `docker ps` works; Testcontainers and
// other tools that read DOCKER_HOST or the default socket alone find no
// daemon (YAD-188, colima on Ashikaga). ok is false whenever that is not
// known to be the case.
//
// Read from docker's own files the way the CLI reads them — DOCKER_CONTEXT,
// else currentContext in config.json under DOCKER_CONFIG or ~/.docker, then
// the context's meta.json, kept under the SHA-256 of its name — and never by
// running docker. Docker's layout is docker's: a file that is missing or
// shaped otherwise is no answer, not an error, and the caller's warning goes
// quiet.
func OffDefaultDockerSocket(getenv func(string) string) (context, socket string, ok bool) {
	dir := getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", false
		}
		dir = filepath.Join(home, ".docker")
	}
	context = getenv("DOCKER_CONTEXT")
	if context == "" {
		var cfg struct {
			CurrentContext string `json:"currentContext"`
		}
		if !readJSON(filepath.Join(dir, "config.json"), &cfg) {
			return "", "", false
		}
		context = cfg.CurrentContext
	}
	if context == "" || context == "default" {
		return "", "", false
	}
	sum := sha256.Sum256([]byte(context))
	var meta struct {
		Endpoints struct {
			Docker struct {
				Host string `json:"Host"`
			} `json:"docker"`
		} `json:"Endpoints"`
	}
	if !readJSON(filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"), &meta) {
		return "", "", false
	}
	socket, isUnix := strings.CutPrefix(meta.Endpoints.Docker.Host, "unix://")
	if !isUnix || socket == "" || filepath.Clean(socket) == DefaultDockerSocket {
		return "", "", false
	}
	// Docker Desktop names its own socket in its context and links the
	// default one to it: a tool reading no context finds the daemon anyway.
	if a, err := os.Stat(socket); err == nil {
		if b, err := os.Stat(DefaultDockerSocket); err == nil && os.SameFile(a, b) {
			return "", "", false
		}
	}
	return context, socket, true
}

func readJSON(path string, v any) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, dockerFileCap))
	return err == nil && json.Unmarshal(b, v) == nil
}

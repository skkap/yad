package hostool

import (
	"bytes"
	"context"
	"fmt"
	"runtime"

	"github.com/skkap/yad/internal/probe"
)

// dockerStatus asks the daemon, not the binary.
//
// A docker CLI with nothing listening runs a container exactly as well as no
// docker at all — on a Mac it is installed for good the moment Docker Desktop
// is, and answers only while the app is open. So the probe asks for the
// *server* version, and a daemon that does not answer is reported with the way
// to start it.
func dockerStatus(ctx context.Context, bin probe.Found, d *Detected) {
	out, err := run(ctx, bin.Path, []string{"version", "--format", "{{.Server.Version}}"}, false, statusWait())
	switch {
	case err != nil:
		d.Error = bin.WontStart()
	case out.TimedOut:
		d.Error = fmt.Sprintf("the Docker daemon did not answer within %s — %s", statusWait(), startDocker())
	case out.Err != nil, len(bytes.TrimSpace(out.Stdout)) == 0:
		// The daemon's own message goes no further: it names the socket, and a
		// DOCKER_HOST may carry credentials in its URL. The next action is
		// worth more to whoever reads this than the text was.
		d.Error = "the Docker daemon is not answering — " + startDocker()
	}
}

func startDocker() string {
	if runtime.GOOS == "darwin" {
		return "start Docker Desktop (`open -a Docker`) and it will be picked up at the next probe"
	}
	return "start the daemon (`sudo systemctl start docker`) and it will be picked up at the next probe"
}

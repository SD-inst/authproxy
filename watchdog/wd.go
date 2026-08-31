package watchdog

import (
	"bufio"
	"context"
	"log"
	"net"
	"strings"
	"time"
)

// Watchdog is a client for the sdwd control endpoint. The endpoint speaks
// newline-delimited "<op> <service>" commands over TCP and replies with a
// single line: "ok" or "error: ...".
type Watchdog struct {
	address string
}

// NewWatchdog creates a client for the control endpoint at address ("ip:port").
// An empty address disables all control traffic (the client becomes a no-op).
func NewWatchdog(address string) *Watchdog {
	return &Watchdog{address: address}
}

// Enabled reports whether a control endpoint is configured.
func (wd *Watchdog) Enabled() bool {
	return wd != nil && wd.address != ""
}

// maxExecTimeout bounds a command that is sent with no caller-supplied deadline
// (the fire-and-forget Send), so a stalled control endpoint cannot block a
// goroutine and its connection forever.
const maxExecTimeout = 2 * time.Minute

// Exec sends a single command and returns the one-line response. A dedicated
// connection is opened per command so that a long op (e.g. start, which blocks
// until the container is healthy) does not delay unrelated commands. The whole
// operation is always deadline-bounded: by the caller's context if it has one,
// otherwise by maxExecTimeout.
func (wd *Watchdog) Exec(ctx context.Context, command string) (string, error) {
	if !wd.Enabled() {
		return "", nil
	}
	deadline := time.Now().Add(maxExecTimeout)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	conn, err := net.DialTimeout("tcp", wd.address, 10*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	if _, err := conn.Write([]byte(command + "\n")); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// Send fires a command without requiring the caller to wait, preserving the
// previous fire-and-forget behaviour used for restart-on-demand. The outcome
// is logged.
func (wd *Watchdog) Send(command string) {
	go func() {
		resp, err := wd.Exec(context.Background(), command)
		if err != nil {
			log.Printf("Error sending command %q: %s", command, err)
			return
		}
		if resp == "ok" {
			log.Printf("Command %q accepted", command)
		} else if resp != "" {
			log.Printf("Command %q result: %s", command, resp)
		}
	}()
}

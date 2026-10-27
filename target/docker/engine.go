package docker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// engine is one Docker engine the installation runs on: a host of the
// environment's hosts, or the one engine when it names none.
type engine struct {
	// name is the host's name in the environment; "" — the one engine.
	name string
	// address is how a neighbour reaches the host: an IP or a DNS name.
	address       string
	host, context string // docker's --host, --context
	bin           string
}

// command is the docker CLI against the engine.
func (e *engine) command(ctx context.Context, args ...string) *exec.Cmd {
	var base []string
	if e.host != "" {
		base = append(base, "--host", e.host)
	}
	if e.context != "" {
		base = append(base, "--context", e.context)
	}
	return exec.CommandContext(ctx, e.bin, append(base, args...)...)
}

// docker runs the docker CLI against the engine; stdin, when set, is what -
// reads, env is added to the CLI's environment. Output goes to events line
// by line.
func (e *engine) docker(ctx context.Context, stdin io.Reader, env []string, events func(string), args ...string) ([]byte, error) {
	cmd := e.command(ctx, args...)
	cmd.Stdin = stdin
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if events != nil {
		sc := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				events(e.prefix() + line)
			}
		}
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("%sdocker %s: %s", e.prefix(), args[0], msg)
	}
	return stdout.Bytes(), nil
}

// lines runs docker and returns its output's non-empty lines.
func (e *engine) lines(ctx context.Context, args ...string) ([]string, error) {
	out, err := e.docker(ctx, nil, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	var ls []string
	for l := range strings.SplitSeq(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			ls = append(ls, l)
		}
	}
	return ls, nil
}

// prefix names the host in what is told of it, when there are hosts.
func (e *engine) prefix() string {
	if e.name == "" {
		return ""
	}
	return e.name + ": "
}

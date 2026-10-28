//go:build unix

package provider

import (
	"os/exec"
	"syscall"
)

// detach puts the provider in a process group of its own. An interrupt at
// the terminal goes to the foreground group — pack's — and pack decides
// what becomes of the step under way: it cancels it and rolls back through
// the provider, which must still be there, its docker or kubectl with it.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

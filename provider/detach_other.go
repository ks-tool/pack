//go:build !unix

package provider

import "os/exec"

func detach(*exec.Cmd) {}

//go:build !unix

package main

import (
	"os/exec"
	"time"
)

func killTree(cmd *exec.Cmd) *exec.Cmd { cmd.WaitDelay = 3 * time.Second; return cmd }

func stopTree(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

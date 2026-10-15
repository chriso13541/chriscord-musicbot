//go:build unix

package main

import (
	"os/exec"
	"syscall"
	"time"
)

// killTree makes a command stoppable as a whole: it runs in its own
// process group, and cancelling it kills the group — the command and
// anything it started. yt-dlp hands live streams (and some downloads) to
// an ffmpeg of its own; killing only yt-dlp left that ffmpeg running,
// holding the output pipes open, so waiting for the command never ended
// (a skipped live stream hung the player). WaitDelay stops waiting on
// pipes regardless, shortly after.
func killTree(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 3 * time.Second
	return cmd
}

// stopTree kills a started command and everything it started.
func stopTree(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

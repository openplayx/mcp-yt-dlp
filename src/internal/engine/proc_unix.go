//go:build unix

package engine

import (
	"os/exec"
	"syscall"
)

// yt-dlp starts ffmpeg; a new process group lets cancel stop both.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

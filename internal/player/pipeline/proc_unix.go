//go:build !windows

package pipeline

import (
	"os/exec"
	"syscall"
)

// setProcessGroup 让 ffmpeg 独立成组，便于整组回收。
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 杀整个进程组。
// 只 kill 父进程时，ffmpeg 内部的解码线程/子进程可能残留并继续占 CPU。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil && pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	_ = cmd.Process.Kill()
}

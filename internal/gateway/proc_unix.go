//go:build !windows

package gateway

import (
	"os"
	"os/exec"
	"syscall"
)

func setDetachedAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// ProcessAlive checks whether a PID currently exists on Unix systems.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// KillProcessGroup sends SIGTERM to the target process group (falling back to the single PID).
func KillProcessGroup(pid int) error {
	if pid <= 1 || pid == os.Getpid() {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}

// ForceKillProcessGroup sends SIGKILL to the target process group (falling back to the single PID).
func ForceKillProcessGroup(pid int) error {
	if pid <= 1 || pid == os.Getpid() {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}

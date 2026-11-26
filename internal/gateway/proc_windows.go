//go:build windows

package gateway

import (
	"os"
	"os/exec"
)

func setDetachedAttr(cmd *exec.Cmd) {}

func setProcessGroup(cmd *exec.Cmd) {}

func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}

func KillProcessGroup(pid int) error {
	if pid <= 1 || pid == os.Getpid() {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func ForceKillProcessGroup(pid int) error {
	return KillProcessGroup(pid)
}

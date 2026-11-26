//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
	createNoWindow        = 0x08000000
)

func startDetachedWatchdog(treeID string) error {
	if os.Getenv("BSWARM_NO_WATCH") == "1" || os.Getenv("ANTIGRAVITY_LS_ADDRESS") == "" {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "watch", "--tree", treeID)
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup | detachedProcess | createNoWindow,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}

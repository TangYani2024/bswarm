//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
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
	// Inherit parent env in memory (including ANTIGRAVITY_LS_ADDRESS & ANTIGRAVITY_CSRF_TOKEN) without touching disk.
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}

//go:build windows

package gateway

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
	createNoWindow        = 0x08000000

	processQueryLimitedInfo = 0x1000
	stillActiveExitCode     = 259
)

var (
	modKernel32          = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess      = modKernel32.NewProc("OpenProcess")
	procGetExitCodeProc  = modKernel32.NewProc("GetExitCodeProcess")
	procCloseHandle      = modKernel32.NewProc("CloseHandle")
)

func setDetachedAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup | detachedProcess | createNoWindow,
	}
}

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup | createNoWindow,
	}
}

func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, _, _ := procOpenProcess.Call(uintptr(processQueryLimitedInfo), 0, uintptr(pid))
	if h == 0 {
		return false
	}
	defer procCloseHandle.Call(h)
	var code uint32
	r1, _, _ := procGetExitCodeProc.Call(h, uintptr(unsafe.Pointer(&code)))
	return r1 != 0 && code == stillActiveExitCode
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

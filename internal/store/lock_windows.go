//go:build windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	modKernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modKernel32.NewProc("LockFileEx")
	procUnlockFileEx = modKernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
)

type fileLock struct {
	f *os.File
}

func lockFileEx(h syscall.Handle, flags uint32) error {
	var ol syscall.Overlapped
	r1, _, err := procLockFileEx.Call(
		uintptr(h),
		uintptr(flags),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}

func unlockFileEx(h syscall.Handle) {
	var ol syscall.Overlapped
	_, _, _ = procUnlockFileEx.Call(
		uintptr(h),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&ol)),
	)
}

func acquireLock(path string) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := lockFileEx(syscall.Handle(f.Fd()), lockfileExclusiveLock); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("LockFileEx %s: %w", path, err)
	}
	return &fileLock{f: f}, nil
}

func tryAcquireLock(path string) (*fileLock, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, fmt.Errorf("mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock file: %w", err)
	}
	err = lockFileEx(syscall.Handle(f.Fd()), lockfileExclusiveLock|lockfileFailImmediately)
	if err != nil {
		_ = f.Close()
		// ERROR_LOCK_VIOLATION (33) means another process holds the lock.
		if errno, ok := err.(syscall.Errno); ok && errno == 33 {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &fileLock{f: f}, true, nil
}

func (l *fileLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	unlockFileEx(syscall.Handle(l.f.Fd()))
	_ = l.f.Close()
	l.f = nil
}

//go:build windows

package certs

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lockCRLFile Windows 实现：LockFileEx（独占锁，阻塞等待）。
// 语义与 Unix Flock 版一致：crl.lock 进程级独占锁，锁竞争时阻塞。
func lockCRLFile(certDir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(certDir, FileCRLLock), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	var ol windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// unlockCRLFile Windows 实现：UnlockFileEx 释放并关闭。
func unlockCRLFile(f *os.File) {
	if f == nil {
		return
	}
	var ol windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
	_ = f.Close()
}

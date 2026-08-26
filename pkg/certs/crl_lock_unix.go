//go:build !windows

package certs

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockCRLFile 打开并独占锁住吊销锁文件（crl.lock，syscall.Flock LOCK_EX，
// 锁竞争时阻塞等待而非失败）。锁文件独立于 crl.json/crl.pem——这两个文件
// 会被 writeFileAtomic 原子替换（rename），若直接锁它们，换名后新 inode
// 可被并发方立即锁住，互斥失效；crl.lock 从不被替换/删除，锁语义稳定。
// 返回的 *os.File 需由 unlockCRLFile 释放。
func lockCRLFile(certDir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(certDir, FileCRLLock), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// unlockCRLFile 释放吊销锁并关闭锁文件（Flock 随 fd 关闭自动释放，
// 显式解锁保证同一进程内其他等待者尽快获得锁）。
func unlockCRLFile(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

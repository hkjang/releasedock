//go:build linux

package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// AcquireRunnerHostLock enforces the single Runner process assumed by the
// execute-only credential handoff tree. Keeping the returned descriptor open
// holds the advisory lock; the kernel releases it even after SIGKILL.
func AcquireRunnerHostLock(root string) (*os.File, error) {
	if err := validateOwnedDirectory(root, uint32(os.Geteuid()), uint32(os.Getegid()), 0o710, false); err != nil {
		return nil, fmt.Errorf("validate Runner lock RuntimeDirectory: %w", err)
	}
	path := filepath.Join(filepath.Clean(root), RunnerHostLockFile)
	descriptor, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Runner host lock: %w", err)
	}
	file := os.NewFile(uintptr(descriptor), path)
	fail := func(err error) (*os.File, error) {
		_ = file.Close()
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("secure Runner host lock: %w", err))
	}
	if err := validateRunnerLockFile(path); err != nil {
		return fail(err)
	}
	if err := syscall.Flock(descriptor, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fail(errors.New("another releasedock-runner process is already active on this host"))
		}
		return fail(fmt.Errorf("lock Runner host process: %w", err))
	}
	return file, nil
}

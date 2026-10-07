package lock

import (
	"os"
	"path/filepath"
	"syscall"
)

// Hold is an exclusive cross-process lock held until Release.
type Hold struct {
	f *os.File
}

// Acquire takes an exclusive flock on path (creating parent dirs).
// Blocks until the lock is available.
func Acquire(path string) (*Hold, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return &Hold{f: f}, nil
}

// Release unlocks and closes.
func (h *Hold) Release() error {
	if h == nil || h.f == nil {
		return nil
	}
	_ = syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN)
	return h.f.Close()
}

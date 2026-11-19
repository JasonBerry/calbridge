//go:build unix

// Package lock enforces a single running calbridge per database with an
// exclusive, non-blocking flock on <db>.lock (PLAN.md §9).
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked means another process holds the lock.
var ErrLocked = errors.New("lock: another calbridge is running with this database")

// Lock is a held lock. The kernel releases it if the process dies.
type Lock struct {
	f *os.File
}

// Acquire takes the lock at path (created 0600 if missing) or returns
// ErrLocked immediately.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { // #nosec G703 -- operator-configured store path
		return nil, fmt.Errorf("lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 G703 -- operator-configured store path
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- fd fits in int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock: %w", err)
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN) // #nosec G115 -- fd fits in int
	return errors.Join(err, l.f.Close())
}

// PathFor returns the lock path for a database file.
func PathFor(dbPath string) string { return dbPath + ".lock" }

//go:build unix

package collector

import (
	"errors"
	"os"
	"syscall"
)

// lockFile opens path and takes a non-blocking flock on it. flock locks
// belong to the open file, so a second open in the same process conflicts
// too.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil
}

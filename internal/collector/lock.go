package collector

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrLocked means another dotastats process holds the data directory's
// collector lock: a running serve, or a CLI collect, backfill, prune or
// reextract.
var ErrLocked = errors.New("another dotastats process is using this data directory")

// Lock is an exclusive lock on <data-dir>/collector.lock. serve holds it
// for its whole life, CLI commands for their run, so a CLI run never races
// the server's scheduler. The OS drops it when the process exits, so a
// crash never leaves a stale lock.
type Lock struct{ f *os.File }

// AcquireLock takes the lock or returns ErrLocked without waiting.
func AcquireLock(dataDir string) (*Lock, error) {
	f, err := lockFile(filepath.Join(dataDir, "collector.lock"))
	if err != nil {
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() error { return l.f.Close() }

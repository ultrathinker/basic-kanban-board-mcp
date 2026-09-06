//go:build windows

package app

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockExclusive takes a non-blocking exclusive byte-range lock on the first
// byte of the file, via x/sys/windows because the stdlib syscall package
// does not expose LockFileEx (x/sys is already pinned in go.mod as a
// modernc.org/sqlite dependency; importing it adds no requirement).
//
// Windows releases LockFileEx ranges when the handle closes, so a killed
// process cannot leave a stale claim behind. FAIL_IMMEDIATELY turns
// contention into ERROR_LOCK_VIOLATION instead of a wait.
func lockExclusive(f *os.File) error {
	const flags = windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{})
	if err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return errLockHeld
		}
		return err
	}
	return nil
}

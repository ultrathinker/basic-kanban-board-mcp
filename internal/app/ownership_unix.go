//go:build unix

package app

import (
	"errors"
	"os"
	"syscall"
)

// lockExclusive takes a non-blocking exclusive flock on the whole file.
// flock locks live on the open file description, so they die with the process
// even on SIGKILL — the property that makes a crashed server unable to brick
// the install. A second open of the same file (even inside the same process)
// is a different description and therefore genuinely refuses, which is what
// the ownership tests rely on.
func lockExclusive(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			// EWOULDBLOCK is "another descriptor holds it" in non-blocking
			// mode; EAGAIN is the same errno value on some platforms.
			return errLockHeld
		}
		return err
	}
	return nil
}

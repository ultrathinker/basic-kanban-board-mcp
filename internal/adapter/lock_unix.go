//go:build !windows

package adapter

import (
	"errors"
	"os"
	"syscall"
)

var errAdapterLockHeld = errors.New("adapter lock held")

func lockAdapterFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errAdapterLockHeld
	}
	return err
}

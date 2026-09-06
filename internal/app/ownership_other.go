//go:build !unix && !windows

package app

import (
	"errors"
	"os"
)

// lockExclusive refuses on platforms with no byte-range lock primitive. Failing
// loudly here is correct: this program's correctness depends on the ownership
// lock, and silently serving without it would reintroduce exactly the
// multi-process corruption the lock exists to prevent.
func lockExclusive(*os.File) error {
	return errors.New("ownership locks are not supported on this platform")
}

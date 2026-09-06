package auth

import (
	"crypto/rand"
)

// readRandom fills buf with cryptographic random bytes. It is a thin wrapper
// so tests can substitute a deterministic source without monkey-patching
// crypto/rand (which is impossible from another package).
var readRandom = func(buf []byte) error {
	_, err := rand.Read(buf)
	return err
}

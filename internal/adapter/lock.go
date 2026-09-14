package adapter

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrInstanceHeld means an adapter with the same identity, session, and work
// directory is already active on this machine.
var ErrInstanceHeld = errors.New("adapter instance already active")

// InstanceLock is an OS-backed local process ownership claim.
type InstanceLock struct {
	file *os.File
}

// AcquireInstance prevents two local consumers from reading for the same CLI
// identity and session. It is intentionally not advertised as a distributed
// lock: separate machines need separate identities or an external supervisor.
func AcquireInstance(identity, session, workDir string) (*InstanceLock, error) {
	if workDir == "" {
		return nil, errors.New("work directory is required for adapter instance ownership")
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve adapter work directory: %w", err)
	}
	sum := sha256.Sum256([]byte(identity + "\x00" + session + "\x00" + filepath.Clean(abs)))
	path := filepath.Join(abs, fmt.Sprintf(".kanban-adapter-%x.lock", sum[:8]))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open adapter instance lock: %w", err)
	}
	if err := lockAdapterFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errAdapterLockHeld) {
			return nil, fmt.Errorf("%w: identity %q, session %q, work directory %s; stop the existing adapter or use a different identity", ErrInstanceHeld, identity, session, abs)
		}
		return nil, fmt.Errorf("lock adapter instance: %w", err)
	}
	return &InstanceLock{file: f}, nil
}

// Release releases this process's adapter ownership claim.
func (l *InstanceLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

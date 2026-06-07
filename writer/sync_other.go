//go:build !linux

package writer

import (
	"fmt"
	"os"
)

// fdatasync falls back to f.Sync() on platforms without syscall.Fdatasync
// (notably macOS).
func fdatasync(f *os.File) error {
	if err := f.Sync(); err != nil {
		return fmt.Errorf("writer: sync: %w", err)
	}

	return nil
}

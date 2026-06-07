//go:build linux

package writer

import (
	"fmt"
	"os"
	"syscall"
)

// fdatasync issues a data-only sync on Linux via syscall.Fdatasync, mirroring
// Tarantool's WAL sync (cheaper than full fsync because inode metadata is
// not flushed).
func fdatasync(f *os.File) error {
	if err := syscall.Fdatasync(int(f.Fd())); err != nil { //nolint:gosec // G115: a file descriptor fits in int.
		return fmt.Errorf("writer: sync: %w", err)
	}

	return nil
}

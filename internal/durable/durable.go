// Package durable holds low-level helpers for making filesystem operations
// survive power loss. Its single export, SyncDir, fsyncs a directory so that
// a rename or create within it is durable — the metadata operation that
// publishes a file under its final name is not flushed by fsyncing the file
// contents alone.
package durable

import (
	"fmt"
	"os"
)

// SyncDir fsyncs the directory at dirPath.
//
// fsyncing a file flushes its data and inode, but NOT the parent directory
// entry that names it. After an atomic rename `<x>.inprogress` → `<x>`, only a
// directory fsync makes the new name durable; without it a power loss can
// leave the file stranded under its old name (or absent) even though the
// writer's Close returned success. Tarantool issues the same directory sync
// (xdir_sync) after publishing a WAL segment.
//
// The directory is opened read-only purely to obtain a descriptor to sync;
// nothing is written through it. On platforms where a directory cannot be
// opened for sync the open will fail and the error is returned to the caller.
func SyncDir(dirPath string) error {
	d, err := os.Open(dirPath)
	if err != nil {
		return fmt.Errorf("durable: open dir %q: %w", dirPath, err)
	}

	syncErr := d.Sync()
	closeErr := d.Close()

	if syncErr != nil {
		return fmt.Errorf("durable: sync dir %q: %w", dirPath, syncErr)
	}

	if closeErr != nil {
		return fmt.Errorf("durable: close dir %q: %w", dirPath, closeErr)
	}

	return nil
}

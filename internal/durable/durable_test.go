package durable_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/internal/durable"
)

// SyncDir succeeds on a real directory: it opens the dir read-only, fsyncs the
// descriptor, and closes it.
func TestSyncDirRealDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	require.NoError(t, durable.SyncDir(dir))
}

// SyncDir surfaces an error (rather than panicking) when the path does not
// exist — callers depend on this to report a failed durability barrier.
func TestSyncDirMissingDirectory(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "does-not-exist")

	require.Error(t, durable.SyncDir(missing))
}

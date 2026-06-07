package durable_test

// coverage_extra_test.go adds targeted tests for the error branches in SyncDir
// that were not covered by the existing suite:
//
//   - SyncDir: Sync() error branch — triggered on macOS by passing /dev/null,
//     which opens successfully as read-only but whose Sync() returns ENOTSUP
//     ("operation not supported by device").
//
// The d.Close() error branch cannot be reached on macOS through the public API
// without interfering with OS internals (a successful Close never fails on a
// locally-opened fd on APFS/HFS+), so it remains uncovered.

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/internal/durable"
)

// TestSyncDir_SyncError exercises the Sync() failure path inside SyncDir.
// On macOS, /dev/null opens read-only without error but its Sync() returns
// ENOTSUP ("operation not supported by device"), which SyncDir wraps and
// returns. On Linux /dev/null Sync returns nil, so the test is macOS-only.
func TestSyncDir_SyncError(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "darwin" {
		t.Skip("Sync-on-/dev/null errors only on macOS; skipping on " + runtime.GOOS)
	}

	err := durable.SyncDir("/dev/null")
	require.Error(t, err, "SyncDir(/dev/null) must fail on macOS")
}

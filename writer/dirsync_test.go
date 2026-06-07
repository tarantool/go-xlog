package writer //nolint:testpackage // white-box: swaps the unexported syncDir seam to observe Close's directory fsync.

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// errSyncDirBoom is a static sentinel injected through the syncDir seam to
// assert Close propagates directory-fsync failures.
var errSyncDirBoom = errors.New("sync dir boom")

// withSyncDirSpy swaps the package-level syncDir seam for a recorder and
// restores it on cleanup. The returned slice accumulates every directory path
// Close asks to fsync. Tests using it must NOT call t.Parallel(): syncDir is a
// shared global, so the swap is only safe during the sequential test phase.
func withSyncDirSpy(t *testing.T) *[]string {
	t.Helper()

	var calls []string

	orig := syncDir
	syncDir = func(dir string) error {
		calls = append(calls, dir)

		return orig(dir) // Still perform the real sync so durability holds.
	}

	t.Cleanup(func() { syncDir = orig })

	return &calls
}

// Reproduction for the missing directory fsync after rename: a successful
// Close must fsync the parent directory so the `.inprogress` → final rename is
// durable. Before the fix Close never touched the directory and this assertion
// failed (zero recorded calls).
//
//nolint:paralleltest // swaps the package-level syncDir seam; must not run in parallel.
func TestCloseFsyncsParentDir(t *testing.T) {
	calls := withSyncDirSpy(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "00000000000000000001.xlog")

	w, err := Create(path, newMeta(t))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	require.Equal(t, []string{dir}, *calls,
		"Close must fsync exactly the parent directory of the final file after rename")
}

// SyncNone opts out of Close-time durability, so it must also skip the
// directory fsync — the directory barrier follows the same policy as the file
// fsync it complements.
//
//nolint:paralleltest // swaps the package-level syncDir seam; must not run in parallel.
func TestCloseSyncNoneSkipsDirFsync(t *testing.T) {
	calls := withSyncDirSpy(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "00000000000000000001.xlog")

	w, err := Create(path, newMeta(t), Sync(SyncNone))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	require.Empty(t, *calls, "SyncNone must not fsync the parent directory")
}

// A failing directory fsync is surfaced from Close rather than swallowed: a
// silent failure would defeat the durability guarantee the barrier exists for.
//
//nolint:paralleltest // swaps the package-level syncDir seam; must not run in parallel.
func TestCloseDirFsyncErrorPropagates(t *testing.T) {
	orig := syncDir
	syncDir = func(string) error { return errSyncDirBoom }

	t.Cleanup(func() { syncDir = orig })

	dir := t.TempDir()
	path := filepath.Join(dir, "00000000000000000001.xlog")

	w, err := Create(path, newMeta(t))
	require.NoError(t, err)

	err = w.Close()
	require.ErrorIs(t, err, errSyncDirBoom)
}

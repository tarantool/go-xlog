package tools

// SetSyncDirForTest swaps the package-level syncDir seam (used by RewriteMeta
// to fsync the destination directory after rename) for fn, returning a
// restore func. Lets the external test package observe the directory barrier
// without exposing the seam in the public API.
func SetSyncDirForTest(fn func(string) error) func() {
	prev := syncDir
	syncDir = fn

	return func() { syncDir = prev }
}

// SetTestHookAfterBackup installs (or clears, with nil) the crash-injection
// hook that ReplaceInstanceUUIDInPlace fires after writing its undo-log
// sidecar but before the in-place WriteAt. It exists only in test builds so
// the external _test package can simulate a crash inside the torn-write
// window.
func SetTestHookAfterBackup(fn func(path string, valStart, valEnd int)) {
	testHookAfterBackup = fn
}

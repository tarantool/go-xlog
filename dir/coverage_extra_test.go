package dir_test

// coverage_extra_test.go exercises the error-path and edge-case branches
// in the dir package that are not covered by the existing tests. All tests
// are black-box (package dir_test), parallel-safe, and use t.TempDir() for
// filesystem isolation.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/dir"
	"github.com/tarantool/go-xlog/format"
)

// ---------------------------------------------------------------------------
// OpenDir error paths
// ---------------------------------------------------------------------------

// TestOpenDir_NonExistentDir covers the os.ReadDir error branch in OpenDir.
func TestOpenDir_NonExistentDir(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// Point at a subdirectory that was never created.
	_, err := dir.OpenDir(filepath.Join(tmp, "does_not_exist"), format.FiletypeXLOG)
	require.Error(t, err)
}

// TestOpenDir_CorruptFile covers the loadEntry error path: a file whose name
// matches <digits>.xlog but whose content is not a valid xlog header causes
// reader.Open to fail, which propagates up through loadEntry → OpenDir.
func TestOpenDir_CorruptFile(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// A file named 42.xlog with garbage bytes — no valid meta header.
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, "42.xlog"),
		[]byte("this is definitely not an xlog header"),
		0o644,
	))

	_, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.Error(t, err)
}

// TestOpenDir_IncomparableVClock covers the ErrIncomparable branch in
// buildDir. Two adjacent files (by signature) whose VClocks are mutually
// incomparable (neither ≤ the other on the partial order) must be rejected.
//
// Construction: we need sig(vc1) < sig(vc2) but vc1 and vc2 incomparable.
//
//	vc1 = {1: 10, 2:  5} → sig = 15
//	vc2 = {1:  5, 2: 11} → sig = 16
//
// Compare({1:10,2:5}, {1:5,2:11}): id 1 → 10 > 5 (sawGT); id 2 → 5 < 11
// (sawLT) → both flags set → incomparable (ok=false). ✓.
func TestOpenDir_IncomparableVClock(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// vc1: signature 15, written to tmp/15.xlog.
	writeXLog(t, tmp, format.VClock{1: 10, 2: 5}, nil, 1)
	// vc2: signature 16, written to tmp/16.xlog.
	// We pass prevVClock=nil because we are intentionally building a broken
	// chain; the signature-mismatch check runs first, and vc2's sig is 16
	// which matches the filename "16.xlog".
	writeXLog(t, tmp, format.VClock{1: 5, 2: 11}, nil, 2)

	_, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.ErrorIs(t, err, dir.ErrIncomparable)
}

// ---------------------------------------------------------------------------
// OpenDirFS error paths
// ---------------------------------------------------------------------------

// TestOpenDirFS_UnknownFiletype covers the Ext() error branch in OpenDirFS.
func TestOpenDirFS_UnknownFiletype(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{}
	_, err := dir.OpenDirFS(fsys, ".", format.Filetype("BOGUS"))
	require.Error(t, err)
}

// TestOpenDirFS_NonExistentDir covers the fs.ReadDir error branch in
// OpenDirFS (the requested sub-directory does not exist in the FS).
func TestOpenDirFS_NonExistentDir(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{} // empty FS — "nodir" does not exist
	_, err := dir.OpenDirFS(fsys, "nodir", format.FiletypeXLOG)
	require.Error(t, err)
}

// TestOpenDirFS_SkipsSubdirectory covers the de.IsDir() continue branch
// inside OpenDirFS. A MapFile with ModeDir set reports IsDir()==true and
// must be silently skipped rather than causing an error.
func TestOpenDirFS_SkipsSubdirectory(t *testing.T) {
	t.Parallel()

	// Place one valid xlog alongside a directory entry.  The directory must
	// be silently ignored; only the xlog should be indexed.
	tmp := t.TempDir()
	// Build a valid xlog to copy into the MapFS.
	p := writeXLog(t, tmp, format.VClock{1: 0}, nil, 1)
	data, err := os.ReadFile(p)
	require.NoError(t, err)

	sig := format.VClock{1: 0}.Signature()
	name := strconv.FormatInt(sig, 10) + ".xlog"

	fsys := fstest.MapFS{
		name:     &fstest.MapFile{Data: data},
		"subdir": &fstest.MapFile{Mode: fs.ModeDir | 0o755},
	}

	d, err := dir.OpenDirFS(fsys, ".", format.FiletypeXLOG)
	require.NoError(t, err)
	require.Len(t, d.Files(), 1)
}

// TestOpenDirFS_SkipsNonMatchingFile covers the ok=false (skip) branch in
// OpenDirFS for files that match the FS listing but not the <digits><ext>
// pattern (e.g. "README.md" inside the same FS directory).
func TestOpenDirFS_SkipsNonMatchingFile(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	p := writeXLog(t, tmp, format.VClock{1: 0}, nil, 1)
	data, err := os.ReadFile(p)
	require.NoError(t, err)

	sig := format.VClock{1: 0}.Signature()
	name := strconv.FormatInt(sig, 10) + ".xlog"

	fsys := fstest.MapFS{
		name:        &fstest.MapFile{Data: data},
		"README.md": &fstest.MapFile{Data: []byte("hello")},
		"abc.xlog":  &fstest.MapFile{Data: []byte("x")}, // non-digit stem
		"notes.txt": &fstest.MapFile{Data: []byte("y")},
	}

	d, err := dir.OpenDirFS(fsys, ".", format.FiletypeXLOG)
	require.NoError(t, err)
	// Only the properly named valid file should appear.
	require.Len(t, d.Files(), 1)
}

// TestOpenDirFS_CorruptFile covers the loadEntryFS error path: a file whose
// name matches <digits>.xlog but whose content is not a valid xlog header
// causes reader.OpenFS to fail, which propagates up through loadEntryFS →
// OpenDirFS.
func TestOpenDirFS_CorruptFile(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"42.xlog": &fstest.MapFile{Data: []byte("totally corrupt not-an-xlog")},
	}

	_, err := dir.OpenDirFS(fsys, ".", format.FiletypeXLOG)
	require.Error(t, err)
}

// TestOpenDirFS_IncomparableVClock covers the ErrIncomparable branch reached
// via OpenDirFS (same incomparable-vclock scenario as the OpenDir variant).
func TestOpenDirFS_IncomparableVClock(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// vc1 sig=15, vc2 sig=16 are incomparable (see TestOpenDir_IncomparableVClock).
	p1 := writeXLog(t, tmp, format.VClock{1: 10, 2: 5}, nil, 1)
	p2 := writeXLog(t, tmp, format.VClock{1: 5, 2: 11}, nil, 2)

	data1, err := os.ReadFile(p1)
	require.NoError(t, err)
	data2, err := os.ReadFile(p2)
	require.NoError(t, err)

	fsys := fstest.MapFS{
		"15.xlog": &fstest.MapFile{Data: data1},
		"16.xlog": &fstest.MapFile{Data: data2},
	}

	_, err = dir.OpenDirFS(fsys, ".", format.FiletypeXLOG)
	require.ErrorIs(t, err, dir.ErrIncomparable)
}

// ---------------------------------------------------------------------------
// LocateVClock edge cases
// ---------------------------------------------------------------------------

// TestLocateVClock_EmptyDir verifies that LocateVClock on an empty Dir
// returns ErrNotFound (via OpenDirFS to exercise a slightly different path).
func TestLocateVClock_EmptyDirFS(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{}
	d, err := dir.OpenDirFS(fsys, ".", format.FiletypeXLOG)
	require.NoError(t, err)

	_, err = d.LocateVClock(format.VClock{1: 1})
	require.ErrorIs(t, err, dir.ErrNotFound)
}

// TestLocateVClock_BeforeAllFS is the ErrNotFound case where target is
// strictly below the earliest indexed entry's VClock, reached via OpenDirFS.
func TestLocateVClock_BeforeAllFS(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// Write a single file whose VClock starts at {1: 10}.
	p := writeXLog(t, tmp, format.VClock{1: 10}, nil, 11)
	data, err := os.ReadFile(p)
	require.NoError(t, err)

	fsys := fstest.MapFS{
		"10.xlog": &fstest.MapFile{Data: data},
	}

	d, err := dir.OpenDirFS(fsys, ".", format.FiletypeXLOG)
	require.NoError(t, err)

	// Target {1: 5} is strictly below the earliest entry {1: 10}.
	_, err = d.LocateVClock(format.VClock{1: 5})
	require.ErrorIs(t, err, dir.ErrNotFound)
}

// TestLocateVClock_PastEndFS verifies that a target beyond all entries
// still resolves to the last entry (not ErrNotFound) via OpenDirFS.
func TestLocateVClock_PastEndFS(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	p1 := writeXLog(t, tmp, format.VClock{1: 0}, nil, 1)
	p2 := writeXLog(t, tmp, format.VClock{1: 10}, format.VClock{1: 0}, 11)

	data1, err := os.ReadFile(p1)
	require.NoError(t, err)
	data2, err := os.ReadFile(p2)
	require.NoError(t, err)

	fsys := fstest.MapFS{
		"0.xlog":  &fstest.MapFile{Data: data1},
		"10.xlog": &fstest.MapFile{Data: data2},
	}

	d, err := dir.OpenDirFS(fsys, ".", format.FiletypeXLOG)
	require.NoError(t, err)

	got, err := d.LocateVClock(format.VClock{1: 999})
	require.NoError(t, err)
	// Should resolve to the last (highest-signature) entry.
	require.Equal(t, int64(10), got.Signature)
}

// ---------------------------------------------------------------------------
// LocateLSN edge cases
// ---------------------------------------------------------------------------

// TestLocateLSN_EmptyDirFS verifies that LocateLSN on an empty Dir (via
// OpenDirFS) returns ErrNotFound.
func TestLocateLSN_EmptyDirFS(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{}
	d, err := dir.OpenDirFS(fsys, ".", format.FiletypeXLOG)
	require.NoError(t, err)

	_, err = d.LocateLSN(1, 0)
	require.ErrorIs(t, err, dir.ErrNotFound)
}

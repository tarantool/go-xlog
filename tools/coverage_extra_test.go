package tools_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/tools"
)

// writeMetaFile builds a minimal but DecodeMeta-parseable file from m,
// followed by the 4-byte EOF marker, in a fresh temp dir and returns its
// path. ReplaceInstanceUUIDInPlace and RewriteMeta only parse the meta and
// then copy/overwrite around it, so a real tx body is unnecessary for the
// header-span and error-branch tests.
func writeMetaFile(t *testing.T, name string, m *format.Meta) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, format.EncodeMeta(&buf, m), "EncodeMeta")
	buf.Write([]byte{0xD5, 0x10, 0xAD, 0xED}) // format.EOFMarker

	dst := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(dst, buf.Bytes(), 0o600), "write meta file")

	return dst
}

// rawFile writes arbitrary bytes to a fresh temp file and returns its path.
func rawFile(t *testing.T, name string, data []byte) string {
	t.Helper()

	dst := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(dst, data, 0o600), "write raw file")

	return dst
}

// validMeta returns a canonical XLOG meta with a 36-byte Instance UUID.
func validMeta() *format.Meta {
	return &format.Meta{
		Filetype:     format.FiletypeXLOG,
		FormatVer:    "0.13",
		Version:      "2.11.0",
		InstanceUUID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		VClock:       format.VClock{1: 10},
		PrevVClock:   format.VClock{1: 5},
	}
}

// --- ReplaceInstanceUUIDInPlace error branches ---

// A non-existent path: the open fails (recovery is a no-op for the absent
// sidecar) and a wrapped error is returned.
func TestReplaceInPlace_NonexistentPath(t *testing.T) {
	t.Parallel()

	err := tools.ReplaceInstanceUUIDInPlace(
		filepath.Join(t.TempDir(), "nope.xlog"), uuid.New())
	require.Error(t, err)
}

// A file whose bytes are not a valid meta header: DecodeMeta fails and the
// error is surfaced. The file is left untouched.
func TestReplaceInPlace_NotAnXlog(t *testing.T) {
	t.Parallel()

	path := rawFile(t, "garbage.xlog", []byte("this is not an xlog header at all\n"))

	err := tools.ReplaceInstanceUUIDInPlace(path, uuid.New())
	require.Error(t, err)
}

// A header with no Instance/Server line yields ErrUUIDLineNotFound.
func TestReplaceInPlace_NoInstanceLine(t *testing.T) {
	t.Parallel()

	// A valid header that omits the Instance line. EncodeMeta always writes
	// one, so craft the bytes by hand: filetype, version, VClock, blank line.
	header := "XLOG\n0.13\nVClock: {}\n\n"
	path := rawFile(t, "noinstance.xlog", append([]byte(header), 0xD5, 0x10, 0xAD, 0xED))

	err := tools.ReplaceInstanceUUIDInPlace(path, uuid.New())
	require.ErrorIs(t, err, tools.ErrUUIDLineNotFound)
}

// An on-disk UUID written in the 32-char dash-less form parses fine but is
// not 36 bytes, so the in-place overwrite is refused with
// ErrUUIDWidthMismatch and the file is left unmodified.
func TestReplaceInPlace_WidthMismatch(t *testing.T) {
	t.Parallel()

	// 32-char hex form (no dashes) — uuid.Parse accepts it, span is 32 bytes.
	header := "XLOG\n0.13\nInstance: 11111111222233334444555555555555\nVClock: {}\n\n"
	path := rawFile(t, "narrow.xlog", append([]byte(header), 0xD5, 0x10, 0xAD, 0xED))

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	err = tools.ReplaceInstanceUUIDInPlace(path, uuid.New())
	require.ErrorIs(t, err, tools.ErrUUIDWidthMismatch)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "file must be untouched on width mismatch")
	noBackup(t, path)
}

// A stale sidecar that is too corrupt to recover causes the leading recovery
// step to fail, and ReplaceInstanceUUIDInPlace surfaces that error before
// touching the header.
func TestReplaceInPlace_StaleBackupRecoverFails(t *testing.T) {
	t.Parallel()

	path := copyFixture(t)

	// Too-short sidecar: rejected by recovery with ErrCorruptUUIDBackup.
	require.NoError(t, os.WriteFile(path+uuidBackupSuffix, []byte("\x00\x00\x00"), 0o600))

	err := tools.ReplaceInstanceUUIDInPlace(path, uuid.New())
	require.ErrorIs(t, err, tools.ErrCorruptUUIDBackup)
}

// When the sidecar cannot be created (read-only parent directory),
// writeUUIDBackup fails and the error propagates. Skipped on platforms where
// a directory chmod does not block file creation (e.g. running as root).
func TestReplaceInPlace_BackupWriteFails(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}

	if runtime.GOOS == "windows" {
		t.Skip("unix permission model")
	}

	dir := t.TempDir()

	const name = "simple.xlog"

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, testutil.Load(t, name), 0o600))

	// Make the directory read-only so the sidecar cannot be created. The file
	// itself stays writable (O_RDWR open succeeds), so the failure is isolated
	// to the backup-creation step.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := tools.ReplaceInstanceUUIDInPlace(path, uuid.New())
	require.Error(t, err, "expected backup write to fail in read-only dir")
}

// When the sidecar write itself fails mid-stream (the open succeeds but the
// device is full), writeUUIDBackup's Write-error branch fires. Implemented by
// pointing the sidecar path at /dev/full, whose every write returns ENOSPC.
// Skipped where /dev/full is absent (e.g. macOS).
func TestReplaceInPlace_BackupWriteErrno(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("/dev/full not available on this platform")
	}

	path := copyFixture(t)

	// Pre-place the sidecar as a symlink to /dev/full: the O_CREATE|O_TRUNC
	// open follows it and succeeds, but the subsequent Write returns ENOSPC.
	require.NoError(t, os.Symlink("/dev/full", path+uuidBackupSuffix), "symlink sidecar")

	err := tools.ReplaceInstanceUUIDInPlace(path,
		uuid.MustParse("99999999-aaaa-bbbb-cccc-dddddddddddd"))
	require.Error(t, err, "backup write to /dev/full must fail with ENOSPC")
}

// Success path on a hand-built meta file, then recover from a manually-staged
// sidecar — exercises the instanceValueSpan happy path and the
// writeUUIDBackup/removeUUIDBackup pair on a non-fixture file.
func TestReplaceInPlace_BuiltFileRoundtrip(t *testing.T) {
	t.Parallel()

	path := writeMetaFile(t, "built.xlog", validMeta())

	newID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	require.NoError(t, tools.ReplaceInstanceUUIDInPlace(path, newID))
	assert.Equal(t, newID, readInstanceUUID(t, path))
	noBackup(t, path)
}

// --- RecoverInstanceUUIDInPlace error branches ---

// A sidecar that is a directory (not a regular file) makes os.ReadFile fail
// with something other than ErrNotExist, surfacing a wrapped error.
func TestRecover_BackupReadError(t *testing.T) {
	t.Parallel()

	path := copyFixture(t)
	require.NoError(t, os.Mkdir(path+uuidBackupSuffix, 0o700), "mkdir backup dir")

	recovered, err := tools.RecoverInstanceUUIDInPlace(path)
	require.Error(t, err)
	assert.False(t, recovered)
}

// A sidecar whose 8-byte offset has the high bit set decodes to a negative
// int64 and is rejected with ErrCorruptUUIDBackup.
func TestRecover_NegativeOffset(t *testing.T) {
	t.Parallel()

	path := copyFixture(t)

	buf := make([]byte, 8+4)
	binary.BigEndian.PutUint64(buf[:8], 0x8000000000000000) // high bit -> negative
	copy(buf[8:], []byte("abcd"))
	require.NoError(t, os.WriteFile(path+uuidBackupSuffix, buf, 0o600))

	recovered, err := tools.RecoverInstanceUUIDInPlace(path)
	require.ErrorIs(t, err, tools.ErrCorruptUUIDBackup)
	assert.False(t, recovered)

	// Corrupt sidecar left in place for inspection.
	_, statErr := os.Stat(path + uuidBackupSuffix)
	assert.NoError(t, statErr)
}

// A valid sidecar but a missing target file: the open in recovery fails.
func TestRecover_TargetMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "gone.xlog")

	// Stage a well-formed sidecar for a target that does not exist.
	buf := make([]byte, 8+4)
	binary.BigEndian.PutUint64(buf[:8], 20)
	copy(buf[8:], []byte("wxyz"))
	require.NoError(t, os.WriteFile(path+uuidBackupSuffix, buf, 0o600))

	recovered, err := tools.RecoverInstanceUUIDInPlace(path)
	require.Error(t, err)
	assert.False(t, recovered)
}

// --- RewriteMeta error branches ---

func TestRewriteMeta_NilTransform(t *testing.T) {
	t.Parallel()

	srcPath := testutil.Path(t, "simple.xlog")
	dst := filepath.Join(t.TempDir(), "out.xlog")

	err := tools.RewriteMeta(srcPath, dst, nil)
	require.Error(t, err)
}

func TestRewriteMeta_BadSourceMeta(t *testing.T) {
	t.Parallel()

	src := rawFile(t, "bad.xlog", []byte("NOTAFILETYPE\n0.13\n\n"))
	dst := filepath.Join(t.TempDir(), "out.xlog")

	err := tools.RewriteMeta(src, dst, tools.ReplaceInstanceUUID(uuid.New()))
	require.Error(t, err)

	_, statErr := os.Stat(dst + ".inprogress")
	assert.True(t, os.IsNotExist(statErr), ".inprogress must not survive bad meta")
}

// When dst.inprogress already exists, the O_EXCL open fails and nothing is
// overwritten.
func TestRewriteMeta_InprogressExists(t *testing.T) {
	t.Parallel()

	srcPath := testutil.Path(t, "simple.xlog")
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.xlog")

	// Pre-create the staging file so O_CREATE|O_EXCL fails.
	require.NoError(t, os.WriteFile(dst+".inprogress", []byte("squatter"), 0o600))

	err := tools.RewriteMeta(srcPath, dst, tools.ReplaceInstanceUUID(uuid.New()))
	require.Error(t, err)

	// The squatting file is left in place (not removed by the cleanup defer,
	// which only fires when this call created it).
	data, readErr := os.ReadFile(dst + ".inprogress")
	require.NoError(t, readErr)
	assert.Equal(t, "squatter", string(data))
}

// A transform that returns nil yields errNilMetaFromFn and removes the
// partial .inprogress file.
func TestRewriteMeta_NilMetaFromFn(t *testing.T) {
	t.Parallel()

	srcPath := testutil.Path(t, "simple.xlog")
	dst := filepath.Join(t.TempDir(), "out.xlog")

	err := tools.RewriteMeta(srcPath, dst, func(*format.Meta) *format.Meta { return nil })
	require.Error(t, err)

	_, statErr := os.Stat(dst + ".inprogress")
	assert.True(t, os.IsNotExist(statErr), ".inprogress must be cleaned up")
	_, statErr = os.Stat(dst)
	assert.True(t, os.IsNotExist(statErr), "dst must not exist")
}

// A transform that returns a meta EncodeMeta rejects (empty Filetype) makes
// the encode step fail; the partial .inprogress is cleaned up.
func TestRewriteMeta_EncodeFails(t *testing.T) {
	t.Parallel()

	srcPath := testutil.Path(t, "simple.xlog")
	dst := filepath.Join(t.TempDir(), "out.xlog")

	err := tools.RewriteMeta(srcPath, dst, func(m *format.Meta) *format.Meta {
		m.Filetype = "" // EncodeMeta rejects an empty filetype.

		return m
	})
	require.Error(t, err)

	_, statErr := os.Stat(dst + ".inprogress")
	assert.True(t, os.IsNotExist(statErr), ".inprogress must be cleaned up after encode failure")
}

// A syncDir failure after a successful rename surfaces as an error from
// RewriteMeta even though dst is already in place. Uses the test seam.
//
//nolint:paralleltest // mutates the package-level syncDir seam.
func TestRewriteMeta_SyncDirFails(t *testing.T) {
	srcPath := testutil.Path(t, "simple.xlog")
	dst := filepath.Join(t.TempDir(), "out.xlog")

	restore := tools.SetSyncDirForTest(func(string) error {
		return assertErr{}
	})
	defer restore()

	err := tools.RewriteMeta(srcPath, dst, tools.ReplaceInstanceUUID(uuid.New()))
	require.Error(t, err, "syncDir failure must propagate")

	// The rename already happened, so dst exists despite the error.
	_, statErr := os.Stat(dst)
	assert.NoError(t, statErr, "dst should exist after rename even if dir-sync failed")
}

// assertErr is a trivial error used by the syncDir seam.
type assertErr struct{}

func (assertErr) Error() string { return "injected sync error" }

// A syncDir failure inside writeUUIDBackup (the first dir-sync of a replace)
// aborts the operation before the in-place write. Exercises the
// sync-dir-of-sidecar error branch of writeUUIDBackup.
//
//nolint:paralleltest // mutates the package-level syncDir seam.
func TestReplaceInPlace_BackupSyncDirFails(t *testing.T) {
	path := copyFixture(t)

	restore := tools.SetSyncDirForTest(func(string) error { return assertErr{} })
	defer restore()

	err := tools.ReplaceInstanceUUIDInPlace(path,
		uuid.MustParse("99999999-aaaa-bbbb-cccc-dddddddddddd"))
	require.Error(t, err, "writeUUIDBackup dir-sync failure must propagate")
}

// A syncDir that succeeds for writeUUIDBackup but fails on the post-removal
// sync drives the dir-sync error branch of removeUUIDBackup. The replace's
// destructive write has already happened, so the failure is reported after
// the UUID is updated.
//
//nolint:paralleltest // mutates the package-level syncDir seam.
func TestReplaceInPlace_RemoveBackupSyncDirFails(t *testing.T) {
	path := copyFixture(t)

	var calls int

	restore := tools.SetSyncDirForTest(func(string) error {
		calls++
		// writeUUIDBackup issues the first dir-sync (let it pass); the second
		// is removeUUIDBackup after the successful in-place write (fail it).
		if calls >= 2 {
			return assertErr{}
		}

		return nil
	})
	defer restore()

	err := tools.ReplaceInstanceUUIDInPlace(path,
		uuid.MustParse("99999999-aaaa-bbbb-cccc-dddddddddddd"))
	require.Error(t, err, "removeUUIDBackup dir-sync failure must propagate")
	require.GreaterOrEqual(t, calls, 2, "both dir-syncs should have been attempted")
}

// A syncDir failure during a successful recovery drives removeUUIDBackup's
// error return up through RecoverInstanceUUIDInPlace (restore succeeds, then
// the post-removal dir-sync fails).
//
//nolint:paralleltest // mutates the package-level syncDir seam.
func TestRecover_RemoveBackupSyncDirFails(t *testing.T) {
	path := copyFixture(t)

	start, end := instanceSpan(t, path)

	orig := make([]byte, end-start)
	f, err := os.Open(path)
	require.NoError(t, err)
	_, err = f.ReadAt(orig, start)
	require.NoError(t, err)
	_ = f.Close()

	writeBackupFile(t, path, start, orig)

	restore := tools.SetSyncDirForTest(func(string) error { return assertErr{} })
	defer restore()

	recovered, err := tools.RecoverInstanceUUIDInPlace(path)
	require.Error(t, err, "removeUUIDBackup dir-sync failure must propagate")
	assert.False(t, recovered, "a failed recovery should not report success")
}

// A recovery whose sidecar cannot be unlinked (read-only parent directory)
// drives removeUUIDBackup's os.Remove error branch. The WriteAt restore still
// succeeds because the target file itself stays writable; only the directory
// entry removal is blocked.
//
//nolint:paralleltest // chmods its (isolated) temp dir; keep serial.
func TestRecover_RemoveBackupUnlinkFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}

	if runtime.GOOS == "windows" {
		t.Skip("unix permission model")
	}

	dir := t.TempDir()

	const name = "simple.xlog"

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, testutil.Load(t, name), 0o600))

	start, end := instanceSpan(t, path)

	orig := make([]byte, end-start)
	f, err := os.Open(path)
	require.NoError(t, err)
	_, err = f.ReadAt(orig, start)
	require.NoError(t, err)
	_ = f.Close()

	writeBackupFile(t, path, start, orig)

	// Read-only directory: the WriteAt restore still works (file perms intact)
	// but unlinking the sidecar fails.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	recovered, err := tools.RecoverInstanceUUIDInPlace(path)
	require.Error(t, err, "unlink of sidecar in read-only dir must fail")
	assert.False(t, recovered)
}

// RewriteMeta's final rename fails when dstPath is an existing non-empty
// directory: os.Rename cannot replace it. Exercises the rename-error branch.
func TestRewriteMeta_RenameFails(t *testing.T) {
	t.Parallel()

	srcPath := testutil.Path(t, "simple.xlog")
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst-is-a-dir")

	// Make dst a non-empty directory so os.Rename(inprogress, dst) fails.
	require.NoError(t, os.Mkdir(dst, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dst, "occupant"), []byte("x"), 0o600))

	err := tools.RewriteMeta(srcPath, dst, tools.ReplaceInstanceUUID(uuid.New()))
	require.Error(t, err, "rename onto a non-empty dir must fail")

	// The staging file is cleaned up on the failure path.
	_, statErr := os.Stat(dst + ".inprogress")
	assert.True(t, os.IsNotExist(statErr), ".inprogress must be cleaned up after rename failure")
}

// --- instanceValueSpan: legacy "Server:" line and trailing-whitespace trim ---

// A legacy "Server:" line (pre-2.x alias) with a trailing carriage return and
// spaces exercises the Server-prefix branch and the trailing-trim loop of
// instanceValueSpan, then replaces the UUID in place.
func TestReplaceInPlace_LegacyServerLineWithCRLF(t *testing.T) {
	t.Parallel()

	// CRLF line endings plus a trailing space after the UUID value. The value
	// is a canonical 36-byte UUID so the in-place overwrite is allowed.
	header := "XLOG\r\n0.13\r\n" +
		"Version: 2.11.0\r\n" +
		"Server: 11111111-2222-3333-4444-555555555555 \r\n" +
		"VClock: {1: 1}\r\n\r\n"
	path := rawFile(t, "legacy.xlog", append([]byte(header), 0xD5, 0x10, 0xAD, 0xED))

	newID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	require.NoError(t, tools.ReplaceInstanceUUIDInPlace(path, newID))
	assert.Equal(t, newID, readInstanceUUID(t, path))
	noBackup(t, path)
}

// RewriteMeta byte-copies the source tail after the transform. A transform
// that swaps the source file for a directory makes the reopen + seek succeed
// but the io.Copy read fail (EISDIR), exercising the copy-error branch and
// the partial-file cleanup.
func TestRewriteMeta_CopyFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.xlog")
	require.NoError(t, os.WriteFile(srcPath, testutil.Load(t, "simple.xlog"), 0o600))

	dst := filepath.Join(dir, "out.xlog")

	err := tools.RewriteMeta(srcPath, dst, func(m *format.Meta) *format.Meta {
		// Replace the source file with a directory: os.Open still succeeds and
		// the seek is fine, but reading a directory fd errors with EISDIR.
		require.NoError(t, os.Remove(srcPath))
		require.NoError(t, os.Mkdir(srcPath, 0o700))

		return m
	})
	require.Error(t, err, "copy from a directory fd must fail")

	_, statErr := os.Stat(dst + ".inprogress")
	assert.True(t, os.IsNotExist(statErr), ".inprogress must be cleaned up after copy failure")
}

// RewriteMeta reopens the source for the byte-copy stage after running the
// transform. A transform that removes the source as a side effect makes that
// reopen fail, exercising the reopen-error branch and the partial-file
// cleanup defer.
func TestRewriteMeta_ReopenSrcFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.xlog")
	require.NoError(t, os.WriteFile(srcPath, testutil.Load(t, "simple.xlog"), 0o600))

	dst := filepath.Join(dir, "out.xlog")

	err := tools.RewriteMeta(srcPath, dst, func(m *format.Meta) *format.Meta {
		// Side effect: delete the source before RewriteMeta reopens it for
		// the tx-byte copy. The first handle (already parsed) stays valid;
		// the second os.Open fails with ENOENT.
		_ = os.Remove(srcPath)

		return m
	})
	require.Error(t, err, "reopen of a deleted source must fail")

	_, statErr := os.Stat(dst + ".inprogress")
	assert.True(t, os.IsNotExist(statErr), ".inprogress must be cleaned up after reopen failure")
}

// --- remapVC nil-vclock branch (via RemapVClock over a snap) ---

// RemapVClock over a snapshot, whose PrevVClock is nil, exercises the nil
// branch of remapVC for PrevVClock while remapping the non-nil VClock.
func TestRewriteMeta_RemapVClock_NilPrevVClock(t *testing.T) {
	t.Parallel()

	srcPath := testutil.Path(t, "populated.snap")
	dst := filepath.Join(t.TempDir(), "out.snap")

	src, err := reader.Open(srcPath)
	require.NoError(t, err)

	srcMeta := src.Meta()
	require.Nil(t, srcMeta.PrevVClock, "snap fixture should have nil PrevVClock")
	srcVClock := srcMeta.VClock.Clone()
	_ = src.Close()

	// Remap an id that is absent so every entry passes through unchanged but
	// the function still runs over both clocks.
	require.NoError(t, tools.RewriteMeta(srcPath, dst,
		tools.RemapVClock(map[uint32]uint32{999: 1000})))

	out, err := reader.Open(dst)
	require.NoError(t, err)

	defer func() { _ = out.Close() }()

	assert.Equal(t, srcVClock.String(), out.Meta().VClock.String(), "VClock should be unchanged")
	assert.Nil(t, out.Meta().PrevVClock, "PrevVClock should remain nil")
}

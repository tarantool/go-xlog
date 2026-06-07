package tools_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/tools"
)

// uuidBackupSuffix mirrors the unexported constant in inplace.go; the tests
// need to assert on the sidecar's presence/absence directly.
const uuidBackupSuffix = ".uuidbak"

// copyFixture copies the simple.xlog fixture into a fresh temp dir and
// returns the path of the writable copy. ReplaceInstanceUUIDInPlace mutates
// its target, so tests must never point it at the shared fixture.
func copyFixture(t *testing.T) string {
	t.Helper()

	const name = "simple.xlog"

	data := testutil.Load(t, name)
	dst := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(dst, data, 0o600), "write fixture copy")

	return dst
}

// instanceSpan locates the [start, end) byte span of the Instance/Server UUID
// value in path, using the same trimming rules as the production decoder.
func instanceSpan(t *testing.T, path string) (int64, int64) {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err, "read %q", path)

	for lineStart := 0; lineStart < len(data); {
		nl := bytes.IndexByte(data[lineStart:], '\n')
		if nl < 0 {
			break
		}

		lineEnd := lineStart + nl
		line := data[lineStart:lineEnd]

		var keyLen int

		switch {
		case bytes.HasPrefix(line, []byte("Instance:")):
			keyLen = len("Instance:")
		case bytes.HasPrefix(line, []byte("Server:")):
			keyLen = len("Server:")
		default:
			lineStart = lineEnd + 1

			continue
		}

		valStart := lineStart + keyLen
		valEnd := lineEnd

		for valStart < valEnd && data[valStart] == ' ' {
			valStart++
		}

		for valEnd > valStart && (data[valEnd-1] == ' ' || data[valEnd-1] == '\r') {
			valEnd--
		}

		return int64(valStart), int64(valEnd)
	}

	t.Fatalf("no Instance/Server line in %q", path)

	return 0, 0
}

// readInstanceUUID opens path with the reader and returns its instance UUID,
// failing the test if the file does not parse.
func readInstanceUUID(t *testing.T, path string) uuid.UUID {
	t.Helper()

	r, err := reader.Open(path)
	require.NoError(t, err, "reader.Open %q", path)

	defer func() { _ = r.Close() }()

	return r.Meta().InstanceUUID
}

// tornWrite overwrites the middle of [start, end) with non-hex bytes,
// preserving length so following bytes do not shift. It simulates a UUID span
// that a crash left half-rewritten: still 36 bytes, but no longer a valid
// UUID.
func tornWrite(t *testing.T, path string, start, end int64) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err, "open for torn write")

	defer func() { _ = f.Close() }()

	garbage := bytes.Repeat([]byte("Z"), int(end-start)/2)
	_, err = f.WriteAt(garbage, start)
	require.NoError(t, err, "torn write")
}

// writeBackupFile writes a well-formed undo-log sidecar (8-byte big-endian
// offset + original bytes) for path, as the production code would have left
// behind before crashing.
func writeBackupFile(t *testing.T, path string, offset int64, orig []byte) {
	t.Helper()

	buf := make([]byte, 8+len(orig))
	binary.BigEndian.PutUint64(buf[:8], uint64(offset))
	copy(buf[8:], orig)

	require.NoError(t, os.WriteFile(path+uuidBackupSuffix, buf, 0o600), "write backup sidecar")
}

// noBackup asserts no sidecar remains for path.
func noBackup(t *testing.T, path string) {
	t.Helper()

	_, err := os.Stat(path + uuidBackupSuffix)
	assert.Truef(t, os.IsNotExist(err), "sidecar %q should not exist: %v", path+uuidBackupSuffix, err)
}

// TestReplaceInstanceUUIDInPlace_Success — the happy path: the UUID is
// replaced, the tx tail is byte-for-byte unchanged, no sidecar is left
// behind, and the reader sees the new value.
func TestReplaceInstanceUUIDInPlace_Success(t *testing.T) {
	t.Parallel()

	path := copyFixture(t)

	start, end := instanceSpan(t, path)

	before, err := os.ReadFile(path)
	require.NoError(t, err, "read before")

	newID := uuid.MustParse("99999999-aaaa-bbbb-cccc-dddddddddddd")
	require.NoError(t, tools.ReplaceInstanceUUIDInPlace(path, newID), "ReplaceInstanceUUIDInPlace")

	assert.Equal(t, newID, readInstanceUUID(t, path), "new UUID not visible")
	noBackup(t, path)

	after, err := os.ReadFile(path)
	require.NoError(t, err, "read after")

	// Everything outside the UUID span is untouched (same length, same tail).
	require.Len(t, after, len(before), "file length changed")
	assert.Equal(t, before[:start], after[:start], "bytes before UUID changed")
	assert.Equal(t, before[end:], after[end:], "tail bytes (tx blocks + EOF) changed")
}

// TestReplaceInstanceUUIDInPlace_NoOp — replacing the UUID with the value
// already on disk is a no-op: no write, no sidecar, bytes unchanged.
func TestReplaceInstanceUUIDInPlace_NoOp(t *testing.T) {
	t.Parallel()

	path := copyFixture(t)

	before, err := os.ReadFile(path)
	require.NoError(t, err, "read before")

	current := readInstanceUUID(t, path)
	require.NoError(t, tools.ReplaceInstanceUUIDInPlace(path, current), "no-op replace")

	noBackup(t, path)

	after, err := os.ReadFile(path)
	require.NoError(t, err, "read after")
	assert.Equal(t, before, after, "no-op should not change bytes")
}

// TestReplaceInstanceUUIDInPlace_RecoverAfterTornWrite is the reproduction
// for the crash-atomicity bug: a torn in-place write leaves an unparseable
// header. We inject a crash inside the WriteAt window (after the undo log is
// written), confirm the file is genuinely corrupt, then show that the new
// recovery path restores it to the consistent pre-write value.
//
//nolint:paralleltest // mutates the package-level test hook; must run alone.
func TestReplaceInstanceUUIDInPlace_RecoverAfterTornWrite(t *testing.T) {
	path := copyFixture(t)

	origID := readInstanceUUID(t, path)

	// Simulate a crash mid-write: corrupt the UUID span, then bail out via
	// panic so the production WriteAt/Sync/remove-backup steps never run.
	tools.SetTestHookAfterBackup(func(p string, start, end int) {
		tornWrite(t, p, int64(start), int64(end))
		panic("simulated crash inside torn-write window")
	})
	t.Cleanup(func() { tools.SetTestHookAfterBackup(nil) })

	newID := uuid.MustParse("99999999-aaaa-bbbb-cccc-dddddddddddd")

	func() {
		defer func() {
			r := recover()
			require.NotNil(t, r, "expected simulated crash")
		}()

		_ = tools.ReplaceInstanceUUIDInPlace(path, newID)
	}()

	// Corruption demonstrated: the header is now torn and will not parse...
	_, err := reader.Open(path)
	require.Error(t, err, "expected torn header to be unparseable")

	// ...but the undo log survived the crash.
	_, statErr := os.Stat(path + uuidBackupSuffix)
	require.NoError(t, statErr, "sidecar should survive the crash")

	// Recovery rolls the file back to the consistent pre-write value.
	recovered, err := tools.RecoverInstanceUUIDInPlace(path)
	require.NoError(t, err, "RecoverInstanceUUIDInPlace")
	assert.True(t, recovered, "expected a recovery to be performed")

	assert.Equal(t, origID, readInstanceUUID(t, path), "recovery did not restore original UUID")
	noBackup(t, path)
}

// TestReplaceInstanceUUIDInPlace_AutoHealsStaleSidecar — a new call finds a
// sidecar left by a prior interrupted run (with a torn header on disk),
// silently recovers, and then applies the requested replacement.
func TestReplaceInstanceUUIDInPlace_AutoHealsStaleSidecar(t *testing.T) {
	t.Parallel()

	path := copyFixture(t)

	start, end := instanceSpan(t, path)

	orig := make([]byte, end-start)
	f, err := os.Open(path)
	require.NoError(t, err, "open to snapshot original UUID")
	_, err = f.ReadAt(orig, start)
	require.NoError(t, err, "read original UUID bytes")

	_ = f.Close()

	// Stage a crashed state: a torn header plus a valid undo log.
	writeBackupFile(t, path, start, orig)
	tornWrite(t, path, start, end)

	// A normal call should heal the sidecar on entry, then apply the new ID.
	newID := uuid.MustParse("12121212-3434-5656-7878-9a9a9a9a9a9a")
	require.NoError(t, tools.ReplaceInstanceUUIDInPlace(path, newID), "ReplaceInstanceUUIDInPlace over stale sidecar")

	assert.Equal(t, newID, readInstanceUUID(t, path), "new UUID not applied after auto-heal")
	noBackup(t, path)
}

// TestRecoverInstanceUUIDInPlace_NoSidecar — recovery with no sidecar present
// is a no-op that reports (false, nil) and changes nothing.
func TestRecoverInstanceUUIDInPlace_NoSidecar(t *testing.T) {
	t.Parallel()

	path := copyFixture(t)

	before, err := os.ReadFile(path)
	require.NoError(t, err, "read before")

	recovered, err := tools.RecoverInstanceUUIDInPlace(path)
	require.NoError(t, err, "RecoverInstanceUUIDInPlace")
	assert.False(t, recovered, "no recovery should be reported")

	after, err := os.ReadFile(path)
	require.NoError(t, err, "read after")
	assert.Equal(t, before, after, "no-op recovery changed bytes")
}

// TestRecoverInstanceUUIDInPlace_CorruptBackup — a too-short sidecar is
// rejected with ErrCorruptUUIDBackup and left in place for inspection.
func TestRecoverInstanceUUIDInPlace_CorruptBackup(t *testing.T) {
	t.Parallel()

	path := copyFixture(t)

	// Fewer than the 8-byte offset header → unusable.
	require.NoError(t, os.WriteFile(path+uuidBackupSuffix, []byte("\x00\x00\x00"), 0o600), "write short sidecar")

	recovered, err := tools.RecoverInstanceUUIDInPlace(path)
	require.ErrorIs(t, err, tools.ErrCorruptUUIDBackup, "expected ErrCorruptUUIDBackup")
	assert.False(t, recovered, "no recovery on corrupt backup")

	_, statErr := os.Stat(path + uuidBackupSuffix)
	assert.NoError(t, statErr, "corrupt sidecar should be left in place")
}

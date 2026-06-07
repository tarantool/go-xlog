package tools_test

import (
	"bufio"
	"errors"
	"io"
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

// metaEndOffset re-parses the meta from disk and returns the byte offset
// of the first byte AFTER the blank-line terminator.
func metaEndOffset(t *testing.T, path string) int64 {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err, "open %q", path)

	defer func() { _ = f.Close() }()

	// Count bytes by reading one byte at a time via bufio so we can detect
	// the blank line terminator deterministically. For test code this is
	// fine — fixtures are small.
	br := bufio.NewReader(f)

	var off int64

	for {
		line, err := br.ReadString('\n')
		require.NoError(t, err, "read meta line")

		off += int64(len(line))
		if line == "\n" || line == "\r\n" {
			return off
		}
	}
}

// TestRewriteMeta_ReplaceInstanceUUID — read simple.xlog, rewrite the
// Instance UUID, and assert:
//  1. Reader sees the new UUID and unchanged other meta fields.
//  2. The byte range src[srcMetaEnd:EOF] equals dst[dstMetaEnd:EOF]
//     (tx blocks + EOF marker are copied verbatim).
//  3. Reader-roundtrip of dst yields the same number of rows as src.
func TestRewriteMeta_ReplaceInstanceUUID(t *testing.T) {
	t.Parallel()

	srcPath := testutil.Path(t, "simple.xlog")
	tmp := t.TempDir()
	dstPath := filepath.Join(tmp, "rewritten.xlog")

	newID := uuid.MustParse("99999999-aaaa-bbbb-cccc-dddddddddddd")

	// Capture original meta so we can compare unchanged fields.
	origReader, err := reader.Open(srcPath)
	require.NoError(t, err, "reader.Open src")

	origMeta := origReader.Meta()
	origFiletype := origMeta.Filetype
	origFormatVer := origMeta.FormatVer
	origVersion := origMeta.Version
	origVClock := origMeta.VClock.Clone()
	origPrevVClock := origMeta.PrevVClock.Clone()
	origRows := countRowsReader(t, origReader)
	_ = origReader.Close()

	require.NoError(t, tools.RewriteMeta(srcPath, dstPath, tools.ReplaceInstanceUUID(newID)), "RewriteMeta")

	// 1. Reader sees the new UUID and unchanged other meta fields.
	dst, err := reader.Open(dstPath)
	require.NoError(t, err, "reader.Open dst")

	dm := dst.Meta()
	assert.Equal(t, newID, dm.InstanceUUID, "dst.InstanceUUID")
	assert.Equal(t, origFiletype, dm.Filetype, "dst.Filetype")
	assert.Equal(t, origFormatVer, dm.FormatVer, "dst.FormatVer")
	assert.Equal(t, origVersion, dm.Version, "dst.Version")
	assert.Equal(t, origVClock.String(), dm.VClock.String(), "dst.VClock")
	assert.Equal(t, origPrevVClock.String(), dm.PrevVClock.String(), "dst.PrevVClock")

	// 3. Reader-roundtrip of dst yields the same row count as src.
	dstRows := countRowsReader(t, dst)
	_ = dst.Close()

	assert.Equal(t, origRows, dstRows, "dst row count")

	// 2. Byte-for-byte tail check.
	srcBytes, err := os.ReadFile(srcPath)
	require.NoError(t, err, "read src")
	dstBytes, err := os.ReadFile(dstPath)
	require.NoError(t, err, "read dst")
	srcEnd := metaEndOffset(t, srcPath)
	dstEnd := metaEndOffset(t, dstPath)
	srcTail := srcBytes[srcEnd:]
	dstTail := dstBytes[dstEnd:]
	assert.Equal(t, srcTail, dstTail, "tail bytes differ")
}

// TestRewriteMeta_RemapVClock — RemapVClock({1: 7}) renumbers replica 1 to
// 7 in both VClock and PrevVClock. The test only checks the meta-level
// remap; the row ReplicaID values are NOT touched by this transform
// (caveat documented on the transform).
func TestRewriteMeta_RemapVClock(t *testing.T) {
	t.Parallel()

	srcPath := testutil.Path(t, "populated.snap")
	tmp := t.TempDir()
	dstPath := filepath.Join(tmp, "remapped.snap")

	// Capture src vclock for "before" reference.
	srcReader, err := reader.Open(srcPath)
	require.NoError(t, err, "reader.Open src")

	srcVClock := srcReader.Meta().VClock.Clone()
	_ = srcReader.Close()

	if _, ok := srcVClock[1]; !ok {
		t.Skipf("populated.snap missing replica 1 in VClock; remap fixture would be a no-op: %v", srcVClock)
	}

	require.NoError(t, tools.RewriteMeta(srcPath, dstPath, tools.RemapVClock(map[uint32]uint32{1: 7})), "RewriteMeta")

	dst, err := reader.Open(dstPath)
	require.NoError(t, err, "reader.Open dst")

	defer func() { _ = dst.Close() }()

	dv := dst.Meta().VClock

	_, stillThere := dv[1]
	assert.False(t, stillThere, "dst.VClock still has replica 1: %v", dv)
	assert.Equal(t, srcVClock[1], dv[7], "dst.VClock[7] should equal the old VClock[1]")
}

// TestRewriteMeta_MissingSource — opening a non-existent source returns
// an error and never creates a destination .inprogress file.
func TestRewriteMeta_MissingSource(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	dst := filepath.Join(tmp, "should-not-appear.xlog")
	err := tools.RewriteMeta(filepath.Join(tmp, "nope.xlog"), dst, tools.ReplaceInstanceUUID(uuid.New()))
	require.Error(t, err, "expected error for missing source")

	_, statErr := os.Stat(dst)
	assert.True(t, os.IsNotExist(statErr), "destination should not exist after failed open: %v", statErr)
	_, statErr = os.Stat(dst + ".inprogress")
	assert.True(t, os.IsNotExist(statErr), ".inprogress should not exist after failed open: %v", statErr)
}

// countRowsReader is a small helper that drains the reader and returns the
// total row count. Leaves the reader closed-by-caller.
func countRowsReader(t *testing.T, r *reader.Reader) int64 {
	t.Helper()

	var n int64

	for {
		_, err := r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return n
			}

			require.NoError(t, err, "Next")
		}

		n++
	}
}

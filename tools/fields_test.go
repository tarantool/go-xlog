package tools_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/tools"
)

// TestRewriteMetaFields_AllFields overwrites uuid, version, and vclock on copy
// and asserts they change while the tx blocks (tail bytes) and row count are
// preserved verbatim.
func TestRewriteMetaFields_AllFields(t *testing.T) {
	t.Parallel()

	srcPath := testutil.Path(t, "populated.snap")
	dstPath := filepath.Join(t.TempDir(), "fields.snap")

	newID := uuid.MustParse("99999999-aaaa-bbbb-cccc-dddddddddddd")
	newVClock := format.VClock{2: 99}

	require.NoError(t, tools.RewriteMetaFields(srcPath, dstPath,
		tools.WithInstanceUUID(newID),
		tools.WithVersion("3.8.0-rewritten"),
		tools.WithVClock(newVClock),
		tools.WithFormatVer(format.FormatVersion),
	))

	dm, err := reader.ReadHeader(dstPath)
	require.NoError(t, err)
	assert.Equal(t, newID, dm.InstanceUUID, "InstanceUUID")
	assert.Equal(t, "3.8.0-rewritten", dm.Version, "Version")
	assert.Equal(t, newVClock.String(), dm.VClock.String(), "VClock")
	assert.Equal(t, "0.13", dm.FormatVer, "FormatVer")

	assertTailEqual(t, srcPath, dstPath)
}

// TestRewriteMetaFields_FormatVersion retargets the format-version line to the
// legacy 0.12 and reads it back via the AcceptV012 option.
func TestRewriteMetaFields_FormatVersion(t *testing.T) {
	t.Parallel()

	srcPath := testutil.Path(t, "simple.xlog")
	dstPath := filepath.Join(t.TempDir(), "v012.xlog")

	require.NoError(t, tools.RewriteMetaFields(srcPath, dstPath,
		tools.WithFormatVer(format.LegacyFormatVersion)))

	dm, err := reader.ReadHeader(dstPath, reader.AcceptV012())
	require.NoError(t, err)
	assert.Equal(t, "0.12", dm.FormatVer, "FormatVer retargeted to 0.12")

	assertTailEqual(t, srcPath, dstPath)
}

// TestRewriteMetaFields_NoOpts is a meta-preserving copy.
func TestRewriteMetaFields_NoOpts(t *testing.T) {
	t.Parallel()

	srcPath := testutil.Path(t, "simple.xlog")
	dstPath := filepath.Join(t.TempDir(), "copy.xlog")

	require.NoError(t, tools.RewriteMetaFields(srcPath, dstPath))

	src, err := os.ReadFile(srcPath)
	require.NoError(t, err)
	dst, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	assert.Equal(t, src, dst, "no-opt rewrite should reproduce the file byte-for-byte")
}

// TestReplaceInstanceUUIDInPlace patches the UUID directly: the file keeps its
// exact size, every byte except the 36-byte UUID span is unchanged, the new
// UUID reads back, and the rows still decode.
func TestReplaceInstanceUUIDInPlace(t *testing.T) {
	t.Parallel()

	path := copyToTemp(t, testutil.Path(t, "simple.xlog"))

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	origMeta, err := reader.ReadHeader(path)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, origMeta.InstanceUUID)

	newID := uuid.MustParse("99999999-aaaa-bbbb-cccc-dddddddddddd")
	require.NoError(t, tools.ReplaceInstanceUUIDInPlace(path, newID))

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, after, len(before), "in-place patch must not change file size")

	// Exactly the bytes that differ should be the 36-byte UUID span.
	diffCount := 0

	for i := range before {
		if before[i] != after[i] {
			diffCount++
		}
	}

	assert.LessOrEqual(t, diffCount, len(newID.String()), "only the UUID span may differ")

	dm, err := reader.ReadHeader(path)
	require.NoError(t, err)
	assert.Equal(t, newID, dm.InstanceUUID, "patched InstanceUUID")
	assert.Equal(t, origMeta.Version, dm.Version, "Version unchanged")
	assert.Equal(t, origMeta.FormatVer, dm.FormatVer, "FormatVer unchanged")
	assert.Equal(t, origMeta.VClock.String(), dm.VClock.String(), "VClock unchanged")

	// Rows still decode end-to-end.
	r, err := reader.Open(path)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	assert.Positive(t, countRowsReader(t, r), "rows still readable after in-place patch")
}

// assertTailEqual checks that the post-header bytes (tx blocks + EOF marker)
// of src and dst are byte-identical.
func assertTailEqual(t *testing.T, srcPath, dstPath string) {
	t.Helper()

	srcBytes, err := os.ReadFile(srcPath)
	require.NoError(t, err)
	dstBytes, err := os.ReadFile(dstPath)
	require.NoError(t, err)

	srcTail := srcBytes[metaEndOffset(t, srcPath):]
	dstTail := dstBytes[metaEndOffset(t, dstPath):]
	assert.Equal(t, srcTail, dstTail, "tail (tx blocks + EOF) must be preserved verbatim")
}

// copyToTemp copies src into a fresh temp file and returns its path, so
// in-place mutation never touches the shared testdata fixture.
func copyToTemp(t *testing.T, src string) string {
	t.Helper()

	data, err := os.ReadFile(src)
	require.NoError(t, err)

	dst := filepath.Join(t.TempDir(), filepath.Base(src))
	require.NoError(t, os.WriteFile(dst, data, 0o600))

	return dst
}

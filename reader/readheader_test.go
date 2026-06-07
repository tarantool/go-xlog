package reader_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/reader"
)

// TestReadHeader returns the same header Open+Meta would, and the result is an
// independent copy the caller may mutate.
func TestReadHeader(t *testing.T) {
	t.Parallel()

	path := testutil.Path(t, "populated.snap")

	want, err := reader.Open(path)
	require.NoError(t, err)

	wantMeta := want.Meta()

	got, err := reader.ReadHeader(path)
	require.NoError(t, err)

	assert.Equal(t, wantMeta.Filetype, got.Filetype)
	assert.Equal(t, wantMeta.FormatVer, got.FormatVer)
	assert.Equal(t, wantMeta.Version, got.Version)
	assert.Equal(t, wantMeta.InstanceUUID, got.InstanceUUID)
	assert.Equal(t, wantMeta.VClock.String(), got.VClock.String())

	_ = want.Close()

	// The returned meta is a clone: mutating it does not panic or alias the
	// reader's now-closed state, and a second read is unaffected.
	got.Version = "mutated"

	again, err := reader.ReadHeader(path)
	require.NoError(t, err)
	assert.NotEqual(t, "mutated", again.Version, "ReadHeader must return an independent copy")
}

// TestReadHeader_Error surfaces open/decode failures.
func TestReadHeader_Error(t *testing.T) {
	t.Parallel()

	_, err := reader.ReadHeader(filepath.Join(t.TempDir(), "does-not-exist.xlog"))
	require.Error(t, err)
}

// TestReadHeaderFS reads through an fs.FS (os.DirFS over testdata).
func TestReadHeaderFS(t *testing.T) {
	t.Parallel()

	path := testutil.Path(t, "simple.xlog")
	fsys := os.DirFS(filepath.Dir(path))

	m, err := reader.ReadHeaderFS(fsys, filepath.Base(path))
	require.NoError(t, err)
	assert.Equal(t, "0.13", m.FormatVer)
}

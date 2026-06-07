package dir_test

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/dir"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/testutil"
)

// mapFSFromDir loads every file with the given extension from a real directory
// into an in-memory fstest.MapFS rooted at "journal/".
func mapFSFromDir(t *testing.T, realDir, ext string) fstest.MapFS {
	t.Helper()

	fsys := fstest.MapFS{}
	ents, err := os.ReadDir(realDir)
	require.NoError(t, err)

	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ext {
			continue
		}

		b, err := os.ReadFile(filepath.Join(realDir, e.Name()))
		require.NoError(t, err)

		fsys["journal/"+e.Name()] = &fstest.MapFile{Data: b}
	}

	return fsys
}

// TestOpenDirFS_MatchesOpenDir asserts OpenDirFS over an in-memory MapFS indexes
// the same chain (signatures + vclocks) as OpenDir over the on-disk directory.
// Uses the 2.11 historical dir, which has a >=2 file xlog chain.
func TestOpenDirFS_MatchesOpenDir(t *testing.T) {
	t.Parallel()

	realDir := testutil.Path(t, filepath.Join("historical", "2.11"))

	onDisk, err := dir.OpenDir(realDir, format.FiletypeXLOG)
	require.NoError(t, err)
	fsys := mapFSFromDir(t, realDir, ".xlog")
	inFS, err := dir.OpenDirFS(fsys, "journal", format.FiletypeXLOG)
	require.NoError(t, err)

	df, ff := onDisk.Files(), inFS.Files()
	require.Len(t, ff, len(df))
	require.GreaterOrEqual(t, len(ff), 2)

	for i := range df {
		assert.Equal(t, df[i].Signature, ff[i].Signature, "entry[%d] signature", i)

		if ord, ok := ff[i].VClock.Compare(df[i].VClock); !ok || ord != 0 {
			t.Errorf("entry[%d] vclock: disk=%s, fs=%s", i, df[i].VClock, ff[i].VClock)
		}
	}
}

// TestOpenDirFS_SignatureMismatch confirms the filename-signature check fires through the FS
// path: a file whose name signature disagrees with its meta vclock is rejected.
func TestOpenDirFS_SignatureMismatch(t *testing.T) {
	t.Parallel()

	realDir := testutil.Path(t, filepath.Join("historical", "2.11"))
	// The bootstrap xlog has vclock {} → signature 0; place it under a name
	// claiming signature 99.
	raw, err := os.ReadFile(filepath.Join(realDir, "00000000000000000000.xlog"))
	require.NoError(t, err)

	fsys := fstest.MapFS{"00000000000000000099.xlog": {Data: raw}}

	_, err = dir.OpenDirFS(fsys, ".", format.FiletypeXLOG)
	require.ErrorIs(t, err, dir.ErrSignatureMismatch)
}

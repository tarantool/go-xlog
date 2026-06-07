package reader_test

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/reader"
)

// TestOpenFS_MatchesOpen asserts OpenFS over an in-memory fstest.MapFS decodes
// the same meta and row stream as Open over the real file.
func TestOpenFS_MatchesOpen(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"simple.xlog", "populated.snap", "vylog_sample.vylog"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			raw := testutil.Load(t, name)
			fsys := fstest.MapFS{name: {Data: raw}}

			ondisk, err := reader.Open(testutil.Path(t, name))
			require.NoError(t, err)

			defer func() { _ = ondisk.Close() }()

			infs, err := reader.OpenFS(fsys, name)
			require.NoError(t, err)

			defer func() { _ = infs.Close() }()

			if infs.Meta().Filetype != ondisk.Meta().Filetype ||
				infs.Meta().FormatVer != ondisk.Meta().FormatVer ||
				infs.Meta().InstanceUUID != ondisk.Meta().InstanceUUID {
				t.Errorf("meta mismatch: fs=%+v disk=%+v", infs.Meta(), ondisk.Meta())
			}

			fsRows, diskRows := drain(t, infs), drain(t, ondisk)
			assert.Equal(t, diskRows, fsRows, "row count: fs=%d, disk=%d", fsRows, diskRows)
		})
	}
}

// TestOpenFS_Missing surfaces a clean error for an absent file.
func TestOpenFS_Missing(t *testing.T) {
	t.Parallel()

	_, err := reader.OpenFS(fstest.MapFS{}, "nope.xlog")
	require.Error(t, err, "OpenFS on missing file = nil error")
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func drain(t *testing.T, r *reader.Reader) int {
	t.Helper()

	n := 0

	for _, err := range r.Rows() {
		if err != nil {
			t.Fatalf("Rows: %v", err)
		}

		n++
	}

	return n
}

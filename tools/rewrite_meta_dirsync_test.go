package tools_test

import (
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/tools"
)

// Reproduction for the missing directory fsync after rename in RewriteMeta: a
// successful rewrite must fsync the destination directory so the
// `.inprogress` → final rename is durable. Before the fix RewriteMeta synced
// the file but never the directory, and this assertion failed (no recorded
// call). The spy still delegates to the real sync so durability holds.
//
//nolint:paralleltest // mutates the package-level syncDir seam; must not run in parallel.
func TestRewriteMetaFsyncsDestDir(t *testing.T) {
	srcPath := testutil.Path(t, "simple.xlog")
	dstDir := t.TempDir()
	dstPath := filepath.Join(dstDir, "rewritten.xlog")

	var calls []string

	restore := tools.SetSyncDirForTest(func(dir string) error {
		calls = append(calls, dir)

		return nil
	})
	defer restore()

	newID := uuid.MustParse("99999999-aaaa-bbbb-cccc-dddddddddddd")
	require.NoError(t, tools.RewriteMeta(srcPath, dstPath, tools.ReplaceInstanceUUID(newID)))

	require.Equal(t, []string{dstDir}, calls,
		"RewriteMeta must fsync exactly the destination directory after rename")
}

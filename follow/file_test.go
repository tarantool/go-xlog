package follow_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/follow"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/writer"
)

func TestFile_AppendThenRead(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "live.xlog")
	lw := newLiveWriter(t, path)

	c := collectRows(follow.File(t.Context(), path, fastPoll()))

	lw.write(t, 1)
	c.waitCount(t, 1)

	lw.write(t, 2)
	c.waitCount(t, 2)

	lw.finalize(t) // EOF marker → the follower ends cleanly

	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, lsns)
}

func TestFile_Finalization(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "live.xlog")
	lw := newLiveWriter(t, path)
	lw.write(t, 1)
	lw.write(t, 2)
	lw.write(t, 3)
	lw.finalize(t)

	c := collectRows(follow.File(t.Context(), path, fastPoll()))
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2, 3}, lsns)
}

func TestFile_Cancellation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "live.xlog")
	lw := newLiveWriter(t, path)
	lw.write(t, 1)

	ctx, cancel := context.WithCancel(context.Background())

	c := collectRows(follow.File(ctx, path, fastPoll()))
	c.waitCount(t, 1) // got row 1; file has no EOF marker, so the follower is now waiting

	cancel()
	c.waitDone(t) // must return promptly on cancellation

	lsns, err := c.snapshot()
	require.NoError(t, err) // cancellation is a clean stop, no error yielded
	require.Equal(t, []int64{1}, lsns)
}

func TestFile_Resume(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "live.xlog")
	lw := newLiveWriter(t, path)
	lw.write(t, 1)
	lw.write(t, 2)
	lw.write(t, 3)
	lw.finalize(t)

	// First pass: read rows, capturing the resume offset via a Follower.
	f := follow.NewFileFollower(path, fastPoll())

	ctx := context.Background()

	r1, err := f.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), r1.LSN)

	r2, err := f.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), r2.LSN)

	off := f.Offset()
	require.NoError(t, f.Close())

	// Second pass: resume from the captured offset; expect rows [2..3]
	// (at-least-once — row 2 is re-read because off is the start of its block).
	c := collectRows(follow.File(ctx, path, fastPoll(), follow.WithStartOffset(off)))
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{2, 3}, lsns)
}

func TestFile_PartialThenComplete(t *testing.T) {
	t.Parallel()

	meta, blocks := buildBlocks(t, []int64{1, 2})
	require.Len(t, blocks, 2)

	path := filepath.Join(t.TempDir(), "live.xlog")

	// Write the header + block 1 + a torn prefix of block 2.
	torn := blocks[1][:len(blocks[1])-3]
	require.NoError(t, os.WriteFile(path, concat(meta, blocks[0], torn), 0o600))

	c := collectRows(follow.File(t.Context(), path, fastPoll()))
	c.waitCount(t, 1) // row 1 arrives; the torn tail of block 2 makes the follower wait

	// The follower must not have emitted a second row from the partial block.
	require.Equal(t, 1, c.count())

	// Complete block 2 and finalise.
	appendBytes(t, path, blocks[1][len(blocks[1])-3:])
	appendBytes(t, path, format.EOFMarker[:])

	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, lsns)
}

// buildBlocks returns the meta header bytes and the per-tx block bytes of an
// xlog containing one single-row tx per lsn (no EOF marker).
func buildBlocks(t *testing.T, lsns []int64) ([]byte, [][]byte) {
	t.Helper()

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, metaWith(format.VClock{1: 0}, nil))
	require.NoError(t, err)
	require.NoError(t, w.Sync()) // flush meta

	meta := append([]byte(nil), buf.Bytes()...)
	buf.Reset()

	blocks := make([][]byte, 0, len(lsns))

	for _, lsn := range lsns {
		require.NoError(t, w.WriteTx([]format.XRow{row(lsn)}))
		require.NoError(t, w.Sync())

		blocks = append(blocks, append([]byte(nil), buf.Bytes()...))
		buf.Reset()
	}

	return meta, blocks
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}

	return out
}

func appendBytes(t *testing.T, path string, b []byte) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)

	_, err = f.Write(b)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

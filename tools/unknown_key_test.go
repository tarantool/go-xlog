package tools_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/tools"
	"github.com/tarantool/go-xlog/writer"
)

// TestRewriteMetaFields_PreservesUnknownBodyKey - rewriting the header of a
// file keeps a row body key no decoder has a field for: the tx blocks are
// copied verbatim, never decoded and encoded again.
func TestRewriteMetaFields_PreservesUnknownBodyKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src.xlog")
	dst := filepath.Join(dir, "dst.xlog")

	// {SPACE_ID: 512, 0x7e: 5}
	body := []byte{0x82, byte(iproto.IPROTO_SPACE_ID), 0xcd, 0x02, 0x00, 0x7e, 0x05}

	w, err := writer.Create(src, validMeta())
	require.NoError(t, err)
	require.NoError(t, w.WriteTx([]format.XRow{{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: body}}))
	require.NoError(t, w.Close())

	require.NoError(t, tools.RewriteMetaFields(src, dst, tools.WithVersion("rewritten")))

	r, err := reader.Open(dst)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	row, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, body, row.BodyRaw)

	var unknown map[uint64][]byte

	var got format.DMLBody

	require.NoError(t, format.DecodeDMLBodyInto(row.BodyRaw, &got, format.CollectUnknown(&unknown)))
	assert.Equal(t, uint32(512), got.SpaceID)
	assert.Equal(t, map[uint64][]byte{0x7e: {0x05}}, unknown)
}

package pipe_test

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/pipe"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// unknownKeyBody is a DML body with two keys DMLBody has no field for:
// IPROTO_TUPLE_META, which Tarantool writes, and 0x7e, which it does not
// (yet).
func unknownKeyBody() []byte {
	var b []byte

	b = appendMPMapHeader(b, 4)
	b = appendMPUint(b, uint64(iproto.IPROTO_SPACE_ID))
	b = appendMPUint(b, 512)
	b = appendMPUint(b, uint64(iproto.IPROTO_TUPLE))
	b = appendMPArrayHeader(b, 1)
	b = appendMPUint(b, 1)
	b = appendMPUint(b, uint64(iproto.IPROTO_TUPLE_META))
	b = appendMPMapHeader(b, 1)
	b = appendMPUint(b, 1)
	b = appendMPUint(b, 1)
	b = appendMPUint(b, 0x7e)
	b = appendMPUint(b, 5)

	return b
}

// TestCopy_PreservesUnknownBodyKey - Copy is a rewrite path whose filter
// decodes the body, and the body decoders skip unknown keys by default. The
// copy must still carry them: Copy writes the BodyRaw it read, never a body
// re-encoded from a DMLBody.
func TestCopy_PreservesUnknownBodyKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src.xlog")
	dst := filepath.Join(dir, "dst.xlog")
	body := unknownKeyBody()

	w, err := writer.Create(src, newMeta())
	require.NoError(t, err)
	require.NoError(t, w.WriteTx([]format.XRow{{Type: iproto.IPROTO_INSERT, ReplicaID: 1, LSN: 1, BodyRaw: body}}))
	require.NoError(t, w.Close())

	// A body-aware filter, decoding with no options: it sees no unknown key.
	inSpace := func(r format.XRow) bool {
		b, err := format.DecodeDMLBody(r.BodyRaw)

		return err == nil && b.SpaceID == 512
	}

	r, w := openSrcDst(t, src, dst)

	defer func() { _ = r.Close() }()

	n, err := pipe.Copy(r, w, inSpace)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.Equal(t, int64(1), n)

	out, err := reader.Open(dst)
	require.NoError(t, err)

	defer func() { _ = out.Close() }()

	row, err := out.Next()
	require.NoError(t, err)
	assert.Equal(t, body, row.BodyRaw, "the copied body must be byte-identical")

	var unknown map[uint64][]byte

	var got format.DMLBody

	require.NoError(t, format.DecodeDMLBodyInto(row.BodyRaw, &got, format.CollectUnknown(&unknown)))
	assert.Equal(t, uint32(512), got.SpaceID)
	assert.Equal(t, map[uint64][]byte{
		uint64(iproto.IPROTO_TUPLE_META): {0x81, 0x01, 0x01},
		0x7e:                             {0x05},
	}, unknown, "the unknown body keys survive the copy")

	_, err = out.Next()
	require.ErrorIs(t, err, io.EOF, "one row")
}

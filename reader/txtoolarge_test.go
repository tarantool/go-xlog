package reader_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// TestReadTxBlock_TooLargeLength guards the fixheader length field: a fixheader declaring a tx
// payload above MaxTxPayloadLen must error before allocating, not attempt a
// multi-GiB make. We hand a valid meta + a fixheader claiming a 4 GiB payload
// over a stream that holds none of it.
func TestReadTxBlock_TooLargeLength(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, format.EncodeMeta(&buf, &format.Meta{
		Filetype: format.FiletypeXLOG,
		VClock:   format.VClock{1: 0},
	}))

	var fh [format.FixheaderSize]byte
	format.EncodeFixheader(&fh, &format.Fixheader{
		Magic: format.RowMarker,
		Len:   0xFFFFFFFF, // ~4 GiB claimed, zero bytes provided.
	})
	buf.Write(fh[:])

	r, err := reader.NewReader(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	_, err = r.Next()
	require.ErrorIs(t, err, reader.ErrTxTooLarge)
}

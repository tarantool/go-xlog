package writer //nolint:testpackage // white-box: exercises the writer with internal helpers

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

// frameBlock builds a valid framed tx block (fixheader + payload) with a correct
// CRC32C over the given payload and magic.
func frameBlock(magic [4]byte, payload []byte) []byte {
	var fhBuf [format.FixheaderSize]byte

	fh := format.Fixheader{
		Magic:  magic,
		Len:    uint32(len(payload)),
		CRC32C: format.CRC32C(payload),
	}
	format.EncodeFixheader(&fhBuf, &fh)

	block := make([]byte, 0, format.FixheaderSize+len(payload))
	block = append(block, fhBuf[:]...)
	block = append(block, payload...)

	return block
}

// TestWriteRawBlock_RoundTrip — a verbatim block written via WriteRawBlock reads
// back as the same on-disk bytes.
func TestWriteRawBlock_RoundTrip(t *testing.T) {
	t.Parallel()

	payload, err := format.AppendTxBlockPayload(nil, []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: encodeDMLBody([]uint64{1, 42})},
	})
	require.NoError(t, err)

	block := frameBlock(format.RowMarker, payload)

	var buf bytes.Buffer

	w, err := NewWriter(&buf, benchMetaForRaw())
	require.NoError(t, err)
	require.NoError(t, w.WriteRawBlock(block))
	require.NoError(t, w.Close())

	// The block bytes must appear verbatim in the output (after the meta header,
	// before the EOF marker).
	assert.True(t, bytes.Contains(buf.Bytes(), block), "raw block written verbatim")
}

// TestWriteRawBlock_Guards — short / wrong-magic / closed / pending-row rejects.
func TestWriteRawBlock_Guards(t *testing.T) {
	t.Parallel()

	newW := func() (*Writer, *bytes.Buffer) {
		var buf bytes.Buffer

		w, err := NewWriter(&buf, benchMetaForRaw())
		require.NoError(t, err)

		return w, &buf
	}

	t.Run("too short", func(t *testing.T) {
		t.Parallel()

		w, _ := newW()
		err := w.WriteRawBlock(make([]byte, format.FixheaderSize-1))
		assert.ErrorIs(t, err, ErrInvalidRawBlock)
	})

	t.Run("bad magic", func(t *testing.T) {
		t.Parallel()

		w, _ := newW()
		bad := make([]byte, format.FixheaderSize)
		copy(bad, format.EOFMarker[:]) // Not a Row/ZRow block.
		err := w.WriteRawBlock(bad)
		assert.ErrorIs(t, err, ErrInvalidRawBlock)
	})

	t.Run("closed", func(t *testing.T) {
		t.Parallel()

		w, _ := newW()
		require.NoError(t, w.Close())
		err := w.WriteRawBlock(frameBlock(format.RowMarker, []byte{0x90}))
		assert.ErrorIs(t, err, ErrClosed)
	})

	t.Run("pending write row", func(t *testing.T) {
		t.Parallel()

		w, _ := newW()
		require.NoError(t, w.WriteRow(format.XRow{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: encodeDMLBody([]uint64{1})}))
		err := w.WriteRawBlock(frameBlock(format.RowMarker, []byte{0x90}))
		assert.ErrorIs(t, err, ErrPendingWriteRow)
	})
}

func benchMetaForRaw() *format.Meta {
	return &format.Meta{Filetype: format.FiletypeXLOG, Version: "go-xlog/test", VClock: format.VClock{1: 0}}
}

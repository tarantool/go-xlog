package format_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/tarantool/go-xlog/format"
)

func TestTxBlock_RoundTrip_Uncompressed(t *testing.T) {
	t.Parallel()

	rows := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, TSN: 1, Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: []byte{0x80}},
		{Type: iproto.IPROTO_INSERT, LSN: 2, TSN: 2, Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: []byte{0x80}},
	}
	blob, err := format.EncodeTxBlock(rows, format.TxOptions{Compression: format.Compression{Disabled: true}})
	require.NoError(t, err, "encode")
	slices, plain, n, err := format.DecodeTxBlock(blob)
	require.NoError(t, err, "decode")
	assert.Equalf(t, len(blob), n, "consumed %d, want %d", n, len(blob))
	require.Lenf(t, slices, 2, "expected 2 row slices, got %d (plain=%x)", len(slices), plain)

	for i, s := range slices {
		got, _, err := format.DecodeXRow(s)
		require.NoErrorf(t, err, "row %d", i)
		assert.Truef(t, got.LSN == rows[i].LSN && got.Type == rows[i].Type, "row %d mismatch: %+v vs %+v", i, got, rows[i])
	}
}

func TestTxBlock_RoundTrip_Compressed(t *testing.T) {
	t.Parallel()

	// Force compression: row body is 4 KiB to easily exceed CompressThreshold.
	big := make([]byte, 4096)
	for i := range big {
		big[i] = byte(i)
	}
	// Wrap big as a msgpack bin32 (msgpcode.Bin32 = 0xc6 + 4 byte len + payload).
	body := append([]byte{0xc6, 0x00, 0x00, 0x10, 0x00}, big...)
	rows := []format.XRow{{Type: iproto.IPROTO_INSERT, LSN: 1, TSN: 1, Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: wrapMap(body)}}

	blob, err := format.EncodeTxBlock(rows, format.TxOptions{})
	require.NoError(t, err, "encode")
	// Confirm we ended up with a ZRowMarker tx block.
	require.Truef(t, blob[0] == format.ZRowMarker[0] && blob[1] == format.ZRowMarker[1] && blob[2] == format.ZRowMarker[2] && blob[3] == format.ZRowMarker[3], "expected ZRowMarker, got %x", blob[:4])
	slices, _, _, err := format.DecodeTxBlock(blob)
	require.NoError(t, err, "decode")
	require.Lenf(t, slices, 1, "expected 1 row, got %d", len(slices))
}

func TestTxBlock_CRCMismatch(t *testing.T) {
	t.Parallel()

	rows := []format.XRow{{Type: iproto.IPROTO_INSERT, LSN: 1, TSN: 1, Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: []byte{0x80}}}
	blob, err := format.EncodeTxBlock(rows, format.TxOptions{Compression: format.Compression{Disabled: true}})
	require.NoError(t, err)
	// Flip a byte inside the payload (past the 19-byte fixheader).
	blob[format.FixheaderSize] ^= 0xff
	slices, plain, n, decErr := format.DecodeTxBlock(blob)
	require.ErrorIs(t, decErr, format.ErrCorruptCRC)
	require.Nil(t, slices)
	require.Nil(t, plain)
	require.Zero(t, n)
}

// wrapMap wraps a single-key body in a {KeyTuple: <payload>} map so the row
// has well-formed body shape. We use map16/fixmap+fixint+payload.
func wrapMap(payload []byte) []byte {
	// Fixmap(1) | fixint(KeyTuple=0x21) | payload(...)
	out := make([]byte, 0, 2+len(payload))
	out = append(out, 0x81, byte(iproto.IPROTO_TUPLE))
	out = append(out, payload...)

	return out
}

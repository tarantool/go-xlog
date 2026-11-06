package format_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

// TestDecodeXRowInto_MatchesDecodeXRow — the zero-alloc core must produce the
// same row and byte count as the allocating wrapper.
func TestDecodeXRowInto_MatchesDecodeXRow(t *testing.T) {
	t.Parallel()

	src := &format.XRow{Type: iproto.IPROTO_INSERT, LSN: 7, TSN: 7, Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: benchBody(40)}

	enc, err := format.EncodeXRow(nil, src)
	require.NoError(t, err)

	want, wantN, err := format.DecodeXRow(enc)
	require.NoError(t, err)

	var got format.XRow

	gotN, err := format.DecodeXRowInto(enc, &got)
	require.NoError(t, err)

	assert.Equal(t, wantN, gotN, "consumed bytes")
	assert.Equal(t, *want, got, "decoded row")
}

// TestDecodeXRowInto_NoFieldBleed — reusing one dst across rows with different
// shapes must not leak fields (e.g. a prior StreamID/Flags) into a later row
// that omits them on the wire.
func TestDecodeXRowInto_NoFieldBleed(t *testing.T) {
	t.Parallel()

	// Row A: rich — replica, group, stream, sync, schema version, extra
	// flags, body.
	rowA := &format.XRow{
		Type: iproto.IPROTO_INSERT, ReplicaID: 3, GroupID: 1, LSN: 10, TSN: 10,
		Flags: iproto.IPROTO_FLAG_COMMIT | 0x02, StreamID: 99, Sync: 5, SchemaVersion: 6,
		BodyRaw: benchBody(8),
	}
	// Row B: sparse single-stmt insert — none of A's optional fields.
	rowB := &format.XRow{Type: iproto.IPROTO_INSERT, LSN: 11, TSN: 11, Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: benchBody(8)}

	encA, err := format.EncodeXRow(nil, rowA)
	require.NoError(t, err)

	encB, err := format.EncodeXRow(nil, rowB)
	require.NoError(t, err)

	var dst format.XRow

	_, err = format.DecodeXRowInto(encA, &dst)
	require.NoError(t, err)
	require.NotZero(t, dst.StreamID, "row A should set StreamID")
	require.NotZero(t, dst.Sync, "row A should set Sync")
	require.NotZero(t, dst.SchemaVersion, "row A should set SchemaVersion")
	require.True(t, dst.HasStreamID(), "row A should carry IPROTO_STREAM_ID")

	// Decode B into the SAME dst — its zero fields must come back zero.
	_, err = format.DecodeXRowInto(encB, &dst)
	require.NoError(t, err)

	wantB, _, err := format.DecodeXRow(encB)
	require.NoError(t, err)
	assert.Equal(t, *wantB, dst, "row B decoded into reused dst must equal a fresh decode")
	assert.Zero(t, dst.StreamID, "StreamID must not bleed from row A")
	assert.Zero(t, dst.ReplicaID, "ReplicaID must not bleed from row A")
	assert.Zero(t, dst.GroupID, "GroupID must not bleed from row A")
	assert.Zero(t, dst.Sync, "Sync must not bleed from row A")
	assert.Zero(t, dst.SchemaVersion, "SchemaVersion must not bleed from row A")
	assert.Equal(t, format.PresentType|format.PresentLSN, dst.Present, "Present must not bleed from row A")
}

// TestXRow_ZeroAlloc is not parallel: AllocsPerRun counts the mallocs of the
// whole process. A row with every header key fills the encoder's whole fixed
// key table, and neither direction allocates.
func TestXRow_ZeroAlloc(t *testing.T) { //nolint:paralleltest // AllocsPerRun counts process-wide mallocs
	row := benchFullRow()
	buf := make([]byte, 0, 256)

	encAllocs := testing.AllocsPerRun(100, func() {
		buf, _ = format.EncodeXRow(buf[:0], row)
	})
	assert.Zero(t, encAllocs, "EncodeXRow allocs")

	enc := append([]byte(nil), buf...)

	var dst format.XRow

	decAllocs := testing.AllocsPerRun(100, func() {
		_, _ = format.DecodeXRowInto(enc, &dst)
	})
	assert.Zero(t, decAllocs, "DecodeXRowInto allocs")
	assert.Equal(t, allPresent, dst.Present, "every key is on the wire")

	dst.Present = 0

	assert.Equal(t, *row, dst, "full row round-trips")
}

// TestAppendTxBlock_MatchesEncodeTxBlock — byte-for-byte identical output, and
// AppendTxBlock must honour a non-empty dst prefix.
func TestAppendTxBlock_MatchesEncodeTxBlock(t *testing.T) {
	t.Parallel()

	rows := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, TSN: 1, Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: benchBody(20)},
		{Type: iproto.IPROTO_INSERT, LSN: 2, TSN: 2, Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: benchBody(20)},
	}

	for _, noComp := range []bool{true, false} {
		opts := format.TxOptions{Compression: format.Compression{Disabled: noComp}}

		want, err := format.EncodeTxBlock(rows, opts)
		require.NoError(t, err)

		got, err := format.AppendTxBlock(nil, rows, opts)
		require.NoError(t, err)
		assert.Equal(t, want, got, "AppendTxBlock(nil) noComp=%v", noComp)

		prefix := []byte("PREFIX")
		withPrefix, err := format.AppendTxBlock(append([]byte(nil), prefix...), rows, opts)
		require.NoError(t, err)
		assert.Equal(t, append(prefix, want...), withPrefix, "AppendTxBlock must preserve dst prefix noComp=%v", noComp)
	}
}

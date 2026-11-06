package format_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/tarantool/go-xlog/format"
)

// TestXRow_SingleStmt_OmitsTSNandCommit — encoder must NOT emit
// KeyTSN (0x08) when TSN==LSN && IsCommit; the FlagCommit bit must also
// be cleared from any emitted KeyFlags (and KeyFlags itself omitted when
// the residual flags byte is zero).
func TestXRow_SingleStmt_OmitsTSNandCommit(t *testing.T) {
	t.Parallel()

	r := &format.XRow{
		Type:  iproto.IPROTO_INSERT,
		LSN:   5,
		TSN:   5,
		Flags: iproto.IPROTO_FLAG_COMMIT,
		// Empty body, fine for the test — we're only checking header shape.
		BodyRaw: []byte{0x80}, // Empty msgpack map.
	}
	buf, err := format.EncodeXRow(nil, r)
	require.NoError(t, err, "EncodeXRow")
	assert.Falsef(t, bytes.Contains(buf, []byte{byte(iproto.IPROTO_TSN)}), "single-stmt row encoded KeyTSN; bytes=%x", buf)
	assert.Falsef(t, bytes.Contains(buf, []byte{byte(iproto.IPROTO_FLAGS)}), "single-stmt row encoded KeyFlags; bytes=%x", buf)
	// Round-trip — decoder must infer TSN=LSN, IsCommit=true.
	got, _, err := format.DecodeXRow(buf)
	require.NoError(t, err, "DecodeXRow")
	assert.Truef(t, got.TSN == 5 && got.IsCommit(), "inferred wrong: TSN=%d IsCommit=%v, want 5/true", got.TSN, got.IsCommit())
}

// TestXRow_MultiStmt_TSNDiffs_OnlyLastCommits — a 3-row tx with LSNs
// 10,11,12 and TSN=10 must encode KeyTSN diffs 0,1,2; only the last row has
// the COMMIT bit; the first two rows omit KeyFlags (no flags to encode).
func TestXRow_MultiStmt_TSNDiffs_OnlyLastCommits(t *testing.T) {
	t.Parallel()

	rows := []*format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 10, TSN: 10, Flags: 0, BodyRaw: []byte{0x80}},
		{Type: iproto.IPROTO_INSERT, LSN: 11, TSN: 10, Flags: 0, BodyRaw: []byte{0x80}},
		{Type: iproto.IPROTO_INSERT, LSN: 12, TSN: 10, Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: []byte{0x80}},
	}
	wantDiffs := []int64{0, 1, 2}

	for i, r := range rows {
		buf, err := format.EncodeXRow(nil, r)
		require.NoErrorf(t, err, "row %d", i)
		got, _, err := format.DecodeXRow(buf)
		require.NoErrorf(t, err, "row %d decode", i)
		assert.Truef(t, got.TSN == 10 && got.LSN == r.LSN, "row %d: TSN=%d LSN=%d, want 10/%d", i, got.TSN, got.LSN, r.LSN)
		// LSN-TSN diff is what's on the wire.
		diff := got.LSN - got.TSN
		assert.Equalf(t, wantDiffs[i], diff, "row %d: diff=%d want %d", i, diff, wantDiffs[i])
		// Only the last row has IsCommit set after decoding.
		wantCommit := i == len(rows)-1
		assert.Equalf(t, wantCommit, got.IsCommit(), "row %d: IsCommit=%v want %v (bytes=%x)", i, got.IsCommit(), wantCommit, buf)
	}
}

// TestXRow_NOP_NoBody — NOP rows produce nil BodyRaw.
func TestXRow_NOP_NoBody(t *testing.T) {
	t.Parallel()

	r := &format.XRow{Type: iproto.IPROTO_NOP, LSN: 100, TSN: 100, Flags: iproto.IPROTO_FLAG_COMMIT}
	buf, err := format.EncodeXRow(nil, r)
	require.NoError(t, err, "encode")
	got, n, err := format.DecodeXRow(buf)
	require.NoError(t, err, "decode")
	assert.Nilf(t, got.BodyRaw, "NOP body should be nil, got %x", got.BodyRaw)
	assert.Equalf(t, len(buf), n, "consumed %d, want %d", n, len(buf))
}

// TestXRow_WaitSync_OnSingleStmt — when a non-commit flag is set on a
// single-stmt tx, KeyFlags is still emitted (FlagCommit is omitted).
func TestXRow_WaitSync_OnSingleStmt(t *testing.T) {
	t.Parallel()

	r := &format.XRow{Type: iproto.IPROTO_INSERT, LSN: 7, TSN: 7, Flags: iproto.IPROTO_FLAG_COMMIT | iproto.IPROTO_FLAG_WAIT_SYNC, BodyRaw: []byte{0x80}}
	buf, err := format.EncodeXRow(nil, r)
	require.NoError(t, err, "encode")
	// KeyFlags should appear and the encoded value should be FlagWaitSync only.
	got, _, err := format.DecodeXRow(buf)
	require.NoError(t, err, "decode")
	// Decoder reads the on-wire flags (no FlagCommit) and infers FlagCommit
	// from KeyTSN absence.
	assert.NotZerof(t, got.Flags&iproto.IPROTO_FLAG_WAIT_SYNC, "WaitSync flag lost: got 0x%02x", got.Flags)
	assert.True(t, got.IsCommit(), "expected inferred IsCommit on single-stmt tx with WaitSync")
}

// TestXRow_TimestampRoundTrip — IEEE 754 round-trip.
func TestXRow_TimestampRoundTrip(t *testing.T) {
	t.Parallel()

	r := &format.XRow{Type: iproto.IPROTO_INSERT, LSN: 1, TSN: 1, Timestamp: 1234567.89, BodyRaw: []byte{0x80}}
	buf, err := format.EncodeXRow(nil, r)
	require.NoError(t, err)
	got, _, err := format.DecodeXRow(buf)
	require.NoError(t, err)
	assert.InDeltaf(t, r.Timestamp, got.Timestamp, 1e-9, "Timestamp: got %v want %v", got.Timestamp, r.Timestamp)
}

// TestEncodeXRow_KeyOrder — every header key in the order xrow_header_encode
// writes them (src/box/xrow.c:338-452), with IPROTO_SCHEMA_VERSION right after
// IPROTO_SYNC as in iproto_header_encode, and the row decodes back whole.
func TestEncodeXRow_KeyOrder(t *testing.T) {
	t.Parallel()

	r := &format.XRow{
		Type: iproto.IPROTO_INSERT, Sync: 5, SchemaVersion: 6, ReplicaID: 3, GroupID: 1,
		LSN: 11, TSN: 10, Timestamp: 1.5, StreamID: 7,
		Flags: iproto.IPROTO_FLAG_COMMIT | iproto.IPROTO_FLAG_WAIT_SYNC, BodyRaw: []byte{0x80},
	}

	want := []byte{
		0x8a,       // fixmap, 10 entries
		0x00, 0x02, // IPROTO_REQUEST_TYPE: IPROTO_INSERT
		0x01, 0x05, // IPROTO_SYNC
		0x05, 0x06, // IPROTO_SCHEMA_VERSION
		0x02, 0x03, // IPROTO_REPLICA_ID
		0x07, 0x01, // IPROTO_GROUP_ID
		0x03, 0x0b, // IPROTO_LSN
		0x04, 0xcb, 0x3f, 0xf8, 0, 0, 0, 0, 0, 0, // IPROTO_TIMESTAMP: float64 1.5
		0x08, 0x01, // IPROTO_TSN: LSN - TSN
		0x0a, 0x07, // IPROTO_STREAM_ID
		0x09, 0x03, // IPROTO_FLAGS: COMMIT (last row of a multi-row tx) | WAIT_SYNC
		0x80, // body
	}

	buf, err := format.EncodeXRow(nil, r)
	require.NoError(t, err)
	assert.Equal(t, want, buf)

	got, n, err := format.DecodeXRow(buf)
	require.NoError(t, err)
	assert.Equal(t, len(buf), n)

	wantRow := *r
	wantRow.Present = allPresent

	assert.Equal(t, wantRow, *got)
}

// TestXRow_SyncSchemaVersion_OmittedWhenZero — like every optional key,
// IPROTO_SYNC and IPROTO_SCHEMA_VERSION are emitted iff non-zero.
func TestXRow_SyncSchemaVersion_OmittedWhenZero(t *testing.T) {
	t.Parallel()

	r := &format.XRow{Type: iproto.IPROTO_INSERT, BodyRaw: []byte{0x80}}

	buf, err := format.EncodeXRow(nil, r)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x81, 0x00, 0x02, 0x80}, buf, "only IPROTO_REQUEST_TYPE")

	r.Sync = 1 << 40
	r.SchemaVersion = 300

	buf, err = format.EncodeXRow(nil, r)
	require.NoError(t, err)

	got, _, err := format.DecodeXRow(buf)
	require.NoError(t, err)
	assert.Equal(t, uint64(1<<40), got.Sync)
	assert.Equal(t, uint64(300), got.SchemaVersion)
}

// allPresent is every key DecodeXRowInto records in XRow.Present.
const allPresent = format.PresentType | format.PresentSync | format.PresentReplicaID |
	format.PresentLSN | format.PresentTimestamp | format.PresentSchemaVersion |
	format.PresentGroupID | format.PresentTSN | format.PresentFlags | format.PresentStreamID

// TestEncodeXRow_IgnoresPresent locks in the invariant of XRow.Present: it is
// decode-only and EncodeXRow never consults it. A hand-built row has
// Present == 0 and must still encode its whole header; a row whose Present
// disagrees with its fields encodes from the fields.
func TestEncodeXRow_IgnoresPresent(t *testing.T) {
	t.Parallel()

	r := format.XRow{
		Type: iproto.IPROTO_INSERT, Sync: 5, SchemaVersion: 6, ReplicaID: 3, GroupID: 1,
		LSN: 11, TSN: 10, Timestamp: 1.5, StreamID: 7,
		Flags: iproto.IPROTO_FLAG_COMMIT | iproto.IPROTO_FLAG_WAIT_SYNC, BodyRaw: []byte{0x80},
	}
	require.Zero(t, r.Present, "a hand-built row has no Present bits")

	buf, err := format.EncodeXRow(nil, &r)
	require.NoError(t, err)

	got, _, err := format.DecodeXRow(buf)
	require.NoError(t, err)
	assert.Equal(t, allPresent, got.Present, "every header key is on the wire")

	gotFields := *got
	gotFields.Present = 0

	assert.Equal(t, r, gotFields, "every field round-trips")

	// Present bits for keys whose field is zero, and none for keys whose
	// field is set: the encoding is the same.
	for _, p := range []format.Present{0, format.PresentType, allPresent, ^format.Present(0)} {
		bogus := r
		bogus.Present = p

		again, err := format.EncodeXRow(nil, &bogus)
		require.NoError(t, err)
		assert.Equalf(t, buf, again, "Present=%#x", p)
	}

	empty := format.XRow{Type: iproto.IPROTO_INSERT, Present: allPresent, BodyRaw: []byte{0x80}}

	buf, err = format.EncodeXRow(nil, &empty)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x81, 0x00, 0x02, 0x80}, buf, "Present does not resurrect zero fields")
}

// TestDecodeXRow_Present — Present records the keys on the wire, tells a key
// with the value 0 from an absent one, and is not set by the TSN and commit
// flag the decoder infers for a single-statement row.
func TestDecodeXRow_Present(t *testing.T) {
	t.Parallel()

	t.Run("single statement", func(t *testing.T) {
		t.Parallel()

		buf, err := format.EncodeXRow(nil, &format.XRow{
			Type: iproto.IPROTO_INSERT, LSN: 5, TSN: 5, Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: []byte{0x80},
		})
		require.NoError(t, err)

		got, _, err := format.DecodeXRow(buf)
		require.NoError(t, err)
		assert.Equal(t, format.PresentType|format.PresentLSN, got.Present)
		assert.True(t, got.HasType())
		assert.True(t, got.HasLSN())
		assert.Equal(t, int64(5), got.TSN, "TSN is inferred")
		assert.False(t, got.HasTSN(), "an inferred TSN is not on the wire")
		assert.True(t, got.IsCommit(), "commit is inferred")
		assert.False(t, got.HasFlags(), "an inferred commit flag is not on the wire")
	})

	t.Run("heartbeat", func(t *testing.T) {
		t.Parallel()

		// A relay heartbeat: IPROTO_OK with a timestamp and no LSN.
		buf, err := format.EncodeXRow(nil, &format.XRow{Type: iproto.IPROTO_OK, ReplicaID: 2, Timestamp: 3.5})
		require.NoError(t, err)

		got, _, err := format.DecodeXRow(buf)
		require.NoError(t, err)
		assert.False(t, got.HasLSN())
		assert.True(t, got.HasTimestamp())
		assert.True(t, got.HasReplicaID())
		assert.False(t, got.HasSync())
	})

	t.Run("zero values", func(t *testing.T) {
		t.Parallel()

		// Every recognised key, each with the value 0 (the encoder would
		// omit them), and an unknown key that sets no bit.
		zero := []byte{0x00}
		hdr := make([]byte, 0, 64)

		for _, k := range []iproto.Key{
			iproto.IPROTO_REQUEST_TYPE, iproto.IPROTO_SYNC, iproto.IPROTO_REPLICA_ID,
			iproto.IPROTO_LSN, iproto.IPROTO_SCHEMA_VERSION, iproto.IPROTO_GROUP_ID,
			iproto.IPROTO_TSN, iproto.IPROTO_FLAGS, iproto.IPROTO_STREAM_ID,
		} {
			hdr = append(hdr, kv(uint64(k), zero)...)
		}

		hdr = append(hdr, kv(uint64(iproto.IPROTO_TIMESTAMP), []byte{0xcb, 0, 0, 0, 0, 0, 0, 0, 0})...)
		hdr = append(hdr, kv(0x7d, zero)...)
		raw := append(buildMap(11, hdr), 0x80)

		got, _, err := format.DecodeXRow(raw)
		require.NoError(t, err)
		assert.Equal(t, allPresent, got.Present)
		assert.True(t, got.HasType())
		assert.True(t, got.HasSync())
		assert.True(t, got.HasReplicaID())
		assert.True(t, got.HasLSN())
		assert.True(t, got.HasTimestamp())
		assert.True(t, got.HasSchemaVersion())
		assert.True(t, got.HasGroupID())
		assert.True(t, got.HasTSN())
		assert.True(t, got.HasFlags())
		assert.True(t, got.HasStreamID())
		assert.False(t, got.IsCommit(), "IPROTO_TSN is on the wire, so no commit is inferred")
	})
}

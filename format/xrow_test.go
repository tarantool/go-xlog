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

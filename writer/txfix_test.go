package writer //nolint:testpackage // white-box: tests the unexported assignTxIDs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

// TestAssignTxIDs_Empty — no-op on empty input.
func TestAssignTxIDs_Empty(t *testing.T) {
	t.Parallel()

	// Should not panic.
	assignTxIDs(nil)
	assignTxIDs([]format.XRow{})
}

// TestAssignTxIDs_SingleRow — single row gets TSN=LSN, FlagCommit set, other
// flags preserved. The encoder will then emit the single-stmt short form.
func TestAssignTxIDs_SingleRow(t *testing.T) {
	t.Parallel()

	rows := []format.XRow{{
		Type:  iproto.IPROTO_INSERT,
		LSN:   5,
		Flags: iproto.IPROTO_FLAG_WAIT_SYNC, // Caller-set non-commit flag.
	}}
	assignTxIDs(rows)
	require.Equal(t, int64(5), rows[0].TSN, "TSN")
	require.True(t, rows[0].IsCommit(), "FlagCommit should be set on single-row tx")
	require.NotZero(t, rows[0].Flags&iproto.IPROTO_FLAG_WAIT_SYNC, "FlagWaitSync should be preserved")
}

// TestAssignTxIDs_MultiRow — TSN = rows[0].LSN for all; only last row has
// FlagCommit set; earlier rows have it cleared.
func TestAssignTxIDs_MultiRow(t *testing.T) {
	t.Parallel()

	rows := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 10, Flags: iproto.IPROTO_FLAG_COMMIT | iproto.IPROTO_FLAG_WAIT_ACK}, // Caller stale commit bit.
		{Type: iproto.IPROTO_INSERT, LSN: 11},
		{Type: iproto.IPROTO_INSERT, LSN: 12},
	}
	assignTxIDs(rows)

	for i, r := range rows {
		assert.Equal(t, int64(10), r.TSN, "row[%d].TSN", i)
	}

	assert.False(t, rows[0].IsCommit(), "row[0] should have FlagCommit cleared (was set by caller stale)")
	assert.False(t, rows[1].IsCommit(), "row[1] should not have FlagCommit set")
	assert.True(t, rows[2].IsCommit(), "row[2] (last) should have FlagCommit set")
	// Non-commit flags preserved (WaitAck on row[0]).
	assert.NotZero(t, rows[0].Flags&iproto.IPROTO_FLAG_WAIT_ACK, "row[0] FlagWaitAck should be preserved")
}

package writer

import (
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

// assignTxIDs is the pre-encode pass over a tx's rows. It:
//
//   - sets every row's TSN to rows[0].LSN (the tsn convention for a logical tx);
//   - clears IPROTO_FLAG_COMMIT on every row;
//   - sets IPROTO_FLAG_COMMIT on the *last* row only.
//
// Non-commit flag bits (IPROTO_FLAG_WAIT_SYNC, IPROTO_FLAG_WAIT_ACK) are preserved exactly as
// the caller set them. For a 1-row tx the last row is row[0]; IPROTO_FLAG_COMMIT is
// set, TSN == LSN, and the format encoder will then omit IPROTO_TSN AND clear
// IPROTO_FLAG_COMMIT on the wire (single-stmt short form, src/box/xrow.c:402-410).
//
// No-op on an empty slice.
func assignTxIDs(rows []format.XRow) {
	if len(rows) == 0 {
		return
	}

	tsn := rows[0].LSN

	last := len(rows) - 1
	for i := range rows {
		rows[i].TSN = tsn

		rows[i].Flags &^= iproto.IPROTO_FLAG_COMMIT
		if i == last {
			rows[i].Flags |= iproto.IPROTO_FLAG_COMMIT
		}
	}
}

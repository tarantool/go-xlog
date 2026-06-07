//go:build tarantool

package integration

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/writer"
)

// dumpLua reads the xlog at arg[1] with Tarantool's own xlog.pairs reader and
// prints one JSON object per row to stdout. It needs no box.cfg — the xlog
// module reads files standalone.
const dumpLua = `
local xlog = require('xlog')
local json = require('json')
for _, row in xlog.pairs(arg[1]) do
    -- Normalise the box tuple to a plain array so json.encode is stable.
    if row.BODY ~= nil and row.BODY.tuple ~= nil then
        row.BODY.tuple = row.BODY.tuple:totable()
    end
    print(json.encode(row))
end
os.exit(0)
`

// TestWrite_GoXlogWritesTarantoolReads writes an xlog with go-xlog (single
// inserts, a multi-statement tx, and a >2 KiB tuple that crosses the zstd
// compression threshold) and asserts Tarantool's xlog.pairs decodes every
// row, tuple, and tx coordinate exactly as written.
func TestWrite_GoXlogWritesTarantoolReads(t *testing.T) {
	t.Parallel()

	bin := requireTarantool(t)
	work := t.TempDir()
	path := filepath.Join(work, "00000000000000000000.xlog")

	big := strings.Repeat("z", 4096) // > CompressThreshold (2 KiB) → zstd tx.

	meta := &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "go-xlog/integration",
		InstanceUUID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		VClock:       format.VClock{1: 0},
	}

	w, err := writer.Create(path, meta)
	require.NoError(t, err, "writer.Create")

	// Three single-statement transactions (LSN 1..3).
	singles := []struct {
		lsn   int64
		tuple []interface{}
	}{
		{1, []interface{}{int64(1), "alpha"}},
		{2, []interface{}{int64(2), "beta"}},
		{3, []interface{}{int64(3), "gamma"}},
	}
	for _, s := range singles {
		row := format.XRow{
			Type:    iproto.IPROTO_INSERT,
			LSN:     s.lsn,
			BodyRaw: encodeDML(t, 512, s.tuple...),
		}
		require.NoErrorf(t, w.WriteTx([]format.XRow{row}), "WriteTx single (lsn %d)", s.lsn)
	}

	// One three-statement transaction (LSN 4..6); the writer fixes up the
	// shared TSN and places the commit flag on the last row.
	multi := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 4, BodyRaw: encodeDML(t, 512, int64(10), "x")},
		{Type: iproto.IPROTO_INSERT, LSN: 5, BodyRaw: encodeDML(t, 512, int64(11), "y")},
		{Type: iproto.IPROTO_INSERT, LSN: 6, BodyRaw: encodeDML(t, 512, int64(12), "z")},
	}
	require.NoError(t, w.WriteTx(multi), "WriteTx multi")

	// One single-statement tx with a 4 KiB tuple → zstd-compressed block.
	bigRow := format.XRow{
		Type:    iproto.IPROTO_INSERT,
		LSN:     7,
		BodyRaw: encodeDML(t, 512, int64(100), big),
	}
	require.NoError(t, w.WriteTx([]format.XRow{bigRow}), "WriteTx big")

	require.NoError(t, w.Close(), "writer.Close")

	// Tarantool reads it back.
	out := runLua(t, bin, work, dumpLua, path)
	lines := nonEmptyLines(out)

	var got []luaRow
	for i, ln := range lines {
		var r luaRow
		require.NoErrorf(t, json.Unmarshal([]byte(ln), &r), "unmarshal row %d (%q)", i, ln)
		got = append(got, r)
	}

	type want struct {
		lsn    int64
		tsn    int64 // 0 == absent in JSON (single-stmt).
		commit bool
		tuple  []interface{}
	}
	wants := []want{
		{1, 0, false, []interface{}{int64(1), "alpha"}},
		{2, 0, false, []interface{}{int64(2), "beta"}},
		{3, 0, false, []interface{}{int64(3), "gamma"}},
		{4, 4, false, []interface{}{int64(10), "x"}},
		{5, 4, false, []interface{}{int64(11), "y"}},
		{6, 4, true, []interface{}{int64(12), "z"}},
		{7, 0, false, []interface{}{int64(100), big}},
	}
	require.Lenf(t, got, len(wants), "Tarantool decoded %d rows, want %d:\n%s", len(got), len(wants), out)

	for i, wnt := range wants {
		g := got[i]
		assert.Equalf(t, "INSERT", g.Header.Type, "row[%d] type = %q, want INSERT", i, g.Header.Type)
		assert.Equalf(t, wnt.lsn, g.Header.LSN, "row[%d] lsn = %d, want %d", i, g.Header.LSN, wnt.lsn)
		assert.Equalf(t, wnt.tsn, g.Header.TSN, "row[%d] tsn = %d, want %d", i, g.Header.TSN, wnt.tsn)
		assert.Equalf(t, wnt.commit, g.Header.Commit, "row[%d] commit = %v, want %v", i, g.Header.Commit, wnt.commit)
		assert.Equalf(t, int64(512), g.Body.SpaceID, "row[%d] space_id = %d, want 512", i, g.Body.SpaceID)
		assert.Truef(t, tuplesEqual(g.Body.Tuple, wnt.tuple), "row[%d] tuple mismatch:\n got  %v\n want %v", i, g.Body.Tuple, wnt.tuple)
	}
}

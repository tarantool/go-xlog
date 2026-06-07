//go:build tarantool

package integration

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// genLua boots Tarantool in the cwd, creates user space 512, and writes a
// deterministic mix of single-statement inserts and one multi-statement
// transaction into the bootstrap xlog. Clean os.exit closes the xlog with
// its EOF marker so go-xlog sees a complete file.
const genLua = `
box.cfg{ work_dir = '.', log = 'tarantool.log', listen = box.NULL }
local s = box.schema.space.create('test', { id = 512 })
s:create_index('pk', { parts = {1, 'unsigned'} })
-- three single-statement inserts
s:insert{1, 'alpha'}
s:insert{2, 'beta'}
s:insert{3, 'gamma'}
-- one three-statement transaction
box.begin()
s:insert{10, 'x'}
s:insert{11, 'y'}
s:insert{12, 'z'}
box.commit()
os.exit(0)
`

// TestRead_TarantoolWritesGoXlogReads has a real Tarantool emit an xlog and
// asserts go-xlog decodes the user-space rows, tuples, and tx-grouping
// exactly as inserted.
func TestRead_TarantoolWritesGoXlogReads(t *testing.T) {
	t.Parallel()

	bin := requireTarantool(t)
	work := t.TempDir()
	runLua(t, bin, work, genLua)

	xlogs, err := filepath.Glob(filepath.Join(work, "*.xlog"))
	require.NoError(t, err, "glob xlogs")
	require.NotEmpty(t, xlogs, "Tarantool produced no .xlog files")
	sort.Strings(xlogs)

	// Collect every space-512 row across all xlogs, in order, with its tuple
	// decoded and its tx coordinates retained.
	type row struct {
		lsn      int64
		tsn      int64
		isCommit bool
		tuple    []interface{}
	}
	var got []row
	for _, path := range xlogs {
		r, err := reader.Open(path)
		require.NoErrorf(t, err, "Open %s", path)
		assert.Equalf(t, format.FiletypeXLOG, r.Meta().Filetype, "%s: Filetype = %q, want XLOG", path, r.Meta().Filetype)
		for xr, err := range r.Rows() {
			require.NoErrorf(t, err, "Rows %s", path)
			body, err := format.DecodeDMLBody(xr.BodyRaw)
			require.NoErrorf(t, err, "DecodeDMLBody (lsn %d)", xr.LSN)
			if body.SpaceID != 512 {
				continue // Skip schema rows (_space, _index, ...)
			}
			got = append(got, row{
				lsn:      xr.LSN,
				tsn:      xr.TSN,
				isCommit: xr.IsCommit(),
				tuple:    decodeTuple(t, body.Tuple),
			})
		}
		r.Close()
	}

	wantTuples := [][]interface{}{
		{int64(1), "alpha"},
		{int64(2), "beta"},
		{int64(3), "gamma"},
		{int64(10), "x"},
		{int64(11), "y"},
		{int64(12), "z"},
	}
	require.Lenf(t, got, len(wantTuples), "space-512 row count = %d, want %d (rows: %+v)", len(got), len(wantTuples), got)
	for i, want := range wantTuples {
		assert.Truef(t, tuplesEqual(got[i].tuple, want), "row[%d] tuple = %v, want %v", i, got[i].tuple, want)
	}

	// LSNs strictly increasing.
	for i := 1; i < len(got); i++ {
		assert.Greaterf(t, got[i].lsn, got[i-1].lsn, "LSN not increasing at row %d: %d <= %d", i, got[i].lsn, got[i-1].lsn)
	}

	// The three single-statement inserts: decoder infers TSN == LSN and a
	// commit flag on each.
	for i := 0; i < 3; i++ {
		assert.Equalf(t, got[i].lsn, got[i].tsn, "single-stmt row[%d]: TSN %d != LSN %d", i, got[i].tsn, got[i].lsn)
		assert.Truef(t, got[i].isCommit, "single-stmt row[%d] should be a commit", i)
	}

	// The multi-statement tx: rows 3..5 share one TSN; only the last commits.
	txTSN := got[3].tsn
	assert.Equalf(t, got[3].lsn, txTSN, "tx first row TSN %d != its LSN %d", txTSN, got[3].lsn)
	for i := 3; i <= 5; i++ {
		assert.Equalf(t, txTSN, got[i].tsn, "tx row[%d] TSN %d, want shared %d", i, got[i].tsn, txTSN)
		wantCommit := i == 5
		assert.Equalf(t, wantCommit, got[i].isCommit, "tx row[%d] isCommit = %v, want %v", i, got[i].isCommit, wantCommit)
	}
}

// TestRead_NextTxGrouping re-reads the Tarantool xlog through NextTx and
// asserts the multi-statement insert surfaces as a single 3-row transaction.
func TestRead_NextTxGrouping(t *testing.T) {
	t.Parallel()

	bin := requireTarantool(t)
	work := t.TempDir()
	runLua(t, bin, work, genLua)

	xlogs, _ := filepath.Glob(filepath.Join(work, "*.xlog"))
	sort.Strings(xlogs)

	var multiRowTuples [][]interface{}
	for _, path := range xlogs {
		r, err := reader.Open(path)
		require.NoErrorf(t, err, "Open %s", path)
		for {
			tx, err := r.NextTx()
			if err != nil {
				break
			}
			// Decode the space-512 tuples in this tx; ignore schema-only txs.
			var tuples [][]interface{}
			allCommitCorrect := true
			for i, xr := range tx.Rows {
				body, err := format.DecodeDMLBody(xr.BodyRaw)
				require.NoError(t, err, "DecodeDMLBody")
				if body.SpaceID != 512 {
					tuples = nil
					break
				}
				tuples = append(tuples, decodeTuple(t, body.Tuple))
				// Only the final row of the group should carry the commit flag.
				if xr.IsCommit() != (i == len(tx.Rows)-1) {
					allCommitCorrect = false
				}
			}
			if len(tuples) == 3 {
				assert.True(t, allCommitCorrect, "3-row tx: commit flag not on last row only")
				multiRowTuples = tuples
			}
		}
		r.Close()
	}

	want := [][]interface{}{
		{int64(10), "x"},
		{int64(11), "y"},
		{int64(12), "z"},
	}
	require.Lenf(t, multiRowTuples, len(want), "multi-row tx tuple count = %d, want %d (%v)", len(multiRowTuples), len(want), multiRowTuples)
	for i := range want {
		assert.Truef(t, tuplesEqual(multiRowTuples[i], want[i]), "multi-row tx tuple[%d] = %v, want %v", i, multiRowTuples[i], want[i])
	}
}

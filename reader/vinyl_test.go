package reader_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/reader"
)

// runExt is the extension of a vinyl run file.
const runExt = ".run"

// vinylPage is a page of a .run file: a tx block of statements ended by a row
// index.
type vinylPage struct {
	offset    int64 // where the block starts in the file
	size      int   // the size of the block in the file
	plain     int   // the size of its rows, uncompressed
	rowOffset []int // where each row starts in the rows
	stmts     []format.XRow
	index     []uint32
}

// readRun reads the pages of a .run file block by block.
func readRun(t *testing.T, path string) []vinylPage {
	t.Helper()

	r, err := reader.Open(path)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	require.Equal(t, format.FiletypeRUN, r.Meta().Filetype)

	var pages []vinylPage

	for {
		offset := r.Offset()

		block, err := r.NextBlockRaw()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}

		// The block aliases a buffer of the reader the next block reuses.
		slices, plain, n, err := format.DecodeTxBlock(bytes.Clone(block))
		require.NoError(t, err)
		require.NotEmpty(t, slices)

		page := vinylPage{offset: offset, size: n, plain: len(plain)}

		for _, slice := range slices {
			row, _, err := format.DecodeXRow(slice)
			require.NoError(t, err)

			page.rowOffset = append(page.rowOffset, cap(plain)-cap(slice))
			page.stmts = append(page.stmts, *row)
		}

		last := page.stmts[len(page.stmts)-1]
		require.Equal(t, iproto.VY_RUN_ROW_INDEX, last.Type, "a page ends with its row index")

		page.index, err = format.DecodeVyRowIndexBody(last.BodyRaw)
		require.NoError(t, err)

		page.stmts = page.stmts[:len(page.stmts)-1]
		pages = append(pages, page)
	}

	return pages
}

// readIndex reads the run info and the page infos of a .index file.
func readIndex(t *testing.T, path string) (*format.VyRunInfo, []*format.VyPageInfo) {
	t.Helper()

	r, err := reader.Open(path)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	require.Equal(t, format.FiletypeINDEX, r.Meta().Filetype)

	var (
		info  *format.VyRunInfo
		pages []*format.VyPageInfo
	)

	for row, err := range r.Rows() {
		require.NoError(t, err)

		switch row.Type { //nolint:exhaustive // An index holds only these two.
		case iproto.VY_INDEX_RUN_INFO:
			require.Nil(t, info, "one run info")

			info, err = format.DecodeVyRunInfoBody(row.BodyRaw)
			require.NoError(t, err)
		case iproto.VY_INDEX_PAGE_INFO:
			require.NotNil(t, info, "the run info comes first")

			page, err := format.DecodeVyPageInfoBody(row.BodyRaw)
			require.NoError(t, err)

			pages = append(pages, page)
		default:
			t.Fatalf("%s: unexpected row type %s", path, format.TypeName(row.Type))
		}
	}

	require.NotNil(t, info)

	return info, pages
}

func renderMP(t *testing.T, raw []byte) string {
	t.Helper()

	var v any
	require.NoError(t, msgpack.Unmarshal(raw, &v))

	return fmt.Sprint(v)
}

// renderStmt renders a statement as type, LSN, the tuple, key or operations,
// and the flags.
func renderStmt(t *testing.T, row format.XRow) string {
	t.Helper()

	body, err := format.DecodeVyStmtBody(row.BodyRaw)
	require.NoError(t, err)
	assert.Empty(t, body.Extras, "unrecognised statement keys")
	assert.Zero(t, row.ReplicaID, "a statement has no replica")

	out := fmt.Sprintf("%s@%d", format.TypeName(row.Type), row.LSN)
	if body.Tuple != nil {
		out += " " + renderMP(t, body.Tuple)
	}

	if body.Key != nil {
		out += " key=" + renderMP(t, body.Key)
	}

	if body.Ops != nil {
		out += " ops=" + renderMP(t, body.Ops)
	}

	if body.Flags != 0 {
		out += fmt.Sprintf(" flags=%d", body.Flags)
	}

	return out
}

// TestVinyl_ReadRealRuns reads every .run and .index file a real Tarantool
// wrote (scripts/gen-vinyl.lua) and checks that the index describes its run:
// the page offsets are where the reader finds the pages, the counts and the
// LSN bounds match the statements, and each row index has an offset per
// statement.
func TestVinyl_ReadRealRuns(t *testing.T) {
	t.Parallel()

	root := testutil.Path(t, "vinyl")

	var runs []string

	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err == nil && filepath.Ext(path) == runExt {
			runs = append(runs, path)
		}

		return err
	})
	require.NoError(t, err)
	require.NotEmpty(t, runs)

	got := map[string][]string{}
	compressed, multiPage := 0, 0

	for _, run := range runs {
		rel, err := filepath.Rel(root, run)
		require.NoError(t, err)

		pages := readRun(t, run)
		info, infos := readIndex(t, strings.TrimSuffix(run, runExt)+".index")

		require.Len(t, infos, int(info.PageCount), "%s: page count", rel)
		require.Len(t, pages, len(infos), "%s: pages", rel)

		if len(pages) > 1 {
			multiPage++
		}

		assert.Equal(t, uint8(3), info.BloomVersion, "%s: bloom filter", rel)
		assert.NotEmpty(t, info.Bloom, "%s: bloom filter", rel)

		var (
			stat   format.VyStmtStat
			minLSN = info.MaxLSN
			maxLSN = info.MinLSN
		)

		for i, page := range pages {
			pi := infos[i]
			assert.Equal(t, uint64(page.offset), pi.Offset, "%s: page %d offset", rel, i)
			assert.Equal(t, page.size, int(pi.Size), "%s: page %d size", rel, i)
			assert.Equal(t, page.plain, int(pi.UnpackedSize), "%s: page %d unpacked size", rel, i)
			assert.Len(t, page.stmts, int(pi.RowCount), "%s: page %d rows", rel, i)
			require.Len(t, page.index, len(page.stmts), "%s: page %d row index", rel, i)

			for k, off := range page.index {
				assert.Equal(t, page.rowOffset[k], int(off), "%s: page %d row %d offset", rel, i, k)
			}

			assert.Equal(t, page.rowOffset[len(page.stmts)], int(pi.RowIndexOffset), "%s: page %d row index offset", rel, i)

			if pi.Size < pi.UnpackedSize {
				compressed++
			}

			if i > 0 {
				got[rel] = append(got[rel], "-- page")
			}

			for _, stmt := range page.stmts {
				switch stmt.Type { //nolint:exhaustive // A run holds only these four.
				case iproto.IPROTO_INSERT:
					stat.Inserts++
				case iproto.IPROTO_REPLACE:
					stat.Replaces++
				case iproto.IPROTO_DELETE:
					stat.Deletes++
				case iproto.IPROTO_UPSERT:
					stat.Upserts++
				default:
					t.Fatalf("%s: unexpected statement %s", rel, format.TypeName(stmt.Type))
				}

				minLSN = min(minLSN, stmt.LSN)
				maxLSN = max(maxLSN, stmt.LSN)
				got[rel] = append(got[rel], renderStmt(t, stmt))
			}
		}

		assert.Equal(t, info.StmtStat, stat, "%s: statement counts", rel)
		assert.Equal(t, info.MinLSN, minLSN, "%s: min LSN", rel)
		assert.Equal(t, info.MaxLSN, maxLSN, "%s: max LSN", rel)
	}

	assert.Positive(t, compressed, "no zstd-compressed page")
	assert.Positive(t, multiPage, "no run of several pages")

	// Deferred DELETEs, squashed UPSERTs and their operation lists as
	// Tarantool writes them.
	for rel, want := range map[string][]string{
		// A primary index of a space with defer_deletes: the REPLACE and the
		// DELETE did not delete the old tuple from the secondary index.
		"700/0/00000000000000000017.run": {
			"REPLACE@11 [1 a2] flags=1",
			"DELETE@12 key=[2] flags=1",
			"INSERT@13 [5 e " + strings.Repeat("z", 3000) + "]",
		},
		// The deferred DELETEs compaction of the primary index generated,
		// in the parts of the secondary index and the primary one.
		"700/1/00000000000000000024.run": {
			"DELETE@11 key=[a 1] flags=2",
			"DELETE@12 key=[b 2] flags=2",
		},
		// Two UPSERTs of a key squashed into one statement of two operation
		// lists, and an UPSERT of a key no run holds.
		"701/0/00000000000000000014.run": {
			"UPSERT@15 [1 0] ops=[[[+ 1 1]] [[+ 1 10] [= 2 x]]]",
			"UPSERT@16 [2 0] ops=[[[+ 1 1]]]",
		},
	} {
		assert.Equal(t, want, got[rel], rel)
	}
}

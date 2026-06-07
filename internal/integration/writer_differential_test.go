//go:build tarantool

package integration

import (
	"encoding/json"
	"fmt"
	"math/rand"
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

// TestWriteDifferential_Tarantool is the randomized writer differential: for
// many seeded-random valid row sets, go-xlog writes an xlog and Tarantool's own
// xlog.pairs decodes it; every row's lsn/type/space_id/tuple and tx coordinates
// (tsn/commit) must match what the writer was given. A reproducible PRNG
// keeps any failure replayable.
func TestWriteDifferential_Tarantool(t *testing.T) {
	t.Parallel()

	bin := requireTarantool(t)
	rng := rand.New(rand.NewSource(0xC0FFEE))

	const cases = 30
	for k := 0; k < cases; k++ {
		t.Run(fmt.Sprintf("case-%02d", k), func(t *testing.T) {
			work := t.TempDir()
			path := filepath.Join(work, "00000000000000000000.xlog")
			exp := writeRandomXlog(t, path, rng)

			var got []luaRow
			for i, ln := range nonEmptyLines(runLua(t, bin, work, dumpLua, path)) {
				var r luaRow
				require.NoErrorf(t, json.Unmarshal([]byte(ln), &r), "unmarshal row %d (%q)", i, ln)
				got = append(got, r)
			}
			require.Lenf(t, got, len(exp), "Tarantool decoded %d rows, want %d", len(got), len(exp))
			for i := range exp {
				g, w := got[i], exp[i]
				assert.Equalf(t, "INSERT", g.Header.Type, "row[%d] type=%q, want INSERT", i, g.Header.Type)
				assert.Equalf(t, w.lsn, g.Header.LSN, "row[%d] lsn=%d, want %d", i, g.Header.LSN, w.lsn)
				assert.Equalf(t, w.tsn, g.Header.TSN, "row[%d] tsn=%d, want %d", i, g.Header.TSN, w.tsn)
				assert.Equalf(t, w.commit, g.Header.Commit, "row[%d] commit=%v, want %v", i, g.Header.Commit, w.commit)
				assert.Equalf(t, int64(512), g.Body.SpaceID, "row[%d] space_id=%d, want 512", i, g.Body.SpaceID)
				assert.Truef(t, tuplesEqual(g.Body.Tuple, w.tuple), "row[%d] tuple mismatch:\n got  %v\n want %v", i, g.Body.Tuple, w.tuple)
			}
		})
	}
}

// expRow is one row the writer emitted, with the coordinates Tarantool should
// report back. Single-statement rows carry tsn 0 (absent on the wire) and no
// commit; multi-statement rows share the tx's first LSN as tsn and commit only
// on the last row.
type expRow struct {
	lsn    int64
	tsn    int64
	commit bool
	tuple  []interface{}
}

// writeRandomXlog writes a random mix of single-statement inserts, multi-stmt
// transactions, and the occasional >2 KiB tuple (zstd path) into an xlog at
// path, and returns the expected decode.
func writeRandomXlog(t *testing.T, path string, rng *rand.Rand) []expRow {
	t.Helper()
	meta := &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "go-xlog/integration",
		InstanceUUID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		VClock:       format.VClock{1: 0},
	}
	w, err := writer.Create(path, meta)
	require.NoError(t, err, "writer.Create")

	var exp []expRow
	lsn := int64(1)
	ops := 1 + rng.Intn(6)
	for o := 0; o < ops; o++ {
		switch roll := rng.Intn(10); {
		case roll < 2: // Multi-statement transaction.
			n := 2 + rng.Intn(2)
			rows := make([]format.XRow, n)
			tuples := make([][]interface{}, n)
			base := lsn
			for j := 0; j < n; j++ {
				tuples[j] = randTuple(rng, lsn)
				rows[j] = format.XRow{Type: iproto.IPROTO_INSERT, LSN: lsn, BodyRaw: encodeDML(t, 512, tuples[j]...)}
				lsn++
			}
			require.NoError(t, w.WriteTx(rows), "WriteTx multi")
			for j := 0; j < n; j++ {
				exp = append(exp, expRow{lsn: base + int64(j), tsn: base, commit: j == n-1, tuple: tuples[j]})
			}
		default: // Single-statement insert; roll==9 forces a zstd-sized tuple.
			tup := randTuple(rng, lsn)
			if roll == 9 {
				tup = []interface{}{lsn, strings.Repeat("z", 3000)}
			}
			require.NoError(t, w.WriteTx([]format.XRow{{Type: iproto.IPROTO_INSERT, LSN: lsn, BodyRaw: encodeDML(t, 512, tup...)}}), "WriteTx single")
			exp = append(exp, expRow{lsn: lsn, tsn: 0, commit: false, tuple: tup})
			lsn++
		}
	}
	require.NoError(t, w.Close(), "writer.Close")
	return exp
}

// randTuple builds a tuple whose first field is the row id and which carries 1–2
// random scalar fields (int or short printable string).
func randTuple(rng *rand.Rand, id int64) []interface{} {
	tup := []interface{}{id}
	for extra := 1 + rng.Intn(2); extra > 0; extra-- {
		if rng.Intn(2) == 0 {
			tup = append(tup, int64(rng.Intn(1<<20)))
		} else {
			tup = append(tup, randString(rng))
		}
	}
	return tup
}

func randString(rng *rand.Rand) string {
	const cs = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, rng.Intn(12))
	for i := range b {
		b[i] = cs[rng.Intn(len(cs))]
	}
	return string(b)
}

package memsize_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/memsize"
	"github.com/tarantool/go-xlog/writer"
)

const replayTestSpaceID = uint32(512)

func TestAnalyzeReplaysDMLTransactions(t *testing.T) {
	t.Parallel()

	memtxDir, walDir := replayTestDirs(t)
	base := analyzeReplayFixture(t, memtxDir, t.TempDir())
	baseSpace := findSpaceReport(t, base, replayTestSpaceID)

	replacement := marshalMP(t, []any{uint64(1), strings.Repeat("r", 300)})
	deleted := marshalMP(t, []any{3, "gamma"})
	upserted := marshalMP(t, []any{uint64(900), "upserted"})
	ops := marshalMP(t, []any{[]any{"=", uint64(2), "ignored"}})

	writeReplayXlog(t, walDir, format.VClock{0: 1, 1: 21}, nil, [][]format.XRow{
		{
			replayRow(t, iproto.IPROTO_REPLACE, 22, dmlBody(t, replayTestSpaceID, replacement, nil, nil)),
			replayRow(t, iproto.IPROTO_DELETE, 23, dmlBody(t, replayTestSpaceID, nil, marshalMP(t, []any{uint64(3)}), nil)),
		},
		{
			replayRow(t, iproto.IPROTO_UPSERT, 24, dmlBody(t, replayTestSpaceID, upserted, nil, ops)),
		},
		{
			replayRow(t, iproto.IPROTO_UPDATE, 25, dmlBody(t, replayTestSpaceID, nil, marshalMP(t, []any{uint64(10)}), ops)),
		},
		{
			replayRow(t, iproto.IPROTO_RAFT, 26, []byte{0xc0}),
		},
	})

	report := analyzeReplayFixture(t, memtxDir, walDir)
	space := findSpaceReport(t, report, replayTestSpaceID)

	oldReplacement := marshalMP(t, []any{1, "alpha2"})
	wantPayload := baseSpace.PayloadBytes - uint64(len(oldReplacement)) - uint64(len(deleted)) +
		uint64(len(replacement)) + uint64(len(upserted))

	assert.Equal(t, baseSpace.TupleCount, space.TupleCount)
	assert.Equal(t, wantPayload, space.PayloadBytes)
	assert.True(t, space.Indexes[0].Estimated, "TREE bytes after WAL churn are approximate")
	assert.True(t, hasWarning(report, memsize.WarnUpdateApproximation, "test", ""))
	assert.Equal(t, format.VClock{0: 1, 1: 26}, report.Source.ToVClock)
	assert.Equal(t, uint64(5), report.Source.Rows-base.Source.Rows)
	assert.Equal(t, uint64(4), report.Source.Txs-base.Source.Txs)
}

func TestAnalyzeReplaysTruncate(t *testing.T) {
	t.Parallel()

	memtxDir, walDir := replayTestDirs(t)
	after := marshalMP(t, []any{uint64(77), "after truncate"})
	truncateOps := marshalMP(t, []any{[]any{"+", uint64(2), uint64(1)}})

	writeReplayXlog(t, walDir, format.VClock{0: 1, 1: 21}, nil, [][]format.XRow{
		{replayRow(t, iproto.IPROTO_UPSERT, 22,
			dmlBody(t, 330, marshalMP(t, []any{uint64(replayTestSpaceID), uint64(1)}), nil, truncateOps))},
		{replayRow(t, iproto.IPROTO_INSERT, 23, dmlBody(t, replayTestSpaceID, after, nil, nil))},
	})

	report := analyzeReplayFixture(t, memtxDir, walDir)
	space := findSpaceReport(t, report, replayTestSpaceID)

	assert.Equal(t, uint64(1), space.TupleCount)
	assert.Equal(t, uint64(len(after)), space.PayloadBytes)
	assert.False(t, hasWarning(report, memsize.WarnSchemaDDL, "test", ""))
}

func TestAnalyzeTruncateDeleteDoesNotClearSpace(t *testing.T) {
	t.Parallel()

	memtxDir, walDir := replayTestDirs(t)
	after := marshalMP(t, []any{uint64(77), "after truncate"})

	writeReplayXlog(t, walDir, format.VClock{0: 1, 1: 21}, nil, [][]format.XRow{
		{replayRow(t, iproto.IPROTO_REPLACE, 22,
			dmlBody(t, 330, marshalMP(t, []any{uint64(replayTestSpaceID), uint64(1)}), nil, nil))},
		{replayRow(t, iproto.IPROTO_INSERT, 23, dmlBody(t, replayTestSpaceID, after, nil, nil))},
		{replayRow(t, iproto.IPROTO_DELETE, 24,
			dmlBody(t, 330, nil, marshalMP(t, []any{uint64(replayTestSpaceID)}), nil))},
	})

	report := analyzeReplayFixture(t, memtxDir, walDir)
	space := findSpaceReport(t, report, replayTestSpaceID)

	assert.Equal(t, uint64(1), space.TupleCount)
	assert.Equal(t, uint64(len(after)), space.PayloadBytes)
}

func TestAnalyzeFiltersRowsAtSnapshotVClock(t *testing.T) {
	t.Parallel()

	memtxDir, walDir := replayTestDirs(t)
	base := analyzeReplayFixture(t, memtxDir, t.TempDir())

	writeReplayXlog(t, walDir, format.VClock{0: 1, 1: 20}, nil, [][]format.XRow{
		{replayRow(t, iproto.IPROTO_INSERT, 21,
			dmlBody(t, replayTestSpaceID, marshalMP(t, []any{uint64(800), "already snapshotted"}), nil, nil))},
		{replayRow(t, iproto.IPROTO_INSERT, 22,
			dmlBody(t, replayTestSpaceID, marshalMP(t, []any{uint64(801), "after snapshot"}), nil, nil))},
	})

	report := analyzeReplayFixture(t, memtxDir, walDir)
	space := findSpaceReport(t, report, replayTestSpaceID)

	assert.Equal(t, baseSpaceCount(t, base)+1, space.TupleCount)
	assert.Equal(t, uint64(1), report.Source.Rows-base.Source.Rows)
	assert.Equal(t, format.VClock{0: 1, 1: 22}, report.Source.ToVClock)
}

func TestAnalyzeDropsTornTrailingTransaction(t *testing.T) {
	t.Parallel()

	memtxDir, walDir := replayTestDirs(t)
	base := analyzeReplayFixture(t, memtxDir, t.TempDir())

	path := writeReplayXlog(t, walDir, format.VClock{0: 1, 1: 21}, nil, [][]format.XRow{
		{replayRow(t, iproto.IPROTO_INSERT, 22,
			dmlBody(t, replayTestSpaceID, marshalMP(t, []any{uint64(700), "committed"}), nil, nil))},
		{replayRow(t, iproto.IPROTO_INSERT, 23,
			dmlBody(t, replayTestSpaceID, marshalMP(t, []any{uint64(701), strings.Repeat("torn", 30)}), nil, nil))},
	})

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Truncate(path, info.Size()-20))

	report := analyzeReplayFixture(t, memtxDir, walDir)
	space := findSpaceReport(t, report, replayTestSpaceID)

	assert.Equal(t, baseSpaceCount(t, base)+1, space.TupleCount)
	assert.True(t, report.Source.TailTruncated)
	assert.True(t, hasWarning(report, memsize.WarnTailTruncated, "", ""))
}

func TestAnalyzeAcceptsLiveXlogWithoutEOFMarker(t *testing.T) {
	t.Parallel()

	memtxDir, walDir := replayTestDirs(t)
	base := analyzeReplayFixture(t, memtxDir, t.TempDir())

	path := writeReplayXlog(t, walDir, format.VClock{0: 1, 1: 21}, nil, [][]format.XRow{
		{replayRow(t, iproto.IPROTO_INSERT, 22,
			dmlBody(t, replayTestSpaceID, marshalMP(t, []any{uint64(702), "committed live"}), nil, nil))},
	})

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Truncate(path, info.Size()-format.MarkerSize))

	report := analyzeReplayFixture(t, memtxDir, walDir)
	space := findSpaceReport(t, report, replayTestSpaceID)

	assert.Equal(t, baseSpaceCount(t, base)+1, space.TupleCount)
	assert.True(t, report.Source.TailTruncated)
	assert.True(t, hasWarning(report, memsize.WarnTailTruncated, "", ""))
}

func TestAnalyzeWarnsForDDLAndSyncRollback(t *testing.T) {
	t.Parallel()

	memtxDir, walDir := replayTestDirs(t)
	writeReplayXlog(t, walDir, format.VClock{0: 1, 1: 21}, nil, [][]format.XRow{
		{replayRow(t, iproto.IPROTO_DELETE, 22,
			dmlBody(t, 280, nil, marshalMP(t, []any{uint64(replayTestSpaceID)}), nil))},
		{replayRow(t, iproto.IPROTO_DELETE, 23,
			dmlBody(t, 288, nil, marshalMP(t, []any{uint64(replayTestSpaceID), uint64(0)}), nil))},
		{replayRow(t, iproto.IPROTO_RAFT_ROLLBACK, 24, []byte{0xc0})},
	})

	report := analyzeReplayFixture(t, memtxDir, walDir)
	assert.True(t, hasWarning(report, memsize.WarnSchemaDDL, "test", ""))
	assert.True(t, hasWarning(report, memsize.WarnSchemaDDL, "test", "pk"))
	assert.True(t, hasWarning(report, memsize.WarnSyncRollback, "", ""))
}

func TestAnalyzeRejectsXlogChainGap(t *testing.T) {
	t.Parallel()

	memtxDir, walDir := replayTestDirs(t)
	writeReplayXlog(t, walDir, format.VClock{0: 1, 1: 21}, nil, [][]format.XRow{
		{replayRow(t, iproto.IPROTO_INSERT, 22,
			dmlBody(t, replayTestSpaceID, marshalMP(t, []any{uint64(800), "first"}), nil, nil))},
	})
	writeReplayXlog(t, walDir, format.VClock{0: 1, 1: 22}, format.VClock{0: 1, 1: 19}, nil)

	_, err := memsize.AnalyzeDirs(context.Background(), memtxDir, walDir, memsize.Config{})
	require.Error(t, err)
	assert.ErrorIs(t, err, memsize.ErrChainGap)
}

func replayTestDirs(t *testing.T) (string, string) {
	t.Helper()

	memtxDir := t.TempDir()
	walDir := t.TempDir()
	source := testutil.Path(t, "historical/2.11/00000000000000000022.snap")
	data, err := os.ReadFile(source)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(memtxDir, filepath.Base(source)), data, 0o600))

	return memtxDir, walDir
}

func writeReplayXlog(
	t *testing.T,
	dir string,
	vclock format.VClock,
	prev format.VClock,
	txs [][]format.XRow,
) string {
	t.Helper()

	path := filepath.Join(dir, fmt.Sprintf("%020d.xlog", vclock.Signature()))
	journal, err := writer.Create(path, &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "memsize replay test",
		InstanceUUID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		VClock:       vclock,
		PrevVClock:   prev,
	}, writer.NoCompression(), writer.Sync(writer.SyncNone))
	require.NoError(t, err)

	for _, tx := range txs {
		require.NoError(t, journal.WriteTx(tx))
	}

	require.NoError(t, journal.Close())

	return path
}

func replayRow(t *testing.T, typ iproto.Type, lsn int64, body []byte) format.XRow {
	t.Helper()

	return format.XRow{Type: typ, ReplicaID: 1, LSN: lsn, BodyRaw: body}
}

func dmlBody(t *testing.T, spaceID uint32, tuple, key, ops []byte) []byte {
	t.Helper()

	body := map[uint64]any{uint64(iproto.IPROTO_SPACE_ID): uint64(spaceID)}
	if tuple != nil {
		body[uint64(iproto.IPROTO_TUPLE)] = msgpack.RawMessage(tuple)
	}

	if key != nil {
		body[uint64(iproto.IPROTO_KEY)] = msgpack.RawMessage(key)
	}

	if ops != nil {
		body[uint64(iproto.IPROTO_OPS)] = msgpack.RawMessage(ops)
	}

	return marshalMP(t, body)
}

func marshalMP(t *testing.T, value any) []byte {
	t.Helper()

	data, err := msgpack.Marshal(value)
	require.NoError(t, err)

	return data
}

func analyzeReplayFixture(t *testing.T, memtxDir, walDir string) *memsize.Report {
	t.Helper()

	report, err := memsize.AnalyzeDirs(context.Background(), memtxDir, walDir, memsize.Config{})
	require.NoError(t, err)

	return report
}

func baseSpaceCount(t *testing.T, report *memsize.Report) uint64 {
	t.Helper()

	return findSpaceReport(t, report, replayTestSpaceID).TupleCount
}

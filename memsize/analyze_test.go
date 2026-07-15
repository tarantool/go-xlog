package memsize_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/memsize"
)

func TestAnalyzeHistoricalReplay211(t *testing.T) {
	t.Parallel()

	snapPath := testutil.Path(t, "historical/2.11/00000000000000000022.snap")
	report, err := memsize.Analyze(context.Background(), filepath.Dir(snapPath), memsize.Config{})
	require.NoError(t, err)

	assert.Equal(t, snapPath, report.Source.SnapPath)
	require.Len(t, report.Source.XlogPaths, 2)
	assert.Equal(t, int64(22), report.Source.FromVClock.Signature())
	assert.Equal(t, int64(24), report.Source.ToVClock.Signature())
	assert.Positive(t, report.Source.Rows)
	assert.Positive(t, report.Source.Txs)
	assert.Equal(t, uint64(256<<20), report.Budget)
	assert.Equal(t, report.TupleBytes+report.IndexBytes, report.Total)
	assert.Equal(t, report.Total+report.Reserve <= report.Budget, report.Fits)

	testSpace := findSpaceReport(t, report, 512)
	assert.Equal(t, "test", testSpace.Name)
	assert.Equal(t, uint64(8), testSpace.TupleCount)
	assert.Equal(t, uint64(4169), testSpace.PayloadBytes)
	assert.Zero(t, testSpace.FieldMapSize)
	require.Len(t, testSpace.Indexes, 1)
	assert.Equal(t, uint64(8), testSpace.Indexes[0].Entries)
	assert.Equal(t, uint64(48<<10), testSpace.Indexes[0].Bytes)

	for _, space := range report.Spaces {
		assert.NotEqual(t, uint32(514), space.ID, "vinyl spaces are outside the memtx report")
	}

	assert.True(t, hasWarning(report, memsize.WarnDefaultConfig, "", ""))
}

func TestAnalyzeSystemAllocator(t *testing.T) {
	t.Parallel()

	snapPath := testutil.Path(t, "historical/2.11/00000000000000000022.snap")
	report, err := memsize.Analyze(context.Background(), filepath.Dir(snapPath), memsize.Config{
		Allocator: memsize.AllocatorSystem,
	})
	require.NoError(t, err)

	assert.True(t, hasWarning(report, memsize.WarnSystemAllocator, "", ""))
}

func TestAnalyzeLargeGranularity(t *testing.T) {
	t.Parallel()

	snapPath := testutil.Path(t, "historical/2.11/00000000000000000022.snap")
	report, err := memsize.Analyze(context.Background(), filepath.Dir(snapPath), memsize.Config{
		MemtxMemory: 32 << 20,
		Granularity: 64,
	})
	require.NoError(t, err)

	assert.Equal(t, uint64(64<<20), report.Budget, "Tarantool enforces its 64 MiB quota floor")

	for _, space := range report.Spaces {
		assert.Zero(t, space.TupleBytes%64)
	}
}

func TestAnalyzeNoSnapshot(t *testing.T) {
	t.Parallel()

	_, err := memsize.Analyze(context.Background(), t.TempDir(), memsize.Config{})
	require.Error(t, err)
	assert.ErrorIs(t, err, memsize.ErrNoSnapshot)
}

func TestAnalyzeCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	snapPath := testutil.Path(t, "historical/2.11/00000000000000000022.snap")
	_, err := memsize.Analyze(ctx, filepath.Dir(snapPath), memsize.Config{})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func findSpaceReport(t *testing.T, report *memsize.Report, id uint32) memsize.SpaceReport {
	t.Helper()

	for _, space := range report.Spaces {
		if space.ID == id {
			return space
		}
	}

	require.FailNowf(t, "missing space report", "space %d", id)

	return memsize.SpaceReport{}
}

func hasWarning(report *memsize.Report, kind memsize.WarningKind, space, index string) bool {
	for _, warning := range report.Warnings {
		if warning.Kind == kind && warning.Space == space && warning.Index == index {
			return true
		}
	}

	return false
}

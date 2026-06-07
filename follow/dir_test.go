package follow_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/follow"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/rotate"
	"github.com/tarantool/go-xlog/writer"
)

func TestDir_RotationSwitch(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()

	rw, err := rotate.New(dirPath, format.FiletypeXLOG, testInstance, format.VClock{1: 0}, rotate.MaxFileSize(1))
	require.NoError(t, err)

	// Writer goroutine: tiny MaxFileSize rotates on (almost) every tx, so each
	// row lands in its own finalised file as we go.
	go func() {
		for i := int64(1); i <= 5; i++ {
			_ = rw.WriteTx([]format.XRow{row(i)})

			time.Sleep(2 * time.Millisecond)
		}

		_ = rw.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := collectRows(follow.Dir(ctx, dirPath, format.FiletypeXLOG, follow.WithFromHead(), fastPoll()))

	c.waitCount(t, 5)
	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2, 3, 4, 5}, lsns)
}

func TestDir_NoStart(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)

	c := collectRows(follow.Dir(t.Context(), dirPath, format.FiletypeXLOG, fastPoll()))
	c.waitDone(t)

	_, err := c.snapshot()
	require.ErrorIs(t, err, follow.ErrNoStart)
}

func TestDir_ChainBroken(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)                 // file0
	writeChainFile(t, dirPath, format.VClock{1: 3}, format.VClock{1: 2}, 3) // file1: prev {1:2} ≠ file0 {1:1}

	c := collectRows(follow.Dir(t.Context(), dirPath, format.FiletypeXLOG, follow.WithFromHead(), fastPoll()))
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.ErrorIs(t, err, follow.ErrChainBroken)
	require.Equal(t, []int64{1}, lsns) // read file0's row, then the chain broke
}

func TestDir_StartLSN(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)
	writeChainFile(t, dirPath, format.VClock{1: 2}, format.VClock{1: 1}, 2)
	writeChainFile(t, dirPath, format.VClock{1: 3}, format.VClock{1: 2}, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// LocateLSN(1, 1) selects file0 (largest VClock[1] ≤ 1).
	c := collectRows(follow.Dir(ctx, dirPath, format.FiletypeXLOG, follow.WithStartLSN(1, 1), fastPoll()))

	c.waitCount(t, 3)
	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2, 3}, lsns)
}

func TestDir_CancelWaitingSuccessor(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)

	ctx, cancel := context.WithCancel(context.Background())

	c := collectRows(follow.Dir(ctx, dirPath, format.FiletypeXLOG, follow.WithFromHead(), fastPoll()))
	c.waitCount(t, 1) // read the only file; now waiting for a successor

	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1}, lsns)
}

func TestDir_ReadInprogress(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1) // finalised head, sig 1

	// An active successor, chaining onto the head, left .inprogress (not closed).
	ipPath := filepath.Join(dirPath, "2.xlog")

	ipw, err := writer.Create(ipPath, metaWith(format.VClock{1: 2}, format.VClock{1: 1}))
	require.NoError(t, err)

	defer func() { _ = ipw.Discard() }()

	require.NoError(t, ipw.WriteTx([]format.XRow{row(2)}))
	require.NoError(t, ipw.Sync()) // flush, but keep .inprogress (no Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := collectRows(follow.Dir(ctx, dirPath, format.FiletypeXLOG,
		follow.WithFromHead(), follow.WithReadInprogress(), fastPoll()))

	// Reads the finalised head (row 1) then hands off to the live .inprogress
	// successor (row 2) without waiting for it to be finalised.
	c.waitCount(t, 2)
	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, lsns)
}

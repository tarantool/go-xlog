package follow_test

import (
	"context"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/follow"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// txCollector drains a transaction iterator in a goroutine, recording each
// transaction's StartLSN and the terminal error (if any). It mirrors collector
// for the row iterators.
type txCollector struct {
	mu   sync.Mutex
	lsns []int64
	err  error
	done chan struct{}
}

func collectTxs(seq iter.Seq2[*reader.Transaction, error]) *txCollector {
	c := &txCollector{done: make(chan struct{})}

	go func() {
		defer close(c.done)

		for tx, err := range seq {
			if err != nil {
				c.mu.Lock()
				c.err = err
				c.mu.Unlock()

				return
			}

			c.mu.Lock()
			c.lsns = append(c.lsns, tx.StartLSN)
			c.mu.Unlock()
		}
	}()

	return c
}

func (c *txCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.lsns)
}

func (c *txCollector) snapshot() ([]int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]int64(nil), c.lsns...), c.err
}

func (c *txCollector) waitCount(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return c.count() >= n }, 2*time.Second, 5*time.Millisecond)
}

func (c *txCollector) waitDone(t *testing.T) {
	t.Helper()

	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatal("tx collector did not finish in time")
	}
}

// TestFileTx tails a single live file at transaction granularity, exercising
// FileTx and engine.runTx.
func TestFileTx(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/live.xlog"
	lw := newLiveWriter(t, path)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := collectTxs(follow.FileTx(ctx, path, fastPoll()))

	for i := int64(1); i <= 3; i++ {
		lw.write(t, i)
	}

	c.waitCount(t, 3)
	lw.finalize(t)
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2, 3}, lsns)
}

// TestDirTx tails a rotation chain at transaction granularity, exercising DirTx
// and engine.runTx across files.
func TestDirTx(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)
	writeChainFile(t, dirPath, format.VClock{1: 2}, format.VClock{1: 1}, 2)
	writeChainFile(t, dirPath, format.VClock{1: 3}, format.VClock{1: 2}, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := collectTxs(follow.DirTx(ctx, dirPath, format.FiletypeXLOG, follow.WithFromHead(), fastPoll()))

	c.waitCount(t, 3)
	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2, 3}, lsns)
}

// TestDir_StartVClock starts a directory follow at the file containing a target
// vclock via WithStartVClock + dir.LocateVClock.
func TestDir_StartVClock(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)
	writeChainFile(t, dirPath, format.VClock{1: 2}, format.VClock{1: 1}, 2)
	writeChainFile(t, dirPath, format.VClock{1: 3}, format.VClock{1: 2}, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// LocateVClock({1:2}) selects the file whose coverage includes {1:2}.
	c := collectRows(follow.Dir(ctx, dirPath, format.FiletypeXLOG,
		follow.WithStartVClock(format.VClock{1: 2}), fastPoll()))

	c.waitCount(t, 2)
	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{2, 3}, lsns)
}

// TestNewDirFollower drives the stateful dir Follower, pulling rows one at a
// time and checkpointing via Offset.
func TestNewDirFollower(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)
	writeChainFile(t, dirPath, format.VClock{1: 2}, format.VClock{1: 1}, 2)

	f := follow.NewDirFollower(dirPath, format.FiletypeXLOG, follow.WithFromHead(), fastPoll())
	defer func() { _ = f.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	r1, err := f.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), r1.LSN)

	r2, err := f.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), r2.LSN)

	// Offset reports a clean resume boundary within the current file.
	require.GreaterOrEqual(t, f.Offset(), int64(0))
}

// countingWatcher records how many times Wait is invoked, proving a custom
// Watcher supplied via WithWatcher is actually consulted.
type countingWatcher struct {
	mu    sync.Mutex
	calls int
}

func (w *countingWatcher) Wait(ctx context.Context, _ string) error {
	w.mu.Lock()
	w.calls++
	w.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Millisecond):
		return nil
	}
}

func (w *countingWatcher) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.calls
}

// TestWithWatcherAndReaderOptions exercises WithWatcher (custom Watcher path)
// and WithReaderOptions (threading a reader Option into every open).
func TestWithWatcherAndReaderOptions(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/live.xlog"
	lw := newLiveWriter(t, path)

	w := &countingWatcher{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := collectRows(follow.File(ctx, path,
		follow.WithWatcher(w),
		follow.WithReaderOptions(reader.SkipCorruptTx(), reader.WithAliasBodies()),
	))

	lw.write(t, 1)
	c.waitCount(t, 1)
	lw.finalize(t)
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1}, lsns)
	require.Positive(t, w.count(), "custom watcher should have been consulted")
}

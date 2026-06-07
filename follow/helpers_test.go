package follow_test

import (
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/follow"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/writer"
)

var testInstance = uuid.MustParse("00000000-0000-0000-0000-0000000000bb")

// body is a tiny valid msgpack body map {0x10: lsn}; valid for lsn <= 127.
func body(lsn int64) []byte { return []byte{0x81, 0x10, byte(lsn)} }

func metaWith(vclock, prev format.VClock) *format.Meta {
	return &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "go-xlog/test",
		InstanceUUID: testInstance,
		VClock:       vclock,
		PrevVClock:   prev,
	}
}

func row(lsn int64) format.XRow {
	return format.XRow{Type: iproto.IPROTO_INSERT, ReplicaID: 1, LSN: lsn, BodyRaw: body(lsn)}
}

// liveWriter streams an xlog to a fixed path (no .inprogress / rename), so a
// single-file follower can tail the same path while it is being written.
type liveWriter struct {
	w *writer.Writer
	f *os.File
}

func newLiveWriter(t *testing.T, path string) *liveWriter {
	t.Helper()

	f, err := os.Create(path)
	require.NoError(t, err)

	w, err := writer.NewWriter(f, metaWith(format.VClock{1: 0}, nil))
	require.NoError(t, err)

	require.NoError(t, w.Sync()) // flush meta so the follower can open the header

	return &liveWriter{w: w, f: f}
}

func (lw *liveWriter) write(t *testing.T, lsn int64) {
	t.Helper()
	require.NoError(t, lw.w.WriteTx([]format.XRow{row(lsn)}))
	require.NoError(t, lw.w.Sync())
}

func (lw *liveWriter) finalize(t *testing.T) {
	t.Helper()
	require.NoError(t, lw.w.Close()) // writes EOF marker + flush
	require.NoError(t, lw.f.Close())
}

// writeChainFile writes a finalised <sig>.xlog into dirPath with one tx (lsn).
func writeChainFile(t *testing.T, dirPath string, vclock, prev format.VClock, lsn int64) {
	t.Helper()

	path := filepath.Join(dirPath, strconv.FormatInt(vclock.Signature(), 10)+".xlog")

	w, err := writer.Create(path, metaWith(vclock, prev))
	require.NoError(t, err)

	require.NoError(t, w.WriteTx([]format.XRow{row(lsn)}))
	require.NoError(t, w.Close())
}

// collector drains an iterator in a goroutine, recording row LSNs and the
// terminal error (if any).
type collector struct {
	mu   sync.Mutex
	lsns []int64
	err  error
	done chan struct{}
}

func collectRows(seq iter.Seq2[format.XRow, error]) *collector {
	c := &collector{done: make(chan struct{})}

	go func() {
		defer close(c.done)

		for r, err := range seq {
			if err != nil {
				c.mu.Lock()
				c.err = err
				c.mu.Unlock()

				return
			}

			c.mu.Lock()
			c.lsns = append(c.lsns, r.LSN)
			c.mu.Unlock()
		}
	}()

	return c
}

func (c *collector) snapshot() ([]int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]int64(nil), c.lsns...), c.err
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.lsns)
}

// waitCount blocks until the collector has at least n rows or the deadline hits.
func (c *collector) waitCount(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return c.count() >= n }, 2*time.Second, 5*time.Millisecond)
}

// waitDone blocks until the collecting goroutine has returned.
func (c *collector) waitDone(t *testing.T) {
	t.Helper()

	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatal("collector did not finish in time")
	}
}

// fastPoll is the poll interval used across tests for snappy, low-flake runs.
func fastPoll() follow.Option { return follow.WithPollInterval(5 * time.Millisecond) }

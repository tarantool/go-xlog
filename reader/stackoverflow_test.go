package reader_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime/debug"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/reader"
)

// reproEnv flips the test binary into "child" mode: instead of re-execing
// itself it runs the vulnerable parse directly. See
// TestSkipCorruptTx_ManyBadBlocks_NoStackOverflow.
const reproEnv = "XLOG_REPRO_STACKOVERFLOW"

// childMaxStack is a deliberately small max goroutine stack for the child
// process. The pre-fix recursive recovery path (loadNextTx → readTxBlock →
// loadNextTx …) grows the stack one frame per corrupt block, so a modest file
// blows a 4 MiB stack; the fixed iterative path runs in constant stack and
// completes regardless. Keeping the cap small keeps the crafted fixture (and
// the test) cheap.
const childMaxStack = 4 << 20 // 4 MiB

// manyBadBlocksCount is sized so the recursive path overflows childMaxStack
// with comfortable margin, while the iterative path drains it in O(1) stack.
const manyBadBlocksCount = 200_000

// TestSkipCorruptTx_ManyBadBlocks_NoStackOverflow reproduces the stack-overflow
// DoS: a crafted file of many small bad-CRC RowMarker blocks read under
// SkipCorruptTx drives the mutually-recursive recovery path
// (loadNextTx ↔ readTxBlock, and the in-memory twins) into unbounded recursion,
// crashing the process with an uncatchable "goroutine stack exceeds" fatal
// error.
//
// A stack overflow cannot be recovered, so the vulnerable parse runs in a child
// process (this same test binary re-execed with reproEnv set). The parent
// asserts the child exits cleanly. Pre-fix the child dies with a non-zero exit
// and "stack exceeds" on stderr; post-fix it exits 0.
func TestSkipCorruptTx_ManyBadBlocks_NoStackOverflow(t *testing.T) {
	if os.Getenv(reproEnv) == "1" {
		runManyBadBlocksChild(t)

		return
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestSkipCorruptTx_ManyBadBlocks_NoStackOverflow$", "-test.v")

	cmd.Env = append(os.Environ(), reproEnv+"=1")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child crashed — recursive corrupt-tx recovery overflowed the stack:\n%s\nexit error: %v", out, err)
	}
}

// runManyBadBlocksChild is the child-process body: cap the stack low, then drain
// the crafted fixture through both reader paths under SkipCorruptTx. If the
// recovery path is recursive, either drain overflows the stack and aborts the
// process; if iterative, both return cleanly.
func runManyBadBlocksChild(t *testing.T) {
	t.Helper()

	debug.SetMaxStack(childMaxStack)

	data := craftManyBadBlocks(t, manyBadBlocksCount)

	// Streaming path (bufio-backed): loadNextTx ↔ readTxBlock.
	rs, err := reader.NewReader(bytes.NewReader(data), reader.SkipCorruptTx())
	require.NoError(t, err)
	drainIgnoringErr(rs)

	// In-memory path: loadNextTxBytes ↔ readTxBlockBytes.
	rb, err := reader.NewReaderBytes(data, reader.SkipCorruptTx())
	require.NoError(t, err)
	drainIgnoringErr(rb)
}

// drainIgnoringErr walks every row to end-of-stream, ignoring the (expected)
// corruption errors — the point is solely that the walk terminates without a
// fatal stack overflow.
func drainIgnoringErr(r *reader.Reader) {
	for range r.Rows() { //nolint:revive // intentional drain
	}
}

// craftManyBadBlocks builds a journal image with a valid meta header (borrowed
// from a real fixture) followed by n consecutive RowMarker blocks whose stated
// CRC32C never matches their payload, terminated by an EOFMarker. Each block is
// individually well-formed in shape but fails its checksum, so under
// SkipCorruptTx the reader resyncs from one to the next — the exact pattern that
// drives the recursive recovery path.
func craftManyBadBlocks(t *testing.T, n int) []byte {
	t.Helper()

	fixture := testutil.Load(t, "simple.xlog")

	metaEnd := bytes.Index(fixture, format.RowMarker[:])
	require.GreaterOrEqual(t, metaEnd, 0, "no RowMarker in fixture — cannot extract meta header")

	payload := []byte{0x00}
	// Guaranteed CRC mismatch: the real checksum XOR all-ones can never equal
	// the real checksum.
	badCRC := format.CRC32C(payload) ^ 0xFFFFFFFF

	var fh [format.FixheaderSize]byte

	header := format.Fixheader{
		Magic:  format.RowMarker,
		Len:    uint32(len(payload)),
		CRC32C: badCRC,
	}
	format.EncodeFixheader(&fh, &header)

	block := append(append([]byte{}, fh[:]...), payload...)

	out := make([]byte, 0, metaEnd+n*len(block)+format.MarkerSize)
	out = append(out, fixture[:metaEnd]...)

	for range n {
		out = append(out, block...)
	}

	out = append(out, format.EOFMarker[:]...)

	return out
}

package follow_test

// coverage_extra_test.go – additional black-box tests that drive the follow
// package through scenarios left uncovered by the existing suite.
//
// Targeted engine paths:
//   - adoptInprogressIdentity (0%)
//   - reswitchToFinalized (50%)
//   - onInprogressEOF (50%)
//   - findInprogressSuccessor (79%)
//   - runTx ErrTruncated path (72%)
//   - runTx cancellation path
//   - allDigits junk-filename branch (67%)
//   - startDir wait-for-first-file branch (77%)
//   - open chain-broken on inprogress open (79%)
//   - onWait inprogress→finalised rename race (75%)
//   - Follower io.EOF on finalised single file
//   - closeReader nil guard (second Close)

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/follow"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// newLiveWriterWithMeta creates a live (no-rename) writer at path using a
// custom meta, so callers can set a non-nil PrevVClock for chain validation.
func newLiveWriterWithMeta(t *testing.T, path string, meta *format.Meta) *liveWriter {
	t.Helper()

	f, err := os.Create(path)
	require.NoError(t, err)

	w, err := writer.NewWriter(f, meta)
	require.NoError(t, err)

	require.NoError(t, w.Sync())

	return &liveWriter{w: w, f: f}
}

// ─────────────────────────────────────────────────────────────────────────────
// runTx ErrTruncated (single-file, mid-tx at finalised EOF)
// ─────────────────────────────────────────────────────────────────────────────

// buildNonCommitFile creates a finalised xlog that contains:
//   - one complete committed tx (lsn=1)
//   - one non-commit row (lsn=2, TSN=2, IsCommit=false) as a standalone block
//   - EOF marker
//
// This exercises engine.runTx's ErrTruncated path: runTx accumulates the
// non-commit row then gets io.EOF (saw EOF marker) with pending rows.
func buildNonCommitFile(t *testing.T, path string) {
	t.Helper()

	f, err := os.Create(path)
	require.NoError(t, err)

	defer func() { require.NoError(t, f.Close()) }()

	w, err := writer.NewWriter(f, metaWith(format.VClock{1: 0}, nil))
	require.NoError(t, err)
	require.NoError(t, w.Sync())

	// Committed tx (lsn=1): normal committed row.
	require.NoError(t, w.WriteTx([]format.XRow{row(1)}))
	require.NoError(t, w.Sync())

	// Non-commit row (lsn=2): use WriteBlock which writes verbatim (no flag injection).
	// Set TSN=1 (≠ LSN=2) to signal it's the first row of a multi-row tx.
	// Leave Flags=0 (no IPROTO_FLAG_COMMIT) so IsCommit()=false.
	nonCommitRow := format.XRow{
		Type:      iproto.IPROTO_INSERT,
		ReplicaID: 1,
		LSN:       2,
		TSN:       1, // TSN < LSN → non-commit row
		Flags:     0, // no IPROTO_FLAG_COMMIT
		BodyRaw:   body(2),
	}
	require.NoError(t, w.WriteBlock([]format.XRow{nonCommitRow}))
	require.NoError(t, w.Sync())

	// Write the EOF marker to finalise the file (without the committing row).
	_, err = f.Write(format.EOFMarker[:])
	require.NoError(t, err)
}

// TestFileTx_TruncatedAtEOF verifies that a finalised file ending with a
// non-commit row yields reader.ErrTruncated from FileTx.
func TestFileTx_TruncatedAtEOF(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "trunc.xlog")
	buildNonCommitFile(t, path)

	ctx := t.Context()
	c := collectTxs(follow.FileTx(ctx, path, fastPoll()))
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.ErrorIs(t, err, reader.ErrTruncated)
	require.Equal(t, []int64{1}, lsns) // tx1 committed; tx2 is truncated
}

// ─────────────────────────────────────────────────────────────────────────────
// runTx cancellation path
// ─────────────────────────────────────────────────────────────────────────────

// TestFileTx_Cancellation cancels a FileTx follow mid-stream, exercising the
// ctx.Err() stopClean branch inside runTx.
func TestFileTx_Cancellation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "live.xlog")
	lw := newLiveWriter(t, path)
	lw.write(t, 1)

	ctx, cancel := context.WithCancel(context.Background())

	c := collectTxs(follow.FileTx(ctx, path, fastPoll()))
	c.waitCount(t, 1) // received tx 1

	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1}, lsns)
}

// ─────────────────────────────────────────────────────────────────────────────
// allDigits: junk filenames alongside valid chain files
// ─────────────────────────────────────────────────────────────────────────────

// TestDir_JunkFilesIgnored places non-digit-named and wrong-extension files
// in the directory. The follower must ignore them and only read valid chain
// files, exercising the allDigits false branch in findInprogressSuccessor.
func TestDir_JunkFilesIgnored(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)
	writeChainFile(t, dirPath, format.VClock{1: 2}, format.VClock{1: 1}, 2)

	// Junk files that must be ignored by findInprogressSuccessor.
	junkNames := []string{
		"notdigits.xlog.inprogress", // stem is "notdigits" → allDigits=false
		"1abc.xlog.inprogress",      // stem has a letter → allDigits=false
		".xlog.inprogress",          // empty stem → allDigits=false
		"3.xlog.inprogress.extra",   // wrong suffix (not exactly ".xlog.inprogress")
		"9999.snap.inprogress",      // wrong extension (snap ≠ xlog)
	}

	for _, name := range junkNames {
		require.NoError(t, os.WriteFile(filepath.Join(dirPath, name), []byte("junk"), 0o600))
	}

	// A subdirectory must be skipped too (IsDir branch).
	require.NoError(t, os.Mkdir(filepath.Join(dirPath, "subdir"), 0o755))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := collectRows(follow.Dir(ctx, dirPath, format.FiletypeXLOG,
		follow.WithFromHead(), follow.WithReadInprogress(), fastPoll()))

	c.waitCount(t, 2)
	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, lsns)
}

// TestDir_InprogressWithJunkNeighbors places junk .inprogress files alongside
// a valid one. The follower must pick the valid inprogress successor and ignore
// the junk, hitting the allDigits false branch.
func TestDir_InprogressWithJunkNeighbors(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	// head: vclock={1:1}, prev=nil
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)

	// Valid inprogress successor: PrevVClock must match curVClock={1:1}.
	ipPath := filepath.Join(dirPath, "2.xlog.inprogress")
	lw := newLiveWriterWithMeta(t, ipPath,
		metaWith(format.VClock{1: 2}, format.VClock{1: 1}))
	lw.write(t, 2)

	// Junk inprogress files — must be ignored (allDigits=false).
	require.NoError(t, os.WriteFile(
		filepath.Join(dirPath, "abc.xlog.inprogress"), []byte("junk"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(dirPath, "0x3.xlog.inprogress"), []byte("junk"), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := collectRows(follow.Dir(ctx, dirPath, format.FiletypeXLOG,
		follow.WithFromHead(), follow.WithReadInprogress(), fastPoll()))

	c.waitCount(t, 2) // row 1 from head + row 2 from .inprogress
	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, lsns)

	lw.finalize(t)
}

// ─────────────────────────────────────────────────────────────────────────────
// startDir: wait for first file (empty directory)
// ─────────────────────────────────────────────────────────────────────────────

// TestDir_WaitForFirstFile starts a directory follow on an empty directory,
// exercising the "empty directory — wait" branch in startDir.
func TestDir_WaitForFirstFile(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := collectRows(follow.Dir(ctx, dirPath, format.FiletypeXLOG,
		follow.WithFromHead(), fastPoll()))

	// Nothing yet — follower is waiting.
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, 0, c.count())

	// Drop the first file.
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)

	c.waitCount(t, 1)
	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1}, lsns)
}

// ─────────────────────────────────────────────────────────────────────────────
// open: chain-broken on inprogress open (ErrChainBroken from engine.open)
// ─────────────────────────────────────────────────────────────────────────────

// TestDir_InprogressChainBroken exercises the error path in engine.open where
// an .inprogress file's PrevVClock does not match the expected chain vclock.
func TestDir_InprogressChainBroken(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	// head: sig=1, vclock={1:1}, prev=nil
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)

	// Create an .inprogress file with wrong PrevVClock:
	// head's vclock is {1:1} but this inprogress claims prev={1:99}.
	ipPath := filepath.Join(dirPath, "2.xlog.inprogress")
	f, err := os.Create(ipPath)
	require.NoError(t, err)

	badMeta := metaWith(format.VClock{1: 2}, format.VClock{1: 99}) // wrong prev
	w, err := writer.NewWriter(f, badMeta)
	require.NoError(t, err)
	require.NoError(t, w.WriteTx([]format.XRow{row(2)}))
	require.NoError(t, w.Sync())
	require.NoError(t, f.Close())

	ctx := t.Context()

	c := collectRows(follow.Dir(ctx, dirPath, format.FiletypeXLOG,
		follow.WithFromHead(), follow.WithReadInprogress(), fastPoll()))
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.ErrorIs(t, err, follow.ErrChainBroken)
	require.Equal(t, []int64{1}, lsns)
}

// ─────────────────────────────────────────────────────────────────────────────
// onInprogressEOF with finalized=true (EOF marker in .inprogress path)
// → adoptInprogressIdentity + advance
// ─────────────────────────────────────────────────────────────────────────────

// TestDir_InprogressFinalizedWithEOFMarker exercises onInprogressEOF when the
// .inprogress file has been finalised in-place (EOF marker written, file NOT
// yet renamed). The engine sees SawEOFMarker=true while isInprogress=true,
// calls adoptInprogressIdentity and then advances to the next file.
func TestDir_InprogressFinalizedWithEOFMarker(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	// head: vclock={1:1}, prev=nil
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)

	// Write .inprogress file with PrevVClock={1:1} to chain onto the head.
	// VClock={1:2} is what adoptInprogressIdentity will copy into curVClock.
	ipPath := filepath.Join(dirPath, "2.xlog.inprogress")
	lw := newLiveWriterWithMeta(t, ipPath,
		metaWith(format.VClock{1: 2}, format.VClock{1: 1}))
	lw.write(t, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := collectRows(follow.Dir(ctx, dirPath, format.FiletypeXLOG,
		follow.WithFromHead(), follow.WithReadInprogress(), fastPoll()))

	c.waitCount(t, 2) // rows 1 (head) + 2 (.inprogress)

	// Finalize the .inprogress in-place: write EOF marker without renaming.
	// The follower re-reads and sees SawEOFMarker=true while isInprogress=true
	// → onInprogressEOF(finalized=true) → adoptInprogressIdentity.
	lw.finalize(t)

	// After adoptInprogressIdentity, curVClock={1:2} (ipVClock from the meta).
	// The successor must chain onto {1:2}. Publish it atomically: a live
	// WithReadInprogress follower would otherwise trip on the transient partial
	// "3.xlog.inprogress" that writer.Create exposes mid-creation.
	writeChainFileAtomic(t, dirPath, format.VClock{1: 3}, format.VClock{1: 2}, 3)

	c.waitCount(t, 3)
	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2, 3}, lsns)
}

// writeChainFileAtomic publishes a finalised <sig>.xlog the way writeChainFile
// does, but ATOMICALLY: it builds the complete file (header + tx + EOF marker)
// under a name the directory index and the .inprogress scanner both ignore
// ("<sig>.xlog.building"), then renames it into place. writeChainFile uses
// writer.Create, which exposes "<sig>.xlog.inprogress" the instant it is
// created — before the header is flushed — and a live WithReadInprogress
// follower can open that partial header and fail fatally. Use this whenever a
// successor file is created while such a follower is already running.
func writeChainFileAtomic(t *testing.T, dirPath string, vclock, prev format.VClock, lsn int64) {
	t.Helper()

	finalPath := filepath.Join(dirPath, strconv.FormatInt(vclock.Signature(), 10)+".xlog")
	buildPath := finalPath + ".building"

	f, err := os.Create(buildPath)
	require.NoError(t, err)

	w, err := writer.NewWriter(f, metaWith(vclock, prev))
	require.NoError(t, err)

	require.NoError(t, w.WriteTx([]format.XRow{row(lsn)}))
	require.NoError(t, w.Close()) // EOF marker + flush
	require.NoError(t, f.Close())
	require.NoError(t, os.Rename(buildPath, finalPath)) // atomic: never seen partial

	// Note on reswitchToFinalized: the engine's "in-progress file was renamed to
	// its final name while we tailed it" branch cannot be driven deterministically
	// through the public polling API. After an in-progress EOF with no marker the
	// engine checks reswitch ONCE, then waits and re-opens the SAME .inprogress
	// path; if an external rename lands in that window the re-open fails fatally
	// rather than re-resolving. Any test that renames a tailed .inprogress is
	// therefore inherently flaky, so adoptInprogressIdentity is covered instead
	// via the deterministic in-place-EOF-marker path (TestDir_InprogressFinalizedWithEOFMarker).
}

// ─────────────────────────────────────────────────────────────────────────────
// Follower: io.EOF on a finalised single-file follow
// ─────────────────────────────────────────────────────────────────────────────

// TestFileFollower_IOEOFOnFinalized verifies that NewFileFollower.Next returns
// io.EOF after consuming all rows of a finalised file.
func TestFileFollower_IOEOFOnFinalized(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "final.xlog")
	lw := newLiveWriter(t, path)
	lw.write(t, 1)
	lw.write(t, 2)
	lw.finalize(t)

	f := follow.NewFileFollower(path, fastPoll())

	defer func() { _ = f.Close() }()

	ctx := t.Context()

	r1, err := f.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), r1.LSN)

	r2, err := f.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), r2.LSN)

	// Third Next must return io.EOF (finalized single-file follow).
	_, err = f.Next(ctx)
	require.ErrorIs(t, err, io.EOF)
}

// ─────────────────────────────────────────────────────────────────────────────
// closeReader: nil guard (second Close is a no-op)
// ─────────────────────────────────────────────────────────────────────────────

// TestFollower_CloseIdempotent calls Close twice, exercising the nil guard
// in closeReader.
func TestFollower_CloseIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "final.xlog")
	lw := newLiveWriter(t, path)
	lw.write(t, 1)
	lw.finalize(t)

	f := follow.NewFileFollower(path, fastPoll())

	ctx := t.Context()
	_, err := f.Next(ctx)
	require.NoError(t, err)

	require.NoError(t, f.Close())
	require.NoError(t, f.Close()) // second Close must be a no-op
}

// ─────────────────────────────────────────────────────────────────────────────
// DirTx cancellation (runTx stopClean via ctx)
// ─────────────────────────────────────────────────────────────────────────────

// TestDirTx_Cancellation cancels a DirTx follow, exercising the ctx stopClean
// branch inside runTx.
func TestDirTx_Cancellation(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)
	writeChainFile(t, dirPath, format.VClock{1: 2}, format.VClock{1: 1}, 2)

	ctx, cancel := context.WithCancel(context.Background())

	c := collectTxs(follow.DirTx(ctx, dirPath, format.FiletypeXLOG,
		follow.WithFromHead(), fastPoll()))
	c.waitCount(t, 2)

	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, lsns)
}

// ─────────────────────────────────────────────────────────────────────────────
// startDir: startNone via DirTx → ErrNoStart
// ─────────────────────────────────────────────────────────────────────────────

// TestDir_StartNoneViaDirTx verifies that DirTx without a start option also
// returns ErrNoStart (startNone branch in startDir, reached via runTx).
func TestDir_StartNoneViaDirTx(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)

	c := collectTxs(follow.DirTx(t.Context(), dirPath, format.FiletypeXLOG, fastPoll()))
	c.waitDone(t)

	_, err := c.snapshot()
	require.ErrorIs(t, err, follow.ErrNoStart)
}

// ─────────────────────────────────────────────────────────────────────────────
// findInprogressSuccessor: sig <= curSig (stale inprogress ignored)
// ─────────────────────────────────────────────────────────────────────────────

// TestDir_InprogressLowerSigIgnored places a stale .inprogress file with a
// signature ≤ the current file's signature. It must be ignored by
// findInprogressSuccessor (sig <= curSig branch).
func TestDir_InprogressLowerSigIgnored(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)
	writeChainFile(t, dirPath, format.VClock{1: 2}, format.VClock{1: 1}, 2)

	// Stale .inprogress with sig=1 (≤ curSig=2 after reading both files).
	stale := filepath.Join(dirPath, "1.xlog.inprogress")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := collectRows(follow.Dir(ctx, dirPath, format.FiletypeXLOG,
		follow.WithFromHead(), follow.WithReadInprogress(), fastPoll()))

	c.waitCount(t, 2)
	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, lsns)
}

// ─────────────────────────────────────────────────────────────────────────────
// WithStartLSN resolution in startDir
// ─────────────────────────────────────────────────────────────────────────────

// TestDir_StartLSN_Resolution verifies WithStartLSN picks the correct start
// file and continues across rotation, exercising the startLSN branch of startDir.
func TestDir_StartLSN_Resolution(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)
	writeChainFile(t, dirPath, format.VClock{1: 2}, format.VClock{1: 1}, 2)
	writeChainFile(t, dirPath, format.VClock{1: 3}, format.VClock{1: 2}, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// WithStartLSN(1, 2) → LocateLSN picks file with VClock[1]=2.
	c := collectRows(follow.Dir(ctx, dirPath, format.FiletypeXLOG,
		follow.WithStartLSN(1, 2), fastPoll()))

	c.waitCount(t, 2)
	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Equal(t, []int64{2, 3}, lsns)
}

// ─────────────────────────────────────────────────────────────────────────────
// Dir follow: context cancelled while waiting for startDir (empty dir)
// ─────────────────────────────────────────────────────────────────────────────

// TestDir_CancelDuringWaitForFirst cancels a follow while the engine is
// blocked in startDir waiting for the first file in an empty directory.
func TestDir_CancelDuringWaitForFirst(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())

	c := collectRows(follow.Dir(ctx, dirPath, format.FiletypeXLOG,
		follow.WithFromHead(), fastPoll()))

	// Give the follower time to enter the wait loop.
	time.Sleep(20 * time.Millisecond)

	cancel()
	c.waitDone(t)

	lsns, err := c.snapshot()
	require.NoError(t, err)
	require.Empty(t, lsns)
}

// ─────────────────────────────────────────────────────────────────────────────
// Follower dir: across rotation with Offset checkpointing
// ─────────────────────────────────────────────────────────────────────────────

// TestDirFollower_OffsetAndRotation drives a NewDirFollower across two files,
// verifying Offset() and Next() work correctly.
func TestDirFollower_OffsetAndRotation(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()
	writeChainFile(t, dirPath, format.VClock{1: 1}, nil, 1)
	writeChainFile(t, dirPath, format.VClock{1: 2}, format.VClock{1: 1}, 2)
	writeChainFile(t, dirPath, format.VClock{1: 3}, format.VClock{1: 2}, 3)

	f := follow.NewDirFollower(dirPath, format.FiletypeXLOG,
		follow.WithFromHead(), fastPoll())

	defer func() { _ = f.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, wantLSN := range []int64{1, 2, 3} {
		r, err := f.Next(ctx)
		require.NoError(t, err)
		require.Equal(t, wantLSN, r.LSN)
		require.GreaterOrEqual(t, f.Offset(), int64(0))
	}
}

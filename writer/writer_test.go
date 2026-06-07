package writer //nolint:testpackage // white-box: tests unexported behavior and defines shared helper newMeta

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

// newMeta is a small test helper.
func newMeta(t *testing.T) *format.Meta {
	t.Helper()

	return &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "go-xlog/test",
		InstanceUUID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		VClock:       format.VClock{1: 0},
	}
}

// TestInProgressLifecycle — file exists as `.inprogress` while writer
// open; final name appears only after Close(); the .inprogress is gone after.
func TestInProgressLifecycle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "00000000000000000001.xlog")
	inprog := path + ".inprogress"

	w, err := Create(path, newMeta(t))
	require.NoError(t, err)

	// .inprogress exists; final does not.
	_, err = os.Stat(inprog)
	require.NoError(t, err, "inprogress should exist while writer open")
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "final path should NOT exist while writer open: %v", err)

	// Write one row so the file has content.
	row := format.XRow{
		Type:    iproto.IPROTO_INSERT,
		LSN:     1,
		BodyRaw: encodeDMLBody([]uint64{1, 42}),
	}
	require.NoError(t, w.WriteTx([]format.XRow{row}))

	require.NoError(t, w.Close())

	// After Close: final exists; inprogress does not.
	_, err = os.Stat(path)
	require.NoError(t, err, "final path should exist after Close")
	_, err = os.Stat(inprog)
	require.True(t, os.IsNotExist(err), "inprogress should NOT exist after Close: stat err=%v", err)
}

// TestDiscardRemovesInprogress — Discard cleans up the temp file and never
// promotes to final.
func TestDiscardRemovesInprogress(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "00000000000000000002.xlog")
	inprog := path + ".inprogress"

	w, err := Create(path, newMeta(t))
	require.NoError(t, err)
	require.NoError(t, w.Discard())

	_, err = os.Stat(inprog)
	require.True(t, os.IsNotExist(err), "inprogress should be gone after Discard: %v", err)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "final should not exist after Discard: %v", err)
	// Further calls return ErrClosed.
	require.ErrorIs(t, w.WriteTx([]format.XRow{{Type: iproto.IPROTO_NOP, LSN: 1}}), ErrClosed, "WriteTx after Discard")
	require.ErrorIs(t, w.Close(), ErrClosed, "Close after Discard")
}

// TestCreateRejectsExistingInprogress — O_EXCL semantics.
func TestCreateRejectsExistingInprogress(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "x.xlog")
	require.NoError(t, os.WriteFile(path+".inprogress", []byte("squat"), 0o644))
	_, err := Create(path, newMeta(t))
	require.Error(t, err, "Create over existing .inprogress should fail")
}

// TestWriteAfterClose — operations after Close return ErrClosed.
func TestWriteAfterClose(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "x.xlog")
	w, err := Create(path, newMeta(t))
	require.NoError(t, err)
	require.NoError(t, w.WriteTx([]format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: encodeDMLBody([]uint64{1})},
	}))
	require.NoError(t, w.Close())
	require.ErrorIs(t, w.WriteRow(format.XRow{Type: iproto.IPROTO_NOP, LSN: 99}), ErrClosed, "WriteRow after Close")
	require.ErrorIs(t, w.CommitTx(), ErrClosed, "CommitTx after Close")
	require.ErrorIs(t, w.Close(), ErrClosed, "Close again")
}

// TestVersionOptionPopulatesMeta — Version() option sets Meta.Version when
// caller-supplied meta has it blank.
func TestVersionOptionPopulatesMeta(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "x.xlog")
	m := &format.Meta{
		Filetype:     format.FiletypeXLOG,
		InstanceUUID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
	}
	w, err := Create(path, m, Version("custom-version/1.0"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Contains(raw, []byte("Version: custom-version/1.0\n")), "file does not contain custom Version line:\n%s", raw)
}

// TestWriteTxRejectsPendingWriteRow — WriteTx is an alternate entry point; if
// there is a pending tx from WriteRow, WriteTx errors out.
func TestWriteTxRejectsPendingWriteRow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "x.xlog")
	w, err := Create(path, newMeta(t))
	require.NoError(t, err)

	defer func() { _ = w.Discard() }()

	require.NoError(t, w.WriteRow(format.XRow{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: encodeDMLBody([]uint64{1})}))
	err = w.WriteTx([]format.XRow{{Type: iproto.IPROTO_INSERT, LSN: 2, BodyRaw: encodeDMLBody([]uint64{2})}})
	require.Error(t, err, "WriteTx with pending row should error")
}

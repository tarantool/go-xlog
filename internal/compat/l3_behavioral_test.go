package compat //nolint:testpackage // white-box: consumes internal test helper corpusRoot from harness_test.go (via chainDir)

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/dir"
	"github.com/tarantool/go-xlog/filter"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/pipe"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/rotate"
	"github.com/tarantool/go-xlog/tools"
	"github.com/tarantool/go-xlog/writer"
)

// chainVersion is a modern version whose corpus dir holds a >=2 file xlog
// chain plus >=2 snaps — used by the directory-level tests.
const chainVersion = "2.11"

func chainDir(tb testing.TB) string {
	tb.Helper()

	return filepath.Join(corpusRoot(tb), chainVersion)
}

// readRows opens a fixture and returns its rows with bodies cloned (the
// reader aliases a scratch buffer across Next).
func readRows(t *testing.T, path string) []format.XRow {
	t.Helper()

	r, err := reader.Open(path)
	require.NoErrorf(t, err, "open %s", path)

	defer func() { _ = r.Close() }()

	var out []format.XRow

	for row, err := range r.Rows() {
		require.NoErrorf(t, err, "rows %s", path)

		cp := row
		cp.BodyRaw = append([]byte(nil), row.BodyRaw...)
		out = append(out, cp)
	}

	return out
}

// TestDir_HistoricalChain opens a real multi-file historical directory and
// asserts the vclock chain loads (filename signature must equal the in-meta VClock signature)
// and locates a known LSN.
func TestDir_HistoricalChain(t *testing.T) {
	t.Parallel()

	for _, ft := range []format.Filetype{format.FiletypeXLOG, format.FiletypeSNAP} {
		d, err := dir.OpenDir(chainDir(t), ft)
		require.NoErrorf(t, err, "OpenDir %s", ft)

		files := d.Files()
		require.GreaterOrEqualf(t, len(files), 2, "%s: want >=2 files", ft)

		for i := 1; i < len(files); i++ {
			assert.Greaterf(t, files[i].Signature, files[i-1].Signature,
				"%s: signatures not ascending at %d", ft, i)
		}

		if ft == format.FiletypeXLOG {
			e, err := d.LocateLSN(1, 5)
			require.NoError(t, err, "LocateLSN(1,5)")
			assert.Equalf(t, int64(0), e.Signature, "LocateLSN(1,5): want the first xlog (0)")
		}
	}
}

// TestPipe_SemanticRoundTrip copies a historical xlog through reader→pipe→writer
// and asserts the rows survive (type/lsn/body); the filtered variant keeps only
// insert-bearing transactions.
func TestPipe_SemanticRoundTrip(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(chainDir(t), "00000000000000000000.xlog")
	want := readRows(t, srcPath)

	t.Run("full", func(t *testing.T) {
		t.Parallel()

		got := pipeCopy(t, srcPath)
		require.Lenf(t, got, len(want), "row count")

		for i := range want {
			if got[i].Type != want[i].Type || got[i].LSN != want[i].LSN {
				t.Errorf("row[%d]: got (type=%d lsn=%d), want (type=%d lsn=%d)",
					i, got[i].Type, got[i].LSN, want[i].Type, want[i].LSN)
			}

			assert.Truef(t, bytes.Equal(got[i].BodyRaw, want[i].BodyRaw), "row[%d]: body mismatch", i)
		}
	})

	t.Run("filter_inserts", func(t *testing.T) {
		t.Parallel()

		got := pipeCopy(t, srcPath, filter.Types(iproto.IPROTO_INSERT))
		require.NotEmpty(t, got, "filtered copy produced no rows")

		for i, r := range got {
			assert.Equalf(t, iproto.IPROTO_INSERT, r.Type, "row[%d]: want INSERT (filtered txs are insert-only in this corpus)", i)
		}
	})
}

// pipeCopy streams srcPath through pipe.Copy into a temp xlog and returns the
// re-read rows.
func pipeCopy(t *testing.T, srcPath string, fs ...filter.Filter) []format.XRow {
	t.Helper()

	src, err := reader.Open(srcPath)
	require.NoError(t, err, "open src")

	defer func() { _ = src.Close() }()

	dstPath := filepath.Join(t.TempDir(), "00000000000000000000.xlog")
	dst, err := writer.Create(dstPath, src.Meta().Clone())
	require.NoError(t, err, "create dst")
	_, err = pipe.Copy(src, dst, fs...)
	require.NoError(t, err, "pipe.Copy")
	require.NoError(t, dst.Close(), "close dst")

	return readRows(t, dstPath)
}

// TestRewriteMeta_VerbatimTx rewrites a historical file's instance UUID and
// asserts the meta changed while every tx byte after the header is copied
// verbatim.
func TestRewriteMeta_VerbatimTx(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(chainDir(t), "00000000000000000000.xlog")
	dstPath := filepath.Join(t.TempDir(), "rewritten.xlog")
	newUUID := uuid.MustParse("00000000-0000-0000-0000-0000deadbeef")

	srcMeta := mustMeta(t, srcPath)
	require.NotEqual(t, newUUID, srcMeta.InstanceUUID, "precondition: source already has the target UUID")

	err := tools.RewriteMeta(srcPath, dstPath, func(m *format.Meta) *format.Meta {
		m.InstanceUUID = newUUID

		return m
	})
	require.NoError(t, err, "RewriteMeta")

	assert.Equal(t, newUUID, mustMeta(t, dstPath).InstanceUUID, "dst InstanceUUID")
	assert.Truef(t, bytes.Equal(txBytes(t, srcPath), txBytes(t, dstPath)),
		"tx bytes after the meta header are not byte-identical (RewriteMeta must copy them verbatim)")
}

// TestRotate_HistoricalRows feeds historical insert bodies into a rotating
// writer with a small size cap and asserts the resulting directory is a valid
// >=2 file chain.
func TestRotate_HistoricalRows(t *testing.T) {
	t.Parallel()

	src := readRows(t, filepath.Join(chainDir(t), "00000000000000000000.xlog"))

	var bodies [][]byte

	for _, r := range src {
		if r.Type == iproto.IPROTO_INSERT {
			bodies = append(bodies, r.BodyRaw)
		}
	}

	require.GreaterOrEqualf(t, len(bodies), 3, "need >=3 insert bodies, got %d", len(bodies))

	outDir := t.TempDir()
	rw, err := rotate.New(outDir, format.FiletypeXLOG,
		uuid.MustParse("00000000-0000-0000-0000-000000000001"), format.VClock{1: 0},
		rotate.MaxFileSize(256)) // Small cap; the 4 KiB insert forces a rotation.
	require.NoError(t, err, "rotate.New")

	for i, body := range bodies {
		row := format.XRow{Type: iproto.IPROTO_INSERT, ReplicaID: 1, LSN: int64(i + 1), BodyRaw: body}
		require.NoErrorf(t, rw.WriteTx([]format.XRow{row}), "WriteTx %d", i)
	}

	require.NoError(t, rw.Close(), "rotate.Close")

	d, err := dir.OpenDir(outDir, format.FiletypeXLOG)
	require.NoError(t, err, "OpenDir rotated")
	require.GreaterOrEqualf(t, len(d.Files()), 2, "rotation produced %d files, want >=2", len(d.Files()))
}

func mustMeta(t *testing.T, path string) *format.Meta {
	t.Helper()

	r, err := reader.Open(path)
	require.NoErrorf(t, err, "open %s", path)

	defer func() { _ = r.Close() }()

	return r.Meta().Clone()
}

// txBytes returns the file bytes after the meta header (the blank-line
// terminator), i.e. the tx blocks + EOF marker.
func txBytes(t *testing.T, path string) []byte {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoErrorf(t, err, "read %s", path)

	idx := bytes.Index(raw, []byte("\n\n"))
	require.GreaterOrEqualf(t, idx, 0, "%s: meta terminator not found", path)

	return raw[idx+2:]
}

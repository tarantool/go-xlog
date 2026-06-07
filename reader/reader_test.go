package reader_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/reader"
)

// Open is exercised in integration_test.go; here we cover the
// io.ReadSeeker path so we have construction errors without filesystem
// dependencies.

func TestNewReader_DecodesMeta_Xlog(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "simple.xlog")
	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	m := r.Meta()
	require.NotNil(t, m, "Meta() returned nil")
	assert.Equal(t, format.FiletypeXLOG, m.Filetype)
	assert.Equal(t, format.FormatVersion, m.FormatVer)
}

func TestNewReader_DecodesMeta_Snap(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "populated.snap")
	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	assert.Equal(t, format.FiletypeSNAP, r.Meta().Filetype)
}

func TestNewReader_DecodesMeta_VyLog(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "vylog_sample.vylog")
	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	assert.Equal(t, format.FiletypeVYLOG, r.Meta().Filetype)
}

func TestClose_NewReader_IsNoOp(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "simple.xlog")
	r, err := reader.NewReader(bytes.NewReader(data))
	require.NoError(t, err)
	// NewReader does not own the stream — Close is a no-op for I/O and
	// must be safe to call (even twice).
	assert.NoError(t, r.Close(), "first Close")
	assert.NoError(t, r.Close(), "second Close")
}

func TestOpen_OwnsAndClosesFile(t *testing.T) {
	t.Parallel()

	path := testutil.Path(t, "simple.xlog")
	r, err := reader.Open(path)
	require.NoError(t, err)
	assert.Equal(t, format.FiletypeXLOG, r.Meta().Filetype)
	assert.NoError(t, r.Close(), "Close")
	// Second Close should not error.
	assert.NoError(t, r.Close(), "second Close")
}

func TestNewReader_RejectsTruncatedMeta(t *testing.T) {
	t.Parallel()
	// Chop the meta header so DecodeMeta cannot find the blank-line
	// terminator. We expect the format-layer ErrMetaTruncated to
	// propagate (wrapped) up through NewReader.
	data := testutil.Load(t, "simple.xlog")
	// Find the meta terminator and cut before it.
	end := bytes.Index(data, []byte("\n\n"))
	require.GreaterOrEqual(t, end, 0, "could not find meta terminator in fixture")
	truncated := data[:end] // No blank line at all.
	_, err := reader.NewReader(bytes.NewReader(truncated))
	require.Error(t, err, "expected error, got nil")
	assert.ErrorIs(t, err, format.ErrMetaTruncated)
}

func TestNewReader_RejectsBadVersion_AcceptsWithOption(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, "simple.xlog")
	// Patch "0.13\n" -> "0.12\n" on the second line.
	munged := bytes.Replace(data, []byte("\n"+format.FormatVersion+"\n"), []byte("\n0.12\n"), 1)
	require.NotEqual(t, data, munged, "munge did not find the version line")
	// Strict: rejected.
	_, err := reader.NewReader(bytes.NewReader(munged))
	require.ErrorIs(t, err, format.ErrMetaBadVersion, "strict NewReader")
	// With AcceptV012: ok.
	r, err := reader.NewReader(bytes.NewReader(munged), reader.AcceptV012())
	require.NoError(t, err)
	assert.Equal(t, format.LegacyFormatVersion, r.Meta().FormatVer)
}

// drainRows exhausts r.Rows() and returns the number of rows and the
// terminating error (nil for clean EOF, non-nil for any other reason).
func drainRows(t *testing.T, r *reader.Reader) (int, error) {
	t.Helper()

	n := 0

	var lastErr error

	for _, err := range r.Rows() {
		if err != nil {
			lastErr = err

			break
		}

		n++
	}
	// If we exited via clean EOF the iterator does not yield it; lastErr
	// stays nil and the caller distinguishes by row count + io.EOF
	// (which never appears in the iterator).
	if lastErr != nil {
		return n, lastErr
	}
	// Confirm a follow-up Next returns io.EOF (no more rows hidden).
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("post-iteration Next = %v, want io.EOF", err)
	}

	return n, nil
}

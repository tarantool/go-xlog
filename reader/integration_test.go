package reader_test

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/internal/testutil"
	"github.com/tarantool/go-xlog/reader"
)

// TestOpen_AllFixtures exercises the path-shaped Open constructor over
// every fixture in testdata/, asserting per-fixture row counts that
// were captured from the fixture_test logs.
func TestOpen_AllFixtures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		fixture  string
		filetype format.Filetype
		wantRows int
	}{
		{"simple.xlog", format.FiletypeXLOG, 12},
		{"multistmt.xlog", format.FiletypeXLOG, 12},
		{"compressed.xlog", format.FiletypeXLOG, 12},
		{"empty.snap", format.FiletypeSNAP, 391},
		{"populated.snap", format.FiletypeSNAP, 402},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			path := testutil.Path(t, tc.fixture)
			r, err := reader.Open(path)
			require.NoError(t, err)

			defer func() { _ = r.Close() }()

			assert.Equal(t, tc.filetype, r.Meta().Filetype)

			n := 0

			for row, err := range r.Rows() {
				require.NoError(t, err, "Rows error")
				require.NotNil(t, row, "nil row with nil error")

				n++
			}

			assert.Equal(t, tc.wantRows, n, "row count")
			// Idempotent EOF.
			_, err = r.Next()
			assert.ErrorIs(t, err, io.EOF, "post-drain Next")
		})
	}
}

// TestVyLog_DecodeFirstBody confirms vylog rows decode through the
// reader and that the typed body decoder (format.DecodeVyLogBody)
// accepts the first row's BodyRaw.
func TestVyLog_DecodeFirstBody(t *testing.T) {
	t.Parallel()

	path := testutil.Path(t, "vylog_sample.vylog")
	r, err := reader.Open(path)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	assert.Equal(t, format.FiletypeVYLOG, r.Meta().Filetype)
	row, err := r.Next()
	require.NoError(t, err)
	require.NotEmpty(t, row.BodyRaw, "first vylog row has empty BodyRaw: %+v", row)
	body, err := format.DecodeVyLogBody(row.BodyRaw)
	require.NoError(t, err)
	require.NotNil(t, body, "DecodeVyLogBody returned nil body with nil error")
	// Drain the rest; the file must terminate cleanly.
	for row, err := range r.Rows() {
		require.NoError(t, err, "Rows error")
		require.NotNil(t, row, "nil row, nil error")
	}
}

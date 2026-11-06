package reader_test

import (
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

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

// TestSynchro_DecodeRealRows decodes the synchronous replication rows a real
// Tarantool wrote (scripts/gen-synchro.lua): PROMOTE/DEMOTE carry their term
// under IPROTO_TERM and the limbo checkpoint in a snapshot carries the
// confirmed vclock under IPROTO_VCLOCK.
func TestSynchro_DecodeRealRows(t *testing.T) {
	t.Parallel()

	cases := []struct {
		fixture string
		want    []string
	}{
		{"synchro/00000000000000000000.xlog", []string{
			"IPROTO_RAFT_PROMOTE lsn=1 owner=0 lsn=0 term=2 vclock=map[]",
			"IPROTO_RAFT_CONFIRM lsn=5 owner=1 lsn=4 term=0 vclock=map[]",
			"IPROTO_RAFT_ROLLBACK lsn=7 owner=1 lsn=6 term=0 vclock=map[]",
			"IPROTO_RAFT_CONFIRM lsn=9 owner=1 lsn=8 term=0 vclock=map[]",
		}},
		{"synchro/00000000000000000010.xlog", []string{
			"IPROTO_RAFT_DEMOTE lsn=10 owner=1 lsn=8 term=3 vclock=map[]",
			"IPROTO_RAFT_PROMOTE lsn=11 owner=0 lsn=0 term=4 vclock=map[]",
			"IPROTO_RAFT_CONFIRM lsn=13 owner=1 lsn=12 term=0 vclock=map[]",
		}},
		{"synchro/00000000000000000010.snap", []string{
			"IPROTO_RAFT_PROMOTE lsn=394 owner=1 lsn=8 term=2 vclock=map[1:8]",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			r, err := reader.Open(testutil.Path(t, tc.fixture))
			require.NoError(t, err)

			defer func() { _ = r.Close() }()

			var got []string

			for row, err := range r.Rows() {
				require.NoError(t, err)

				switch row.Type {
				case iproto.IPROTO_RAFT_CONFIRM, iproto.IPROTO_RAFT_ROLLBACK,
					iproto.IPROTO_RAFT_PROMOTE, iproto.IPROTO_RAFT_DEMOTE:
				default:
					continue
				}

				body, err := format.DecodeSynchroBody(row.BodyRaw)
				require.NoError(t, err)
				assert.Empty(t, body.Extras, "unrecognised synchro keys")
				got = append(got, fmt.Sprintf("%s lsn=%d owner=%d lsn=%d term=%d vclock=%v",
					row.Type, row.LSN, body.ReplicaID, body.LSN, body.Term, map[uint32]int64(body.ConfirmedVClock)))
			}

			assert.Equal(t, tc.want, got)
		})
	}
}

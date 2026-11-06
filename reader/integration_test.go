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

// TestVyLog_DecodeRealRecords decodes every record of a vinyl log a real
// Tarantool wrote on creating a vinyl space and dumping it: the record type
// and the fields come from the [type, {key: value}] tuple of the row.
func TestVyLog_DecodeRealRecords(t *testing.T) {
	t.Parallel()

	r, err := reader.Open(testutil.Path(t, "vylog_sample.vylog"))
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	assert.Equal(t, format.FiletypeVYLOG, r.Meta().Filetype)

	var got []string

	for row, err := range r.Rows() {
		require.NoError(t, err)

		body, err := format.DecodeVyLogBody(row.BodyRaw)
		require.NoError(t, err)

		keys := make([]string, 0, len(body.Keys))
		for _, key := range []format.VyKey{
			format.VyKeyLSMID, format.VyKeyRangeID, format.VyKeyRunID,
			format.VyKeySpaceID, format.VyKeyDef, format.VyKeySliceID,
			format.VyKeyDumpLSN, format.VyKeyCreateLSN, format.VyKeyDumpCount,
		} {
			if raw, ok := body.Keys[key]; ok {
				keys = append(keys, fmt.Sprintf("%s=%x", key, raw))
			}
		}

		got = append(got, fmt.Sprintf("%s %v", body.Type, keys))
	}

	assert.Equal(t, []string{
		"SNAPSHOT []",
		"PREPARE_LSM [SPACE_ID=cd0201 DEF=9183a56669656c6400a474797065a8756e7369676e6564aa736f72745f6f72646572a3617363]",
		"INSERT_RANGE [RANGE_ID=01]",
		"CREATE_LSM [CREATE_LSN=04]",
		"PREPARE_RUN [RUN_ID=02]",
		"CREATE_RUN [RUN_ID=02 DUMP_LSN=0c DUMP_COUNT=01]",
		"INSERT_SLICE [RANGE_ID=01 RUN_ID=02 SLICE_ID=03]",
		"DUMP_LSM [DUMP_LSN=0c]",
	}, got)
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

				var unknown map[uint64][]byte

				body := &format.SynchroBody{}
				require.NoError(t, format.DecodeSynchroBodyInto(row.BodyRaw, body, format.CollectUnknown(&unknown)))
				assert.Empty(t, unknown, "unrecognised synchro keys")

				got = append(got, fmt.Sprintf("%s lsn=%d owner=%d lsn=%d term=%d vclock=%v",
					row.Type, row.LSN, body.ReplicaID, body.LSN, body.Term, map[uint32]int64(body.ConfirmedVClock)))
			}

			assert.Equal(t, tc.want, got)
		})
	}
}

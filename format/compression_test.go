package format_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

// TestCompression_Policy covers the zero-value defaults and the resolution
// helpers of the Compression policy.
func TestCompression_Policy(t *testing.T) {
	t.Parallel()

	// Zero value = Tarantool default: zstd at ZstdLevel over >= CompressThreshold.
	var def format.Compression

	assert.Equal(t, format.ZstdLevel, def.ResolvedLevel(), "zero Level resolves to ZstdLevel")
	assert.True(t, def.Compresses(format.CompressThreshold), "compresses at the threshold")
	assert.False(t, def.Compresses(format.CompressThreshold-1), "below threshold stays plain")

	// Custom threshold.
	cThr := format.Compression{Threshold: 100}
	assert.False(t, cThr.Compresses(99))
	assert.True(t, cThr.Compresses(100))

	// Custom level only — threshold still defaults.
	cLvl := format.Compression{Level: 19}
	assert.Equal(t, 19, cLvl.ResolvedLevel())
	assert.True(t, cLvl.Compresses(format.CompressThreshold))

	// Disabled never compresses, whatever the size.
	cOff := format.Compression{Disabled: true}
	assert.False(t, cOff.Compresses(1<<20))
}

// bigRow returns a one-row tx whose body comfortably crosses the default
// compression threshold.
func bigRow() []format.XRow {
	return []format.XRow{{
		Type: iproto.IPROTO_INSERT, LSN: 1, TSN: 1, Flags: iproto.IPROTO_FLAG_COMMIT,
		BodyRaw: benchBody(4000),
	}}
}

func blockMagic(b []byte) [4]byte {
	var m [4]byte
	copy(m[:], b)

	return m
}

// TestEncodeTxBlock_CompressionPolicy checks that the policy drives the on-disk
// magic (ZRow vs Row) and that every variant round-trips to the same row.
func TestEncodeTxBlock_CompressionPolicy(t *testing.T) {
	t.Parallel()

	rows := bigRow()
	wantBody := rows[0].BodyRaw

	cases := []struct {
		name      string
		comp      format.Compression
		wantMagic [4]byte
	}{
		{"default compresses", format.Compression{}, format.ZRowMarker},
		{"disabled stays plain", format.Compression{Disabled: true}, format.RowMarker},
		{"threshold above payload stays plain", format.Compression{Threshold: 1 << 20}, format.RowMarker},
		{"level 1 compresses", format.Compression{Level: 1}, format.ZRowMarker},
		{"level 19 compresses", format.Compression{Level: 19}, format.ZRowMarker},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			blob, err := format.EncodeTxBlock(rows, format.TxOptions{Compression: tc.comp})
			require.NoError(t, err)
			assert.Equal(t, tc.wantMagic, blockMagic(blob), "on-disk magic")

			// Decode (decompresses + verifies CRC) and check the row survives.
			rowSlices, _, _, err := format.DecodeTxBlock(blob)
			require.NoError(t, err)
			require.Len(t, rowSlices, 1)

			got, _, err := format.DecodeXRow(rowSlices[0])
			require.NoError(t, err)
			assert.True(t, bytes.Equal(wantBody, got.BodyRaw), "body round-trips")
		})
	}
}

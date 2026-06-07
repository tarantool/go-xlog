package writer_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// bigBody returns a msgpack bin body of n bytes — large enough to cross the
// default compression threshold.
func bigBody(n int) []byte {
	b, err := msgpack.Marshal(make([]byte, n))
	if err != nil {
		panic(err)
	}

	return b
}

// firstBlockMagic returns the 4-byte magic of the first tx block in an xlog,
// found right after the meta header's blank-line terminator.
func firstBlockMagic(t *testing.T, data []byte) [4]byte {
	t.Helper()

	i := bytes.Index(data, []byte("\n\n"))
	require.GreaterOrEqual(t, i, 0, "meta header terminator not found")

	off := i + 2
	require.GreaterOrEqual(t, len(data), off+4, "no tx block after header")

	var m [4]byte
	copy(m[:], data[off:off+4])

	return m
}

// TestWriter_Compression drives the per-writer Compression policy end to end:
// the policy selects the on-disk block magic (ZRow vs Row), and every variant
// round-trips byte-exact through the reader — including non-default zstd levels,
// which exercises the level-aware encoder pool.
func TestWriter_Compression(t *testing.T) {
	t.Parallel()

	body := bigBody(4000)

	cases := []struct {
		name      string
		opts      []writer.Option
		wantMagic [4]byte
	}{
		{"default compresses", nil, format.ZRowMarker},
		{"NoCompression stays plain", []writer.Option{writer.NoCompression()}, format.RowMarker},
		{"disabled policy stays plain", []writer.Option{writer.WithCompression(format.Compression{Disabled: true})}, format.RowMarker},
		{"high threshold stays plain", []writer.Option{writer.WithCompression(format.Compression{Threshold: 1 << 20})}, format.RowMarker},
		{"level 1 compresses", []writer.Option{writer.WithCompression(format.Compression{Level: 1})}, format.ZRowMarker},
		{"level 19 compresses", []writer.Option{writer.WithCompression(format.Compression{Level: 19})}, format.ZRowMarker},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			w, err := writer.NewWriter(&buf, exampleMeta(), tc.opts...)
			require.NoError(t, err)

			require.NoError(t, w.WriteTx([]format.XRow{
				{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: body},
			}))
			require.NoError(t, w.Close())

			assert.Equal(t, tc.wantMagic, firstBlockMagic(t, buf.Bytes()), "on-disk block magic")

			// Round-trip: the reader decompresses transparently at any level.
			r, err := reader.NewReader(bytes.NewReader(buf.Bytes()))
			require.NoError(t, err)

			defer func() { _ = r.Close() }()

			var got [][]byte

			for row, err := range r.Rows() {
				require.NoError(t, err)

				got = append(got, append([]byte(nil), row.BodyRaw...))
			}

			require.Len(t, got, 1)
			assert.True(t, bytes.Equal(body, got[0]), "body round-trips")
		})
	}
}

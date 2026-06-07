package format_test

import (
	"testing"

	"github.com/tarantool/go-iproto"
	"github.com/tarantool/go-xlog/format"
)

// benchBody returns a valid msgpack value of roughly n payload bytes (a
// bin8/bin16 wrapper). It is only ever skipped/aliased by the decoders, so
// the exact shape does not matter — only its byte length.
func benchBody(n int) []byte {
	hdr := []byte{0xc4, byte(n)} // 0xc4 = msgpack bin8.
	if n > 0xff {
		hdr = []byte{0xc5, byte(n >> 8), byte(n)} // 0xc5 = msgpack bin16.
	}

	b := make([]byte, 0, len(hdr)+n)
	b = append(b, hdr...)
	b = append(b, make([]byte, n)...)

	return b
}

func benchRow() *format.XRow {
	return &format.XRow{
		Type:    iproto.IPROTO_INSERT,
		LSN:     42,
		TSN:     42,
		Flags:   iproto.IPROTO_FLAG_COMMIT,
		BodyRaw: benchBody(64),
	}
}

func BenchmarkEncodeXRow(b *testing.B) {
	row := benchRow()
	buf := make([]byte, 0, 256)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		var err error

		buf, err = format.EncodeXRow(buf[:0], row)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeXRow(b *testing.B) {
	row := benchRow()

	enc, err := format.EncodeXRow(nil, row)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, _, err := format.DecodeXRow(enc); err != nil {
			b.Fatal(err)
		}
	}
}

package format_test

import (
	"bytes"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

// ExampleVClock shows the two everyday VClock operations: Signature (the
// arithmetic sum of per-replica LSNs, which names the file on disk) and the
// Tarantool textual form via String.
func ExampleVClock() {
	v := format.VClock{1: 10, 2: 5}

	fmt.Println(v)             // Tarantool "{id: lsn, ...}" form, ids ascending.
	fmt.Println(v.Signature()) // 10 + 5 — the filename signature.

	// Output:
	// {1: 10, 2: 5}
	// 15
}

// ExampleVClock_Compare demonstrates the vector-clock partial order. Two
// vclocks are comparable only when one dominates the other on every replica;
// mixed advances (one ahead on replica 1, the other on replica 2) are
// incomparable.
func ExampleVClock_Compare() {
	a := format.VClock{1: 5, 2: 3}
	b := format.VClock{1: 6, 2: 3}
	c := format.VClock{1: 3, 2: 5}

	ord, ok := a.Compare(b)
	fmt.Printf("a vs b: ord=%d ok=%t\n", ord, ok) // a < b

	ord, ok = a.Compare(c)
	fmt.Printf("a vs c: ord=%d ok=%t\n", ord, ok) // incomparable

	// Output:
	// a vs b: ord=-1 ok=true
	// a vs c: ord=0 ok=false
}

// ExampleParseVClock parses a Tarantool vclock literal back into a VClock and
// recomputes its signature — the inverse of VClock.String.
func ExampleParseVClock() {
	v, err := format.ParseVClock("{0: 0, 1: 42}")
	if err != nil {
		panic(err)
	}

	fmt.Println(v)
	fmt.Println(v.Signature())

	// Output:
	// {0: 0, 1: 42}
	// 42
}

// ExampleEncodeMeta writes a journal meta header to a byte buffer. The header
// is plain text terminated by a blank line; the format version defaults to
// "0.13" and an empty PrevVClock is omitted.
func ExampleEncodeMeta() {
	meta := &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "example",
		InstanceUUID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		VClock:       format.VClock{1: 2},
	}

	var buf bytes.Buffer
	if err := format.EncodeMeta(&buf, meta); err != nil {
		panic(err)
	}

	_, _ = os.Stdout.Write(buf.Bytes())

	// Output:
	// XLOG
	// 0.13
	// Version: example
	// Instance: 11111111-2222-3333-4444-555555555555
	// VClock: {1: 2}
}

// ExampleEncodeXRow round-trips a single-statement row through the byte codec.
// EncodeXRow appends the header map plus the verbatim body bytes; DecodeXRow
// parses them back and, because no IPROTO_TSN was emitted, infers the row is a
// committed single-statement transaction.
func ExampleEncodeXRow() {
	// A minimal valid msgpack body: the one-element array [1].
	body := []byte{0x91, 0x01}

	row := &format.XRow{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: body}

	encoded, err := format.EncodeXRow(nil, row)
	if err != nil {
		panic(err)
	}

	decoded, n, err := format.DecodeXRow(encoded)
	if err != nil {
		panic(err)
	}

	fmt.Printf("type=%s lsn=%d commit=%t body=%d bytes (consumed %d of %d)\n",
		format.TypeName(decoded.Type), decoded.LSN, decoded.IsCommit(),
		len(decoded.BodyRaw), n, len(encoded))

	// Output:
	// type=INSERT lsn=1 commit=true body=2 bytes (consumed 7 of 7)
}

// ExampleCRC32C shows the Castagnoli CRC used to protect every tx block: it is
// a pure function of the input, so identical bytes hash equally and any change
// changes the checksum.
func ExampleCRC32C() {
	a := format.CRC32C([]byte("tarantool"))
	b := format.CRC32C([]byte("tarantool"))
	c := format.CRC32C([]byte("Tarantool"))

	fmt.Println(a == b)
	fmt.Println(a == c)

	// Output:
	// true
	// false
}

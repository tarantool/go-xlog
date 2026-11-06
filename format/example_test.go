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

// ExampleDecodeVyStmtBody decodes a DELETE statement of a vinyl run: the key
// it deletes and the flag that it did not delete the old tuple from the
// secondary indexes, as Tarantool writes it for a space with defer_deletes.
func ExampleDecodeVyStmtBody() {
	// {IPROTO_KEY: [2], IPROTO_TUPLE_META: {VY_STMT_FLAGS: 1}}
	raw := []byte{0x82, 0x20, 0x91, 0x02, 0x2a, 0x81, 0x01, 0x01}

	stmt, err := format.DecodeVyStmtBody(raw)
	if err != nil {
		panic(err)
	}

	fmt.Printf("key=%x deferred_delete=%t\n", stmt.Key, stmt.Flags&format.VyStmtDeferredDelete != 0)
	// Output: key=9102 deferred_delete=true
}

// ExampleDecodeDMLBodyInto decodes the bodies of a stream of rows into one
// DMLBody, with no allocation per row. A body key DMLBody has no field for is
// skipped unless the options ask for it: here CollectUnknown gathers them,
// as a path that writes the body again must.
func ExampleDecodeDMLBodyInto() {
	bodies := [][]byte{
		// {IPROTO_SPACE_ID: 512, IPROTO_INDEX_BASE: 1, IPROTO_KEY: [1]}
		{0x83, 0x10, 0xcd, 0x02, 0x00, 0x15, 0x01, 0x20, 0x91, 0x01},
		// {IPROTO_SPACE_ID: 513, 0x7e: 5}
		{0x82, 0x10, 0xcd, 0x02, 0x01, 0x7e, 0x05},
	}

	var body format.DMLBody

	for _, raw := range bodies {
		var unknown map[uint64][]byte

		if err := format.DecodeDMLBodyInto(raw, &body, format.CollectUnknown(&unknown)); err != nil {
			panic(err)
		}

		fmt.Printf("space=%d index_base=%d key=%x unknown=%x\n", body.SpaceID, body.IndexBase, body.Key, unknown)
	}

	// Output:
	// space=512 index_base=1 key=9101 unknown=map[]
	// space=513 index_base=0 key= unknown=map[7e:05]
}

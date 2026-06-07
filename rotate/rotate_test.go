package rotate_test

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5/msgpcode"

	"github.com/tarantool/go-xlog/dir"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/rotate"
)

var testInstance = uuid.MustParse("00000000-0000-0000-0000-000000000042")

// makeRow synthesises a single-row tx for ReplicaID 1 with the given LSN and
// a BodyRaw payload of ~bodySize bytes. The body is a valid msgpack DML body.
func makeRow(lsn int64, bodySize int) format.XRow {
	// Construct a payload tuple whose msgpack-encoded body is roughly
	// bodySize bytes by padding a string element.
	pad := strings.Repeat("x", bodySize)

	return format.XRow{
		Type:      iproto.IPROTO_INSERT,
		ReplicaID: 1,
		LSN:       lsn,
		BodyRaw:   encodeInsertBody(512, pad),
	}
}

// TestRotate_ChainVClock — small MaxFileSize forces multiple rotations; each
// adjacent pair must satisfy Meta(f_k).VClock == Meta(f_{k+1}).PrevVClock.
func TestRotate_ChainVClock(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	start := format.VClock{}

	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, start,
		rotate.MaxFileSize(256))
	require.NoError(t, err, "New")

	// Write 6 single-row txs with LSNs 1..6, each body ~100 bytes. This
	// reliably crosses the 256-byte size threshold multiple times.
	for lsn := int64(1); lsn <= 6; lsn++ {
		require.NoError(t, rw.WriteTx([]format.XRow{makeRow(lsn, 100)}), "WriteTx lsn=%d", lsn)
	}

	require.NoError(t, rw.Close(), "Close")

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err, "OpenDir")

	files := d.Files()
	require.GreaterOrEqual(t, len(files), 2, "expected ≥2 files after rotation")

	// Chain consistency: Meta(f_k).VClock == Meta(f_{k+1}).PrevVClock.
	for i := 1; i < len(files); i++ {
		prev := files[i-1]
		cur := files[i]
		assert.True(t, vclockEqual(prev.VClock, cur.PrevVClock),
			"chain break: file[%d].VClock=%v but file[%d].PrevVClock=%v",
			i-1, prev.VClock, i, cur.PrevVClock)
	}
}

// TestRotate_NeverMidTx — a multi-row tx larger than MaxFileSize must
// still land entirely in one file. After the tx the writer may rotate.
func TestRotate_NeverMidTx(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{},
		rotate.MaxFileSize(128))
	require.NoError(t, err, "New")

	// 3-row tx, each row ~200 bytes → total ~600 bytes > rotate.MaxFileSize(128).
	bigTx := []format.XRow{
		makeRow(1, 200),
		makeRow(2, 200),
		makeRow(3, 200),
	}
	require.NoError(t, rw.WriteTx(bigTx), "WriteTx big")
	// Follow-up small txs that should trigger rotation.
	for lsn := int64(4); lsn <= 6; lsn++ {
		require.NoError(t, rw.WriteTx([]format.XRow{makeRow(lsn, 100)}), "WriteTx lsn=%d", lsn)
	}

	require.NoError(t, rw.Close(), "Close")

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err, "OpenDir")

	files := d.Files()
	require.GreaterOrEqual(t, len(files), 2, "expected ≥2 files (rotation occurred)")

	// The first file must contain the entire 3-row tx as ONE transaction.
	// Read it back and verify the first tx has all 3 rows with LSNs 1,2,3.
	first := files[0]
	rd, err := reader.Open(first.Path)
	require.NoError(t, err, "reader.Open %q", first.Path)

	defer func() { _ = rd.Close() }()

	tx, err := rd.NextTx()
	require.NoError(t, err, "NextTx first file")
	require.Len(t, tx.Rows, 3, "first tx in first file: want 3 rows (multi-row tx held together)")

	for i, r := range tx.Rows {
		wantLSN := int64(i + 1)
		assert.Equal(t, wantLSN, r.LSN, "tx.Rows[%d].LSN", i)
	}

	assert.True(t, tx.Rows[2].IsCommit(), "last row of multi-row tx should have IsCommit=true")
}

// TestRotate_FilenameSignature — every file in the dir has filename
// %020d.xlog with the digits equal to Meta.VClock.Signature().
func TestRotate_FilenameSignature(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{},
		rotate.MaxFileSize(256))
	require.NoError(t, err, "New")

	for lsn := int64(1); lsn <= 6; lsn++ {
		require.NoError(t, rw.WriteTx([]format.XRow{makeRow(lsn, 100)}), "WriteTx lsn=%d", lsn)
	}

	require.NoError(t, rw.Close(), "Close")

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err, "OpenDir")

	for _, f := range d.Files() {
		base := filepath.Base(f.Path)
		want := fmt.Sprintf("%020d.xlog", f.VClock.Signature())
		assert.Equal(t, want, base, "filename (sig=%d)", f.VClock.Signature())
	}
}

// TestRotate_EmptyStart — New with an empty startVClock should produce a
// first file named "00000000000000000000.xlog" with empty in-meta VClock.
func TestRotate_EmptyStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, format.VClock{})
	require.NoError(t, err, "New")
	require.NoError(t, rw.Close(), "Close")

	want := filepath.Join(tmp, "00000000000000000000.xlog")
	_, err = os.Stat(want)
	require.NoError(t, err, "expected file %q", want)
}

// TestRotate_NilStart — New(nil startVClock) must not panic on the first
// WriteTx. A nil VClock clones to nil, leaving runningVClock a nil map; the
// LSN-advance assignment in WriteTx would then panic on a nil-map write. New
// must normalise nil → empty VClock so the writer behaves like EmptyStart.
func TestRotate_NilStart(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rw, err := rotate.New(tmp, format.FiletypeXLOG, testInstance, nil)
	require.NoError(t, err, "New(nil startVClock)")

	require.NoError(t, rw.WriteTx([]format.XRow{makeRow(1, 100)}), "first WriteTx")
	require.NoError(t, rw.Close(), "Close")
}

// TestRotate_MkdirAll — New must create the dir if it doesn't exist.
func TestRotate_MkdirAll(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	nested := filepath.Join(tmp, "a", "b", "c")
	rw, err := rotate.New(nested, format.FiletypeXLOG, testInstance, format.VClock{})
	require.NoError(t, err, "New (mkdir)")
	require.NoError(t, rw.Close(), "Close")

	_, err = os.Stat(nested)
	require.NoError(t, err, "expected nested dir %q", nested)
}

// --- helpers ---.

func vclockEqual(a, b format.VClock) bool {
	// Reflect.DeepEqual on maps with the same keys/values; nil vs empty
	// must be treated as equal (empty start case).
	if len(a) == 0 && len(b) == 0 {
		return true
	}

	return reflect.DeepEqual(map[uint32]int64(a), map[uint32]int64(b))
}

func encodeInsertBody(spaceID uint32, payload string) []byte {
	var b []byte

	b = mpMapHeader(b, 2)
	b = mpUint(b, 0x10)
	b = mpUint(b, uint64(spaceID))
	b = mpUint(b, 0x21)
	b = mpArrayHeader(b, 1)
	b = mpStr(b, payload)

	return b
}

func mpUint(buf []byte, n uint64) []byte {
	switch {
	case n <= 0x7f:
		return append(buf, byte(n))
	case n <= 0xff:
		return append(buf, msgpcode.Uint8, byte(n))
	case n <= 0xffff:
		buf = append(buf, msgpcode.Uint16)

		var tmp [2]byte
		binary.BigEndian.PutUint16(tmp[:], uint16(n))

		return append(buf, tmp[:]...)
	case n <= 0xffffffff:
		buf = append(buf, msgpcode.Uint32)

		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], uint32(n))

		return append(buf, tmp[:]...)
	default:
		buf = append(buf, msgpcode.Uint64)

		var tmp [8]byte
		binary.BigEndian.PutUint64(tmp[:], n)

		return append(buf, tmp[:]...)
	}
}

func mpMapHeader(buf []byte, n int) []byte {
	if n <= 15 {
		return append(buf, msgpcode.FixedMapLow|byte(n))
	}

	if n <= 0xffff {
		buf = append(buf, msgpcode.Map16)

		var tmp [2]byte
		binary.BigEndian.PutUint16(tmp[:], uint16(n))

		return append(buf, tmp[:]...)
	}

	buf = append(buf, msgpcode.Map32)

	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], uint32(n))

	return append(buf, tmp[:]...)
}

func mpArrayHeader(buf []byte, n int) []byte {
	if n <= 15 {
		return append(buf, msgpcode.FixedArrayLow|byte(n))
	}

	if n <= 0xffff {
		buf = append(buf, msgpcode.Array16)

		var tmp [2]byte
		binary.BigEndian.PutUint16(tmp[:], uint16(n))

		return append(buf, tmp[:]...)
	}

	buf = append(buf, msgpcode.Array32)

	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], uint32(n))

	return append(buf, tmp[:]...)
}

func mpStr(buf []byte, s string) []byte {
	n := len(s)
	switch {
	case n <= 31:
		buf = append(buf, msgpcode.FixedStrLow|byte(n))
	case n <= 0xff:
		buf = append(buf, msgpcode.Str8, byte(n))
	case n <= 0xffff:
		buf = append(buf, msgpcode.Str16)

		var tmp [2]byte
		binary.BigEndian.PutUint16(tmp[:], uint16(n))
		buf = append(buf, tmp[:]...)
	default:
		buf = append(buf, msgpcode.Str32)

		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], uint32(n))
		buf = append(buf, tmp[:]...)
	}

	return append(buf, s...)
}

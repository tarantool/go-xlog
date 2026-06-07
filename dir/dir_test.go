package dir_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5/msgpcode"

	"github.com/tarantool/go-xlog/dir"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/writer"
)

// testInstance is the UUID used by every fixture this test file generates.
// Stable across runs so failures reproduce deterministically.
var testInstance = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// writeXLog creates a valid <sig>.xlog file in tmpDir using writer.Create.
// Vclock / prevVClock are written into the meta; a single insert row is
// emitted with lsn = rowLSN.
func writeXLog(t *testing.T, tmpDir string, vclock, prev format.VClock, rowLSN int64) string {
	t.Helper()

	sig := vclock.Signature()
	path := filepath.Join(tmpDir, strconv.FormatInt(sig, 10)+".xlog")

	meta := &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "go-xlog/test",
		InstanceUUID: testInstance,
		VClock:       vclock,
		PrevVClock:   prev,
	}
	w, err := writer.Create(path, meta)
	require.NoError(t, err)

	row := format.XRow{
		Type:    iproto.IPROTO_INSERT,
		LSN:     rowLSN,
		BodyRaw: encodeInsertBody(512, rowLSN),
	}
	require.NoError(t, w.WriteTx([]format.XRow{row}))
	require.NoError(t, w.Close())

	return path
}

// TestOpenDir_BasicOrder builds 3 xlog files and asserts they come back
// sorted by signature ascending.
func TestOpenDir_BasicOrder(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeXLog(t, tmp, format.VClock{1: 0}, nil, 1)
	writeXLog(t, tmp, format.VClock{1: 100}, format.VClock{1: 0}, 101)
	writeXLog(t, tmp, format.VClock{1: 200}, format.VClock{1: 100}, 201)

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err)

	got := d.Files()
	require.Len(t, got, 3)

	wantSigs := []int64{0, 100, 200}
	for i, want := range wantSigs {
		assert.Equal(t, want, got[i].Signature, "entry[%d].Signature", i)
		assert.Equal(t, format.FiletypeXLOG, got[i].Filetype, "entry[%d].Filetype", i)
		assert.Equal(t, want, got[i].VClock.Signature(), "entry[%d].VClock.Signature", i)
	}
}

// TestOpenDir_LocateVClock — point inside / between / after the chain.
func TestOpenDir_LocateVClock(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	a := writeXLog(t, tmp, format.VClock{1: 0}, nil, 1)
	b := writeXLog(t, tmp, format.VClock{1: 100}, format.VClock{1: 0}, 101)
	c := writeXLog(t, tmp, format.VClock{1: 200}, format.VClock{1: 100}, 201)

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err)

	cases := []struct {
		name   string
		target format.VClock
		want   string
	}{
		{"equal-A", format.VClock{1: 0}, a},
		{"inside-A", format.VClock{1: 50}, a},
		{"boundary-B", format.VClock{1: 100}, b},
		{"inside-B", format.VClock{1: 150}, b},
		{"boundary-C", format.VClock{1: 200}, c},
		{"past-end", format.VClock{1: 500}, c},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := d.LocateVClock(tc.target)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Path, "LocateVClock(%v)", tc.target)
		})
	}
}

// TestOpenDir_LocateVClock_BeforeAll — target strictly below the earliest
// indexed entry returns ErrNotFound.
func TestOpenDir_LocateVClock_BeforeAll(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeXLog(t, tmp, format.VClock{1: 10}, nil, 11)
	writeXLog(t, tmp, format.VClock{1: 100}, format.VClock{1: 10}, 101)

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err)
	_, err = d.LocateVClock(format.VClock{1: 5})
	require.ErrorIs(t, err, dir.ErrNotFound)
}

// TestOpenDir_LocateVClock_Empty — empty dir returns ErrNotFound.
func TestOpenDir_LocateVClock_Empty(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err)
	_, err = d.LocateVClock(format.VClock{1: 0})
	require.ErrorIs(t, err, dir.ErrNotFound)
}

// TestOpenDir_LocateLSN — projects to a single replica axis.
func TestOpenDir_LocateLSN(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	a := writeXLog(t, tmp, format.VClock{1: 0}, nil, 1)
	b := writeXLog(t, tmp, format.VClock{1: 100}, format.VClock{1: 0}, 101)
	writeXLog(t, tmp, format.VClock{1: 200}, format.VClock{1: 100}, 201)

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err)

	got, err := d.LocateLSN(1, 50)
	require.NoError(t, err)
	assert.Equal(t, a, got.Path, "LocateLSN(1,50)")

	got, err = d.LocateLSN(1, 150)
	require.NoError(t, err)
	assert.Equal(t, b, got.Path, "LocateLSN(1,150)")

	_, err = d.LocateLSN(1, -1)
	require.ErrorIs(t, err, dir.ErrNotFound)
}

// TestOpenDir_LocateLSN_UnknownReplica — projecting onto a replica id no
// file mentions: every entry has VClock[that-id]=0, so a negative lsn must
// still produce ErrNotFound (0 is not ≤ -1).
func TestOpenDir_LocateLSN_UnknownReplica(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeXLog(t, tmp, format.VClock{1: 0}, nil, 1)
	writeXLog(t, tmp, format.VClock{1: 100}, format.VClock{1: 0}, 101)

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err)
	_, err = d.LocateLSN(2, -1)
	require.ErrorIs(t, err, dir.ErrNotFound)
}

// TestOpenDir_SignatureMismatch — rename a file so its filename signature
// no longer equals its in-meta VClock sum. OpenDir must reject with
// ErrSignatureMismatch.
func TestOpenDir_SignatureMismatch(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeXLog(t, tmp, format.VClock{1: 0}, nil, 1)
	writeXLog(t, tmp, format.VClock{1: 100}, format.VClock{1: 0}, 101)
	pathC := writeXLog(t, tmp, format.VClock{1: 200}, format.VClock{1: 100}, 201)

	bogus := filepath.Join(tmp, "999.xlog")
	require.NoError(t, os.Rename(pathC, bogus))

	_, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.ErrorIs(t, err, dir.ErrSignatureMismatch)
}

// TestOpenDir_Duplicate — write two files whose distinct VClocks both sum
// to the same signature. OpenDir must reject with ErrDuplicate.
//
// To get two files with stem-parsing to the same int64 signature without
// colliding on the filesystem name we exploit strconv.ParseInt's
// leading-zero tolerance: "0100" and "100" parse to the same value.
func TestOpenDir_Duplicate(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()

	// File 1: VClock {1: 100} → signature 100, filename `100.xlog`.
	writeXLog(t, tmp, format.VClock{1: 100}, nil, 101)

	// File 2: VClock {1: 50, 2: 50} → signature 100 as well. Write it in
	// a scratch dir under `100.xlog` (writer derives filename from path
	// alone, so any path works), then move into `tmp` under a distinct
	// on-disk name that still parses to 100.
	scratch := t.TempDir()
	scratchPath := filepath.Join(scratch, "100.xlog")
	meta := &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "go-xlog/test",
		InstanceUUID: testInstance,
		VClock:       format.VClock{1: 50, 2: 50},
	}
	w, err := writer.Create(scratchPath, meta)
	require.NoError(t, err)

	row := format.XRow{
		Type:    iproto.IPROTO_INSERT,
		LSN:     51,
		BodyRaw: encodeInsertBody(512, 51),
	}
	require.NoError(t, w.WriteTx([]format.XRow{row}))
	require.NoError(t, w.Close())

	// `0100.xlog` parses to 100 via strconv.ParseInt, but is a distinct
	// filename so both files coexist in tmp.
	dupPath := filepath.Join(tmp, "0100.xlog")
	require.NoError(t, os.Rename(scratchPath, dupPath))

	_, err = dir.OpenDir(tmp, format.FiletypeXLOG)
	require.ErrorIs(t, err, dir.ErrDuplicate)
}

// TestOpenDir_SkipInprogress — a `.inprogress` file in the directory must
// not be parsed or indexed.
func TestOpenDir_SkipInprogress(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeXLog(t, tmp, format.VClock{1: 0}, nil, 1)
	writeXLog(t, tmp, format.VClock{1: 100}, format.VClock{1: 0}, 101)

	junk := filepath.Join(tmp, "999.xlog.inprogress")
	require.NoError(t, os.WriteFile(junk, []byte("this is not a valid xlog header"), 0o644))

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err)
	require.Len(t, d.Files(), 2)
}

// TestOpenDir_VYLOG — copy the testdata vylog fixture into a tmpdir
// renamed to <sig>.vylog, then assert OpenDir indexes it as one entry.
func TestOpenDir_VYLOG(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()

	src := findTestdata(t, "vylog_sample.vylog")

	// Sniff the fixture's in-meta signature so the renamed copy matches
	// the requirement that filename signature == in-meta VClock.Signature().
	rd, err := reader.Open(src)
	require.NoError(t, err)

	srcSig := rd.Meta().VClock.Signature()
	require.NoError(t, rd.Close())

	data, err := os.ReadFile(src)
	require.NoError(t, err)

	dst := filepath.Join(tmp, strconv.FormatInt(srcSig, 10)+".vylog")
	require.NoError(t, os.WriteFile(dst, data, 0o644))

	d, err := dir.OpenDir(tmp, format.FiletypeVYLOG)
	require.NoError(t, err)
	require.Len(t, d.Files(), 1)
	assert.Equal(t, format.FiletypeVYLOG, d.Files()[0].Filetype)
	assert.Equal(t, srcSig, d.Files()[0].Signature)
}

// TestOpenDir_UnknownFiletype — passing an unrecognised filetype is a
// programmer error and must surface as an error rather than silently
// matching nothing.
func TestOpenDir_UnknownFiletype(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	_, err := dir.OpenDir(tmp, format.Filetype("BOGUS"))
	require.Error(t, err)
}

// TestOpenDir_IgnoresUnrelatedFiles — non-journal files in the directory
// (README.md, hidden files, etc.) are silently skipped.
func TestOpenDir_IgnoresUnrelatedFiles(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeXLog(t, tmp, format.VClock{1: 0}, nil, 1)

	// Sundry junk that does not match <digits>.xlog:.
	for _, junk := range []string{"README.md", "notes.txt", "abc.xlog", ".hidden"} {
		require.NoError(t, os.WriteFile(filepath.Join(tmp, junk), []byte("x"), 0o644))
	}

	d, err := dir.OpenDir(tmp, format.FiletypeXLOG)
	require.NoError(t, err)
	require.Len(t, d.Files(), 1)
}

// --- helpers ---.

// findTestdata walks up from cwd looking for a directory containing
// `testdata/<name>`. Mirrors internal/testutil but kept local.
func findTestdata(t *testing.T, name string) string {
	t.Helper()

	cwd, err := os.Getwd()
	require.NoError(t, err)

	d := cwd
	for {
		candidate := filepath.Join(d, "testdata", name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}

		parent := filepath.Dir(d)
		if parent == d {
			break
		}

		d = parent
	}

	t.Fatalf("could not locate testdata/%s from cwd=%s", name, cwd)

	return ""
}

// encodeInsertBody — tiny msgpack map {KeySpaceID: spaceID, KeyTuple: [val]}.
// Identical in shape to writer/testhelpers_test.go's encodeDMLBody but
// scoped to this test package.
func encodeInsertBody(spaceID uint32, val int64) []byte {
	var b []byte

	b = mpMapHeader(b, 2)
	b = mpUint(b, 0x10)
	b = mpUint(b, uint64(spaceID))
	b = mpUint(b, 0x21)
	b = mpArrayHeader(b, 1)
	b = mpInt(b, val)

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

func mpInt(buf []byte, n int64) []byte {
	if n >= 0 {
		return mpUint(buf, uint64(n))
	}

	if n >= -32 {
		return append(buf, byte(int8(n)))
	}

	buf = append(buf, msgpcode.Int64)

	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], uint64(n))

	return append(buf, tmp[:]...)
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

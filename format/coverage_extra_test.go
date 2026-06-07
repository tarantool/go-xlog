package format_test

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-iproto"
	"github.com/tarantool/go-xlog/format"
)

// --- small msgpack builders (black-box: we only hand bytes to exported
// decoders, never call the package's unexported helpers). ---

func mpFixmap(n int) []byte {
	return []byte{0x80 | byte(n)}
}

// decodeMetaString feeds a literal meta blob through DecodeMeta.
func decodeMetaString(s string) (*format.Meta, error) {
	return format.DecodeMeta(bufio.NewReader(bytes.NewReader([]byte(s))), format.MetaOptions{})
}

// mpUint encodes a uint with the smallest width, mirroring mp_encode_uint.
func mpUint(n uint64) []byte {
	switch {
	case n <= 0x7f:
		return []byte{byte(n)}
	case n <= math.MaxUint8:
		return []byte{0xcc, byte(n)}
	case n <= math.MaxUint16:
		b := []byte{0xcd, 0, 0}
		binary.BigEndian.PutUint16(b[1:], uint16(n))
		return b
	case n <= math.MaxUint32:
		b := []byte{0xce, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(b[1:], uint32(n))
		return b
	default:
		b := []byte{0xcf, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint64(b[1:], n)
		return b
	}
}

func mpFloat32(f float32) []byte {
	b := []byte{0xca, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[1:], math.Float32bits(f))
	return b
}

// kv builds a single key/value pair (both already-encoded msgpack).
func kv(key uint64, val []byte) []byte {
	return append(mpUint(key), val...)
}

// buildMap concatenates a fixmap header of n entries with the given body bytes.
func buildMap(n int, body []byte) []byte {
	return append(mpFixmap(n), body...)
}

// ---------------------------------------------------------------------------
// RAFT body: DecodeRaftBody + decodeVClockMap.
// ---------------------------------------------------------------------------

func TestDecodeRaftBody_AllKeys(t *testing.T) {
	t.Parallel()

	// VCLOCK nested map {1: 100, 2: 200}.
	vclock := buildMap(2, append(kv(1, mpUint(100)), kv(2, mpUint(200))...))

	var body []byte
	body = append(body, kv(uint64(iproto.IPROTO_RAFT_TERM), mpUint(5))...)
	body = append(body, kv(uint64(iproto.IPROTO_RAFT_VOTE), mpUint(3))...)
	body = append(body, kv(uint64(iproto.IPROTO_RAFT_STATE), mpUint(2))...)
	body = append(body, kv(uint64(iproto.IPROTO_RAFT_VCLOCK), vclock)...)
	body = append(body, kv(uint64(iproto.IPROTO_RAFT_LEADER_ID), mpUint(7))...)
	body = append(body, kv(uint64(iproto.IPROTO_RAFT_IS_LEADER_SEEN), []byte{0xc3})...) // true
	body = append(body, kv(99, mpUint(42))...)                                          // unknown -> Extras

	raw := buildMap(7, body)

	rb, err := format.DecodeRaftBody(raw)
	require.NoError(t, err)
	assert.Equal(t, uint64(5), rb.Term)
	assert.Equal(t, uint64(3), rb.Vote)
	assert.Equal(t, uint32(2), rb.State)
	assert.Equal(t, uint32(7), rb.LeaderID)
	assert.True(t, rb.IsLeaderSeen)
	assert.Equal(t, int64(100), rb.VClock[1])
	assert.Equal(t, int64(200), rb.VClock[2])
	require.Contains(t, rb.Extras, uint64(99))
}

func TestDecodeRaftBody_IsLeaderSeenFalse(t *testing.T) {
	t.Parallel()

	raw := buildMap(1, kv(uint64(iproto.IPROTO_RAFT_IS_LEADER_SEEN), []byte{0xc2})) // false
	rb, err := format.DecodeRaftBody(raw)
	require.NoError(t, err)
	assert.False(t, rb.IsLeaderSeen)
}

func TestDecodeRaftBody_Errors(t *testing.T) {
	t.Parallel()

	t.Run("empty body", func(t *testing.T) {
		t.Parallel()
		_, err := format.DecodeRaftBody(nil)
		require.ErrorIs(t, err, format.ErrEmptyBody)
	})

	t.Run("not a map", func(t *testing.T) {
		t.Parallel()
		_, err := format.DecodeRaftBody([]byte{0xc0}) // nil, not a map header
		require.Error(t, err)
	})

	t.Run("truncated key", func(t *testing.T) {
		t.Parallel()
		// fixmap(1) then a uint8 key tag with no following byte.
		_, err := format.DecodeRaftBody([]byte{0x81, 0xcc})
		require.Error(t, err)
	})

	t.Run("truncated value", func(t *testing.T) {
		t.Parallel()
		// fixmap(1) key=TERM, then value tag uint16 with missing bytes.
		raw := append(mpFixmap(1), kv(uint64(iproto.IPROTO_RAFT_TERM), []byte{0xcd, 0x00})...)
		_, err := format.DecodeRaftBody(raw)
		require.Error(t, err)
	})

	t.Run("is_leader_seen bad length", func(t *testing.T) {
		t.Parallel()
		// value is a 2-byte uint8 (len 2 != 1).
		raw := buildMap(1, kv(uint64(iproto.IPROTO_RAFT_IS_LEADER_SEEN), mpUint(200)))
		_, err := format.DecodeRaftBody(raw)
		require.ErrorIs(t, err, format.ErrIsLeaderSeenLen)
	})

	t.Run("is_leader_seen bad tag", func(t *testing.T) {
		t.Parallel()
		// single byte but not true/false (use mpcNil 0xc0).
		raw := buildMap(1, kv(uint64(iproto.IPROTO_RAFT_IS_LEADER_SEEN), []byte{0xc0}))
		_, err := format.DecodeRaftBody(raw)
		require.ErrorIs(t, err, format.ErrIsLeaderSeenTag)
	})

	t.Run("bad vclock value", func(t *testing.T) {
		t.Parallel()
		// VCLOCK value is not a map (a fixint instead).
		raw := buildMap(1, kv(uint64(iproto.IPROTO_RAFT_VCLOCK), []byte{0x01}))
		_, err := format.DecodeRaftBody(raw)
		require.Error(t, err)
	})

	t.Run("bad term value", func(t *testing.T) {
		t.Parallel()
		// TERM value is a string, not a uint.
		raw := buildMap(1, kv(uint64(iproto.IPROTO_RAFT_TERM), []byte{0xa1, 'x'}))
		_, err := format.DecodeRaftBody(raw)
		require.Error(t, err)
	})
}

func TestDecodeRaftBody_VClockMapTruncated(t *testing.T) {
	t.Parallel()

	// VCLOCK declares 1 entry but lacks the lsn after the id.
	vclock := append(mpFixmap(1), mpUint(1)...) // id only, no lsn
	raw := buildMap(1, kv(uint64(iproto.IPROTO_RAFT_VCLOCK), vclock))
	_, err := format.DecodeRaftBody(raw)
	require.Error(t, err)
}

func TestDecodeRaftBody_VClockBadID(t *testing.T) {
	t.Parallel()

	// id is a string, not a uint.
	vclock := append(mpFixmap(1), 0xa1, 'x', 0x01)
	raw := buildMap(1, kv(uint64(iproto.IPROTO_RAFT_VCLOCK), vclock))
	_, err := format.DecodeRaftBody(raw)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// DML body.
// ---------------------------------------------------------------------------

func TestDecodeDMLBody_AllKeys(t *testing.T) {
	t.Parallel()

	tuple := []byte{0x91, 0x01} // array(1)[1]
	key := []byte{0x91, 0x02}   // array(1)[2]
	ops := []byte{0x90}         // array(0)
	oldT := []byte{0x91, 0x03}  // array(1)[3]
	newT := []byte{0x91, 0x04}  // array(1)[4]
	extraV := []byte{0x05}      // fixint

	var b []byte
	b = append(b, kv(uint64(iproto.IPROTO_SPACE_ID), mpUint(512))...)
	b = append(b, kv(uint64(iproto.IPROTO_TUPLE), tuple)...)
	b = append(b, kv(uint64(iproto.IPROTO_KEY), key)...)
	b = append(b, kv(uint64(iproto.IPROTO_OPS), ops)...)
	b = append(b, kv(uint64(iproto.IPROTO_OLD_TUPLE), oldT)...)
	b = append(b, kv(uint64(iproto.IPROTO_NEW_TUPLE), newT)...)
	b = append(b, kv(0x7e, extraV)...)
	raw := buildMap(7, b)

	body, err := format.DecodeDMLBody(raw)
	require.NoError(t, err)
	assert.Equal(t, uint32(512), body.SpaceID)
	assert.Equal(t, tuple, body.Tuple)
	assert.Equal(t, key, body.Key)
	assert.Equal(t, ops, body.Ops)
	assert.Equal(t, oldT, body.OldTuple)
	assert.Equal(t, newT, body.NewTuple)
	require.Contains(t, body.Extras, uint64(0x7e))
}

func TestDecodeDMLBody_Errors(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		_, err := format.DecodeDMLBody(nil)
		require.ErrorIs(t, err, format.ErrEmptyBody)
	})

	t.Run("not a map", func(t *testing.T) {
		t.Parallel()
		_, err := format.DecodeDMLBody([]byte{0x01})
		require.Error(t, err)
	})

	t.Run("bad key", func(t *testing.T) {
		t.Parallel()
		_, err := format.DecodeDMLBody([]byte{0x81, 0xcc})
		require.Error(t, err)
	})

	t.Run("truncated value", func(t *testing.T) {
		t.Parallel()
		raw := append(mpFixmap(1), kv(uint64(iproto.IPROTO_TUPLE), []byte{0xdc, 0x00})...) // array16 short
		_, err := format.DecodeDMLBody(raw)
		require.Error(t, err)
	})

	t.Run("bad space_id value", func(t *testing.T) {
		t.Parallel()
		raw := buildMap(1, kv(uint64(iproto.IPROTO_SPACE_ID), []byte{0xa1, 'x'}))
		_, err := format.DecodeDMLBody(raw)
		require.Error(t, err)
	})
}

// ---------------------------------------------------------------------------
// Synchro body.
// ---------------------------------------------------------------------------

func TestDecodeSynchroBody_AllKeys(t *testing.T) {
	t.Parallel()

	var b []byte
	b = append(b, kv(uint64(iproto.IPROTO_REPLICA_ID), mpUint(3))...)
	b = append(b, kv(uint64(iproto.IPROTO_LSN), mpUint(99))...)
	b = append(b, kv(uint64(iproto.IPROTO_RAFT_TERM), mpUint(7))...)
	b = append(b, kv(0x55, []byte{0x01})...) // extra
	raw := buildMap(4, b)

	body, err := format.DecodeSynchroBody(raw)
	require.NoError(t, err)
	assert.Equal(t, uint32(3), body.ReplicaID)
	assert.Equal(t, int64(99), body.LSN)
	assert.Equal(t, uint64(7), body.Term)
	require.Contains(t, body.Extras, uint64(0x55))
}

func TestDecodeSynchroBody_Errors(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		_, err := format.DecodeSynchroBody(nil)
		require.ErrorIs(t, err, format.ErrEmptyBody)
	})

	t.Run("not a map", func(t *testing.T) {
		t.Parallel()
		_, err := format.DecodeSynchroBody([]byte{0x01})
		require.Error(t, err)
	})

	t.Run("bad key", func(t *testing.T) {
		t.Parallel()
		_, err := format.DecodeSynchroBody([]byte{0x81, 0xcc})
		require.Error(t, err)
	})

	t.Run("bad replica_id value", func(t *testing.T) {
		t.Parallel()
		raw := buildMap(1, kv(uint64(iproto.IPROTO_REPLICA_ID), []byte{0xa1, 'x'}))
		_, err := format.DecodeSynchroBody(raw)
		require.Error(t, err)
	})

	t.Run("bad lsn value", func(t *testing.T) {
		t.Parallel()
		raw := buildMap(1, kv(uint64(iproto.IPROTO_LSN), []byte{0xa1, 'x'}))
		_, err := format.DecodeSynchroBody(raw)
		require.Error(t, err)
	})

	t.Run("bad term value", func(t *testing.T) {
		t.Parallel()
		raw := buildMap(1, kv(uint64(iproto.IPROTO_RAFT_TERM), []byte{0xa1, 'x'}))
		_, err := format.DecodeSynchroBody(raw)
		require.Error(t, err)
	})

	t.Run("truncated value skip", func(t *testing.T) {
		t.Parallel()
		raw := append(mpFixmap(1), kv(0x55, []byte{0xdc, 0x00})...) // array16 short header
		_, err := format.DecodeSynchroBody(raw)
		require.Error(t, err)
	})
}

// ---------------------------------------------------------------------------
// VyLog body.
// ---------------------------------------------------------------------------

func TestDecodeVyLogBody_Errors(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		_, err := format.DecodeVyLogBody(nil)
		require.ErrorIs(t, err, format.ErrEmptyBody)
	})

	t.Run("not a map", func(t *testing.T) {
		t.Parallel()
		_, err := format.DecodeVyLogBody([]byte{0x01})
		require.Error(t, err)
	})

	t.Run("bad key", func(t *testing.T) {
		t.Parallel()
		_, err := format.DecodeVyLogBody([]byte{0x81, 0xcc})
		require.Error(t, err)
	})

	t.Run("bad type value", func(t *testing.T) {
		t.Parallel()
		raw := buildMap(1, kv(uint64(iproto.IPROTO_REQUEST_TYPE), []byte{0xa1, 'x'}))
		_, err := format.DecodeVyLogBody(raw)
		require.Error(t, err)
	})

	t.Run("truncated value skip", func(t *testing.T) {
		t.Parallel()
		raw := append(mpFixmap(1), kv(0x10, []byte{0xdc, 0x00})...)
		_, err := format.DecodeVyLogBody(raw)
		require.Error(t, err)
	})
}

func TestDecodeVyLogBody_TypeAndKeys(t *testing.T) {
	t.Parallel()

	var b []byte
	b = append(b, kv(uint64(iproto.IPROTO_REQUEST_TYPE), mpUint(9))...)
	b = append(b, kv(0x10, []byte{0x91, 0x01})...)
	raw := buildMap(2, b)

	body, err := format.DecodeVyLogBody(raw)
	require.NoError(t, err)
	assert.Equal(t, uint32(9), body.Type)
	require.Contains(t, body.Keys, format.VyKey(0x10))
}

// ---------------------------------------------------------------------------
// Fixheader: large lengths force multi-byte mp uint encodings.
// ---------------------------------------------------------------------------

func TestFixheader_RoundTrip_LargeLen(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		lenV, p, c uint32
	}{
		{"small", 1, 0, 1},
		{"uint8", 200, 250, 100},
		{"uint16", 60000, 40000, 50000},
		{"uint32-max", math.MaxUint32, math.MaxUint32, math.MaxUint32},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &format.Fixheader{Magic: format.RowMarker, Len: tc.lenV, CRC32P: tc.p, CRC32C: tc.c}
			var buf [format.FixheaderSize]byte
			format.EncodeFixheader(&buf, h)
			got, err := format.DecodeFixheader(buf)
			require.NoError(t, err)
			assert.Equal(t, h.Len, got.Len)
			assert.Equal(t, h.CRC32P, got.CRC32P)
			assert.Equal(t, h.CRC32C, got.CRC32C)
			assert.Equal(t, h.Magic, got.Magic)
		})
	}
}

func TestFixheader_Decode_Errors(t *testing.T) {
	t.Parallel()

	t.Run("unknown magic", func(t *testing.T) {
		t.Parallel()
		var b [format.FixheaderSize]byte
		b[0], b[1], b[2], b[3] = 'X', 'X', 'X', 'X'
		_, err := format.DecodeFixheader(b)
		require.ErrorIs(t, err, format.ErrUnknownMagic)
	})

	t.Run("bad len uint", func(t *testing.T) {
		t.Parallel()
		var b [format.FixheaderSize]byte
		copy(b[:4], format.RowMarker[:])
		b[4] = 0xc1 // never-used msgpack tag -> readMPUint unexpected tag
		_, err := format.DecodeFixheader(b)
		require.ErrorIs(t, err, format.ErrFixheaderShape)
	})

	t.Run("bad crc32p uint", func(t *testing.T) {
		t.Parallel()
		var b [format.FixheaderSize]byte
		copy(b[:4], format.RowMarker[:])
		b[4] = 0x01 // len fixint
		b[5] = 0xc1 // bad crc32p tag
		_, err := format.DecodeFixheader(b)
		require.ErrorIs(t, err, format.ErrFixheaderShape)
	})

	t.Run("bad padding shape", func(t *testing.T) {
		t.Parallel()
		var b [format.FixheaderSize]byte
		copy(b[:4], format.RowMarker[:])
		b[4], b[5], b[6] = 0x01, 0x00, 0x00 // three fixints
		// remaining bytes from off=7..18 must be a single mp_str consuming
		// exactly 12 bytes; instead put a fixstr that consumes too few.
		b[7] = 0xa0 // fixstr len 0 -> consumes 1 byte, leaving 11 unconsumed
		_, err := format.DecodeFixheader(b)
		require.ErrorIs(t, err, format.ErrFixheaderShape)
	})

	t.Run("padding truncated", func(t *testing.T) {
		t.Parallel()
		var b [format.FixheaderSize]byte
		copy(b[:4], format.RowMarker[:])
		b[4], b[5], b[6] = 0x01, 0x00, 0x00
		b[7] = 0xd9 // str8 wanting a length byte + payload past the buffer end
		b[8] = 0xff // declared length 255 -> overruns
		_, err := format.DecodeFixheader(b)
		require.ErrorIs(t, err, format.ErrFixheaderShape)
	})
}

func TestFixheader_DecodeTxBlock_TooShort(t *testing.T) {
	t.Parallel()

	_, _, _, err := format.DecodeTxBlock([]byte{0x01, 0x02})
	require.ErrorIs(t, err, format.ErrShortFixheader)
}

// ---------------------------------------------------------------------------
// Meta: extra headers, malformed lines, truncation.
// ---------------------------------------------------------------------------

func TestDecodeMeta_Errors(t *testing.T) {
	t.Parallel()

	dec := func(s string) (*format.Meta, error) {
		return decodeMetaString(s)
	}

	t.Run("nil reader", func(t *testing.T) {
		t.Parallel()
		_, err := format.DecodeMeta(nil, format.MetaOptions{})
		require.ErrorIs(t, err, format.ErrNilMetaReader)
	})

	t.Run("truncated before terminator", func(t *testing.T) {
		t.Parallel()
		_, err := dec("XLOG\n0.13\nVersion: x\n")
		require.ErrorIs(t, err, format.ErrMetaTruncated)
	})

	t.Run("unknown filetype", func(t *testing.T) {
		t.Parallel()
		_, err := dec("WUT\n0.13\n\n")
		require.ErrorIs(t, err, format.ErrMetaBadFormat)
	})

	t.Run("bad version", func(t *testing.T) {
		t.Parallel()
		_, err := dec("XLOG\n7.77\n\n")
		require.ErrorIs(t, err, format.ErrMetaBadVersion)
	})

	t.Run("line without colon", func(t *testing.T) {
		t.Parallel()
		_, err := dec("XLOG\n0.13\nnocolonhere\n\n")
		require.ErrorIs(t, err, format.ErrMetaBadFormat)
	})

	t.Run("bad instance uuid", func(t *testing.T) {
		t.Parallel()
		_, err := dec("XLOG\n0.13\nInstance: not-a-uuid\n\n")
		require.ErrorIs(t, err, format.ErrMetaBadFormat)
	})

	t.Run("bad vclock", func(t *testing.T) {
		t.Parallel()
		_, err := dec("XLOG\n0.13\nVClock: {bad}\n\n")
		require.ErrorIs(t, err, format.ErrMetaBadFormat)
	})

	t.Run("bad prevvclock", func(t *testing.T) {
		t.Parallel()
		_, err := dec("XLOG\n0.13\nPrevVClock: {bad}\n\n")
		require.ErrorIs(t, err, format.ErrMetaBadFormat)
	})

	t.Run("truncated after version line", func(t *testing.T) {
		t.Parallel()
		_, err := dec("XLOG\n")
		require.ErrorIs(t, err, format.ErrMetaTruncated)
	})

	t.Run("empty input", func(t *testing.T) {
		t.Parallel()
		_, err := dec("")
		require.ErrorIs(t, err, format.ErrMetaTruncated)
	})
}

func TestDecodeMeta_ExtrasAndCRLF(t *testing.T) {
	t.Parallel()

	// CRLF line endings + an unknown header preserved into Extras, and both
	// VClock + PrevVClock present.
	s := "XLOG\r\n0.13\r\nVersion: 3.0\r\nInstance: 11111111-2222-3333-4444-555555555555\r\n" +
		"VClock: {1: 5}\r\nPrevVClock: {1: 4}\r\nCustomKey: custom-value\r\n\r\n"
	m, err := decodeMetaString(s)
	require.NoError(t, err)
	assert.Equal(t, format.FiletypeXLOG, m.Filetype)
	assert.Equal(t, int64(5), m.VClock[1])
	assert.Equal(t, int64(4), m.PrevVClock[1])
	require.Contains(t, m.Extras, "CustomKey")
	assert.Equal(t, "custom-value", m.Extras["CustomKey"])
}

// ---------------------------------------------------------------------------
// readMPDouble float32 path (through DecodeXRow IPROTO_TIMESTAMP).
// ---------------------------------------------------------------------------

func TestDecodeXRow_Float32Timestamp(t *testing.T) {
	t.Parallel()

	// Header map: {REQUEST_TYPE: INSERT, LSN: 1, TIMESTAMP: float32(1.5)} + body.
	var hdr []byte
	hdr = append(hdr, kv(uint64(iproto.IPROTO_REQUEST_TYPE), mpUint(uint64(iproto.IPROTO_INSERT)))...)
	hdr = append(hdr, kv(uint64(iproto.IPROTO_LSN), mpUint(1))...)
	hdr = append(hdr, kv(uint64(iproto.IPROTO_TIMESTAMP), mpFloat32(1.5))...)
	raw := append(buildMap(3, hdr), 0x80) // empty body map

	row, _, err := format.DecodeXRow(raw)
	require.NoError(t, err)
	assert.InDelta(t, 1.5, row.Timestamp, 1e-6)
}

func TestDecodeXRow_BadTimestamp(t *testing.T) {
	t.Parallel()

	cases := map[string][]byte{
		"wrong type":    {0xa1, 'x'},        // string, not float
		"float32 short": {0xca, 0x00},       // float32 tag, missing bytes
		"float64 short": {0xcb, 0x00, 0x00}, // double tag, missing bytes
	}

	for name, tsVal := range cases {
		tsVal := tsVal
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var hdr []byte
			hdr = append(hdr, kv(uint64(iproto.IPROTO_REQUEST_TYPE), mpUint(uint64(iproto.IPROTO_INSERT)))...)
			hdr = append(hdr, kv(uint64(iproto.IPROTO_TIMESTAMP), tsVal)...)
			raw := append(buildMap(2, hdr), 0x80)

			_, _, err := format.DecodeXRow(raw)
			require.Error(t, err)
		})
	}
}

// ---------------------------------------------------------------------------
// DecodeXRow: error paths + GroupID/StreamID/unknown-key branches.
// ---------------------------------------------------------------------------

func TestDecodeXRow_AllHeaderKeys(t *testing.T) {
	t.Parallel()

	var hdr []byte
	hdr = append(hdr, kv(uint64(iproto.IPROTO_REQUEST_TYPE), mpUint(uint64(iproto.IPROTO_INSERT)))...)
	hdr = append(hdr, kv(uint64(iproto.IPROTO_REPLICA_ID), mpUint(2))...)
	hdr = append(hdr, kv(uint64(iproto.IPROTO_GROUP_ID), mpUint(1))...)
	hdr = append(hdr, kv(uint64(iproto.IPROTO_LSN), mpUint(10))...)
	hdr = append(hdr, kv(uint64(iproto.IPROTO_TSN), mpUint(2))...) // diff -> tsn = lsn-2 = 8
	hdr = append(hdr, kv(uint64(iproto.IPROTO_FLAGS), mpUint(uint64(iproto.IPROTO_FLAG_WAIT_SYNC)))...)
	hdr = append(hdr, kv(uint64(iproto.IPROTO_STREAM_ID), mpUint(77))...)
	hdr = append(hdr, kv(0x7d, []byte{0x05})...) // unknown key
	raw := append(buildMap(8, hdr), 0x80)

	row, _, err := format.DecodeXRow(raw)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), row.ReplicaID)
	assert.Equal(t, uint32(1), row.GroupID)
	assert.Equal(t, int64(10), row.LSN)
	assert.Equal(t, int64(8), row.TSN)
	assert.Equal(t, uint64(77), row.StreamID)
	assert.NotZero(t, row.Flags&iproto.IPROTO_FLAG_WAIT_SYNC)
}

func TestDecodeXRow_Errors(t *testing.T) {
	t.Parallel()

	hdrPrefix := func(entries int, body []byte) []byte { return append(mpFixmap(entries), body...) }

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		_, _, err := format.DecodeXRow(nil)
		require.ErrorIs(t, err, format.ErrEmptyXRowInput)
	})

	t.Run("bad header map", func(t *testing.T) {
		t.Parallel()
		_, _, err := format.DecodeXRow([]byte{0x01})
		require.Error(t, err)
	})

	t.Run("bad header key", func(t *testing.T) {
		t.Parallel()
		_, _, err := format.DecodeXRow([]byte{0x81, 0xcc})
		require.Error(t, err)
	})

	t.Run("bad type value", func(t *testing.T) {
		t.Parallel()
		raw := hdrPrefix(1, kv(uint64(iproto.IPROTO_REQUEST_TYPE), []byte{0xa1, 'x'}))
		_, _, err := format.DecodeXRow(raw)
		require.Error(t, err)
	})

	t.Run("bad replica_id value", func(t *testing.T) {
		t.Parallel()
		raw := hdrPrefix(1, kv(uint64(iproto.IPROTO_REPLICA_ID), []byte{0xa1, 'x'}))
		_, _, err := format.DecodeXRow(raw)
		require.Error(t, err)
	})

	t.Run("bad group_id value", func(t *testing.T) {
		t.Parallel()
		raw := hdrPrefix(1, kv(uint64(iproto.IPROTO_GROUP_ID), []byte{0xa1, 'x'}))
		_, _, err := format.DecodeXRow(raw)
		require.Error(t, err)
	})

	t.Run("bad lsn value", func(t *testing.T) {
		t.Parallel()
		raw := hdrPrefix(1, kv(uint64(iproto.IPROTO_LSN), []byte{0xa1, 'x'}))
		_, _, err := format.DecodeXRow(raw)
		require.Error(t, err)
	})

	t.Run("bad tsn value", func(t *testing.T) {
		t.Parallel()
		raw := hdrPrefix(1, kv(uint64(iproto.IPROTO_TSN), []byte{0xa1, 'x'}))
		_, _, err := format.DecodeXRow(raw)
		require.Error(t, err)
	})

	t.Run("bad flags value", func(t *testing.T) {
		t.Parallel()
		raw := hdrPrefix(1, kv(uint64(iproto.IPROTO_FLAGS), []byte{0xa1, 'x'}))
		_, _, err := format.DecodeXRow(raw)
		require.Error(t, err)
	})

	t.Run("bad stream_id value", func(t *testing.T) {
		t.Parallel()
		raw := hdrPrefix(1, kv(uint64(iproto.IPROTO_STREAM_ID), []byte{0xa1, 'x'}))
		_, _, err := format.DecodeXRow(raw)
		require.Error(t, err)
	})

	t.Run("bad unknown-key value", func(t *testing.T) {
		t.Parallel()
		raw := hdrPrefix(1, kv(0x7d, []byte{0xdc, 0x00})) // array16 short
		_, _, err := format.DecodeXRow(raw)
		require.Error(t, err)
	})

	t.Run("bad body", func(t *testing.T) {
		t.Parallel()
		// Valid header (INSERT) then a truncated body value.
		raw := append(buildMap(1, kv(uint64(iproto.IPROTO_REQUEST_TYPE), mpUint(uint64(iproto.IPROTO_INSERT)))), 0xdc, 0x00)
		_, _, err := format.DecodeXRow(raw)
		require.Error(t, err)
	})

	t.Run("no body bytes left", func(t *testing.T) {
		t.Parallel()
		// INSERT header but nothing after — liberal: nil body, no error.
		raw := buildMap(1, kv(uint64(iproto.IPROTO_REQUEST_TYPE), mpUint(uint64(iproto.IPROTO_INSERT))))
		row, _, err := format.DecodeXRow(raw)
		require.NoError(t, err)
		assert.Nil(t, row.BodyRaw)
	})
}

// ---------------------------------------------------------------------------
// tx: multi-row transactions, compressed round-trip, decode error paths.
// ---------------------------------------------------------------------------

func TestTxBlock_NilRow(t *testing.T) {
	t.Parallel()

	rows := []format.XRow{{Type: iproto.IPROTO_INSERT}} // LSN==TSN==0
	_, err := format.EncodeTxBlock(rows, format.TxOptions{Compression: format.Compression{Disabled: true}})
	require.NoError(t, err)
}

func TestTxBlock_MultiRow_Compressed_RoundTrip(t *testing.T) {
	t.Parallel()

	// Three rows in one tx, large enough to compress.
	big := make([]byte, 3000)
	for i := range big {
		big[i] = byte(i * 7)
	}
	body := wrapMap(append([]byte{0xc6, 0x00, 0x00, 0x0b, 0xb8}, big...)) // bin32 len 3000

	rows := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 10, TSN: 10, Flags: 0, BodyRaw: body},
		{Type: iproto.IPROTO_INSERT, LSN: 11, TSN: 10, Flags: 0, BodyRaw: []byte{0x80}},
		{Type: iproto.IPROTO_INSERT, LSN: 12, TSN: 10, Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: []byte{0x80}},
	}
	blob, err := format.EncodeTxBlock(rows, format.TxOptions{})
	require.NoError(t, err)
	require.Equal(t, format.ZRowMarker, [4]byte{blob[0], blob[1], blob[2], blob[3]})

	slices, _, n, err := format.DecodeTxBlock(blob)
	require.NoError(t, err)
	assert.Equal(t, len(blob), n)
	require.Len(t, slices, 3)
}

func TestTxBlock_AppendTxBlockPayload_MultiRow(t *testing.T) {
	t.Parallel()

	rows := []format.XRow{
		{Type: iproto.IPROTO_INSERT, LSN: 1, TSN: 1, Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: []byte{0x80}},
		{Type: iproto.IPROTO_NOP, LSN: 2, TSN: 2, Flags: iproto.IPROTO_FLAG_COMMIT},
	}
	payload, err := format.AppendTxBlockPayload(nil, rows)
	require.NoError(t, err)

	got, err := format.AppendFramedBlock(nil, payload, format.Compression{Disabled: true})
	require.NoError(t, err)
	slices, _, _, err := format.DecodeTxBlock(got)
	require.NoError(t, err)
	require.Len(t, slices, 2)
}

func TestTxBlock_EOFMarker(t *testing.T) {
	t.Parallel()

	// A fixheader carrying the EOF marker magic decodes as ErrEOFMarkerBlock.
	h := &format.Fixheader{Magic: format.EOFMarker}
	var fh [format.FixheaderSize]byte
	format.EncodeFixheader(&fh, h)
	_, _, _, err := format.DecodeTxBlock(fh[:])
	require.ErrorIs(t, err, format.ErrEOFMarkerBlock)
}

func TestTxBlock_PayloadLenExceeds(t *testing.T) {
	t.Parallel()

	rows := []format.XRow{{Type: iproto.IPROTO_INSERT, LSN: 1, TSN: 1, Flags: iproto.IPROTO_FLAG_COMMIT, BodyRaw: []byte{0x80}}}
	blob, err := format.EncodeTxBlock(rows, format.TxOptions{Compression: format.Compression{Disabled: true}})
	require.NoError(t, err)
	// Truncate the payload so h.Len exceeds the remaining bytes.
	_, _, _, err = format.DecodeTxBlock(blob[:len(blob)-1])
	require.ErrorIs(t, err, format.ErrShortFixheader)
}

func TestTxBlock_BadCompressedPayload(t *testing.T) {
	t.Parallel()

	// Build a ZRowMarker block whose payload is not valid zstd. CRC must
	// match so we reach the DecompressTx error path.
	payload := []byte{0x01, 0x02, 0x03, 0x04}
	crc := format.CRC32C(payload)
	h := &format.Fixheader{Magic: format.ZRowMarker, Len: uint32(len(payload)), CRC32C: crc}
	var fh [format.FixheaderSize]byte
	format.EncodeFixheader(&fh, h)
	blob := append(fh[:], payload...)

	_, _, _, err := format.DecodeTxBlock(blob)
	require.Error(t, err)
}

func TestTxBlock_SplitRows_MissingType(t *testing.T) {
	t.Parallel()

	// A header map with one entry that is NOT IPROTO_REQUEST_TYPE -> splitRows
	// errors with ErrMissingType. Wrap in a valid plain tx block.
	payload := buildMap(1, kv(uint64(iproto.IPROTO_LSN), mpUint(1)))
	got, err := format.AppendFramedBlock(nil, payload, format.Compression{Disabled: true})
	require.NoError(t, err)
	_, _, _, err = format.DecodeTxBlock(got)
	require.ErrorIs(t, err, format.ErrMissingType)
}

func TestTxBlock_SplitRows_BadHeader(t *testing.T) {
	t.Parallel()

	// Payload that is not a valid msgpack map header.
	payload := []byte{0x01}
	got, err := format.AppendFramedBlock(nil, payload, format.Compression{Disabled: true})
	require.NoError(t, err)
	_, _, _, err = format.DecodeTxBlock(got)
	require.Error(t, err)
}

func TestTxBlock_SplitRows_BadHeaderValue(t *testing.T) {
	t.Parallel()

	// Header with a non-type key whose value is truncated.
	payload := buildMap(2, append(
		kv(uint64(iproto.IPROTO_REQUEST_TYPE), mpUint(uint64(iproto.IPROTO_INSERT))),
		kv(0x7d, []byte{0xdc, 0x00})...)) // array16 short
	got, err := format.AppendFramedBlock(nil, payload, format.Compression{Disabled: true})
	require.NoError(t, err)
	_, _, _, err = format.DecodeTxBlock(got)
	require.Error(t, err)
}

func TestTxBlock_SplitRows_BadType(t *testing.T) {
	t.Parallel()

	// REQUEST_TYPE present but its value is malformed.
	payload := buildMap(1, kv(uint64(iproto.IPROTO_REQUEST_TYPE), []byte{0xcc})) // uint8 missing byte
	got, err := format.AppendFramedBlock(nil, payload, format.Compression{Disabled: true})
	require.NoError(t, err)
	_, _, _, err = format.DecodeTxBlock(got)
	require.Error(t, err)
}

func TestTxBlock_SplitRows_NopThenInsert(t *testing.T) {
	t.Parallel()

	// NOP row (no body) immediately followed by an INSERT row with a body.
	var p []byte
	p = append(p, buildMap(1, kv(uint64(iproto.IPROTO_REQUEST_TYPE), mpUint(uint64(iproto.IPROTO_NOP))))...)
	p = append(p, buildMap(1, kv(uint64(iproto.IPROTO_REQUEST_TYPE), mpUint(uint64(iproto.IPROTO_INSERT))))...)
	p = append(p, 0x80) // body for the INSERT

	got, err := format.AppendFramedBlock(nil, p, format.Compression{Disabled: true})
	require.NoError(t, err)
	slices, _, _, err := format.DecodeTxBlock(got)
	require.NoError(t, err)
	require.Len(t, slices, 2)
}

func TestTxBlock_SplitRows_BadBody(t *testing.T) {
	t.Parallel()

	// INSERT header followed by a malformed body value.
	p := append(buildMap(1, kv(uint64(iproto.IPROTO_REQUEST_TYPE), mpUint(uint64(iproto.IPROTO_INSERT)))), 0xdc, 0x00)
	got, err := format.AppendFramedBlock(nil, p, format.Compression{Disabled: true})
	require.NoError(t, err)
	_, _, _, err = format.DecodeTxBlock(got)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// compress: round-trip + decode error paths (DecompressTx, encoder/decoder
// construction via pools).
// ---------------------------------------------------------------------------

func TestCompressTx_RoundTrip(t *testing.T) {
	t.Parallel()

	payload := make([]byte, 5000)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	comp, err := format.CompressTx(payload)
	require.NoError(t, err)
	out, err := format.DecompressTx(comp, nil)
	require.NoError(t, err)
	assert.Equal(t, payload, out)
}

func TestCompressTxInto_CustomLevel(t *testing.T) {
	t.Parallel()

	payload := []byte("hello world hello world hello world")
	comp, err := format.CompressTxInto(nil, payload, 1)
	require.NoError(t, err)
	out, err := format.DecompressTx(comp, make([]byte, 0, 64))
	require.NoError(t, err)
	assert.Equal(t, payload, out)
}

func TestDecompressTx_BadFrame(t *testing.T) {
	t.Parallel()

	_, err := format.DecompressTx([]byte{0xde, 0xad, 0xbe, 0xef}, nil)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// mp helpers reached through the public encode/decode API.
// ---------------------------------------------------------------------------

// TestEncodeXRow_LargeUints_RoundTrip drives appendMPUint through every width
// (uint8/16/32/64) via header fields and round-trips back through DecodeXRow,
// which exercises readMPUint's matching width branches.
func TestEncodeXRow_LargeUints_RoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		replica uint32
		lsn     int64
		stream  uint64
	}{
		{"uint8", 200, 0, 0},
		{"uint16", 0, 60000, 0},
		{"uint32", 0, 0, 4000000000},
		{"uint64", 0, 0, 1 << 40},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &format.XRow{
				Type:      iproto.IPROTO_INSERT,
				ReplicaID: tc.replica,
				LSN:       tc.lsn,
				StreamID:  tc.stream,
				TSN:       1, // make multi-stmt so fields aren't elided unexpectedly
				BodyRaw:   []byte{0x80},
			}
			buf, err := format.EncodeXRow(nil, r)
			require.NoError(t, err)
			got, _, err := format.DecodeXRow(buf)
			require.NoError(t, err)
			assert.Equal(t, tc.replica, got.ReplicaID)
			assert.Equal(t, tc.stream, got.StreamID)
		})
	}
}

// TestReadMPUint_ShortInputs feeds DecodeXRow header values whose uint tag
// promises more bytes than are present, hitting each ErrShortInput branch in
// readMPUint.
func TestReadMPUint_ShortInputs(t *testing.T) {
	t.Parallel()

	// Each entry: a value tag that needs N trailing bytes, given fewer.
	for _, tag := range [][]byte{
		{0xcc},             // uint8, no byte
		{0xcd, 0x00},       // uint16, 1 of 2
		{0xce, 0x00, 0x00}, // uint32, 2 of 4
		{0xcf, 0x00},       // uint64, 1 of 8
	} {
		raw := append(mpFixmap(1), kv(uint64(iproto.IPROTO_LSN), tag)...)
		_, _, err := format.DecodeXRow(raw)
		require.Errorf(t, err, "tag %x should error", tag)
	}
}

// TestReadMPMapLen_Map16Map32 feeds DecodeXRow a header using map16/map32
// prefixes, plus their short variants.
func TestReadMPMapLen_Variants(t *testing.T) {
	t.Parallel()

	t.Run("map16 header", func(t *testing.T) {
		t.Parallel()
		// map16 with 1 entry: 0xde 0x00 0x01.
		raw := []byte{0xde, 0x00, 0x01}
		raw = append(raw, kv(uint64(iproto.IPROTO_REQUEST_TYPE), mpUint(uint64(iproto.IPROTO_NOP)))...)
		row, _, err := format.DecodeXRow(raw)
		require.NoError(t, err)
		assert.Equal(t, iproto.IPROTO_NOP, row.Type)
	})

	t.Run("map32 header", func(t *testing.T) {
		t.Parallel()
		raw := []byte{0xdf, 0x00, 0x00, 0x00, 0x01}
		raw = append(raw, kv(uint64(iproto.IPROTO_REQUEST_TYPE), mpUint(uint64(iproto.IPROTO_NOP)))...)
		row, _, err := format.DecodeXRow(raw)
		require.NoError(t, err)
		assert.Equal(t, iproto.IPROTO_NOP, row.Type)
	})

	t.Run("map16 short", func(t *testing.T) {
		t.Parallel()
		_, _, err := format.DecodeXRow([]byte{0xde, 0x00})
		require.Error(t, err)
	})

	t.Run("map32 short", func(t *testing.T) {
		t.Parallel()
		_, _, err := format.DecodeXRow([]byte{0xdf, 0x00, 0x00})
		require.Error(t, err)
	})
}

// TestSkipMP_AllTypes drives mpHead over every msgpack type tag by placing a
// value of that type as an *unknown* header key's value (DecodeXRow skips it
// via skipMP -> mpHead). A trailing valid REQUEST_TYPE ensures the row parses.
func TestSkipMP_AllTypes(t *testing.T) {
	t.Parallel()

	str := func(tag byte, lenBytes []byte, payloadLen int) []byte {
		v := append([]byte{tag}, lenBytes...)
		return append(v, make([]byte, payloadLen)...)
	}

	values := map[string][]byte{
		"posfixint": {0x05},
		"negfixint": {0xff},
		"nil":       {0xc0},
		"false":     {0xc2},
		"true":      {0xc3},
		"float32":   {0xca, 0, 0, 0, 0},
		"float64":   {0xcb, 0, 0, 0, 0, 0, 0, 0, 0},
		"uint8":     {0xcc, 0x01},
		"uint16":    {0xcd, 0, 1},
		"uint32":    {0xce, 0, 0, 0, 1},
		"uint64":    {0xcf, 0, 0, 0, 0, 0, 0, 0, 1},
		"int8":      {0xd0, 0xff},
		"int16":     {0xd1, 0xff, 0xff},
		"int32":     {0xd2, 0xff, 0xff, 0xff, 0xff},
		"int64":     {0xd3, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		"fixstr":    {0xa3, 'a', 'b', 'c'},
		"str8":      str(0xd9, []byte{0x02}, 2),
		"str16":     str(0xda, []byte{0x00, 0x02}, 2),
		"str32":     str(0xdb, []byte{0x00, 0x00, 0x00, 0x02}, 2),
		"bin8":      str(0xc4, []byte{0x02}, 2),
		"bin16":     str(0xc5, []byte{0x00, 0x02}, 2),
		"bin32":     str(0xc6, []byte{0x00, 0x00, 0x00, 0x02}, 2),
		"fixarray":  {0x92, 0x01, 0x02},
		"array16":   {0xdc, 0x00, 0x01, 0x07},
		"array32":   {0xdd, 0x00, 0x00, 0x00, 0x01, 0x07},
		"fixmapval": {0x81, 0x01, 0x02},
		"map16":     {0xde, 0x00, 0x01, 0x01, 0x02},
		"map32":     {0xdf, 0x00, 0x00, 0x00, 0x01, 0x01, 0x02},
		"fixext1":   {0xd4, 0x01, 0xaa},
		"fixext2":   {0xd5, 0x01, 0xaa, 0xbb},
		"fixext4":   {0xd6, 0x01, 1, 2, 3, 4},
		"fixext8":   {0xd7, 0x01, 1, 2, 3, 4, 5, 6, 7, 8},
		"fixext16":  append([]byte{0xd8, 0x01}, make([]byte, 16)...),
		"ext8":      {0xc7, 0x02, 0x01, 0xaa, 0xbb},
		"ext16":     {0xc8, 0x00, 0x02, 0x01, 0xaa, 0xbb},
		"ext32":     {0xc9, 0x00, 0x00, 0x00, 0x02, 0x01, 0xaa, 0xbb},
	}

	for name, val := range values {
		val := val
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// {unknownKey: val, REQUEST_TYPE: NOP} as a 2-entry header.
			body := append(kv(0x7d, val), kv(uint64(iproto.IPROTO_REQUEST_TYPE), mpUint(uint64(iproto.IPROTO_NOP)))...)
			raw := buildMap(2, body)
			row, _, err := format.DecodeXRow(raw)
			require.NoErrorf(t, err, "skip %s", name)
			assert.Equal(t, iproto.IPROTO_NOP, row.Type)
		})
	}
}

// TestSkipMP_TruncatedTypes hits the short-header / short-payload error paths
// of mpHead for the variable and fixed-width types.
func TestSkipMP_TruncatedTypes(t *testing.T) {
	t.Parallel()

	bad := map[string][]byte{
		"uint8 short":      {0xcc},
		"uint16 short":     {0xcd, 0x00},
		"uint32 short":     {0xce, 0x00},
		"uint64 short":     {0xcf, 0x00},
		"fixstr short":     {0xa3, 'a'},
		"str8 header":      {0xd9},
		"str8 payload":     {0xd9, 0x05, 'a'},
		"str16 header":     {0xda, 0x00},
		"str16 payload":    {0xda, 0x00, 0x05, 'a'},
		"str32 header":     {0xdb, 0x00, 0x00},
		"str32 payload":    {0xdb, 0x00, 0x00, 0x00, 0x05, 'a'},
		"array16 header":   {0xdc, 0x00},
		"array32 header":   {0xdd, 0x00, 0x00},
		"map16 header":     {0xde, 0x00},
		"map32 header":     {0xdf, 0x00, 0x00},
		"ext8 header":      {0xc7},
		"ext8 payload":     {0xc7, 0x05, 0x01, 'a'},
		"ext16 header":     {0xc8, 0x00},
		"ext16 payload":    {0xc8, 0x00, 0x05, 0x01, 'a'},
		"ext32 header":     {0xc9, 0x00, 0x00},
		"ext32 payload":    {0xc9, 0x00, 0x00, 0x00, 0x05, 0x01, 'a'},
		"fixext truncated": {0xd7, 0x01, 0x00},
		"unsupported tag":  {0xc1},
	}

	for name, val := range bad {
		val := val
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Place the malformed value as an unknown header key's value.
			raw := append(mpFixmap(1), kv(0x7d, val)...)
			_, _, err := format.DecodeXRow(raw)
			require.Errorf(t, err, "%s should error", name)
		})
	}
}

// TestSkipMP_EmptyValue covers skipMP's empty-input guard (header claims an
// entry but no bytes follow).
func TestSkipMP_TruncatedInput(t *testing.T) {
	t.Parallel()

	// fixmap(1) with one unknown key but no value bytes at all.
	raw := append(mpFixmap(1), mpUint(0x7d)...)
	_, _, err := format.DecodeXRow(raw)
	require.Error(t, err)
}

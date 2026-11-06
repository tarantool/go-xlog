package format

import (
	"errors"
	"fmt"

	"github.com/tarantool/go-iproto"
)

// XRow is one row in a tx-block payload.
//
// On disk an xrow is a *header* msgpack map followed by an optional *body*
// msgpack map. NOP rows (IPROTO_NOP) have no body — the row ends after the
// header map (src/box/xrow.c:272-282). All other types carry exactly one
// body map immediately after the header.
//
// BodyRaw holds the body map bytes verbatim (decoded on demand). It aliases
// the input. For NOP rows it is nil.
type XRow struct {
	Type      iproto.Type
	ReplicaID uint32
	GroupID   uint32
	LSN       int64
	TSN       int64
	Timestamp float64
	// Flags carries every flag bit *except* IPROTO_FLAG_COMMIT, plus
	// IPROTO_FLAG_COMMIT when the row is a transaction-terminating row. The
	// encoder is responsible for the single-stmt vs multi-stmt logic of
	// whether to actually emit IPROTO_FLAGS / IPROTO_FLAG_COMMIT on the wire.
	Flags    iproto.Flag
	BodyRaw  []byte
	StreamID uint64 // Optional, wire-only; usually zero in xlog.
	// Sync is IPROTO_SYNC, the id that correlates an IPROTO request with its
	// response. Wire-only: a row written to an xlog carries none.
	Sync uint64
	// SchemaVersion is IPROTO_SCHEMA_VERSION, the schema version a response
	// (or a request that checks it) carries. Wire-only, like Sync.
	SchemaVersion uint64
	// Present records which header keys the decoded row carried on the wire,
	// so a key that is absent can be told from a key present with the value
	// 0: a replication heartbeat, for one, is a row without IPROTO_LSN
	// (!HasLSN()), not a row whose LSN is 0. Only DecodeXRowInto (and
	// DecodeXRow) sets it, and it describes the row as decoded: changing a
	// field afterwards does not update it.
	//
	// DECODE-ONLY AND ADVISORY: EncodeXRow never consults Present. It infers
	// which keys to emit from the zero-ness of the fields, as it always has,
	// because every XRow built by hand - in a test, a writer, a filter that
	// rewrites rows - has Present == 0, and an encoder that honoured it would
	// emit an empty header for all of them. The asymmetry is deliberate; do
	// not "fix" it. To drop a key from an encoded row, zero its field.
	Present Present
}

// Present is a set of the header keys of a row, one bit per key the decoder
// recognises. The bit of a key is 1 << its IPROTO code, so PresentLSN is
// 1 << IPROTO_LSN.
type Present uint32

// The header keys of a row recorded in XRow.Present.
const (
	PresentType          Present = 1 << iproto.IPROTO_REQUEST_TYPE
	PresentSync          Present = 1 << iproto.IPROTO_SYNC
	PresentReplicaID     Present = 1 << iproto.IPROTO_REPLICA_ID
	PresentLSN           Present = 1 << iproto.IPROTO_LSN
	PresentTimestamp     Present = 1 << iproto.IPROTO_TIMESTAMP
	PresentSchemaVersion Present = 1 << iproto.IPROTO_SCHEMA_VERSION
	PresentGroupID       Present = 1 << iproto.IPROTO_GROUP_ID
	// PresentTSN is set only when IPROTO_TSN is on the wire. The TSN the
	// decoder infers from the LSN of a single-statement row, which carries no
	// IPROTO_TSN, does not set it.
	PresentTSN Present = 1 << iproto.IPROTO_TSN
	// PresentFlags is set only when IPROTO_FLAGS is on the wire. The
	// IPROTO_FLAG_COMMIT the decoder infers for a single-statement row does
	// not set it.
	PresentFlags    Present = 1 << iproto.IPROTO_FLAGS
	PresentStreamID Present = 1 << iproto.IPROTO_STREAM_ID
)

// HasType reports whether the decoded row carried IPROTO_REQUEST_TYPE.
func (r *XRow) HasType() bool { return r.Present&PresentType != 0 }

// HasSync reports whether the decoded row carried IPROTO_SYNC.
func (r *XRow) HasSync() bool { return r.Present&PresentSync != 0 }

// HasReplicaID reports whether the decoded row carried IPROTO_REPLICA_ID.
func (r *XRow) HasReplicaID() bool { return r.Present&PresentReplicaID != 0 }

// HasLSN reports whether the decoded row carried IPROTO_LSN. A heartbeat of
// the replication stream carries none.
func (r *XRow) HasLSN() bool { return r.Present&PresentLSN != 0 }

// HasTimestamp reports whether the decoded row carried IPROTO_TIMESTAMP.
func (r *XRow) HasTimestamp() bool { return r.Present&PresentTimestamp != 0 }

// HasSchemaVersion reports whether the decoded row carried
// IPROTO_SCHEMA_VERSION.
func (r *XRow) HasSchemaVersion() bool { return r.Present&PresentSchemaVersion != 0 }

// HasGroupID reports whether the decoded row carried IPROTO_GROUP_ID.
func (r *XRow) HasGroupID() bool { return r.Present&PresentGroupID != 0 }

// HasTSN reports whether the decoded row carried IPROTO_TSN, that is whether
// it belongs to a multi-statement transaction. It is false for a
// single-statement row, whose TSN the decoder infers from its LSN.
func (r *XRow) HasTSN() bool { return r.Present&PresentTSN != 0 }

// HasFlags reports whether the decoded row carried IPROTO_FLAGS. It is false
// for a single-statement row with no other flag, whose IPROTO_FLAG_COMMIT the
// decoder infers.
func (r *XRow) HasFlags() bool { return r.Present&PresentFlags != 0 }

// HasStreamID reports whether the decoded row carried IPROTO_STREAM_ID.
func (r *XRow) HasStreamID() bool { return r.Present&PresentStreamID != 0 }

// IsCommit reports whether IPROTO_FLAG_COMMIT is set on r.
func (r *XRow) IsCommit() bool { return r.Flags&iproto.IPROTO_FLAG_COMMIT != 0 }

// xrowHeaderKeys is the number of header keys EncodeXRow can emit:
// REQUEST_TYPE, SYNC, SCHEMA_VERSION, REPLICA_ID, GROUP_ID, LSN, TIMESTAMP,
// TSN, STREAM_ID and FLAGS.
const xrowHeaderKeys = 10

// Sentinel errors for xrow encode/decode.
var (
	ErrNilRow         = errors.New("nil row")
	ErrEmptyXRowInput = errors.New("empty input")
)

// EncodeXRow appends a complete xrow record to buf and returns the
// new buf. The record consists of:
//
//   - a header map (per src/box/xrow.c:331-444)
//   - the BodyRaw bytes appended verbatim
//
// Encoding rules (src/box/xrow.c:402-410):
//
//   - IPROTO_REQUEST_TYPE is always present.
//   - IPROTO_SYNC / IPROTO_SCHEMA_VERSION / IPROTO_REPLICA_ID / IPROTO_LSN /
//     IPROTO_GROUP_ID / IPROTO_TIMESTAMP / IPROTO_STREAM_ID present iff nonzero.
//   - Single-statement tx (TSN == LSN && IsCommit && TSN != 0):
//     omit IPROTO_TSN AND clear IPROTO_FLAG_COMMIT in the encoded flags.
//   - Multi-statement tx (TSN != LSN || !IsCommit):
//     emit IPROTO_TSN as the diff (LSN - TSN).
//     If IsCommit && TSN != LSN (last row of multi-stmt), OR IPROTO_FLAG_COMMIT
//     back into the encoded flags.
//   - IPROTO_FLAGS is emitted iff the resulting flags value is non-zero.
//
// The caller is responsible for BodyRaw being a valid msgpack map (or nil
// for NOP rows).
//
// EncodeXRow never reads r.Present: a key is emitted by the rules above,
// from the value of its field alone. Present is decode-only, and every XRow
// built by hand has Present == 0; gating a key on it would encode such rows
// with an empty header (see XRow.Present).
//
// Caveat: this function mirrors Tarantool's `if (header->tsn != 0)` guard
// (src/box/xrow.c:402). A row with LSN == TSN == 0 will skip the TSN/COMMIT
// emission block entirely. For a single-statement tx that is harmless — the
// decoder also infers `is_commit = true` when IPROTO_TSN is absent. For a
// multi-row tx where some rows legitimately have LSN == 0, the encoded
// output would be ambiguous (Tarantool would treat each as an independent
// single-stmt tx). The writer package's assignTxIDs avoids this by setting
// TSN = rows[0].LSN before encoding; callers using EncodeXRow directly
// must ensure rows in a multi-row tx have non-zero LSN.
//
// The keys are emitted in the order of xrow_header_encode
// (src/box/xrow.c:338-452): REQUEST_TYPE, SYNC, REPLICA_ID, GROUP_ID, LSN,
// TIMESTAMP, TSN, STREAM_ID, FLAGS, so a header Tarantool would write encodes
// to the same bytes. xrow_header_encode never writes IPROTO_SCHEMA_VERSION;
// only iproto_header_encode (src/box/xrow.c:497-515) does, in a response
// header, right after IPROTO_SYNC, so EncodeXRow emits it there too.
func EncodeXRow(buf []byte, r *XRow) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("format: EncodeXRow: %w", ErrNilRow)
	}
	// Decide which fields are present and at what size. Presence is the
	// zero-ness of a field, never r.Present: see XRow.Present.
	// We use a small fixed array rather than allocating a slice of pairs; it
	// holds one entry per header key the encoder can emit.
	var hdr [xrowHeaderKeys]struct {
		key iproto.Key
		fn  func([]byte) []byte
	}

	n := 0
	add := func(k iproto.Key, fn func([]byte) []byte) {
		hdr[n].key = k
		hdr[n].fn = fn
		n++
	}

	add(iproto.IPROTO_REQUEST_TYPE, func(b []byte) []byte { return appendMPUint(b, uint64(r.Type)) }) //nolint:gosec // G115: request type is a small protocol number

	if r.Sync != 0 {
		sync := r.Sync

		add(iproto.IPROTO_SYNC, func(b []byte) []byte { return appendMPUint(b, sync) })
	}

	if r.SchemaVersion != 0 {
		sv := r.SchemaVersion

		add(iproto.IPROTO_SCHEMA_VERSION, func(b []byte) []byte { return appendMPUint(b, sv) })
	}

	if r.ReplicaID != 0 {
		add(iproto.IPROTO_REPLICA_ID, func(b []byte) []byte { return appendMPUint(b, uint64(r.ReplicaID)) })
	}

	if r.GroupID != 0 {
		add(iproto.IPROTO_GROUP_ID, func(b []byte) []byte { return appendMPUint(b, uint64(r.GroupID)) })
	}

	if r.LSN != 0 {
		add(iproto.IPROTO_LSN, func(b []byte) []byte { return appendMPUint(b, uint64(r.LSN)) }) //nolint:gosec // G115: LSN is non-negative in this branch (r.LSN != 0 guard above), msgpack uint
	}

	if r.Timestamp != 0 {
		ts := r.Timestamp

		add(iproto.IPROTO_TIMESTAMP, func(b []byte) []byte { return appendMPDouble(b, ts) })
	}

	// Single-stmt vs multi-stmt logic. Tarantool/src/box/xrow.c:393-423.
	flagsToEncode := r.Flags &^ iproto.IPROTO_FLAG_COMMIT
	if r.TSN != 0 {
		if r.TSN != r.LSN || !r.IsCommit() {
			diff := r.LSN - r.TSN

			add(iproto.IPROTO_TSN, func(b []byte) []byte { return appendMPUint(b, uint64(diff)) }) //nolint:gosec // G115: tsn diff (LSN-TSN) is non-negative, encoded as msgpack uint
		}

		if r.IsCommit() && r.TSN != r.LSN {
			flagsToEncode |= iproto.IPROTO_FLAG_COMMIT
		}
	}

	if r.StreamID != 0 {
		sid := r.StreamID

		add(iproto.IPROTO_STREAM_ID, func(b []byte) []byte { return appendMPUint(b, sid) })
	}

	if flagsToEncode != 0 {
		flagsLocal := flagsToEncode

		add(iproto.IPROTO_FLAGS, func(b []byte) []byte { return appendMPUint(b, uint64(flagsLocal)) }) //nolint:gosec // G115: flags is a small bitset
	}

	// Header map prefix.
	buf = appendMPMapHeader(buf, n)
	for i := range n {
		buf = appendMPUint(buf, uint64(hdr[i].key)) //nolint:gosec // G115: header key is a small protocol number
		buf = hdr[i].fn(buf)
	}
	// Body bytes (nil for NOP).
	if len(r.BodyRaw) > 0 {
		buf = append(buf, r.BodyRaw...)
	}

	return buf, nil
}

// DecodeXRow parses one xrow record at b[0:] and returns the row plus the
// number of bytes consumed (header + body). Unknown header keys are
// skipped via skipMP, preserving forward compatibility with future IPROTO
// keys (strict on shape, forgiving on unknown keys).
//
// The "is there a body?" question is answered as: NOP rows (IPROTO_NOP) have
// no body; everything else has a single trailing msgpack value (the body
// map). This matches src/box/xrow.c:272-282.
//
// Single-statement tx detection: if IPROTO_TSN was absent, set
// TSN = LSN and Flags |= IPROTO_FLAG_COMMIT.
//
// The row's Present records the header keys found on the wire; the TSN and
// IPROTO_FLAG_COMMIT inferred for a single-statement row set no bit. Its
// BodyRaw aliases b; see Aliasing in the package doc.
func DecodeXRow(b []byte) (*XRow, int, error) {
	r := &XRow{}

	n, err := DecodeXRowInto(b, r)
	if err != nil {
		return nil, 0, err
	}

	return r, n, nil
}

// DecodeXRowInto parses one xrow record at b[0:] into dst, fully overwriting
// every field (including zeroing BodyRaw / StreamID / Sync / SchemaVersion /
// Flags / Present so a reused dst
// carries no state from a prior row), and returns the number of bytes
// consumed. It is the zero-allocation core of DecodeXRow: a hot read loop can
// reuse a single *XRow — or an arena slot — across rows with no per-row heap
// allocation. As with DecodeXRow, dst.BodyRaw aliases b; see Aliasing in the
// package doc.
func DecodeXRowInto(b []byte, dst *XRow) (int, error) {
	if len(b) == 0 {
		return 0, fmt.Errorf("format: DecodeXRow: %w", ErrEmptyXRowInput)
	}

	*dst = XRow{}

	entries, hdrOff, err := readMPMapLen(b)
	if err != nil {
		return 0, fmt.Errorf("format: DecodeXRow: header map: %w", err)
	}

	off := hdrOff
	r := dst
	sawTSN := false

	for i := range entries {
		// Each entry is (key, value); both are msgpack values.
		key, n, err := readMPUint(b[off:])
		if err != nil {
			return 0, fmt.Errorf("format: DecodeXRow: header key %d: %w", i, err)
		}

		off += n

		switch iproto.Key(key) { //nolint:gosec // G115: msgpack header key is a small protocol number
		case iproto.IPROTO_REQUEST_TYPE:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: type: %w", err)
			}

			r.Type = iproto.Type(v) //nolint:gosec // G115: request type is a small enum bounded by iproto.Type
			r.Present |= PresentType
			off += n
		case iproto.IPROTO_SYNC:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: sync: %w", err)
			}

			r.Sync = v
			r.Present |= PresentSync
			off += n
		case iproto.IPROTO_REPLICA_ID:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: replica_id: %w", err)
			}

			r.ReplicaID = uint32(v) //nolint:gosec // G115: replica_id is a uint32 field per iproto_constants.h
			r.Present |= PresentReplicaID
			off += n
		case iproto.IPROTO_GROUP_ID:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: group_id: %w", err)
			}

			r.GroupID = uint32(v) //nolint:gosec // G115: group_id is a uint32 field per iproto_constants.h
			r.Present |= PresentGroupID
			off += n
		case iproto.IPROTO_LSN:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: lsn: %w", err)
			}

			r.LSN = int64(v) //nolint:gosec // G115: lsn is a msgpack uint that fits int64 in the xlog format
			r.Present |= PresentLSN
			off += n
		case iproto.IPROTO_TIMESTAMP:
			v, n, err := readMPDouble(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: timestamp: %w", err)
			}

			r.Timestamp = v
			r.Present |= PresentTimestamp
			off += n
		case iproto.IPROTO_SCHEMA_VERSION:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: schema_version: %w", err)
			}

			r.SchemaVersion = v
			r.Present |= PresentSchemaVersion
			off += n
		case iproto.IPROTO_TSN:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: tsn: %w", err)
			}
			// Differential encoding: tsn_diff = lsn - tsn.
			r.TSN = r.LSN - int64(v) //nolint:gosec // G115: tsn diff is a msgpack uint that fits int64 in the xlog format
			sawTSN = true
			r.Present |= PresentTSN
			off += n
		case iproto.IPROTO_FLAGS:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: flags: %w", err)
			}

			r.Flags = iproto.Flag(v) //nolint:gosec // G115: xrow flags is a small bitset bounded by iproto.Flag
			r.Present |= PresentFlags
			off += n
		case iproto.IPROTO_STREAM_ID:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: stream_id: %w", err)
			}

			r.StreamID = v
			r.Present |= PresentStreamID
			off += n
		default:
			// Unknown header key — skip the value, preserving compat.
			n, err := skipMP(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: skip key %d: %w", key, err)
			}

			off += n
		}
	}

	// Single-statement tx — when IPROTO_TSN absent, infer tsn=lsn,
	// is_commit=true. This is the only place where the decoder sets
	// IPROTO_FLAG_COMMIT on a row that did not have IPROTO_FLAGS set.
	// Neither inference touches Present: PresentTSN and PresentFlags record
	// the keys on the wire, not the values the decoder derives.
	if !sawTSN {
		r.TSN = r.LSN
		r.Flags |= iproto.IPROTO_FLAG_COMMIT
	}

	// Body. NOP rows have no body; everything else has one msgpack value
	// after the header.
	if r.Type == iproto.IPROTO_NOP {
		r.BodyRaw = nil

		return off, nil
	}

	if off >= len(b) {
		// No body bytes left. Treat as empty body (some synthesised types
		// may have no body even outside NOP — be liberal).
		r.BodyRaw = nil

		return off, nil
	}

	bodyLen, err := skipMP(b[off:])
	if err != nil {
		return 0, fmt.Errorf("format: DecodeXRow: body: %w", err)
	}
	// A sub-slice of the input, not a copy: the aliasing contract of the
	// package doc, which keeps the hot path zero-copy.
	r.BodyRaw = b[off : off+bodyLen]
	off += bodyLen

	return off, nil
}

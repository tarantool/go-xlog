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
// BodyRaw holds the body map bytes verbatim (decoded on demand). For
// NOP rows it is nil.
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
}

// IsCommit reports whether IPROTO_FLAG_COMMIT is set on r.
func (r *XRow) IsCommit() bool { return r.Flags&iproto.IPROTO_FLAG_COMMIT != 0 }

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
//   - IPROTO_REPLICA_ID / IPROTO_LSN / IPROTO_GROUP_ID / IPROTO_TIMESTAMP present iff nonzero.
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
// Caveat: this function mirrors Tarantool's `if (header->tsn != 0)` guard
// (src/box/xrow.c:402). A row with LSN == TSN == 0 will skip the TSN/COMMIT
// emission block entirely. For a single-statement tx that is harmless — the
// decoder also infers `is_commit = true` when IPROTO_TSN is absent. For a
// multi-row tx where some rows legitimately have LSN == 0, the encoded
// output would be ambiguous (Tarantool would treat each as an independent
// single-stmt tx). The writer package's assignTxIDs avoids this by setting
// TSN = rows[0].LSN before encoding; callers using EncodeXRow directly
// must ensure rows in a multi-row tx have non-zero LSN.
func EncodeXRow(buf []byte, r *XRow) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("format: EncodeXRow: %w", ErrNilRow)
	}
	// Decide which fields are present and at what size.
	// We use a small fixed slice rather than allocating a slice of pairs.
	var hdr [10]struct {
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
func DecodeXRow(b []byte) (*XRow, int, error) {
	r := &XRow{}

	n, err := DecodeXRowInto(b, r)
	if err != nil {
		return nil, 0, err
	}

	return r, n, nil
}

// DecodeXRowInto parses one xrow record at b[0:] into dst, fully overwriting
// every field (including zeroing BodyRaw / StreamID / Flags so a reused dst
// carries no state from a prior row), and returns the number of bytes
// consumed. It is the zero-allocation core of DecodeXRow: a hot read loop can
// reuse a single *XRow — or an arena slot — across rows with no per-row heap
// allocation. As with DecodeXRow, dst.BodyRaw aliases b; clone it before
// retaining past b's lifetime.
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
			off += n
		case iproto.IPROTO_REPLICA_ID:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: replica_id: %w", err)
			}

			r.ReplicaID = uint32(v) //nolint:gosec // G115: replica_id is a uint32 field per iproto_constants.h
			off += n
		case iproto.IPROTO_GROUP_ID:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: group_id: %w", err)
			}

			r.GroupID = uint32(v) //nolint:gosec // G115: group_id is a uint32 field per iproto_constants.h
			off += n
		case iproto.IPROTO_LSN:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: lsn: %w", err)
			}

			r.LSN = int64(v) //nolint:gosec // G115: lsn is a msgpack uint that fits int64 in the xlog format
			off += n
		case iproto.IPROTO_TIMESTAMP:
			v, n, err := readMPDouble(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: timestamp: %w", err)
			}

			r.Timestamp = v
			off += n
		case iproto.IPROTO_TSN:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: tsn: %w", err)
			}
			// Differential encoding: tsn_diff = lsn - tsn.
			r.TSN = r.LSN - int64(v) //nolint:gosec // G115: tsn diff is a msgpack uint that fits int64 in the xlog format
			sawTSN = true
			off += n
		case iproto.IPROTO_FLAGS:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: flags: %w", err)
			}

			r.Flags = iproto.Flag(v) //nolint:gosec // G115: xrow flags is a small bitset bounded by iproto.Flag
			off += n
		case iproto.IPROTO_STREAM_ID:
			v, n, err := readMPUint(b[off:])
			if err != nil {
				return 0, fmt.Errorf("format: DecodeXRow: stream_id: %w", err)
			}

			r.StreamID = v
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
	// Copy or alias? The format package returns a sub-slice of the input;
	// callers who keep it past the input's lifetime must clone. This keeps
	// us zero-copy on the hot path.
	r.BodyRaw = b[off : off+bodyLen]
	off += bodyLen

	return off, nil
}

package format

import (
	"errors"
	"fmt"

	"github.com/tarantool/go-iproto"
)

// VyLogType is the type of a vinyl log record (Tarantool's vy_log_record_type
// enum, src/box/vy_log.h).
type VyLogType uint32

// Vinyl log record types (src/box/vy_log.h).
const (
	VyLogCreateLSM        VyLogType = 0
	VyLogDropLSM          VyLogType = 1
	VyLogInsertRange      VyLogType = 2
	VyLogDeleteRange      VyLogType = 3
	VyLogPrepareRun       VyLogType = 4
	VyLogCreateRun        VyLogType = 5
	VyLogDropRun          VyLogType = 6
	VyLogForgetRun        VyLogType = 7
	VyLogInsertSlice      VyLogType = 8
	VyLogDeleteSlice      VyLogType = 9
	VyLogDumpLSM          VyLogType = 10
	VyLogSnapshot         VyLogType = 11
	VyLogTruncateLSM      VyLogType = 12
	VyLogModifyLSM        VyLogType = 13
	VyLogForgetLSM        VyLogType = 14
	VyLogPrepareLSM       VyLogType = 15
	VyLogRebootstrap      VyLogType = 16
	VyLogAbortRebootstrap VyLogType = 17
)

// vyLogTypeNames maps vinyl log record types to their short names.
var vyLogTypeNames = map[VyLogType]string{
	VyLogCreateLSM:        "CREATE_LSM",
	VyLogDropLSM:          "DROP_LSM",
	VyLogInsertRange:      "INSERT_RANGE",
	VyLogDeleteRange:      "DELETE_RANGE",
	VyLogPrepareRun:       "PREPARE_RUN",
	VyLogCreateRun:        "CREATE_RUN",
	VyLogDropRun:          "DROP_RUN",
	VyLogForgetRun:        "FORGET_RUN",
	VyLogInsertSlice:      "INSERT_SLICE",
	VyLogDeleteSlice:      "DELETE_SLICE",
	VyLogDumpLSM:          "DUMP_LSM",
	VyLogSnapshot:         "SNAPSHOT",
	VyLogTruncateLSM:      "TRUNCATE_LSM",
	VyLogModifyLSM:        "MODIFY_LSM",
	VyLogForgetLSM:        "FORGET_LSM",
	VyLogPrepareLSM:       "PREPARE_LSM",
	VyLogRebootstrap:      "REBOOTSTRAP",
	VyLogAbortRebootstrap: "ABORT_REBOOTSTRAP",
}

// String returns the short record type name (e.g. "CREATE_LSM"), or
// VyLogType(n) for an unrecognised type.
func (t VyLogType) String() string {
	if name, ok := vyLogTypeNames[t]; ok {
		return name
	}

	return fmt.Sprintf("VyLogType(%d)", uint32(t))
}

// ErrBadVyLogRecord is returned by DecodeVyLogBody for a VYLOG row body that
// does not hold a vinyl log record.
var ErrBadVyLogRecord = errors.New("bad vylog record")

// VyLogBody is the typed body of a VYLOG row: one vinyl log record. Tarantool's
// vy_log_record_encode (src/box/vy_log.c) stores a record as an IPROTO_INSERT
// of no space whose tuple is a pair of the record type and a map of its
// fields keyed by vy_log_key:
//
//	{IPROTO_TUPLE: [type, {VyKey: value, ...}]}
//
// A field equal to zero is not written: an absent key reads as zero (the
// first LSM tree, for instance, has id 0 and no VyKeyLSMID).
type VyLogBody struct {
	// Type is the record type.
	Type VyLogType
	// Keys holds every field of the record as raw msgpack, keyed by
	// vy_log_key. The caller decodes the fields it needs lazily.
	Keys map[VyKey][]byte
}

// DecodeVyLogBody parses a VYLOG row body. Body keys other than IPROTO_TUPLE
// are ignored, as Tarantool's vy_log_record_decode does. It is
// DecodeVyLogBodyInto into a new VyLogBody, with no options.
func DecodeVyLogBody(b []byte) (*VyLogBody, error) {
	body := &VyLogBody{}

	if err := DecodeVyLogBodyInto(b, body, nil); err != nil {
		return nil, err
	}

	return body, nil
}

// DecodeVyLogBodyInto parses a VYLOG row body into dst, the core of
// DecodeVyLogBody. It first resets dst to the zero VyLogBody, so a dst reused
// across rows carries no field of a prior row. The values of Keys alias b.
// A body key other than IPROTO_TUPLE goes to opts.OnUnknown, or is skipped
// when opts or its OnUnknown is nil. The Keys map of a prior row is never
// reused: every record allocates a new one. On error dst holds what was
// decoded before it.
func DecodeVyLogBodyInto(b []byte, dst *VyLogBody, opts *DecodeOptions) error {
	*dst = VyLogBody{}

	if len(b) == 0 {
		return fmt.Errorf("format: DecodeVyLogBody: %w", ErrEmptyBody)
	}

	entries, off, err := readMPMapLen(b)
	if err != nil {
		return fmt.Errorf("format: DecodeVyLogBody: %w", err)
	}

	var record []byte

	for i := range entries {
		key, n, err := readMPUint(b[off:])
		if err != nil {
			return fmt.Errorf("format: DecodeVyLogBody: key %d: %w", i, err)
		}

		off += n

		valLen, err := skipMP(b[off:])
		if err != nil {
			return fmt.Errorf("format: DecodeVyLogBody: skip value for key %d: %w", key, err)
		}

		if iproto.Key(key) == iproto.IPROTO_TUPLE { //nolint:gosec // G115: msgpack body key is a small protocol number
			record = b[off : off+valLen]
		} else {
			opts.unknown(key, b[off:off+valLen])
		}

		off += valLen
	}

	if record == nil {
		return fmt.Errorf("format: DecodeVyLogBody: %w: no IPROTO_TUPLE", ErrBadVyLogRecord)
	}

	return decodeVyLogRecord(record, dst)
}

// decodeVyLogRecord parses the [type, {key: value}] pair of a vinyl log record
// into body.
func decodeVyLogRecord(b []byte, body *VyLogBody) error {
	n, off, err := readMPArrayLen(b)
	if err != nil {
		return fmt.Errorf("format: DecodeVyLogBody: record: %w", err)
	}

	if n != 2 { //nolint:mnd // A record is a pair of its type and its fields.
		return fmt.Errorf("format: DecodeVyLogBody: %w: an array of %d, not 2", ErrBadVyLogRecord, n)
	}

	typ, k, err := readMPUint(b[off:])
	if err != nil {
		return fmt.Errorf("format: DecodeVyLogBody: type: %w", err)
	}

	off += k

	entries, k, err := readMPMapLen(b[off:])
	if err != nil {
		return fmt.Errorf("format: DecodeVyLogBody: fields: %w", err)
	}

	off += k
	body.Type = VyLogType(typ) //nolint:gosec // G115: vy_log record type is a small enum bounded by uint32
	body.Keys = make(map[VyKey][]byte, entries)

	for i := range entries {
		key, n, err := readMPUint(b[off:])
		if err != nil {
			return fmt.Errorf("format: DecodeVyLogBody: field %d: %w", i, err)
		}

		off += n

		valLen, err := skipMP(b[off:])
		if err != nil {
			return fmt.Errorf("format: DecodeVyLogBody: skip value for field %d: %w", key, err)
		}

		body.Keys[VyKey(key)] = b[off : off+valLen]
		off += valLen
	}

	return nil
}

package format

import (
	"errors"
	"fmt"

	"github.com/tarantool/go-iproto"
)

// DMLBody is the typed body of an INSERT/REPLACE/UPDATE/DELETE/UPSERT row
// (and of INSERT_ARROW). Keys come from src/box/iproto_constants.h and are
// written by Tarantool's xrow_encode_dml (src/box/xrow.c). Each variable-
// length field (Tuple, Key, Ops, OldTuple, NewTuple, Arrow) is held as raw
// msgpack that aliases the decoded buffer, so callers who only need the
// SpaceID pay no decode cost; clone a field before retaining it past the
// buffer's lifetime.
//
// A body key with no field (IPROTO_TUPLE_META, IPROTO_BEGIN_KEY, ...) is
// skipped, unless DecodeOptions.OnUnknown of DecodeDMLBodyInto receives it.
type DMLBody struct {
	// SpaceID is IPROTO_SPACE_ID, the space the request applies to.
	SpaceID uint32
	// IndexID is IPROTO_INDEX_ID, the index a DELETE or UPDATE looks its key
	// up in; 0 (the primary index) when the body carries none. Tarantool
	// rebinds a request on a secondary index to the primary key before it
	// writes the WAL, so a journal row normally has none.
	IndexID uint32
	// IndexBase is IPROTO_INDEX_BASE of an UPDATE or UPSERT: the number of
	// the first field in the field numbers of Ops (or of Tuple, which holds
	// the operations of an UPDATE). 0 when the body carries none; a request
	// from Lua carries 1.
	IndexBase uint32
	// Tuple is IPROTO_TUPLE, raw msgpack: the tuple of an INSERT, REPLACE or
	// UPSERT, the operations of an UPDATE.
	Tuple []byte
	// Key is IPROTO_KEY, raw msgpack: the key of an UPDATE or DELETE.
	Key []byte
	// Ops is IPROTO_OPS, raw msgpack: the operations of an UPSERT.
	Ops []byte
	// OldTuple is IPROTO_OLD_TUPLE, raw msgpack: the tuple before the change,
	// written for replica-applied and audited rows.
	OldTuple []byte
	// NewTuple is IPROTO_NEW_TUPLE, raw msgpack: the tuple after the change,
	// written for replica-applied and audited rows.
	NewTuple []byte
	// Arrow is IPROTO_ARROW of an INSERT_ARROW, raw msgpack: the whole
	// MP_EXT value, ext header included, holding the Arrow IPC stream.
	Arrow []byte
}

// ErrEmptyBody is returned by the body decoders when handed a zero-length
// body slice.
var ErrEmptyBody = errors.New("empty body")

// DecodeDMLBody parses a DML body map at b[0:]. It returns an error if b
// is not a msgpack map header. It is DecodeDMLBodyInto into a new DMLBody,
// with no options: a key with no field is skipped.
func DecodeDMLBody(b []byte) (*DMLBody, error) {
	body := &DMLBody{}

	if err := DecodeDMLBodyInto(b, body, nil); err != nil {
		return nil, err
	}

	return body, nil
}

// DecodeDMLBodyInto parses a DML body map at b[0:] into dst. It is the
// zero-allocation core of DecodeDMLBody: a hot loop can decode every body
// into one caller-owned DMLBody. It first resets dst to the zero DMLBody, so
// a reused dst carries no field of a prior row, then sets the fields of the
// keys b holds. The raw fields of dst (Tuple, Key, Ops, OldTuple, NewTuple,
// Arrow) alias b; clone one before retaining it past b's lifetime. A key with
// no field goes to opts.OnUnknown, or is skipped when opts or its OnUnknown
// is nil. On error dst holds what was decoded before it.
func DecodeDMLBodyInto(b []byte, dst *DMLBody, opts *DecodeOptions) error {
	*dst = DMLBody{}

	if len(b) == 0 {
		return fmt.Errorf("format: DecodeDMLBody: %w", ErrEmptyBody)
	}

	entries, off, err := readMPMapLen(b)
	if err != nil {
		return fmt.Errorf("format: DecodeDMLBody: %w", err)
	}

	body := dst

	for i := range entries {
		key, n, err := readMPUint(b[off:])
		if err != nil {
			return fmt.Errorf("format: DecodeDMLBody: key %d: %w", i, err)
		}

		off += n
		valStart := off

		valLen, err := skipMP(b[off:])
		if err != nil {
			return fmt.Errorf("format: DecodeDMLBody: skip value for key %d: %w", key, err)
		}

		val := b[valStart : valStart+valLen]
		off += valLen

		switch iproto.Key(key) { //nolint:gosec // G115: msgpack body key is a small protocol number
		case iproto.IPROTO_SPACE_ID:
			v, _, err := readMPUint(val)
			if err != nil {
				return fmt.Errorf("format: DecodeDMLBody: space_id: %w", err)
			}

			body.SpaceID = uint32(v) //nolint:gosec // G115: space_id is a uint32 field per iproto_constants.h
		case iproto.IPROTO_INDEX_ID:
			v, _, err := readMPUint(val)
			if err != nil {
				return fmt.Errorf("format: DecodeDMLBody: index_id: %w", err)
			}

			body.IndexID = uint32(v) //nolint:gosec // G115: index_id is a uint32 field per iproto_constants.h
		case iproto.IPROTO_INDEX_BASE:
			v, _, err := readMPUint(val)
			if err != nil {
				return fmt.Errorf("format: DecodeDMLBody: index_base: %w", err)
			}

			body.IndexBase = uint32(v) //nolint:gosec // G115: index_base is a uint32 field per iproto_constants.h
		case iproto.IPROTO_TUPLE:
			body.Tuple = val
		case iproto.IPROTO_KEY:
			body.Key = val
		case iproto.IPROTO_OPS:
			body.Ops = val
		case iproto.IPROTO_OLD_TUPLE:
			body.OldTuple = val
		case iproto.IPROTO_NEW_TUPLE:
			body.NewTuple = val
		case iproto.IPROTO_ARROW:
			body.Arrow = val
		default:
			opts.unknown(key, val)
		}
	}

	return nil
}

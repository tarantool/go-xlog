package format

import (
	"errors"
	"fmt"

	"github.com/tarantool/go-iproto"
)

// DMLBody is the typed body of an INSERT/REPLACE/UPDATE/DELETE/UPSERT row.
// Keys come from src/box/iproto_constants.h:98-138. We hold each variable-
// length field (Tuple/Key/Ops/Old/New) as raw msgpack bytes so callers
// who only need the SpaceID pay no decode cost.
//
// Extras captures any unknown body key, preserving it across rewrite paths.
type DMLBody struct {
	SpaceID  uint32
	Tuple    []byte // IPROTO_TUPLE — INSERT/REPLACE/UPSERT.
	Key      []byte // IPROTO_KEY — UPDATE/DELETE.
	Ops      []byte // IPROTO_OPS — UPDATE/UPSERT.
	OldTuple []byte // IPROTO_OLD_TUPLE — replica-applied, audit.
	NewTuple []byte // IPROTO_NEW_TUPLE — replica-applied, audit.
	Extras   map[uint64][]byte
}

// ErrEmptyBody is returned by the body decoders when handed a zero-length
// body slice.
var ErrEmptyBody = errors.New("empty body")

// DecodeDMLBody parses a DML body map at b[0:]. It returns an error if b
// is not a msgpack map header.
func DecodeDMLBody(b []byte) (*DMLBody, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("format: DecodeDMLBody: %w", ErrEmptyBody)
	}

	entries, off, err := readMPMapLen(b)
	if err != nil {
		return nil, fmt.Errorf("format: DecodeDMLBody: %w", err)
	}

	body := &DMLBody{}

	for i := range entries {
		key, n, err := readMPUint(b[off:])
		if err != nil {
			return nil, fmt.Errorf("format: DecodeDMLBody: key %d: %w", i, err)
		}

		off += n
		valStart := off

		valLen, err := skipMP(b[off:])
		if err != nil {
			return nil, fmt.Errorf("format: DecodeDMLBody: skip value for key %d: %w", key, err)
		}

		val := b[valStart : valStart+valLen]
		off += valLen

		switch iproto.Key(key) { //nolint:gosec // G115: msgpack body key is a small protocol number
		case iproto.IPROTO_SPACE_ID:
			v, _, err := readMPUint(val)
			if err != nil {
				return nil, fmt.Errorf("format: DecodeDMLBody: space_id: %w", err)
			}

			body.SpaceID = uint32(v) //nolint:gosec // G115: space_id is a uint32 field per iproto_constants.h
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
		default:
			if body.Extras == nil {
				body.Extras = map[uint64][]byte{}
			}

			body.Extras[key] = val
		}
	}

	return body, nil
}

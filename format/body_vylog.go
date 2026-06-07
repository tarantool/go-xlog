package format

import (
	"fmt"

	"github.com/tarantool/go-iproto"
)

// VyLogBody is the typed body of a VYLOG row. The body is structured as a
// `{IPROTO_REQUEST_TYPE: vy_log_record_type, vy_log_key_i: ..., ...}` map —
// vy_log piggybacks on the same IPROTO request-type slot to identify which
// of its 17 record types this row is, then the rest of the map uses keys
// from `vy_log_key` (src/box/vy_log.c:71-89).
//
// Keys holds every recognised vy_log_key field as raw msgpack bytes. The
// caller can decode specific fields lazily.
type VyLogBody struct {
	Type uint32 // vy_log record type.
	Keys map[VyKey][]byte
}

// DecodeVyLogBody parses a VYLOG row body. It treats the body as a generic
// uint-keyed msgpack map — Type comes from the IPROTO_REQUEST_TYPE (0x00) slot
// and every other entry goes into Keys verbatim.
func DecodeVyLogBody(b []byte) (*VyLogBody, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("format: DecodeVyLogBody: %w", ErrEmptyBody)
	}

	entries, off, err := readMPMapLen(b)
	if err != nil {
		return nil, fmt.Errorf("format: DecodeVyLogBody: %w", err)
	}

	body := &VyLogBody{Keys: map[VyKey][]byte{}}

	for i := range entries {
		key, n, err := readMPUint(b[off:])
		if err != nil {
			return nil, fmt.Errorf("format: DecodeVyLogBody: key %d: %w", i, err)
		}

		off += n
		valStart := off

		valLen, err := skipMP(b[off:])
		if err != nil {
			return nil, fmt.Errorf("format: DecodeVyLogBody: skip value for key %d: %w", key, err)
		}

		val := b[valStart : valStart+valLen]
		off += valLen

		if iproto.Key(key) == iproto.IPROTO_REQUEST_TYPE { //nolint:gosec // G115: msgpack body key is a small protocol number
			v, _, err := readMPUint(val)
			if err != nil {
				return nil, fmt.Errorf("format: DecodeVyLogBody: type: %w", err)
			}

			body.Type = uint32(v) //nolint:gosec // G115: vy_log record type is a small enum bounded by uint32

			continue
		}

		body.Keys[VyKey(key)] = val
	}

	return body, nil
}

package format

import (
	"fmt"

	"github.com/tarantool/go-iproto"
)

// SynchroBody is the typed body of IPROTO_RAFT_CONFIRM / IPROTO_RAFT_ROLLBACK rows.
// Tarantool's xrow_encode_synchro / xrow_decode_synchro encodes a small
// fixmap using the standard IPROTO keys (REPLICA_ID, LSN, plus Term — the
// last is encoded under a Raft-namespace key in some builds; we surface it
// as Extras when we can't recognise it).
//
// We keep this conservative: required fields ReplicaID and LSN; the rest
// goes into Extras so downstream code can probe specific keys without
// blocking on future additions.
type SynchroBody struct {
	ReplicaID uint32
	LSN       int64
	Term      uint64
	Extras    map[uint64][]byte
}

// DecodeSynchroBody parses a synchro body map at b[0:].
func DecodeSynchroBody(b []byte) (*SynchroBody, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("format: DecodeSynchroBody: %w", ErrEmptyBody)
	}

	entries, off, err := readMPMapLen(b)
	if err != nil {
		return nil, fmt.Errorf("format: DecodeSynchroBody: %w", err)
	}

	body := &SynchroBody{}

	for i := range entries {
		key, n, err := readMPUint(b[off:])
		if err != nil {
			return nil, fmt.Errorf("format: DecodeSynchroBody: key %d: %w", i, err)
		}

		off += n
		valStart := off

		valLen, err := skipMP(b[off:])
		if err != nil {
			return nil, fmt.Errorf("format: DecodeSynchroBody: skip value for key %d: %w", key, err)
		}

		val := b[valStart : valStart+valLen]
		off += valLen

		// Synchro bodies mix header keys (REPLICA_ID, LSN — iproto.Key) with a
		// raft key (TERM — iproto.RaftKey); both namespaces share the raw uint
		// keyspace on the wire, so switch over the raw uint64 with cast cases.
		switch key {
		case uint64(iproto.IPROTO_REPLICA_ID):
			v, _, err := readMPUint(val)
			if err != nil {
				return nil, fmt.Errorf("format: DecodeSynchroBody: replica_id: %w", err)
			}

			body.ReplicaID = uint32(v) //nolint:gosec // G115: replica_id is a uint32 field per iproto_constants.h
		case uint64(iproto.IPROTO_LSN):
			v, _, err := readMPUint(val)
			if err != nil {
				return nil, fmt.Errorf("format: DecodeSynchroBody: lsn: %w", err)
			}

			body.LSN = int64(v) //nolint:gosec // G115: lsn is a msgpack uint that fits int64 in the xlog format
		case uint64(iproto.IPROTO_RAFT_TERM): // 0 collides with REQUEST_TYPE but body keys live in their own namespace.
			// In practice xrow_encode_synchro writes the term under the
			// RAFT_KEY_TERM (0) code; we accept that mapping here.
			v, _, err := readMPUint(val)
			if err != nil {
				return nil, fmt.Errorf("format: DecodeSynchroBody: term: %w", err)
			}

			body.Term = v
		default:
			if body.Extras == nil {
				body.Extras = map[uint64][]byte{}
			}

			body.Extras[key] = val
		}
	}

	return body, nil
}

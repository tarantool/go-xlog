package format

import (
	"errors"
	"fmt"

	"github.com/tarantool/go-iproto"
)

// RaftBody is the typed body of a IPROTO_RAFT row. Keys come from
// src/box/iproto_constants.h:465-471. A body key with no field is skipped,
// unless DecodeOptions.OnUnknown of DecodeRaftBodyInto receives it.
type RaftBody struct {
	// Term is IPROTO_RAFT_TERM, the Raft term of the instance.
	Term uint64
	// Vote is IPROTO_RAFT_VOTE, the instance voted for in Term, 0 for none.
	Vote uint64
	// State is IPROTO_RAFT_STATE, the Raft state (follower, candidate,
	// leader).
	State uint32
	// VClock is IPROTO_RAFT_VCLOCK, the vclock of the instance, nil when the
	// body carries none.
	VClock VClock
	// LeaderID is IPROTO_RAFT_LEADER_ID, the leader the instance knows of.
	LeaderID uint32
	// IsLeaderSeen is IPROTO_RAFT_IS_LEADER_SEEN, whether the instance sees
	// the leader directly.
	IsLeaderSeen bool
}

// Sentinel errors for malformed RAFT bodies.
var (
	ErrIsLeaderSeenLen = errors.New("is_leader_seen bad length")
	ErrIsLeaderSeenTag = errors.New("is_leader_seen unexpected tag")
)

// DecodeRaftBody parses a RAFT body map. It is DecodeRaftBodyInto into a new
// RaftBody, with no options: a key with no field is skipped.
func DecodeRaftBody(b []byte) (*RaftBody, error) {
	body := &RaftBody{}

	if err := DecodeRaftBodyInto(b, body, nil); err != nil {
		return nil, err
	}

	return body, nil
}

// DecodeRaftBodyInto parses a RAFT body map into dst, the core of
// DecodeRaftBody. It first resets dst to the zero RaftBody, so a dst reused
// across rows carries no field of a prior row, then sets the fields of the
// keys b holds. A key with no field goes to opts.OnUnknown, or is skipped
// when opts or its OnUnknown is nil. The VClock map of a prior row is never
// reused: a body without a vclock decodes with no allocation, but a vclock
// allocates a new map. VClockArray is the allocation-free alternative to
// VClock; the body decoders do not fill one. On error dst holds what was
// decoded before it.
func DecodeRaftBodyInto(b []byte, dst *RaftBody, opts *DecodeOptions) error {
	*dst = RaftBody{}

	if len(b) == 0 {
		return fmt.Errorf("format: DecodeRaftBody: %w", ErrEmptyBody)
	}

	entries, off, err := readMPMapLen(b)
	if err != nil {
		return fmt.Errorf("format: DecodeRaftBody: %w", err)
	}

	body := dst

	for i := range entries {
		key, n, err := readMPUint(b[off:])
		if err != nil {
			return fmt.Errorf("format: DecodeRaftBody: key %d: %w", i, err)
		}

		off += n
		valStart := off

		valLen, err := skipMP(b[off:])
		if err != nil {
			return fmt.Errorf("format: DecodeRaftBody: skip value for key %d: %w", key, err)
		}

		val := b[valStart : valStart+valLen]
		off += valLen

		switch iproto.RaftKey(key) { //nolint:gosec // G115: raft body key is a small protocol number
		case iproto.IPROTO_RAFT_TERM:
			v, _, err := readMPUint(val)
			if err != nil {
				return fmt.Errorf("format: DecodeRaftBody: term: %w", err)
			}

			body.Term = v
		case iproto.IPROTO_RAFT_VOTE:
			v, _, err := readMPUint(val)
			if err != nil {
				return fmt.Errorf("format: DecodeRaftBody: vote: %w", err)
			}

			body.Vote = v
		case iproto.IPROTO_RAFT_STATE:
			v, _, err := readMPUint(val)
			if err != nil {
				return fmt.Errorf("format: DecodeRaftBody: state: %w", err)
			}

			body.State = uint32(v) //nolint:gosec // G115: raft state is a small enum bounded by uint32
		case iproto.IPROTO_RAFT_VCLOCK:
			// VClock value is itself a msgpack map of {replica_id: lsn}.
			vc, err := decodeVClockMap(val)
			if err != nil {
				return fmt.Errorf("format: DecodeRaftBody: vclock: %w", err)
			}

			body.VClock = vc
		case iproto.IPROTO_RAFT_LEADER_ID:
			v, _, err := readMPUint(val)
			if err != nil {
				return fmt.Errorf("format: DecodeRaftBody: leader_id: %w", err)
			}

			body.LeaderID = uint32(v) //nolint:gosec // G115: replica/leader id is a uint32 field per iproto_constants.h
		case iproto.IPROTO_RAFT_IS_LEADER_SEEN:
			// Boolean: encoded as msgpack true/false.
			if len(val) != 1 {
				return fmt.Errorf("format: DecodeRaftBody: %w %d", ErrIsLeaderSeenLen, len(val))
			}

			switch val[0] {
			case mpcTrue:
				body.IsLeaderSeen = true
			case mpcFalse:
				body.IsLeaderSeen = false
			default:
				return fmt.Errorf("format: DecodeRaftBody: %w 0x%02x", ErrIsLeaderSeenTag, val[0])
			}
		default:
			opts.unknown(key, val)
		}
	}

	return nil
}

// decodeVClockMap reads a msgpack map of {uint replica_id: uint lsn}.
// Shared by RaftBody (IPROTO_RAFT_VCLOCK).
func decodeVClockMap(b []byte) (VClock, error) {
	entries, off, err := readMPMapLen(b)
	if err != nil {
		return nil, err
	}

	v := VClock{}

	for range entries {
		id, n, err := readMPUint(b[off:])
		if err != nil {
			return nil, err
		}

		off += n

		lsn, n, err := readMPUint(b[off:])
		if err != nil {
			return nil, err
		}

		off += n
		v[uint32(id)] = int64(lsn) //nolint:gosec // G115: replica id is uint32 and lsn fits int64 in the vclock format
	}

	return v, nil
}

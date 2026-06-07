package format

import (
	"errors"
	"fmt"

	"github.com/tarantool/go-iproto"
)

// RaftBody is the typed body of a IPROTO_RAFT row. Keys come from
// src/box/iproto_constants.h:465-471.
type RaftBody struct {
	Term         uint64
	Vote         uint64
	State        uint32
	VClock       VClock
	LeaderID     uint32
	IsLeaderSeen bool
	Extras       map[uint64][]byte
}

// Sentinel errors for malformed RAFT bodies.
var (
	ErrIsLeaderSeenLen = errors.New("is_leader_seen bad length")
	ErrIsLeaderSeenTag = errors.New("is_leader_seen unexpected tag")
)

// DecodeRaftBody parses a RAFT body map.
func DecodeRaftBody(b []byte) (*RaftBody, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("format: DecodeRaftBody: %w", ErrEmptyBody)
	}

	entries, off, err := readMPMapLen(b)
	if err != nil {
		return nil, fmt.Errorf("format: DecodeRaftBody: %w", err)
	}

	body := &RaftBody{}

	for i := range entries {
		key, n, err := readMPUint(b[off:])
		if err != nil {
			return nil, fmt.Errorf("format: DecodeRaftBody: key %d: %w", i, err)
		}

		off += n
		valStart := off

		valLen, err := skipMP(b[off:])
		if err != nil {
			return nil, fmt.Errorf("format: DecodeRaftBody: skip value for key %d: %w", key, err)
		}

		val := b[valStart : valStart+valLen]
		off += valLen

		switch iproto.RaftKey(key) { //nolint:gosec // G115: raft body key is a small protocol number
		case iproto.IPROTO_RAFT_TERM:
			v, _, err := readMPUint(val)
			if err != nil {
				return nil, fmt.Errorf("format: DecodeRaftBody: term: %w", err)
			}

			body.Term = v
		case iproto.IPROTO_RAFT_VOTE:
			v, _, err := readMPUint(val)
			if err != nil {
				return nil, fmt.Errorf("format: DecodeRaftBody: vote: %w", err)
			}

			body.Vote = v
		case iproto.IPROTO_RAFT_STATE:
			v, _, err := readMPUint(val)
			if err != nil {
				return nil, fmt.Errorf("format: DecodeRaftBody: state: %w", err)
			}

			body.State = uint32(v) //nolint:gosec // G115: raft state is a small enum bounded by uint32
		case iproto.IPROTO_RAFT_VCLOCK:
			// VClock value is itself a msgpack map of {replica_id: lsn}.
			vc, err := decodeVClockMap(val)
			if err != nil {
				return nil, fmt.Errorf("format: DecodeRaftBody: vclock: %w", err)
			}

			body.VClock = vc
		case iproto.IPROTO_RAFT_LEADER_ID:
			v, _, err := readMPUint(val)
			if err != nil {
				return nil, fmt.Errorf("format: DecodeRaftBody: leader_id: %w", err)
			}

			body.LeaderID = uint32(v) //nolint:gosec // G115: replica/leader id is a uint32 field per iproto_constants.h
		case iproto.IPROTO_RAFT_IS_LEADER_SEEN:
			// Boolean: encoded as msgpack true/false.
			if len(val) != 1 {
				return nil, fmt.Errorf("format: DecodeRaftBody: %w %d", ErrIsLeaderSeenLen, len(val))
			}

			switch val[0] {
			case mpcTrue:
				body.IsLeaderSeen = true
			case mpcFalse:
				body.IsLeaderSeen = false
			default:
				return nil, fmt.Errorf("format: DecodeRaftBody: %w 0x%02x", ErrIsLeaderSeenTag, val[0])
			}
		default:
			if body.Extras == nil {
				body.Extras = map[uint64][]byte{}
			}

			body.Extras[key] = val
		}
	}

	return body, nil
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

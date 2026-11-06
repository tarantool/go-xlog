package format

import (
	"fmt"

	"github.com/tarantool/go-iproto"
)

// SynchroBody is the typed body of the synchronous replication rows:
// IPROTO_RAFT_CONFIRM, IPROTO_RAFT_ROLLBACK, IPROTO_RAFT_PROMOTE and
// IPROTO_RAFT_DEMOTE. Tarantool's xrow_encode_synchro writes a map of
// ordinary IPROTO keys (src/box/xrow.c):
//
//   - CONFIRM and ROLLBACK: IPROTO_REPLICA_ID and IPROTO_LSN.
//   - PROMOTE and DEMOTE: IPROTO_REPLICA_ID, IPROTO_LSN, IPROTO_TERM and, when
//     it is set, IPROTO_VCLOCK. The PROMOTE that checkpoints the limbo in a
//     snapshot carries the vclock.
//
// Any other key goes into Extras.
type SynchroBody struct {
	// ReplicaID is the owner of the synchronous queue (queue_owner_id): the
	// instance whose transactions the row confirms or rolls back, and for a
	// PROMOTE/DEMOTE the previous owner. It is not the issuer of the row,
	// which is the replica id of the row header.
	ReplicaID uint32
	// LSN is the LSN of the owner the row applies to: a CONFIRM confirms the
	// transactions up to it, a ROLLBACK rolls back those from it on, and a
	// PROMOTE/DEMOTE confirms up to it and rolls back the rest.
	LSN int64
	// Term is the Raft term a PROMOTE/DEMOTE is issued in (IPROTO_TERM).
	// CONFIRM and ROLLBACK carry none.
	Term uint64
	// ConfirmedVClock is the confirmed LSN of every previous queue owner
	// (IPROTO_VCLOCK) of a PROMOTE/DEMOTE, nil when the row carries none.
	ConfirmedVClock VClock
	// Extras holds every unrecognised key as raw msgpack.
	Extras map[uint64][]byte
}

// DecodeSynchroBody parses a synchro body map at b[0:]. It is
// DecodeSynchroBodyInto into a new SynchroBody.
func DecodeSynchroBody(b []byte) (*SynchroBody, error) {
	body := &SynchroBody{}

	if err := DecodeSynchroBodyInto(b, body); err != nil {
		return nil, err
	}

	return body, nil
}

// DecodeSynchroBodyInto parses a synchro body map at b[0:] into dst, the core
// of DecodeSynchroBody. It first resets dst to the zero SynchroBody, so a dst
// reused across rows carries no field of a prior row, then sets the fields of
// the keys b holds. The values of Extras alias b. A map of a prior row is
// never reused: a CONFIRM or ROLLBACK decodes with no allocation, but a
// PROMOTE or DEMOTE with a vclock allocates a new ConfirmedVClock map, and a
// key that has no field a new Extras map. VClockArray is the allocation-free
// alternative to VClock; the body decoders do not fill one. On error dst
// holds what was decoded before it.
func DecodeSynchroBodyInto(b []byte, dst *SynchroBody) error {
	*dst = SynchroBody{}

	if len(b) == 0 {
		return fmt.Errorf("format: DecodeSynchroBody: %w", ErrEmptyBody)
	}

	entries, off, err := readMPMapLen(b)
	if err != nil {
		return fmt.Errorf("format: DecodeSynchroBody: %w", err)
	}

	body := dst

	for i := range entries {
		key, n, err := readMPUint(b[off:])
		if err != nil {
			return fmt.Errorf("format: DecodeSynchroBody: key %d: %w", i, err)
		}

		off += n
		valStart := off

		valLen, err := skipMP(b[off:])
		if err != nil {
			return fmt.Errorf("format: DecodeSynchroBody: skip value for key %d: %w", key, err)
		}

		val := b[valStart : valStart+valLen]
		off += valLen

		switch iproto.Key(key) { //nolint:gosec // G115: msgpack body key is a small protocol number
		case iproto.IPROTO_REPLICA_ID:
			v, _, err := readMPUint(val)
			if err != nil {
				return fmt.Errorf("format: DecodeSynchroBody: replica_id: %w", err)
			}

			body.ReplicaID = uint32(v) //nolint:gosec // G115: replica_id is a uint32 field per iproto_constants.h
		case iproto.IPROTO_LSN:
			v, _, err := readMPUint(val)
			if err != nil {
				return fmt.Errorf("format: DecodeSynchroBody: lsn: %w", err)
			}

			body.LSN = int64(v) //nolint:gosec // G115: lsn is a msgpack uint that fits int64 in the xlog format
		case iproto.IPROTO_TERM:
			v, _, err := readMPUint(val)
			if err != nil {
				return fmt.Errorf("format: DecodeSynchroBody: term: %w", err)
			}

			body.Term = v
		case iproto.IPROTO_VCLOCK:
			vc, err := decodeVClockMap(val)
			if err != nil {
				return fmt.Errorf("format: DecodeSynchroBody: vclock: %w", err)
			}

			body.ConfirmedVClock = vc
		default:
			if body.Extras == nil {
				body.Extras = map[uint64][]byte{}
			}

			body.Extras[key] = val
		}
	}

	return nil
}

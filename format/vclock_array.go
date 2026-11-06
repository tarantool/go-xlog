package format

import (
	"errors"
	"fmt"
	"math/bits"
	"strconv"
)

// VClockMax is the number of components of a vclock — Tarantool's
// VCLOCK_MAX (src/lib/vclock/vclock.h:54). Replica ids of a VClockArray
// are 0..VClockMax-1.
const VClockMax = 32

// Bounds of the textual form of a VClockArray — Tarantool's
// VCLOCK_STR_LEN_MAX (src/lib/vclock/vclock.h:67): the braces plus, per
// replica, a 2-digit id, ": ", a 20-character int64 and ", ".
const (
	vclockStrPairLenMax = 2 + len(": ") + 20 + len(", ")
	vclockStrLenMax     = len("{") + VClockMax*vclockStrPairLenMax + len("}")
)

// decimalBase is the base of the ids and LSNs of the textual form.
const decimalBase = 10

// ErrVClockReplicaID reports a replica id that does not fit a VClockArray
// (id >= VClockMax). The VClock map accepts any id; a VClockArray, like
// Tarantool's struct vclock, does not.
var ErrVClockReplicaID = errors.New("vclock: replica id out of range")

// VClockArray is a dense vclock: Tarantool's struct vclock
// (src/lib/vclock/vclock.h:71-80) without the cached signature. It holds
// one LSN per replica id 0..VClockMax-1 and a bitmap of the ids that are
// set, so it copies by value and allocates nothing — the hot-path
// counterpart of VClock, which is a map. Convert at the boundary with
// VClock and VClockArrayFrom.
//
// The zero value is an empty vclock. The LSN of an id that is not set is
// always 0, so two VClockArrays are == exactly when they set the same ids
// to the same LSNs.
//
// The methods take a pointer receiver so that a call does not copy the
// 264-byte value; print a VClockArray through a pointer (fmt.Print(&v))
// for String to apply.
type VClockArray struct {
	lsn     [VClockMax]int64
	present uint32
}

// VClockArrayFrom converts v to a VClockArray. It returns
// ErrVClockReplicaID if v holds an id >= VClockMax. A nil or empty v
// gives the zero VClockArray.
func VClockArrayFrom(v VClock) (VClockArray, error) {
	var out VClockArray

	for id, lsn := range v {
		if err := out.Set(id, lsn); err != nil {
			return VClockArray{}, err
		}
	}

	return out, nil
}

// VClock converts v to a VClock map. An empty v gives an empty, non-nil
// VClock, as decoding an empty msgpack vclock map does.
func (v *VClockArray) VClock() VClock {
	out := make(VClock, v.Len())

	for m := v.present; m != 0; m &= m - 1 {
		id := bits.TrailingZeros32(m)
		out[uint32(id)] = v.lsn[id] //nolint:gosec // G115: id is a bit index of a uint32, 0..31
	}

	return out
}

// Get returns the LSN of replica id and whether id is set. An id >=
// VClockMax is never set.
func (v *VClockArray) Get(id uint32) (int64, bool) {
	if id >= VClockMax {
		return 0, false
	}

	return v.lsn[id], v.present&(1<<id) != 0
}

// Has reports whether replica id is set.
func (v *VClockArray) Has(id uint32) bool {
	return id < VClockMax && v.present&(1<<id) != 0
}

// Set sets the LSN of replica id, marking it set even when lsn is 0. It
// returns ErrVClockReplicaID, leaving v unchanged, if id >= VClockMax.
func (v *VClockArray) Set(id uint32, lsn int64) error {
	if id >= VClockMax {
		return fmt.Errorf("%w: %d >= %d", ErrVClockReplicaID, id, VClockMax)
	}

	v.lsn[id] = lsn
	v.present |= 1 << id

	return nil
}

// Len returns the number of replica ids that are set.
func (v *VClockArray) Len() int {
	return bits.OnesCount32(v.present)
}

// IsZero reports whether no replica id is set — the zero VClockArray.
func (v *VClockArray) IsZero() bool {
	return v.present == 0
}

// Reset clears every replica id, making v the zero VClockArray.
func (v *VClockArray) Reset() {
	v.lsn = [VClockMax]int64{}
	v.present = 0
}

// Signature returns the sum of the LSNs — Tarantool's vclock_sum
// (src/lib/vclock/vclock.h:236), the same value as VClock.Signature.
func (v *VClockArray) Signature() int64 {
	var sum int64

	for i := range v.lsn {
		sum += v.lsn[i]
	}

	return sum
}

// Compare orders v against o with the semantics of VClock.Compare: (-1,
// true) if v is before o, (0, true) if they are equal, (1, true) if v is
// after o, and (0, false) if they are incomparable. An id that is not set
// counts as LSN 0. This is Tarantool's vclock_compare
// (src/lib/vclock/vclock.h:310-344).
func (v *VClockArray) Compare(o *VClockArray) (int, bool) {
	var sawLT, sawGT bool

	for i := range v.lsn {
		a, b := v.lsn[i], o.lsn[i]
		sawLT = sawLT || a < b
		sawGT = sawGT || a > b
	}

	switch {
	case sawLT && sawGT:
		return 0, false
	case sawLT:
		return -1, true
	case sawGT:
		return 1, true
	default:
		return 0, true
	}
}

// String formats v as VClock.String does — Tarantool's vclock_to_string
// form `{id: lsn, id: lsn}` with ids ascending, `{}` when empty
// (src/lib/vclock/vclock.c:55-72).
func (v *VClockArray) String() string {
	var arr [vclockStrLenMax]byte

	buf := append(arr[:0], '{')

	for m := v.present; m != 0; m &= m - 1 {
		id := bits.TrailingZeros32(m)
		if m != v.present {
			buf = append(buf, ", "...)
		}

		buf = strconv.AppendInt(buf, int64(id), decimalBase)
		buf = append(buf, ": "...)
		buf = strconv.AppendInt(buf, v.lsn[id], decimalBase)
	}

	buf = append(buf, '}')

	return string(buf)
}

// DecodeVClockArrayInto decodes a msgpack map of {uint replica_id: uint
// lsn} — the encoding of IPROTO_RAFT_VCLOCK and IPROTO_VCLOCK — into dst
// and returns the number of bytes consumed. It resets dst first, so
// nothing of a previous value survives, and allocates nothing on success.
//
// A replica id >= VClockMax fails with ErrVClockReplicaID. A repeated id
// keeps its last LSN, as the VClock decoder does. On error dst is left
// reset.
func DecodeVClockArrayInto(b []byte, dst *VClockArray) (int, error) {
	dst.Reset()

	entries, off, err := readMPMapLen(b)
	if err != nil {
		return 0, err
	}

	for range entries {
		id, n, err := readMPUint(b[off:])
		if err != nil {
			dst.Reset()

			return 0, err
		}

		off += n

		lsn, n, err := readMPUint(b[off:])
		if err != nil {
			dst.Reset()

			return 0, err
		}

		off += n

		if id >= VClockMax {
			dst.Reset()

			return 0, fmt.Errorf("%w: %d >= %d", ErrVClockReplicaID, id, VClockMax)
		}

		dst.lsn[id] = int64(lsn) //nolint:gosec // G115: lsn fits int64 in the vclock format
		dst.present |= 1 << id
	}

	return off, nil
}

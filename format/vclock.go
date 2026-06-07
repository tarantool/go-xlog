package format

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// VClock is a Lamport vector keyed by replica id. Empty map means an
// "empty" vclock — Tarantool's `vclock_is_set` returns false in that case.
// The meta header omits the `PrevVClock:` line when empty, but always emits
// the `VClock:` line (as `VClock: {}` when empty), because Tarantool's
// recovery validates the filename signature against it.
//
// Tarantool stores vclocks as a fixed-size array internally but the
// textual ("{id: lsn, id: lsn}") and msgpack-map encodings are sparse, so
// a Go map is the natural representation here.
type VClock map[uint32]int64

// Sentinel errors for ParseVClock.
var (
	ErrVClockBraces = errors.New("vclock: missing surrounding braces")
	ErrVClockColon  = errors.New("vclock: missing ':' in entry")
	ErrVClockDupID  = errors.New("vclock: duplicate replica id")
)

// Signature returns the arithmetic sum of all per-replica LSNs — Tarantool's
// `vclock_sum` (src/lib/vclock/vclock.h:230). Used for the `<signature>.<ext>`
// filename naming.
func (v VClock) Signature() int64 {
	var sum int64
	for _, lsn := range v {
		sum += lsn
	}

	return sum
}

// Clone returns a deep copy. The receiver and the result share no map.
func (v VClock) Clone() VClock {
	if v == nil {
		return nil
	}

	out := make(VClock, len(v))
	maps.Copy(out, v)

	return out
}

// Compare implements the standard vector-clock partial order.
//
// Returns:
//
//	(-1, true) — v < o on every replica (and strictly less on at least one)
//	( 0, true) — v == o on every replica
//	( 1, true) — v > o on every replica (and strictly greater on at least one)
//	( 0, false) — incomparable (e.g. {1:5,2:3} vs {1:3,2:5})
//
// Missing replicas in either side are treated as LSN 0.
func (v VClock) Compare(o VClock) (int, bool) {
	// Walk the union of keys, tracking whether we have seen any "less"
	// and any "greater" axis.
	seen := make(map[uint32]struct{}, len(v)+len(o))
	for id := range v {
		seen[id] = struct{}{}
	}

	for id := range o {
		seen[id] = struct{}{}
	}

	var sawLT, sawGT bool

	for id := range seen {
		a := v[id]

		b := o[id]
		switch {
		case a < b:
			sawLT = true
		case a > b:
			sawGT = true
		}

		if sawLT && sawGT {
			// Mixed: strictly incomparable.
			return 0, false
		}
	}

	switch {
	case sawLT:
		return -1, true
	case sawGT:
		return 1, true
	default:
		return 0, true
	}
}

// String formats the vclock in Tarantool's `vclock_to_string` form:
// `{id: lsn, id: lsn}` with ids sorted ascending. An empty vclock renders
// as `{}` — which matches Tarantool's output for the empty case
// (e.g. `simple.xlog`'s meta has `VClock: {}` only because the file was
// pre-bootstrap; in normal operation `vclock_is_set` would gate the line
// out entirely, see meta.go).
//
// Src/lib/vclock/vclock.c:55-72.
func (v VClock) String() string {
	if len(v) == 0 {
		return "{}"
	}

	ids := make([]uint32, 0, len(v))
	for id := range v {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	var sb strings.Builder
	sb.WriteByte('{')

	for i, id := range ids {
		if i > 0 {
			sb.WriteString(", ")
		}

		sb.WriteString(strconv.FormatUint(uint64(id), 10))
		sb.WriteString(": ")
		sb.WriteString(strconv.FormatInt(v[id], 10))
	}

	sb.WriteByte('}')

	return sb.String()
}

// ParseVClock decodes a Tarantool vclock literal (`{}`, `{1: 42}`, or
// `{0: 0, 1: 42, 2: 7}`). Whitespace around braces, commas, and colons is
// tolerated. Returns an empty (non-nil) VClock for `{}`.
func ParseVClock(s string) (VClock, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("%w in %q", ErrVClockBraces, s)
	}

	body := strings.TrimSpace(s[1 : len(s)-1])

	out := VClock{}
	if body == "" {
		return out, nil
	}

	parts := strings.SplitSeq(body, ",")
	for p := range parts {
		p = strings.TrimSpace(p)

		before, after, ok := strings.Cut(p, ":")
		if !ok {
			return nil, fmt.Errorf("%w %q", ErrVClockColon, p)
		}

		idTxt := strings.TrimSpace(before)
		lsnTxt := strings.TrimSpace(after)

		id, err := strconv.ParseUint(idTxt, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("vclock: parse replica id %q: %w", idTxt, err)
		}

		lsn, err := strconv.ParseInt(lsnTxt, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("vclock: parse lsn %q: %w", lsnTxt, err)
		}

		if _, dup := out[uint32(id)]; dup {
			return nil, fmt.Errorf("%w %d in %q", ErrVClockDupID, id, s)
		}

		out[uint32(id)] = lsn
	}

	return out, nil
}

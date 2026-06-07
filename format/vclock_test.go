package format_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-xlog/format"
)

func TestVClock_Signature(t *testing.T) {
	t.Parallel()

	v := format.VClock{1: 42, 2: 7, 3: 0}
	require.Equalf(t, int64(49), v.Signature(), "Signature: got %d want 49", v.Signature())
	require.Equalf(t, int64(0), format.VClock(nil).Signature(), "nil Signature: got %d want 0", format.VClock(nil).Signature())
}

func TestVClock_StringRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []format.VClock{
		{},
		{0: 0},
		{1: 42},
		{0: 0, 1: 42, 2: 7},
	}
	for _, want := range cases {
		s := want.String()
		got, err := format.ParseVClock(s)
		require.NoErrorf(t, err, "ParseVClock(%q)", s)
		require.Truef(t, equalVClock(got, want), "round-trip %q: got %v want %v", s, got, want)
	}
}

func TestVClock_StringSortedByID(t *testing.T) {
	t.Parallel()

	v := format.VClock{3: 30, 1: 10, 2: 20}
	got := v.String()
	want := "{1: 10, 2: 20, 3: 30}"
	require.Equalf(t, want, got, "String: got %q want %q", got, want)
}

func TestVClock_Compare(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b   format.VClock
		order  int
		canCmp bool
	}{
		{format.VClock{1: 10}, format.VClock{1: 10}, 0, true},
		{format.VClock{1: 5}, format.VClock{1: 10}, -1, true},
		{format.VClock{1: 10}, format.VClock{1: 5}, 1, true},
		{format.VClock{1: 5, 2: 3}, format.VClock{1: 3, 2: 5}, 0, false}, // Incomparable.
		{format.VClock{1: 5, 2: 3}, format.VClock{1: 5}, 1, true},        // {1:5,2:3} vs {1:5,2:0}.
		{}, // Sentinel; we'll handle empty next.
	}
	for i, c := range cases[:len(cases)-1] {
		order, ok := c.a.Compare(c.b)
		require.Falsef(t, ok != c.canCmp || (ok && order != c.order), "case %d (%v vs %v): got (%d,%v) want (%d,%v)", i, c.a, c.b, order, ok, c.order, c.canCmp)
	}
	// Empty vs empty is equal.
	o, ok := (format.VClock{}).Compare(format.VClock{})
	require.Truef(t, ok && o == 0, "empty-empty: got (%d,%v), want (0,true)", o, ok)
}

func TestVClock_ParseErrors(t *testing.T) {
	t.Parallel()

	bad := []string{"", "{", "1: 2", "{1}", "{1: x}", "{a: 1}", "{1: 1, 1: 2}"}
	for _, s := range bad {
		_, err := format.ParseVClock(s)
		assert.Errorf(t, err, "ParseVClock(%q) expected error, got nil", s)
	}
}

func equalVClock(a, b format.VClock) bool {
	if len(a) != len(b) {
		return false
	}

	for k, va := range a {
		if vb, ok := b[k]; !ok || va != vb {
			return false
		}
	}

	return true
}

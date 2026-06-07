package format_test

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-xlog/format"
)

// TestMetaClone_RoundTrip — Clone must produce a Meta that round-trips
// through EncodeMeta to bytes identical to the original.
func TestMetaClone_RoundTrip(t *testing.T) {
	t.Parallel()

	orig := &format.Meta{
		Filetype:     format.FiletypeXLOG,
		FormatVer:    format.FormatVersion,
		Version:      "tarantool/3.x",
		InstanceUUID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		VClock:       format.VClock{1: 42, 2: 7},
		PrevVClock:   format.VClock{1: 41, 2: 6},
		Extras:       map[string]string{"Foo": "bar", "Baz": "qux"},
	}

	clone := orig.Clone()

	var origBuf, cloneBuf bytes.Buffer
	require.NoError(t, format.EncodeMeta(&origBuf, orig), "EncodeMeta(orig)")
	require.NoError(t, format.EncodeMeta(&cloneBuf, clone), "EncodeMeta(clone)")
	assert.Equalf(t, origBuf.Bytes(), cloneBuf.Bytes(), "clone round-trip mismatch:\norig=%q\nclone=%q",
		origBuf.String(), cloneBuf.String())
}

// TestMetaClone_Independent — mutating fields of the clone must not affect
// the original (and vice-versa). This is the headline property tools.RewriteMeta
// relies on when it hands the clone to a user transform fn.
func TestMetaClone_Independent(t *testing.T) {
	t.Parallel()

	orig := &format.Meta{
		Filetype:     format.FiletypeXLOG,
		FormatVer:    format.FormatVersion,
		Version:      "v1",
		InstanceUUID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		VClock:       format.VClock{1: 10},
		PrevVClock:   format.VClock{1: 9},
		Extras:       map[string]string{"K": "v"},
	}
	clone := orig.Clone()

	// Mutate every mutable field on the clone.
	clone.Version = "v2"
	clone.InstanceUUID = uuid.MustParse("99999999-9999-9999-9999-999999999999")
	clone.VClock[1] = 999
	clone.VClock[2] = 5
	clone.PrevVClock[1] = 888
	clone.Extras["K"] = "v2"
	clone.Extras["New"] = "x"

	assert.Equalf(t, "v1", orig.Version, "orig.Version mutated: %q", orig.Version)
	assert.Equalf(t, "11111111-2222-3333-4444-555555555555", orig.InstanceUUID.String(), "orig.InstanceUUID mutated: %q", orig.InstanceUUID.String())
	assert.Equalf(t, int64(10), orig.VClock[1], "orig.VClock[1] mutated: got %d, want 10", orig.VClock[1])
	_, ok := orig.VClock[2]
	assert.Falsef(t, ok, "orig.VClock[2] should not exist; got %d", orig.VClock[2])
	assert.Equalf(t, int64(9), orig.PrevVClock[1], "orig.PrevVClock[1] mutated: got %d, want 9", orig.PrevVClock[1])
	assert.Equalf(t, "v", orig.Extras["K"], "orig.Extras[K] mutated: got %q, want v", orig.Extras["K"])
	_, ok = orig.Extras["New"]
	assert.False(t, ok, "orig.Extras[New] should not exist")
}

// TestMetaClone_Nil — Clone on a nil receiver returns nil.
func TestMetaClone_Nil(t *testing.T) {
	t.Parallel()

	var m *format.Meta
	assert.Nil(t, m.Clone(), "nil.Clone() should be nil")
}

// TestMetaClone_NilMaps — Clone on a Meta with nil maps does not panic and
// leaves clone maps nil (avoids creating empty maps that would alter
// EncodeMeta output when nil-vs-empty matters).
func TestMetaClone_NilMaps(t *testing.T) {
	t.Parallel()

	orig := &format.Meta{
		Filetype:  format.FiletypeXLOG,
		FormatVer: format.FormatVersion,
		Version:   "v1",
	}
	clone := orig.Clone()
	assert.Nilf(t, clone.VClock, "VClock should be nil, got %v", clone.VClock)
	assert.Nilf(t, clone.PrevVClock, "PrevVClock should be nil, got %v", clone.PrevVClock)
	assert.Nilf(t, clone.Extras, "Extras should be nil, got %v", clone.Extras)
}

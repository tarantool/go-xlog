package format_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-xlog/format"
)

func TestFiletypeExt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		ft   format.Filetype
		want string
	}{
		{format.FiletypeXLOG, ".xlog"},
		{format.FiletypeSNAP, ".snap"},
		{format.FiletypeVYLOG, ".vylog"},
		{format.FiletypeRUN, ".run"},
		{format.FiletypeINDEX, ".index"},
	}
	for _, c := range cases {
		got, err := c.ft.Ext()
		require.NoErrorf(t, err, "%s: unexpected error", c.ft)
		assert.Equalf(t, c.want, got, "%s: ext mismatch", c.ft)
	}
}

func TestFiletypeExtUnknown(t *testing.T) {
	t.Parallel()

	_, err := format.Filetype("BOGUS").Ext()
	require.Error(t, err, "expected error for unknown filetype")
	assert.ErrorIs(t, err, format.ErrUnknownFiletype)
}

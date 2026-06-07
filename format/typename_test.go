package format_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tarantool/go-iproto"

	"github.com/tarantool/go-xlog/format"
)

func TestTypeName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		typ  iproto.Type
		want string
	}{
		{iproto.IPROTO_INSERT, "INSERT"},
		{iproto.IPROTO_REPLACE, "REPLACE"},
		{iproto.IPROTO_UPDATE, "UPDATE"},
		{iproto.IPROTO_DELETE, "DELETE"},
		{iproto.IPROTO_UPSERT, "UPSERT"},
		{iproto.IPROTO_NOP, "NOP"},
		{iproto.IPROTO_RAFT, "RAFT"},
		{iproto.IPROTO_RAFT_PROMOTE, "RAFT_PROMOTE"},
		{iproto.IPROTO_RAFT_DEMOTE, "RAFT_DEMOTE"},
		{iproto.IPROTO_RAFT_CONFIRM, "RAFT_CONFIRM"},
		{iproto.IPROTO_RAFT_ROLLBACK, "RAFT_ROLLBACK"},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, format.TypeName(c.typ), "type %d", c.typ)
	}

	// Unknown types fall back to iproto.Type's own Stringer form.
	assert.Equal(t, iproto.Type(9999).String(), format.TypeName(9999))
}

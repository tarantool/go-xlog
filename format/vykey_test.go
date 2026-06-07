package format_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tarantool/go-xlog/format"
)

func TestVyKeyString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "LSM_ID", format.VyKeyLSMID.String())
	assert.Equal(t, "SPACE_ID", format.VyKeySpaceID.String())
	assert.Equal(t, "DUMP_COUNT", format.VyKeyDumpCount.String())
	assert.Equal(t, "VyKey(99)", format.VyKey(99).String())
}

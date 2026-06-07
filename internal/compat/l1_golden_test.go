package compat //nolint:testpackage // shares internal test helper renderFile with white-box compat tests (render_test.go)

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGolden_AllFixtures renders every corpus fixture and diffs against its
// committed <fixture>.golden. With -update it (re)writes the goldens. This is
// the L1 layer: every decoded field of every row, regression-checked.
func TestGolden_AllFixtures(t *testing.T) {
	t.Parallel()

	fixtures := corpusFixtures(t)
	require.NotEmpty(t, fixtures, "no corpus fixtures discovered")

	for _, fixture := range fixtures {
		name := strings.TrimPrefix(fixture, filepath.Join("..", "..", "testdata", "historical")+string(filepath.Separator))
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := renderFile(fixture)
			require.NoError(t, err, "renderFile")

			golden := fixture + ".golden"
			if *updateGolden {
				err := os.WriteFile(golden, []byte(got), 0o644)
				require.NoError(t, err, "write golden")

				return
			}

			want, err := os.ReadFile(golden)
			require.NoError(t, err, "read golden (run with -update to create)")
			assert.Equalf(t, string(want), got, "render mismatch for %s\n%s", name, firstDiff(string(want), got))
		})
	}
}

// firstDiff returns a short description of the first differing line between
// want and got, to keep failure output readable for large snap dumps.
func firstDiff(want, got string) string {
	wl := strings.Split(want, "\n")

	gl := strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}

		if i < len(gl) {
			g = gl[i]
		}

		if w != g {
			return "first diff at line " + itoa(i+1) + ":\n  want: " + w + "\n  got:  " + g
		}
	}

	return "(no line diff; trailing-newline or length mismatch)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var b [20]byte

	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}

	return string(b[i:])
}

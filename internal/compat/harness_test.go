package compat //nolint:testpackage // shares internal test helpers corpusRoot/corpusFixtures with white-box compat tests (render_test.go, l3_behavioral_test.go)

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// updateGolden, when set via `-update`, rewrites the committed *.golden files
// from the current renderer output instead of comparing against them.
var updateGolden = flag.Bool("update", false, "rewrite golden files from current renderer output")

// corpusRoot is the historical corpus directory, relative to this package.
func corpusRoot(tb testing.TB) string {
	tb.Helper()

	root := filepath.Join("..", "..", "testdata", "historical")
	_, err := os.Stat(root)
	require.NoErrorf(tb, err, "corpus root %s", root)

	return root
}

// corpusFixtures returns every storage artefact in the corpus (all five file
// types, recursing into the vinyl <space>/<index>/ subtree), sorted.
func corpusFixtures(tb testing.TB) []string {
	tb.Helper()

	var out []string

	err := filepath.Walk(corpusRoot(tb), func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if fi.IsDir() {
			return nil
		}

		switch filepath.Ext(p) {
		case ".xlog", ".snap", ".vylog", ".run", ".index":
			out = append(out, p)
		}

		return nil
	})
	require.NoError(tb, err, "walk corpus")
	sort.Strings(out)

	return out
}

// corpusVersions returns the version subdirectory names (e.g. "2.11", "3.8"),
// sorted.
func corpusVersions(tb testing.TB) []string {
	tb.Helper()
	entries, err := os.ReadDir(corpusRoot(tb))
	require.NoError(tb, err, "read corpus root")

	var out []string

	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}

	sort.Strings(out)

	return out
}

package compat //nolint:testpackage // shares internal test helper typeName with white-box compat tests (render_test.go)

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// manifest is the committed per-version expectation: the load-bearing facts a
// regeneration must preserve. JSON (not YAML) to avoid a new dependency.
type manifest struct {
	FormatVersion string   `json:"format_version"`
	MustRowTypes  []string `json:"must_row_types"`  // Present in some XLOG/SNAP row.
	MustFileTypes []string `json:"must_file_types"` // Present as a file in the dir.
}

// TestManifest_Highlights asserts, per version, that the corpus still carries
// the format version and the feature rows/files the manifest demands. Guards
// against a regeneration silently dropping RAFT/SYNCHRO/vinyl coverage.
func TestManifest_Highlights(t *testing.T) {
	t.Parallel()

	for _, ver := range corpusVersions(t) {
		t.Run(ver, func(t *testing.T) {
			t.Parallel()

			dir := filepath.Join(corpusRoot(t), ver)

			var m manifest

			raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
			require.NoError(t, err, "read manifest")
			require.NoError(t, json.Unmarshal(raw, &m), "parse manifest")

			rowTypes := map[string]bool{}
			fileTypes := map[string]bool{}

			for _, f := range versionFixtures(t, dir) {
				r, err := reader.Open(f)
				require.NoErrorf(t, err, "open %s", f)
				assert.Equalf(t, m.FormatVersion, r.Meta().FormatVer, "%s: format mismatch", filepath.Base(f))
				fileTypes[string(r.Meta().Filetype)] = true

				ft := r.Meta().Filetype
				if ft == format.FiletypeXLOG || ft == format.FiletypeSNAP {
					for row, err := range r.Rows() {
						require.NoErrorf(t, err, "rows %s", f)

						rowTypes[typeName(row.Type)] = true
					}
				}

				_ = r.Close()
			}

			for _, want := range m.MustRowTypes {
				assert.Truef(t, rowTypes[want], "row type %q absent (have %v)", want, sortedKeys(rowTypes))
			}

			for _, want := range m.MustFileTypes {
				assert.Truef(t, fileTypes[want], "file type %q absent (have %v)", want, sortedKeys(fileTypes))
			}
		})
	}
}

// versionFixtures returns the corpus artefacts under a single version dir.
func versionFixtures(tb testing.TB, dir string) []string {
	tb.Helper()

	var out []string

	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
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
	require.NoErrorf(tb, err, "walk %s", dir)
	sort.Strings(out)

	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// Package testutil provides test-only helpers shared across packages.
// It is a function-only library with no global state.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// Load reads testdata/<name> by walking up from the current package directory
// looking for a "testdata" sibling. It fails the test if the file (or the
// testdata directory) cannot be located.
//
// The walk-up strategy lets test files in any subpackage share a single
// repo-root testdata/ tree without duplicating fixture files.
func Load(t *testing.T, name string) []byte {
	t.Helper()
	path := Path(t, name)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("testutil.Load: read %s: %v", path, err)
	}

	return data
}

// Path returns the absolute path to testdata/<name> using the same
// walk-up strategy as Load. It fails the test if the fixture cannot be
// located. Used by callers that need a file path (e.g. reader.Open) rather
// than the bytes.
func Path(t *testing.T, name string) string {
	t.Helper()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("testutil.Path: os.Getwd: %v", err)
	}

	dir := cwd
	for {
		candidate := filepath.Join(dir, "testdata", name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root without finding the fixture.
			t.Fatalf("testutil.Path: fixture %q not found walking up from %q", name, cwd)
		}

		dir = parent
	}
}

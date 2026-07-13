package testutil_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tarantool/go-xlog/internal/testutil"
)

// knownFixture is a fixture that lives in the repo-root testdata/ tree and is
// already used by many other test packages.
const knownFixture = "simple.xlog"

// TestPath_ReturnsAbsolutePath verifies that Path returns an absolute path
// whose suffix is testdata/<name>.
func TestPath_ReturnsAbsolutePath(t *testing.T) {
	t.Parallel()

	got := testutil.Path(t, knownFixture)

	if !filepath.IsAbs(got) {
		t.Errorf("Path(%q) = %q; want an absolute path", knownFixture, got)
	}

	want := filepath.Join("testdata", knownFixture)
	if !strings.HasSuffix(got, want) {
		t.Errorf("Path(%q) = %q; want suffix %q", knownFixture, got, want)
	}
}

// TestPath_MultipleFixtures runs Path for every known fixture to confirm the
// walk-up logic works consistently.
func TestPath_MultipleFixtures(t *testing.T) {
	t.Parallel()

	fixtures := []string{
		"simple.xlog",
		"populated.snap",
		"empty.snap",
		"multistmt.xlog",
		"vylog_sample.vylog",
	}

	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := testutil.Path(t, name)
			if !filepath.IsAbs(got) {
				t.Errorf("Path(%q) = %q; want absolute path", name, got)
			}

			want := filepath.Join("testdata", name)
			if !strings.HasSuffix(got, want) {
				t.Errorf("Path(%q) = %q; want suffix %q", name, got, want)
			}
		})
	}
}

// TestLoad_ReturnsNonEmptyBytes checks that Load returns non-empty content for
// a known fixture.
func TestLoad_ReturnsNonEmptyBytes(t *testing.T) {
	t.Parallel()

	data := testutil.Load(t, knownFixture)
	if len(data) == 0 {
		t.Errorf("Load(%q) returned 0 bytes; want non-empty content", knownFixture)
	}
}

// TestLoad_ConsistentWithPath verifies that the bytes returned by Load match
// what you get by combining Path + os.ReadFile.
func TestLoad_ConsistentWithPath(t *testing.T) {
	t.Parallel()

	path := testutil.Path(t, knownFixture)
	dataViaLoad := testutil.Load(t, knownFixture)

	dataViaDirect, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q): %v", path, err)
	}

	if string(dataViaLoad) != string(dataViaDirect) {
		t.Errorf("Load and Path+os.ReadFile returned different content for %q", knownFixture)
	}
}

// runInIsolatedTests runs fn inside testing.RunTests, which creates its own
// independent testing context. Failures inside fn do NOT propagate to the
// outer *testing.T. Returns true if the inner tests all passed, false if any
// called t.Fatal / t.Error.
//
// This is used to exercise error branches (t.Fatalf calls) in tested code
// without tainting the outer test result.
func runInIsolatedTests(name string, fn func(t *testing.T)) bool {
	return testing.RunTests(
		func(_, _ string) (bool, error) { return true, nil },
		[]testing.InternalTest{{Name: name, F: fn}},
	)
}

// TestPath_NotFound verifies that Path calls t.Fatal when the fixture is absent.
// We use testing.RunTests to isolate the failure so the outer test stays green.
func TestPath_NotFound(t *testing.T) {
	t.Parallel()

	passed := runInIsolatedTests("path_not_found", func(t *testing.T) {
		t.Helper()
		testutil.Path(t, "definitely-does-not-exist-fixture-xyz.snap")
	})

	// We expect the inner test to FAIL (passed == false) because Fatalf was called.
	if passed {
		t.Error("Path with a missing fixture should have called t.Fatalf; the inner test should have failed but it passed")
	}
}

// TestLoad_NotFound verifies that Load calls t.Fatal when the fixture is absent.
func TestLoad_NotFound(t *testing.T) {
	t.Parallel()

	passed := runInIsolatedTests("load_not_found", func(t *testing.T) {
		t.Helper()
		testutil.Load(t, "definitely-does-not-exist-fixture-xyz.snap")
	})

	if passed {
		t.Error("Load with a missing fixture should have called t.Fatalf; the inner test should have failed but it passed")
	}
}

// TestLoad_ReadError exercises the branch in Load where Path succeeds but
// os.ReadFile fails because the file has been made unreadable.
//
// Not parallel: t.Chdir temporarily changes the process working directory,
// which is incompatible with t.Parallel().
//
//nolint:paralleltest // uses t.Chdir, cannot run in parallel
func TestLoad_ReadError(t *testing.T) {
	tmp := t.TempDir()

	tdDir := filepath.Join(tmp, "testdata")
	if err := os.MkdirAll(tdDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	fixtureName := "unreadable_for_coverage_test.bin"

	fixturePath := filepath.Join(tdDir, fixtureName)
	if err := os.WriteFile(fixturePath, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Chdir(tmp)

	t.Cleanup(func() {
		_ = os.Chmod(fixturePath, 0o644)
	})

	if err := os.Chmod(fixturePath, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	passed := runInIsolatedTests("load_read_error", func(t *testing.T) {
		t.Helper()
		testutil.Load(t, fixtureName)
	})

	if passed {
		t.Error("Load on an unreadable file should have called t.Fatalf; the inner test should have failed but it passed")
	}
}

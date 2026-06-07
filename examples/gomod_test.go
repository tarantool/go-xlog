// Package examples holds only a guard test: it has no importable code, just the
// cat/play example commands under subdirectories. The test mirrors the CI
// "examples — go mod tidy (no changes)" step so a stale or mis-sorted go.mod is
// caught locally instead of turning the first push red.
package examples

import (
	"os/exec"
	"testing"
)

// TestGoModTidy asserts that go.mod/go.sum are already in the state `go mod
// tidy` would produce — including the alphabetical sort order of require
// blocks. It runs the same `go mod tidy -diff` the static CI job runs; a
// non-empty diff means the module is not tidy.
func TestGoModTidy(t *testing.T) {
	cmd := exec.Command("go", "mod", "tidy", "-diff")
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		t.Fatalf("go.mod/go.sum are not tidy; run `go mod tidy` in examples/.\n"+
			"`go mod tidy -diff` output:\n%s", out)
	}
	if err != nil {
		t.Fatalf("go mod tidy -diff failed: %v", err)
	}
}

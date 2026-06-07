package integration_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-xlog/pipe"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/tools"
	"github.com/tarantool/go-xlog/writer"
)

// These recovery tests drive a real Tarantool binary. Unlike the other
// integration tests they carry no build tag: they run as part of the normal
// suite and skip gracefully when no Tarantool is available (see needTarantool),
// so CI can exercise them once a binary is installed.

const scriptPerm = 0o644

// dumpStateLua defines dump_state(), a canonical JSON serialization of the two
// user spaces (sorted for determinism). Both the bootstrap and recovery scripts
// include it so their output is directly comparable. The Lua `end`s carry
// trailing comments so the embedded source does not trip the dupword linter.
const dumpStateLua = `
local json = require('json')
local function dump_state()
    local out = {}
    for _, id in ipairs({512, 513}) do
        local space = box.space[id]
        if space ~= nil then
            local rows = {}
            for _, tup in space:pairs() do
                table.insert(rows, tup:totable())
            end -- for
            table.sort(rows, function(a, b) return json.encode(a) < json.encode(b) end)
            out[tostring(id)] = rows
        end -- if
    end -- for
    return json.encode(out)
end
`

// bootstrapScript boots a fresh instance, snapshots part of the data (so the
// snapshot file matters), then performs post-snapshot DML covering every row
// type (insert/replace/update/delete/upsert), a >2 KiB tuple (zstd block), and
// a multi-statement transaction. It prints the final state as one JSON line.
const bootstrapScript = dumpStateLua + `
box.cfg{ work_dir = '.', log = 'tarantool.log', listen = box.NULL }

local s = box.schema.space.create('test', { id = 512 })
s:create_index('pk', { parts = {1, 'unsigned'} })
local kv = box.schema.space.create('kv', { id = 513 })
kv:create_index('pk', { parts = {1, 'string'} })

-- Captured by the snapshot below -> recovered from a go-xlog-written .snap.
s:insert{1, 'alpha', 100}
s:insert{2, 'beta', 200}
box.snapshot()

-- Post-snapshot ops -> recovered from a go-xlog-written .xlog.
s:insert{3, 'gamma', 300}
s:replace{2, 'beta-2', 222}
s:update({1}, {{'+', 3, 5}})
s:delete{3}
kv:upsert({'k1', 1}, {{'+', 2, 1}})
kv:upsert({'k1', 1}, {{'+', 2, 1}})
kv:insert{'k2', 'v2'}
s:insert{9, string.rep('z', 4096)}
box.begin()
s:insert{20, 'm1'}
s:insert{21, 'm2'}
box.commit()

print(dump_state())
os.exit(0)
`

// recoverScript box.cfg-recovers from the cwd (no schema creation — the spaces
// must come from the recovered snapshot + xlog) and prints the final state. A
// rejected file makes box.cfg raise, so tarantool exits non-zero.
const recoverScript = dumpStateLua + `
box.cfg{ work_dir = '.', log = 'tarantool.log', listen = box.NULL }
print(dump_state())
os.exit(0)
`

// TestRecovery_GeneratedFilesAccepted verifies that journal files written by
// go-xlog are accepted by Tarantool's recovery engine — a stronger guarantee
// than the xlog.pairs reader used by the tag-gated write tests.
//
// A real Tarantool bootstraps and writes a .snap + .xlog; go-xlog regenerates
// every .snap/.xlog into a fresh directory through its writer; a second
// Tarantool recovers from the go-xlog-written files and its state must equal
// the source instance's. Non-journal files (e.g. .vylog) are copied verbatim
// so only snap/xlog generation is under test.
func TestRecovery_GeneratedFilesAccepted(t *testing.T) {
	t.Parallel()

	bin := needTarantool(t)

	strategies := []struct {
		name string
		raw  bool // true: pipe.CopyRaw (verbatim blocks); false: pipe.Copy (re-encode)
	}{
		{"copy_reencode", false},
		{"copyraw_verbatim", true},
	}

	for _, st := range strategies {
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()

			src := t.TempDir()
			want := strings.TrimSpace(runLua(t, bin, src, bootstrapScript))
			require.NotEmpty(t, want, "bootstrap produced no state")

			dst := t.TempDir()
			regenerateJournal(t, src, dst, st.raw)

			got := strings.TrimSpace(runLua(t, bin, dst, recoverScript))
			require.Equalf(t, want, got,
				"recovery from go-xlog-generated files (%s) differs from source state", st.name)
		})
	}
}

// TestRecovery_RewrittenUUIDRejected documents the limit of a meta-only UUID
// rewrite: go-xlog can change the instance UUID in the meta header, but the UUID
// is also stored in the snapshot's _cluster system space, which the meta rewrite
// does not touch. The header change is real (go-xlog reads the new UUID back),
// but Tarantool rejects the now-inconsistent files on recovery.
func TestRecovery_RewrittenUUIDRejected(t *testing.T) {
	t.Parallel()

	bin := needTarantool(t)

	src := t.TempDir()
	require.NotEmpty(t, strings.TrimSpace(runLua(t, bin, src, bootstrapScript)), "bootstrap produced no state")

	dst := t.TempDir()
	regenerateJournal(t, src, dst, false)

	newID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	rewrote := 0

	for _, path := range journalFiles(t, dst) {
		require.NoErrorf(t, tools.ReplaceInstanceUUIDInPlace(path, newID), "rewrite UUID in %s", filepath.Base(path))

		meta, err := reader.ReadHeader(path)
		require.NoErrorf(t, err, "read header %s", filepath.Base(path))
		require.Equalf(t, newID, meta.InstanceUUID, "%s: meta UUID not rewritten", filepath.Base(path))

		rewrote++
	}

	require.Positive(t, rewrote, "no journal files to rewrite")

	// Recovery must fail: the header UUID no longer matches the _cluster row.
	_, err := tryRunLua(t, bin, dst, recoverScript)
	require.Error(t, err, "Tarantool unexpectedly accepted files with a rewritten instance UUID")
}

// needTarantool returns a usable Tarantool binary path, or skips the test.
// Resolution order: $TARANTOOL_BIN, then `tarantool` on PATH.
func needTarantool(t *testing.T) string {
	t.Helper()

	if bin := os.Getenv("TARANTOOL_BIN"); bin != "" {
		_, err := os.Stat(bin)
		require.NoErrorf(t, err, "TARANTOOL_BIN=%q set but not found", bin)

		return bin
	}

	bin, err := exec.LookPath("tarantool")
	if err != nil {
		t.Skip("tarantool binary not found (set TARANTOOL_BIN or add to PATH)")
	}

	return bin
}

// runLua executes script in workDir and returns stdout, failing the test on a
// non-zero exit (both streams attached).
func runLua(t *testing.T, bin, workDir, script string) string {
	t.Helper()

	stdout, err := tryRunLua(t, bin, workDir, script)
	require.NoErrorf(t, err, "tarantool run failed: %v", err)

	return stdout
}

// tryRunLua executes script in workDir and returns its stdout and the run error
// (nil on success). Used by the negative test, which expects a failure.
func tryRunLua(t *testing.T, bin, workDir, script string) (string, error) {
	t.Helper()

	scriptPath := filepath.Join(workDir, "script.lua")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), scriptPerm), "write lua script")

	cmd := exec.CommandContext(t.Context(), bin, scriptPath)
	cmd.Dir = workDir

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		t.Logf("tarantool stderr:\n%s", stderr.String())
	}

	return stdout.String(), err
}

// journalFiles returns the .snap/.xlog paths in dir, sorted.
func journalFiles(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "read dir")

	var out []string

	for _, e := range entries {
		switch filepath.Ext(e.Name()) {
		case ".snap", ".xlog":
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}

	return out
}

// regenerateJournal rewrites every .snap/.xlog in src into dst with go-xlog and
// copies any other journal file (e.g. .vylog) verbatim. The harness's own
// script.lua / tarantool.log are skipped.
func regenerateJournal(t *testing.T, src, dst string, raw bool) {
	t.Helper()

	entries, err := os.ReadDir(src)
	require.NoError(t, err, "read src dir")

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		name := e.Name()
		srcPath := filepath.Join(src, name)
		dstPath := filepath.Join(dst, name)

		switch filepath.Ext(name) {
		case ".snap", ".xlog":
			regenerateOne(t, srcPath, dstPath, raw)
		case ".lua", ".log":
			// Harness script / tarantool log — not part of the journal.
		default:
			copyFileVerbatim(t, srcPath, dstPath) // E.g. .vylog.
		}
	}
}

// regenerateOne reads srcPath with go-xlog and writes an equivalent file at
// dstPath through the go-xlog writer (preserving the meta header and name),
// either re-encoding every row (pipe.Copy) or forwarding blocks verbatim
// (pipe.CopyRaw).
func regenerateOne(t *testing.T, srcPath, dstPath string, raw bool) {
	t.Helper()

	r, err := reader.Open(srcPath)
	require.NoErrorf(t, err, "open %s", filepath.Base(srcPath))

	defer func() { _ = r.Close() }()

	w, err := writer.Create(dstPath, r.Meta().Clone())
	require.NoErrorf(t, err, "create %s", filepath.Base(dstPath))

	if raw {
		_, err = pipe.CopyRaw(r, w)
	} else {
		_, err = pipe.Copy(r, w)
	}

	require.NoErrorf(t, err, "regenerate %s", filepath.Base(srcPath))
	require.NoErrorf(t, w.Close(), "close %s", filepath.Base(dstPath))
}

func copyFileVerbatim(t *testing.T, srcPath, dstPath string) {
	t.Helper()

	data, err := os.ReadFile(srcPath)
	require.NoError(t, err, "read src file")
	require.NoError(t, os.WriteFile(dstPath, data, scriptPerm), "write dst file")
}

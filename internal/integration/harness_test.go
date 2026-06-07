//go:build tarantool

package integration

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
)

// findTarantool returns the path to a usable Tarantool binary, or "" if none
// is available. Resolution order: $TARANTOOL_BIN, then `tarantool` on PATH.
func findTarantool(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("TARANTOOL_BIN"); bin != "" {
		if _, err := os.Stat(bin); err == nil {
			return bin
		}
		require.Failf(t, "TARANTOOL_BIN not found", "TARANTOOL_BIN=%q set but not found on disk", bin)
	}
	if p, err := exec.LookPath("tarantool"); err == nil {
		return p
	}
	return ""
}

// requireTarantool skips the test if no Tarantool binary is available.
func requireTarantool(t *testing.T) string {
	t.Helper()
	bin := findTarantool(t)
	if bin == "" {
		t.Skip("tarantool binary not found (set TARANTOOL_BIN or add to PATH)")
	}
	return bin
}

// runLua writes script to a temp .lua file under workDir and executes
// `tarantool script.lua args...` with workDir as the process cwd. It returns
// stdout. A non-zero exit fails the test with both streams attached.
func runLua(t *testing.T, bin, workDir, script string, args ...string) string {
	t.Helper()
	scriptPath := filepath.Join(workDir, "script.lua")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o644), "write lua script")
	cmdArgs := append([]string{scriptPath}, args...)
	cmd := exec.Command(bin, cmdArgs...)
	cmd.Dir = workDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoErrorf(t, cmd.Run(), "tarantool run failed\n--- stdout ---\n%s\n--- stderr ---\n%s",
		stdout.String(), stderr.String())
	return stdout.String()
}

// luaRow mirrors the JSON shape that Tarantool's xlog.pairs emits per row
// (see src/box/lua/xlog.c). Header fields tsn/commit appear only for
// multi-statement transactions; absent JSON keys decode to the zero value.
type luaRow struct {
	Header struct {
		LSN       int64   `json:"lsn"`
		Type      string  `json:"type"`
		ReplicaID int64   `json:"replica_id"`
		TSN       int64   `json:"tsn"`
		Commit    bool    `json:"commit"`
		Timestamp float64 `json:"timestamp"`
	} `json:"HEADER"`
	Body struct {
		SpaceID int64         `json:"space_id"`
		Tuple   []interface{} `json:"tuple"`
	} `json:"BODY"`
}

// encodeDML builds a DML body map {0x10: spaceID, 0x21: tuple} as msgpack,
// suitable for format.XRow.BodyRaw. The tuple elements are encoded with the
// shared msgpack codec so strings/ints land as Tarantool expects them.
func encodeDML(t *testing.T, spaceID uint32, tuple ...interface{}) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(2), "encode map len")
	// 0x10 = IPROTO_SPACE_ID.
	require.NoError(t, enc.EncodeUint(0x10), "encode space key")
	require.NoError(t, enc.EncodeUint(uint64(spaceID)), "encode space id")
	// 0x21 = IPROTO_TUPLE.
	require.NoError(t, enc.EncodeUint(0x21), "encode tuple key")
	require.NoError(t, enc.EncodeArrayLen(len(tuple)), "encode tuple len")
	for i, v := range tuple {
		require.NoErrorf(t, enc.Encode(v), "encode tuple[%d]", i)
	}
	return buf.Bytes()
}

// decodeTuple unmarshals a msgpack tuple array (e.g. DMLBody.Tuple) into a
// []interface{} for value comparison.
func decodeTuple(t *testing.T, raw []byte) []interface{} {
	t.Helper()
	var out []interface{}
	require.NoError(t, msgpack.Unmarshal(raw, &out), "decode tuple")
	return out
}

// asInt64 reports whether v is any integer-valued number (msgpack decodes
// small ints to int8/uint8/…; JSON decodes all numbers to float64) and, if
// so, its int64 value.
func asInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		return int64(n), true
	case uint:
		return int64(n), true
	case float64:
		if float64(int64(n)) == n {
			return int64(n), true
		}
	case float32:
		if float64(int64(n)) == float64(n) {
			return int64(n), true
		}
	}
	return 0, false
}

// tuplesEqual compares two tuples elementwise, treating any two
// integer-valued numbers as equal regardless of their concrete Go type. This
// bridges the width-specific ints msgpack yields and the float64s JSON yields
// against the int64 literals the tests use as expectations.
func tuplesEqual(got, want []interface{}) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if gi, ok := asInt64(got[i]); ok {
			if wi, ok := asInt64(want[i]); ok {
				if gi != wi {
					return false
				}
				continue
			}
			return false
		}
		if !reflect.DeepEqual(got[i], want[i]) {
			return false
		}
	}
	return true
}

// nonEmptyLines splits s on newlines and drops blank lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

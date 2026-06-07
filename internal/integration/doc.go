// Package integration holds end-to-end tests that drive a real Tarantool
// binary to validate go-xlog against the reference implementation, in both
// directions:
//
//   - read:  Tarantool writes an xlog, go-xlog reads it and the decoded
//     rows/tuples/tx-grouping are asserted against what Tarantool was told
//     to insert.
//   - write: go-xlog writes an xlog, Tarantool's own `xlog.pairs` reader
//     consumes it, and the rows Tarantool decodes are asserted against what
//     go-xlog was told to write.
//
// The tests are gated behind the `tarantool` build tag so the default
// `go test ./...` (which has no Tarantool dependency) skips them entirely.
// Run them with:
//
//	go test -tags tarantool ./internal/integration/...
//
// Even with the tag set, each test skips gracefully when no Tarantool
// binary is found (see findTarantool). Point at a specific binary with the
// TARANTOOL_BIN environment variable; otherwise `tarantool` on PATH is used.
//
// This file carries no build tag so the directory always has a buildable
// Go file (and `go test ./...` reports "no test files" rather than an
// "build constraints exclude all Go files" error).
package integration

// Package compat holds historical read-compatibility tests: they decode the
// frozen multi-version Tarantool corpus under testdata/historical/ with the
// current library and assert the result against committed golden dumps and
// per-version semantic manifests.
//
// Unlike internal/integration (which needs a live Tarantool binary and is
// gated behind the `tarantool` build tag), these tests run on every
// `go test ./...` against the frozen corpus — no Tarantool, no Docker.
//
// Regenerate goldens after an intentional corpus or renderer change:
//
//	go test ./internal/compat/ -run Golden -update
//
// This file carries no build tag so the directory always has a buildable Go
// file even when the test files are excluded.
package compat

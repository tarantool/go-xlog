package follow

import (
	"context"
	"iter"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// newDirEngine builds an engine that tails a rotation chain in dirPath.
func newDirEngine(dirPath string, ft format.Filetype, opts []Option) *engine {
	return &engine{
		cfg:      applyOptions(opts),
		dirPath:  dirPath,
		filetype: ft,
	}
}

// Dir tails a WAL directory across rotation: it reads the start file (set by
// WithFromHead / WithStartLSN / WithStartVClock — required, else ErrNoStart),
// and when that file is finalised it switches to the next file in the chain and
// continues, indefinitely, until ctx is cancelled. Each switch validates chain
// continuity (the successor's PrevVClock must equal the current file's VClock);
// a break ends the iterator with ErrChainBroken.
//
//	for row, err := range follow.Dir(ctx, walDir, format.FiletypeXLOG, follow.WithFromHead()) {
//	    if err != nil { return err }
//	    // ... process row ...
//	}
//
// By default only finalised files are read; WithReadInprogress also tails the
// active .inprogress file for lower latency.
func Dir(ctx context.Context, dirPath string, ft format.Filetype, opts ...Option) iter.Seq2[format.XRow, error] {
	return func(yield func(format.XRow, error) bool) {
		newDirEngine(dirPath, ft, opts).run(ctx, yield)
	}
}

// DirTx is Dir at logical-transaction granularity, yielding one
// *reader.Transaction per committed tx across the whole chain.
func DirTx(ctx context.Context, dirPath string, ft format.Filetype, opts ...Option) iter.Seq2[*reader.Transaction, error] {
	return func(yield func(*reader.Transaction, error) bool) {
		newDirEngine(dirPath, ft, opts).runTx(ctx, yield)
	}
}

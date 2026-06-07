package follow

import (
	"context"
	"iter"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// newFileEngine builds an engine that tails a single file at path.
func newFileEngine(path string, opts []Option) *engine {
	cfg := applyOptions(opts)

	return &engine{
		cfg:      cfg,
		fileMode: true,
		path:     path,
		off:      cfg.startOffset,
	}
}

// File tails a single .xlog (or snap/vylog) file, yielding each row the writer
// appends. It blocks for new rows until the file is finalised — its EOF marker
// is written — at which point the iterator ends, or until ctx is cancelled
// (also a clean end). A terminal error (corruption, I/O) is yielded once as the
// last pair.
//
//	for row, err := range follow.File(ctx, path) {
//	    if err != nil { return err }
//	    // ... process row ...
//	}
//
// Use WithStartOffset to resume from a byte offset captured earlier via
// reader.Offset / Follower.Offset.
func File(ctx context.Context, path string, opts ...Option) iter.Seq2[format.XRow, error] {
	return func(yield func(format.XRow, error) bool) {
		newFileEngine(path, opts).run(ctx, yield)
	}
}

// FileTx is File at logical-transaction granularity: it yields one
// *reader.Transaction per committed tx, waiting for the commit row of an
// incomplete tx at the tail before emitting it.
func FileTx(ctx context.Context, path string, opts ...Option) iter.Seq2[*reader.Transaction, error] {
	return func(yield func(*reader.Transaction, error) bool) {
		newFileEngine(path, opts).runTx(ctx, yield)
	}
}

// Package follow tails Tarantool journal files as they are written. It adds
// two live-read modes on top of the forward-only reader:
//
//   - FollowFile / FollowFileTx — tail a single .xlog, blocking for rows the
//     writer appends, until the file is finalised (its EOF marker appears) or
//     the context is cancelled.
//   - FollowDir / FollowDirTx — tail a whole WAL directory: read the current
//     file, and when it is finalised switch to the next file in the rotation
//     chain (validating chain continuity), continuing indefinitely.
//
// Both are exposed as Go 1.23 iterators (iter.Seq2), matching reader.Rows /
// reader.Txs. Cancellation via context.Context ends the iterator cleanly
// (without yielding an error), the same way io.EOF ends the reader iterators.
//
// The mechanism is offset-tracked re-open: each poll re-opens the file at the
// byte offset a prior pass stopped at (reader.OpenAt + reader.Offset) and reads
// the newly-appended blocks, so already-consumed blocks are never re-read. A
// partial trailing block or a still-missing EOF marker means "wait for more";
// the real EOF marker (reader.SawEOFMarker) means "finalised". Directory and
// file growth are observed by a pluggable Watcher; the default polls.
package follow

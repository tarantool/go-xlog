package follow_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/tarantool/go-xlog/follow"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/writer"
)

// writeExampleFile writes a finalised xlog at path with one tx per lsn.
func writeExampleFile(path string, lsns ...int64) {
	w, err := writer.Create(path, metaWith(format.VClock{1: 0}, nil))
	if err != nil {
		log.Fatal(err)
	}

	for _, lsn := range lsns {
		if err := w.WriteTx([]format.XRow{row(lsn)}); err != nil {
			log.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		log.Fatal(err)
	}
}

// writeExampleChainFile writes one finalised <signature>.xlog into dirPath.
func writeExampleChainFile(dirPath string, vclock, prev format.VClock, lsn int64) {
	path := filepath.Join(dirPath, fmt.Sprintf("%020d.xlog", vclock.Signature()))

	w, err := writer.Create(path, metaWith(vclock, prev))
	if err != nil {
		log.Fatal(err)
	}

	if err := w.WriteTx([]format.XRow{row(lsn)}); err != nil {
		log.Fatal(err)
	}

	if err := w.Close(); err != nil {
		log.Fatal(err)
	}
}

// ExampleFile tails a single .xlog file. It blocks for rows as they are
// appended and ends when the file is finalised (its EOF marker is written). Here
// the file is already complete, so the follow drains it and stops.
func ExampleFile() {
	dir, err := os.MkdirTemp("", "follow-file")
	if err != nil {
		log.Fatal(err)
	}

	path := filepath.Join(dir, "00000000000000000000.xlog")
	writeExampleFile(path, 1, 2, 3)

	// Tail it: the loop yields each row, then ends at the EOF marker.
	for r, err := range follow.File(context.Background(), path) {
		if err != nil {
			log.Fatal(err)
		}

		fmt.Println(r.LSN)
	}

	_ = os.RemoveAll(dir)

	// Output:
	// 1
	// 2
	// 3
}

// ExampleDir tails a whole WAL directory, following rotation: it reads the
// current file and, when that file is finalised, switches to the next file in
// the chain — indefinitely, until the context is cancelled. A directory follow
// must be given a start position (here, the head of the chain). This example
// breaks out after the two rows it knows are present; a real consumer keeps
// ranging until it cancels the context.
func ExampleDir() {
	dir, err := os.MkdirTemp("", "follow-dir")
	if err != nil {
		log.Fatal(err)
	}

	// Two finalised files forming a chain: file 2's PrevVClock == file 1's VClock.
	writeExampleChainFile(dir, format.VClock{1: 1}, nil, 1)
	writeExampleChainFile(dir, format.VClock{1: 2}, format.VClock{1: 1}, 2)

	ctx, cancel := context.WithCancel(context.Background())

	seq := follow.Dir(ctx, dir, format.FiletypeXLOG, follow.WithFromHead())

	n := 0

	for r, err := range seq {
		if err != nil {
			log.Fatal(err)
		}

		fmt.Printf("replica %d lsn %d\n", r.ReplicaID, r.LSN)

		if n++; n == 2 {
			break // stop following (a real consumer would cancel on its own condition)
		}
	}

	cancel()

	_ = os.RemoveAll(dir)

	// Output:
	// replica 1 lsn 1
	// replica 1 lsn 2
}

// ExampleFollower shows the pull-style driver with offset checkpointing. Persist
// Offset() (with the current file) to resume after a restart via WithStartOffset.
func ExampleFollower() {
	dir, err := os.MkdirTemp("", "follow-er")
	if err != nil {
		log.Fatal(err)
	}

	path := filepath.Join(dir, "00000000000000000000.xlog")
	writeExampleFile(path, 1, 2)

	f := follow.NewFileFollower(path)
	ctx := context.Background()

	for {
		r, err := f.Next(ctx)
		if errors.Is(err, io.EOF) {
			break // the file was finalised
		}

		if err != nil {
			log.Fatal(err)
		}

		fmt.Println(r.LSN)

		_ = f.Offset() // resume point: follow.WithStartOffset(off) on a later run
	}

	_ = f.Close()
	_ = os.RemoveAll(dir)

	// Output:
	// 1
	// 2
}

package rotate_test

import (
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/tarantool/go-xlog/dir"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/rotate"
)

var exampleInstance = uuid.MustParse("11111111-2222-3333-4444-555555555555")

// exampleBody msgpack-encodes a one-element tuple so each row carries a valid
// body.
func exampleBody(v int) []byte {
	b, err := msgpack.Marshal([]int{v})
	if err != nil {
		panic(err)
	}

	return b
}

// ExampleRotatingWriter writes a chain of xlog files into one directory,
// rotating between transactions. With a tiny MaxFileSize every transaction
// after the first lands in a fresh file, so the three writes produce three
// files. Each file's Meta.PrevVClock links back to the previous file's
// Meta.VClock, keeping the chain consistent — which OpenDir then verifies.
func ExampleRotatingWriter() {
	baseDir, err := os.MkdirTemp("", "go-xlog-rotate-example")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(baseDir) }()

	// MaxFileSize(1) forces a rotation before every transaction but the first.
	rw, err := rotate.New(baseDir, format.FiletypeXLOG, exampleInstance,
		format.VClock{}, rotate.MaxFileSize(1))
	if err != nil {
		panic(err)
	}

	for lsn := int64(1); lsn <= 3; lsn++ {
		if err := rw.WriteTx([]format.XRow{
			{Type: iproto.IPROTO_INSERT, ReplicaID: 1, LSN: lsn, BodyRaw: exampleBody(int(lsn))},
		}); err != nil {
			panic(err)
		}
	}

	if err := rw.Close(); err != nil {
		panic(err)
	}

	// Index the resulting directory to inspect the chain.
	d, err := dir.OpenDir(baseDir, format.FiletypeXLOG)
	if err != nil {
		panic(err)
	}

	for _, f := range d.Files() {
		fmt.Printf("signature=%d vclock=%s prev=%s\n", f.Signature, f.VClock, f.PrevVClock)
	}

	// Output:
	// signature=0 vclock={} prev={}
	// signature=1 vclock={1: 1} prev={}
	// signature=2 vclock={1: 2} prev={1: 1}
}

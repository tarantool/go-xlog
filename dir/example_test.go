package dir_test

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/tarantool/go-xlog/dir"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/writer"
)

var exampleInstance = uuid.MustParse("11111111-2222-3333-4444-555555555555")

// writeXlog writes an empty xlog with the given VClock/PrevVClock header into
// baseDir. The filename is <signature>.xlog, and the in-meta VClock signature
// must match that stem — exactly the invariant OpenDir validates.
func writeXlog(baseDir string, vclock, prev format.VClock) {
	name := fmt.Sprintf("%d.xlog", vclock.Signature())

	meta := &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "example",
		InstanceUUID: exampleInstance,
		VClock:       vclock,
		PrevVClock:   prev,
	}

	w, err := writer.Create(filepath.Join(baseDir, name), meta)
	if err != nil {
		panic(err)
	}

	if err := w.Close(); err != nil {
		panic(err)
	}
}

// writeSampleDir populates a fresh temp directory with a two-file xlog chain:
// {1: 5} followed by {1: 10} (whose PrevVClock links back to the first). The
// caller is responsible for removing the returned directory.
func writeSampleDir() string {
	baseDir, err := os.MkdirTemp("", "go-xlog-dir-example")
	if err != nil {
		panic(err)
	}

	writeXlog(baseDir, format.VClock{1: 5}, nil)
	writeXlog(baseDir, format.VClock{1: 10}, format.VClock{1: 5})

	return baseDir
}

// ExampleOpenDir indexes a directory of xlog files. OpenDir reads each file's
// meta header, checks the filename signature against the in-meta vclock, and
// returns the entries sorted ascending by signature.
func ExampleOpenDir() {
	baseDir := writeSampleDir()

	defer func() { _ = os.RemoveAll(baseDir) }()

	d, err := dir.OpenDir(baseDir, format.FiletypeXLOG)
	if err != nil {
		panic(err)
	}

	for _, f := range d.Files() {
		fmt.Printf("%s signature=%d vclock=%s\n", filepath.Base(f.Path), f.Signature, f.VClock)
	}

	// Output:
	// 5.xlog signature=5 vclock={1: 5}
	// 10.xlog signature=10 vclock={1: 10}
}

// ExampleDir_LocateLSN finds which file holds a given per-replica LSN by
// projecting every indexed vclock onto that replica's axis.
func ExampleDir_LocateLSN() {
	baseDir := writeSampleDir()

	defer func() { _ = os.RemoveAll(baseDir) }()

	d, err := dir.OpenDir(baseDir, format.FiletypeXLOG)
	if err != nil {
		panic(err)
	}

	for _, lsn := range []int64{7, 10} {
		f, err := d.LocateLSN(1, lsn)
		if err != nil {
			panic(err)
		}

		fmt.Printf("replica 1 lsn %d is in %s\n", lsn, filepath.Base(f.Path))
	}

	// Output:
	// replica 1 lsn 7 is in 5.xlog
	// replica 1 lsn 10 is in 10.xlog
}

// ExampleDir_LocateVClock finds the file whose [VClock, nextVClock) interval
// contains a target position under the vector-clock partial order.
func ExampleDir_LocateVClock() {
	baseDir := writeSampleDir()

	defer func() { _ = os.RemoveAll(baseDir) }()

	d, err := dir.OpenDir(baseDir, format.FiletypeXLOG)
	if err != nil {
		panic(err)
	}

	f, err := d.LocateVClock(format.VClock{1: 7})
	if err != nil {
		panic(err)
	}

	fmt.Printf("vclock {1: 7} is in %s\n", filepath.Base(f.Path))

	// Output:
	// vclock {1: 7} is in 5.xlog
}

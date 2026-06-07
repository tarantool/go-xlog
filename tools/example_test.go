package tools_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
	"github.com/tarantool/go-xlog/tools"
	"github.com/tarantool/go-xlog/writer"
)

// writeSampleXlog writes a minimal one-row xlog to path, for the on-disk
// tools examples.
func writeSampleXlog(path string) {
	meta := &format.Meta{
		Filetype:     format.FiletypeXLOG,
		Version:      "example",
		InstanceUUID: uuid.MustParse("11111111-2222-3333-4444-555555555555"),
	}

	var buf bytes.Buffer

	w, err := writer.NewWriter(&buf, meta)
	if err != nil {
		panic(err)
	}

	body, err := msgpack.Marshal([]int{1})
	if err != nil {
		panic(err)
	}

	if err := w.WriteTx([]format.XRow{{Type: iproto.IPROTO_INSERT, LSN: 1, BodyRaw: body}}); err != nil {
		panic(err)
	}

	if err := w.Close(); err != nil {
		panic(err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		panic(err)
	}
}

// ExampleRewriteMetaFields copies an xlog while overwriting selected header
// fields (here the instance UUID and version). The tx blocks are copied
// verbatim — only the header changes.
func ExampleRewriteMetaFields() {
	dir, err := os.MkdirTemp("", "go-xlog-example")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	src := filepath.Join(dir, "src.xlog")
	dst := filepath.Join(dir, "dst.xlog")

	writeSampleXlog(src)

	newID := uuid.MustParse("99999999-aaaa-bbbb-cccc-dddddddddddd")
	if err := tools.RewriteMetaFields(src, dst,
		tools.WithInstanceUUID(newID),
		tools.WithVersion("3.8.0"),
	); err != nil {
		panic(err)
	}

	m, err := reader.ReadHeader(dst)
	if err != nil {
		panic(err)
	}

	fmt.Printf("uuid=%s version=%s\n", m.InstanceUUID, m.Version)

	// Output:
	// uuid=99999999-aaaa-bbbb-cccc-dddddddddddd version=3.8.0
}

// ExampleReplaceInstanceUUIDInPlace re-stamps the instance UUID directly in a
// file, without copying it — sound because a canonical UUID is always 36
// bytes, so nothing downstream shifts.
func ExampleReplaceInstanceUUIDInPlace() {
	dir, err := os.MkdirTemp("", "go-xlog-example")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "x.xlog")
	writeSampleXlog(path)

	newID := uuid.MustParse("99999999-aaaa-bbbb-cccc-dddddddddddd")
	if err := tools.ReplaceInstanceUUIDInPlace(path, newID); err != nil {
		panic(err)
	}

	m, err := reader.ReadHeader(path)
	if err != nil {
		panic(err)
	}

	fmt.Printf("uuid=%s\n", m.InstanceUUID)

	// Output:
	// uuid=99999999-aaaa-bbbb-cccc-dddddddddddd
}

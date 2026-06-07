// Command cat prints the contents of Tarantool .snap/.xlog files to stdout,
// a pure-Go reimplementation of `tt cat` built on go-xlog.
//
// Unlike `tt cat`, which drives a Lua script inside a real Tarantool, this
// reads and decodes the files directly with the go-xlog reader — no Tarantool
// binary required.
//
// Usage:
//
//	cat [flags] <FILE|DIR>...
//
// Flags mirror `tt cat`: --from/--to (LSN range), --timestamp, --space and
// --replica (repeatable filters), --show-system, --format (yaml|json|lua),
// and --recursive/-r.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tarantool/go-iproto"
	"github.com/vmihailenco/msgpack/v5"
	"gopkg.in/yaml.v3"

	"github.com/tarantool/go-xlog/examples/internal/checkpoint"
	"github.com/tarantool/go-xlog/format"
)

// newFlagSet builds a ContinueOnError flag set with a short usage banner.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <FILE|DIR>...\n\n", name)
		fs.PrintDefaults()
	}
	return fs
}

// intSlice is a repeatable, comma-separated integer flag (e.g. --space).
type intSlice []int

func (s *intSlice) String() string {
	parts := make([]string, len(*s))
	for i, v := range *s {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}

func (s *intSlice) Set(v string) error {
	for part := range strings.SplitSeq(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return err
		}
		*s = append(*s, n)
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cat:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	fs := newFlagSet("cat")
	opts := checkpoint.DefaultOpts()
	var (
		from, to   uint64
		timestamp  string
		outFormat  string
		spaces     intSlice
		replicas   intSlice
		showSystem bool
		recursive  bool
	)
	fs.Uint64Var(&from, "from", 0, "show operations starting from the given LSN")
	fs.Uint64Var(&to, "to", 0, "show operations ending before the given LSN (0 = no limit)")
	fs.StringVar(&timestamp, "timestamp", "", "show operations before the given timestamp (unix seconds or RFC3339)")
	fs.StringVar(&outFormat, "format", "yaml", "output format: yaml, json or lua")
	fs.Var(&spaces, "space", "filter by space id (repeatable)")
	fs.Var(&replicas, "replica", "filter by replica id (repeatable)")
	fs.BoolVar(&showSystem, "show-system", false, "show the contents of system spaces")
	fs.BoolVar(&recursive, "recursive", false, "process journal files in directories recursively")
	fs.BoolVar(&recursive, "r", false, "shorthand for --recursive")

	if err := fs.Parse(argv); err != nil {
		return err
	}
	files := fs.Args()
	if len(files) == 0 {
		fs.Usage()
		return fmt.Errorf("specify at least one .xlog/.snap file or directory")
	}

	emit, err := formatter(outFormat)
	if err != nil {
		return err
	}

	opts.From = from
	if to != 0 {
		opts.To = to
	}
	if opts.Timestamp, err = checkpoint.ParseTimestamp(timestamp); err != nil {
		return err
	}
	opts.Spaces = spaces
	opts.Replicas = replicas
	opts.ShowSystem = showSystem

	walFiles, err := checkpoint.CollectFiles(files, recursive)
	if err != nil {
		return err
	}

	lastFile := ""
	return checkpoint.Process(walFiles, checkpoint.Filter(opts), func(rec checkpoint.Record) error {
		if rec.File != lastFile {
			fmt.Fprintf(os.Stderr, "• Result of cat: the file %q is processed below •\n", rec.File)
			lastFile = rec.File
		}
		return emit(rec)
	})
}

// formatter returns the per-record printer for the requested format.
func formatter(name string) (func(checkpoint.Record) error, error) {
	switch name {
	case "yaml":
		return emitYAML, nil
	case "json":
		return emitJSON, nil
	case "lua":
		return emitLua, nil
	default:
		return nil, fmt.Errorf("unknown --format %q (want yaml, json or lua)", name)
	}
}

// displayRecord is the HEADER/BODY shape Tarantool's own cat produces.
type displayRecord struct {
	Header displayHeader  `json:"HEADER" yaml:"HEADER"`
	Body   map[string]any `json:"BODY,omitempty" yaml:"BODY,omitempty"`
}

type displayHeader struct {
	LSN       int64   `json:"lsn" yaml:"lsn"`
	ReplicaID uint32  `json:"replica_id" yaml:"replica_id"`
	Type      string  `json:"type" yaml:"type"`
	Timestamp float64 `json:"timestamp,omitempty" yaml:"timestamp,omitempty"`
}

func toDisplay(rec checkpoint.Record) (displayRecord, error) {
	d := displayRecord{Header: displayHeader{
		LSN:       rec.Row.LSN,
		ReplicaID: rec.Row.ReplicaID,
		Type:      format.TypeName(rec.Row.Type),
		Timestamp: rec.Row.Timestamp,
	}}
	body, err := bodyMap(rec.Body)
	if err != nil {
		return d, err
	}
	d.Body = body
	return d, nil
}

// bodyMap decodes the DML body fields into a name-keyed map for display.
func bodyMap(b *format.DMLBody) (map[string]any, error) {
	if b == nil {
		return nil, nil
	}
	m := map[string]any{}
	if b.SpaceID != 0 {
		m["space_id"] = b.SpaceID
	}
	for name, raw := range map[string][]byte{
		"tuple":      b.Tuple,
		"key":        b.Key,
		"operations": b.Ops,
		"old_tuple":  b.OldTuple,
		"new_tuple":  b.NewTuple,
	} {
		v, err := mpDecode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", name, err)
		}
		if v != nil {
			m[name] = v
		}
	}
	for key, raw := range b.Extras {
		v, err := mpDecode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode body key 0x%02x: %w", key, err)
		}
		m[fmt.Sprintf("0x%02x", key)] = v
	}
	return m, nil
}

func emitYAML(rec checkpoint.Record) error {
	d, err := toDisplay(rec)
	if err != nil {
		return err
	}
	out, err := yaml.Marshal(d)
	if err != nil {
		return err
	}
	fmt.Print("---\n", string(out))
	return nil
}

func emitJSON(rec checkpoint.Record) error {
	d, err := toDisplay(rec)
	if err != nil {
		return err
	}
	out, err := json.Marshal(d)
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// emitLua prints a replayable box.space[...]:op(...) Statement, mirroring
// tt's lua formatter. Non-DML rows and rows without a space are skipped.
func emitLua(rec checkpoint.Record) error {
	if rec.Body == nil || rec.Body.SpaceID == 0 || rec.Row.Type == iproto.IPROTO_NOP {
		return nil
	}
	op := strings.ToLower(format.TypeName(rec.Row.Type))

	var sb strings.Builder
	fmt.Fprintf(&sb, "box.space[%d]:%s(", rec.Body.SpaceID, op)
	switch rec.Row.Type {
	case iproto.IPROTO_INSERT, iproto.IPROTO_REPLACE, iproto.IPROTO_UPSERT:
		if err := writeLuaField(&sb, rec.Body.Tuple); err != nil {
			return err
		}
		if rec.Row.Type == iproto.IPROTO_UPSERT {
			sb.WriteString(", ")
			if err := writeLuaField(&sb, rec.Body.Ops); err != nil {
				return err
			}
		}
	case iproto.IPROTO_DELETE:
		if err := writeLuaField(&sb, rec.Body.Key); err != nil {
			return err
		}
	case iproto.IPROTO_UPDATE:
		if err := writeLuaField(&sb, rec.Body.Key); err != nil {
			return err
		}
		sb.WriteString(", ")
		// For UPDATE the operations travel in the IPROTO_TUPLE field.
		if err := writeLuaField(&sb, rec.Body.Tuple); err != nil {
			return err
		}
	default:
		return nil
	}
	sb.WriteString(")")
	fmt.Println(sb.String())
	return nil
}

func writeLuaField(sb *strings.Builder, raw []byte) error {
	v, err := mpDecode(raw)
	if err != nil {
		return err
	}
	if v == nil {
		v = []any{}
	}
	writeLuaValue(sb, v)
	return nil
}

func writeLuaValue(sb *strings.Builder, v any) {
	switch val := v.(type) {
	case string:
		writeLuaString(sb, val)
	case []byte:
		writeLuaString(sb, string(val))
	case []any:
		sb.WriteString("{")
		for i, e := range val {
			if i > 0 {
				sb.WriteString(", ")
			}
			writeLuaValue(sb, e)
		}
		sb.WriteString("}")
	case map[string]any:
		sb.WriteString("{")
		first := true
		for k, e := range val {
			if !first {
				sb.WriteString(", ")
			}
			first = false
			fmt.Fprintf(sb, "[%q] = ", k)
			writeLuaValue(sb, e)
		}
		sb.WriteString("}")
	default:
		fmt.Fprintf(sb, "%v", val)
	}
}

// writeLuaString hex-escapes every byte, matching tt's cat.lua so binary
// keys/values survive the round-trip into a Lua string literal.
func writeLuaString(sb *strings.Builder, s string) {
	sb.WriteString("'")
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(sb, "\\x%02x", s[i])
	}
	sb.WriteString("'")
}

// mpDecode decodes one msgpack value into a normalized Go value suitable for
// yaml/json/lua rendering. Returns nil for an empty field.
func mpDecode(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := msgpack.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return normalize(v), nil
}

// normalize rewrites map[any]any into map[string]any so the value marshals
// cleanly to JSON.
func normalize(v any) any {
	switch val := v.(type) {
	case map[string]any:
		for k, e := range val {
			val[k] = normalize(e)
		}
		return val
	case map[any]any:
		m := make(map[string]any, len(val))
		for k, e := range val {
			m[fmt.Sprintf("%v", k)] = normalize(e)
		}
		return m
	case []any:
		for i, e := range val {
			val[i] = normalize(e)
		}
		return val
	default:
		return val
	}
}

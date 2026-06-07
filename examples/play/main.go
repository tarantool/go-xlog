// Command play replays the contents of Tarantool .snap/.xlog files into a
// running Tarantool instance, a pure-Go reimplementation of `tt play` built
// on go-xlog and the go-tarantool client.
//
// It reads and filters the files locally with go-xlog (no Tarantool binary
// needed to read them), then applies each surviving DML statement
// (insert/replace/update/delete/upsert) to the target instance over IPROTO.
//
// Usage:
//
//	play [flags] <URI> <FILE|DIR>...
//
// URI is host:port, optionally with embedded credentials
// (user:pass@host:port); --username/-u and --password/-p override it. The
// filter flags match `cat` / `tt play`. SSL transport is out of scope for
// this example.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tarantool/go-iproto"
	"github.com/tarantool/go-tarantool/v2"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/tarantool/go-xlog/examples/internal/checkpoint"
)

const connectTimeout = 10 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "play:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	fs := newFlagSet("play")
	opts := checkpoint.DefaultOpts()
	var (
		username, password string
		from, to           uint64
		timestamp          string
		spaces, replicas   intSlice
		showSystem         bool
		recursive          bool
	)
	fs.StringVar(&username, "username", "", "username (overrides URI credentials)")
	fs.StringVar(&username, "u", "", "shorthand for --username")
	fs.StringVar(&password, "password", "", "password (overrides URI credentials)")
	fs.StringVar(&password, "p", "", "shorthand for --password")
	fs.Uint64Var(&from, "from", 0, "replay operations starting from the given LSN")
	fs.Uint64Var(&to, "to", 0, "replay operations ending before the given LSN (0 = no limit)")
	fs.StringVar(&timestamp, "timestamp", "", "replay operations before the given timestamp (unix seconds or RFC3339)")
	fs.Var(&spaces, "space", "filter by space id (repeatable)")
	fs.Var(&replicas, "replica", "filter by replica id (repeatable)")
	fs.BoolVar(&showSystem, "show-system", false, "replay the contents of system spaces")
	fs.BoolVar(&recursive, "recursive", false, "process journal files in directories recursively")
	fs.BoolVar(&recursive, "r", false, "shorthand for --recursive")

	if err := fs.Parse(argv); err != nil {
		return err
	}
	args := fs.Args()
	if len(args) < 2 {
		fs.Usage()
		return fmt.Errorf("specify a URI and at least one .xlog/.snap file or directory")
	}

	uri := args[0]
	user, pass, uri := splitCredentials(uri, username, password)

	opts.From = from
	if to != 0 {
		opts.To = to
	}
	var err error
	if opts.Timestamp, err = checkpoint.ParseTimestamp(timestamp); err != nil {
		return err
	}
	opts.Spaces = spaces
	opts.Replicas = replicas
	opts.ShowSystem = showSystem

	files, err := checkpoint.CollectFiles(args[1:], recursive)
	if err != nil {
		return err
	}

	conn, err := connect(uri, user, pass)
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseGraceful() }()

	applied := 0
	err = checkpoint.Process(files, checkpoint.Filter(opts), func(rec checkpoint.Record) error {
		ok, aerr := apply(conn, rec)
		if aerr != nil {
			return fmt.Errorf("apply lsn=%d from %q: %w", rec.Row.LSN, rec.File, aerr)
		}
		if ok {
			applied++
		}
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Printf("• Play completed: %d statement(s) applied to %s •\n", applied, uri)
	return nil
}

// splitCredentials resolves the effective user/password and the bare URI.
// Explicit --username/--password win over credentials embedded in the URI.
func splitCredentials(uri, flagUser, flagPass string) (user, pass, bareURI string) {
	user, pass, bareURI = flagUser, flagPass, uri
	if at := strings.LastIndex(uri, "@"); at != -1 {
		creds, host := uri[:at], uri[at+1:]
		bareURI = host
		u, p, _ := strings.Cut(creds, ":")
		if flagUser == "" {
			user = u
		}
		if flagPass == "" {
			pass = p
		}
	}
	return user, pass, bareURI
}

func connect(uri, user, pass string) (*tarantool.Connection, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	dialer := tarantool.NetDialer{Address: uri, User: user, Password: pass}
	conn, err := tarantool.Connect(ctx, dialer, tarantool.Opts{})
	if err != nil {
		return nil, fmt.Errorf("connect to %q: %w", uri, err)
	}
	return conn, nil
}

// apply executes one DML record against the instance. It returns false for
// rows it skips (no body, no space, or a non-DML type such as NOP/RAFT).
func apply(conn *tarantool.Connection, rec checkpoint.Record) (bool, error) {
	b := rec.Body
	if b == nil || b.SpaceID == 0 {
		return false, nil
	}

	var req tarantool.Request
	switch rec.Row.Type {
	case iproto.IPROTO_INSERT:
		req = tarantool.NewInsertRequest(b.SpaceID).Tuple(raw(b.Tuple))
	case iproto.IPROTO_REPLACE:
		req = tarantool.NewReplaceRequest(b.SpaceID).Tuple(raw(b.Tuple))
	case iproto.IPROTO_DELETE:
		req = tarantool.NewDeleteRequest(b.SpaceID).Key(raw(b.Key))
	case iproto.IPROTO_UPDATE:
		// For UPDATE the operations travel in the IPROTO_TUPLE field.
		ops, err := buildOperations(b.Tuple)
		if err != nil {
			return false, err
		}
		req = tarantool.NewUpdateRequest(b.SpaceID).Key(raw(b.Key)).Operations(ops)
	case iproto.IPROTO_UPSERT:
		ops, err := buildOperations(b.Ops)
		if err != nil {
			return false, err
		}
		req = tarantool.NewUpsertRequest(b.SpaceID).Tuple(raw(b.Tuple)).Operations(ops)
	default:
		return false, nil
	}

	if _, err := conn.Do(req).Get(); err != nil {
		return false, err
	}
	return true, nil
}

// raw forwards an on-disk msgpack field verbatim to the client, avoiding a
// decode/re-encode round-trip. An empty field becomes an empty array.
func raw(b []byte) any {
	if len(b) == 0 {
		return []any{}
	}
	return msgpack.RawMessage(b)
}

// buildOperations decodes the on-disk update/upsert operations array and
// rebuilds it as a *tarantool.Operations, the only shape the client accepts.
func buildOperations(rawOps []byte) (*tarantool.Operations, error) {
	ops := tarantool.NewOperations()
	if len(rawOps) == 0 {
		return ops, nil
	}

	var decoded []any
	if err := msgpack.Unmarshal(rawOps, &decoded); err != nil {
		return nil, fmt.Errorf("decode operations: %w", err)
	}

	for _, item := range decoded {
		op, ok := item.([]any)
		if !ok || len(op) < 2 {
			return nil, fmt.Errorf("malformed update operation: %v", item)
		}
		operator, _ := op[0].(string)
		field, err := toInt(op[1])
		if err != nil {
			return nil, fmt.Errorf("operation field: %w", err)
		}

		switch operator {
		case "+":
			ops.Add(field, op[2])
		case "-":
			ops.Subtract(field, op[2])
		case "&":
			ops.BitwiseAnd(field, op[2])
		case "|":
			ops.BitwiseOr(field, op[2])
		case "^":
			ops.BitwiseXor(field, op[2])
		case "!":
			ops.Insert(field, op[2])
		case "=":
			ops.Assign(field, op[2])
		case "#":
			n, err := toInt(op[2])
			if err != nil {
				return nil, fmt.Errorf("delete count: %w", err)
			}
			ops.Delete(field, n)
		case ":":
			if len(op) < 5 {
				return nil, fmt.Errorf("malformed splice operation: %v", op)
			}
			pos, err := toInt(op[2])
			if err != nil {
				return nil, fmt.Errorf("splice pos: %w", err)
			}
			length, err := toInt(op[3])
			if err != nil {
				return nil, fmt.Errorf("splice len: %w", err)
			}
			repl, _ := op[4].(string)
			ops.Splice(field, pos, length, repl)
		default:
			return nil, fmt.Errorf("unsupported update operator %q", operator)
		}
	}

	return ops, nil
}

// toInt coerces any msgpack-decoded integer kind to int.
func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int8:
		return int(n), nil
	case int16:
		return int(n), nil
	case int32:
		return int(n), nil
	case int64:
		return int(n), nil
	case uint:
		return int(n), nil //nolint:gosec // update field numbers/counts are small.
	case uint8:
		return int(n), nil
	case uint16:
		return int(n), nil
	case uint32:
		return int(n), nil
	case uint64:
		return int(n), nil //nolint:gosec // update field numbers/counts are small.
	default:
		return 0, fmt.Errorf("not an integer: %T", v)
	}
}

// newFlagSet builds a ContinueOnError flag set with a short usage banner.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <URI> <FILE|DIR>...\n\n", name)
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

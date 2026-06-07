# go-xlog examples

Two runnable commands built on go-xlog, reimplementing `tt cat` and `tt play`
in pure Go — no Tarantool binary needed to read the files.

These live in their own module so their dependencies (go-tarantool, msgpack,
yaml) stay out of the core `go-xlog` module's graph.

## cat

Prints the contents of `.snap`/`.xlog` files to stdout in `yaml`, `json`, or
`lua` format.

```sh
go run ./cat path/to/00000000000000000000.xlog
go run ./cat --format lua --space 512 path/to/dir/
go run ./cat --from 10 --to 100 --format json file.xlog
go run ./cat --recursive path/to/dir1 path/to/dir2
```

Flags: `--from`/`--to` (LSN range), `--timestamp` (Unix seconds or RFC3339),
`--space`/`--replica` (repeatable filters), `--show-system`, `--format`,
`--recursive`/`-r`.

## play

Replays the DML statements (`insert`/`replace`/`update`/`delete`/`upsert`) of
`.snap`/`.xlog` files into a running Tarantool instance over IPROTO.

```sh
go run ./play localhost:3301 path/to/file.xlog
go run ./play -u admin -p secret app-host:3301 path/to/dir/
go run ./play 'admin:secret@localhost:3301' --space 512 file.xlog
```

The first argument is the target `host:port` (credentials may be embedded as
`user:pass@host:port`, or passed via `--username`/`-u` and `--password`/`-p`).
The remaining arguments and the filter flags match `cat`. SSL transport is out
of scope for this example.

## Build

```sh
go build -o cat-bin ./cat
go build -o play-bin ./play
```

(An explicit `-o` is needed because `cat/` and `play/` are also the package
directory names.) Or install both onto your `PATH` with `go install ./...`.

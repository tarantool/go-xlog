// Separate module so the cat/play example dependencies (go-tarantool,
// msgpack, yaml) stay out of the core go-xlog module's graph.
module github.com/tarantool/go-xlog/examples

go 1.24

require (
	github.com/tarantool/go-iproto v1.1.0
	github.com/tarantool/go-tarantool/v2 v2.4.2
	github.com/tarantool/go-xlog v0.0.0
	github.com/vmihailenco/msgpack/v5 v5.4.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
)

replace github.com/tarantool/go-xlog => ../

# go-xlog — common developer tasks.
#
# Override any variable on the command line, e.g.:
#   make test PKG=./reader/
#   make bench BENCH=BenchmarkCopyRaw PKG=./pipe/
#   make fuzz FUZZTIME=10s

GO         ?= go
GOLANGCI   ?= golangci-lint
PKG        ?= ./...
FUZZTIME   ?= 1m
FUZZMINTIME ?= 5s
BENCH      ?= .
BENCHFLAGS ?= -benchmem

.DEFAULT_GOAL := help

.PHONY: help test test-race test-integration cover lint lint-fix fmt vet tidy bench fuzz ci

help: ## Show this help.
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

## --- testing ---

test: ## Run the unit/round-trip/compat tests.
	$(GO) test $(PKG)

test-race: ## Run the tests with the race detector.
	$(GO) test -race $(PKG) -count=10

test-integration: ## Run the live-Tarantool integration tests (needs a tarantool binary).
	$(GO) test -tags tarantool ./internal/integration/...

cover: ## Run the tests and open the HTML coverage report.
	$(GO) test -coverprofile=coverage.out $(PKG)
	$(GO) tool cover -html=coverage.out

## --- linting ---

lint: ## Run golangci-lint.
	$(GOLANGCI) run $(PKG)

lint-fix: ## Run golangci-lint and apply autofixes.
	$(GOLANGCI) run --fix $(PKG)

fmt: ## gofmt-format all Go files in place.
	$(GO) fmt $(PKG)

vet: ## Run go vet.
	$(GO) vet $(PKG)

tidy: ## Tidy and verify go.mod / go.sum.
	$(GO) mod tidy
	$(GO) mod verify

## --- benchmarking ---

bench: ## Run benchmarks (BENCH=<regex> PKG=<pkg> to narrow).
	$(GO) test -run '^$$' -bench '$(BENCH)' $(BENCHFLAGS) $(PKG)

## --- fuzzing ---

fuzz: ## Fuzz every target for FUZZTIME each (default 1m; override e.g. FUZZTIME=10s).
	@for pkg in $$($(GO) list $(PKG)); do \
		for fz in $$($(GO) test $$pkg -list '^Fuzz' 2>/dev/null | grep '^Fuzz' || true); do \
			echo "=== $$pkg $$fz (fuzztime $(FUZZTIME), minimize $(FUZZMINTIME)) ==="; \
			$(GO) test $$pkg -run '^$$' -fuzz "^$$fz$$" -fuzztime $(FUZZTIME) -fuzzminimizetime $(FUZZMINTIME) || exit 1; \
		done; \
	done

## --- aggregate ---

ci: lint test ## Run lint + tests (the pre-push gate).

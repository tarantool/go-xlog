# Contribution Guide

## First steps

The project requires Go 1.24 or later. Clone the repository and tidy
dependencies.

```sh
$ git clone https://github.com/tarantool/go-xlog
$ cd go-xlog
$ go mod tidy
```

The runnable example programs (`cat`, `play`) live in a separate module under
`examples/` with its own `go.mod`, so their dependencies stay out of the core
library's graph. Build them from that directory:

```sh
$ cd examples && go build ./...
```

## Branch naming conventions

When creating feature branches, follow these naming patterns:

- `<user.name>/gh-<issue-id>-short-description` (for GitHub issues)
- `<user.name>/gh-no-short-description` (for work not tied to an issue)

For release branches, use:

- `<user.name>/release-v<version>`

Replace `<user.name>` with your Git username, `<issue-id>` with the issue
number, and `<short-description>` with lowercase, hyphen-separated words (only
letters, numbers, and dashes).

Examples:

- `bigbes/gh-17-initial-version`
- `bigbes/gh-no-readme-cleanup`
- `bigbes/release-v0.1.0`

Although working on a feature without an open issue is possible, avoid it.

## Running tests

```sh
# Unit, round-trip, and compatibility tests for every package:
make test

# The same with the race detector (-count=10 to shake out flakes):
make test-race

# A single package, e.g. reader:
go test ./reader/
```

### Test naming conventions

Follow these patterns when naming test functions:

```go
Test<FunctionName>_<Description>
Test<TypeName>_<Method>_<Description>
```

### Integration tests

Some tests replay real journals against a live Tarantool to prove byte-for-byte
compatibility. They are behind the `tarantool` build tag and need a `tarantool`
binary on `PATH`:

```sh
make test-integration
```

When Tarantool is absent the integration tests are skipped automatically, so the
default `make test` and the public CI stay green without it.

### Code coverage

```sh
make cover
```

This writes `coverage.out` and opens the HTML report. The project keeps
statement coverage at or above 80% for every package.

## Examples

Public APIs are documented with runnable `ExampleXxx` functions that carry a
verified `// Output:` block, so they cannot rot. Run them with:

```sh
go test -run Example ./...
```

## Linting

```sh
make lint       # golangci-lint run
make lint-fix   # apply autofixes
```

The linter starts from golangci-lint's full set (`default: all`) with a curated
list of disabled linters, each justified by a comment in `.golangci.yml`. A
`depguard` allow-list pins the permitted imports; an import outside that list
fails linting rather than surfacing after publication. The pinned linter version
is kept in sync between the Makefile and CI.

## Formatting

Code is formatted with `gofmt` and `goimports` (both enforced by the linter):

```sh
make fmt
```

## Spell checking

Code and documentation are spell-checked with
[codespell](https://github.com/codespell-project/codespell) (configured in
`.codespellrc`):

```sh
codespell
```

## Code review checklist

- The public API exposes only what external users need; everything else stays
  unexported or under `internal/`.
- Every exported function, type, variable, and constant has a doc comment.
- Code is DRY.
- New features come with functional and, where relevant, performance tests.
- Bug-fix commits include a regression test built from the reproducer, and the
  test fails without the fix.
- There are no changes to files unrelated to the issue.
- There are no obviously flaky tests.
- A `CHANGELOG.md` entry is present.
- New public methods carry an executable example with reference output.
- The generated documentation looks right. Run `godoc -http=:6060` and open
  <http://127.0.0.1:6060>.
- Comments, commit titles, and identifiers are grammatically correct — start
  with a capital letter, end with a period.

## Commit message guidelines

A commit message has three parts: a header, a body, and links to related
issues.

```
prefix: commit title

The commit body, explaining what changed and why.

Part of #17
```

- The header is `prefix: subject` — a short prefix, a colon, a space, and a
  lowercase subject in the imperative mood: it must complete the sentence "If
  applied, this commit will …". Keep it within ~50 characters, no trailing
  period. Typical prefixes are the package name (`reader`, `writer`, `format`,
  …) or a shared scope (`ci`, `tests`, `doc`, `all`).
- Do not put issue numbers in the header — links go in the body.
- Separate the body from the header with a blank line; wrap body lines at 72
  characters. The body explains *what* and *why*, not *how*.
- Put issue links on the last lines of the body. Use `Part of #NNN` on
  intermediate commits of a task and `Closes #NNN` on the final commit of the
  pull request.
- Record co-authorship with a `Co-authored-by:` trailer.
- Use your real name and working email.
- Each commit is atomic and self-contained.

## Pull request process

1. Fork the repository and create a feature branch.
2. Self-review before requesting reviewers: all changes belong to the pull
   request, the diff is under ~500–700 lines (excluding file deletions), every
   line is explainable, tests pass, and new features have documentation.
3. Update `CHANGELOG.md` with a user-facing description of your change.
4. Open the pull request with a clear description of the change and its purpose.
5. A pull request merges after at least two approvals; the reviewer, not the
   author, resolves review threads.

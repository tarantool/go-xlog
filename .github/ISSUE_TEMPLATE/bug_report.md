---
name: Bug report
about: Report a problem with reading, writing, or rewriting Tarantool journal files
title: ''
labels: bug
assignees: ''
---

## Description

A clear and concise description of what the bug is.

## Steps to reproduce

1. ...
2. ...
3. ...

If possible, attach a minimal code snippet and a (sanitized) `.xlog` / `.snap` /
`.vylog` file that reproduces the problem.

## Expected behavior

What you expected to happen.

## Actual behavior

What actually happened. Include the full error message and, if relevant, the
output of `tarantoolctl cat` / `tt cat` for the same file.

## Environment

- go-xlog version (commit or tag):
- Go version (`go version`):
- OS and architecture:
- Tarantool version that produced the file (if applicable):

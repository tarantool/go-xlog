# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/)
and this project adheres to [Semantic
Versioning](http://semver.org/spec/v2.0.0.html) except to the first release.

## [Unreleased]

### Added

- format: Pure byte-level codec for the Tarantool journal format — `Meta`,
  `XRow`, `VClock`, and encode/decode helpers with no I/O. Format-faithful to
  Tarantool 3.x (version `0.13`), reads legacy `0.12` with `AcceptV012()`,
  and mirrors the C implementation byte-for-byte (CRC32C/Castagnoli, msgpack
  row encoding, zstd compression threshold). File types `XLOG`, `SNAP`,
  `VYLOG`, `RUN`, and `INDEX`.

### Changed

### Fixed

// Package tools provides high-level meta-only rewrites of Tarantool
// journal files. The headline operation is RewriteMeta, which parses the
// meta header, hands a clone to a caller-supplied transform, writes the
// new header, and then byte-copies the rest of the source (tx blocks +
// EOF marker) verbatim to the destination. CRCs are preserved because
// the on-disk bytes are unchanged.
package tools

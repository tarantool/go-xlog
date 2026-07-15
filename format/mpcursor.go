package format

import "fmt"

// MPCursor reads consecutive MessagePack values directly from a byte slice.
// Returned Str and Raw slices alias the input passed to NewMPCursor.
type MPCursor struct {
	b   []byte
	off int
}

// NewMPCursor returns a zero-allocation cursor positioned at the start of b.
func NewMPCursor(b []byte) MPCursor {
	return MPCursor{b: b}
}

// ArrayLen consumes an array header and returns its element count.
func (c *MPCursor) ArrayLen() (int, error) {
	value, n, err := readMPArrayLen(c.remaining())
	if err != nil {
		return 0, fmt.Errorf("format: MPCursor.ArrayLen: %w", err)
	}

	c.off += n

	return value, nil
}

// MapLen consumes a map header and returns its entry count.
func (c *MPCursor) MapLen() (int, error) {
	value, n, err := readMPMapLen(c.remaining())
	if err != nil {
		return 0, fmt.Errorf("format: MPCursor.MapLen: %w", err)
	}

	c.off += n

	return value, nil
}

// Uint consumes an unsigned integer scalar.
func (c *MPCursor) Uint() (uint64, error) {
	value, n, err := readMPUint(c.remaining())
	if err != nil {
		return 0, fmt.Errorf("format: MPCursor.Uint: %w", err)
	}

	c.off += n

	return value, nil
}

// Int consumes a signed or unsigned integer scalar representable as int64.
func (c *MPCursor) Int() (int64, error) {
	value, n, err := readMPInt(c.remaining())
	if err != nil {
		return 0, fmt.Errorf("format: MPCursor.Int: %w", err)
	}

	c.off += n

	return value, nil
}

// Str consumes a string and returns its payload as an alias of the input.
func (c *MPCursor) Str() ([]byte, error) {
	value, n, err := readMPStr(c.remaining())
	if err != nil {
		return nil, fmt.Errorf("format: MPCursor.Str: %w", err)
	}

	c.off += n

	return value, nil
}

// Bool consumes a MessagePack boolean scalar.
func (c *MPCursor) Bool() (bool, error) {
	b := c.remaining()
	if len(b) == 0 {
		return false, fmt.Errorf("format: MPCursor.Bool: %w", ErrEmptyInput)
	}

	switch b[0] {
	case mpcFalse:
		c.off++

		return false, nil
	case mpcTrue:
		c.off++

		return true, nil
	default:
		return false, fmt.Errorf("format: MPCursor.Bool: %w 0x%02x", ErrUnexpectedTag, b[0])
	}
}

// Raw consumes one complete value and returns its verbatim bytes as an alias
// of the input.
func (c *MPCursor) Raw() ([]byte, error) {
	b := c.remaining()

	n, err := skipMP(b)
	if err != nil {
		return nil, fmt.Errorf("format: MPCursor.Raw: %w", err)
	}

	c.off += n

	return b[:n], nil
}

// Skip consumes one complete value.
func (c *MPCursor) Skip() error {
	n, err := skipMP(c.remaining())
	if err != nil {
		return fmt.Errorf("format: MPCursor.Skip: %w", err)
	}

	c.off += n

	return nil
}

// More reports whether unread bytes remain.
func (c *MPCursor) More() bool {
	return c.off < len(c.b)
}

func (c *MPCursor) remaining() []byte {
	return c.b[c.off:]
}

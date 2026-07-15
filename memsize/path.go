package memsize

import (
	"errors"
	"fmt"
	"math"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidIndexPath marks an invalid Tarantool JSON index path.
var ErrInvalidIndexPath = errors.New("invalid index JSON path")

const decimalRadix = 10

type jsonPathTokenKind uint8

const (
	jsonPathEnd jsonPathTokenKind = iota
	jsonPathKey
	jsonPathIndex
	jsonPathAny
)

type jsonPathToken struct {
	kind  jsonPathTokenKind
	key   string
	index int
}

type jsonPathLexer struct {
	path   string
	offset int
}

func pathHasWildcard(path string) (bool, error) {
	_, wildcard, err := pathMultikeySuffix(path)

	return wildcard, err
}

func pathMultikeySuffix(path string) (string, bool, error) {
	lexer := jsonPathLexer{path: path}
	hasWildcard := false
	suffixOffset := 0

	for {
		token, err := lexer.next()
		if err != nil {
			return "", false, err
		}

		switch token.kind {
		case jsonPathAny:
			if hasWildcard {
				return "", false, fmt.Errorf("%w: more than one [*] token", ErrInvalidIndexPath)
			}

			hasWildcard = true
			suffixOffset = lexer.offset
		case jsonPathEnd:
			if !hasWildcard {
				return "", false, nil
			}

			return path[suffixOffset:], true, nil
		}
	}
}

func (l *jsonPathLexer) next() (jsonPathToken, error) {
	if l.offset == len(l.path) {
		return jsonPathToken{kind: jsonPathEnd}, nil
	}

	start := l.offset

	switch l.path[l.offset] {
	case '[':
		return l.bracketToken()
	case '.':
		l.offset++
		if l.offset == len(l.path) {
			return jsonPathToken{}, l.pathError(start, "trailing dot")
		}

		return l.identifierToken()
	default:
		if l.offset != 0 {
			return jsonPathToken{}, l.pathError(start, "missing path separator")
		}

		return l.identifierToken()
	}
}

func (l *jsonPathLexer) bracketToken() (jsonPathToken, error) {
	start := l.offset
	l.offset++

	if l.offset == len(l.path) {
		return jsonPathToken{}, l.pathError(start, "unterminated bracket")
	}

	switch l.path[l.offset] {
	case '*':
		l.offset++
		if !l.consumeClosingBracket() {
			return jsonPathToken{}, l.pathError(start, "invalid wildcard")
		}

		return jsonPathToken{kind: jsonPathAny}, nil
	case '\'', '"':
		quote := l.path[l.offset]
		l.offset++
		keyStart := l.offset

		for l.offset < len(l.path) && l.path[l.offset] != quote {
			_, width := utf8.DecodeRuneInString(l.path[l.offset:])
			if width == 0 || width == 1 && l.path[l.offset] >= utf8.RuneSelf {
				return jsonPathToken{}, l.pathError(l.offset, "invalid UTF-8")
			}

			l.offset += width
		}

		if l.offset == keyStart || l.offset == len(l.path) {
			return jsonPathToken{}, l.pathError(start, "invalid quoted key")
		}

		key := l.path[keyStart:l.offset]
		l.offset++

		if !l.consumeClosingBracket() {
			return jsonPathToken{}, l.pathError(start, "unterminated quoted key")
		}

		return jsonPathToken{kind: jsonPathKey, key: key}, nil
	default:
		if l.path[l.offset] < '0' || l.path[l.offset] > '9' {
			return jsonPathToken{}, l.pathError(l.offset, "expected array index")
		}

		value := 0

		for l.offset < len(l.path) && l.path[l.offset] >= '0' && l.path[l.offset] <= '9' {
			digit := int(l.path[l.offset] - '0')
			if value > (math.MaxInt-digit)/decimalRadix {
				return jsonPathToken{}, l.pathError(start, "array index overflows int")
			}

			value = value*decimalRadix + digit
			l.offset++
		}

		if value < 1 || !l.consumeClosingBracket() {
			return jsonPathToken{}, l.pathError(start, "invalid one-based array index")
		}

		return jsonPathToken{kind: jsonPathIndex, index: value - 1}, nil
	}
}

func (l *jsonPathLexer) identifierToken() (jsonPathToken, error) {
	start := l.offset

	r, width := utf8.DecodeRuneInString(l.path[l.offset:])
	if r == utf8.RuneError && width == 1 || r != '_' && !unicode.IsLetter(r) {
		return jsonPathToken{}, l.pathError(start, "invalid identifier")
	}

	l.offset += width

	for l.offset < len(l.path) {
		r, width = utf8.DecodeRuneInString(l.path[l.offset:])
		if r == utf8.RuneError && width == 1 {
			return jsonPathToken{}, l.pathError(l.offset, "invalid UTF-8")
		}

		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			break
		}

		l.offset += width
	}

	return jsonPathToken{kind: jsonPathKey, key: l.path[start:l.offset]}, nil
}

func (l *jsonPathLexer) consumeClosingBracket() bool {
	if l.offset == len(l.path) || l.path[l.offset] != ']' {
		return false
	}

	l.offset++

	return true
}

func (l *jsonPathLexer) pathError(offset int, detail string) error {
	return fmt.Errorf("%w at byte %d: %s", ErrInvalidIndexPath, offset, detail)
}

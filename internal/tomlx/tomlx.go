// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

// Package tomlx parses the subset of TOML that xmat's config files use.
//
// Supported: comments, [tables] (bare, quoted and dotted names), dotted keys,
// basic/literal/multi-line strings, integers, booleans, arrays (multi-line,
// trailing commas, comments) and inline tables. Not supported: arrays of
// tables ([[x]]), floats, and date/time values; these produce a clear error
// instead of being silently misread. Key order is preserved so callers can
// honor declaration order.
package tomlx

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Table is an ordered TOML table. Values are string, int64, bool, []any or *Table.
type Table struct {
	keys   []string
	vals   map[string]any
	header bool // defined by a [header]
	dotted bool // created by a dotted key
	inline bool // an inline table (immutable once closed)
}

func newTable() *Table { return &Table{vals: map[string]any{}} }

// Keys returns the table's keys in declaration order.
func (t *Table) Keys() []string { return append([]string(nil), t.keys...) }

// Get returns the value stored under key.
func (t *Table) Get(key string) (any, bool) {
	v, ok := t.vals[key]
	return v, ok
}

func (t *Table) set(k string, v any) {
	if _, ok := t.vals[k]; !ok {
		t.keys = append(t.keys, k)
	}
	t.vals[k] = v
}

// Error is a parse error with a 1-based position.
type Error struct {
	Line, Col int
	Msg       string
}

func (e *Error) Error() string { return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Col, e.Msg) }

const eof = rune(-1)

type parser struct {
	s     []rune
	pos   int
	depth int
	root  *Table
	cur   *Table
}

// Limits for hostile input: config files come from commits.
const (
	MaxSize  = 1 << 20 // bytes
	maxDepth = 32      // nested arrays / inline tables
)

// Parse parses src into an ordered table tree.
func Parse(src string) (t *Table, err error) {
	if len(src) > MaxSize {
		return nil, &Error{Line: 1, Col: 1, Msg: fmt.Sprintf("file is larger than %d bytes", MaxSize)}
	}
	if !utf8.ValidString(src) {
		return nil, &Error{Line: 1, Col: 1, Msg: "file is not valid UTF-8"}
	}
	src = strings.TrimPrefix(src, "\uFEFF")
	p := &parser{s: []rune(src), root: newTable()}
	p.cur = p.root
	defer func() {
		if r := recover(); r != nil {
			pe, ok := r.(*Error)
			if !ok {
				panic(r)
			}
			t, err = nil, pe
		}
	}()
	p.document()
	return p.root, nil
}

func (p *parser) fail(format string, a ...any) {
	line, col := 1, 1
	for i := 0; i < p.pos && i < len(p.s); i++ {
		if p.s[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	panic(&Error{Line: line, Col: col, Msg: fmt.Sprintf(format, a...)})
}

func (p *parser) peek() rune { return p.at(0) }

func (p *parser) at(off int) rune {
	if p.pos+off >= len(p.s) {
		return eof
	}
	return p.s[p.pos+off]
}

func (p *parser) skipWS() {
	for c := p.peek(); c == ' ' || c == '\t'; c = p.peek() {
		p.pos++
	}
}

func (p *parser) skipComment() {
	if p.peek() != '#' {
		return
	}
	for c := p.peek(); c != '\n' && c != eof; c = p.peek() {
		if c < 0x20 && c != '\t' && !(c == '\r' && p.at(1) == '\n') {
			p.fail("control character in comment")
		}
		p.pos++
	}
}

// newline consumes a newline if present and reports whether it did.
func (p *parser) newline() bool {
	switch {
	case p.peek() == '\n':
		p.pos++
		return true
	case p.peek() == '\r' && p.at(1) == '\n':
		p.pos += 2
		return true
	}
	return false
}

// skipAll skips whitespace, newlines and comments (inside arrays and inline tables).
func (p *parser) skipAll() {
	for {
		p.skipWS()
		p.skipComment()
		if !p.newline() {
			return
		}
	}
}

func (p *parser) expectEOL() {
	p.skipWS()
	p.skipComment()
	if p.peek() == eof || p.newline() {
		return
	}
	p.fail("expected end of line, found %q", p.peek())
}

func (p *parser) document() {
	for {
		p.skipWS()
		switch c := p.peek(); {
		case c == eof:
			return
		case p.newline():
		case c == '#':
			p.skipComment()
		case c == '[':
			p.header()
		default:
			p.keyval(p.cur)
			p.expectEOL()
		}
	}
}

func (p *parser) header() {
	p.pos++ // [
	if p.peek() == '[' {
		p.fail("arrays of tables ([[...]]) are not supported by xmat's config parser")
	}
	p.skipWS()
	keys := p.key()
	p.skipWS()
	if p.peek() != ']' {
		p.fail("expected ']' to close table header")
	}
	p.pos++
	p.expectEOL()

	t := p.root
	for i, k := range keys {
		v, ok := t.vals[k]
		if !ok {
			nt := newTable()
			t.set(k, nt)
			t = nt
			continue
		}
		tt, isTable := v.(*Table)
		if !isTable || tt.inline {
			p.fail("key %q is already defined as a value", strings.Join(keys[:i+1], "."))
		}
		if i == len(keys)-1 && (tt.header || tt.dotted) {
			p.fail("table [%s] is defined more than once", strings.Join(keys, "."))
		}
		t = tt
	}
	t.header = true
	p.cur = t
}

func (p *parser) key() []string {
	var keys []string
	for {
		p.skipWS()
		keys = append(keys, p.simpleKey())
		p.skipWS()
		if p.peek() != '.' {
			return keys
		}
		p.pos++
	}
}

func isBare(c rune) bool {
	return c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func (p *parser) simpleKey() string {
	switch p.peek() {
	case '"':
		if p.at(1) == '"' && p.at(2) == '"' {
			p.fail("multi-line strings cannot be used as keys")
		}
		return p.basicString()
	case '\'':
		if p.at(1) == '\'' && p.at(2) == '\'' {
			p.fail("multi-line strings cannot be used as keys")
		}
		return p.literalString()
	}
	start := p.pos
	for isBare(p.peek()) {
		p.pos++
	}
	if p.pos == start {
		if p.peek() == eof {
			p.fail("expected a key, found end of file")
		}
		p.fail("expected a key, found %q", p.peek())
	}
	return string(p.s[start:p.pos])
}

func (p *parser) keyval(t *Table) {
	keys := p.key()
	p.skipWS()
	if p.peek() != '=' {
		p.fail("expected '=' after key %q", strings.Join(keys, "."))
	}
	p.pos++
	p.skipWS()
	v := p.value()

	for i, k := range keys[:len(keys)-1] {
		ex, ok := t.vals[k]
		if !ok {
			nt := newTable()
			nt.dotted = true
			t.set(k, nt)
			t = nt
			continue
		}
		tt, isTable := ex.(*Table)
		if !isTable || tt.inline || tt.header {
			p.fail("cannot extend %q with a dotted key", strings.Join(keys[:i+1], "."))
		}
		t = tt
	}
	last := keys[len(keys)-1]
	if _, dup := t.vals[last]; dup {
		p.fail("duplicate key %q", strings.Join(keys, "."))
	}
	t.set(last, v)
}

var intRE = regexp.MustCompile(`^[+-]?(0|[1-9](_?[0-9])*)$`)

func (p *parser) value() any {
	switch c := p.peek(); {
	case c == '"':
		if p.at(1) == '"' && p.at(2) == '"' {
			return p.mlBasicString()
		}
		return p.basicString()
	case c == '\'':
		if p.at(1) == '\'' && p.at(2) == '\'' {
			return p.mlLiteralString()
		}
		return p.literalString()
	case c == '[':
		return p.array()
	case c == '{':
		return p.inlineTable()
	case c == 't' || c == 'f':
		for _, w := range []string{"true", "false"} {
			if p.hasWord(w) {
				p.pos += len(w)
				return w == "true"
			}
		}
	case c == '+' || c == '-' || (c >= '0' && c <= '9'):
		start := p.pos
		for c := p.peek(); isBare(c) || c == '+' || c == '.' || c == ':'; c = p.peek() {
			p.pos++
		}
		tok := string(p.s[start:p.pos])
		if !intRE.MatchString(tok) {
			p.pos = start
			p.fail("unsupported value %q (only integers are supported, not floats or dates)", tok)
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(tok, "_", ""), 10, 64)
		if err != nil {
			p.pos = start
			p.fail("integer %q out of range", tok)
		}
		return n
	case c == eof:
		p.fail("expected a value, found end of file")
	}
	p.fail("expected a value (string, integer, boolean, array or inline table), found %q", p.peek())
	return nil
}

func (p *parser) hasWord(w string) bool {
	for i, r := range w {
		if p.at(i) != r {
			return false
		}
	}
	return !isBare(p.at(len(w)))
}

func (p *parser) nest() func() {
	p.depth++
	if p.depth > maxDepth {
		p.fail("values nested more than %d levels deep", maxDepth)
	}
	return func() { p.depth-- }
}

func (p *parser) array() []any {
	defer p.nest()()
	p.pos++ // [
	arr := []any{}
	for {
		p.skipAll()
		if p.peek() == ']' {
			p.pos++
			return arr
		}
		arr = append(arr, p.value())
		p.skipAll()
		switch p.peek() {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return arr
		default:
			p.fail("expected ',' or ']' in array")
		}
	}
}

func (p *parser) inlineTable() *Table {
	defer p.nest()()
	p.pos++ // {
	t := newTable()
	for {
		p.skipAll()
		if p.peek() == '}' {
			p.pos++
			t.inline = true
			return t
		}
		p.keyval(t)
		p.skipAll()
		switch p.peek() {
		case ',':
			p.pos++
		case '}':
			p.pos++
			t.inline = true
			return t
		default:
			p.fail("expected ',' or '}' in inline table")
		}
	}
}

func (p *parser) checkChar(c rune, multiline bool) {
	if c == '\t' {
		return
	}
	if multiline && (c == '\n' || (c == '\r' && p.at(1) == '\n')) {
		return
	}
	if c < 0x20 || c == 0x7f {
		p.fail("control character U+%04X in string", c)
	}
}

func (p *parser) basicString() string {
	p.pos++ // "
	var b strings.Builder
	for {
		c := p.peek()
		switch {
		case c == eof || c == '\n' || c == '\r':
			p.fail("unterminated string")
		case c == '"':
			p.pos++
			return b.String()
		case c == '\\':
			p.escape(&b)
		default:
			p.checkChar(c, false)
			b.WriteRune(c)
			p.pos++
		}
	}
}

func (p *parser) mlBasicString() string {
	p.pos += 3
	p.newline() // a newline right after the opening quotes is trimmed
	var b strings.Builder
	for {
		c := p.peek()
		switch {
		case c == eof:
			p.fail("unterminated multi-line string")
		case c == '"' && p.at(1) == '"' && p.at(2) == '"':
			n := 3
			for p.at(n) == '"' {
				n++
			}
			if n > 5 {
				p.fail("too many quotes at end of multi-line string")
			}
			b.WriteString(strings.Repeat(`"`, n-3))
			p.pos += n
			return b.String()
		case c == '\\':
			// A backslash at the end of a line trims the newline and leading whitespace.
			j := 1
			for p.at(j) == ' ' || p.at(j) == '\t' {
				j++
			}
			if p.at(j) == '\n' || (p.at(j) == '\r' && p.at(j+1) == '\n') {
				p.pos += j
				for {
					if p.newline() {
						continue
					}
					if c := p.peek(); c == ' ' || c == '\t' {
						p.pos++
						continue
					}
					break
				}
				continue
			}
			p.escape(&b)
		case p.newline():
			b.WriteByte('\n')
		default:
			p.checkChar(c, true)
			b.WriteRune(c)
			p.pos++
		}
	}
}

func (p *parser) escape(b *strings.Builder) {
	p.pos++ // backslash
	c := p.peek()
	p.pos++
	switch c {
	case 'b':
		b.WriteByte('\b')
	case 't':
		b.WriteByte('\t')
	case 'n':
		b.WriteByte('\n')
	case 'f':
		b.WriteByte('\f')
	case 'r':
		b.WriteByte('\r')
	case 'e':
		b.WriteByte(0x1b)
	case '"':
		b.WriteByte('"')
	case '\\':
		b.WriteByte('\\')
	case 'u', 'U':
		n := 4
		if c == 'U' {
			n = 8
		}
		if p.pos+n > len(p.s) {
			p.fail("truncated unicode escape")
		}
		hex := string(p.s[p.pos : p.pos+n])
		v, err := strconv.ParseUint(hex, 16, 32)
		if err != nil || !utf8.ValidRune(rune(v)) {
			p.fail("invalid unicode escape \\%c%s", c, hex)
		}
		b.WriteRune(rune(v))
		p.pos += n
	default:
		p.pos--
		p.fail("invalid escape sequence \\%c (use '...' literal strings for Windows paths)", c)
	}
}

func (p *parser) literalString() string {
	p.pos++ // '
	start := p.pos
	for {
		c := p.peek()
		switch {
		case c == eof || c == '\n' || c == '\r':
			p.fail("unterminated literal string")
		case c == '\'':
			s := string(p.s[start:p.pos])
			p.pos++
			return s
		default:
			p.checkChar(c, false)
			p.pos++
		}
	}
}

func (p *parser) mlLiteralString() string {
	p.pos += 3
	p.newline()
	var b strings.Builder
	for {
		c := p.peek()
		switch {
		case c == eof:
			p.fail("unterminated multi-line literal string")
		case c == '\'' && p.at(1) == '\'' && p.at(2) == '\'':
			n := 3
			for p.at(n) == '\'' {
				n++
			}
			if n > 5 {
				p.fail("too many quotes at end of multi-line literal string")
			}
			b.WriteString(strings.Repeat("'", n-3))
			p.pos += n
			return b.String()
		case p.newline():
			b.WriteByte('\n')
		default:
			p.checkChar(c, true)
			b.WriteRune(c)
			p.pos++
		}
	}
}

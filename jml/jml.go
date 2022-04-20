package jml

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type jsonMarshaler struct{ marshaler }
type jmlMarshaler struct{ marshaler }
type jmlLexer struct{ lexer }
type jsonLexer struct{ lexer }

type marshaler struct {
	ts     []token
	out    []byte
	i      int
	parent string
}
type lexer struct {
	in                   string
	ts                   []token
	i, start, width, lvl int
}
type lexFn func() lexFn
type token struct {
	k, v       string
	index, lvl int
}

const digits = "0123456789"

func Unmarshal(jml []byte, v interface{}) error {
	jl := &jmlLexer{lexer{in: string(jml)}}
	for f := jl.lexSpace; f != nil; {
		f = f()
	}
	jm := jsonMarshaler{marshaler{ts: jl.ts}}
	if err := jm.marshalValue(); err != nil {
		return err
	} else if jm.next().k != "eof" {
		return fmt.Errorf("remainder: %v", jm.ts[jm.i:])
	}
	return json.Unmarshal(jm.out, v)
}

func Marshal(v interface{}) ([]byte, error) {
	bs, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	jl := jsonLexer{lexer{in: string(bs)}}
	for f := jl.lexSpace; f != nil; {
		f = f()
	}
	jm := jmlMarshaler{marshaler{ts: jl.ts}}
	if err := jm.marshalValue(0); err != nil {
		return nil, err
	} else if jm.next().k != "eof" {
		return nil, fmt.Errorf("remainder: %v", jm.ts[jm.i:])
	}
	return jm.out, nil
}

func (j *jsonLexer) lexSpace() lexFn {
	switch r := j.next(); {
	case r == -1:
		return nil
	case r == '[', r == ']', r == '{', r == '}':
		return j.emit("object", j.lexSpace)
	case r == ',':
		j.start = j.i
		return j.lexSpace
	case r == '"':
		return j.lexStringOrKey
	case r == '-' || r == '+' || (r >= '0' && r <= '9'):
		return j.lexNumber
	default:
		return j.lexSymbol
	}
}

func (j *jsonLexer) lexStringOrKey() lexFn {
	for c := j.next(); c != -1; c = j.next() {
		if c == '\\' {
			j.next()
		} else if c == '"' && j.peek() == ':' {
			j.next()
			return j.emit("key", j.lexSpace)
		} else if c == '"' && strings.Contains(j.value(), `\n`) {
			return j.emit("multiline-string", j.lexSpace)
		} else if c == '"' {
			return j.emit("string", j.lexSpace)
		}
	}
	return j.errorf("unterminated string")
}

func (j *jsonLexer) lexNumber() lexFn {
	j.acceptNumber()
	return j.emit("number", j.lexSpace)
}

func (j *jsonLexer) lexSymbol() lexFn {
	j.acceptWhile(func(r rune) bool { return !strings.ContainsRune(` \t\n{}[],"`, r) }, -1)
	switch v := j.value(); v {
	case "true", "false", "null", "undefined":
		return j.emit("symbol", j.lexSpace)
	default:
		return j.errorf("unexpected '%s'", v)
	}
}

func (j *jsonMarshaler) marshalValue() error {
	switch t := j.next(); t.k {
	case "key":
		return j.marshalMap(t.lvl)
	case "list":
		return j.marshalList(t.lvl)
	case "symbol", "number":
		return j.write(t.v)
	case "string":
		return j.write(`"`, t.v[1:len(t.v)-1], `"`)
	default:
		return fmt.Errorf("%v", t)
	}
}

func (j *jsonMarshaler) marshalObject(k, open, close string, lvl int, f func(token) string) error {
	j.write(open)
	j.backup()
	for t := j.next(); t.k != "eof"; t = j.next() {
		if t.lvl > lvl {
			return fmt.Errorf("unexpected %#v in object", t)
		} else if t.lvl < lvl || t.k != k {
			j.backup()
			break
		} else if f != nil {
			j.write(f(t))
		}
		if t2 := j.peek(); t2.lvl <= lvl {
			j.write("null")
		} else if err := j.marshalValue(); err != nil {
			return err
		}
		j.write(",")
	}
	j.out[len(j.out)-1] = close[0]
	return nil
}

func (j *jsonMarshaler) marshalMap(lvl int) error {
	return j.marshalObject("key", "{", "}", lvl, func(t token) string {
		return `"` + t.v[:len(t.v)-1] + `":`
	})
}

func (j *jsonMarshaler) marshalList(lvl int) error {
	return j.marshalObject("list", "[", "]", lvl, nil)
}

func (j *jmlLexer) lexSpace() lexFn {
	j.acceptRun(" \t")
	j.start = j.i
	switch r := j.next(); {
	case r == -1:
		return nil
	case r == '\n':
		return j.lexIndent
	case r == '"', r == '\'':
		return j.lexString
	case r == '|':
		return j.lexMultiLineString
	case ('0' <= r && r <= '9') || r == '+' || r == '-':
		return j.lexKeyOrListOrNumber
	case r == '#':
		return j.lexComment
	default:
		return j.lexKeyOrSymbol
	}
}

func (j *jmlLexer) lexKeyOrSymbol() lexFn {
	j.acceptWhile(func(r rune) bool { return !strings.ContainsRune(" \t\n:#", r) }, -1)
	if j.peek() == ':' {
		j.next()
		j.emit("key", nil)
		j.lvl += 2
		return j.lexSpace
	} else if v := j.value(); v != "true" && v != "false" && v != "null" {
		return j.errorf("unexpected symbol '%s'", v)
	}
	return j.emit("symbol", j.lexSpace)
}

func (j *jmlLexer) lexIndent() lexFn {
	j.acceptRun(" \t")
	j.lvl, j.start = j.i-j.start-1, j.i
	return j.lexSpace
}

func (j *jmlLexer) lexString() lexFn {
	q := rune(j.in[j.i-1])
	for r := j.next(); r != q; r = j.next() {
		if r == '\\' {
			r = j.next()
		} else if r == -1 || r == '\n' {
			return j.errorf("unterminated string")
		}
	}
	return j.emit("string", j.lexSpace)
}

func (j *jmlLexer) lexMultiLineString() lexFn {
	lines, lvl := []string{}, j.lvl
	if c := j.acceptWhile(func(r rune) bool { return r != '\n' }, -1); c != 0 {
		return j.errorf("multiline string with content on | line")
	}
	j.next()
	for {
		c := j.acceptWhile(func(r rune) bool { return r != '\n' }, -1)
		rl := j.in[j.i-c : j.i]
		j.next()
		if tl := strings.TrimLeftFunc(rl, unicode.IsSpace); rl == "" && j.i < len(j.in) {
			lines = append(lines, rl)
		} else if len(rl)-len(tl) >= lvl {
			lines = append(lines, rl[lvl:])
		} else {
			j.i -= c + 2
			break
		}
	}
	v := `"` + strings.ReplaceAll(strings.Join(lines, "\n"), "\n", "\\n") + `"`
	j.start, j.ts = j.i, append(j.ts, token{"string", v, j.start, j.lvl})
	return j.lexSpace
}

func (j *jmlLexer) lexComment() lexFn {
	j.acceptWhile(func(r rune) bool { return r != '\n' }, -1)
	j.start, j.i = j.i, j.i
	return j.lexSpace
}

func (j *jmlLexer) lexKeyOrListOrNumber() lexFn {
	j.acceptNumber()
	if v := j.value(); unicode.IsSpace(j.peek()) {
		if v == "-" {
			j.emit("list", nil)
			j.lvl += 2
			return j.lexSpace
		} else {
			return j.emit("number", j.lexSpace)
		}
	}
	return j.lexKeyOrSymbol
}

func (j *jmlMarshaler) marshalValue(lvl int) error {
	switch t := j.next(); {
	case t.k == "object" && t.v == "{":
		return j.marshalMap(lvl)
	case t.k == "object" && t.v == "[":
		return j.marshalList(lvl)
	case t.k == "multiline-string":
		return j.marshalMultilineString(t, lvl)
	case t.k == "symbol", t.k == "number", t.k == "string":
		return j.write(t.v)
	default:
		return fmt.Errorf("%v", t)
	}
}

func (j *jmlMarshaler) marshalMultilineString(t token, lvl int) error {
	lines, indent := strings.Split(t.v[1:len(t.v)-1], `\n`), strings.Repeat(" ", lvl)
	return j.write("|\n", indent, strings.Join(lines, "\n"+indent))
}

func (j *jmlMarshaler) marshalObject(lvl int, close string, f func(token) string) error {
	for t, isMap, start := j.peek(), close == "}", j.i; t.k != "eof"; t = j.peek() {
		if t.k == "object" && t.v == close {
			j.next()
			return nil
		} else if (isMap && j.parent == "]" && j.i == start) || j.i == 1 {
			j.write(f(t))
		} else {
			j.write("\n", strings.Repeat(" ", lvl), f(t))
		}
		if isMap {
			t = j.next()
		}
		if t := j.peek(); t.k != "object" || t.v == "{" {
			j.write(" ")
		}
		j.parent = close
		if err := j.marshalValue(lvl + 2); err != nil {
			return err
		}
	}
	return fmt.Errorf("unexpected eof")
}

func (j *jmlMarshaler) marshalMap(lvl int) error {
	return j.marshalObject(lvl, "}", func(t token) string { return t.v[1:len(t.v)-2] + ":" })
}

func (j *jmlMarshaler) marshalList(lvl int) error {
	return j.marshalObject(lvl, "]", func(t token) string { return "-" })
}

func (m *marshaler) next() token {
	m.i++
	if m.i-1 >= len(m.ts) {
		return token{"eof", "", -1, -1}
	}
	return m.ts[m.i-1]
}

func (m *marshaler) peek() token {
	t := m.next()
	m.i--
	return t
}

func (m *marshaler) backup() {
	m.i--
}

func (m *marshaler) write(xs ...string) error {
	for _, x := range xs {
		m.out = append(m.out, x...)
	}
	return nil
}

func (l *lexer) next() rune {
	if l.i >= len(l.in) {
		l.width = 0
		return -1
	}
	r, w := utf8.DecodeRuneInString(l.in[l.i:])
	l.width = w
	l.i += l.width
	return r
}

func (l *lexer) peek() rune {
	r := l.next()
	l.backup()
	return r
}

func (l *lexer) backup() {
	l.i -= l.width
	_, w := utf8.DecodeRuneInString(l.in[l.i:])
	l.width = w
}

func (l *lexer) emit(k string, f lexFn) lexFn {
	l.ts, l.start = append(l.ts, token{k, l.value(), l.start, l.lvl}), l.i
	return f
}

func (l *lexer) value() string {
	return l.in[l.start:l.i]
}

func (l *lexer) acceptNumber() {
	l.accept("+-")
	l.acceptRun(digits)
	if l.accept(".") != 0 {
		l.acceptRun(digits)
	}
	if l.accept("eE") != 0 {
		l.accept("+-")
		l.acceptRun(digits)
	}
}

func (l *lexer) accept(valid string) int {
	return l.acceptWhile(func(r rune) bool { return strings.ContainsRune(valid, r) }, 1)
}

func (l *lexer) acceptRun(valid string) int {
	return l.acceptWhile(func(r rune) bool { return strings.ContainsRune(valid, r) }, -1)
}

func (l *lexer) acceptWhile(f func(rune) bool, n int) int {
	c := 0
	for ; f(l.next()) && l.i < len(l.in) && (n == -1 || c < n); c++ {
	}
	l.backup()
	return c
}

func (l *lexer) errorf(format string, args ...interface{}) lexFn {
	l.ts = append(l.ts, token{"err", fmt.Sprintf(format, args...), l.start, l.lvl})
	return nil
}

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package tomlx

import (
	"reflect"
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) *Table {
	t.Helper()
	tb, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, src)
	}
	return tb
}

func get(t *testing.T, tb *Table, path string) any {
	t.Helper()
	cur := any(tb)
	for _, k := range strings.Split(path, ".") {
		tt, ok := cur.(*Table)
		if !ok {
			t.Fatalf("%s: %T is not a table", path, cur)
		}
		v, ok := tt.Get(k)
		if !ok {
			t.Fatalf("%s: missing key %q", path, k)
		}
		cur = v
	}
	return cur
}

func TestValues(t *testing.T) {
	src := `
# comment
name = "cro-mag" # trailing comment
count = 1_000
neg = -3
yes = true
no = false
lit = 'C:\Program Files\x'
esc = "tab\there \"q\" \u00e9 \\"
arr = [ "a", 'b',
  # comment inside
  "c", ]
empty = []
nested = [[1, 2], ["x"]]
env = { A = "1", "B C" = 'two', d.e = "dotted" }
ml = """
line one
line two \
    continued"""
mll = '''
raw \n stays
'''
four = """a""""
`
	tb := mustParse(t, src)
	cases := map[string]any{
		"name":    "cro-mag",
		"count":   int64(1000),
		"neg":     int64(-3),
		"yes":     true,
		"no":      false,
		"lit":     `C:\Program Files\x`,
		"esc":     "tab\there \"q\" é \\",
		"ml":      "line one\nline two continued",
		"mll":     "raw \\n stays\n",
		"four":    `a"`,
		"env.A":   "1",
		"env.d.e": "dotted",
	}
	for path, want := range cases {
		if got := get(t, tb, path); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", path, got, want)
		}
	}
	if got := get(t, tb, "env.B C"); got != "two" {
		t.Errorf("quoted inline key = %#v", got)
	}
	if got := get(t, tb, "arr"); !reflect.DeepEqual(got, []any{"a", "b", "c"}) {
		t.Errorf("arr = %#v", got)
	}
	if got := get(t, tb, "empty"); !reflect.DeepEqual(got, []any{}) {
		t.Errorf("empty = %#v", got)
	}
	if got := get(t, tb, "nested"); !reflect.DeepEqual(got, []any{[]any{int64(1), int64(2)}, []any{"x"}}) {
		t.Errorf("nested = %#v", got)
	}
}

func TestTablesAndOrder(t *testing.T) {
	src := `
top = 1
[step.build]
run = "make"
[step.test]
run = "make test"
[step.build.os.windows]
run = "nmake"
[step."quoted name"]
run = "x"
[a.b]
c = 1
[a]
d = 2
`
	tb := mustParse(t, src)
	steps := get(t, tb, "step").(*Table)
	if got := steps.Keys(); !reflect.DeepEqual(got, []string{"build", "test", "quoted name"}) {
		t.Errorf("step order = %v", got)
	}
	if got := get(t, tb, "step.build.os.windows.run"); got != "nmake" {
		t.Errorf("override = %#v", got)
	}
	if got := get(t, tb, "a.d"); got != int64(2) {
		t.Errorf("implicit table later defined: %#v", got)
	}
	if got := tb.Keys(); !reflect.DeepEqual(got, []string{"top", "step", "a"}) {
		t.Errorf("root order = %v", got)
	}
}

func TestCRLFAndBOM(t *testing.T) {
	tb := mustParse(t, "\uFEFFa = \"x\"\r\n[t]\r\nb = '''\r\nl1\r\nl2'''\r\n")
	if get(t, tb, "a") != "x" || get(t, tb, "t.b") != "l1\nl2" {
		t.Fatalf("CRLF handling wrong: %#v", get(t, tb, "t.b"))
	}
}

func TestErrors(t *testing.T) {
	cases := map[string]string{
		"dup key":           "a = 1\na = 2",
		"dup table":         "[a]\n[a]",
		"array of tables":   "[[a]]",
		"float":             "a = 1.5",
		"date":              "a = 2026-09-30",
		"bad escape":        `a = "C:\path"`,
		"unterminated":      `a = "abc`,
		"no value":          "a =",
		"missing equals":    "a 1",
		"trailing garbage":  `a = "x" y`,
		"inline immutable":  "a = { b = 1 }\n[a]",
		"extend inline":     "a = { b = 1 }\na.c = 2",
		"dotted vs header":  "[a.b]\nx = 1\n[a]\nb.y = 2",
		"value then table":  "a = 1\n[a]",
		"newline in string": "a = \"x\ny\"",
		"bare junk":         "a = hello",
	}
	for name, src := range cases {
		if _, err := Parse(src); err == nil {
			t.Errorf("%s: expected an error for %q", name, src)
		} else if !strings.Contains(err.Error(), "line ") {
			t.Errorf("%s: error lacks a position: %v", name, err)
		}
	}
}

func TestErrorPosition(t *testing.T) {
	_, err := Parse("a = 1\nb = 2\nc = 3.0\n")
	e, ok := err.(*Error)
	if !ok || e.Line != 3 {
		t.Fatalf("want error on line 3, got %v", err)
	}
}

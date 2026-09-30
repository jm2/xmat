// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"
)

// Build logs are long; the controller only ever shows a one-line summary and
// a short tail, and agents ask for more with `xmat log`. The summary is the
// first line that looks like a real diagnostic, preferring compiler and
// linker errors over build-system noise such as ninja's "FAILED:" banner.

var errTiers = [][]*regexp.Regexp{
	{ // compilers, linkers, sanitizers
		regexp.MustCompile(`\berror [A-Z]{1,4}\d{3,5}\s*:`),         // MSVC: error C2065:, error LNK2019:
		regexp.MustCompile(`:\d+(:\d+)?:\s*(fatal )?error\b`),       // gcc/clang file:line:col: error
		regexp.MustCompile(`\(\d+(,\d+)?\)\s*:\s*(fatal )?error\b`), // MSVC file(line): error
		regexp.MustCompile(`^\s*(fatal )?error(\[E\d+\])?:`),        // rustc, clang driver, generic tools
		regexp.MustCompile(`undefined reference to`),                // GNU ld
		regexp.MustCompile(`Undefined symbols for architecture`),    // Apple ld
		regexp.MustCompile(`\bld(\.lld|64\.lld)?: error\b`),         // lld
		regexp.MustCompile(`unresolved external symbol`),            // MSVC link
		regexp.MustCompile(`ERROR: (Address|Leak|Thread|Undefined)Sanitizer`),
		regexp.MustCompile(`^panic: `),           // Go
		regexp.MustCompile(`^\S+\.go:\d+:\d+: `), // Go compiler
	},
	{ // build systems and test runners
		regexp.MustCompile(`^CMake Error`),
		regexp.MustCompile(`^FAILED: `), // ninja
		regexp.MustCompile(`\bmake(\[\d+\])?: \*\*\*`),
		regexp.MustCompile(`\d+% tests passed, \d+ tests failed`), // ctest
		regexp.MustCompile(`^\s*\d+/\d+ Test\s+#\d+: .*\*\*\*`),   // ctest per-test failure
		regexp.MustCompile(`^--- FAIL: `),                         // go test
		regexp.MustCompile(`^Traceback \(most recent call last\)`),
		regexp.MustCompile(`\bMSB\d{4}\b`),            // msbuild
		regexp.MustCompile(`^\*\* BUILD FAILED \*\*`), // xcodebuild
	},
	{ // anything that smells like failure
		regexp.MustCompile(`(?i)\bsegmentation fault\b|\bcore dumped\b`),
		regexp.MustCompile(`(?i)assertion .* failed`),
		regexp.MustCompile(`\bFAIL(ED|URE)?\b`),
		regexp.MustCompile(`(?i)\berror\b`),
	},
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func cleanLine(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.TrimRight(s, "\r\n \t")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

// firstError returns the most specific diagnostic line in r.
func firstError(r io.Reader) string {
	best := make([]string, len(errTiers))
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := cleanLine(sc.Text())
		if line == "" {
			continue
		}
		for i, tier := range errTiers {
			if best[i] != "" {
				continue
			}
			for _, re := range tier {
				if re.MatchString(line) {
					best[i] = strings.TrimSpace(line)
					break
				}
			}
		}
		if best[0] != "" {
			return best[0]
		}
	}
	for _, b := range best {
		if b != "" {
			return b
		}
	}
	return ""
}

// tailLines returns up to n non-empty lines from the end of the file at path.
func tailLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const chunk = 256 * 1024
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := st.Size() - chunk
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil
	}
	return lastLines(buf, n, off > 0)
}

func lastLines(buf []byte, n int, dropFirst bool) []string {
	lines := bytes.Split(buf, []byte("\n"))
	if dropFirst && len(lines) > 0 {
		lines = lines[1:] // likely a partial line
	}
	var out []string
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		l := cleanLine(string(lines[i]))
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func firstErrorInFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	return firstError(f)
}

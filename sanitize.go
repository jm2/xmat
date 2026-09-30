// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"io"
	"os"
)

// Everything the controller prints can contain text from build hosts or from
// the commit under test: compiler errors, log tails, git stderr, config
// errors. A malicious commit could embed terminal control sequences there to
// rewrite the matrix (cursor movement), set the window title, or write the
// clipboard (OSC 52). termFilter strips them before they reach a terminal or
// an agent.
//
// It keeps printable text, '\n' and '\t', and drops other C0 controls
// (including '\r' and BEL), DEL, C1 controls (U+0080-U+009F) and every
// escape sequence: CSI, OSC/DCS/SOS/PM/APC strings, and two-byte escapes. It
// is stateful, so a sequence split across writes is still caught.
type termFilter struct {
	w      io.Writer
	state  int
	strLen int
	lead   bool // saw 0xC2, waiting to see whether a C1 control follows
	out    []byte
}

const (
	tfNormal   = iota
	tfEsc      // after ESC
	tfEscInter // ESC followed by intermediate bytes
	tfCSI      // control sequence
	tfString   // OSC/DCS/SOS/PM/APC payload
	tfStrEsc   // ESC inside a string (possible ST)
)

const maxCtlString = 4096

func newTermFilter(w io.Writer) *termFilter { return &termFilter{w: w} }

func (f *termFilter) Write(p []byte) (int, error) {
	f.out = f.out[:0]
	for _, b := range p {
		f.byte(b)
	}
	if len(f.out) > 0 {
		if _, err := f.w.Write(f.out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// c1 handles a C1 control (second byte of 0xC2 0x80..0x9F).
func (f *termFilter) c1(b byte) {
	switch b {
	case 0x9B: // CSI
		f.state = tfCSI
	case 0x90, 0x98, 0x9D, 0x9E, 0x9F: // DCS, SOS, OSC, PM, APC
		f.state, f.strLen = tfString, 0
	case 0x9C: // ST
		f.state = tfNormal
	}
}

func (f *termFilter) byte(b byte) {
	if f.lead {
		f.lead = false
		if b >= 0x80 && b <= 0x9F {
			f.c1(b)
			return
		}
		if f.state == tfNormal {
			f.out = append(f.out, 0xC2)
		}
	}
	if b == 0xC2 {
		f.lead = true
		return
	}
	switch f.state {
	case tfNormal:
		switch {
		case b == 0x1B:
			f.state = tfEsc
		case b == '\n' || b == '\t':
			f.out = append(f.out, b)
		case b < 0x20 || b == 0x7F:
			// drop
		default:
			f.out = append(f.out, b)
		}
	case tfEsc:
		switch {
		case b == '[':
			f.state = tfCSI
		case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
			f.state, f.strLen = tfString, 0
		case b >= 0x20 && b <= 0x2F:
			f.state = tfEscInter
		default:
			f.state = tfNormal // two-byte escape: drop it
		}
	case tfEscInter:
		if b < 0x20 || b > 0x2F {
			f.state = tfNormal
		}
	case tfCSI:
		switch {
		case b >= 0x20 && b <= 0x3F:
		case b == 0x1B:
			f.state = tfEsc
		default:
			f.state = tfNormal // final byte (or garbage) ends it
		}
	case tfString:
		f.strLen++
		switch {
		case b == 0x07:
			f.state = tfNormal
		case b == 0x1B:
			f.state = tfStrEsc
		case f.strLen > maxCtlString:
			f.state = tfNormal // unterminated; stop swallowing output
		}
	case tfStrEsc:
		if b == '\\' {
			f.state = tfNormal
		} else {
			f.state = tfEsc
			f.byte(b)
		}
	}
}

// sanitize returns s with terminal control sequences removed.
func sanitize(s string) string {
	var sb stringsBuilder
	f := newTermFilter(&sb)
	_, _ = f.Write([]byte(s))
	if f.lead && f.state == tfNormal {
		sb.b = append(sb.b, 0xC2)
	}
	return string(sb.b)
}

type stringsBuilder struct{ b []byte }

func (s *stringsBuilder) Write(p []byte) (int, error) {
	s.b = append(s.b, p...)
	return len(p), nil
}

// Controller-side output goes through these; host-side protocol output never does.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

func filterOutput() {
	stdout = newTermFilter(os.Stdout)
	stderr = newTermFilter(os.Stderr)
}

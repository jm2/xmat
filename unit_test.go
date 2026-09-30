// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstError(t *testing.T) {
	cases := []struct{ log, want string }{
		{"[1/3] Building C object foo.o\nFAILED: foo.o\nsrc/foo.c:12:3: error: 'bar' undeclared\nninja: build stopped", "src/foo.c:12:3: error: 'bar' undeclared"},
		{"  foo.c\nC:\\src\\foo.c(123): error C2065: 'bar': undeclared identifier\n", `C:\src\foo.c(123): error C2065: 'bar': undeclared identifier`},
		{"main.obj : error LNK2019: unresolved external symbol _foo", "main.obj : error LNK2019: unresolved external symbol _foo"},
		{"ld: warning: x\nUndefined symbols for architecture arm64:\n  \"_foo\"", "Undefined symbols for architecture arm64:"},
		{"-- Configuring incomplete, errors occurred!\nCMake Error at CMakeLists.txt:3 (project):", "CMake Error at CMakeLists.txt:3 (project):"},
		{"compiling -Werror=format\nok\n", ""},
		{"\x1b[31merror\x1b[0m: could not compile `xmat`", "error: could not compile `xmat`"},
		{"50% tests passed, 1 tests failed out of 2", "50% tests passed, 1 tests failed out of 2"},
		{"all good\nSegmentation fault (core dumped)", "Segmentation fault (core dumped)"},
	}
	for _, c := range cases {
		if got := firstError(strings.NewReader(c.log)); got != c.want {
			t.Errorf("firstError(%q)\n got %q\nwant %q", c.log, got, c.want)
		}
	}
}

func TestLastLines(t *testing.T) {
	got := lastLines([]byte("partial\na\n\nb\nc\n"), 2, true)
	if strings.Join(got, "|") != "b|c" {
		t.Errorf("lastLines = %v", got)
	}
	got = lastLines([]byte("only\n"), 5, false)
	if strings.Join(got, "|") != "only" {
		t.Errorf("lastLines = %v", got)
	}
}

func TestLocalHubPath(t *testing.T) {
	cases := map[string]string{
		"/srv/hub/x.git":            "/srv/hub/x.git",
		"file:///srv/hub/x.git":     filepath.FromSlash("/srv/hub/x.git"),
		"ssh://jm@antec/srv/x.git":  "",
		"jm@antec:srv/x.git":        "",
		"antec:x.git":               "",
		"relative/dir.git":          "relative/dir.git",
		"https://example.com/x.git": "",
	}
	for in, want := range cases {
		if got := localHubPath(in); got != want {
			t.Errorf("localHubPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSSHHubTarget(t *testing.T) {
	cases := []struct{ in, dest, port, path string }{
		{"ssh://jm@antec/srv/xmat/cmr.git", "jm@antec", "", "/srv/xmat/cmr.git"},
		{"ssh://antec:2222/~/hub/cmr.git", "antec", "2222", "~/hub/cmr.git"},
		{"jm@antec:hub/cmr.git", "jm@antec", "", "hub/cmr.git"},
	}
	for _, c := range cases {
		d, p, path, err := sshHubTarget(c.in)
		if err != nil || d != c.dest || p != c.port || path != c.path {
			t.Errorf("sshHubTarget(%q) = %q %q %q %v", c.in, d, p, path, err)
		}
	}
}

func TestRunFilePathContainment(t *testing.T) {
	t.Setenv("XMAT_ROOT", t.TempDir())
	for _, p := range []string{"../x", "/etc/passwd", "a/../../x", ""} {
		if _, err := runFilePath(FileRequest{Repo: "r", RunID: "id", Target: "t", Path: p}); err == nil {
			t.Errorf("path %q should be rejected", p)
		}
	}
	if _, err := runFilePath(FileRequest{Repo: "../r", RunID: "id", Target: "t", Path: "x"}); err == nil {
		t.Error("repo name with .. should be rejected")
	}
}

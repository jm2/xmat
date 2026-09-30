// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// End-to-end tests run a real xmat binary against a real git repository and
// hub. This file holds the shared harness; see e2e_unix_test.go and
// e2e_windows_test.go for the tests themselves.

var xmatBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "xmat-e2e-bin")
	if err != nil {
		panic(err)
	}
	xmatBin = filepath.Join(dir, "xmat"+exeSuffix())
	out, err := exec.Command("go", "build", "-o", xmatBin, ".").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "building xmat: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	t     *testing.T
	dir   string
	repo  string
	bin   string
	roots map[string]string
	vars  []string
}

// newEnvWith creates a repository containing repoCfg and a fleet file produced
// by fleet (which may use e.dir, e.bin and e.roots).
func newEnvWith(t *testing.T, repoCfg string, targets []string, fleet func(e *env) string) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, dir: dir, repo: filepath.Join(dir, "demo"), bin: filepath.Join(dir, "bin"), roots: map[string]string{}}
	must(t, os.MkdirAll(e.bin, 0o755))
	must(t, copyExe(xmatBin, filepath.Join(e.bin, "xmat"+exeSuffix())))
	for _, n := range targets {
		e.roots[n] = filepath.Join(dir, "root-"+n)
	}
	must(t, os.WriteFile(filepath.Join(dir, "fleet.toml"), []byte(fleet(e)), 0o644))
	e.vars = append(os.Environ(),
		"HOME="+dir,
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
		"XMAT_FLEET="+filepath.Join(dir, "fleet.toml"),
		"PATH="+e.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	must(t, os.MkdirAll(e.repo, 0o755))
	e.git("init", "-q", "-b", "main")
	e.git("config", "core.autocrlf", "false")
	e.write(".xmat.toml", repoCfg)
	e.write(".gitignore", "out/\n")
	e.git("add", "-A")
	e.git("commit", "-q", "-m", "init")
	return e
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func copyExe(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) write(name, body string) {
	e.t.Helper()
	must(e.t, os.WriteFile(filepath.Join(e.repo, name), []byte(body), 0o644))
}

func (e *env) git(args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = e.repo, e.vars
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// xmat runs the binary and returns stdout, stderr and the exit code.
func (e *env) xmat(extraEnv []string, args ...string) (string, string, int) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, xmatBin, args...)
	cmd.Dir, cmd.Env = e.repo, append(append([]string{}, e.vars...), extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			e.t.Fatalf("xmat %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errb.String(), code
}

func expectContains(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%s does not contain %q:\n%s", what, sub, s)
		}
	}
}

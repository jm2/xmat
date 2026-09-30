// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Remote hosts are simulated with a fake ssh that runs the command locally.

const fakeSSH = `#!/bin/sh
# fake ssh: <host> <command>; runs the command locally, like a remote login shell would.
host="$1"; shift
exec sh -c "$1"
`

func newEnv(t *testing.T, repoCfg string) *env {
	t.Helper()
	return newEnvWith(t, repoCfg, []string{"linux-a", "linux-b"}, func(e *env) string {
		must(t, os.WriteFile(filepath.Join(e.bin, "fakessh"), []byte(fakeSSH), 0o755))
		return fmt.Sprintf(`
[hub]
push = %q

[ssh]
command = [%q]

[target.linux-a]
local = true
root = %q

[target.linux-b]
host = "fakehost"
root = %q

[target.linux-c]
local = true
root = %q # shares linux-a's workspace: same mirror, different worktree

[target.broken]
host = "nohost"
xmat = "/nonexistent/xmat"
`, filepath.Join(e.dir, "hub", "{repo}.git"), filepath.Join(e.bin, "fakessh"), e.roots["linux-a"], e.roots["linux-b"], e.roots["linux-a"])
	})
}

const demoCfg = `
name = "demo"
targets = ["linux-a", "linux-b"]

[step.build]
run = '''
echo "building $XMAT_REPO@$XMAT_SHA on $XMAT_TARGET ($XMAT_OS/$XMAT_ARCH) greeting=$GREETING"
mkdir -p out
echo "artifact from $XMAT_TARGET" > out/a.txt
if [ "$XMAT_TARGET" = "linux-b" ] && [ -f fail-on-b ]; then
  echo "[2/3] Building C object foo.o"
  echo "FAILED: foo.o"
  echo "src/foo.c:12:3: error: 'bar' undeclared (first use in this function)" >&2
  exit 2
fi
'''
env = { GREETING = "hi" }
artifacts = ["out/a.txt"]

[step.test]
run = "test -f out/a.txt && echo tests pass"

[step.smoke]
default = false
desktop = true
timeout = "30s"
run = 'echo "smoke on $XMAT_TARGET desk=$DESKTOP_MARK"'
`

func TestRunMatrixLogGet(t *testing.T) {
	e := newEnv(t, demoCfg)

	out, errOut, code := e.xmat(nil, "run")
	if code != exitOK {
		t.Fatalf("clean run exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	expectContains(t, "matrix", out, "TARGET", "BUILD", "TEST", "linux-a", "linux-b", "local", "fakehost", "2 of 2 targets passed")
	if strings.Contains(out, "SMOKE") {
		t.Errorf("non-default step shown without --with:\n%s", out)
	}
	expectContains(t, "progress", errOut, "[linux-b] build ok", "[linux-a] test ok")

	// The hub got the commit under refs/xmat/.
	head := e.git("rev-parse", "HEAD")
	hub := filepath.Join(e.dir, "hub", "demo.git")
	if out, err := exec.Command("git", "--git-dir", hub, "cat-file", "-t", head).Output(); err != nil || strings.TrimSpace(string(out)) != "commit" {
		t.Fatalf("hub lacks %s: %v", head, err)
	}

	// An untracked trigger file is ignored without --dirty...
	e.write("fail-on-b", "x")
	out, errOut, code = e.xmat(nil, "run")
	if code != exitOK {
		t.Fatalf("HEAD run should still pass, exit %d\n%s\n%s", code, out, errOut)
	}
	expectContains(t, "stderr", errOut, "uncommitted changes are NOT included")

	// ...and picked up with --dirty, which snapshots without touching HEAD or the index.
	out, errOut, code = e.xmat(nil, "run", "--dirty")
	if code != exitFailed {
		t.Fatalf("dirty run exit %d, want %d\n%s\n%s", code, exitFailed, out, errOut)
	}
	expectContains(t, "matrix", out, "FAIL", "working tree snapshot",
		"linux-b build (fail, exit 2): src/foo.c:12:3: error: 'bar' undeclared",
		"more: xmat log linux-b build", "1 of 2 targets passed")
	if e.git("rev-parse", "HEAD") != head {
		t.Error("--dirty moved HEAD")
	}
	if st := e.git("status", "--porcelain"); !strings.Contains(st, "?? fail-on-b") {
		t.Errorf("--dirty touched the index: %q", st)
	}

	// Logs: default picks the failing step.
	out, _, code = e.xmat(nil, "log", "linux-b")
	if code != exitOK {
		t.Fatalf("log exit %d", code)
	}
	expectContains(t, "log", out, "undeclared", "greeting=hi")
	out, _, _ = e.xmat(nil, "log", "linux-b", "build", "--grep", "FAILED")
	if strings.TrimSpace(out) != "FAILED: foo.o" {
		t.Errorf("grep log = %q", out)
	}
	out, _, _ = e.xmat(nil, "log", "-n", "1", "linux-a", "test")
	if strings.TrimSpace(out) != "tests pass" {
		t.Errorf("tail log = %q", out)
	}

	// Artifacts.
	dest := filepath.Join(e.dir, "got.txt")
	_, errOut, code = e.xmat(nil, "get", "-o", dest, "linux-a", "artifacts/build/out/a.txt")
	if code != exitOK {
		t.Fatalf("get exit %d: %s", code, errOut)
	}
	if data, _ := os.ReadFile(dest); strings.TrimSpace(string(data)) != "artifact from linux-a" {
		t.Errorf("artifact = %q", data)
	}
	if _, _, code := e.xmat(nil, "get", "linux-a", "../../../etc/passwd"); code == exitOK {
		t.Error("path traversal was allowed")
	}

	// JSON output.
	out, _, code = e.xmat(nil, "run", "--json", "-q", "--targets", "linux-a")
	if code != exitOK {
		t.Fatalf("json run exit %d", code)
	}
	var lr lastRun
	if err := json.Unmarshal([]byte(out), &lr); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if r := lr.Results["linux-a"]; r == nil || r.Status != StOK || len(r.Steps) != 2 || r.Steps[0].Artifacts[0] != "artifacts/build/out/a.txt" {
		t.Errorf("json result = %+v", lr.Results["linux-a"])
	}

	// Explicit commit and step selection.
	out, _, code = e.xmat(nil, "run", "HEAD", "--steps", "build", "-t", "linux-a")
	if code != exitOK || strings.Contains(out, "TEST") {
		t.Errorf("--steps build: exit %d\n%s", code, out)
	}
	if _, errOut, code := e.xmat(nil, "run", "--steps", "nope"); code != exitUsage || !strings.Contains(errOut, `no step "nope"`) {
		t.Errorf("unknown step: exit %d %s", code, errOut)
	}

	// Two targets sharing one host workspace run concurrently on one mirror.
	for i := 0; i < 3; i++ {
		e.write("bump.txt", fmt.Sprint(i))
		out, errOut, code := e.xmat(nil, "run", "--dirty", "-t", "linux-a,linux-c")
		if code != exitOK {
			t.Fatalf("shared-mirror run %d: exit %d\n%s\n%s", i, code, out, errOut)
		}
	}
}

func TestInfraErrorsAndDoctor(t *testing.T) {
	e := newEnv(t, demoCfg)
	out, errOut, code := e.xmat(nil, "run", "--targets", "linux-a,broken")
	if code != exitInfra {
		t.Fatalf("exit %d, want %d\n%s\n%s", code, exitInfra, out, errOut)
	}
	expectContains(t, "matrix", out, "ERROR", "broken:", "1 of 2 targets passed")

	out, _, code = e.xmat(nil, "doctor")
	if code != exitInfra {
		t.Errorf("doctor exit %d", code)
	}
	expectContains(t, "doctor", out, "linux-a", "linux-b", "broken", "UNREACHABLE", "not running")
	if !strings.Contains(out, "linux-b  ") {
		t.Errorf("doctor table missing linux-b row:\n%s", out)
	}
}

func TestDesktopAgent(t *testing.T) {
	e := newEnv(t, demoCfg)

	// Without an agent the desktop step fails with a clear message.
	out, _, code := e.xmat(nil, "run", "--with", "smoke", "-t", "linux-a")
	if code != exitFailed {
		t.Fatalf("exit %d without agent\n%s", code, out)
	}
	expectContains(t, "matrix", out, "SMOKE", "xmat desktop")

	// With an agent running (in its own environment), the step runs there.
	agent := exec.Command(xmatBin, "desktop", "--root", e.roots["linux-a"])
	agent.Env = append(append([]string{}, e.vars...), "DESKTOP_MARK=from-agent")
	var agentErr bytes.Buffer
	agent.Stderr = &agentErr
	must(t, agent.Start())
	defer func() {
		agent.Process.Signal(os.Interrupt)
		agent.Wait()
	}()
	deadline := time.Now().Add(10 * time.Second)
	for desktopStatus(e.roots["linux-a"]) != "running" {
		if time.Now().After(deadline) {
			t.Fatalf("agent did not start: %s", agentErr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	// A second agent on the same root refuses to start.
	if out, err := exec.Command(xmatBin, "desktop", "--root", e.roots["linux-a"]).CombinedOutput(); err == nil {
		t.Errorf("second agent started: %s", out)
	}

	out, errOut, code := e.xmat(nil, "run", "--with", "smoke", "-t", "linux-a")
	if code != exitOK {
		t.Fatalf("exit %d with agent\n%s\n%s\nagent: %s", code, out, errOut, agentErr.String())
	}
	expectContains(t, "progress", errOut, "smoke ... (desktop session)")
	log, _, _ := e.xmat(nil, "log", "linux-a", "smoke")
	expectContains(t, "smoke log", log, "smoke on linux-a desk=from-agent")
}

func TestTimeoutKillsProcessTree(t *testing.T) {
	e := newEnv(t, `
[step.hang]
timeout = "2s"
run = '''
sh -c 'sleep 60' &
echo started
sleep 60
'''
`)
	start := time.Now()
	out, errOut, code := e.xmat(nil, "run", "-t", "linux-b")
	if code != exitFailed {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("timeout took %s", d)
	}
	expectContains(t, "matrix", out, "TIMEOUT", "timed out after 2s")
}

func TestCancelOnInterrupt(t *testing.T) {
	e := newEnv(t, `
[step.hang]
run = '''
echo $$ > "$XMAT_RUN_DIR/pid"
sleep 60 &
sleep 60
'''
`)
	for _, target := range []string{"linux-a", "linux-b"} { // local and "ssh"
		cmd := exec.Command(xmatBin, "run", "-t", target)
		cmd.Dir, cmd.Env = e.repo, e.vars
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		must(t, cmd.Start())

		var pidFile string
		deadline := time.Now().Add(20 * time.Second)
		for pidFile == "" {
			m, _ := filepath.Glob(filepath.Join(e.roots[target], "repos", "demo", "runs", "*", "pid"))
			if len(m) > 0 {
				if data, _ := os.ReadFile(m[0]); len(bytes.TrimSpace(data)) > 0 {
					pidFile = m[0]
				}
			}
			if time.Now().After(deadline) {
				cmd.Process.Kill()
				t.Fatalf("%s: step never started\n%s", target, errb.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
		data, _ := os.ReadFile(pidFile)
		var pgid int
		fmt.Sscan(string(data), &pgid)

		start := time.Now()
		must(t, cmd.Process.Signal(os.Interrupt))
		err := cmd.Wait()
		if d := time.Since(start); d > 20*time.Second {
			t.Errorf("%s: interrupt took %s", target, d)
		}
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 130 {
			t.Errorf("%s: exit %v, want 130\n%s\n%s", target, err, out.String(), errb.String())
		}
		expectContains(t, "matrix", out.String(), "CANCEL")
		if runtime.GOOS == "linux" {
			gone := false
			for i := 0; i < 50 && !gone; i++ {
				gone = !groupAlive(pgid)
				time.Sleep(100 * time.Millisecond)
			}
			if !gone {
				t.Errorf("%s: step process group %d survived the interrupt", target, pgid)
				syscallKill(-pgid, 9)
			}
		}
		os.Remove(pidFile)
	}
}

func syscallKill(pid int, sig int) error { return syscall.Kill(pid, syscall.Signal(sig)) }

// groupAlive reports whether any non-zombie process is in process group pgid
// (containers without a reaping init leave zombies behind for a while).
func groupAlive(pgid int) bool {
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, p := range stats {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := string(data)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:]) // state ppid pgrp ...
		if len(f) < 3 || f[0] == "Z" {
			continue
		}
		if f[2] == fmt.Sprint(pgid) {
			return true
		}
	}
	return false
}

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

//go:build windows

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Windows end-to-end tests use local targets only; they exercise cmd.exe and
// PowerShell scripts, LockFileEx, taskkill-based timeouts and the desktop
// spool on a real Windows machine (CI runs them on windows-latest).

const winCfg = `
name = "demo"
targets = ["win-a"]

[step.build]
run = '''
echo building %XMAT_TARGET% on %XMAT_OS%/%XMAT_ARCH% greeting=%GREETING%
if not exist out mkdir out
echo artifact from %XMAT_TARGET%> out\a.txt
if not exist fail-me goto done
echo [2/3] Building C object foo.obj
echo C:\src\foo.c(12): error C2065: 'bar': undeclared identifier
exit /b 2
:done
'''
env = { GREETING = "hi" }
artifacts = ["out/a.txt"]

[step.test]
shell = "powershell"
run = '''
if (-not (Test-Path out\a.txt)) { throw "missing artifact" }
Write-Output "ps tests pass"
'''

[step.native]
default = false
shell = "pwsh"
run = '''
cmd /c exit 7
Write-Output "should not get here"
'''

[step.hang]
default = false
timeout = "3s"
run = "ping -n 60 127.0.0.1 >nul"

[step.smoke]
default = false
desktop = true
run = "echo smoke desk=%DESKTOP_MARK%"
`

func newWinEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, winCfg, []string{"win-a"}, func(e *env) string {
		return fmt.Sprintf(`
[hub]
push = %q

[target.win-a]
local = true
root = %q
os = "windows"
`, filepath.Join(e.dir, "hub", "{repo}.git"), e.roots["win-a"])
	})
}

func TestWindowsRun(t *testing.T) {
	e := newWinEnv(t)

	out, errOut, code := e.xmat(nil, "run")
	if code != exitOK {
		t.Fatalf("clean run exit %d\n%s\n%s", code, out, errOut)
	}
	expectContains(t, "matrix", out, "BUILD", "TEST", "win-a", "1 of 1 targets passed")

	e.write("fail-me", "x")
	out, errOut, code = e.xmat(nil, "run", "--dirty")
	if code != exitFailed {
		t.Fatalf("dirty run exit %d\n%s\n%s", code, out, errOut)
	}
	expectContains(t, "matrix", out, `error C2065: 'bar': undeclared identifier`, "exit 2")

	log, _, _ := e.xmat(nil, "log", "win-a", "build")
	expectContains(t, "log", log, "greeting=hi", "error C2065")

	os.Remove(filepath.Join(e.repo, "fail-me"))
	if _, errOut, code := e.xmat(nil, "run"); code != exitOK {
		t.Fatalf("rerun exit %d: %s", code, errOut)
	}
	dest := filepath.Join(e.dir, "got.txt")
	if _, errOut, code := e.xmat(nil, "get", "-o", dest, "win-a", "artifacts/build/out/a.txt"); code != exitOK {
		t.Fatalf("get exit %d: %s", code, errOut)
	}
	if data, _ := os.ReadFile(dest); !strings.Contains(string(data), "artifact from win-a") {
		t.Errorf("artifact = %q", data)
	}

	// A failing native command inside a PowerShell step fails the step.
	out, _, code = e.xmat(nil, "run", "--steps", "native")
	if code != exitFailed || strings.Contains(out, "should not get here") {
		t.Errorf("native failure: exit %d\n%s", code, out)
	}
}

func TestWindowsTimeout(t *testing.T) {
	e := newWinEnv(t)
	start := time.Now()
	out, errOut, code := e.xmat(nil, "run", "--steps", "hang")
	if code != exitFailed {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if d := time.Since(start); d > 40*time.Second {
		t.Errorf("timeout took %s", d)
	}
	expectContains(t, "matrix", out, "TIMEOUT")
}

func TestWindowsDesktopAgent(t *testing.T) {
	e := newWinEnv(t)
	agent := exec.Command(xmatBin, "desktop", "--root", e.roots["win-a"])
	agent.Env = append(append([]string{}, e.vars...), "DESKTOP_MARK=from-agent")
	var agentErr bytes.Buffer
	agent.Stderr = &agentErr
	must(t, agent.Start())
	defer func() {
		agent.Process.Kill()
		agent.Wait()
	}()
	deadline := time.Now().Add(15 * time.Second)
	for desktopStatus(e.roots["win-a"]) != "running" {
		if time.Now().After(deadline) {
			t.Fatalf("agent did not start: %s", agentErr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if out, err := exec.Command(xmatBin, "desktop", "--root", e.roots["win-a"]).CombinedOutput(); err == nil {
		t.Errorf("second agent started (LockFileEx not exclusive?): %s", out)
	}
	out, errOut, code := e.xmat(nil, "run", "--with", "smoke")
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s\nagent: %s", code, out, errOut, agentErr.String())
	}
	log, _, _ := e.xmat(nil, "log", "win-a", "smoke")
	expectContains(t, "smoke log", log, "smoke desk=from-agent")
}

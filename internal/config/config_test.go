// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const fleetSrc = `
[hub]
push = "ssh://jm@antec/srv/xmat/{repo}.git"

[ssh]
command = ["ssh", "-T"]

[target.linux-x64]
host = "antec"
os = "linux"

[target.win-x64]
host = "antec-win"
os = "Windows"
xmat = 'C:\tools\xmat.exe'
root = 'D:\xmat'
hub_fetch = "ssh://jm@antec.lan/srv/xmat/{repo}.git"

[target.local]
local = true
`

func TestFleet(t *testing.T) {
	f, err := ParseFleet(fleetSrc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Order, []string{"linux-x64", "win-x64", "local"}) {
		t.Errorf("order = %v", f.Order)
	}
	w := f.Targets["win-x64"]
	if w.OS != "windows" || w.Xmat != `C:\tools\xmat.exe` || w.Root != `D:\xmat` {
		t.Errorf("win target = %+v", w)
	}
	if got := f.HubFetchFor(w, "cmr"); got != "ssh://jm@antec.lan/srv/xmat/cmr.git" {
		t.Errorf("hub fetch override = %s", got)
	}
	if got := f.HubFetchFor(f.Targets["linux-x64"], "cmr"); got != "ssh://jm@antec/srv/xmat/cmr.git" {
		t.Errorf("hub fetch default = %s", got)
	}
	if !f.Targets["local"].Local || f.Targets["local"].Xmat != "xmat" {
		t.Errorf("local target = %+v", f.Targets["local"])
	}
	if !reflect.DeepEqual(f.SSH, []string{"ssh", "-T"}) {
		t.Errorf("ssh = %v", f.SSH)
	}
}

func TestFleetErrors(t *testing.T) {
	cases := map[string]string{
		"no hub":      "[target.a]\nhost = \"x\"",
		"no targets":  "[hub]\npush = \"/x\"",
		"no host":     "[hub]\npush = \"/x\"\n[target.a]\nos = \"linux\"",
		"unknown key": "[hub]\npush = \"/x\"\n[target.a]\nhost = \"x\"\nhots = \"y\"",
		"bad name":    "[hub]\npush = \"/x\"\n[target.\"a b\"]\nhost = \"x\"",
		"wrong type":  "[hub]\npush = 5\n[target.a]\nhost = \"x\"",
		"empty ssh":   "[hub]\npush = \"/x\"\n[ssh]\ncommand = []\n[target.a]\nhost = \"x\"",
	}
	for name, src := range cases {
		if _, err := ParseFleet(src); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	_, err := ParseFleet("[hub]\npush = \"/x\"\n[target.a]\nhost = \"x\"\nhots = \"y\"")
	if err == nil || !strings.Contains(err.Error(), "target.a.hots") {
		t.Errorf("unknown key error should name the key path, got %v", err)
	}
}

const repoSrc = `
name = "CroMagRally"
targets = ["linux-x64", "win-x64"]
keep_runs = 5
env = { COMMON = "1", OVERRIDE = "base" }

[os.windows]
env = { OVERRIDE = "win" }

[target.linux-arm64]
skip = true

[step.configure]
run = "cmake -S . -B build"

[step.build]
run = "cmake --build build"
timeout = "20m"
env = { STEP = "b" }

[step.build.os.windows]
run = '''
call vcvars64.bat
cmake --build build
'''

[step.build.target.win-arm64]
shell = "pwsh"

[step.sign.os.darwin]
run = "codesign ..."

[step.smoke]
run = "./smoke"
default = false
desktop = true
artifacts = ["smoke.png"]

[step.smoke.target.linux-x64]
skip = true
`

func TestRepoResolve(t *testing.T) {
	r, err := ParseRepo(repoSrc)
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "CroMagRally" || r.KeepRuns != 5 {
		t.Errorf("repo = %+v", r)
	}
	if !reflect.DeepEqual(r.StepNames(), []string{"configure", "build", "sign", "smoke"}) {
		t.Errorf("step order = %v", r.StepNames())
	}
	if !reflect.DeepEqual(r.DefaultSteps(), []string{"configure", "build", "sign"}) {
		t.Errorf("default steps = %v", r.DefaultSteps())
	}

	lin, err := r.Resolve("linux", "linux-x64")
	if err != nil {
		t.Fatal(err)
	}
	if lin.Env["OVERRIDE"] != "base" || lin.Env["COMMON"] != "1" {
		t.Errorf("linux env = %v", lin.Env)
	}
	byName := func(rs *Resolved) map[string]Step {
		m := map[string]Step{}
		for _, s := range rs.Steps {
			m[s.Name] = s
		}
		return m
	}
	ls := byName(lin)
	if ls["build"].Shell != "sh" || ls["build"].Timeout != 20*time.Minute || ls["build"].Env["STEP"] != "b" {
		t.Errorf("linux build = %+v", ls["build"])
	}
	if !ls["sign"].Skip || ls["sign"].SkipReason == "" {
		t.Errorf("darwin-only step should skip on linux: %+v", ls["sign"])
	}
	if !ls["smoke"].Skip || ls["smoke"].Default || !ls["smoke"].Desktop {
		t.Errorf("linux smoke = %+v", ls["smoke"])
	}
	if ls["configure"].Timeout != DefaultStepTimeout {
		t.Errorf("default timeout = %v", ls["configure"].Timeout)
	}

	win, err := r.Resolve("windows", "win-x64")
	if err != nil {
		t.Fatal(err)
	}
	ws := byName(win)
	if win.Env["OVERRIDE"] != "win" {
		t.Errorf("windows env = %v", win.Env)
	}
	if ws["build"].Shell != "cmd" || !strings.Contains(ws["build"].Run, "vcvars64") {
		t.Errorf("windows build = %+v", ws["build"])
	}
	if ws["smoke"].Skip || !reflect.DeepEqual(ws["smoke"].Artifacts, []string{"smoke.png"}) {
		t.Errorf("windows smoke = %+v", ws["smoke"])
	}

	arm, _ := r.Resolve("windows", "win-arm64")
	if byName(arm)["build"].Shell != "pwsh" || !strings.Contains(byName(arm)["build"].Run, "vcvars64") {
		t.Errorf("target override should layer over os override: %+v", byName(arm)["build"])
	}

	mac, _ := r.Resolve("macos", "mac-arm64")
	if byName(mac)["sign"].Skip || byName(mac)["sign"].Run != "codesign ..." {
		t.Errorf("darwin sign = %+v", byName(mac)["sign"])
	}

	skipped, _ := r.Resolve("linux", "linux-arm64")
	if !skipped.Skip {
		t.Error("target-level skip not applied")
	}
}

func TestRepoErrors(t *testing.T) {
	cases := map[string]string{
		"no steps":      `name = "x"`,
		"no run":        "[step.a]\ntimeout = \"1m\"",
		"bad shell":     "[step.a]\nrun = \"x\"\nshell = \"zsh\"",
		"bad timeout":   "[step.a]\nrun = \"x\"\ntimeout = \"soon\"",
		"abs artifact":  "[step.a]\nrun = \"x\"\nartifacts = [\"/etc/passwd\"]",
		"dotdot dir":    "[step.a]\nrun = \"x\"\ndir = \"../x\"",
		"unknown field": "[step.a]\nrun = \"x\"\nrn = \"y\"",
		"keep_runs 0":   "keep_runs = 0\n[step.a]\nrun = \"x\"",
		"env not table": "[step.a]\nrun = \"x\"\nenv = \"A=1\"",
	}
	for name, src := range cases {
		if _, err := ParseRepo(src); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	r, err := ParseRepo("[step.a]\nrun = \"x\"\nshell = \"cmd\"")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("linux", "l"); err == nil {
		t.Error("cmd shell on linux should be rejected")
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "win-x64", "Cro.Mag_Rally", "20260930T171501Z-1a2b3c4d"} {
		if !ValidName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-x", "a/b", `a\b`, "a b", "..", "a..b"} {
		if ValidName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

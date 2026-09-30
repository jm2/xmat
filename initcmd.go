// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jm2/xmat/internal/config"
)

const repoTemplate = `# xmat: build and test this commit on every platform at once.
# Docs: https://github.com/jm2/xmat
#
# Steps run in declaration order inside a persistent per-target checkout, so
# incremental builds stay incremental. Scripts run with sh -e on Linux/macOS
# and cmd.exe on Windows unless a step sets shell = "bash" | "pwsh" | ...
# Override any step field per OS ([step.X.os.windows]) or per target
# ([step.X.target.win-x64]); the most specific layer wins.

# name = "myproject"          # hub repository name (default: directory name)
# targets = ["linux-x64", "win-x64", "mac-arm64", "linux-arm64"]  # default: all fleet targets
# clean = false               # git clean -ffdx before building (slow; defeats incremental builds)
# env = { CMAKE_GENERATOR = "Ninja" }

[step.configure]
run = "cmake -S . -B build -G Ninja -DCMAKE_BUILD_TYPE=RelWithDebInfo"

[step.configure.os.windows]
# cmd.exe does not stop on the first failing line; chain with && or check errorlevel.
run = '''
call "%ProgramFiles%\Microsoft Visual Studio\2022\Community\VC\Auxiliary\Build\vcvars64.bat" >nul || exit /b 1
cmake -S . -B build -G Ninja -DCMAKE_BUILD_TYPE=RelWithDebInfo
'''

[step.build]
run = "cmake --build build --parallel"
timeout = "45m"

[step.build.os.windows]
run = '''
call "%ProgramFiles%\Microsoft Visual Studio\2022\Community\VC\Auxiliary\Build\vcvars64.bat" >nul || exit /b 1
cmake --build build --parallel
'''

[step.test]
run = "ctest --test-dir build --output-on-failure"

# A smoke test that opens a window must run in the logged-in desktop session:
# desktop = true hands it to ` + "`xmat desktop`" + ` on the host. Opt in with --with smoke.
[step.smoke]
default = false
desktop = true
timeout = "5m"
run = "./build/myproject --smoke --frames 300 --screenshot smoke.png"
artifacts = ["smoke.png"]

[step.smoke.os.windows]
run = 'build\myproject.exe --smoke --frames 300 --screenshot smoke.png'

# [target.linux-arm64]
# skip = true                 # skip a whole target for this repo
`

const fleetTemplate = `# xmat fleet: which build hosts exist and how to reach them.
# Docs: https://github.com/jm2/xmat

[hub]
# A bare repository every host can fetch from. The controller pushes the commit
# under test to refs/xmat/<sha> (never to a branch). {repo} is replaced with the
# repository name; xmat creates the repository on first use.
push  = "ssh://you@antec/home/you/xmat-hub/{repo}.git"
# fetch = "..."               # URL hosts use, if different from push

# [ssh]
# command = ["ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15"]

[target.linux-x64]
host = "antec"                # anything ssh accepts, including ~/.ssh/config aliases
# local = true                # run on this machine without ssh
os = "linux"                  # optional: verified against the host
arch = "amd64"

[target.win-x64]
host = "antec-win"
os = "windows"
arch = "amd64"
# xmat = 'C:\tools\xmat.exe'   # if xmat is not on PATH for ssh sessions
# root = 'D:\xmat'             # workspace root on the host (default: ~/xmat)

[target.mac-arm64]
host = "mbp16-m1max"
os = "darwin"
arch = "arm64"

[target.linux-arm64]
host = "macmini-m1"
os = "linux"
arch = "arm64"
`

func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fleet := fs.Bool("fleet", false, "write a fleet config instead of a repo config")
	force := fs.Bool("force", false, "overwrite an existing file")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: xmat init [--fleet] [--force]\n\nWrite a commented starter .xmat.toml at the repository root, or with\n--fleet a starter fleet config.")
		fs.PrintDefaults()
	}
	if _, err := parseInterspersed(fs, args); err != nil {
		return exitUsage
	}
	var path, body string
	if *fleet {
		path, body = config.DefaultFleetPath(), fleetTemplate
	} else {
		ctx, stop := signalContext()
		defer stop()
		repo, err := findRepo(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "xmat init:", err)
			return exitUsage
		}
		path, body = filepath.Join(repo.top, config.RepoFile), repoTemplate
	}
	if _, err := os.Stat(path); err == nil && !*force {
		fmt.Fprintf(os.Stderr, "xmat init: %s already exists (use --force to overwrite)\n", path)
		return exitUsage
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "xmat init:", err)
		return exitUsage
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "xmat init:", err)
		return exitUsage
	}
	fmt.Println("wrote", path)
	return exitOK
}

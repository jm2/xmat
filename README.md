# xmat

Build and test one commit on all your machines at once, and get back a
five-line answer.

```
$ xmat run --dirty
xmat CroMagRally d7a3b8e34a (working tree snapshot)  run 20260930T171658Z-d7a3b8e3

TARGET       HOST         CONFIGURE  BUILD  TEST  TIME
linux-x64    antec        ok         ok     ok    41s
win-x64      antec-win    ok         ok     ok    1m02s
mac-arm64    mbp16-m1max  ok         FAIL   -     22s
linux-arm64  macmini-m1   ok         ok     ok    1m15s

mac-arm64 build (fail, exit 1): Source/Main.c:412:5: error: use of undeclared identifier 'gGammaFadeFrac'
  more: xmat log mac-arm64 build

3 of 4 targets passed.
```

xmat is for multi-platform projects where the platform-specific parts cannot
be cross-compiled or cross-tested, so every change has to be built on the real
machines. It replaces "push, ssh into three boxes, pull, build, squint at the
log" with one command that:

- pins the exact tree under test (a commit, or a snapshot of your uncommitted
  work), so every host builds byte-identical sources;
- runs each host's steps in a persistent per-target checkout, so builds stay
  incremental;
- keeps full logs on the hosts and prints only a matrix plus the first real
  compiler or linker error, so neither you nor a coding agent drowns in
  40,000 lines of MSVC output;
- exits 0/1/2/3 meaningfully, so scripts and agents can branch on it.

It is a single static Go binary with no dependencies beyond `git` and `ssh`.
There is no daemon, no server and no database; hosts are reached over the ssh
you already have.

## How it works

```
 controller (any host)               hub (bare git repo)            each target host
 ──────────────────────              ───────────────────            ────────────────────────────
 xmat run                 push ───▶  refs/xmat/<sha>     ◀─── fetch  xmat host (over ssh)
   │ snapshot/resolve sha                                              mirror.git → wt/<target>
   │ ssh <host> xmat host  ───────── job (JSON) ─────────────────────▶ run steps, write logs
   │                       ◀──────── events (JSON lines) ──────────── result
   ▼
 matrix on stdout, details on demand (xmat log / xmat get)
```

1. The controller resolves the commit (or, with `--dirty`, commits the working
   tree into a dangling snapshot without touching your index, HEAD or
   branches) and pushes it to the hub under `refs/xmat/<sha>`. Nothing is
   pushed to a branch, and WIP never reaches GitHub.
2. It starts `xmat host` on every target in parallel over ssh (or directly,
   for `local = true` targets) and sends it a one-line JSON job.
3. Each host fetches the commit into a local mirror, checks it out into that
   target's persistent worktree, reads `.xmat.toml` **from that commit**, and
   runs the steps. Output goes to per-step log files; the host streams back
   only step start/end events and a final result.
4. The controller prints the matrix and remembers the run, so
   `xmat log <target>` and `xmat get <target> <file>` can fetch details later.

Ctrl-C hangs up on every host; each host then kills the running step's whole
process tree and reports what it stopped.

## Install

Every machine needs `xmat` and `git` on the `PATH` that **non-interactive ssh
sessions** see (that is what `xmat run` gets). With Go installed:

```sh
go install github.com/jm2/xmat@latest
```

Or download a binary for your platform from the
[releases page](https://github.com/jm2/xmat/releases) and put it on `PATH`.
All hosts should run the same version; `xmat doctor` flags mismatches.

## Quick start

1. **Describe your machines** once, on the machine you run `xmat` from:

   ```sh
   xmat init --fleet        # writes ~/.config/xmat/fleet.toml (%AppData%\xmat on Windows)
   ```

   ```toml
   [hub]
   push = "ssh://you@antec/home/you/xmat-hub/{repo}.git"   # created on first use

   [target.linux-x64]
   host = "antec"          # anything ssh accepts, including ~/.ssh/config aliases
   os = "linux"            # optional; verified against the host

   [target.win-x64]
   host = "antec-win"
   os = "windows"

   [target.mac-arm64]
   host = "mbp16-m1max"
   os = "darwin"

   [target.linux-arm64]
   host = "macmini-m1"
   os = "linux"
   arch = "arm64"
   ```

   Every host must be able to `git fetch` from the hub URL without a password
   prompt (ssh keys), and the controller must be able to `ssh <host>` without
   one.

2. **Describe the build** in each repository, and commit it:

   ```sh
   xmat init                # writes a commented .xmat.toml at the repo root
   ```

   ```toml
   [step.configure]
   run = "cmake -S . -B build -G Ninja -DCMAKE_BUILD_TYPE=RelWithDebInfo"

   [step.build]
   run = "cmake --build build --parallel"

   [step.build.os.windows]
   run = '''
   call "%ProgramFiles%\Microsoft Visual Studio\2022\Community\VC\Auxiliary\Build\vcvars64.bat" >nul || exit /b 1
   cmake --build build --parallel
   '''

   [step.test]
   run = "ctest --test-dir build --output-on-failure"
   ```

3. **Check the fleet**, then run:

   ```sh
   xmat doctor              # ssh, xmat/git versions, hub access, desktop agents
   xmat run                 # HEAD on every target
   xmat run --dirty         # your uncommitted work, including untracked files
   xmat log win-x64         # last 100 lines of the first failing step
   ```

## Commands

| Command | What it does |
| --- | --- |
| `xmat run [<commit>]` | Build/test a commit (default `HEAD`) on the targets. `--dirty` snapshots the working tree instead. `-t a,b` picks targets, `--steps x,y` replaces the default steps, `--with smoke` adds non-default ones, `--json` prints everything machine-readably, `-q` drops progress lines. |
| `xmat log <target> [<step>]` | Print a step log from the last run (default: the first step that did not pass). `-n N` last N lines (default 100, `0` = all), `--grep text`. |
| `xmat get <target> <path>` | Download a file from that target's run directory, e.g. `artifacts/smoke/smoke.png` or `result.json`. `-o dest`, `-o -` for stdout. |
| `xmat doctor` | Check every target: reachable, xmat and git versions, OS/arch, hub fetch access, desktop agent, available shells. |
| `xmat init [--fleet]` | Write a commented starter `.xmat.toml` (or fleet file). |
| `xmat desktop` | Run steps marked `desktop = true` inside this graphical session (see below). `--print-autostart` shows how to start it at login on this OS. |

`xmat host`, `xmat host-file` and `xmat host-info` are the host side of the
protocol; the commands above start them over ssh.

### Exit status of `xmat run`

| Code | Meaning |
| --- | --- |
| 0 | every target passed (or was skipped by config) |
| 1 | a step failed, timed out or was cancelled on at least one target |
| 2 | usage or config error; nothing ran |
| 3 | nothing failed, but at least one target could not run (unreachable, no xmat, hub fetch failed, wrong OS) |
| 130 | interrupted |

## Configuration

Both files use TOML. xmat reads a strict subset (no arrays of tables, floats
or dates) and rejects unknown keys, so typos fail loudly with a line number.
Use `'single-quoted'` or `'''multi-line'''` literal strings for Windows paths.

### Fleet file

Looked up in `--fleet`, `$XMAT_FLEET`, `$XDG_CONFIG_HOME/xmat/fleet.toml`,
`~/.config/xmat/fleet.toml`, then the OS config directory.

| Key | Meaning |
| --- | --- |
| `hub.push` | Where the controller pushes; `{repo}` is replaced by the repo name. A local path, `file://`, `ssh://` or `host:path` URL. Local and ssh hubs are created on first use (ssh hubs must be POSIX hosts). |
| `hub.fetch` | URL hosts fetch from, if different from `push` (e.g. push to a local path on the hub machine itself). |
| `ssh.command` | ssh command prefix. Default `["ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15"]`. |
| `target.<name>.host` | ssh destination. |
| `target.<name>.local` | `true` to run on this machine without ssh. |
| `target.<name>.xmat` | xmat command on the host if it is not on `PATH` (e.g. `'C:\tools\xmat.exe'`). |
| `target.<name>.root` | Workspace root on the host. Default `~/xmat` (`%USERPROFILE%\xmat`), or `$XMAT_ROOT`. |
| `target.<name>.hub_fetch` | Per-target hub URL, e.g. when one host reaches the hub under a different name. |
| `target.<name>.os`, `.arch` | Expected `GOOS`/`GOARCH` (`linux`, `darwin`/`macos`, `windows`; `amd64`, `arm64`). A mismatch is reported instead of building. |

### Repo file: `.xmat.toml`

Read from the commit under test, so steps are versioned with the code.

| Key | Meaning |
| --- | --- |
| `name` | Repository name on the hub and hosts. Default: the directory name. |
| `targets` | Default targets for this repo. Default: every fleet target. |
| `keep_runs` | Run directories each host keeps per target (default 20). |
| `env` | Environment for every step, e.g. `env = { CMAKE_GENERATOR = "Ninja" }`. |
| `clean` | `git clean -ffdx` before every run (slow: defeats incremental builds). |
| `submodules` | Update submodules (default: when `.gitmodules` exists). Hosts fetch submodules from their own URLs. |
| `[os.<os>]`, `[target.<name>]` | Override `env`, `clean`, `submodules` for an OS or target; `skip = true` skips the target. |

Steps are `[step.<name>]` tables and run in the order they are declared. Each
target runs its steps in sequence and stops at the first failure.

| Step key | Meaning |
| --- | --- |
| `run` | The script. Required somewhere; a step with no `run` for a platform is skipped there. |
| `shell` | `sh` (default on Linux/macOS, run with `-e`), `bash` (`-e -o pipefail`), `cmd` (default on Windows), `pwsh`, `powershell`. |
| `dir` | Working directory, relative to the repo root. |
| `timeout` | e.g. `"30m"` (default `60m`). The whole process tree is killed on timeout. |
| `env` | Extra environment, merged over the repo `env`. |
| `default` | `false` to run only when asked with `--with <name>` or `--steps`. |
| `desktop` | Run inside the logged-in graphical session (see below). |
| `artifacts` | Globs, relative to the repo root, copied into the run directory after the step (pass or fail). |
| `skip` | `true` to skip. |

Override any step key for an OS or target: `[step.build.os.windows]`,
`[step.smoke.target.linux-arm64]`. Layers apply as base, then OS, then target.

Every step sees `XMAT=1`, `XMAT_REPO`, `XMAT_SHA`, `XMAT_TARGET`, `XMAT_OS`,
`XMAT_ARCH`, `XMAT_RUN_ID`, `XMAT_RUN_DIR` (write extra outputs here) and
`XMAT_WORKTREE`.

Scripts are written to files and run from there, so there is no quoting to
fight. Two shell semantics to keep in mind:

- **cmd** does not stop at the first failing line; its exit code is the last
  command's. Chain with `&&`, add `|| exit /b 1`, or check `errorlevel`.
  xmat prepends `@echo off`.
- **pwsh** 7.3+ stops on failing native commands (xmat sets
  `$PSNativeCommandUseErrorActionPreference`); **Windows PowerShell 5.1**
  does not, but the step still fails if the last native command did.

## Desktop-session steps (games, GUI smoke tests)

Processes started over ssh do not run in your logged-in desktop session. On
Windows they cannot open windows or swapchains on the interactive desktop; on
macOS they live outside the Aqua session. Mark such steps `desktop = true`:

```toml
[step.smoke]
default = false           # opt in: xmat run --with smoke
desktop = true
timeout = "5m"
run = "./build/game --smoke --frames 300 --screenshot smoke.png"
artifacts = ["smoke.png"]
```

and start `xmat desktop` at login on each host that runs them. It watches a
spool directory under the workspace root and runs handed-over steps with the
session's own environment. Plain files, no sockets, same behaviour on every OS.
`xmat desktop --print-autostart` prints the right recipe: a logon task
(`schtasks /SC ONLOGON`) on Windows, a LaunchAgent on macOS, a systemd user
unit on Linux. For machines that must survive a reboot unattended, enable
auto-logon so the session exists.

A smoke mode in the program itself (load a level, render N frames, write a
screenshot, exit non-zero on failure) turns "does it run on Windows?" into an
exit code and a PNG: `xmat get win-x64 artifacts/smoke/smoke.png`.

## Platform notes

**Windows hosts.** Enable the built-in *OpenSSH Server* feature. For an
account in the Administrators group, authorized keys go in
`C:\ProgramData\ssh\administrators_authorized_keys`, not `~\.ssh`. The default
shell (cmd) is fine for xmat; set `DefaultShell` if you prefer pwsh. Install
Git for Windows and put `xmat.exe` on your user `PATH`. xmat enables
`core.longpaths` in its mirrors. Your global `core.autocrlf` applies to
checkouts as it would for any clone.

**macOS hosts.** Enable *Remote Login*. Non-interactive ssh gets a minimal
`PATH` without Homebrew, so add `eval "$(/opt/homebrew/bin/brew shellenv)"`
to `~/.zshenv` (zsh reads it for `ssh host cmd`), or set `xmat =
"/opt/homebrew/bin/xmat"` and use absolute tool paths. Laptops drop out when
asleep; `caffeinate -s` or `pmset` keeps them reachable while docked.

**Linux hosts.** Tools your steps need must be on the `PATH` that `ssh host
cmd` sees; bash reads `~/.bashrc` for such sessions.

**The hub** is an ordinary bare repository. Refs accumulate under
`refs/xmat/`; hosts prune their own mirrors, and you can trim the hub from
cron:

```sh
git -C ~/xmat-hub/REPO.git for-each-ref --sort=-creatordate --format='%(refname)' refs/xmat/ \
  | tail -n +201 | xargs -r -n1 git -C ~/xmat-hub/REPO.git update-ref -d
```

## Using xmat from coding agents

xmat was written so agents can validate their own changes across platforms
without burning context. A snippet for `AGENTS.md` / `CLAUDE.md`:

```markdown
## Cross-platform validation
- Run `xmat run --dirty` to build and test your uncommitted changes on
  Linux x64/arm64, Windows x64 and macOS arm64 in parallel. Exit 0 means all
  passed; the output is a short matrix with the first error per failure.
- For a failure, run `xmat log <target> <step>` (last 100 lines) or
  `xmat log <target> <step> --grep <text>`. Do not dump whole logs.
- `xmat run --with smoke` adds the GUI smoke test; fetch its screenshot with
  `xmat get <target> artifacts/smoke/smoke.png`.
- Exit 3 means a host was unreachable, not that your change is broken; say so
  instead of "fixing" code.
```

## Host workspace layout

```
~/xmat/
  repos/<repo>/mirror.git              bare cache of the hub
  repos/<repo>/wt/<target>/            persistent checkout, reused for incremental builds
  repos/<repo>/runs/<run>-<target>/    <step>.log, scripts/, artifacts/, result.json
  repos/<repo>/lock-<target>           serializes runs of one repo+target
  desktop/                             spool shared with `xmat desktop`
```

Concurrent runs of the same repo and target on one host queue behind a lock
(the OS releases it if a run dies). Different targets on the same host run in
parallel.

## Non-goals

xmat is not a CI system: no web UI, no history beyond the last `keep_runs`
runs per host, no scheduling. It pairs well with one. Use xmat for the
edit-build-test loop, and let CI keep the record.

## License

Copyright (C) 2026 jm2 and xmat contributors.

xmat is free software: you can redistribute it and/or modify it under the
terms of the GNU General Public License as published by the Free Software
Foundation, either version 3 of the License, or (at your option) any later
version. See [LICENSE](LICENSE).

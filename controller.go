// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/jm2/xmat/internal/config"
)

// Exit codes of `xmat run`.
const (
	exitOK     = 0
	exitFailed = 1 // at least one step failed on at least one target
	exitUsage  = 2 // bad flags or config
	exitInfra  = 3 // no step failed, but a target could not run (unreachable, hub, lock...)
)

// transport starts xmat subcommands on a target.
type transport struct {
	fleet *config.Fleet
	t     *config.Target
}

func (tr transport) command(ctx context.Context, sub string) *exec.Cmd {
	if tr.t.Local {
		exe, err := os.Executable()
		if err != nil {
			exe = os.Args[0]
		}
		return exec.CommandContext(ctx, exe, sub)
	}
	ssh := tr.fleet.SSH
	args := append(append([]string{}, ssh[1:]...), tr.t.Host, tr.t.Xmat+" "+sub)
	return exec.CommandContext(ctx, ssh[0], args...)
}

func (tr transport) describe() string {
	if tr.t.Local {
		return "local"
	}
	return tr.t.Host
}

// call runs a request/response subcommand (host-file, host-info) and returns stdout.
func (tr transport) call(ctx context.Context, sub string, req any, stdout io.Writer) error {
	cmd := tr.command(ctx, sub)
	data, _ := json.Marshal(req)
	cmd.Stdin = bytes.NewReader(append(data, '\n'))
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s: %s", tr.describe(), lastLine(msg))
	}
	return nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// lastRun is what `xmat run` remembers for `xmat log` and `xmat get`.
type lastRun struct {
	Repo     string                   `json:"repo"`
	SHA      string                   `json:"sha"`
	Ref      string                   `json:"ref"`
	Snapshot bool                     `json:"snapshot"`
	RunID    string                   `json:"run_id"`
	Started  time.Time                `json:"started"`
	Steps    []string                 `json:"steps"`
	Targets  []string                 `json:"targets"`
	Results  map[string]*TargetResult `json:"results"`
}

func lastRunPath(r *repoInfo) string { return filepath.Join(r.common, "xmat", "last-run.json") }

func loadLastRun(r *repoInfo) (*lastRun, error) {
	data, err := os.ReadFile(lastRunPath(r))
	if err != nil {
		return nil, errors.New("no previous `xmat run` recorded in this repository")
	}
	var lr lastRun
	if err := json.Unmarshal(data, &lr); err != nil {
		return nil, err
	}
	return &lr, nil
}

type progress struct {
	mu    sync.Mutex
	quiet bool
}

func (p *progress) printf(target, format string, a ...any) {
	if p.quiet {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(os.Stderr, "[%s] %s\n", target, fmt.Sprintf(format, a...))
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	targetsF := fs.String("targets", "", "comma-separated targets (default: repo `targets`, else every fleet target)")
	fs.StringVar(targetsF, "t", "", "shorthand for --targets")
	stepsF := fs.String("steps", "", "comma-separated steps to run instead of the default steps")
	withF := fs.String("with", "", "comma-separated non-default steps to add, e.g. --with smoke")
	dirty := fs.Bool("dirty", false, "include uncommitted and untracked (non-ignored) files via a snapshot commit")
	jsonOut := fs.Bool("json", false, "print the full result as JSON on stdout")
	quiet := fs.Bool("q", false, "no progress lines on stderr")
	fleetPath := fs.String("fleet", "", "fleet config (default: $XMAT_FLEET or ~/.config/xmat/fleet.toml)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: xmat run [flags] [<commit-ish>]

Build and test one commit on every target in parallel and print a compact
pass/fail matrix. Defaults to HEAD; with --dirty, to the working tree.

`)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, `
Exit status: 0 all passed, 1 a step failed, 2 usage/config error,
3 a target could not run (unreachable host, hub, ...) but nothing failed.
`)
	}
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) > 1 {
		fs.Usage()
		return exitUsage
	}
	if len(pos) == 1 && *dirty {
		fmt.Fprintln(os.Stderr, "xmat run: --dirty snapshots the working tree; it cannot be combined with a commit")
		return exitUsage
	}

	ctx, stop := signalContext()
	defer stop()

	repo, err := findRepo(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xmat run:", err)
		return exitUsage
	}
	fleet, err := config.FindFleet(*fleetPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xmat run:", err)
		return exitUsage
	}

	// Resolve what to build.
	ref := "HEAD"
	if len(pos) == 1 {
		ref = pos[0]
	}
	var sha string
	snap := false
	if *dirty {
		if sha, snap, err = snapshot(ctx, repo); err != nil {
			fmt.Fprintln(os.Stderr, "xmat run: snapshot:", err)
			return exitUsage
		}
		ref = "working tree"
	} else {
		if sha, err = git(ctx, repo.top, nil, "rev-parse", "--verify", "-q", ref+"^{commit}"); err != nil {
			fmt.Fprintf(os.Stderr, "xmat run: %q is not a commit\n", ref)
			return exitUsage
		}
		if len(pos) == 0 && worktreeDirty(ctx, repo) && !*quiet {
			fmt.Fprintln(os.Stderr, "xmat: note: uncommitted changes are NOT included; building HEAD (use --dirty to include them)")
		}
	}

	cfgText, err := git(ctx, repo.top, nil, "show", sha+":"+config.RepoFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xmat run: %s is missing from %s (create one with `xmat init`, then commit it or use --dirty)\n", config.RepoFile, shortSHA(sha))
		return exitUsage
	}
	rcfg, err := config.ParseRepo(cfgText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xmat run: %s: %v\n", config.RepoFile, err)
		return exitUsage
	}
	repoName := rcfg.Name
	if repoName == "" {
		repoName = filepath.Base(repo.top)
	}
	if !config.ValidName(repoName) {
		fmt.Fprintf(os.Stderr, "xmat run: directory name %q is not a valid repo name; set name = \"...\" in %s\n", repoName, config.RepoFile)
		return exitUsage
	}

	targets := splitList(*targetsF)
	if len(targets) == 0 {
		targets = rcfg.Targets
	}
	if len(targets) == 0 {
		targets = fleet.Order
	}
	for _, t := range targets {
		if fleet.Targets[t] == nil {
			fmt.Fprintf(os.Stderr, "xmat run: target %q is not in %s (known: %s)\n", t, fleet.Path, strings.Join(fleet.Order, ", "))
			return exitUsage
		}
	}
	explicit, with := splitList(*stepsF), splitList(*withF)
	for _, n := range append(append([]string{}, explicit...), with...) {
		if rcfg.Step(n) == nil {
			fmt.Fprintf(os.Stderr, "xmat run: no step %q in %s (steps: %s)\n", n, config.RepoFile, strings.Join(rcfg.StepNames(), ", "))
			return exitUsage
		}
	}

	prog := &progress{quiet: *quiet}
	runID := time.Now().UTC().Format("20060102T150405Z") + "-" + sha[:8]
	if !*quiet {
		what := ref
		if snap {
			what = "working tree snapshot"
		}
		fmt.Fprintf(os.Stderr, "xmat: %s %s (%s) on %s\n", repoName, shortSHA(sha), what, strings.Join(targets, ", "))
	}

	if err := pushToHub(ctx, repo.top, fleet.SSH, fleet.HubPushFor(repoName), sha); err != nil {
		fmt.Fprintln(os.Stderr, "xmat run: pushing to hub:", err)
		return exitInfra
	}

	results := make([]*TargetResult, len(targets))
	var wg sync.WaitGroup
	for i, name := range targets {
		wg.Add(1)
		go func(i int, t *config.Target) {
			defer wg.Done()
			job := &Job{
				Proto: Proto, Repo: repoName, SHA: sha, Target: t.Name, RunID: runID,
				HubFetch: fleet.HubFetchFor(t, repoName), Root: t.Root,
				Steps: explicit, With: with, WantOS: t.OS, WantArch: t.Arch,
			}
			results[i] = runRemote(ctx, transport{fleet, t}, job, prog)
		}(i, fleet.Targets[name])
	}
	wg.Wait()

	lr := &lastRun{Repo: repoName, SHA: sha, Ref: ref, Snapshot: snap, RunID: runID, Started: time.Now(),
		Targets: targets, Results: map[string]*TargetResult{}}
	for _, r := range results {
		lr.Results[r.Target] = r
	}
	lr.Steps = columns(rcfg, results)
	if data, err := json.MarshalIndent(lr, "", "  "); err == nil {
		_ = os.MkdirAll(filepath.Dir(lastRunPath(repo)), 0o755)
		_ = os.WriteFile(lastRunPath(repo), data, 0o644)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(lr)
	} else {
		printMatrix(os.Stdout, lr, results)
	}
	if ctx.Err() != nil {
		return 130
	}
	return exitStatus(results)
}

func exitStatus(results []*TargetResult) int {
	failed, infra := false, false
	for _, r := range results {
		switch r.Status {
		case StFail:
			failed = true
		case StError:
			infra = true
		}
	}
	switch {
	case failed:
		return exitFailed
	case infra:
		return exitInfra
	}
	return exitOK
}

// runRemote runs one job on one target and never returns nil.
func runRemote(ctx context.Context, tr transport, job *Job, prog *progress) *TargetResult {
	start := time.Now()
	res := &TargetResult{Target: job.Target, Host: tr.describe(), Status: StError}
	finish := func() *TargetResult {
		res.Host = tr.describe()
		if res.DurationMS == 0 {
			res.DurationMS = time.Since(start).Milliseconds()
		}
		return res
	}

	cmd := tr.command(ctx, "host")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		res.Error = err.Error()
		return finish()
	}
	// On Ctrl-C, hang up instead of killing: the host sees EOF on stdin,
	// stops the running step's whole process tree and still reports back.
	// Killing a local host process outright would orphan its build.
	cmd.Cancel = func() error { return stdin.Close() }
	cmd.WaitDelay = 30 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		res.Error = err.Error()
		return finish()
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		res.Error = fmt.Sprintf("cannot start %s: %v", cmd.Path, err)
		return finish()
	}
	data, _ := json.Marshal(job)
	_, _ = stdin.Write(append(data, '\n')) // stdin stays open until we are done

	var got *TargetResult
	var hostErr string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte(`{"xmat":`)) {
			continue // login banners and other noise
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.T {
		case "hello":
			res.OS, res.Arch, res.Hostname, res.Version = ev.OS, ev.Arch, ev.Host, ev.Version
			if ev.Version != version {
				prog.printf(job.Target, "note: host runs xmat %s, controller %s", ev.Version, version)
			}
		case "phase":
			prog.printf(job.Target, "%s", ev.Msg)
		case "step_start":
			if ev.Step != nil {
				suffix := ""
				if ev.Step.Desktop {
					suffix = " (desktop session)"
				}
				prog.printf(job.Target, "%s ...%s", ev.Step.Name, suffix)
			}
		case "step_end":
			if s := ev.Step; s != nil {
				switch s.Status {
				case StOK:
					prog.printf(job.Target, "%s ok %s", s.Name, fmtDur(s.DurationMS))
				case StSkip:
					prog.printf(job.Target, "%s skipped", s.Name)
				case StNotRun:
				default:
					prog.printf(job.Target, "%s %s (exit %d) %s", s.Name, strings.ToUpper(s.Status), s.Exit, fmtDur(s.DurationMS))
				}
			}
		case "error":
			hostErr = ev.Msg
		case "result":
			got = ev.Result
		}
		if got != nil {
			break
		}
	}
	stdin.Close()
	waitErr := cmd.Wait()

	if got != nil {
		got.Host = tr.describe()
		if got.Status == StError {
			prog.printf(job.Target, "ERROR: %s", got.Error)
		}
		res = got
		return finish()
	}
	switch {
	case hostErr != "":
		res.Error = hostErr
	case ctx.Err() != nil:
		res.Error = "cancelled"
	default:
		msg := lastLine(stderr.String())
		if msg == "" && waitErr != nil {
			msg = waitErr.Error()
		}
		if msg == "" {
			msg = "host exited without a result"
		}
		if strings.Contains(msg, "not found") || strings.Contains(msg, "not recognized") {
			msg += fmt.Sprintf(" (is xmat installed and on PATH on %s? set xmat = \"...\" in the fleet file otherwise)", tr.describe())
		}
		res.Error = msg
	}
	prog.printf(job.Target, "ERROR: %s", res.Error)
	return finish()
}

// tailBuffer keeps the last 64 KiB written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 64*1024 {
		t.buf = t.buf[len(t.buf)-64*1024:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// columns returns the step names to show, in config order.
func columns(rcfg *config.Repo, results []*TargetResult) []string {
	seen := map[string]bool{}
	for _, r := range results {
		for _, s := range r.Steps {
			seen[s.Name] = true
		}
	}
	var out []string
	for _, n := range rcfg.StepNames() {
		if seen[n] {
			out = append(out, n)
		}
	}
	return out
}

func fmtDur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func cell(status string) string {
	switch status {
	case StOK:
		return "ok"
	case StFail:
		return "FAIL"
	case StTimeout:
		return "TIMEOUT"
	case StCancelled:
		return "CANCEL"
	case StSkip:
		return "skip"
	case "":
		return ""
	}
	return "-"
}

func printMatrix(w io.Writer, lr *lastRun, results []*TargetResult) {
	what := lr.Ref
	if lr.Snapshot {
		what = "working tree snapshot"
	}
	fmt.Fprintf(w, "xmat %s %s (%s)  run %s\n\n", lr.Repo, shortSHA(lr.SHA), what, lr.RunID)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	hdr := []string{"TARGET", "HOST"}
	for _, s := range lr.Steps {
		hdr = append(hdr, strings.ToUpper(s))
	}
	hdr = append(hdr, "TIME")
	fmt.Fprintln(tw, strings.Join(hdr, "\t"))
	for _, r := range results {
		row := []string{r.Target, r.Host}
		byName := map[string]StepResult{}
		for _, s := range r.Steps {
			byName[s.Name] = s
		}
		for _, s := range lr.Steps {
			if r.Status == StError {
				row = append(row, "")
				continue
			}
			sr, ok := byName[s]
			if !ok {
				row = append(row, "")
				continue
			}
			row = append(row, cell(sr.Status))
		}
		t := fmtDur(r.DurationMS)
		switch r.Status {
		case StError:
			t = "ERROR"
		case StSkip:
			t = "skipped"
		}
		row = append(row, t)
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	tw.Flush()

	var details []string
	passed := 0
	for _, r := range results {
		switch r.Status {
		case StOK, StSkip:
			passed++
		case StError:
			details = append(details, fmt.Sprintf("%s: %s", r.Target, r.Error))
		case StFail:
			for _, s := range r.Steps {
				if s.Status == StOK || s.Status == StSkip || s.Status == StNotRun {
					continue
				}
				msg := s.FirstError
				if msg == "" && len(s.Tail) > 0 {
					msg = s.Tail[len(s.Tail)-1]
				}
				if msg == "" {
					msg = "(no output)"
				}
				details = append(details, fmt.Sprintf("%s %s (%s, exit %d): %s\n  more: xmat log %s %s",
					r.Target, s.Name, s.Status, s.Exit, msg, r.Target, s.Name))
			}
		}
		for _, s := range r.Steps {
			if len(s.Artifacts) > 0 {
				details = append(details, fmt.Sprintf("%s %s artifacts: %s  (fetch: xmat get %s <path>)",
					r.Target, s.Name, strings.Join(s.Artifacts, " "), r.Target))
			}
		}
	}
	if len(details) > 0 {
		fmt.Fprintln(w)
		for _, d := range details {
			fmt.Fprintln(w, d)
		}
	}
	fmt.Fprintf(w, "\n%d of %d targets passed.\n", passed, len(results))
}

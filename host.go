// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jm2/xmat/internal/config"
)

// Host-side layout under the root (default ~/xmat):
//
//	repos/<repo>/mirror.git        bare cache of the hub
//	repos/<repo>/wt/<target>       persistent worktree, reused so builds stay incremental
//	repos/<repo>/runs/<run-id>/    logs, scripts, artifacts and result.json for one run
//	repos/<repo>/lock-<target>     serializes runs of one repo+target
//	desktop/                       spool shared with `xmat desktop`

const artifactBudget = 1 << 30 // bytes of artifacts kept per run

func hostRoot(root string) (string, error) {
	if root == "" {
		if env := os.Getenv("XMAT_ROOT"); env != "" {
			root = env
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			root = filepath.Join(home, "xmat")
		}
	} else if strings.HasPrefix(root, "~/") || strings.HasPrefix(root, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, root[2:])
	}
	return filepath.Abs(filepath.FromSlash(root))
}

type emitter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (e *emitter) emit(ev Event) {
	ev.Xmat = 1
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.enc.Encode(ev) // the controller may be gone; nothing useful to do
}

// cmdHost is the host role: read one Job from stdin, run it, report events.
func cmdHost(args []string) int {
	in := bufio.NewReader(os.Stdin)
	line, err := in.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		fmt.Fprintln(os.Stderr, "xmat host: expected a job on stdin (this command is started by `xmat run`)")
		return 2
	}
	em := &emitter{enc: json.NewEncoder(os.Stdout)}
	hostname, _ := os.Hostname()
	em.emit(Event{T: "hello", Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH, Host: hostname})

	var job Job
	if err := json.Unmarshal(line, &job); err != nil {
		em.emit(Event{T: "error", Msg: "malformed job: " + err.Error()})
		return 1
	}

	// Stdin stays open for the life of the job; EOF means the controller
	// went away (Ctrl-C, dropped connection), so stop the build. A terminal
	// Ctrl-C also reaches a local host process directly; ignore it and let
	// the controller's hang-up drive an orderly stop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, in)
		cancel()
	}()
	signal.Ignore(os.Interrupt)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigs
		cancel()
	}()

	res := runJob(ctx, em, &job, hostname)
	em.emit(Event{T: "result", Result: res})
	return 0
}

func fail(res *TargetResult, format string, a ...any) *TargetResult {
	res.Status = StError
	res.Error = fmt.Sprintf(format, a...)
	return res
}

func runJob(ctx context.Context, em *emitter, job *Job, hostname string) *TargetResult {
	start := time.Now()
	res := &TargetResult{Target: job.Target, OS: runtime.GOOS, Arch: runtime.GOARCH, Hostname: hostname, Version: version}
	defer func() { res.DurationMS = time.Since(start).Milliseconds() }()

	if job.Proto > Proto {
		return fail(res, "controller speaks protocol %d but this host's xmat (%s) only knows %d; upgrade xmat on %s", job.Proto, version, Proto, hostname)
	}
	if job.WantOS != "" && job.WantOS != runtime.GOOS {
		return fail(res, "target %s expects os %s but %s runs %s", job.Target, job.WantOS, hostname, runtime.GOOS)
	}
	if job.WantArch != "" && job.WantArch != runtime.GOARCH {
		return fail(res, "target %s expects arch %s but %s runs %s", job.Target, job.WantArch, hostname, runtime.GOARCH)
	}
	for _, n := range []string{job.Repo, job.Target, job.RunID} {
		if !config.ValidName(n) {
			return fail(res, "invalid name %q in job", n)
		}
	}
	root, err := hostRoot(job.Root)
	if err != nil {
		return fail(res, "workspace root: %v", err)
	}
	repoDir := filepath.Join(root, "repos", job.Repo)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return fail(res, "creating %s: %v", repoDir, err)
	}

	phase := func(msg string) { em.emit(Event{T: "phase", Msg: msg}) }

	lock, err := lockWait(ctx, filepath.Join(repoDir, "lock-"+job.Target), func() {
		phase("waiting for another run of this target to finish")
	})
	if err != nil {
		return fail(res, "lock: %v", err)
	}
	defer lock.close()

	// The mirror is shared by every target of this repo on this host, so
	// fetching and worktree bookkeeping are serialized separately.
	mlock, err := lockWait(ctx, filepath.Join(repoDir, "lock-mirror"), nil)
	if err != nil {
		return fail(res, "lock: %v", err)
	}
	defer mlock.close() // released early below; closing twice is harmless
	mirror := filepath.Join(repoDir, "mirror.git")
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		if _, err := git(ctx, "", nil, "init", "--bare", "-q", mirror); err != nil {
			return fail(res, "%v", err)
		}
		if runtime.GOOS == "windows" {
			_, _ = git(ctx, mirror, nil, "config", "core.longpaths", "true")
		}
	}
	if _, err := git(ctx, mirror, nil, "cat-file", "-e", job.SHA+"^{commit}"); err != nil {
		phase("fetching " + shortSHA(job.SHA))
		ref := hubRef(job.SHA)
		if _, err := git(ctx, mirror, nil, "fetch", "--no-tags", "--quiet", job.HubFetch, "+"+ref+":"+ref); err != nil {
			return fail(res, "fetching %s from hub %s: %s", shortSHA(job.SHA), job.HubFetch, strings.TrimSpace(gitStderr(err)))
		}
	} else {
		_, _ = git(ctx, mirror, nil, "update-ref", hubRef(job.SHA), job.SHA)
	}

	// Check the commit out into this target's persistent worktree.
	wt := filepath.Join(repoDir, "wt", job.Target)
	err = checkout(ctx, mirror, wt, job.SHA)
	mlock.close()
	if err != nil {
		return fail(res, "checkout: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(wt, config.RepoFile))
	if err != nil {
		return fail(res, "commit %s has no %s", shortSHA(job.SHA), config.RepoFile)
	}
	repoCfg, err := config.ParseRepo(string(data))
	if err != nil {
		return fail(res, "%s: %v", config.RepoFile, err)
	}
	rc, err := repoCfg.Resolve(runtime.GOOS, job.Target)
	if err != nil {
		return fail(res, "%s: %v", config.RepoFile, err)
	}
	if rc.Skip {
		res.Status = StSkip
		res.Error = "target skipped by " + config.RepoFile
		return res
	}

	if rc.Clean {
		phase("cleaning worktree")
		if _, err := git(ctx, wt, nil, "clean", "-ffdxq"); err != nil {
			return fail(res, "%v", err)
		}
	}
	useSub := rc.Submodules
	if useSub == nil {
		_, statErr := os.Stat(filepath.Join(wt, ".gitmodules"))
		b := statErr == nil
		useSub = &b
	}
	if *useSub {
		phase("updating submodules")
		if _, err := git(ctx, wt, nil, "submodule", "sync", "--recursive", "--quiet"); err != nil {
			return fail(res, "%v", err)
		}
		if _, err := git(ctx, wt, nil, "submodule", "update", "--init", "--recursive", "--force", "--quiet"); err != nil {
			return fail(res, "%v", err)
		}
	}

	selected, err := selectSteps(rc.Steps, job.Steps, job.With)
	if err != nil {
		return fail(res, "%v", err)
	}

	runDir := runDirFor(repoDir, job.RunID, job.Target)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fail(res, "%v", err)
	}
	res.RunDir = runDir
	scriptDir := filepath.Join(runDir, "scripts")
	_ = os.MkdirAll(scriptDir, 0o755)

	xmatEnv := map[string]string{
		"XMAT":          "1",
		"XMAT_REPO":     job.Repo,
		"XMAT_SHA":      job.SHA,
		"XMAT_TARGET":   job.Target,
		"XMAT_OS":       runtime.GOOS,
		"XMAT_ARCH":     runtime.GOARCH,
		"XMAT_RUN_ID":   job.RunID,
		"XMAT_RUN_DIR":  runDir,
		"XMAT_WORKTREE": wt,
	}
	budget := int64(artifactBudget)
	failed := false
	res.Status = StOK
	for _, st := range selected {
		sr := StepResult{Name: st.Name, Desktop: st.Desktop}
		switch {
		case st.Skip:
			sr.Status, sr.Note = StSkip, st.SkipReason
		case failed:
			sr.Status = StNotRun
		case ctx.Err() != nil:
			sr.Status = StCancelled
		}
		if sr.Status != "" {
			res.Steps = append(res.Steps, sr)
			em.emit(Event{T: "step_end", Step: &sr})
			continue
		}
		em.emit(Event{T: "step_start", Step: &StepResult{Name: st.Name, Status: StRunning, Desktop: st.Desktop}})
		sr = runStep(ctx, root, wt, runDir, scriptDir, st, stepEnv(rc.Env, st.Env, xmatEnv), &budget)
		res.Steps = append(res.Steps, sr)
		em.emit(Event{T: "step_end", Step: &sr})
		if sr.Status != StOK {
			failed = true
			res.Status = StFail
		}
	}
	if ctx.Err() != nil && res.Status == StOK {
		res.Status = StFail
	}

	if data, err := json.MarshalIndent(res, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(runDir, "result.json"), data, 0o644)
	}
	pruneRuns(filepath.Join(repoDir, "runs"), job.Target, repoCfg.KeepRuns)
	pruneMirrorRefs(ctx, mirror, 200)
	return res
}

// selectSteps picks which resolved steps to run, in config order.
func selectSteps(all []config.Step, explicit, with []string) ([]config.Step, error) {
	byName := map[string]bool{}
	for _, s := range all {
		byName[s.Name] = true
	}
	want := map[string]bool{}
	for _, n := range append(append([]string{}, explicit...), with...) {
		if !byName[n] {
			return nil, fmt.Errorf("no step named %q in %s", n, config.RepoFile)
		}
		want[n] = true
	}
	var out []config.Step
	for _, s := range all {
		if want[s.Name] || (len(explicit) == 0 && s.Default) {
			out = append(out, s)
		}
	}
	return out, nil
}

// checkout puts sha into the worktree at wt, creating or repairing it.
func checkout(ctx context.Context, mirror, wt, sha string) error {
	if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
		if _, err := git(ctx, wt, nil, "checkout", "--detach", "--force", "--quiet", sha); err == nil {
			return nil
		}
		// Broken worktree (moved mirror, interrupted checkout): rebuild it.
		os.RemoveAll(wt)
	} else if _, err := os.Stat(wt); err == nil {
		os.RemoveAll(wt)
	}
	_, _ = git(ctx, mirror, nil, "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return err
	}
	_, err := git(ctx, mirror, nil, "worktree", "add", "--detach", "--force", "--quiet", wt, sha)
	return err
}

func runStep(ctx context.Context, root, wt, runDir, scriptDir string, st config.Step, extraEnv []string, budget *int64) (sr StepResult) {
	sr = StepResult{Name: st.Name, Desktop: st.Desktop, Log: st.Name + ".log"}
	logPath := filepath.Join(runDir, sr.Log)
	start := time.Now()
	defer func() { sr.DurationMS = time.Since(start).Milliseconds() }()

	argv, err := scriptFor(scriptDir, st)
	if err != nil {
		sr.Status, sr.Exit, sr.FirstError = StFail, -1, err.Error()
		return sr
	}
	dir := wt
	if st.Dir != "" {
		dir = filepath.Join(wt, filepath.FromSlash(st.Dir))
	}
	sctx, cancel := context.WithTimeout(ctx, st.Timeout)
	defer cancel()

	var code int
	if st.Desktop {
		code, err = runDesktop(sctx, root, argv, dir, extraEnv, logPath, st.Timeout)
	} else {
		code, err = execLogged(sctx, argv, dir, append(os.Environ(), extraEnv...), logPath)
	}
	sr.Exit = code
	switch {
	case errors.Is(sctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		sr.Status = StTimeout
		sr.FirstError = fmt.Sprintf("timed out after %s", st.Timeout)
	case ctx.Err() != nil:
		sr.Status = StCancelled
	case err != nil:
		sr.Status = StFail
		sr.FirstError = err.Error()
	case code == 0:
		sr.Status = StOK
	default:
		sr.Status = StFail
	}
	if sr.Status == StFail && sr.FirstError == "" {
		sr.FirstError = firstErrorInFile(logPath)
	}
	if sr.Status != StOK {
		sr.Tail = tailLines(logPath, 20)
	}
	if len(st.Artifacts) > 0 {
		var notes []string
		sr.Artifacts, notes = copyArtifacts(wt, filepath.Join(runDir, "artifacts", st.Name), runDir, st.Artifacts, budget)
		if len(notes) > 0 {
			sr.Note = strings.Join(notes, "; ")
		}
	}
	return sr
}

// runDirFor is where one target's results for one run live.
func runDirFor(repoDir, runID, target string) string {
	return filepath.Join(repoDir, "runs", runID+"-"+target)
}

// pruneRuns keeps the newest keep run directories for target.
func pruneRuns(runsDir, target string, keep int) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return
	}
	var mine []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), "-"+target) {
			mine = append(mine, e.Name())
		}
	}
	sort.Strings(mine) // run IDs start with a UTC timestamp
	for len(mine) > keep {
		os.RemoveAll(filepath.Join(runsDir, mine[0]))
		mine = mine[1:]
	}
}

// pruneMirrorRefs drops all but the newest keep refs/xmat/* refs so old
// snapshot objects can eventually be garbage-collected.
func pruneMirrorRefs(ctx context.Context, mirror string, keep int) {
	out, err := git(ctx, mirror, nil, "for-each-ref", "--sort=-creatordate", "--format=%(refname)", "refs/xmat/")
	if err != nil {
		return
	}
	refs := strings.Fields(out)
	for i := keep; i < len(refs); i++ {
		_, _ = git(ctx, mirror, nil, "update-ref", "-d", refs[i])
	}
}

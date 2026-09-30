// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jm2/xmat/internal/config"
)

// cmdLog prints a step log from the last run.
func cmdLog(args []string) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	tail := fs.Int("tail", 100, "print only the last N lines (0 = whole log)")
	fs.IntVar(tail, "n", 100, "shorthand for --tail")
	grep := fs.String("grep", "", "print only lines containing this text (case-insensitive)")
	raw := fs.Bool("raw", false, "print the log byte-for-byte (by default terminal control sequences are stripped)")
	fleetPath := fs.String("fleet", "", "fleet config path")
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: xmat log [flags] <target> [<step>]

Print a step's log from the last `+"`xmat run`"+` in this repository. Without <step>,
prints the first step that did not pass (or the last step that ran).

`)
		fs.PrintDefaults()
	}
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) < 1 || len(pos) > 2 {
		if err == nil {
			fs.Usage()
		}
		return exitUsage
	}
	ctx, stop := signalContext()
	defer stop()
	repo, err := findRepo(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "xmat log:", err)
		return exitUsage
	}
	lr, err := loadLastRun(repo)
	if err != nil {
		fmt.Fprintln(stderr, "xmat log:", err)
		return exitUsage
	}
	res := lr.Results[pos[0]]
	if res == nil {
		fmt.Fprintf(stderr, "xmat log: target %q was not part of run %s (targets: %s)\n", pos[0], lr.RunID, strings.Join(lr.Targets, ", "))
		return exitUsage
	}
	if res.Status == StError {
		fmt.Fprintf(stderr, "xmat log: %s never ran any steps: %s\n", res.Target, res.Error)
		return exitFailed
	}
	step := ""
	if len(pos) == 2 {
		step = pos[1]
	} else {
		for _, s := range res.Steps {
			if s.Status != StSkip && s.Status != StNotRun {
				step = s.Name
			}
			if s.Status != StOK && s.Status != StSkip && s.Status != StNotRun {
				break
			}
		}
	}
	var sr *StepResult
	for i := range res.Steps {
		if res.Steps[i].Name == step {
			sr = &res.Steps[i]
		}
	}
	if sr == nil || sr.Log == "" {
		fmt.Fprintf(stderr, "xmat log: no log for step %q on %s\n", step, res.Target)
		return exitUsage
	}

	fleet, tr, err := targetTransport(*fleetPath, res.Target)
	if err != nil {
		fmt.Fprintln(stderr, "xmat log:", err)
		return exitUsage
	}
	req := FileRequest{Proto: Proto, Root: fleet.Targets[res.Target].Root, Repo: lr.Repo, RunID: lr.RunID,
		Target: res.Target, Path: sr.Log, Tail: *tail}
	if *grep != "" {
		req.Tail = 0
	}
	// Logs are written by commit-controlled code; strip control sequences
	// unless asked not to.
	body := stdout
	if *raw {
		body = os.Stdout
	}
	out := body
	var gw *grepWriter
	if *grep != "" {
		gw = &grepWriter{needle: strings.ToLower(*grep), w: body, max: *tail}
		out = gw
	}
	fmt.Fprintf(stderr, "== %s %s (%s, exit %d) %s\n", res.Target, sr.Name, sr.Status, sr.Exit, lr.RunID)
	if err := tr.call(ctx, "host-file", req, out); err != nil {
		fmt.Fprintln(stderr, "xmat log:", err)
		return exitInfra
	}
	if gw != nil {
		gw.flush()
	}
	return exitOK
}

// grepWriter filters streamed log text by substring, keeping the last max matches.
type grepWriter struct {
	needle  string
	w       io.Writer
	max     int
	partial []byte
	hits    []string
}

func (g *grepWriter) Write(p []byte) (int, error) {
	g.partial = append(g.partial, p...)
	for {
		i := strings.IndexByte(string(g.partial), '\n')
		if i < 0 {
			break
		}
		g.line(string(g.partial[:i]))
		g.partial = g.partial[i+1:]
	}
	return len(p), nil
}

func (g *grepWriter) line(l string) {
	if strings.Contains(strings.ToLower(l), g.needle) {
		g.hits = append(g.hits, strings.TrimRight(l, "\r"))
		if g.max > 0 && len(g.hits) > g.max {
			g.hits = g.hits[1:]
		}
	}
}

func (g *grepWriter) flush() {
	if len(g.partial) > 0 {
		g.line(string(g.partial))
	}
	for _, h := range g.hits {
		fmt.Fprintln(g.w, h)
	}
}

// cmdGet downloads a file (usually an artifact) from the last run.
func cmdGet(args []string) int {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	outPath := fs.String("o", "", "write to this path (default: ./<basename>; '-' for stdout)")
	fleetPath := fs.String("fleet", "", "fleet config path")
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: xmat get [-o path] <target> <path>

Download a file from a target's run directory of the last run, e.g. an
artifact listed in the matrix (artifacts/smoke/smoke.png) or result.json.

`)
		fs.PrintDefaults()
	}
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 2 {
		if err == nil {
			fs.Usage()
		}
		return exitUsage
	}
	ctx, stop := signalContext()
	defer stop()
	repo, err := findRepo(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "xmat get:", err)
		return exitUsage
	}
	lr, err := loadLastRun(repo)
	if err != nil {
		fmt.Fprintln(stderr, "xmat get:", err)
		return exitUsage
	}
	if lr.Results[pos[0]] == nil {
		fmt.Fprintf(stderr, "xmat get: target %q was not part of run %s\n", pos[0], lr.RunID)
		return exitUsage
	}
	fleet, tr, err := targetTransport(*fleetPath, pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "xmat get:", err)
		return exitUsage
	}
	dest := *outPath
	if dest == "" {
		dest = filepath.Base(filepath.FromSlash(pos[1]))
	}
	var w io.Writer = os.Stdout
	var f *os.File
	if dest != "-" {
		if f, err = os.Create(dest + ".part"); err != nil {
			fmt.Fprintln(stderr, "xmat get:", err)
			return exitUsage
		}
		w = f
	}
	req := FileRequest{Proto: Proto, Root: fleet.Targets[pos[0]].Root, Repo: lr.Repo, RunID: lr.RunID, Target: pos[0], Path: pos[1]}
	err = tr.call(ctx, "host-file", req, w)
	if f != nil {
		f.Close()
		if err != nil {
			os.Remove(dest + ".part")
		} else if err = os.Rename(dest+".part", dest); err == nil {
			fmt.Fprintln(stderr, "wrote", dest)
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "xmat get:", err)
		return exitInfra
	}
	return exitOK
}

func targetTransport(fleetPath, target string) (*config.Fleet, transport, error) {
	fleet, err := config.FindFleet(fleetPath)
	if err != nil {
		return nil, transport{}, err
	}
	t := fleet.Targets[target]
	if t == nil {
		return nil, transport{}, fmt.Errorf("target %q is no longer in %s", target, fleet.Path)
	}
	return fleet, transport{fleet, t}, nil
}

// cmdHostFile is the host side of `xmat log` and `xmat get`.
func cmdHostFile(args []string) int {
	var req FileRequest
	line, err := bufio.NewReader(os.Stdin).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		fmt.Fprintln(os.Stderr, "xmat host-file: expected a request on stdin")
		return 2
	}
	if err := json.Unmarshal(line, &req); err != nil {
		fmt.Fprintln(os.Stderr, "xmat host-file: malformed request:", err)
		return 2
	}
	path, err := runFilePath(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xmat host-file:", err)
		return 1
	}
	if req.Tail > 0 {
		lines := tailLines(path, req.Tail)
		if lines == nil {
			if _, err := os.Stat(path); err != nil {
				fmt.Fprintln(os.Stderr, "xmat host-file:", err)
				return 1
			}
		}
		for _, l := range lines {
			fmt.Println(l)
		}
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xmat host-file:", err)
		return 1
	}
	defer f.Close()
	if _, err := io.Copy(os.Stdout, f); err != nil {
		fmt.Fprintln(os.Stderr, "xmat host-file:", err)
		return 1
	}
	return 0
}

// runFilePath resolves a request to a file inside one run directory.
func runFilePath(req FileRequest) (string, error) {
	for _, n := range []string{req.Repo, req.RunID, req.Target} {
		if !config.ValidName(n) {
			return "", fmt.Errorf("invalid name %q", n)
		}
	}
	rel := filepath.FromSlash(req.Path)
	if req.Path == "" || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" ||
		strings.HasPrefix(req.Path, "/") || strings.HasPrefix(req.Path, `\`) {
		return "", errors.New("path must be relative to the run directory")
	}
	clean := filepath.Clean(rel)
	if !filepath.IsLocal(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the run directory")
	}
	root, err := hostRoot(req.Root)
	if err != nil {
		return "", err
	}
	dir := runDirFor(filepath.Join(root, "repos", req.Repo), req.RunID, req.Target)
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("run %s for %s is gone from this host (hosts keep the newest runs only)", req.RunID, req.Target)
	}
	return filepath.Join(dir, clean), nil
}

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/jm2/xmat/internal/config"
)

// cmdDoctor checks that every target is reachable and ready.
func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	targetsF := fs.String("targets", "", "comma-separated targets (default: all)")
	fleetPath := fs.String("fleet", "", "fleet config path")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: xmat doctor [--targets a,b]\n\nCheck ssh reachability, xmat and git on each host, hub access and desktop agents.")
		fs.PrintDefaults()
	}
	if _, err := parseInterspersed(fs, args); err != nil {
		return exitUsage
	}
	ctx, stop := signalContext()
	defer stop()
	fleet, err := config.FindFleet(*fleetPath)
	if err != nil {
		fmt.Fprintln(stderr, "xmat doctor:", err)
		return exitUsage
	}
	fmt.Fprintf(stdout, "fleet: %s\n", fleet.Path)

	// Hub checks need a repo name; use the current repository if there is one.
	repoName := ""
	if repo, err := findRepo(ctx); err == nil {
		repoName = filepath.Base(repo.top)
		if text, err := os.ReadFile(filepath.Join(repo.top, config.RepoFile)); err == nil {
			if rc, err := config.ParseRepo(string(text)); err != nil {
				fmt.Fprintf(stdout, "%s: %v\n", config.RepoFile, err)
			} else if rc.Name != "" {
				repoName = rc.Name
			}
		}
	}
	if repoName != "" {
		push := fleet.HubPushFor(repoName)
		fmt.Fprintf(stdout, "hub (push): %s ... %s\n", push, hubState(ctx, push))
	} else {
		fmt.Fprintln(stdout, "hub: run doctor inside a repository to check hub access for it")
	}

	targets := splitList(*targetsF)
	if len(targets) == 0 {
		targets = fleet.Order
	}
	type row struct {
		name string
		info *HostInfo
		err  error
	}
	rows := make([]row, len(targets))
	var wg sync.WaitGroup
	for i, name := range targets {
		t := fleet.Targets[name]
		if t == nil {
			rows[i] = row{name: name, err: fmt.Errorf("not in fleet")}
			continue
		}
		wg.Add(1)
		go func(i int, t *config.Target) {
			defer wg.Done()
			req := InfoRequest{Proto: Proto, Root: t.Root}
			if repoName != "" {
				req.HubFetch = fleet.HubFetchFor(t, repoName)
			}
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			var out bytes.Buffer
			tr := transport{fleet, t}
			if err := tr.call(cctx, "host-info", req, &out); err != nil {
				rows[i] = row{name: t.Name, err: err}
				return
			}
			info, err := parseInfo(out.Bytes())
			rows[i] = row{name: t.Name, info: info, err: err}
		}(i, t)
	}
	wg.Wait()

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nTARGET\tHOST\tXMAT\tOS/ARCH\tGIT\tHUB\tDESKTOP\tSHELLS")
	problems := 0
	var notes []string
	for _, r := range rows {
		host := ""
		if t := fleet.Targets[r.name]; t != nil {
			host = transport{fleet, t}.describe()
		}
		if r.err != nil {
			problems++
			fmt.Fprintf(tw, "%s\t%s\tUNREACHABLE\t\t\t\t\t\n", r.name, host)
			notes = append(notes, fmt.Sprintf("%s: %v", r.name, r.err))
			continue
		}
		in := r.info
		t := fleet.Targets[r.name]
		osArch := in.OS + "/" + in.Arch
		if (t.OS != "" && t.OS != in.OS) || (t.Arch != "" && t.Arch != in.Arch) {
			problems++
			notes = append(notes, fmt.Sprintf("%s: fleet expects %s/%s but host is %s", r.name, t.OS, t.Arch, osArch))
			osArch += " (!)"
		}
		ver := in.Version
		if in.Version != version {
			ver += " (!)"
			notes = append(notes, fmt.Sprintf("%s: host has xmat %s, this machine %s", r.name, in.Version, version))
		}
		hub := in.Hub
		if hub == "" {
			hub = "-"
		} else if hub != "ok" && hub != "missing" {
			problems++
			notes = append(notes, fmt.Sprintf("%s: cannot fetch from hub: %s", r.name, hub))
			hub = "FAIL"
		}
		if !in.RootOK {
			problems++
			notes = append(notes, fmt.Sprintf("%s: %s", r.name, in.RootErr))
		}
		gitv := strings.TrimPrefix(in.Git, "git version ")
		if gitv == "" {
			problems++
			gitv = "MISSING"
			notes = append(notes, fmt.Sprintf("%s: git is not on PATH for non-interactive ssh sessions", r.name))
		}
		var shells []string
		for _, s := range []string{"sh", "bash", "cmd", "pwsh", "powershell"} {
			if in.Shells[s] != "" {
				shells = append(shells, s)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.name, host, ver, osArch, gitv, hub, in.Desktop, strings.Join(shells, ","))
	}
	tw.Flush()
	if len(notes) > 0 {
		fmt.Fprintln(stdout)
		for _, n := range notes {
			fmt.Fprintln(stdout, n)
		}
	}
	if problems > 0 {
		return exitInfra
	}
	return exitOK
}

func parseInfo(out []byte) (*HostInfo, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.HasPrefix(line, []byte(`{"version"`)) {
			var in HostInfo
			if err := json.Unmarshal(line, &in); err != nil {
				return nil, err
			}
			return &in, nil
		}
	}
	return nil, fmt.Errorf("host replied without host info (is it running an xmat older than this one?)")
}

func hubState(ctx context.Context, url string) string {
	if p := localHubPath(url); p != "" {
		if _, err := os.Stat(filepath.Join(p, "HEAD")); err != nil {
			return "not created yet (xmat run creates it)"
		}
		return "ok"
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := git(cctx, "", nil, "ls-remote", "--heads", url); err != nil {
		msg := gitStderr(err)
		for _, h := range hubMissingHints[:3] {
			if strings.Contains(msg, h) {
				return "reachable; repository not created yet (xmat run creates it)"
			}
		}
		return "FAIL: " + lastLine(msg)
	}
	return "ok"
}

// cmdHostInfo is the host side of `xmat doctor`.
func cmdHostInfo(args []string) int {
	var req InfoRequest
	line, _ := bufio.NewReader(os.Stdin).ReadBytes('\n')
	_ = json.Unmarshal(line, &req)
	hostname, _ := os.Hostname()
	in := HostInfo{Version: version, Proto: Proto, OS: runtime.GOOS, Arch: runtime.GOARCH, Hostname: hostname, Shells: map[string]string{}}
	if req.Proto > Proto {
		in.ProtoError = fmt.Sprintf("controller protocol %d is newer than this host's %d", req.Proto, Proto)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if v, err := git(ctx, "", nil, "--version"); err == nil {
		in.Git = v
	}
	root, err := hostRoot(req.Root)
	in.Root = root
	if err == nil {
		err = ensureRoot(root)
	}
	if err == nil {
		probe := filepath.Join(root, ".xmat-write-test")
		if err = os.WriteFile(probe, []byte("ok"), 0o600); err == nil {
			os.Remove(probe)
		}
	}
	in.RootOK = err == nil
	if err != nil {
		in.RootErr = err.Error()
	}
	in.Desktop = desktopStatus(root)
	for _, s := range []string{"sh", "bash", "cmd", "pwsh", "powershell"} {
		if p, err := exec.LookPath(s); err == nil {
			in.Shells[s] = p
		}
	}
	if req.HubFetch != "" {
		if _, err := git(ctx, "", nil, "ls-remote", "--heads", req.HubFetch); err != nil {
			msg := gitStderr(err)
			in.Hub = lastLine(msg)
			for _, h := range hubMissingHints[:3] {
				if strings.Contains(msg, h) {
					in.Hub = "missing"
				}
			}
		} else {
			in.Hub = "ok"
		}
	}
	data, _ := json.Marshal(in)
	fmt.Println(string(data))
	return 0
}

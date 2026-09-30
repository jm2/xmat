// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jm2/xmat/internal/config"
)

// lockWait takes the lock at path, polling until ctx ends. onWait is called
// once if the lock is busy.
func lockWait(ctx context.Context, path string, onWait func()) (*fileLock, error) {
	l, err := openLock(path)
	if err != nil {
		return nil, err
	}
	notified := false
	for {
		ok, err := l.tryLock()
		if err != nil {
			l.close()
			return nil, err
		}
		if ok {
			return l, nil
		}
		if !notified && onWait != nil {
			onWait()
			notified = true
		}
		select {
		case <-ctx.Done():
			l.close()
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// scriptFor writes a step's script into dir and returns the argv that runs it.
// Writing a file sidesteps every quoting difference between sh, cmd.exe and
// PowerShell.
func scriptFor(dir string, st config.Step) ([]string, error) {
	var name, body string
	var argv []string
	switch st.Shell {
	case "sh":
		name = st.Name + ".sh"
		body = st.Run + "\n"
		argv = []string{"sh", "-e"}
	case "bash":
		name = st.Name + ".sh"
		body = st.Run + "\n"
		argv = []string{"bash", "-e", "-o", "pipefail"}
	case "cmd":
		name = st.Name + ".cmd"
		body = "@echo off\r\n" + strings.ReplaceAll(strings.ReplaceAll(st.Run, "\r\n", "\n"), "\n", "\r\n") + "\r\n"
		argv = []string{"cmd.exe", "/D", "/C"}
	case "pwsh", "powershell":
		name = st.Name + ".ps1"
		body = "\uFEFF$ErrorActionPreference = 'Stop'\n" +
			"$PSNativeCommandUseErrorActionPreference = $true\n" +
			st.Run + "\n" +
			"if ($LASTEXITCODE) { exit $LASTEXITCODE }\n"
		argv = []string{st.Shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File"}
	default:
		return nil, fmt.Errorf("unknown shell %q", st.Shell)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		return nil, err
	}
	return append(argv, path), nil
}

// stepEnv returns the extra environment for a step, sorted for stable output.
func stepEnv(repoEnv, step map[string]string, xmat map[string]string) []string {
	m := map[string]string{}
	for k, v := range repoEnv {
		m[k] = v
	}
	for k, v := range step {
		m[k] = v
	}
	for k, v := range xmat {
		m[k] = v
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

// execLogged runs argv in dir with its combined output appended to logPath.
func execLogged(ctx context.Context, argv []string, dir string, env []string, logPath string) (int, error) {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return -1, err
	}
	defer logf.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = logf, logf
	prepareCmd(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(logf, "xmat: cannot start %s: %v\n", argv[0], err)
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return exitCode(err), nil
	case <-ctx.Done():
		_ = killTree(cmd)
		select {
		case err := <-done:
			return exitCode(err), ctx.Err()
		case <-time.After(15 * time.Second):
			return -1, ctx.Err()
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if c := ee.ExitCode(); c >= 0 {
			return c
		}
	}
	return -1
}

// copyArtifacts copies files matching patterns (relative to the worktree) into
// dst, preserving their relative paths. It returns paths relative to base.
func copyArtifacts(wt, dst, base string, patterns []string, budget *int64) ([]string, []string) {
	var copied, notes []string
	for _, pat := range patterns {
		matches, err := filepath.Glob(filepath.Join(wt, filepath.FromSlash(pat)))
		if err != nil {
			notes = append(notes, fmt.Sprintf("bad artifact pattern %q: %v", pat, err))
			continue
		}
		if len(matches) == 0 {
			notes = append(notes, fmt.Sprintf("no files matched artifact pattern %q", pat))
		}
		for _, m := range matches {
			rel, err := filepath.Rel(wt, m)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			_ = filepath.Walk(m, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() || !info.Mode().IsRegular() {
					return nil
				}
				if *budget-info.Size() < 0 {
					notes = append(notes, "artifact size budget exhausted; skipped "+p)
					return nil
				}
				r, _ := filepath.Rel(wt, p)
				out := filepath.Join(dst, r)
				if err := copyFile(p, out); err != nil {
					notes = append(notes, err.Error())
					return nil
				}
				*budget -= info.Size()
				if rb, err := filepath.Rel(base, out); err == nil {
					copied = append(copied, filepath.ToSlash(rb))
				}
				return nil
			})
		}
	}
	return copied, notes
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// writeFileAtomic writes data to path via a temp file and rename.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The hub is a bare repository that every host can fetch from. The controller
// pushes the commit under test to refs/xmat/<sha>, so WIP and snapshot commits
// never touch GitHub or any branch.

func hubRef(sha string) string { return "refs/xmat/" + sha }

// localHubPath returns the filesystem path for a hub URL that is local, or "".
func localHubPath(u string) string {
	if strings.HasPrefix(u, "file://") {
		p, err := url.Parse(u)
		if err != nil {
			return ""
		}
		path := p.Path
		if len(path) > 2 && path[0] == '/' && path[2] == ':' { // file:///C:/x
			path = path[1:]
		}
		return filepath.FromSlash(path)
	}
	if strings.Contains(u, "://") {
		return ""
	}
	if len(u) >= 2 && u[1] == ':' && ((u[0] >= 'A' && u[0] <= 'Z') || (u[0] >= 'a' && u[0] <= 'z')) {
		return filepath.FromSlash(u) // C:\... or C:/...
	}
	colon, slash := strings.Index(u, ":"), strings.Index(u, "/")
	if colon > 0 && (slash < 0 || colon < slash) {
		return "" // scp-like host:path
	}
	return u
}

// sshHubTarget splits an ssh hub URL into ssh destination, port and path.
func sshHubTarget(u string) (dest, port, path string, err error) {
	if strings.HasPrefix(u, "ssh://") || strings.HasPrefix(u, "git+ssh://") || strings.HasPrefix(u, "ssh+git://") {
		p, perr := url.Parse(u)
		if perr != nil {
			return "", "", "", perr
		}
		dest = p.Hostname()
		if p.User != nil {
			dest = p.User.Username() + "@" + dest
		}
		path = p.Path
		if strings.HasPrefix(path, "/~") {
			path = path[1:]
		}
		return dest, p.Port(), path, nil
	}
	if strings.Contains(u, "://") {
		return "", "", "", fmt.Errorf("hub %q is not an ssh URL; create the bare repository there yourself", u)
	}
	i := strings.Index(u, ":")
	if i <= 0 {
		return "", "", "", fmt.Errorf("cannot parse hub URL %q", u)
	}
	return u[:i], "", u[i+1:], nil
}

// ensureLocalHub creates a local bare hub repository if it does not exist.
func ensureLocalHub(ctx context.Context, path string) error {
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	_, err := git(ctx, "", nil, "init", "--bare", "-q", path)
	return err
}

// initRemoteHub creates a bare repository on an ssh hub (assumed POSIX).
func initRemoteHub(ctx context.Context, sshCmd []string, hubURL string) error {
	dest, port, path, err := sshHubTarget(hubURL)
	if err != nil {
		return err
	}
	if strings.ContainsAny(path, "'\n") {
		return fmt.Errorf("refusing to create hub at unusual path %q", path)
	}
	quoted := "'" + path + "'"
	if strings.HasPrefix(path, "~/") {
		quoted = `"$HOME"/'` + path[2:] + "'"
	}
	args := append([]string{}, sshCmd[1:]...)
	if port != "" {
		args = append(args, "-p", port)
	}
	args = append(args, dest, "mkdir -p "+quoted+" && git init --bare -q "+quoted)
	out, err := exec.CommandContext(ctx, sshCmd[0], args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("creating hub repository on %s: %v: %s", dest, err, strings.TrimSpace(string(out)))
	}
	return nil
}

var hubMissingHints = []string{
	"does not appear to be a git repository",
	"not a git repository",
	"No such file or directory",
	"Could not read from remote repository",
}

// pushToHub pushes sha to the hub, creating the hub repository if needed.
func pushToHub(ctx context.Context, repoTop string, sshCmd []string, hubURL, sha string) error {
	if p := localHubPath(hubURL); p != "" {
		if err := ensureLocalHub(ctx, p); err != nil {
			return fmt.Errorf("creating hub repository %s: %w", p, err)
		}
	}
	push := func() error {
		_, err := git(ctx, repoTop, nil, "push", "--quiet", "--no-verify", hubURL, sha+":"+hubRef(sha))
		return err
	}
	err := push()
	if err == nil || localHubPath(hubURL) != "" {
		return err
	}
	msg := gitStderr(err)
	missing := false
	for _, h := range hubMissingHints {
		if strings.Contains(msg, h) {
			missing = true
		}
	}
	if !missing {
		return err
	}
	if ierr := initRemoteHub(ctx, sshCmd, hubURL); ierr != nil {
		return errors.Join(err, ierr)
	}
	return push()
}

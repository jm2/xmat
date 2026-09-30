// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitEnv returns the environment for git invocations. Credential prompts are
// disabled; ssh prompts fail on their own when there is no terminal, and the
// user's own core.sshCommand is left alone.
func gitEnv(extra ...string) []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	return append(env, extra...)
}

type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	msg := strings.TrimSpace(e.stderr)
	if msg == "" {
		msg = e.err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.args, " "), msg)
}

func (e *gitError) Unwrap() error { return e.err }

// git runs git in dir and returns trimmed stdout.
func git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if env == nil {
		env = gitEnv()
	}
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", &gitError{args: args, err: err, stderr: errb.String()}
	}
	return strings.TrimSpace(out.String()), nil
}

func gitStderr(err error) string {
	var ge *gitError
	if errors.As(err, &ge) {
		return ge.stderr
	}
	return err.Error()
}

// repoInfo describes the repository the controller runs in.
type repoInfo struct {
	top    string // working tree root
	common string // common git dir (shared by worktrees)
	gitDir string // this worktree's git dir
}

func findRepo(ctx context.Context) (*repoInfo, error) {
	top, err := git(ctx, "", nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, errors.New("not inside a git working tree")
	}
	common, err := git(ctx, top, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		// git < 2.31 lacks --path-format
		if common, err = git(ctx, top, nil, "rev-parse", "--git-common-dir"); err != nil {
			return nil, err
		}
		if !filepath.IsAbs(common) {
			common = filepath.Join(top, common)
		}
	}
	gitDir, err := git(ctx, top, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		gitDir = common
	}
	return &repoInfo{top: filepath.FromSlash(top), common: filepath.FromSlash(common), gitDir: filepath.FromSlash(gitDir)}, nil
}

// snapshot commits the working tree (tracked changes plus untracked,
// non-ignored files) into a dangling commit without touching the real index,
// HEAD or any branch. It returns HEAD itself when nothing differs.
func snapshot(ctx context.Context, r *repoInfo) (sha string, dirty bool, err error) {
	head, headErr := git(ctx, r.top, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")

	realIndex, err := git(ctx, r.top, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		realIndex, err = git(ctx, r.top, nil, "rev-parse", "--git-path", "index")
		if err != nil {
			return "", false, err
		}
		if !filepath.IsAbs(realIndex) {
			realIndex = filepath.Join(r.top, realIndex)
		}
	}
	tmp, err := os.CreateTemp(r.common, "xmat-index-*")
	if err != nil {
		return "", false, err
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)
	if data, err := os.ReadFile(filepath.FromSlash(realIndex)); err == nil {
		if err := os.WriteFile(tmpName, data, 0o600); err != nil {
			return "", false, err
		}
	} else {
		os.Remove(tmpName) // no index yet: let git start from empty
	}

	env := gitEnv("GIT_INDEX_FILE="+tmpName,
		"GIT_AUTHOR_NAME=xmat", "GIT_AUTHOR_EMAIL=xmat@localhost",
		"GIT_COMMITTER_NAME=xmat", "GIT_COMMITTER_EMAIL=xmat@localhost")
	if _, err := git(ctx, r.top, env, "add", "-A", "--", "."); err != nil {
		return "", false, err
	}
	tree, err := git(ctx, r.top, env, "write-tree")
	if err != nil {
		return "", false, err
	}
	if headErr == nil {
		headTree, err := git(ctx, r.top, nil, "rev-parse", head+"^{tree}")
		if err == nil && headTree == tree {
			return head, false, nil
		}
	}
	args := []string{"commit-tree", tree, "-m", "xmat snapshot of working tree"}
	if headErr == nil {
		args = append(args, "-p", head)
	}
	sha, err = git(ctx, r.top, env, args...)
	if err != nil {
		return "", false, err
	}
	return sha, true, nil
}

// worktreeDirty reports whether the working tree differs from HEAD
// (including untracked, non-ignored files).
func worktreeDirty(ctx context.Context, r *repoInfo) bool {
	out, err := git(ctx, r.top, nil, "status", "--porcelain", "--untracked-files=normal")
	return err == nil && out != ""
}

func shortSHA(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

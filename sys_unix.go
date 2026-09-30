// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// fileLock is an advisory lock that the OS releases when the process exits,
// so a crashed or killed run never leaves a stale lock behind.
type fileLock struct{ f *os.File }

func openLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return &fileLock{f: f}, nil
}

// tryLock takes the lock without waiting; it reports false if another process holds it.
func (l *fileLock) tryLock() (bool, error) {
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

func (l *fileLock) close() { l.f.Close() }

// procTree is a started step and everything it spawned. The step runs in its
// own process group so a timeout can reach the whole build (compilers,
// linkers, test binaries), not just the shell.
type procTree struct{ pgid int }

func startTree(cmd *exec.Cmd) (*procTree, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &procTree{pgid: cmd.Process.Pid}, nil
}

// terminate asks the whole group to stop.
func (t *procTree) terminate() { _ = syscall.Kill(-t.pgid, syscall.SIGTERM) }

// kill forcibly stops the group, plus (on Linux) anything that left the
// group with setsid and was orphaned onto us.
func (t *procTree) kill() {
	_ = syscall.Kill(-t.pgid, syscall.SIGKILL)
	killOrphans(t.pgid)
}

func (t *procTree) release() {}

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const prSetChildSubreaper = 36

// subreaper is set only in `xmat host`, which runs one step at a time. The
// desktop agent runs jobs concurrently and must not reap other jobs' children.
var subreaper bool

// setSubreaper makes `xmat host` the reaper for orphaned descendants, so a
// build process that escapes its process group with setsid() and outlives its
// parent still ends up as our child, where killOrphans can find it.
func setSubreaper() {
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0)
	subreaper = errno == 0
}

// killOrphans kills and reaps every child of this process except the step's
// own shell (pid except), which os/exec is still waiting for. The host runs
// one step at a time and calls this only after that step was stopped, so
// any other child is a leftover from it.
func killOrphans(except int) {
	if !subreaper {
		return
	}
	me := strconv.Itoa(os.Getpid())
	for round := 0; round < 20; round++ {
		var kids []int
		for _, pid := range childrenOf(me) {
			if pid != except {
				kids = append(kids, pid)
			}
		}
		if len(kids) == 0 {
			return
		}
		for _, pid := range kids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			var ws syscall.WaitStatus
			_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func childrenOf(ppid string) []int {
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	var out []int
	for _, p := range stats {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := string(data)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:]) // state ppid pgrp ...
		if len(f) >= 2 && f[1] == ppid {
			if pid, err := strconv.Atoi(filepath.Base(filepath.Dir(p))); err == nil {
				out = append(out, pid)
			}
		}
	}
	return out
}

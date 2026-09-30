// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

//go:build windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx               = kernel32.NewProc("LockFileEx")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errorLockViolation      = syscall.Errno(33)
	processSetQuota         = 0x0100
	processTerminate        = 0x0001
)

// fileLock is a LockFileEx lock; Windows releases it when the process exits.
type fileLock struct{ f *os.File }

func openLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) tryLock() (bool, error) {
	var ol syscall.Overlapped
	r1, _, err := procLockFileEx.Call(
		l.f.Fd(),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0, 1, 0,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 != 0 {
		return true, nil
	}
	if err == errorLockViolation {
		return false, nil
	}
	return false, err
}

func (l *fileLock) close() { l.f.Close() }

// prepareCmd starts the child in its own process group. For cmd.exe it also
// builds the command line by hand: cmd does not parse arguments the way
// CommandLineToArgvW does. The script path is passed through an environment
// variable so that nothing in it (spaces, parentheses, '%', '&') is ever
// parsed by cmd: the variable is expanded once, inside quotes, and not again.
func prepareCmd(cmd *exec.Cmd) {
	attr := &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	if len(cmd.Args) == 4 && strings.EqualFold(cmd.Args[0], "cmd.exe") && cmd.Args[1] == "/D" && cmd.Args[2] == "/C" {
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		cmd.Env = append(cmd.Env, "XMAT_SCRIPT="+cmd.Args[3])
		attr.CmdLine = `cmd.exe /D /S /C ""%XMAT_SCRIPT%""`
	}
	cmd.SysProcAttr = attr
}

// procTree is a started step and everything it spawned, tracked with a Job
// Object: unlike `taskkill /T`, a job still contains grandchildren whose
// parent has already exited (for example anything launched with `start`).
type procTree struct {
	cmd *exec.Cmd
	job uintptr
}

func startTree(cmd *exec.Cmd) (*procTree, error) {
	prepareCmd(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := &procTree{cmd: cmd}
	job, _, _ := procCreateJobObjectW.Call(0, 0)
	if job != 0 {
		h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
		if err == nil {
			r, _, _ := procAssignProcessToJobObject.Call(job, uintptr(h))
			syscall.CloseHandle(h)
			if r != 0 {
				t.job = job
			} else {
				syscall.CloseHandle(syscall.Handle(job))
			}
		} else {
			syscall.CloseHandle(syscall.Handle(job))
		}
	}
	return t, nil
}

// Console process trees have no reliable polite stop on Windows.
func (t *procTree) terminate() { t.kill() }

func (t *procTree) kill() {
	if t.job != 0 {
		_, _, _ = procTerminateJobObject.Call(t.job, 1)
	}
	// Belt and braces for anything spawned before the job was assigned.
	if t.cmd.Process != nil {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(t.cmd.Process.Pid)).Run()
	}
}

// release closes the job handle. The job has no KILL_ON_JOB_CLOSE, so build
// servers a successful step leaves running (mspdbsrv, sccache) keep running.
func (t *procTree) release() {
	if t.job != 0 {
		syscall.CloseHandle(syscall.Handle(t.job))
		t.job = 0
	}
}

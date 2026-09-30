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
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx = kernel32.NewProc("LockFileEx")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errorLockViolation      = syscall.Errno(33)
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
// CommandLineToArgvW does, and /S with doubled quotes is the only form that
// survives script paths containing spaces or parentheses.
func prepareCmd(cmd *exec.Cmd) {
	attr := &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	if len(cmd.Args) == 4 && strings.EqualFold(cmd.Args[0], "cmd.exe") && cmd.Args[1] == "/D" && cmd.Args[2] == "/C" {
		attr.CmdLine = `cmd.exe /D /S /C ""` + cmd.Args[3] + `""`
	}
	cmd.SysProcAttr = attr
}

// killTree terminates the child and everything it started.
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	if err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

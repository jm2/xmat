// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func mkdirPrivate(path string) error { return os.Mkdir(path, 0o700) }

// checkPrivate requires the directory (following a symlink, if the root is
// one) to be owned by us and not writable by group or others.
func checkPrivate(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("not a directory")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("it is owned by uid %d, not by you (uid %d)", st.Uid, os.Geteuid())
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("it is writable by group or others (mode %04o); fix with: chmod go-w %s", perm, path)
	}
	return nil
}

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnixPrivateRoot(t *testing.T) {
	base := t.TempDir()

	fresh := filepath.Join(base, "fresh")
	if err := ensureRoot(fresh); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o700 {
		t.Errorf("new root has mode %04o, want 0700", fi.Mode().Perm())
	}

	shared := filepath.Join(base, "shared")
	must(t, os.Mkdir(shared, 0o700))
	must(t, os.Chmod(shared, 0o777))
	if err := ensureRoot(shared); err == nil {
		t.Error("world-writable root accepted")
	}

	readable := filepath.Join(base, "readable")
	must(t, os.Mkdir(readable, 0o755))
	if err := ensureRoot(readable); err != nil {
		t.Errorf("0755 root (readable, not writable by others) rejected: %v", err)
	}

	link := filepath.Join(base, "link")
	must(t, os.Symlink(fresh, link))
	if err := ensureRoot(link); err != nil {
		t.Errorf("symlink to a private root rejected: %v", err)
	}

	if os.Geteuid() == 0 {
		theirs := filepath.Join(base, "theirs")
		must(t, os.Mkdir(theirs, 0o700))
		must(t, os.Chown(theirs, 54321, 54321))
		if err := ensureRoot(theirs); err == nil {
			t.Error("root owned by another user accepted")
		}
	}
}

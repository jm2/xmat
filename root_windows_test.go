// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

//go:build windows

package main

import (
	"path/filepath"
	"testing"
)

func TestWindowsPrivateRoot(t *testing.T) {
	base := t.TempDir()

	fresh := filepath.Join(base, "fresh")
	if err := ensureRoot(fresh); err != nil {
		t.Fatalf("ensureRoot on a new directory: %v", err)
	}
	// A subdirectory created normally inherits the protected ACL.
	if err := ensureRoot(filepath.Join(fresh, "nested")); err != nil {
		t.Fatalf("inherited ACL rejected: %v", err)
	}

	// Everyone (WD) with modify rights, directly and inherit-only.
	for name, sddl := range map[string]string{
		"everyone":     "D:P(A;OICI;FA;;;SY)(A;OICI;0x1301bf;;;WD)",
		"inherit-only": "D:P(A;OICI;FA;;;SY)(A;OICIIO;0x1301bf;;;AU)",
	} {
		dir := filepath.Join(base, name)
		if err := mkdirSDDL(dir, sddl); err != nil {
			t.Fatal(err)
		}
		if err := ensureRoot(dir); err == nil {
			t.Errorf("%s: a root others can write to was accepted", name)
		}
	}

	// Read-only access for others is fine.
	ro := filepath.Join(base, "readonly")
	if err := mkdirSDDL(ro, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FR;;;WD)"); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivate(ro); err != nil {
		t.Errorf("read-only access for Everyone rejected: %v", err)
	}
}

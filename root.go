// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The host workspace holds everything that later runs as the user: the git
// mirror (whose hooks and config git executes), step scripts, worktrees and
// the desktop spool. If anyone else can write there, they can run code as
// the user, and on Windows as the elevated token OpenSSH gives admin logins.
// ensureRoot therefore creates the root private to the user and refuses to use
// an existing root that others can write to.
func ensureRoot(root string) error {
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
			return err
		}
		if err := mkdirPrivate(root); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("creating workspace root %s: %w", root, err)
		}
	} else if err != nil {
		return err
	}
	if err := checkPrivate(root); err != nil {
		return fmt.Errorf("refusing to use workspace root %s: %w", root, err)
	}
	return nil
}

// within reports whether path is inside base (after cleaning, no "..").
func within(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && filepath.IsLocal(rel)
}

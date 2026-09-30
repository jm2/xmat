// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

//go:build !linux

package main

// Only Linux has child subreapers; elsewhere a process that escapes its
// process group with setsid() can outlive a cancelled step.

func setSubreaper() {}

func killOrphans(except int) {}

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Processes started over SSH do not run in the logged-in desktop session:
// on Windows they cannot create windows or swapchains on the interactive
// desktop, and on macOS they sit outside the Aqua session. Steps marked
// `desktop = true` are therefore handed to `xmat desktop`, a small agent the
// user starts at login inside their graphical session. The two talk through a
// spool directory under the host root: job files in, done files out. It is
// plain files, so it works the same on every OS and survives either side
// restarting.

const (
	heartbeatEvery = 2 * time.Second
	heartbeatStale = 15 * time.Second
)

type deskJob struct {
	ID         string   `json:"id"`
	Argv       []string `json:"argv"`
	Dir        string   `json:"dir"`
	Env        []string `json:"env"` // added to the agent's own environment
	Log        string   `json:"log"`
	TimeoutSec int64    `json:"timeout_sec"`
}

type deskDone struct {
	Exit  int    `json:"exit"`
	Error string `json:"error,omitempty"`
}

type heartbeat struct {
	PID     int       `json:"pid"`
	Time    time.Time `json:"time"`
	Version string    `json:"version"`
}

func spoolDir(root string) string { return filepath.Join(root, "desktop") }

// desktopStatus reports whether a desktop agent is serving root.
func desktopStatus(root string) string {
	data, err := os.ReadFile(filepath.Join(spoolDir(root), "heartbeat.json"))
	if err != nil {
		return "not running"
	}
	var hb heartbeat
	if json.Unmarshal(data, &hb) != nil {
		return "not running"
	}
	if time.Since(hb.Time) > heartbeatStale {
		return "stale"
	}
	return "running"
}

var errNoDesktop = errors.New("no `xmat desktop` agent is running in this host's graphical session " +
	"(start it at login; `xmat desktop --print-autostart` shows how)")

// runDesktop hands a step to the desktop agent and waits for it.
func runDesktop(ctx context.Context, root string, argv []string, dir string, env []string, logPath string, timeout time.Duration) (int, error) {
	if desktopStatus(root) != "running" {
		return -1, errNoDesktop
	}
	spool := spoolDir(root)
	id := randomID()
	job := deskJob{ID: id, Argv: argv, Dir: dir, Env: env, Log: logPath, TimeoutSec: int64(timeout / time.Second)}
	data, _ := json.Marshal(job)
	if err := writeFileAtomic(filepath.Join(spool, id+".job"), data); err != nil {
		return -1, err
	}
	donePath := filepath.Join(spool, id+".done")
	cancelled := false
	var cancelAt time.Time
	ctxDone := ctx.Done()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if data, err := os.ReadFile(donePath); err == nil {
			os.Remove(donePath)
			var d deskDone
			if err := json.Unmarshal(data, &d); err != nil {
				return -1, fmt.Errorf("desktop agent: bad reply: %v", err)
			}
			if cancelled {
				return d.Exit, ctx.Err()
			}
			if d.Error != "" {
				return d.Exit, errors.New("desktop agent: " + d.Error)
			}
			return d.Exit, nil
		}
		select {
		case <-ctxDone:
			ctxDone = nil // handled; keep polling for the agent's reply
			cancelled, cancelAt = true, time.Now()
			_ = os.WriteFile(filepath.Join(spool, id+".cancel"), nil, 0o600)
			// A job the agent never picked up can simply be withdrawn.
			if os.Remove(filepath.Join(spool, id+".job")) == nil {
				os.Remove(filepath.Join(spool, id+".cancel"))
				return -1, ctx.Err()
			}
		case <-tick.C:
			if cancelled && time.Since(cancelAt) > 20*time.Second {
				return -1, ctx.Err()
			}
			if !cancelled && desktopStatus(root) != "running" {
				return -1, errors.New("desktop agent stopped responding while the step was running")
			}
		}
	}
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b)
}

// cmdDesktop runs the desktop-session agent.
func cmdDesktop(args []string) int {
	fs := flag.NewFlagSet("desktop", flag.ContinueOnError)
	root := fs.String("root", "", "host workspace root (default ~/xmat; must match the fleet's root for this target)")
	autostart := fs.Bool("print-autostart", false, "print how to start the agent at login on this OS, then exit")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: xmat desktop [--root DIR] [--print-autostart]\n\nRuns steps marked desktop = true inside this graphical session. Start it at login.")
		fs.PrintDefaults()
	}
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	r, err := hostRoot(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xmat desktop:", err)
		return 1
	}
	if *autostart {
		printAutostart(*root)
		return 0
	}
	spool := spoolDir(r)
	if err := os.MkdirAll(spool, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "xmat desktop:", err)
		return 1
	}
	lock, err := openLock(filepath.Join(spool, "agent.lock"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "xmat desktop:", err)
		return 1
	}
	if ok, err := lock.tryLock(); err != nil || !ok {
		fmt.Fprintln(os.Stderr, "xmat desktop: another agent is already serving", spool)
		return 1
	}
	defer lock.close()
	fmt.Fprintf(os.Stderr, "xmat desktop %s: serving %s\n", version, spool)

	beat := func() {
		data, _ := json.Marshal(heartbeat{PID: os.Getpid(), Time: time.Now(), Version: version})
		_ = writeFileAtomic(filepath.Join(spool, "heartbeat.json"), data)
	}
	beat()
	defer os.Remove(filepath.Join(spool, "heartbeat.json"))
	ctx, stop := signalContext()
	defer stop()

	lastBeat := time.Now()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "xmat desktop: stopping")
			return 0
		case <-tick.C:
		}
		if time.Since(lastBeat) >= heartbeatEvery {
			beat()
			lastBeat = time.Now()
		}
		entries, err := os.ReadDir(spool)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".job") {
				continue
			}
			id := strings.TrimSuffix(name, ".job")
			claimed := filepath.Join(spool, id+".claimed")
			if os.Rename(filepath.Join(spool, name), claimed) != nil {
				continue // withdrawn or taken
			}
			go serveDesktopJob(ctx, spool, id, claimed)
		}
	}
}

func serveDesktopJob(ctx context.Context, spool, id, claimed string) {
	finish := func(d deskDone) {
		data, _ := json.Marshal(d)
		_ = writeFileAtomic(filepath.Join(spool, id+".done"), data)
		os.Remove(claimed)
		os.Remove(filepath.Join(spool, id+".cancel"))
	}
	data, err := os.ReadFile(claimed)
	if err != nil {
		finish(deskDone{Exit: -1, Error: err.Error()})
		return
	}
	var job deskJob
	if err := json.Unmarshal(data, &job); err != nil || len(job.Argv) == 0 {
		finish(deskDone{Exit: -1, Error: "malformed job"})
		return
	}
	fmt.Fprintf(os.Stderr, "xmat desktop: running %s in %s\n", strings.Join(job.Argv, " "), job.Dir)
	// The host enforces the step timeout and cancels us; this is a backstop in
	// case the host disappears.
	timeout := time.Duration(job.TimeoutSec)*time.Second + 30*time.Second
	if job.TimeoutSec <= 0 {
		timeout = time.Hour
	}
	jctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	go func() {
		for jctx.Err() == nil {
			if _, err := os.Stat(filepath.Join(spool, id+".cancel")); err == nil {
				cancel()
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	}()
	code, err := execLogged(jctx, job.Argv, job.Dir, append(os.Environ(), job.Env...), job.Log)
	d := deskDone{Exit: code}
	if err != nil {
		d.Error = err.Error()
	}
	finish(d)
}

func printAutostart(root string) {
	exe, err := os.Executable()
	if err != nil {
		exe = "xmat"
	}
	rootArg := ""
	if root != "" {
		rootArg = " --root " + root
	}
	switch runtime.GOOS {
	case "windows":
		fmt.Printf(`Register a logon task that runs in your interactive session (run once, in a normal shell):

  schtasks /Create /F /TN "xmat desktop" /SC ONLOGON /RL LIMITED /TR "\"%s\" desktop%s"

Start it now without logging out:

  schtasks /Run /TN "xmat desktop"

For unattended hosts, enable auto-logon (e.g. Sysinternals Autologon) so the
session exists after a reboot.
`, exe, rootArg)
	case "darwin":
		args := fmt.Sprintf("    <string>%s</string>\n    <string>desktop</string>\n", exe)
		if root != "" {
			args += fmt.Sprintf("    <string>--root</string>\n    <string>%s</string>\n", root)
		}
		fmt.Printf(`Save as ~/Library/LaunchAgents/io.github.jm2.xmat-desktop.plist:

<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>io.github.jm2.xmat-desktop</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>LimitLoadToSessionType</key><string>Aqua</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/tmp/xmat-desktop.log</string>
</dict>
</plist>

Then load it:

  launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.github.jm2.xmat-desktop.plist

Grant Screen Recording (and Accessibility, if your smoke tests need it) to the
terminal or binary that runs your tests, or screenshots come back black.
`, args)
	default:
		fmt.Printf(`Save as ~/.config/systemd/user/xmat-desktop.service:

[Unit]
Description=xmat desktop-session agent
PartOf=graphical-session.target
After=graphical-session.target

[Service]
ExecStart=%s desktop%s
Restart=on-failure

[Install]
WantedBy=graphical-session.target

Then enable it:

  systemctl --user daemon-reload
  systemctl --user enable --now xmat-desktop.service

Your desktop environment must export DISPLAY/WAYLAND_DISPLAY to the systemd
user manager (GNOME and KDE do). Without systemd, add "%s desktop%s" to your
session's autostart instead.
`, exe, rootArg, exe, rootArg)
	}
}

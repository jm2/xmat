// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

package main

// Wire protocol between the controller (`xmat run`) and hosts (`xmat host`).
//
// The controller starts `xmat host` on each target (over ssh, or directly for
// local targets), writes one JSON Job line to its stdin and keeps stdin open.
// The host answers with JSON event lines on stdout, each starting with
// {"xmat":1, ...}; anything else on stdout (shell banners, MOTD) is ignored.
// When stdin reaches EOF the host cancels whatever is running, so closing the
// connection or pressing Ctrl-C on the controller stops remote builds.

// Proto is the protocol version. Hosts refuse jobs with a newer version.
const Proto = 1

// Job asks a host to build one commit for one target.
type Job struct {
	Proto    int      `json:"proto"`
	Repo     string   `json:"repo"`
	SHA      string   `json:"sha"`
	Target   string   `json:"target"`
	RunID    string   `json:"run_id"`
	HubFetch string   `json:"hub_fetch"`
	Root     string   `json:"root,omitempty"`
	Steps    []string `json:"steps,omitempty"` // explicit selection; empty = the target's default steps
	With     []string `json:"with,omitempty"`  // extra steps added to the defaults
	WantOS   string   `json:"want_os,omitempty"`
	WantArch string   `json:"want_arch,omitempty"`
}

// Event is one line of host output.
type Event struct {
	Xmat    int           `json:"xmat"`
	T       string        `json:"t"` // hello, phase, step_start, step_end, result, error
	Version string        `json:"version,omitempty"`
	OS      string        `json:"os,omitempty"`
	Arch    string        `json:"arch,omitempty"`
	Host    string        `json:"host,omitempty"`
	Msg     string        `json:"msg,omitempty"`
	Step    *StepResult   `json:"step,omitempty"`
	Result  *TargetResult `json:"result,omitempty"`
}

// Step statuses.
const (
	StOK        = "ok"
	StFail      = "fail"
	StTimeout   = "timeout"
	StCancelled = "cancelled"
	StSkip      = "skip"
	StNotRun    = "not_run"
	StError     = "error" // target-level: could not run at all
	StRunning   = "running"
)

// StepResult is the outcome of one step on one target.
type StepResult struct {
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Exit       int      `json:"exit"`
	DurationMS int64    `json:"duration_ms"`
	Log        string   `json:"log,omitempty"` // relative to the run directory
	FirstError string   `json:"first_error,omitempty"`
	Tail       []string `json:"tail,omitempty"`
	Artifacts  []string `json:"artifacts,omitempty"` // relative to the run directory
	Desktop    bool     `json:"desktop,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// TargetResult is the outcome of one job.
type TargetResult struct {
	Target     string       `json:"target"`
	Host       string       `json:"host"`
	Status     string       `json:"status"` // ok, fail, error
	Error      string       `json:"error,omitempty"`
	OS         string       `json:"os,omitempty"`
	Arch       string       `json:"arch,omitempty"`
	Hostname   string       `json:"hostname,omitempty"`
	Version    string       `json:"version,omitempty"`
	RunDir     string       `json:"run_dir,omitempty"`
	DurationMS int64        `json:"duration_ms"`
	Steps      []StepResult `json:"steps,omitempty"`
}

// FileRequest asks a host for a file from a run directory (`xmat host-file`).
type FileRequest struct {
	Proto  int    `json:"proto"`
	Root   string `json:"root,omitempty"`
	Repo   string `json:"repo"`
	RunID  string `json:"run_id"`
	Target string `json:"target"`
	Path   string `json:"path"`
	Tail   int    `json:"tail,omitempty"` // last N lines; 0 = whole file
}

// InfoRequest asks a host to describe itself (`xmat host-info`).
type InfoRequest struct {
	Proto    int    `json:"proto"`
	Root     string `json:"root,omitempty"`
	HubFetch string `json:"hub_fetch,omitempty"`
}

// HostInfo is the reply to an InfoRequest.
type HostInfo struct {
	Version    string            `json:"version"`
	Proto      int               `json:"proto"`
	OS         string            `json:"os"`
	Arch       string            `json:"arch"`
	Hostname   string            `json:"hostname"`
	Git        string            `json:"git"`
	Root       string            `json:"root"`
	RootOK     bool              `json:"root_ok"`
	RootErr    string            `json:"root_err,omitempty"`
	Hub        string            `json:"hub,omitempty"` // "ok", "missing" (reachable, repo not created yet) or an error
	Desktop    string            `json:"desktop"`       // "running", "stale", "not running"
	Shells     map[string]string `json:"shells"`
	ProtoError string            `json:"proto_error,omitempty"`
}

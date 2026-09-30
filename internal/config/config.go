// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

// Package config loads xmat's two config files: the per-user fleet file
// (which targets exist and how to reach them) and the per-repo .xmat.toml
// (what to run on each target). The repo file is read from the commit being
// built, so steps are versioned with the code.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jm2/xmat/internal/tomlx"
)

// RepoFile is the name of the per-repo config file.
const RepoFile = ".xmat.toml"

// DefaultStepTimeout applies when a step sets no timeout.
const DefaultStepTimeout = 60 * time.Minute

// DefaultKeepRuns is how many run directories a host keeps per repo.
const DefaultKeepRuns = 20

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidName reports whether s is usable as a target, step or repo name.
func ValidName(s string) bool { return nameRE.MatchString(s) && !strings.Contains(s, "..") }

// ---------- fleet ----------

// Fleet is the per-user description of build hosts.
type Fleet struct {
	Path     string
	HubPush  string   // URL/path the controller pushes to; may contain {repo}
	HubFetch string   // URL hosts fetch from; defaults to HubPush
	SSH      []string // ssh command prefix
	Targets  map[string]*Target
	Order    []string // declaration order of targets
}

// Target is one build target in the fleet.
type Target struct {
	Name     string
	Host     string // ssh destination
	Local    bool   // run on this machine without ssh
	Xmat     string // xmat command on the host
	Root     string // host workspace root ("" = host default, ~/xmat)
	HubFetch string // per-target override of hub.fetch
	OS       string // expected GOOS, verified against the host ("" = any)
	Arch     string // expected GOARCH ("" = any)
}

// DefaultSSH is used when the fleet file sets no [ssh] command. It disables
// agent, X11 and port forwarding: build steps run code from the commit under
// test, and must not be able to borrow the controller's ssh agent to reach
// other machines.
var DefaultSSH = []string{"ssh", "-T", "-a", "-x", "-o", "ClearAllForwardings=yes",
	"-o", "BatchMode=yes", "-o", "ConnectTimeout=15"}

// FleetSearchPaths lists where the fleet file is looked for, in order.
func FleetSearchPaths() []string {
	var paths []string
	if p := os.Getenv("XMAT_FLEET"); p != "" {
		paths = append(paths, p)
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		paths = append(paths, filepath.Join(x, "xmat", "fleet.toml"))
	}
	if h, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(h, ".config", "xmat", "fleet.toml"))
	}
	if d, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(d, "xmat", "fleet.toml"))
	}
	return dedup(paths)
}

// DefaultFleetPath is where `xmat init --fleet` writes a new fleet file.
func DefaultFleetPath() string {
	if p := os.Getenv("XMAT_FLEET"); p != "" {
		return p
	}
	if runtime.GOOS == "windows" {
		if d, err := os.UserConfigDir(); err == nil {
			return filepath.Join(d, "xmat", "fleet.toml")
		}
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "xmat", "fleet.toml")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "xmat", "fleet.toml")
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// FindFleet loads the fleet file from explicit (if set) or the search path.
func FindFleet(explicit string) (*Fleet, error) {
	if explicit != "" {
		return LoadFleet(explicit)
	}
	for _, p := range FleetSearchPaths() {
		if _, err := os.Stat(p); err == nil {
			return LoadFleet(p)
		}
	}
	return nil, fmt.Errorf("no fleet config found (looked in %s); create one with `xmat init --fleet`",
		strings.Join(FleetSearchPaths(), ", "))
}

// LoadFleet parses a fleet file.
func LoadFleet(path string) (*Fleet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := ParseFleet(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f.Path = path
	return f, nil
}

// ParseFleet parses fleet file contents.
func ParseFleet(src string) (*Fleet, error) {
	root, err := tomlx.Parse(src)
	if err != nil {
		return nil, err
	}
	v := view(root, "")
	f := &Fleet{Targets: map[string]*Target{}, SSH: DefaultSSH}

	if hub, ok, err := v.table("hub"); err != nil {
		return nil, err
	} else if ok {
		if f.HubPush, _, err = hub.str("push"); err != nil {
			return nil, err
		}
		if f.HubFetch, _, err = hub.str("fetch"); err != nil {
			return nil, err
		}
		if err := hub.done(); err != nil {
			return nil, err
		}
	}
	if f.HubPush == "" {
		return nil, errors.New("[hub] push is required (e.g. push = \"ssh://you@buildhost/srv/xmat/{repo}.git\")")
	}
	if f.HubFetch == "" {
		f.HubFetch = f.HubPush
	}
	for _, u := range []string{f.HubPush, f.HubFetch} {
		if err := checkArg("hub URL", u); err != nil {
			return nil, err
		}
	}

	if s, ok, err := v.table("ssh"); err != nil {
		return nil, err
	} else if ok {
		cmd, set, err := s.strList("command")
		if err != nil {
			return nil, err
		}
		if set {
			if len(cmd) == 0 {
				return nil, errors.New("[ssh] command must not be empty")
			}
			f.SSH = cmd
		}
		if err := s.done(); err != nil {
			return nil, err
		}
	}

	tt, ok, err := v.table("target")
	if err != nil {
		return nil, err
	}
	if !ok || len(tt.t.Keys()) == 0 {
		return nil, errors.New("no targets defined; add at least one [target.<name>] table")
	}
	for _, name := range tt.t.Keys() {
		if !ValidName(name) {
			return nil, fmt.Errorf("invalid target name %q (use letters, digits, '.', '_', '-')", name)
		}
		tv, _, err := tt.table(name)
		if err != nil {
			return nil, err
		}
		t := &Target{Name: name, Xmat: "xmat"}
		if t.Host, _, err = tv.str("host"); err != nil {
			return nil, err
		}
		if t.Local, _, err = tv.boolean("local"); err != nil {
			return nil, err
		}
		if x, set, err := tv.str("xmat"); err != nil {
			return nil, err
		} else if set {
			t.Xmat = x
		}
		if t.Root, _, err = tv.str("root"); err != nil {
			return nil, err
		}
		if t.HubFetch, _, err = tv.str("hub_fetch"); err != nil {
			return nil, err
		}
		if t.OS, _, err = tv.str("os"); err != nil {
			return nil, err
		}
		t.OS = NormalizeOS(t.OS)
		if t.Arch, _, err = tv.str("arch"); err != nil {
			return nil, err
		}
		if err := tv.done(); err != nil {
			return nil, err
		}
		for what, val := range map[string]string{"host": t.Host, "hub_fetch": t.HubFetch, "xmat": t.Xmat} {
			if err := checkArg(fmt.Sprintf("[target.%s] %s", name, what), val); err != nil {
				return nil, err
			}
		}
		if !t.Local && t.Host == "" {
			return nil, fmt.Errorf("[target.%s] needs host = \"<ssh destination>\" (or local = true)", name)
		}
		if t.Local && t.Host == "" {
			t.Host = "localhost"
		}
		f.Targets[name] = t
		f.Order = append(f.Order, name)
	}
	if err := v.done(); err != nil {
		return nil, err
	}
	return f, nil
}

// checkArg rejects values that git or ssh would parse as options.
func checkArg(what, v string) error {
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("%s %q must not start with '-'", what, v)
	}
	if strings.ContainsAny(v, "\x00\n\r") {
		return fmt.Errorf("%s contains a control character", what)
	}
	return nil
}

// HubFetchFor returns the URL target t fetches repo from.
func (f *Fleet) HubFetchFor(t *Target, repo string) string {
	u := f.HubFetch
	if t.HubFetch != "" {
		u = t.HubFetch
	}
	return strings.ReplaceAll(u, "{repo}", repo)
}

// HubPushFor returns the URL the controller pushes repo to.
func (f *Fleet) HubPushFor(repo string) string { return strings.ReplaceAll(f.HubPush, "{repo}", repo) }

// NormalizeOS maps friendly OS names onto GOOS values.
func NormalizeOS(s string) string {
	switch strings.ToLower(s) {
	case "macos", "mac", "osx":
		return "darwin"
	case "win", "win32", "win64":
		return "windows"
	}
	return strings.ToLower(s)
}

// ---------- repo ----------

// Settings are target-level knobs that can be set globally, per OS or per target.
type Settings struct {
	Env        map[string]string
	Clean      *bool
	Submodules *bool
	Skip       *bool
}

// StepFields holds the optional fields of one step layer.
type StepFields struct {
	Run       *string
	Shell     *string
	Dir       *string
	Timeout   *time.Duration
	Default   *bool
	Desktop   *bool
	Skip      *bool
	Env       map[string]string
	Artifacts []string
	artifacts bool // Artifacts was set
}

// StepDef is a step with its per-OS and per-target overrides.
type StepDef struct {
	Name   string
	Base   StepFields
	OS     map[string]StepFields
	Target map[string]StepFields
}

// Repo is a parsed .xmat.toml.
type Repo struct {
	Name     string
	Targets  []string
	KeepRuns int
	Base     Settings
	OS       map[string]Settings
	Target   map[string]Settings
	Steps    []*StepDef
}

// Step is a fully resolved step for one target.
type Step struct {
	Name    string
	Run     string
	Shell   string
	Dir     string
	Timeout time.Duration
	Default bool
	Desktop bool
	Skip    bool
	// SkipReason says why Skip is set.
	SkipReason string
	Env        map[string]string
	Artifacts  []string
}

// Resolved is the repo config resolved for one target.
type Resolved struct {
	Env        map[string]string
	Clean      bool
	Submodules *bool // nil = auto (use submodules if .gitmodules exists)
	Skip       bool
	Steps      []Step
}

// StepNames returns all step names in order.
func (r *Repo) StepNames() []string {
	var out []string
	for _, s := range r.Steps {
		out = append(out, s.Name)
	}
	return out
}

// Step returns the step definition called name.
func (r *Repo) Step(name string) *StepDef {
	for _, s := range r.Steps {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// DefaultSteps returns the names of steps that run unless a selection is given.
// A step counts as default when no layer disables it for any target, so the
// controller treats per-target default overrides conservatively; the host
// makes the final per-target call.
func (r *Repo) DefaultSteps() []string {
	var out []string
	for _, s := range r.Steps {
		if s.Base.Default == nil || *s.Base.Default {
			out = append(out, s.Name)
		}
	}
	return out
}

// ParseRepo parses .xmat.toml contents.
func ParseRepo(src string) (*Repo, error) {
	root, err := tomlx.Parse(src)
	if err != nil {
		return nil, err
	}
	v := view(root, "")
	r := &Repo{KeepRuns: DefaultKeepRuns, OS: map[string]Settings{}, Target: map[string]Settings{}}
	if r.Name, _, err = v.str("name"); err != nil {
		return nil, err
	}
	if r.Name != "" && !ValidName(r.Name) {
		return nil, fmt.Errorf("invalid name %q (use letters, digits, '.', '_', '-')", r.Name)
	}
	if r.Targets, _, err = v.strList("targets"); err != nil {
		return nil, err
	}
	if n, set, err := v.integer("keep_runs"); err != nil {
		return nil, err
	} else if set {
		if n < 1 {
			return nil, errors.New("keep_runs must be at least 1")
		}
		r.KeepRuns = int(n)
	}
	if r.Base, err = settings(v, false); err != nil {
		return nil, err
	}
	for _, kind := range []string{"os", "target"} {
		tv, ok, err := v.table(kind)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		for _, k := range tv.t.Keys() {
			sv, _, err := tv.table(k)
			if err != nil {
				return nil, err
			}
			s, err := settings(sv, true)
			if err != nil {
				return nil, err
			}
			if err := sv.done(); err != nil {
				return nil, err
			}
			if kind == "os" {
				r.OS[NormalizeOS(k)] = s
			} else {
				r.Target[k] = s
			}
		}
		if err := tv.done(); err != nil {
			return nil, err
		}
	}

	sv, ok, err := v.table("step")
	if err != nil {
		return nil, err
	}
	if !ok || len(sv.t.Keys()) == 0 {
		return nil, errors.New("no steps defined; add at least one [step.<name>] table")
	}
	for _, name := range sv.t.Keys() {
		if !ValidName(name) {
			return nil, fmt.Errorf("invalid step name %q (use letters, digits, '.', '_', '-')", name)
		}
		stv, _, err := sv.table(name)
		if err != nil {
			return nil, err
		}
		def := &StepDef{Name: name, OS: map[string]StepFields{}, Target: map[string]StepFields{}}
		if def.Base, err = stepFields(stv); err != nil {
			return nil, err
		}
		for _, kind := range []string{"os", "target"} {
			kv, ok, err := stv.table(kind)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			for _, k := range kv.t.Keys() {
				ov, _, err := kv.table(k)
				if err != nil {
					return nil, err
				}
				fields, err := stepFields(ov)
				if err != nil {
					return nil, err
				}
				if err := ov.done(); err != nil {
					return nil, err
				}
				if kind == "os" {
					def.OS[NormalizeOS(k)] = fields
				} else {
					def.Target[k] = fields
				}
			}
			if err := kv.done(); err != nil {
				return nil, err
			}
		}
		if err := stv.done(); err != nil {
			return nil, err
		}
		if def.Base.Run == nil && len(def.OS) == 0 && len(def.Target) == 0 {
			return nil, fmt.Errorf("[step.%s] has no run command", name)
		}
		r.Steps = append(r.Steps, def)
	}
	if err := sv.done(); err != nil {
		return nil, err
	}
	if err := v.done(); err != nil {
		return nil, err
	}
	return r, nil
}

func settings(v *tview, allowSkip bool) (Settings, error) {
	var s Settings
	var err error
	if s.Env, _, err = v.strMap("env"); err != nil {
		return s, err
	}
	if s.Clean, err = v.boolPtr("clean"); err != nil {
		return s, err
	}
	if s.Submodules, err = v.boolPtr("submodules"); err != nil {
		return s, err
	}
	if allowSkip {
		if s.Skip, err = v.boolPtr("skip"); err != nil {
			return s, err
		}
	}
	return s, nil
}

var validShells = map[string]bool{"sh": true, "bash": true, "cmd": true, "pwsh": true, "powershell": true}

func stepFields(v *tview) (StepFields, error) {
	var f StepFields
	var err error
	str := func(key string) (*string, error) {
		s, set, err := v.str(key)
		if err != nil || !set {
			return nil, err
		}
		return &s, nil
	}
	if f.Run, err = str("run"); err != nil {
		return f, err
	}
	if f.Shell, err = str("shell"); err != nil {
		return f, err
	}
	if f.Shell != nil && !validShells[*f.Shell] {
		return f, fmt.Errorf("%s: shell %q is not one of sh, bash, cmd, pwsh, powershell", v.path, *f.Shell)
	}
	if f.Dir, err = str("dir"); err != nil {
		return f, err
	}
	if f.Dir != nil {
		if err := checkRel(*f.Dir); err != nil {
			return f, fmt.Errorf("%s.dir: %w", v.path, err)
		}
	}
	if d, set, err := v.duration("timeout"); err != nil {
		return f, err
	} else if set {
		f.Timeout = &d
	}
	if f.Default, err = v.boolPtr("default"); err != nil {
		return f, err
	}
	if f.Desktop, err = v.boolPtr("desktop"); err != nil {
		return f, err
	}
	if f.Skip, err = v.boolPtr("skip"); err != nil {
		return f, err
	}
	if f.Env, _, err = v.strMap("env"); err != nil {
		return f, err
	}
	if f.Artifacts, f.artifacts, err = v.strList("artifacts"); err != nil {
		return f, err
	}
	for _, a := range f.Artifacts {
		if err := checkRel(a); err != nil {
			return f, fmt.Errorf("%s.artifacts: %q: %w", v.path, a, err)
		}
	}
	return f, nil
}

func checkRel(p string) error {
	if p == "" {
		return errors.New("empty path")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || (len(p) > 1 && p[1] == ':') {
		return errors.New("must be relative to the repository root")
	}
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return errors.New("must not contain '..'")
		}
	}
	return nil
}

func (dst *StepFields) apply(src StepFields) {
	if src.Run != nil {
		dst.Run = src.Run
	}
	if src.Shell != nil {
		dst.Shell = src.Shell
	}
	if src.Dir != nil {
		dst.Dir = src.Dir
	}
	if src.Timeout != nil {
		dst.Timeout = src.Timeout
	}
	if src.Default != nil {
		dst.Default = src.Default
	}
	if src.Desktop != nil {
		dst.Desktop = src.Desktop
	}
	if src.Skip != nil {
		dst.Skip = src.Skip
	}
	if src.artifacts {
		dst.Artifacts, dst.artifacts = src.Artifacts, true
	}
	if len(src.Env) > 0 {
		m := map[string]string{}
		for k, v := range dst.Env {
			m[k] = v
		}
		for k, v := range src.Env {
			m[k] = v
		}
		dst.Env = m
	}
}

func (dst *Settings) apply(src Settings) {
	if src.Clean != nil {
		dst.Clean = src.Clean
	}
	if src.Submodules != nil {
		dst.Submodules = src.Submodules
	}
	if src.Skip != nil {
		dst.Skip = src.Skip
	}
	if len(src.Env) > 0 {
		m := map[string]string{}
		for k, v := range dst.Env {
			m[k] = v
		}
		for k, v := range src.Env {
			m[k] = v
		}
		dst.Env = m
	}
}

// DefaultShell is the shell used when a step names none.
func DefaultShell(goos string) string {
	if goos == "windows" {
		return "cmd"
	}
	return "sh"
}

// Resolve layers base, per-OS and per-target settings for one target.
func (r *Repo) Resolve(goos, target string) (*Resolved, error) {
	goos = NormalizeOS(goos)
	s := Settings{}
	s.apply(r.Base)
	s.apply(r.OS[goos])
	s.apply(r.Target[target])
	out := &Resolved{Env: s.Env, Submodules: s.Submodules}
	if s.Clean != nil {
		out.Clean = *s.Clean
	}
	if s.Skip != nil {
		out.Skip = *s.Skip
	}
	for _, def := range r.Steps {
		f := StepFields{}
		f.apply(def.Base)
		f.apply(def.OS[goos])
		f.apply(def.Target[target])
		st := Step{Name: def.Name, Timeout: DefaultStepTimeout, Default: true, Env: f.Env, Artifacts: f.Artifacts}
		if f.Skip != nil && *f.Skip {
			st.Skip, st.SkipReason = true, "skip = true"
		}
		if f.Run != nil {
			st.Run = *f.Run
		} else if !st.Skip {
			// A step that only defines a command for some OSes or targets is
			// skipped everywhere else.
			st.Skip, st.SkipReason = true, "no run command for this platform"
		}
		st.Shell = DefaultShell(goos)
		if f.Shell != nil {
			st.Shell = *f.Shell
		}
		if goos != "windows" && (st.Shell == "cmd" || st.Shell == "powershell") && !st.Skip {
			return nil, fmt.Errorf("step %q uses shell %q, which only exists on windows (target %s is %s)", def.Name, st.Shell, target, goos)
		}
		if f.Dir != nil {
			st.Dir = *f.Dir
		}
		if f.Timeout != nil {
			st.Timeout = *f.Timeout
		}
		if f.Default != nil {
			st.Default = *f.Default
		}
		if f.Desktop != nil {
			st.Desktop = *f.Desktop
		}
		out.Steps = append(out.Steps, st)
	}
	return out, nil
}

// ---------- typed views over tomlx tables ----------

type tview struct {
	t    *tomlx.Table
	path string
	used map[string]bool
}

func view(t *tomlx.Table, path string) *tview {
	return &tview{t: t, path: path, used: map[string]bool{}}
}

func (v *tview) name(key string) string {
	if v.path == "" {
		return key
	}
	return v.path + "." + key
}

func (v *tview) get(key string) (any, bool) {
	v.used[key] = true
	return v.t.Get(key)
}

func (v *tview) done() error {
	var unknown []string
	for _, k := range v.t.Keys() {
		if !v.used[k] {
			unknown = append(unknown, strconv.Quote(v.name(k)))
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("unknown key(s): %s", strings.Join(unknown, ", "))
}

func typeName(x any) string {
	switch x.(type) {
	case string:
		return "a string"
	case int64:
		return "an integer"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case *tomlx.Table:
		return "a table"
	}
	return fmt.Sprintf("%T", x)
}

func (v *tview) str(key string) (string, bool, error) {
	x, ok := v.get(key)
	if !ok {
		return "", false, nil
	}
	s, ok := x.(string)
	if !ok {
		return "", false, fmt.Errorf("%s must be a string, not %s", v.name(key), typeName(x))
	}
	return s, true, nil
}

func (v *tview) boolean(key string) (bool, bool, error) {
	x, ok := v.get(key)
	if !ok {
		return false, false, nil
	}
	b, ok := x.(bool)
	if !ok {
		return false, false, fmt.Errorf("%s must be true or false, not %s", v.name(key), typeName(x))
	}
	return b, true, nil
}

func (v *tview) boolPtr(key string) (*bool, error) {
	b, set, err := v.boolean(key)
	if err != nil || !set {
		return nil, err
	}
	return &b, nil
}

func (v *tview) integer(key string) (int64, bool, error) {
	x, ok := v.get(key)
	if !ok {
		return 0, false, nil
	}
	n, ok := x.(int64)
	if !ok {
		return 0, false, fmt.Errorf("%s must be an integer, not %s", v.name(key), typeName(x))
	}
	return n, true, nil
}

func (v *tview) duration(key string) (time.Duration, bool, error) {
	s, set, err := v.str(key)
	if err != nil || !set {
		return 0, set, err
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false, fmt.Errorf("%s: %q is not a positive duration like \"90s\", \"30m\" or \"1h30m\"", v.name(key), s)
	}
	return d, true, nil
}

func (v *tview) strList(key string) ([]string, bool, error) {
	x, ok := v.get(key)
	if !ok {
		return nil, false, nil
	}
	arr, ok := x.([]any)
	if !ok {
		return nil, false, fmt.Errorf("%s must be an array of strings, not %s", v.name(key), typeName(x))
	}
	out := make([]string, 0, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, false, fmt.Errorf("%s[%d] must be a string, not %s", v.name(key), i, typeName(e))
		}
		out = append(out, s)
	}
	return out, true, nil
}

func (v *tview) strMap(key string) (map[string]string, bool, error) {
	x, ok := v.get(key)
	if !ok {
		return nil, false, nil
	}
	t, ok := x.(*tomlx.Table)
	if !ok {
		return nil, false, fmt.Errorf("%s must be a table of strings, e.g. env = { FOO = \"bar\" }", v.name(key))
	}
	out := map[string]string{}
	for _, k := range t.Keys() {
		e, _ := t.Get(k)
		s, ok := e.(string)
		if !ok {
			return nil, false, fmt.Errorf("%s.%s must be a string, not %s", v.name(key), k, typeName(e))
		}
		out[k] = s
	}
	return out, true, nil
}

func (v *tview) table(key string) (*tview, bool, error) {
	x, ok := v.get(key)
	if !ok {
		return nil, false, nil
	}
	t, ok := x.(*tomlx.Table)
	if !ok {
		return nil, false, fmt.Errorf("%s must be a table, not %s", v.name(key), typeName(x))
	}
	return view(t, v.name(key)), true, nil
}

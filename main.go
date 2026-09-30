// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 jm2 and xmat contributors

// xmat builds and tests one commit on several machines at once and reports a
// compact pass/fail matrix. It is meant to be called by people and coding
// agents alike: the output is short, the exit status is meaningful, and full
// logs stay on the hosts until asked for.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
}

const usage = `xmat — build and test one commit on all your machines at once

usage: xmat <command> [flags] [args]

commands:
  run [<commit>]        build/test a commit on every target, print a pass/fail matrix
  log <target> [step]   show a step's log from the last run
  get <target> <path>   download an artifact or file from the last run
  doctor                check that every host is reachable and ready
  init [--fleet]        write a starter .xmat.toml (or fleet config)
  desktop               run steps marked desktop = true in this GUI session
  version               print the version

Run "xmat <command> -h" for a command's flags.
Hosts also answer "xmat host", "xmat host-file" and "xmat host-info" over
ssh; those are started by the commands above, not by hand.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitUsage)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "run", "log", "logs", "get", "doctor", "init":
		// These print text that originates on build hosts or in the commit.
		filterOutput()
	}
	var code int
	switch cmd {
	case "run":
		code = cmdRun(args)
	case "log", "logs":
		code = cmdLog(args)
	case "get":
		code = cmdGet(args)
	case "doctor":
		code = cmdDoctor(args)
	case "init":
		code = cmdInit(args)
	case "desktop":
		code = cmdDesktop(args)
	case "host":
		code = cmdHost(args)
	case "host-file":
		code = cmdHostFile(args)
	case "host-info":
		code = cmdHostInfo(args)
	case "version", "--version", "-V":
		fmt.Println("xmat", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "xmat: unknown command %q\n\n%s", sanitize(cmd), usage)
		code = exitUsage
	}
	os.Exit(code)
}

// parseInterspersed parses flags that may appear before, between or after
// positional arguments, and returns the positionals. "--" ends flag parsing.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			tail = append(tail, args[i+1:]...)
			args = args[:i]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return append(pos, tail...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// signalContext is cancelled by the first SIGINT/SIGTERM; a second one kills the process.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			fmt.Fprintln(os.Stderr, "xmat: interrupted; stopping remote steps (press again to force)")
			cancel()
		case <-ctx.Done():
			return
		}
		<-ch
		os.Exit(130)
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel()
	}
}

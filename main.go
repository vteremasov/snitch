package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"snitch/internal/dash"
	"snitch/internal/discover"
	"snitch/internal/state"
	"snitch/internal/wrapper"
)

func main() {
	if err := state.EnsureDirs(); err != nil {
		die(err)
	}

	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	sub := os.Args[1]
	args := os.Args[2:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}

	switch sub {
	case "run":
		die(wrapper.Run(ctx, args))
	case "dash":
		die(dash.Run(ctx))
	case "ls":
		die(discover.PrintLs(os.Stdout))
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "snitch: unknown subcommand %q\n", sub)
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, `snitch — watch and control claude code sessions

Usage:
  snitch run [-- claude-args...]   launch claude under a pty wrapper
  snitch dash                      open the dashboard TUI
  snitch ls                        print active wrappers and exit
  snitch help                      show this message

Files:
  ~/.snitch/sessions/<pid>.json    one entry per running wrapper
  ~/.snitch/sock/<pid>.sock        wrapper control socket
  ~/.snitch/log/<pid>.log          wrapper debug log`)
}

func die(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "snitch:", err)
	os.Exit(1)
}

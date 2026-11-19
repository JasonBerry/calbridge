// Command calbridge mirrors Google Calendar invitations into iCloud Calendar
// and propagates RSVP changes back to Google. See docs/PLAN.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// errUsage signals a command-line usage error (exit code 2).
var errUsage = errors.New("usage error")

type command struct {
	name    string
	summary string
	hidden  bool
	run     func(ctx context.Context, args []string, stdout, stderr io.Writer) error
}

func commands() []command {
	return []command{
		{name: "version", summary: "print the version", run: runVersion},
		{name: "auth", summary: "authorize the Google account (interactive, loopback OAuth)", run: runAuth},
		{name: "serve", summary: "run the sync loop and the status server", run: runServe},
		{name: "once", summary: "run one sync cycle (--dry-run: plan only)", run: runOnce},
		{name: "status", summary: "show sync status from the database", run: runStatus},
		{name: "calendars", summary: "list iCloud calendars and their hrefs", run: runCalendars},
		{name: "reconcile", summary: "compare Google, iCloud and the database; repair (--apply)", run: runReconcile},
		{name: "spike", summary: "Phase 0 experiments", hidden: true, run: runSpike},
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(stderr)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	for _, c := range commands() {
		if c.name != args[0] {
			continue
		}
		err := c.run(ctx, args[1:], stdout, stderr)
		switch {
		case err == nil:
			return 0
		case errors.Is(err, errUsage):
			return 2
		case errors.Is(err, errLocked):
			_, _ = fmt.Fprintf(stderr, "calbridge %s: %v\n", c.name, err)
			return exitLocked
		default:
			_, _ = fmt.Fprintf(stderr, "calbridge %s: %v\n", c.name, err)
			return 1
		}
	}
	_, _ = fmt.Fprintf(stderr, "calbridge: unknown command %q\n\n", args[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: calbridge <command> [flags]")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "commands:")
	for _, c := range commands() {
		if c.hidden {
			continue
		}
		_, _ = fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
}

func runVersion(_ context.Context, _ []string, stdout, _ io.Writer) error {
	_, err := fmt.Fprintf(stdout, "calbridge %s\n", version)
	return err
}

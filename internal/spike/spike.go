// Package spike holds the Phase 0 experiment commands (PLAN.md §2). They are
// throwaway-quality but repeatable, are run by the user against real accounts,
// and only write to iCloud calendars named calbridge-test*.
package spike

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/JasonBerry/calbridge/internal/config"
	"github.com/JasonBerry/calbridge/internal/secrets"
)

// ErrUsage is returned for command-line usage errors.
var ErrUsage = errors.New("usage error")

type subcommand struct {
	summary string
	run     func(ctx context.Context, e *env, args []string) error
}

func subcommands() map[string]subcommand {
	return map[string]subcommand{
		"google-list":     {"P0-1: list events (--full or --sync)", googleList},
		"google-rsvp":     {"P0-2: set own responseStatus on an event", googleRSVP},
		"caldav-discover": {"P0-3: principal, home, address set, calendars", caldavDiscover},
		"caldav-put":      {"P0-4/P0-7/P0-8: PUT a test object (--variant or --from-google or --file)", caldavPut},
		"caldav-get":      {"GET an object and print it", caldavGet},
		"caldav-sync":     {"P0-5: sync-collection / ctag report", caldavSync},
		"caldav-delete":   {"P0-9: DELETE an object (--mode)", caldavDelete},
		"caldav-volume":   {"P0-10: rate/volume sanity run", caldavVolume},
	}
}

type env struct {
	stdout, stderr io.Writer
	getenv         func(string) string
}

// Main dispatches `calbridge spike <subcommand>`.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	e := &env{stdout: stdout, stderr: stderr, getenv: os.Getenv}
	cmds := subcommands()
	if len(args) == 0 || args[0] == "-h" || args[0] == "help" {
		usage(stderr, cmds)
		return ErrUsage
	}
	c, ok := cmds[args[0]]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "unknown spike command %q\n", args[0])
		usage(stderr, cmds)
		return ErrUsage
	}
	return c.run(ctx, e, args[1:])
}

func usage(w io.Writer, cmds map[string]subcommand) {
	names := make([]string, 0, len(cmds))
	for n := range cmds {
		names = append(names, n)
	}
	sort.Strings(names)
	_, _ = fmt.Fprintln(w, "usage: calbridge spike <command> [flags]   (Phase 0 experiments, PLAN.md §2)")
	for _, n := range names {
		_, _ = fmt.Fprintf(w, "  %-16s %s\n", n, cmds[n].summary)
	}
}

// common flags for every spike command.
type common struct {
	configPath    string
	insecurePerms bool
	dumpDir       string
}

func (c *common) register(fs *flag.FlagSet) {
	def := os.Getenv("CALBRIDGE_CONFIG")
	if def == "" {
		def = "/etc/calbridge/config.yaml"
	}
	fs.StringVar(&c.configPath, "config", def, "config file (env CALBRIDGE_CONFIG)")
	fs.BoolVar(&c.insecurePerms, "insecure-perms", false, "skip secret file permission checks (development only)")
	fs.StringVar(&c.dumpDir, "dump", "dumps", "directory for raw request/response captures (git-ignored; redact before copying to docs/phase0)")
}

func (c *common) load() (config.Config, error) {
	return config.Load(c.configPath, os.Environ())
}

func newFlags(e *env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet("spike "+name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(fs.Output(), "unexpected arguments: %v\n", fs.Args())
		return ErrUsage
	}
	return nil
}

// writeDump writes data to dir/<name> with mode 0600, creating dir (0700).
func writeDump(dir, name string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	if err := secrets.WriteFileAtomic(p, data, 0o600); err != nil {
		return "", err
	}
	return p, nil
}

func stamp() string { return time.Now().UTC().Format("20060102T150405Z") }

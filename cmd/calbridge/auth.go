package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/JasonBerry/calbridge/internal/config"
	"github.com/JasonBerry/calbridge/internal/secrets"
)

// commonFlags are shared by commands that read the config file.
type commonFlags struct {
	configPath    string
	insecurePerms bool
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	def := os.Getenv("CALBRIDGE_CONFIG")
	if def == "" {
		def = "/etc/calbridge/config.yaml"
	}
	fs.StringVar(&c.configPath, "config", def, "config file (env CALBRIDGE_CONFIG)")
	fs.BoolVar(&c.insecurePerms, "insecure-perms", false, "skip secret file permission checks (development only)")
}

func (c *commonFlags) load() (config.Config, error) {
	return config.Load(c.configPath, os.Environ())
}

// parseFlags parses args and maps -h and parse errors to errUsage.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	return nil
}

func runAuth(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	port := fs.Int("port", 0, "loopback port for the OAuth redirect (0 = random; set it when tunnelling with ssh -L)")
	noBrowser := fs.Bool("no-browser", false, "do not try to open a browser")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := cf.load()
	if err != nil {
		return err
	}
	oc, err := secrets.LoadOAuthConfig(cfg.Google.ClientFile, cf.insecurePerms, cfg.Google.Scope)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}
	flow := secrets.LoopbackFlow{Config: oc, Listener: ln, Out: stdout}
	if !*noBrowser {
		flow.OpenBrowser = openBrowser
	}
	tok, err := flow.Run(ctx)
	if err != nil {
		return err
	}

	account, err := verifyAccount(ctx, oc.Client(ctx, tok), "")
	if err != nil {
		return fmt.Errorf("verify account: %w", err)
	}
	switch {
	case strings.EqualFold(account, cfg.Google.SelfEmail):
	case strings.Contains(account, "@"):
		return fmt.Errorf("authorized account %q does not match google.self_email %q; token not saved", account, cfg.Google.SelfEmail)
	default:
		_, _ = fmt.Fprintf(stderr, "warning: could not confirm the account address (primary calendar is named %q); check you signed in as %s\n", account, cfg.Google.SelfEmail)
	}
	if err := secrets.SaveToken(cfg.Google.TokenFile, tok); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "Authorized. Token saved to %s (mode 0600).\n", cfg.Google.TokenFile)
	return nil
}

// verifyAccount returns the address of the authorized account. It tries
// calendarList.get("primary") (whose id is the account email), and falls
// back to the summary of the primary calendar from events.list, because
// calendarList may need a scope broader than calendar.events.
// UNVERIFIED: P0-1 — whether calendar.events covers calendarList.get.
func verifyAccount(ctx context.Context, hc *http.Client, endpoint string) (string, error) {
	opts := []option.ClientOption{option.WithHTTPClient(hc)}
	if endpoint != "" {
		opts = append(opts, option.WithEndpoint(endpoint))
	}
	svc, err := calendar.NewService(ctx, opts...)
	if err != nil {
		return "", err
	}
	entry, err := svc.CalendarList.Get("primary").Context(ctx).Do()
	if err == nil {
		return entry.Id, nil
	}
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || (gerr.Code != http.StatusForbidden && gerr.Code != http.StatusUnauthorized) {
		return "", err
	}
	events, err := svc.Events.List("primary").MaxResults(1).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return events.Summary, nil
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url) // #nosec G204 -- fixed binary; url is our own consent URL
	case "linux":
		cmd = exec.Command("xdg-open", url) // #nosec G204 -- fixed binary; url is our own consent URL
	default:
		return fmt.Errorf("unsupported OS %s", runtime.GOOS)
	}
	return cmd.Start()
}

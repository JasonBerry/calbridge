package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/JasonBerry/calbridge/internal/server"
	"github.com/JasonBerry/calbridge/internal/store"
	"github.com/JasonBerry/calbridge/internal/syncer"
)

func newTable(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 4, 2, ' ', 0) }

// runOnce is `calbridge once [--dry-run]`: one cycle, exit code 0/1.
func runOnce(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("once", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	dryRun := fs.Bool("dry-run", false, "plan only: no remote writes, no store changes")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	a, err := openApp(ctx, &cf, stderr, true)
	if err != nil {
		return err
	}
	defer a.Close()
	a.engine.DryRun = *dryRun
	_, _ = fmt.Fprintf(stdout, "calendar %q (%s)\n", a.cal.DisplayName, a.cal.Href)
	rep, err := a.engine.Cycle(ctx)
	printReport(stdout, rep, *dryRun)
	return err
}

// runServe is `calbridge serve`: the sync loop plus the status server.
func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	a, err := openApp(ctx, &cf, stderr, true)
	if err != nil {
		return err
	}
	defer a.Close()
	interval := a.cfg.Sync.Interval.D()
	srv := server.New(version, a.cal.Href, interval, nil)
	if a.cfg.Server.Listen != "" {
		ln, err := net.Listen("tcp", a.cfg.Server.Listen)
		if err != nil {
			return fmt.Errorf("status server: %w", err)
		}
		hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = hs.Serve(ln) }()
		defer func() {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = hs.Shutdown(sctx)
		}()
	}
	a.log.InfoContext(ctx, "calbridge serving", "version", version, "calendar_href", a.cal.Href, "interval", interval.String())
	pinger := pinger{url: a.cfg.Monitoring.PingURL, client: &http.Client{Timeout: 10 * time.Second}}
	loop := serveLoop{engine: a.engine, store: a.store, srv: srv, ping: pinger.ping, log: a.log.InfoContext}
	return loop.run(ctx, interval)
}

// serveLoop runs cycles until ctx is cancelled.
type serveLoop struct {
	engine interface {
		Cycle(context.Context) (syncer.Report, error)
	}
	store *store.Store
	srv   *server.Server
	ping  func(ctx context.Context, fail bool, class string)
	log   func(ctx context.Context, msg string, args ...any)
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func (l serveLoop) run(ctx context.Context, interval time.Duration) error {
	now, sleep := l.now, l.sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = sleepCtx
	}
	for ctx.Err() == nil {
		rep, err := l.engine.Cycle(ctx)
		if ctx.Err() != nil {
			break // interrupted mid-cycle: the item in flight was finished
		}
		l.record(ctx, rep, err, now())
		switch {
		case err == nil:
			l.ping(ctx, false, "")
		case syncer.IsAuth(err):
			l.ping(ctx, true, syncer.ErrorClass(err))
			l.log(ctx, "authorization failed; fix credentials and restart", "class", syncer.ErrorClass(err))
		default:
			l.log(ctx, "cycle finished with errors; retrying next cycle", "class", syncer.ErrorClass(err), "failed", rep.Failed)
		}
		if err := sleep(ctx, jitter(interval)); err != nil {
			break
		}
	}
	return nil
}

// record updates the status server after a cycle.
func (l serveLoop) record(ctx context.Context, rep syncer.Report, err error, at time.Time) {
	counts, _ := l.store.CountMappingsByState(ctx)
	parked, pending := 0, 0
	if ms, lerr := l.store.ListMappings(ctx); lerr == nil {
		for _, m := range ms {
			if m.Parked() {
				parked++
			}
			if m.RSVPPending != "" {
				pending++
			}
		}
	}
	l.srv.Update(func(st *server.Status) {
		st.LastAttempt = at.UTC()
		st.Mappings = map[string]int{}
		for k, v := range counts {
			st.Mappings[string(k)] = v
		}
		st.Parked, st.PendingRSVPs = parked, pending
		st.LastCycle = server.CycleCounts{Created: rep.Created, Updated: rep.Updated, Adopted: rep.Adopted, Deleted: rep.Deleted,
			Unchanged: rep.Unchanged, Skipped: rep.Skipped, UserDeleted: rep.UserDeleted, Failed: rep.Failed, RSVPs: rep.RSVPs}
		if err == nil {
			st.LastSuccess, st.LastErrorClass, st.ConsecutiveFailures = at.UTC(), "", 0
		} else {
			st.LastErrorClass = syncer.ErrorClass(err)
			st.ConsecutiveFailures++
		}
	})
}

// jitter spreads cycles by ±10% (§9).
func jitter(d time.Duration) time.Duration {
	f := 0.9 + 0.2*rand.Float64() // #nosec G404 -- scheduling jitter, not security
	return time.Duration(float64(d) * f)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pinger is the dead-man's switch (§13): GET ping_url after a successful
// cycle, GET ping_url/fail on an auth failure. Nothing else is sent.
type pinger struct {
	url    string
	client *http.Client
}

func (p pinger) ping(ctx context.Context, fail bool, _ string) {
	if p.url == "" {
		return
	}
	u := p.url
	if fail {
		u = strings.TrimSuffix(u, "/") + "/fail"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return
	}
	if resp, err := p.client.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// runStatus is `calbridge status`: read-only counts from the store (works
// while serve is running).
func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := cf.load()
	if err != nil {
		return err
	}
	s, err := store.Open(ctx, cfg.Store.Path, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	return printStatus(ctx, stdout, s, time.Now())
}

func printStatus(ctx context.Context, w io.Writer, s *store.Store, now time.Time) error {
	counts, err := s.CountMappingsByState(ctx)
	if err != nil {
		return err
	}
	ms, err := s.ListMappings(ctx)
	if err != nil {
		return err
	}
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "SYNC\tLAST SUCCESS\tLAST FULL SYNC")
	for _, prefix := range []string{"google:", "icloud:"} {
		for _, m := range syncKeys(ctx, s, ms, prefix) {
			st, err := s.GetSyncState(ctx, m)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", st.Key, ago(st.LastSuccess, now), ago(st.LastFullSync, now))
		}
	}
	_ = tw.Flush()

	_, _ = fmt.Fprintln(w)
	states := make([]string, 0, len(counts))
	for st := range counts {
		states = append(states, string(st))
	}
	sort.Strings(states)
	_, _ = fmt.Fprint(w, "mirrors:")
	if len(states) == 0 {
		_, _ = fmt.Fprint(w, " none yet")
	}
	for _, st := range states {
		_, _ = fmt.Fprintf(w, " %s=%d", st, counts[store.State(st)])
	}
	_, _ = fmt.Fprintln(w)

	var problems []store.Mapping
	for _, m := range ms {
		if m.Failures > 0 || m.RSVPPending != "" {
			problems = append(problems, m)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	_, _ = fmt.Fprintln(w, "\nretrying or parked:")
	tw = newTable(w)
	_, _ = fmt.Fprintln(tw, "GOOGLE ID\tSTATE\tFAILURES\tLAST ERROR\tNEXT ATTEMPT\tPENDING RSVP")
	for _, m := range problems {
		next := "parked"
		if !m.Parked() {
			next = ago(m.NextAttemptAt, now)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", m.GoogleEventID, m.State, m.Failures, dash(m.LastError), next, dash(m.RSVPPending))
	}
	return tw.Flush()
}

// syncKeys returns the sync_state keys to show: the Google calendars and
// iCloud collections the mappings use.
func syncKeys(ctx context.Context, s *store.Store, ms []store.Mapping, prefix string) []string {
	seen := map[string]bool{}
	var keys []string
	add := func(k string) {
		if !seen[k] {
			if _, err := s.GetSyncState(ctx, k); err == nil {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	for _, m := range ms {
		if prefix == "google:" {
			add(prefix + m.GoogleCalendarID)
		} else {
			add(prefix + m.ICloudCalendar)
		}
	}
	if prefix == "google:" {
		add("google:primary")
	}
	sort.Strings(keys)
	return keys
}

func ago(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t).Round(time.Second)
	if d < 0 {
		return "in " + (-d).String()
	}
	return d.String() + " ago"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// runCalendars is `calbridge calendars`: list iCloud calendars and hrefs,
// marking the configured target.
func runCalendars(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("calendars", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := cf.load()
	if err != nil {
		return err
	}
	c, err := newICloud(cfg, cf.insecurePerms)
	if err != nil {
		return err
	}
	d, err := c.Discover(ctx)
	if err != nil {
		return err
	}
	target, terr := resolveCalendar(d, cfg)
	names := map[string]int{}
	for _, ci := range d.Calendars {
		if ci.IsCalendar {
			names[ci.DisplayName]++
		}
	}
	tw := newTable(stdout)
	_, _ = fmt.Fprintln(tw, "\tNAME\tHREF\tCOMPONENTS\tSYNC")
	for _, ci := range d.Calendars {
		if !ci.IsCalendar {
			continue
		}
		mark := ""
		if terr == nil && ci.Href == target.Href {
			mark = "*"
		}
		name := ci.DisplayName
		if names[name] > 1 {
			name += " (duplicate name)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\n", mark, name, ci.Href, strings.Join(ci.Components, ","), ci.SupportsSync)
	}
	_ = tw.Flush()
	if terr != nil {
		_, _ = fmt.Fprintf(stdout, "\nconfigured target: %v\n", terr)
	} else {
		_, _ = fmt.Fprintf(stdout, "\n* = configured target\n")
	}
	return nil
}

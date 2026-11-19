package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/JasonBerry/calbridge/internal/config"
	"github.com/JasonBerry/calbridge/internal/googlecal"
	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/lock"
	"github.com/JasonBerry/calbridge/internal/secrets"
	"github.com/JasonBerry/calbridge/internal/store"
	"github.com/JasonBerry/calbridge/internal/syncer"
	"github.com/JasonBerry/calbridge/internal/translate"
)

// exitLocked is the exit code when another instance holds the lock (§9).
const exitLocked = 3

// errLocked wraps lock.ErrLocked for the exit-code mapping in run.
var errLocked = lock.ErrLocked

// newLogger builds the slog logger from log.level / log.format.
func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.Log.Level))
	opts := &slog.HandlerOptions{Level: lvl}
	if cfg.Log.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// newICloud builds the CalDAV client from config and the app password.
func newICloud(cfg config.Config, insecurePerms bool) (*icloud.Client, error) {
	pw, err := secrets.ICloudAppPassword(os.Getenv, cfg.ICloud.AppPasswordFile, insecurePerms)
	if err != nil {
		return nil, err
	}
	return icloud.New(icloud.Options{
		Username:     cfg.ICloud.Username,
		Password:     pw,
		HTTPClient:   &http.Client{Timeout: 60 * time.Second},
		RateLimitRPS: cfg.ICloud.RateLimitRPS,
		UserAgent:    "calbridge/" + version,
	})
}

// resolveCalendar picks the target collection: calendar_href when set,
// else the one calendar with the configured display name. Two calendars
// with the same name is an error (P0-3 found two named "Family").
func resolveCalendar(d icloud.Discovery, cfg config.Config) (icloud.CalendarInfo, error) {
	var matches []icloud.CalendarInfo
	for _, ci := range d.Calendars {
		if !ci.IsCalendar {
			continue
		}
		if cfg.ICloud.CalendarHref != "" {
			if strings.TrimSuffix(ci.Href, "/") == strings.TrimSuffix(cfg.ICloud.CalendarHref, "/") {
				return ci, nil
			}
			continue
		}
		if ci.DisplayName == cfg.ICloud.Calendar {
			matches = append(matches, ci)
		}
	}
	switch {
	case cfg.ICloud.CalendarHref != "":
		return icloud.CalendarInfo{}, fmt.Errorf("no calendar with href %q (run `calbridge calendars`)", cfg.ICloud.CalendarHref)
	case len(matches) == 0:
		return icloud.CalendarInfo{}, fmt.Errorf("no calendar named %q (run `calbridge calendars`)", cfg.ICloud.Calendar)
	case len(matches) > 1:
		return icloud.CalendarInfo{}, fmt.Errorf("%d calendars are named %q; set icloud.calendar_href (run `calbridge calendars`)", len(matches), cfg.ICloud.Calendar)
	}
	if !strings.HasSuffix(matches[0].Href, "/") {
		matches[0].Href += "/"
	}
	return matches[0], nil
}

// engineOptions maps the configuration onto syncer.Options.
func engineOptions(cfg config.Config, calendarHref string) syncer.Options {
	alarms := make([]time.Duration, 0, len(cfg.Sync.Alarms))
	for _, a := range cfg.Sync.Alarms {
		alarms = append(alarms, a.D())
	}
	if !strings.HasSuffix(calendarHref, "/") {
		calendarHref += "/"
	}
	return syncer.Options{
		GoogleCalendarID:     cfg.Google.CalendarID,
		CalendarHref:         calendarHref,
		SelfEmail:            cfg.ICloud.AttendeeEmail,
		ScheduleAgent:        cfg.ICloud.ScheduleAgent,
		Privacy:              translate.PrivacyMode(cfg.Sync.Privacy),
		Attendees:            translate.AttendeeMode(cfg.Sync.Attendees),
		DefaultAlarms:        alarms,
		EmitVTimezone:        cfg.ICloud.EmitVTimezone,
		Lookback:             cfg.Sync.Lookback.D(),
		IncludeOrganizedByMe: cfg.Sync.IncludeOrganizedByMe,
		DeleteMode:           cfg.ICloud.DeleteMode,
		RecreateOnReschedule: cfg.Sync.RecreateOnReschedule,
		SendUpdates:          cfg.Google.SendUpdates,
		NoGoogleRSVPToICloud: !cfg.Sync.PushGoogleRSVPToICloud,
		DeclinedDelete:       cfg.Sync.Declined == "delete",
	}
}

// app is a configured engine with its store and (optionally) the lock.
type app struct {
	cfg    config.Config
	log    *slog.Logger
	store  *store.Store
	lock   *lock.Lock
	cal    icloud.CalendarInfo
	engine *syncer.Engine
}

// openApp loads the config, takes the single-instance lock (if withLock),
// opens the store, builds both API clients and resolves the calendar.
func openApp(ctx context.Context, cf *commonFlags, stderr io.Writer, withLock bool) (_ *app, err error) {
	cfg, err := cf.load()
	if err != nil {
		return nil, err
	}
	if cfg.ICloud.SyncMethod == "ctag" {
		return nil, errors.New("icloud.sync_method ctag is not implemented (P0-3: sync-collection works; use auto)")
	}
	a := &app{cfg: cfg, log: newLogger(cfg, stderr)}
	defer func() {
		if err != nil {
			a.Close()
		}
	}()
	if withLock {
		if a.lock, err = lock.Acquire(lock.PathFor(cfg.Store.Path)); err != nil {
			return nil, err
		}
	}
	if a.store, err = store.Open(ctx, cfg.Store.Path, store.Options{}); err != nil {
		return nil, err
	}
	g, err := googlecal.NewAuthorized(ctx, cfg.Google.ClientFile, cfg.Google.TokenFile, cfg.Google.Scope, cf.insecurePerms)
	if err != nil {
		return nil, fmt.Errorf("google: %w (run `calbridge auth`?)", err)
	}
	c, err := newICloud(cfg, cf.insecurePerms)
	if err != nil {
		return nil, err
	}
	d, err := c.Discover(ctx)
	if err != nil {
		return nil, fmt.Errorf("icloud discovery: %w", err)
	}
	if a.cal, err = resolveCalendar(d, cfg); err != nil {
		return nil, err
	}
	a.engine = &syncer.Engine{G: g, C: c, S: a.store, Opts: engineOptions(cfg, a.cal.Href), Clock: syncer.SystemClock{}, Log: a.log}
	if err := a.engine.Opts.Validate(); err != nil {
		return nil, err
	}
	return a, nil
}

// Close releases the store and the lock.
func (a *app) Close() {
	if a.store != nil {
		_ = a.store.Close()
	}
	if a.lock != nil {
		_ = a.lock.Release()
	}
}

// printReport writes a cycle summary and, when there were writes (or
// planned writes in dry-run), a table of them: ids and hrefs only.
func printReport(w io.Writer, rep syncer.Report, dryRun bool) {
	verb := ""
	if dryRun {
		verb = " (dry run: nothing was written)"
	}
	_, _ = fmt.Fprintf(w, "cycle%s: created %d, updated %d, adopted %d, deleted %d, RSVPs sent %d, unchanged %d, skipped %d, deleted in Apple %d, failed %d\n",
		verb, rep.Created, rep.Updated, rep.Adopted, rep.Deleted, rep.RSVPs, rep.Unchanged, rep.Skipped, rep.UserDeleted, rep.Failed)
	if len(rep.Ops) == 0 {
		return
	}
	title := "writes"
	if dryRun {
		title = "planned writes"
	}
	_, _ = fmt.Fprintf(w, "\n%s:\n", title)
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "OP\tGOOGLE ID\tICLOUD HREF")
	for _, op := range rep.Ops {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", op.Op, op.GoogleID, op.Href)
	}
	_ = tw.Flush()
}

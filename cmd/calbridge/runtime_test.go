package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JasonBerry/calbridge/internal/config"
	"github.com/JasonBerry/calbridge/internal/googlecal"
	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/lock"
	"github.com/JasonBerry/calbridge/internal/server"
	"github.com/JasonBerry/calbridge/internal/store"
	"github.com/JasonBerry/calbridge/internal/syncer"
	"github.com/JasonBerry/calbridge/internal/translate"
)

func TestResolveCalendar(t *testing.T) {
	d := icloud.Discovery{Calendars: []icloud.CalendarInfo{
		{Href: "/1/calendars/home/", DisplayName: "Home", IsCalendar: true},
		{Href: "/1/calendars/f1/", DisplayName: "Family", IsCalendar: true},
		{Href: "/1/calendars/f2/", DisplayName: "Family", IsCalendar: true},
		{Href: "/1/calendars/inbox/", DisplayName: "Inbox"},
		{Href: "/1/calendars/test", DisplayName: "calbridge-test", IsCalendar: true},
	}}
	cfg := func(name, href string) config.Config {
		c := config.Default()
		c.ICloud.Calendar, c.ICloud.CalendarHref = name, href
		return c
	}
	if ci, err := resolveCalendar(d, cfg("Home", "")); err != nil || ci.Href != "/1/calendars/home/" {
		t.Errorf("by name: %+v %v", ci, err)
	}
	if ci, err := resolveCalendar(d, cfg("calbridge-test", "")); err != nil || ci.Href != "/1/calendars/test/" {
		t.Errorf("trailing slash: %+v %v", ci, err)
	}
	if _, err := resolveCalendar(d, cfg("Family", "")); err == nil || !strings.Contains(err.Error(), "calendar_href") {
		t.Errorf("duplicate names: %v", err)
	}
	if ci, err := resolveCalendar(d, cfg("Family", "/1/calendars/f2")); err != nil || ci.Href != "/1/calendars/f2/" {
		t.Errorf("by href: %+v %v", ci, err)
	}
	for _, c := range []config.Config{cfg("Nope", ""), cfg("", "/1/calendars/nope/"), cfg("Inbox", "")} {
		if _, err := resolveCalendar(d, c); err == nil {
			t.Errorf("resolved %q/%q", c.ICloud.Calendar, c.ICloud.CalendarHref)
		}
	}
}

func TestEngineOptions(t *testing.T) {
	c := config.Default()
	c.ICloud.AttendeeEmail = "user@YOUR_DOMAIN"
	c.ICloud.ScheduleAgent = "NONE"
	c.Sync.Alarms = []config.Duration{config.Duration(10 * time.Minute)}
	c.Sync.PushGoogleRSVPToICloud = false
	c.Sync.Declined = "delete"
	o := engineOptions(c, "/1/calendars/test")
	if o.CalendarHref != "/1/calendars/test/" || o.SelfEmail != "user@YOUR_DOMAIN" || o.ScheduleAgent != "NONE" ||
		!o.NoGoogleRSVPToICloud || !o.DeclinedDelete || len(o.DefaultAlarms) != 1 || o.DefaultAlarms[0] != 10*time.Minute ||
		o.Privacy != translate.PrivacyFull || o.Attendees != translate.AttendeesAll || o.SendUpdates != "all" || o.DeleteMode != "plain" {
		t.Errorf("options %+v", o)
	}
	if err := o.Validate(); err != nil {
		t.Error(err)
	}
}

func TestPrintReportDryRunTable(t *testing.T) {
	var b bytes.Buffer
	printReport(&b, syncer.Report{Created: 1, Deleted: 1, Ops: []syncer.PlannedOp{
		{Op: "put_create", GoogleID: "abc", Href: "/1/calendars/test/calbridge-x.ics"},
		{Op: "delete", GoogleID: "def", Href: "/1/calendars/test/calbridge-y.ics"},
	}}, true)
	out := b.String()
	for _, want := range []string{"dry run", "created 1", "deleted 1", "planned writes:", "OP", "GOOGLE ID", "put_create", "abc", "calbridge-y.ics"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	b.Reset()
	printReport(&b, syncer.Report{Unchanged: 4}, false)
	if strings.Contains(b.String(), "OP") || strings.Contains(b.String(), "dry run") {
		t.Errorf("no-op report:\n%s", b.String())
	}
}

type scriptedEngine struct {
	errs  []error
	calls int
}

func (s *scriptedEngine) Cycle(context.Context) (syncer.Report, error) {
	err := s.errs[s.calls]
	s.calls++
	return syncer.Report{Created: 1}, err
}

func TestServeLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	eng := &scriptedEngine{errs: []error{nil, errors.New("icloud 503"), fmt.Errorf("x: %w", googlecal.ErrAuth), nil}}
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	srv := server.New("v", "/c/", time.Minute, func() time.Time { return now })
	var pings, backups []string
	var slept []time.Duration
	type snap struct {
		success time.Time
		class   string
		fails   int
	}
	var snaps []snap
	loop := serveLoop{engine: eng, store: st, srv: srv,
		ping: func(_ context.Context, fail bool, class string) {
			pings = append(pings, fmt.Sprintf("%v:%s", fail, class))
		},
		log: func(context.Context, string, ...any) {},
		backup: func(_ context.Context, at time.Time) {
			backups = append(backups, at.Format("15:04"))
		},
		now: func() time.Time { return now },
		sleep: func(_ context.Context, d time.Duration) error {
			s := srv.Snapshot()
			snaps = append(snaps, snap{s.LastSuccess, s.LastErrorClass, s.ConsecutiveFailures})
			slept = append(slept, d)
			now = now.Add(d)
			if len(slept) == len(eng.errs) {
				cancel()
				return ctx.Err()
			}
			return nil
		}}
	if err := loop.run(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if eng.calls != 4 {
		t.Fatalf("%d cycles", eng.calls)
	}
	if got := strings.Join(pings, " "); got != "false: true:google_auth false:" {
		t.Errorf("pings %q", got)
	}
	if len(backups) != 2 {
		t.Errorf("backups after successful cycles only: %v", backups)
	}
	if snaps[1].class != "transient" || snaps[1].fails != 1 || snaps[2].class != "google_auth" || snaps[2].fails != 2 ||
		snaps[3].class != "" || snaps[3].fails != 0 || snaps[3].success.IsZero() {
		t.Errorf("status over time %+v", snaps)
	}
	for _, d := range slept {
		if d < 54*time.Second || d > 66*time.Second {
			t.Errorf("sleep %v outside ±10%% jitter", d)
		}
	}
}

func TestPrintStatus(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	_ = st.PutSyncState(ctx, &store.SyncState{Key: "google:primary", Token: "t", LastSuccess: now.Add(-2 * time.Minute)})
	for i, f := range []int{0, store.MaxFailures, 2} {
		m := &store.Mapping{GoogleCalendarID: "primary", GoogleEventID: fmt.Sprintf("ev%d", i), ICalUID: fmt.Sprintf("u%d", i),
			ICloudCalendar: "/c/", ICloudHref: fmt.Sprintf("/c/%d.ics", i), Failures: f, LastError: map[bool]string{true: "transient"}[f > 0],
			NextAttemptAt: now.Add(4 * time.Minute)}
		if err := st.CreateMapping(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	var b bytes.Buffer
	if err := printStatus(ctx, &b, st, now); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"google:primary", "2m0s ago", "pending_put=3", "ev1", "parked", "ev2", "in 4m0s"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ev0") {
		t.Errorf("healthy row listed:\n%s", out)
	}
}

// TestSecondInstanceExitCode: `once` refuses to run while another calbridge
// holds the database lock, with exit code 3 (§9).
func TestSecondInstanceExitCode(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "calbridge.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`google:
  client_file: %[1]s/client.json
  token_file: %[1]s/token.json
  self_email: user@example.com
icloud:
  username: user@example.com
  attendee_email: user@example.com
  calendar: calbridge-test
  app_password_file: %[1]s/pw
store:
  path: %[2]s
`, dir, db)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := lock.Acquire(lock.PathFor(db))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"once", "--config", cfgPath}, &out, &errb); code != exitLocked {
		t.Fatalf("exit %d; want %d (stderr %q)", code, exitLocked, errb.String())
	}
	if !strings.Contains(errb.String(), "another calbridge is running") {
		t.Errorf("stderr %q", errb.String())
	}
}

func TestPrintReconcile(t *testing.T) {
	var b bytes.Buffer
	printReconcile(&b, syncer.ReconcilePlan{Items: []syncer.ReconcileItem{
		{Action: syncer.ActOK, GoogleID: "ok1", Href: "/c/1.ics"},
		{Action: syncer.ActAdopt, GoogleID: "a1", Href: "/c/2.ics", Note: "mapping missing"},
		{Action: syncer.ActOrphan, Href: "/c/3.ics", Note: "no X-CALBRIDGE-GID"},
	}}, false)
	out := b.String()
	for _, want := range []string{"plan (nothing changed", "adopt 1", "ok 1", "orphan 1", "a1", "mapping missing", "--delete-orphans"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ok1") {
		t.Errorf("ok rows listed:\n%s", out)
	}
}

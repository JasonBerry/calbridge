package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	calendar "google.golang.org/api/calendar/v3"

	"github.com/JasonBerry/calbridge/internal/fakes/caldavfake"
	"github.com/JasonBerry/calbridge/internal/fakes/googlefake"
	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/store"
	"github.com/JasonBerry/calbridge/internal/translate"
)

const (
	calHref   = "/123/calendars/test/"
	selfEmail = "user@YOUR_DOMAIN"
	organizer = "ORGANIZER_TEST@example.com"
)

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

type harness struct {
	t     *testing.T
	g     *googlefake.Fake
	c     *caldavfake.Fake
	s     *store.Store
	e     *Engine
	clock *fixedClock
	logs  *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "calbridge.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h := &harness{t: t, g: googlefake.New(), c: caldavfake.New(calHref), s: s,
		clock: &fixedClock{t: time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)}, logs: &bytes.Buffer{}}
	h.e = &Engine{G: h.g, C: h.c, S: s, Clock: h.clock,
		Log: slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Opts: Options{
			GoogleCalendarID: "primary", CalendarHref: calHref, SelfEmail: selfEmail, ScheduleAgent: "NONE",
			Privacy: translate.PrivacyFull, Attendees: translate.AttendeesAll, Lookback: 30 * 24 * time.Hour,
			DeleteMode: DeleteModePlain,
		}}
	return h
}

func (h *harness) cycle() Report {
	h.t.Helper()
	rep, err := h.e.Cycle(context.Background())
	if err != nil {
		h.t.Fatalf("Cycle: %v", err)
	}
	return rep
}

// cycleTwice runs a cycle and asserts a second one writes nothing (rule 8).
func (h *harness) cycleTwice() Report {
	h.t.Helper()
	rep := h.cycle()
	before := h.c.Writes()
	again := h.cycle()
	if again.Writes() != 0 || h.c.Writes() != before {
		h.t.Fatalf("second cycle wrote %d times: %+v", again.Writes(), again.Ops)
	}
	return rep
}

func (h *harness) mapping(id string) store.Mapping {
	h.t.Helper()
	m, err := h.s.GetMappingByGoogleID(context.Background(), "primary", id)
	if err != nil {
		h.t.Fatalf("mapping %s: %v", id, err)
	}
	return m
}

func hrefFor(id string) string { return translate.ObjectHref(calHref, id+"@google.com") }

func invite(id string) *calendar.Event {
	return &calendar.Event{
		Id: id, Summary: "Meeting " + id, Status: "confirmed", EventType: "default",
		Organizer: &calendar.EventOrganizer{Email: organizer},
		Attendees: []*calendar.EventAttendee{
			{Email: organizer, Organizer: true, ResponseStatus: "accepted"},
			{Email: selfEmail, Self: true, ResponseStatus: "needsAction"},
		},
		Start: &calendar.EventDateTime{DateTime: "2026-11-25T10:00:00+11:00", TimeZone: "Australia/Sydney"},
		End:   &calendar.EventDateTime{DateTime: "2026-11-25T11:00:00+11:00", TimeZone: "Australia/Sydney"},
	}
}

func alarm(trigger string) *ical.Component {
	a := ical.NewComponent(ical.CompAlarm)
	a.Props.Set(&ical.Prop{Name: ical.PropAction, Params: ical.Params{}, Value: "DISPLAY"})
	a.Props.Set(&ical.Prop{Name: ical.PropTrigger, Params: ical.Params{}, Value: trigger})
	return a
}

func summaryOf(t *testing.T, obj *icloud.Object) string {
	t.Helper()
	s, _ := obj.Cal.Children[len(obj.Cal.Children)-1].Props.Text(ical.PropSummary)
	return s
}

func selfPartStat(t *testing.T, obj *icloud.Object) translate.PartStat {
	t.Helper()
	ps, _, err := translate.ExtractSelfStatus(obj.Cal, selfEmail)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func TestCreate(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.g.Put(invite("b"))
	rep := h.cycleTwice()
	if rep.Created != 2 || !rep.FullResync {
		t.Fatalf("report %+v", rep)
	}
	for _, id := range []string{"a", "b"} {
		obj := h.c.Object(hrefFor(id))
		if obj == nil {
			t.Fatalf("no mirror for %s", id)
		}
		m := h.mapping(id)
		if m.State != store.StateSynced || m.ICloudETag != obj.ETag || m.AgreedStatus != "needsAction" {
			t.Errorf("%s mapping %+v", id, m)
		}
		if summaryOf(t, obj) != "Meeting "+id || selfPartStat(t, obj) != translate.PartStatNeedsAction {
			t.Errorf("%s mirror content wrong", id)
		}
	}
	st, err := h.s.GetSyncState(context.Background(), "google:primary")
	if err != nil || st.Token == "" || st.LastFullSync.IsZero() {
		t.Errorf("sync state %+v %v", st, err)
	}
	if h.g.ListCalls < 2 {
		t.Errorf("paging not exercised: %d List calls", h.g.ListCalls)
	}
}

func TestUpdate(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.cycleTwice()
	oldETag := h.c.Object(hrefFor("a")).ETag

	h.g.Update("a", func(ev *calendar.Event) { ev.Summary = "Renamed"; ev.Sequence = 1 })
	rep := h.cycleTwice()
	obj := h.c.Object(hrefFor("a"))
	if rep.Updated != 1 || summaryOf(t, obj) != "Renamed" || obj.ETag == oldETag {
		t.Fatalf("report %+v summary %q", rep, summaryOf(t, obj))
	}
	if m := h.mapping("a"); m.Sequence != 1 || m.ICloudETag != obj.ETag {
		t.Errorf("mapping %+v", m)
	}
}

func TestNoWriteWhenOnlyGoogleMetadataChanges(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.cycleTwice()
	h.g.Update("a", func(*calendar.Event) {}) // new etag/updated, same content
	rep := h.cycle()
	if rep.Writes() != 0 || rep.Unchanged != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestGuestResponseUpdatesMirror(t *testing.T) {
	h := newHarness(t)
	ev := invite("a")
	ev.Attendees = append(ev.Attendees, &calendar.EventAttendee{Email: "guest@example.com", ResponseStatus: "needsAction"})
	h.g.Put(ev)
	h.cycleTwice()
	h.g.Update("a", func(ev *calendar.Event) { ev.Attendees[2].ResponseStatus = "accepted" })
	if rep := h.cycleTwice(); rep.Updated != 1 {
		t.Fatalf("guest RSVP did not update the mirror: %+v", rep)
	}
	unfolded := strings.ReplaceAll(string(h.c.Object(hrefFor("a")).Raw), "\r\n ", "")
	if !strings.Contains(unfolded, "PARTSTAT=ACCEPTED;ROLE=REQ-PARTICIPANT;SCHEDULE-AGENT=NONE:mailto:guest@example.com") {
		t.Errorf("guest status not rendered:\n%s", h.c.Object(hrefFor("a")).Raw)
	}
}

func TestUpdateKeepsAppleAlarm(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.cycleTwice()
	h.c.AppleEdit(hrefFor("a"), func(cal *ical.Calendar) {
		ev := cal.Children[len(cal.Children)-1]
		ev.Children = append(ev.Children, alarm("-PT15M"))
	})
	h.g.Update("a", func(ev *calendar.Event) { ev.Location = "Room 1" })
	h.cycleTwice()
	if !strings.Contains(string(h.c.Object(hrefFor("a")).Raw), "TRIGGER:-PT15M") {
		t.Error("user's alarm lost on update")
	}
}

func TestCancelDeletesMirror(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(h *harness)
	}{
		{"organizer cancels", func(h *harness) { h.g.Cancel("a") }},
		{"user removed as guest", func(h *harness) {
			h.g.Update("a", func(ev *calendar.Event) { ev.Attendees = ev.Attendees[:1] })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.g.Put(invite("a"))
			h.cycleTwice()
			etag := h.c.Object(hrefFor("a")).ETag
			tc.mod(h)
			rep := h.cycleTwice()
			if rep.Deleted != 1 || h.c.Object(hrefFor("a")) != nil {
				t.Fatalf("report %+v", rep)
			}
			if d := h.c.DeleteLog[len(h.c.DeleteLog)-1]; d.IfMatch != etag || d.ScheduleReply != nil {
				t.Errorf("delete opts %+v; want If-Match %s and no Schedule-Reply (P0-9 plain)", d, etag)
			}
			if m := h.mapping("a"); m.State != store.StateDeleted || m.DeletedAt.IsZero() {
				t.Errorf("mapping %+v", m)
			}
		})
	}
}

func TestCancelAfterAppleEditRetriesDelete(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.cycleTwice()
	h.c.AppleRSVP(hrefFor("a"), selfEmail, "ACCEPTED") // new ETag behind our back
	h.g.Cancel("a")
	if rep := h.cycleTwice(); rep.Deleted != 1 || h.c.Object(hrefFor("a")) != nil {
		t.Fatalf("report %+v", rep)
	}
}

func TestDeleteModeScheduleReplyF(t *testing.T) {
	h := newHarness(t)
	h.e.Opts.DeleteMode = DeleteModeScheduleReplyF
	h.g.Put(invite("a"))
	h.cycleTwice()
	h.g.Cancel("a")
	h.cycleTwice()
	d := h.c.DeleteLog[len(h.c.DeleteLog)-1]
	if d.ScheduleReply == nil || *d.ScheduleReply {
		t.Errorf("Schedule-Reply = %v; want F", d.ScheduleReply)
	}
}

func TestReappearAfterCancel(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.cycleTwice()
	h.g.Cancel("a")
	h.cycleTwice()
	h.g.Update("a", func(ev *calendar.Event) { ev.Status = "confirmed" })
	if rep := h.cycleTwice(); rep.Created != 1 || h.c.Object(hrefFor("a")) == nil {
		t.Fatalf("report %+v", rep)
	}
}

func TestTombstoneForUnknownEvent(t *testing.T) {
	h := newHarness(t)
	h.cycle()
	h.g.Put(invite("ghost"))
	h.g.Cancel("ghost") // created and cancelled between two syncs (P0-1)
	if rep := h.cycleTwice(); rep.Writes() != 0 || len(h.c.Hrefs()) != 0 {
		t.Fatalf("report %+v", rep)
	}
}

func TestUserDeletionIsNeverRecreated(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.cycleTwice()
	h.c.UserDelete(hrefFor("a"))
	h.g.Update("a", func(ev *calendar.Event) { ev.Description = "agenda" })
	rep := h.cycleTwice()
	if rep.UserDeleted != 1 || rep.Writes() != 0 || h.c.Object(hrefFor("a")) != nil {
		t.Fatalf("report %+v", rep)
	}
	if m := h.mapping("a"); m.State != store.StateUserDeleted {
		t.Fatalf("state %s", m.State)
	}
	// Even a reschedule does not bring it back by default (§8.5)…
	h.g.Update("a", func(ev *calendar.Event) { ev.Sequence = 3 })
	if rep := h.cycleTwice(); rep.Writes() != 0 {
		t.Fatalf("recreated without opt-in: %+v", rep)
	}
	// …and Google cancelling it later just closes the row.
	h.g.Cancel("a")
	if rep := h.cycleTwice(); rep.Writes() != 0 || h.mapping("a").State != store.StateDeleted {
		t.Fatalf("report %+v state %s", rep, h.mapping("a").State)
	}
}

func TestRecreateOnRescheduleOptIn(t *testing.T) {
	h := newHarness(t)
	h.e.Opts.RecreateOnReschedule = true
	h.g.Put(invite("a"))
	h.cycleTwice()
	h.c.UserDelete(hrefFor("a"))
	h.g.Update("a", func(ev *calendar.Event) { ev.Description = "minor" }) // same sequence
	h.cycleTwice()
	if h.c.Object(hrefFor("a")) != nil {
		t.Fatal("recreated on a minor change")
	}
	h.g.Update("a", func(ev *calendar.Event) { ev.Sequence = 1 })
	if rep := h.cycleTwice(); rep.Created != 1 || h.c.Object(hrefFor("a")) == nil {
		t.Fatalf("not recreated after reschedule: %+v", rep)
	}
}

func TestSyncTokenGoneFullResync(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"a", "b", "c"} {
		h.g.Put(invite(id))
	}
	h.cycleTwice()
	h.g.ExpireSyncTokens()
	rep := h.cycleTwice()
	if !rep.FullResync || rep.Writes() != 0 || rep.Unchanged != 3 {
		t.Fatalf("410 resync wrote: %+v", rep)
	}
}

func TestFullResyncDeletesMissingAndKeepsOld(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.g.Put(invite("b"))
	h.cycleTwice()
	h.g.ExpireSyncTokens()
	h.g.Cancel("a") // not listed by the full resync (showDeleted=false)
	rep := h.cycleTwice()
	if rep.Deleted != 1 || h.c.Object(hrefFor("a")) != nil || h.c.Object(hrefFor("b")) == nil {
		t.Fatalf("report %+v", rep)
	}

	// An event that ended before the lookback window is only closed in the
	// store; its historical mirror stays.
	h.clock.t = h.clock.t.Add(60 * 24 * time.Hour)
	h.g.ExpireSyncTokens()
	rep = h.cycle()
	if rep.Writes() != 0 || h.c.Object(hrefFor("b")) == nil || h.mapping("b").State != store.StateDeleted {
		t.Fatalf("old mirror touched: %+v, state %s", rep, h.mapping("b").State)
	}
}

func TestFullResyncRefusesBulkDelete(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 7; i++ {
		h.g.Put(invite(fmt.Sprintf("e%d", i)))
	}
	h.cycleTwice()
	st, _ := h.s.GetSyncState(context.Background(), "google:primary")
	for i := 0; i < 7; i++ {
		h.g.Cancel(fmt.Sprintf("e%d", i))
	}
	h.g.ExpireSyncTokens()
	before := h.c.Writes()
	_, err := h.e.Cycle(context.Background())
	if !errors.Is(err, ErrTooManyDeletes) {
		t.Fatalf("err = %v; want ErrTooManyDeletes", err)
	}
	if h.c.Writes() != before || len(h.c.Hrefs()) != 7 {
		t.Fatalf("mirrors deleted despite the guard")
	}
	if now, _ := h.s.GetSyncState(context.Background(), "google:primary"); now.Token != st.Token {
		t.Error("sync token advanced after a refused resync")
	}
}

func TestCrashAfterPutAdopts(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.c.LoseNextPutResponse = true
	if _, err := h.e.Cycle(context.Background()); err == nil {
		t.Fatal("lost PUT response not reported")
	}
	if m := h.mapping("a"); m.State != store.StatePendingPut || m.ICloudETag != "" {
		t.Fatalf("write-ahead row %+v", m)
	}
	if st, err := h.s.GetSyncState(context.Background(), "google:primary"); err == nil && st.Token != "" {
		t.Fatal("sync token advanced after a failed series")
	}
	rep := h.cycleTwice()
	if rep.Adopted != 1 || len(h.c.Hrefs()) != 1 || h.mapping("a").State != store.StateSynced {
		t.Fatalf("report %+v hrefs %v", rep, h.c.Hrefs())
	}
}

func TestPutFailureRetriedNextCycle(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.c.FailNextPut = caldavfake.ErrInjected
	rep, err := h.e.Cycle(context.Background())
	if err == nil || rep.Failed == 0 {
		t.Fatalf("failure not reported: %+v %v", rep, err)
	}
	if rep := h.cycleTwice(); rep.Created != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestNoETagOnPutGetsIt(t *testing.T) {
	h := newHarness(t)
	h.c.NoETagOnPut = true
	h.g.Put(invite("a"))
	h.cycleTwice()
	if m := h.mapping("a"); m.ICloudETag == "" || m.ICloudETag != h.c.Object(hrefFor("a")).ETag {
		t.Fatalf("etag %q", m.ICloudETag)
	}
}

func TestUIDConflictAdoptsImportedCopy(t *testing.T) {
	h := newHarness(t)
	ev := invite("a")
	h.g.Put(ev)
	// The user clicked "Add to Calendar" on the emailed invite.ics.
	imported := calHref + "imported-invite.ics"
	h.c.Import(imported, []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Apple//EN\r\nBEGIN:VEVENT\r\n"+
		"UID:a@google.com\r\nDTSTAMP:20261118T000000Z\r\nDTSTART:20261124T230000Z\r\nSUMMARY:Imported\r\n"+
		"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT5M\r\nEND:VALARM\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	rep := h.cycleTwice()
	if rep.Adopted != 1 || len(h.c.Hrefs()) != 1 {
		t.Fatalf("report %+v hrefs %v", rep, h.c.Hrefs())
	}
	if m := h.mapping("a"); m.ICloudHref != imported || m.State != store.StateSynced {
		t.Fatalf("mapping %+v", m)
	}
	obj := h.c.Object(imported)
	if summaryOf(t, obj) != "Meeting a" || !strings.Contains(string(obj.Raw), "TRIGGER:-PT5M") {
		t.Errorf("imported copy not overwritten with alarms kept:\n%s", obj.Raw)
	}
}

func TestHrefCollisionIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	foreign := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:someone-else\r\n" +
		"DTSTAMP:20261118T000000Z\r\nDTSTART:20261124T230000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	h.c.Import(hrefFor("a"), foreign)
	rep := h.cycleTwice()
	if rep.Writes() != 0 || h.mapping("a").SkipReason != SkipHrefCollision {
		t.Fatalf("report %+v mapping %+v", rep, h.mapping("a"))
	}
	if string(h.c.Object(hrefFor("a")).Raw) != string(foreign) {
		t.Error("foreign object modified")
	}
}

// racingCalDAV makes Apple rewrite the object between our GET and our first
// conditional PUT.
type racingCalDAV struct {
	*caldavfake.Fake
	raced bool
}

func (r *racingCalDAV) Put(ctx context.Context, href string, cal *ical.Calendar, cond icloud.PutCond) (string, error) {
	if cond.IfMatch != "" && !r.raced {
		r.raced = true
		r.AppleEdit(href, func(c *ical.Calendar) {
			c.Children[len(c.Children)-1].Children = append(c.Children[len(c.Children)-1].Children, alarm("-PT30M"))
		})
	}
	return r.Fake.Put(ctx, href, cal, cond)
}

func TestPreconditionFailedOnUpdateRetries(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.cycleTwice()
	h.e.C = &racingCalDAV{Fake: h.c}
	h.g.Update("a", func(ev *calendar.Event) { ev.Summary = "Renamed" })
	rep := h.cycleTwice()
	obj := h.c.Object(hrefFor("a"))
	if rep.Updated != 1 || summaryOf(t, obj) != "Renamed" || !strings.Contains(string(obj.Raw), "TRIGGER:-PT30M") {
		t.Fatalf("report %+v\n%s", rep, obj.Raw)
	}
}

func TestFilteredEventsAreSkipped(t *testing.T) {
	h := newHarness(t)
	own := invite("own")
	own.Organizer.Self = true
	rec := invite("rec")
	rec.Recurrence = []string{"RRULE:FREQ=WEEKLY"}
	ooo := invite("ooo")
	ooo.EventType = "outOfOffice"
	for _, ev := range []*calendar.Event{own, rec, ooo} {
		h.g.Put(ev)
	}
	rep := h.cycleTwice()
	if rep.Writes() != 0 || rep.Skipped != 3 {
		t.Fatalf("report %+v", rep)
	}
	for id, reason := range map[string]string{"own": SkipOrganizerSelf, "rec": SkipRecurring, "ooo": "event_type:outOfOffice"} {
		if m := h.mapping(id); m.State != store.StateSkipped || m.SkipReason != reason {
			t.Errorf("%s: %s %q", id, m.State, m.SkipReason)
		}
	}
	// The filter lifting (e.g. include_organized_by_me turned on) creates it.
	h.e.Opts.IncludeOrganizedByMe = true
	h.g.Update("own", func(*calendar.Event) {})
	if rep := h.cycleTwice(); rep.Created != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("keep"))
	h.g.Put(invite("change"))
	h.g.Put(invite("cancel"))
	h.cycleTwice()
	h.g.Update("change", func(ev *calendar.Event) { ev.Summary = "Changed" })
	h.g.Cancel("cancel")
	h.g.Put(invite("new"))

	snap := func() string {
		ms, _ := h.s.ListMappings(context.Background())
		st, _ := h.s.GetSyncState(context.Background(), "google:primary")
		ops, _ := h.s.RecentOps(context.Background(), 1000)
		return fmt.Sprintf("%+v|%+v|%d|%v|%d", ms, st, len(ops), h.c.Hrefs(), h.c.Writes())
	}
	before := snap()
	h.e.DryRun = true
	rep := h.cycle()
	if after := snap(); after != before {
		t.Fatalf("dry-run changed state:\nbefore %s\nafter  %s", before, after)
	}
	got := map[string]string{}
	for _, op := range rep.Ops {
		got[op.GoogleID] = op.Op
	}
	want := map[string]string{"change": "put_update", "cancel": "delete", "new": "put_create"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("planned ops %v; want %v", got, want)
	}
	// The real cycle then does exactly what was planned.
	h.e.DryRun = false
	real := h.cycleTwice()
	if real.Writes() != len(rep.Ops) {
		t.Fatalf("real cycle did %d writes, dry-run planned %d", real.Writes(), len(rep.Ops))
	}
}

func TestLogsHaveNoEventContent(t *testing.T) {
	h := newHarness(t)
	ev := invite("a")
	ev.Summary = "Secret project kickoff"
	ev.Description = "confidential agenda"
	h.g.Put(ev)
	h.cycleTwice()
	h.g.Cancel("a")
	h.cycleTwice()
	for _, s := range []string{"Secret project", "confidential agenda", organizer, selfEmail} {
		if strings.Contains(h.logs.String(), s) {
			t.Errorf("logs contain %q", s)
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	h := newHarness(t)
	for name, mod := range map[string]func(o *Options){
		"href":        func(o *Options) { o.CalendarHref = "/no-slash" },
		"self":        func(o *Options) { o.SelfEmail = "nobody" },
		"delete mode": func(o *Options) { o.DeleteMode = "neutralize_then_delete" },
		"calendar":    func(o *Options) { o.GoogleCalendarID = "" },
	} {
		e := *h.e
		mod(&e.Opts)
		if _, err := e.Cycle(context.Background()); err == nil {
			t.Errorf("%s: invalid options accepted", name)
		}
	}
}

// TestIdempotencyProperty applies random Google mutations; after each step
// a cycle converges and a second cycle performs zero remote writes, and
// every live mirror matches a fresh rendering of its Google event.
func TestIdempotencyProperty(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			h := newHarness(t)
			h.e.Opts.MaxFullResyncDeletes = 100
			rng := rand.New(rand.NewSource(seed))
			n := 0
			statuses := []string{"needsAction", "accepted", "tentative", "declined"}
			for step := 0; step < 40; step++ {
				ids := h.g.SortedIDs()
				pick := func() string { return ids[rng.Intn(len(ids))] }
				switch op := rng.Intn(9); {
				case op <= 1 || len(ids) == 0:
					n++
					ev := invite(fmt.Sprintf("r%d", n))
					if rng.Intn(3) == 0 {
						ev.Attendees = append(ev.Attendees, &calendar.EventAttendee{Email: "g@example.com", ResponseStatus: "needsAction"})
					}
					h.g.Put(ev)
				case op == 2:
					h.g.Update(pick(), func(ev *calendar.Event) { ev.Summary = fmt.Sprintf("S%d", rng.Intn(1000)) })
				case op == 3:
					h.g.Update(pick(), func(ev *calendar.Event) {
						ev.Start.DateTime = fmt.Sprintf("2026-11-%02dT09:00:00+11:00", 20+rng.Intn(8))
						ev.End.DateTime = strings.Replace(ev.Start.DateTime, "T09", "T10", 1)
						ev.Sequence++
					})
				case op == 4:
					h.g.Update(pick(), func(ev *calendar.Event) { ev.Attendees[1].ResponseStatus = statuses[rng.Intn(4)] })
				case op == 5:
					h.g.Update(pick(), func(ev *calendar.Event) {
						if len(ev.Attendees) > 2 {
							ev.Attendees[2].ResponseStatus = statuses[rng.Intn(4)]
						}
					})
				case op == 6:
					h.g.Cancel(pick())
				case op == 7:
					h.g.Update(pick(), func(*calendar.Event) {})
				case op == 8:
					h.g.ExpireSyncTokens()
				}
				h.cycleTwice()
				checkConverged(t, h)
			}
			if h.c.Puts < 20 || h.c.Deletes < 2 {
				t.Fatalf("property run too quiet: %d PUTs, %d DELETEs", h.c.Puts, h.c.Deletes)
			}
		})
	}
}

func checkConverged(t *testing.T, h *harness) {
	t.Helper()
	live := map[string]bool{}
	for _, ev := range h.g.Events() {
		href := hrefFor(ev.Id)
		obj := h.c.Object(href)
		if ev.Status == "cancelled" {
			if obj != nil {
				t.Fatalf("cancelled %s still mirrored", ev.Id)
			}
			continue
		}
		live[href] = true
		if obj == nil {
			t.Fatalf("no mirror for %s", ev.Id)
		}
		want, err := translate.RenderSeries(ev, nil, translate.RSVPView{}, obj.Cal, h.e.newRun(&h.s.Queries).renderOptions(ev.Id+"@google.com", nil))
		if err != nil {
			t.Fatal(err)
		}
		if translate.ContentHash(want, selfEmail) != translate.ContentHash(obj.Cal, selfEmail) {
			t.Fatalf("mirror of %s differs from Google", ev.Id)
		}
		if selfPartStat(t, obj) != translate.GoogleToPartStat(ev.Attendees[1].ResponseStatus) {
			t.Fatalf("mirror of %s has PARTSTAT %s, Google %s", ev.Id, selfPartStat(t, obj), ev.Attendees[1].ResponseStatus)
		}
	}
	for _, href := range h.c.Hrefs() {
		if !live[href] {
			t.Fatalf("orphan mirror %s", href)
		}
	}
}

// TestDuplicateICalUID: a second Google event reusing a mapped iCalUID gets
// its own calbridge UID and href (§7.1) instead of clobbering the first.
func TestDuplicateICalUID(t *testing.T) {
	h := newHarness(t)
	a := invite("a")
	a.ICalUID = "shared@google.com"
	h.g.Put(a)
	h.cycleTwice()
	b := invite("b")
	b.ICalUID = "shared@google.com"
	h.g.Put(b)
	rep := h.cycleTwice()
	m := h.mapping("b")
	if rep.Created != 1 || m.ICalUID != "calbridge-b@YOUR_DOMAIN" || m.ICloudHref == h.mapping("a").ICloudHref {
		t.Fatalf("report %+v mapping %+v", rep, m)
	}
	if len(h.c.Hrefs()) != 2 {
		t.Fatalf("hrefs %v", h.c.Hrefs())
	}
}

func TestFailedDeleteRetriedNextCycle(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.cycleTwice()
	h.g.Cancel("a")
	h.c.FailNextDelete = caldavfake.ErrInjected
	if _, err := h.e.Cycle(context.Background()); err == nil {
		t.Fatal("failed delete not reported")
	}
	if m := h.mapping("a"); m.State != store.StatePendingDelete {
		t.Fatalf("state %s; want pending_delete", m.State)
	}
	if rep := h.cycleTwice(); rep.Deleted != 1 || h.c.Object(hrefFor("a")) != nil {
		t.Fatalf("report %+v", rep)
	}
}

package syncer

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	calendar "google.golang.org/api/calendar/v3"

	"github.com/JasonBerry/calbridge/internal/translate"
)

const sydney = "Australia/Sydney"

// series is a weekly Friday 09:00 Sydney series from 20 Nov 2026 to year end.
func series(id string) *calendar.Event {
	ev := invite(id)
	ev.Summary = "Weekly " + id
	ev.Start = &calendar.EventDateTime{DateTime: "2026-11-20T09:00:00+11:00", TimeZone: sydney}
	ev.End = &calendar.EventDateTime{DateTime: "2026-11-20T09:30:00+11:00", TimeZone: sydney}
	ev.Recurrence = []string{"RRULE:FREQ=WEEKLY;BYDAY=FR;UNTIL=20261231T130000Z"}
	return ev
}

// friday returns the n-th Friday occurrence (0 = 20 Nov) as an instant.
func friday(n int) time.Time {
	return time.Date(2026, 11, 19, 22, 0, 0, 0, time.UTC).AddDate(0, 0, 7*n)
}

func key(n int) string { return friday(n).Format("20060102T150405Z") }

func raw(h *harness, id string) string {
	return strings.ReplaceAll(string(h.c.Object(hrefFor(id)).Raw), "\r\n ", "")
}

func overridesOf(t *testing.T, h *harness, id string) (translate.PartStat, map[string]translate.PartStat) {
	t.Helper()
	ps, ov, err := translate.ExtractSelfStatus(h.c.Object(hrefFor(id)).Cal, selfEmail)
	if err != nil {
		t.Fatal(err)
	}
	return ps, ov
}

// appleAnswerOccurrence makes Apple write an override for occurrence n with
// PARTSTAT ps, as answering one occurrence in Apple Calendar does (P0-8).
func appleAnswerOccurrence(h *harness, id string, n int, ps string) {
	h.c.AppleEdit(hrefFor(id), func(cal *ical.Calendar) {
		loc, _ := time.LoadLocation(sydney)
		ridLocal := friday(n).In(loc).Format("20060102T150405")
		for _, c := range cal.Children {
			if rid := c.Props.Get(ical.PropRecurrenceID); rid != nil && translate.NormalizeRecurrenceID(rid) == key(n) {
				setSelfPartStat(c, ps)
				return
			}
		}
		var master *ical.Component
		for _, c := range cal.Children {
			if c.Name == ical.CompEvent && c.Props.Get(ical.PropRecurrenceID) == nil {
				master = c
			}
		}
		ov := ical.NewComponent(ical.CompEvent)
		for name, props := range master.Props {
			switch name {
			case ical.PropRecurrenceRule, ical.PropExceptionDates, ical.PropRecurrenceDates, ical.PropDateTimeStart, ical.PropDateTimeEnd:
				continue
			}
			for _, p := range props {
				cp := p
				cp.Params = ical.Params{}
				for k, v := range p.Params {
					cp.Params[k] = append([]string(nil), v...)
				}
				ov.Props.Add(&cp)
			}
		}
		for name, v := range map[string]string{
			ical.PropRecurrenceID:  ridLocal,
			ical.PropDateTimeStart: ridLocal,
			ical.PropDateTimeEnd:   friday(n).Add(30 * time.Minute).In(loc).Format("20060102T150405"),
		} {
			p := ical.NewProp(name)
			p.Params.Set(ical.ParamTimezoneID, sydney)
			p.Value = v
			ov.Props.Set(p)
		}
		setSelfPartStat(ov, ps)
		cal.Children = append(cal.Children, ov)
	})
}

func setSelfPartStat(c *ical.Component, ps string) {
	atts := c.Props[ical.PropAttendee]
	for i := range atts {
		if strings.EqualFold(strings.TrimPrefix(strings.ToLower(atts[i].Value), "mailto:"), selfEmail) {
			atts[i].Params.Set(ical.ParamParticipationStatus, ps)
			atts[i].Params.Set("CUTYPE", "UNKNOWN")
		}
	}
}

func TestSeriesCreate(t *testing.T) {
	h := newHarness(t)
	h.g.Put(series("s"))
	rep := h.cycleTwice()
	if rep.Created != 1 || !strings.Contains(raw(h, "s"), "RRULE:FREQ=WEEKLY;BYDAY=FR;UNTIL=20261231T130000Z") {
		t.Fatalf("report %+v\n%s", rep, raw(h, "s"))
	}
}

func TestMovedOccurrence(t *testing.T) {
	h := newHarness(t)
	h.g.Put(series("s"))
	h.cycleTwice()
	h.g.PutInstance("s", friday(1), func(ev *calendar.Event) {
		ev.Start.DateTime, ev.End.DateTime = "2026-11-27T11:00:00+11:00", "2026-11-27T11:30:00+11:00"
		ev.Summary = "Weekly s (moved)"
		ev.Attendees[1].ResponseStatus = "needsAction"
	})
	rep := h.cycleTwice()
	body := raw(h, "s")
	if rep.Updated != 1 || !strings.Contains(body, "RECURRENCE-ID;TZID=Australia/Sydney:20261127T090000") ||
		!strings.Contains(body, "DTSTART;TZID=Australia/Sydney:20261127T110000") {
		t.Fatalf("report %+v\n%s", rep, body)
	}
	if _, ov := overridesOf(t, h, "s"); ov[key(1)] != translate.PartStatNeedsAction {
		t.Errorf("overrides %v", ov)
	}
}

func TestExceptionBeforeMaster(t *testing.T) {
	h := newHarness(t)
	old := series("s")
	old.Start = &calendar.EventDateTime{DateTime: "2026-09-04T09:00:00+10:00", TimeZone: sydney}
	old.End = &calendar.EventDateTime{DateTime: "2026-09-04T09:30:00+10:00", TimeZone: sydney}
	h.g.Put(old)
	h.cycleTwice() // the fake's full listing leaves out a master whose first occurrence is before timeMin
	if len(h.c.Hrefs()) != 0 {
		t.Fatalf("setup: master was listed")
	}
	gets := h.g.GetCalls
	h.g.PutInstance("s", friday(1), func(ev *calendar.Event) { ev.Summary = "Only this week" })
	rep := h.cycleTwice()
	if rep.Created != 1 || h.g.GetCalls != gets+1 || !strings.Contains(raw(h, "s"), "SUMMARY:Only this week") {
		t.Fatalf("report %+v, %d Get calls", rep, h.g.GetCalls-gets)
	}
}

func TestCancelOneOccurrence(t *testing.T) {
	h := newHarness(t)
	h.g.Put(series("s"))
	h.cycleTwice()
	// The cancelled instance comes back as a bare tombstone (P0-1): only its
	// id ties it to the series.
	h.g.PutInstance("s", friday(2), func(ev *calendar.Event) { ev.Status = "cancelled" })
	rep := h.cycleTwice()
	if rep.Updated != 1 || !strings.Contains(raw(h, "s"), "EXDATE;TZID=Australia/Sydney:20261204T090000") {
		t.Fatalf("report %+v\n%s", rep, raw(h, "s"))
	}
}

func TestThisAndFollowingSplit(t *testing.T) {
	h := newHarness(t)
	h.g.Put(series("s"))
	h.cycleTwice()
	// The organizer changes "this and following" from 4 Dec: Google ends
	// the old series and creates a new master (new id, same iCalUID here).
	h.g.Update("s", func(ev *calendar.Event) {
		ev.Recurrence = []string{"RRULE:FREQ=WEEKLY;BYDAY=FR;UNTIL=20261203T125959Z"}
	})
	tail := series("s_R20261204")
	tail.ICalUID = "s@google.com"
	tail.Summary = "Weekly s (new time)"
	tail.Start = &calendar.EventDateTime{DateTime: "2026-12-04T10:00:00+11:00", TimeZone: sydney}
	tail.End = &calendar.EventDateTime{DateTime: "2026-12-04T10:30:00+11:00", TimeZone: sydney}
	h.g.Put(tail)
	rep := h.cycleTwice()
	if rep.Updated != 1 || rep.Created != 1 || len(h.c.Hrefs()) != 2 {
		t.Fatalf("report %+v hrefs %v", rep, h.c.Hrefs())
	}
	if !strings.Contains(raw(h, "s"), "UNTIL=20261203T125959Z") {
		t.Error("old series not truncated")
	}
	if m := h.mapping("s_R20261204"); m.ICalUID != "calbridge-s_R20261204@YOUR_DOMAIN" {
		t.Errorf("new series UID %s", m.ICalUID)
	}
}

func TestAcceptAllPatchesMasterOnce(t *testing.T) {
	h := newHarness(t)
	h.g.Put(series("s"))
	h.cycleTwice()
	h.c.AppleRSVP(hrefFor("s"), selfEmail, "ACCEPTED")
	h.cycleTwice()
	if len(h.g.Patches) != 1 || h.g.Patches[0].EventID != "s" || h.g.Patches[0].Status != "accepted" {
		t.Fatalf("patches %+v", h.g.Patches)
	}
}

func TestAnswerOneOccurrence(t *testing.T) {
	h := newHarness(t)
	h.g.Put(series("s"))
	h.cycleTwice()
	appleAnswerOccurrence(h, "s", 1, "DECLINED")
	h.cycleTwice()
	h.cycleTwice()
	want := "s_" + key(1)
	if len(h.g.Patches) != 1 || h.g.Patches[0].EventID != want || h.g.Patches[0].Status != "declined" {
		t.Fatalf("patches %+v; want one decline of %s", h.g.Patches, want)
	}
	if ev := h.g.Event(want); ev == nil || ev.Attendees[1].ResponseStatus != "declined" {
		t.Fatal("Google exception not declined")
	}
	ps, ov := overridesOf(t, h, "s")
	if ps != translate.PartStatNeedsAction || ov[key(1)] != translate.PartStatDeclined {
		t.Errorf("mirror master %s overrides %v", ps, ov)
	}
	// Changing that occurrence's answer again sends one more patch.
	appleAnswerOccurrence(h, "s", 1, "TENTATIVE")
	h.cycleTwice()
	if len(h.g.Patches) != 2 || h.g.Patches[1].Status != "tentative" {
		t.Fatalf("patches %+v", h.g.Patches)
	}
}

func TestOccurrenceAnswerSurvivesSeriesEdit(t *testing.T) {
	h := newHarness(t)
	h.g.Put(series("s"))
	h.cycleTwice()
	appleAnswerOccurrence(h, "s", 2, "TENTATIVE")
	h.g.Update("s", func(ev *calendar.Event) { ev.Location = "Room 2" }) // same interval
	h.cycleTwice()
	if _, ov := overridesOf(t, h, "s"); ov[key(2)] != translate.PartStatTentative {
		t.Fatalf("occurrence answer lost: %v", ov)
	}
	if len(h.g.Patches) != 1 || h.g.Patches[0].EventID != "s_"+key(2) {
		t.Fatalf("patches %+v", h.g.Patches)
	}
	if !strings.Contains(raw(h, "s"), "LOCATION:Room 2") {
		t.Error("series edit lost")
	}
}

func TestGoogleOccurrenceResponseReachesApple(t *testing.T) {
	h := newHarness(t)
	h.g.Put(series("s"))
	h.cycleTwice()
	h.g.PutInstance("s", friday(3), func(ev *calendar.Event) { ev.Attendees[1].ResponseStatus = "accepted" })
	h.cycleTwice()
	if _, ov := overridesOf(t, h, "s"); ov[key(3)] != translate.PartStatAccepted || len(h.g.Patches) != 0 {
		t.Fatalf("overrides %v patches %v", ov, h.g.Patches)
	}
}

func TestAllDaySeriesOccurrenceAnswer(t *testing.T) {
	h := newHarness(t)
	ev := series("d")
	ev.Start, ev.End = &calendar.EventDateTime{Date: "2026-11-20"}, &calendar.EventDateTime{Date: "2026-11-21"}
	ev.Recurrence = []string{"RRULE:FREQ=WEEKLY;COUNT=4"}
	h.g.Put(ev)
	h.cycleTwice()
	h.c.AppleEdit(hrefFor("d"), func(cal *ical.Calendar) {
		master := cal.Children[len(cal.Children)-1]
		ov := ical.NewComponent(ical.CompEvent)
		for _, name := range []string{ical.PropUID, ical.PropDateTimeStamp, ical.PropOrganizer, ical.PropAttendee, ical.PropSummary} {
			for _, p := range master.Props[name] {
				cp := p
				cp.Params = ical.Params{}
				for k, v := range p.Params {
					cp.Params[k] = append([]string(nil), v...)
				}
				ov.Props.Add(&cp)
			}
		}
		for name, v := range map[string]string{ical.PropRecurrenceID: "20261127", ical.PropDateTimeStart: "20261127", ical.PropDateTimeEnd: "20261128"} {
			p := ical.NewProp(name)
			p.SetValueType(ical.ValueDate)
			p.Value = v
			ov.Props.Set(p)
		}
		setSelfPartStat(ov, "ACCEPTED")
		cal.Children = append(cal.Children, ov)
	})
	h.cycleTwice()
	if len(h.g.Patches) != 1 || h.g.Patches[0].EventID != "d_20261127" {
		t.Fatalf("patches %+v", h.g.Patches)
	}
}

// TestRecurringIdempotencyProperty: random organizer and user actions on
// series converge, a second cycle never writes, and every response the user
// sees in Apple matches Google's.
func TestRecurringIdempotencyProperty(t *testing.T) {
	statuses := []string{"ACCEPTED", "TENTATIVE", "DECLINED"}
	gStatuses := []string{"needsAction", "accepted", "tentative", "declined"}
	for seed := int64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			h := newHarness(t)
			rng := rand.New(rand.NewSource(seed))
			ids := []string{"s1", "s2"}
			for _, id := range ids {
				h.g.Put(series(id))
			}
			h.cycleTwice()
			for step := 0; step < 30; step++ {
				id, n := ids[rng.Intn(len(ids))], rng.Intn(6)
				switch rng.Intn(7) {
				case 0:
					h.g.PutInstance(id, friday(n), func(ev *calendar.Event) {
						ev.Start.DateTime = friday(n).Add(2 * time.Hour).Format(time.RFC3339)
						ev.End.DateTime = friday(n).Add(150 * time.Minute).Format(time.RFC3339)
					})
				case 1:
					h.g.PutInstance(id, friday(n), func(ev *calendar.Event) { ev.Status = "cancelled" })
				case 2:
					h.g.PutInstance(id, friday(n), func(ev *calendar.Event) {
						if ev.Status != "cancelled" {
							ev.Attendees[1].ResponseStatus = gStatuses[rng.Intn(4)]
						}
					})
				case 3:
					h.g.Update(id, func(ev *calendar.Event) { ev.Summary = fmt.Sprintf("S%d", rng.Intn(100)) })
				case 4:
					h.c.AppleRSVP(hrefFor(id), selfEmail, statuses[rng.Intn(3)])
				case 5:
					if ex := h.g.Event(id + "_" + key(n)); ex == nil || ex.Status != "cancelled" {
						appleAnswerOccurrence(h, id, n, statuses[rng.Intn(3)])
					}
				case 6:
					h.g.ExpireSyncTokens()
				}
				h.cycleTwice()
				checkSeriesConverged(t, h, ids)
			}
			exc := 0
			for _, ev := range h.g.Events() {
				if ev.RecurringEventId != "" {
					exc++
				}
			}
			t.Logf("PUTs %d, RSVP patches %d, Google exceptions %d", h.c.Puts, len(h.g.Patches), exc)
			if h.c.Puts < 10 || len(h.g.Patches) < 2 || exc < 3 {
				t.Fatalf("property run too quiet")
			}
		})
	}
}

func checkSeriesConverged(t *testing.T, h *harness, ids []string) {
	t.Helper()
	for _, id := range ids {
		master := h.g.Event(id)
		var exceptions []*calendar.Event
		for _, ev := range h.g.Events() {
			if ev.RecurringEventId == id {
				exceptions = append(exceptions, ev)
			}
		}
		obj := h.c.Object(hrefFor(id))
		if obj == nil {
			t.Fatalf("no mirror for %s", id)
		}
		want, err := translate.RenderSeries(master, exceptions, translate.RSVPView{}, obj.Cal,
			h.e.newRun(&h.s.Queries).renderOptions(id+"@google.com", nil))
		if err != nil {
			t.Fatal(err)
		}
		if translate.ContentHash(withoutSelf(want), selfEmail) != translate.ContentHash(withoutSelf(obj.Cal), selfEmail) {
			t.Fatalf("mirror of %s differs from Google:\n%s", id, obj.Raw)
		}
		ps, ov, err := translate.ExtractSelfStatus(obj.Cal, selfEmail)
		if err != nil {
			t.Fatal(err)
		}
		if ps != translate.GoogleToPartStat(master.Attendees[1].ResponseStatus) {
			t.Fatalf("%s series: Apple %s, Google %s", id, ps, master.Attendees[1].ResponseStatus)
		}
		for _, ex := range exceptions {
			if ex.Status == "cancelled" {
				continue
			}
			k, _ := translate.OccurrenceKey(master, ex)
			if got, want := ov[k], translate.GoogleToPartStat(ex.Attendees[1].ResponseStatus); got != want {
				t.Fatalf("%s occurrence %s: Apple %s, Google %s", id, k, got, want)
			}
		}
	}
}

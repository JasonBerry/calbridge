package translate

import (
	"testing"
	"time"

	"github.com/emersion/go-ical"
	calendar "google.golang.org/api/calendar/v3"
)

func TestOriginalStart(t *testing.T) {
	tests := []struct {
		ev   calendar.Event
		want occurrence
		ok   bool
	}{
		{calendar.Event{OriginalStartTime: &calendar.EventDateTime{Date: "2026-11-27"}}, occurrence{date: "2026-11-27"}, true},
		{calendar.Event{OriginalStartTime: &calendar.EventDateTime{DateTime: "2026-11-27T09:00:00+11:00"}},
			occurrence{at: time.Date(2026, 11, 26, 22, 0, 0, 0, time.UTC)}, true},
		{calendar.Event{Id: "abc_20261126T220000Z"}, occurrence{at: time.Date(2026, 11, 26, 22, 0, 0, 0, time.UTC)}, true},
		{calendar.Event{Id: "abc_20261127"}, occurrence{date: "2026-11-27"}, true},
		{calendar.Event{Id: "abc"}, occurrence{}, false},
		{calendar.Event{Id: "abc_notadate"}, occurrence{}, false},
	}
	for _, tc := range tests {
		got, ok := originalStart(&tc.ev)
		if ok != tc.ok || got.date != tc.want.date || !got.at.Equal(tc.want.at) {
			t.Errorf("originalStart(%+v) = %+v, %v; want %+v, %v", tc.ev, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseContentLine(t *testing.T) {
	pr, err := parseContentLine(`EXDATE;TZID="Australia/Sydney";VALUE=DATE-TIME:20261127T090000,20261204T090000`)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Name != "EXDATE" || pr.Params.Get("TZID") != "Australia/Sydney" || pr.Value != "20261127T090000,20261204T090000" {
		t.Errorf("parsed %+v", pr)
	}
	for _, bad := range []string{"RRULE FREQ=DAILY", ":x", "EXDATE;TZID:1"} {
		if _, err := parseContentLine(bad); err == nil {
			t.Errorf("parseContentLine(%q) accepted", bad)
		}
	}
}

func TestRRuleUntil(t *testing.T) {
	u, ok := rruleUntil([]string{"RRULE:FREQ=WEEKLY;UNTIL=20261231T130000Z;BYDAY=FR", "EXDATE:20261127T090000Z", "RRULE:FREQ=DAILY;UNTIL=20270115"})
	if !ok || !u.Equal(time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("= %v %v", u, ok)
	}
	if _, ok := rruleUntil([]string{"RRULE:FREQ=WEEKLY;COUNT=4"}); ok {
		t.Error("COUNT reported as UNTIL")
	}
}

// TestOverridePartStatNotInHash: the user's response on any occurrence is
// tracked by the RSVP state machine, not the content hash.
func TestOverridePartStatNotInHash(t *testing.T) {
	m, exc := loadEvent(t, "rec-moved-exception"), loadExceptions(t, "rec-moved-exception")
	a, err := RenderSeries(m, exc, RSVPView{}, nil, baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderSeries(m, exc, RSVPView{Overrides: map[string]PartStat{"20261126T220000Z": PartStatDeclined}}, nil, baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if ContentHash(a, selfEmail) != ContentHash(b, selfEmail) {
		t.Error("override PARTSTAT changed the content hash")
	}
	_, ov, err := ExtractSelfStatus(b, selfEmail)
	if err != nil || ov["20261126T220000Z"] != PartStatDeclined {
		t.Errorf("overrides %v %v", ov, err)
	}
	// Moving the exception does change the hash.
	exc[0].Start.DateTime = "2026-11-27T12:00:00+11:00"
	c, _ := RenderSeries(m, exc, RSVPView{}, nil, baseOpts())
	if ContentHash(c, selfEmail) == ContentHash(a, selfEmail) {
		t.Error("moving an exception did not change the hash")
	}
}

func TestSyntheticOverrideAllDay(t *testing.T) {
	m := loadEvent(t, "rec-allday")
	cal, err := RenderSeries(m, nil, RSVPView{Overrides: map[string]PartStat{"20261211": PartStatDeclined}}, nil, baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	ov := overrideEvents(cal)["20261211"]
	if ov == nil {
		t.Fatal("no synthetic override")
	}
	if ov.Props.Get(ical.PropDateTimeStart).Value != "20261211" || ov.Props.Get(ical.PropDateTimeEnd).Value != "20261212" {
		t.Errorf("synthetic all-day times %s-%s", ov.Props.Get(ical.PropDateTimeStart).Value, ov.Props.Get(ical.PropDateTimeEnd).Value)
	}
}

func TestUnplaceableExceptionSkipped(t *testing.T) {
	m := loadEvent(t, "rec-weekly")
	cal, err := RenderSeries(m, []*calendar.Event{{Id: "no-suffix", Status: "cancelled"}}, RSVPView{}, nil, baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(cal.Children) != 1 || masterEvent(cal).Props.Get(ical.PropExceptionDates) != nil {
		t.Error("unplaceable exception rendered")
	}
}

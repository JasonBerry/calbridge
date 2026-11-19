package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-ical"
	"github.com/google/go-cmp/cmp"
	calendar "google.golang.org/api/calendar/v3"
)

var update = flag.Bool("update", false, "rewrite golden .ics files")

const selfEmail = "user@YOUR_DOMAIN"

var fixedNow = time.Date(2026, 11, 19, 1, 2, 3, 0, time.UTC)

func baseOpts() RenderOptions {
	return RenderOptions{
		SelfEmail:     selfEmail,
		ScheduleAgent: "NONE", // P0-4 decision
		Privacy:       PrivacyHonorPrivate,
		Now:           fixedNow,
	}
}

func loadEvent(t *testing.T, name string) *calendar.Event {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "golden", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var ev calendar.Event
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return &ev
}

// loadExceptions reads testdata/golden/<name>.exceptions.json if present.
func loadExceptions(t *testing.T, name string) []*calendar.Event {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "golden", name+".exceptions.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var evs []*calendar.Event
	if err := json.Unmarshal(b, &evs); err != nil {
		t.Fatalf("%s exceptions: %v", name, err)
	}
	return evs
}

func loadICS(t *testing.T, path string) *ical.Calendar {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cal, err := ical.NewDecoder(bytes.NewReader(b)).Decode()
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return cal
}

func decode(t *testing.T, b []byte) *ical.Calendar {
	t.Helper()
	cal, err := ical.NewDecoder(bytes.NewReader(b)).Decode()
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	return cal
}

type goldenCase struct {
	name  string // output testdata/golden/<name>.ics
	input string // input testdata/golden/<input>.json (default name)
	opts  func(*RenderOptions)
	rsvp  RSVPView
	prev  string // testdata/prev/<prev>.ics
}

var goldenCases = []goldenCase{
	{name: "meet-invite"},
	{name: "meet-phone"},
	{name: "allday-single"},
	{name: "allday-multi"},
	{name: "timed-new-york"},
	{name: "timed-two-zones"},
	{name: "timed-utc"},
	{name: "timed-dst-start"},
	{name: "private"},
	{name: "private-full", input: "private", opts: func(o *RenderOptions) { o.Privacy = PrivacyFull }},
	{name: "busy-only", input: "meet-invite", opts: func(o *RenderOptions) { o.Privacy = PrivacyBusyOnly }},
	{name: "html-description"},
	{name: "optional-accepted"},
	{name: "group-organizer"},
	{name: "no-title"},
	{name: "long-unicode"},
	{name: "guests"},
	{name: "guests-self-only", input: "guests", opts: func(o *RenderOptions) { o.Attendees = AttendeesSelf }},
	{name: "guests-busy-only", input: "guests", opts: func(o *RenderOptions) { o.Privacy = PrivacyBusyOnly }},
	{name: "no-schedule-agent", input: "meet-invite", opts: func(o *RenderOptions) { o.ScheduleAgent = "" }},
	{name: "alarm-on-create", input: "meet-invite", opts: func(o *RenderOptions) {
		o.DefaultAlarms = []time.Duration{10 * time.Minute, 24 * time.Hour}
	}},
	{name: "update-preserves-apple", input: "meet-invite", prev: "apple-rsvp", rsvp: RSVPView{Master: PartStatAccepted},
		opts: func(o *RenderOptions) { o.DefaultAlarms = []time.Duration{10 * time.Minute} }},
	{name: "min-sequence", input: "meet-invite", opts: func(o *RenderOptions) { o.MinSequence = 4 }},
	// M6: generated VTIMEZONEs (emit_vtimezone: true) and zone-name handling.
	{name: "vtimezone-sydney", input: "meet-invite", opts: func(o *RenderOptions) { o.EmitVTimezone = true }},
	{name: "vtimezone-two-zones", input: "timed-two-zones", opts: func(o *RenderOptions) { o.EmitVTimezone = true }},
	{name: "vtimezone-allday", input: "allday-single", opts: func(o *RenderOptions) { o.EmitVTimezone = true }},
	{name: "windows-zone"},
	{name: "unknown-zone"},
	// M9: recurring series (input <name>.json + optional <name>.exceptions.json).
	{name: "rec-weekly"},
	{name: "rec-count-exdate"},
	{name: "rec-moved-exception"},
	{name: "rec-cancelled-exceptions"},
	{name: "rec-allday"},
	{name: "rec-dst"},
	{name: "rec-dst-vtimezone", input: "rec-dst", opts: func(o *RenderOptions) { o.EmitVTimezone = true }},
	{name: "rec-rsvp-overrides", rsvp: RSVPView{Overrides: map[string]PartStat{
		"20261126T220000Z": PartStatDeclined,  // Google has this exception: Apple's answer wins for it
		"20261203T220000Z": PartStatTentative, // no Google exception yet: synthetic override
	}}},
	{name: "rec-prev-override", input: "rec-moved-exception", prev: "rec-apple-override",
		opts: func(o *RenderOptions) { o.DefaultAlarms = []time.Duration{10 * time.Minute} }},
	{name: "rec-alarms-on-create", input: "rec-moved-exception",
		opts: func(o *RenderOptions) { o.DefaultAlarms = []time.Duration{10 * time.Minute} }},
}

func renderCase(t *testing.T, tc goldenCase) (*ical.Calendar, []byte) {
	t.Helper()
	in := tc.input
	if in == "" {
		in = tc.name
	}
	o := baseOpts()
	if tc.opts != nil {
		tc.opts(&o)
	}
	var prev *ical.Calendar
	if tc.prev != "" {
		prev = loadICS(t, filepath.Join("testdata", "prev", tc.prev+".ics"))
	}
	cal, err := RenderSeries(loadEvent(t, in), loadExceptions(t, in), tc.rsvp, prev, o)
	if err != nil {
		t.Fatalf("RenderSeries: %v", err)
	}
	b, err := Encode(cal)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return cal, b
}

func TestGolden(t *testing.T) {
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := renderCase(t, tc)
			path := filepath.Join("testdata", "golden", tc.name+".ics")
			if *update {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if diff := cmp.Diff(string(want), string(got)); diff != "" {
				t.Errorf("rendering changed (-golden +got); review and rerun with -update:\n%s", diff)
			}
			for i, line := range strings.Split(strings.TrimSuffix(string(got), "\r\n"), "\r\n") {
				if len(line) > 75 {
					t.Errorf("line %d is %d octets (> 75)", i+1, len(line))
				}
				if !utf8.ValidString(line) {
					t.Errorf("line %d splits a UTF-8 sequence", i+1)
				}
			}
		})
	}
}

// TestRoundTripExtract: what we render, encode and decode yields the
// PARTSTAT we intended, and the text survives folding.
func TestRoundTripExtract(t *testing.T) {
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			cal, b := renderCase(t, tc)
			back := decode(t, b)
			want, wantOv, err := ExtractSelfStatus(cal, selfEmail)
			if err != nil {
				t.Fatal(err)
			}
			got, gotOv, err := ExtractSelfStatus(back, strings.ToUpper(selfEmail))
			if err != nil || got != want {
				t.Fatalf("round trip = %s, %v; want %s", got, err, want)
			}
			if diff := cmp.Diff(wantOv, gotOv); diff != "" {
				t.Fatalf("override PARTSTATs changed in round trip (-want +got):\n%s", diff)
			}
			if hasRRule := masterEvent(back).Props.Get(ical.PropRecurrenceRule) != nil; hasRRule != strings.HasPrefix(tc.name, "rec-") {
				t.Errorf("master has RRULE = %v", hasRRule)
			}
			for _, prop := range []string{ical.PropSummary, ical.PropDescription, ical.PropLocation} {
				w, _ := cal.Children[0].Props.Text(prop)
				g, _ := back.Children[0].Props.Text(prop)
				if w != g {
					t.Errorf("%s changed in round trip:\nwant %q\ngot  %q", prop, w, g)
				}
			}
			if ContentHash(cal, selfEmail) != ContentHash(back, selfEmail) {
				t.Error("ContentHash differs after encode/decode")
			}
		})
	}
}

func TestPartStatOfSelf(t *testing.T) {
	tests := []struct {
		input string
		rsvp  RSVPView
		want  PartStat
	}{
		{"meet-invite", RSVPView{}, PartStatNeedsAction},
		{"optional-accepted", RSVPView{}, PartStatAccepted},
		{"group-organizer", RSVPView{}, PartStatDeclined},
		// A pending Apple-side change is rendered instead of Google's value (§8.4).
		{"meet-invite", RSVPView{Master: PartStatTentative}, PartStatTentative},
	}
	for _, tc := range tests {
		o := baseOpts()
		cal, err := RenderSeries(loadEvent(t, tc.input), nil, tc.rsvp, nil, o)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := ExtractSelfStatus(cal, selfEmail)
		if err != nil || got != tc.want {
			t.Errorf("%s %+v: PARTSTAT %s, %v; want %s", tc.input, tc.rsvp, got, err, tc.want)
		}
	}
}

func TestExtractFromAppleObject(t *testing.T) {
	cal := loadICS(t, filepath.Join("testdata", "prev", "apple-rsvp.ics"))
	ps, ov, err := ExtractSelfStatus(cal, selfEmail)
	if err != nil || ps != PartStatAccepted || len(ov) != 0 {
		t.Fatalf("= %s %v %v", ps, ov, err)
	}
	if _, _, err := ExtractSelfStatus(cal, "someone-else@example.com"); !errors.Is(err, ErrNoSelfAttendee) {
		t.Errorf("other address: %v; want ErrNoSelfAttendee", err)
	}
}

func TestExtractOverridesAndMatching(t *testing.T) {
	const ics = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\n" +
		"BEGIN:VEVENT\r\nUID:u\r\nDTSTAMP:20261118T000000Z\r\n" +
		"DTSTART;TZID=Australia/Sydney:20261120T100000\r\nRRULE:FREQ=WEEKLY\r\n" +
		"ATTENDEE;CN=Someone;PARTSTAT=ACCEPTED:mailto:other@example.com\r\n" +
		"ATTENDEE;PARTSTAT=tentative:MAILTO:User@YOUR_DOMAIN\r\nEND:VEVENT\r\n" +
		// Override in a TZID: key is UTC (10:00 AEDT = 23:00Z the day before).
		"BEGIN:VEVENT\r\nUID:u\r\nDTSTAMP:20261118T000000Z\r\n" +
		"RECURRENCE-ID;TZID=Australia/Sydney:20261127T100000\r\n" +
		"ATTENDEE;CUTYPE=UNKNOWN;EMAIL=user@your_domain;PARTSTAT=DECLINED:urn:uuid:x\r\nEND:VEVENT\r\n" +
		// Override with no PARTSTAT: NEEDS-ACTION.
		"BEGIN:VEVENT\r\nUID:u\r\nDTSTAMP:20261118T000000Z\r\n" +
		"RECURRENCE-ID:20261204T000000Z\r\nATTENDEE:mailto:user@YOUR_DOMAIN\r\nEND:VEVENT\r\n" +
		// Override without self: left out.
		"BEGIN:VEVENT\r\nUID:u\r\nDTSTAMP:20261118T000000Z\r\n" +
		"RECURRENCE-ID;VALUE=DATE:20261211\r\nATTENDEE:mailto:other@example.com\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	cal := decode(t, []byte(ics))
	ps, ov, err := ExtractSelfStatus(cal, selfEmail)
	if err != nil {
		t.Fatal(err)
	}
	if ps != PartStatTentative {
		t.Errorf("master = %s", ps)
	}
	want := map[string]PartStat{
		"20261126T230000Z": PartStatDeclined,
		"20261204T000000Z": PartStatNeedsAction,
	}
	if diff := cmp.Diff(want, ov); diff != "" {
		t.Errorf("overrides (-want +got):\n%s", diff)
	}
}

func TestExtractErrors(t *testing.T) {
	if _, _, err := ExtractSelfStatus(nil, selfEmail); err == nil {
		t.Error("nil calendar")
	}
	only := decode(t, []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:u\r\n"+
		"DTSTAMP:20261118T000000Z\r\nRECURRENCE-ID:20261204T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if _, _, err := ExtractSelfStatus(only, selfEmail); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("no master: %v", err)
	}
}

func TestContentHash(t *testing.T) {
	ev := loadEvent(t, "meet-invite")
	render := func(ev *calendar.Event, rsvp RSVPView, prev *ical.Calendar, mod func(*RenderOptions)) *ical.Calendar {
		t.Helper()
		o := baseOpts()
		if mod != nil {
			mod(&o)
		}
		cal, err := RenderSeries(ev, nil, rsvp, prev, o)
		if err != nil {
			t.Fatal(err)
		}
		return cal
	}
	base := ContentHash(render(ev, RSVPView{}, nil, nil), selfEmail)
	prev := loadICS(t, filepath.Join("testdata", "prev", "apple-rsvp.ics"))

	same := map[string]*ical.Calendar{
		"later DTSTAMP":       render(ev, RSVPView{}, nil, func(o *RenderOptions) { o.Now = fixedNow.Add(time.Hour) }),
		"other PARTSTAT":      render(ev, RSVPView{Master: PartStatDeclined}, nil, nil),
		"alarms on create":    render(ev, RSVPView{}, nil, func(o *RenderOptions) { o.DefaultAlarms = []time.Duration{time.Minute} }),
		"Apple props + alarm": render(ev, RSVPView{}, prev, nil),
	}
	evUpd := *ev
	evUpd.Updated = "2026-11-19T00:00:00.000Z" // another guest responded
	same["Google updated moved"] = render(&evUpd, RSVPView{}, nil, nil)
	for name, cal := range same {
		if h := ContentHash(cal, selfEmail); h != base {
			t.Errorf("%s changed the hash", name)
		}
	}

	differ := map[string]func(e *calendar.Event){
		"summary": func(e *calendar.Event) { e.Summary = "Renamed" },
		"start": func(e *calendar.Event) {
			e.Start = &calendar.EventDateTime{DateTime: "2026-11-20T09:30:00+11:00", TimeZone: "Australia/Sydney"}
		},
		"sequence": func(e *calendar.Event) { e.Sequence = 1 },
		"location": func(e *calendar.Event) { e.Location = "Room 1" },
		"organizer's response": func(e *calendar.Event) {
			a := *e.Attendees[0]
			a.ResponseStatus = "tentative"
			e.Attendees = []*calendar.EventAttendee{&a, e.Attendees[1]}
		},
		"guest added": func(e *calendar.Event) {
			e.Attendees = append(append([]*calendar.EventAttendee(nil), e.Attendees...),
				&calendar.EventAttendee{Email: "guest@example.com", ResponseStatus: "needsAction"})
		},
		"optional": func(e *calendar.Event) {
			a := *e.Attendees[1]
			a.Optional = true
			e.Attendees = []*calendar.EventAttendee{e.Attendees[0], &a}
		},
	}
	for name, mod := range differ {
		e := *ev
		mod(&e)
		if ContentHash(render(&e, RSVPView{}, nil, nil), selfEmail) == base {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
}

func TestRenderErrors(t *testing.T) {
	tests := []struct {
		name string
		mod  func(e *calendar.Event, o *RenderOptions) []*calendar.Event
		want error
	}{
		{"cancelled", func(e *calendar.Event, _ *RenderOptions) []*calendar.Event { e.Status = "cancelled"; return nil }, ErrCancelled},
		{"instance as master", func(e *calendar.Event, _ *RenderOptions) []*calendar.Event { e.RecurringEventId = "m"; return nil }, ErrInvalidEvent},
		{"exceptions without recurrence", func(e *calendar.Event, _ *RenderOptions) []*calendar.Event {
			return []*calendar.Event{{Id: "x"}}
		}, ErrInvalidEvent},
		{"bad recurrence line", func(e *calendar.Event, _ *RenderOptions) []*calendar.Event {
			e.Recurrence = []string{"RRULE FREQ=WEEKLY"}
			return nil
		}, ErrInvalidEvent},
		{"not invited", func(e *calendar.Event, _ *RenderOptions) []*calendar.Event {
			e.Attendees = e.Attendees[:1]
			return nil
		}, ErrNoSelfAttendee},
		{"no organizer", func(e *calendar.Event, _ *RenderOptions) []*calendar.Event { e.Organizer = nil; return nil }, ErrNoOrganizer},
		{"no uid", func(e *calendar.Event, _ *RenderOptions) []*calendar.Event { e.ICalUID = ""; return nil }, ErrInvalidEvent},
		{"no end", func(e *calendar.Event, _ *RenderOptions) []*calendar.Event { e.End = nil; return nil }, ErrInvalidEvent},
		{"mixed all-day", func(e *calendar.Event, _ *RenderOptions) []*calendar.Event {
			e.End = &calendar.EventDateTime{Date: "2026-11-21"}
			return nil
		}, ErrInvalidEvent},
		{"bad dateTime", func(e *calendar.Event, _ *RenderOptions) []*calendar.Event {
			e.Start = &calendar.EventDateTime{DateTime: "tomorrow"}
			return nil
		}, ErrInvalidEvent},
		{"no self email", func(_ *calendar.Event, o *RenderOptions) []*calendar.Event { o.SelfEmail = ""; return nil }, ErrInvalidEvent},
		{"privacy", func(_ *calendar.Event, o *RenderOptions) []*calendar.Event { o.Privacy = "public"; return nil }, ErrUnknownPrivacyMode},
		{"attendee mode", func(_ *calendar.Event, o *RenderOptions) []*calendar.Event { o.Attendees = "some"; return nil }, ErrUnknownAttendeeMode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := loadEvent(t, "meet-invite")
			o := baseOpts()
			exc := tc.mod(ev, &o)
			if _, err := RenderSeries(ev, exc, RSVPView{}, nil, o); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v; want %v", err, tc.want)
			}
		})
	}
	if _, err := RenderSeries(nil, nil, RSVPView{}, nil, baseOpts()); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("nil event: %v", err)
	}
	// emit_vtimezone does not matter for all-day/UTC events.
	o := baseOpts()
	o.EmitVTimezone = true
	if _, err := RenderSeries(loadEvent(t, "allday-single"), nil, RSVPView{}, nil, o); err != nil {
		t.Errorf("all-day with emit_vtimezone: %v", err)
	}
}

func TestRenderDefaults(t *testing.T) {
	o := baseOpts()
	o.Privacy = ""
	cal, err := RenderSeries(loadEvent(t, "private"), nil, RSVPView{}, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	ev := cal.Children[0]
	if s, _ := ev.Props.Text(ical.PropSummary); s != "Doctor" {
		t.Errorf("default privacy is not full: summary %q", s)
	}
	if g, _ := ev.Props.Text(PropGoogleCalendar); g != "primary" {
		t.Errorf("default calendar id %q", g)
	}
	o.UID = "calbridge-evt09@YOUR_DOMAIN"
	cal, _ = RenderSeries(loadEvent(t, "private"), nil, RSVPView{}, nil, o)
	if u, _ := cal.Children[0].Props.Text(ical.PropUID); u != o.UID {
		t.Errorf("UID override ignored: %q", u)
	}
}

// TestPrevWithoutAlarmGetsNone: once an object exists, default alarms are
// never re-added (the user may have deleted them, §7.7).
func TestPrevWithoutAlarmGetsNone(t *testing.T) {
	o := baseOpts()
	o.DefaultAlarms = []time.Duration{10 * time.Minute}
	first, err := RenderSeries(loadEvent(t, "meet-invite"), nil, RSVPView{}, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	first.Children[0].Children = nil // user deleted the alarm in Apple
	again, err := RenderSeries(loadEvent(t, "meet-invite"), nil, RSVPView{}, first, o)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(again.Children[0].Children); n != 0 {
		t.Errorf("update re-added %d alarm(s)", n)
	}
}

func TestPartStatMapping(t *testing.T) {
	for g, p := range map[string]PartStat{
		"accepted": PartStatAccepted, "tentative": PartStatTentative, "declined": PartStatDeclined,
		"needsAction": PartStatNeedsAction, "": PartStatNeedsAction, "weird": PartStatNeedsAction,
	} {
		if got := GoogleToPartStat(g); got != p {
			t.Errorf("GoogleToPartStat(%q) = %s; want %s", g, got, p)
		}
	}
	for p, want := range map[PartStat]string{
		PartStatAccepted: "accepted", "accepted": "accepted", PartStatTentative: "tentative", PartStatDeclined: "declined",
		PartStatNeedsAction: "", "DELEGATED": "", "IN-PROCESS": "", "": "",
	} {
		got, ok := PartStatToGoogle(p)
		if got != want || ok != (want != "") {
			t.Errorf("PartStatToGoogle(%q) = %q, %v; want %q", p, got, ok, want)
		}
	}
}

func TestHTMLToText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain text\nsecond line", "plain text\nsecond line"},
		{"a < b and c > d", "a < b and c > d"},
		{"line1<br>line2<br/>line3<BR />", "line1\nline2\nline3"},
		{"<p>para one</p><p>para two</p>", "para one\npara two"},
		{`see <a href="https://x.example/a?b=1&amp;c=2">docs</a>`, "see docs (https://x.example/a?b=1&c=2)"},
		{`<a href="https://x.example">https://x.example</a>`, "https://x.example"},
		{`<a href="mailto:a@example.com">a@example.com</a>`, "a@example.com"},
		{"<ul><li>one</li><li>two</li></ul>", "• one\n• two"},
		{"Tom &amp; Jerry&nbsp;&lt;3 &#8211; ok", "Tom & Jerry <3 – ok"},
		{"<b>bold</b>\n<i>italic</i>", "bold italic"},
		{"<p>a</p><p></p><p></p><p>b</p>", "a\n\nb"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := HTMLToText(tc.in); got != tc.want {
			t.Errorf("HTMLToText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestFold(t *testing.T) {
	long := "DESCRIPTION:" + strings.Repeat("ab😀漢", 40)
	in := []byte("BEGIN:VCALENDAR\r\n" + long + "\r\nEND:VCALENDAR\r\n")
	out := Fold(in)
	lines := strings.Split(strings.TrimSuffix(string(out), "\r\n"), "\r\n")
	for i, l := range lines {
		if len(l) > 75 || !utf8.ValidString(l) {
			t.Errorf("line %d: %d octets, valid=%v", i, len(l), utf8.ValidString(l))
		}
		if i > 1 && i < len(lines)-1 && !strings.HasPrefix(l, " ") {
			t.Errorf("continuation line %d lacks leading space", i)
		}
	}
	unfolded := strings.ReplaceAll(string(out), "\r\n ", "")
	if unfolded != string(in) {
		t.Error("unfolding does not restore the input")
	}
	if got := Fold([]byte("SHORT:x\r\n")); string(got) != "SHORT:x\r\n" {
		t.Errorf("short line changed: %q", got)
	}
}

func TestTriggerValue(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Minute: "-PT10M", 2 * time.Hour: "-PT2H", 48 * time.Hour: "-P2D",
		90 * time.Second: "-PT90S", -15 * time.Minute: "-PT15M", 0: "PT0S",
	} {
		if got := triggerValue(d); got != want {
			t.Errorf("triggerValue(%v) = %s; want %s", d, got, want)
		}
	}
}

// TestGuestResponsesExtractOnlySelf: with every attendee listed, extraction
// still reads only the user's PARTSTAT, and a self-only hash input ignores
// self but not guests (P0-4 E).
func TestGuestResponsesExtractOnlySelf(t *testing.T) {
	ev := loadEvent(t, "guests")
	cal, err := RenderSeries(ev, nil, RSVPView{}, nil, baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(cal.Children[0].Props.Values(ical.PropAttendee)); n != len(ev.Attendees) {
		t.Fatalf("rendered %d attendees; Google has %d", n, len(ev.Attendees))
	}
	ps, _, err := ExtractSelfStatus(cal, selfEmail)
	if err != nil || ps != PartStatNeedsAction {
		t.Fatalf("self = %s, %v", ps, err)
	}
	// Apple's rewrite after the user accepts (P0-4 E): guests kept as-is.
	apple, err := RenderSeries(ev, nil, RSVPView{Master: PartStatAccepted}, nil, baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if ContentHash(apple, selfEmail) != ContentHash(cal, selfEmail) {
		t.Error("self PARTSTAT change altered the hash")
	}
	if ContentHash(apple, "") == ContentHash(cal, "") {
		t.Error("without a self address every PARTSTAT should count")
	}
}

func TestNormalizeRecurrenceIDRepeatedHour(t *testing.T) {
	// 02:30 on 5 Apr 2026 in Sydney happens twice; the TZID form means the
	// first (AEDT, 15:30Z the day before).
	pr := ical.NewProp(ical.PropRecurrenceID)
	pr.Params.Set(ical.ParamTimezoneID, "Australia/Sydney")
	pr.Value = "20260405T023000"
	if got := NormalizeRecurrenceID(pr); got != "20260404T153000Z" {
		t.Errorf("NormalizeRecurrenceID = %s; want 20260404T153000Z", got)
	}
	pr.Params.Set(ical.ParamTimezoneID, "AUS Eastern Standard Time") // alias
	pr.Value = "20261120T100000"
	if got := NormalizeRecurrenceID(pr); got != "20261119T230000Z" {
		t.Errorf("alias zone: %s", got)
	}
}

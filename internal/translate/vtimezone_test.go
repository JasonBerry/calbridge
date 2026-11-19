package translate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/google/go-cmp/cmp"
	calendar "google.golang.org/api/calendar/v3"
)

var goldenZones = []string{"Australia/Sydney", "America/New_York", "UTC", "Asia/Kolkata", "Australia/Lord_Howe"}

// spanFor2026 is the VTIMEZONE span for an event in Nov 2026.
func spanFor2026() (time.Time, time.Time) {
	ev := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	return vtimezoneSpan(ev, ev)
}

func encodeVTZ(t *testing.T, vtz *ical.Component) []byte {
	t.Helper()
	cal := ical.NewCalendar()
	setRaw(cal.Props, ical.PropProductID, ProdID)
	setRaw(cal.Props, ical.PropVersion, "2.0")
	cal.Children = append(cal.Children, vtz)
	b, err := Encode(cal)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVTimezoneGolden(t *testing.T) {
	from, to := spanFor2026()
	for _, zone := range goldenZones {
		t.Run(zone, func(t *testing.T) {
			vtz, err := VTimezone(zone, from, to)
			if err != nil {
				t.Fatal(err)
			}
			got := encodeVTZ(t, vtz)
			again, _ := VTimezone(zone, from, to)
			if string(encodeVTZ(t, again)) != string(got) {
				t.Fatal("output is not deterministic")
			}
			path := filepath.Join("testdata", "vtimezone", strings.ReplaceAll(zone, "/", "_")+".ics")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if diff := cmp.Diff(string(want), string(got)); diff != "" {
				t.Errorf("VTIMEZONE changed (-golden +got):\n%s", diff)
			}
		})
	}
}

// TestVTimezoneTransitionsComplete checks every offset change in the span,
// found by an independent 15-minute scan, appears exactly once as an onset
// (local time in TZOFFSETFROM) with the right offsets.
func TestVTimezoneTransitionsComplete(t *testing.T) {
	from, to := spanFor2026()
	for _, zone := range append(goldenZones, "Europe/London", "Pacific/Auckland", "America/Sao_Paulo") {
		t.Run(zone, func(t *testing.T) {
			loc, _ := time.LoadLocation(zone)
			type tr struct {
				at       time.Time
				from, to int
			}
			var want []tr
			_, prev := from.In(loc).Zone()
			for u := from; !u.After(to); u = u.Add(15 * time.Minute) {
				if _, off := u.In(loc).Zone(); off != prev {
					// refine to the exact minute
					at := u.Add(-15 * time.Minute)
					for {
						at = at.Add(time.Minute)
						if _, o := at.In(loc).Zone(); o != prev {
							break
						}
					}
					want = append(want, tr{at.UTC(), prev, off})
					prev = off
				}
			}

			vtz, err := VTimezone(zone, from, to)
			if err != nil {
				t.Fatal(err)
			}
			var got []tr
			for _, c := range vtz.Children {
				offFrom := parseOffset(t, c.Props.Get(ical.PropTimezoneOffsetFrom).Value)
				offTo := parseOffset(t, c.Props.Get(ical.PropTimezoneOffsetTo).Value)
				if offFrom == offTo {
					continue // the no-transition placeholder
				}
				onsets := []string{c.Props.Get(ical.PropDateTimeStart).Value}
				for _, r := range c.Props.Values(ical.PropRecurrenceDates) {
					onsets = append(onsets, r.Value)
				}
				for _, o := range onsets {
					local, err := time.Parse("20060102T150405", o)
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, tr{local.Add(-time.Duration(offFrom) * time.Second), offFrom, offTo})
				}
				wantDST := c.Name == ical.CompTimezoneDaylight
				if offTo > offFrom != wantDST && zone != "Europe/Dublin" {
					t.Errorf("%s component moves %d -> %d", c.Name, offFrom, offTo)
				}
			}
			sortTr := func(s []tr) {
				for i := range s {
					for j := i + 1; j < len(s); j++ {
						if s[j].at.Before(s[i].at) {
							s[i], s[j] = s[j], s[i]
						}
					}
				}
			}
			sortTr(got)
			if len(got) != len(want) {
				t.Fatalf("got %d transitions, want %d", len(got), len(want))
			}
			for i := range want {
				if !got[i].at.Equal(want[i].at) || got[i].from != want[i].from || got[i].to != want[i].to {
					t.Errorf("transition %d = %v %d->%d; want %v %d->%d",
						i, got[i].at, got[i].from, got[i].to, want[i].at, want[i].from, want[i].to)
				}
			}
			if zone == "Australia/Sydney" && len(want) != 6 {
				t.Errorf("Sydney 2025–2027 should have 6 transitions, scan found %d", len(want))
			}
		})
	}
}

func parseOffset(t *testing.T, s string) int {
	t.Helper()
	if len(s) != 5 && len(s) != 7 {
		t.Fatalf("bad offset %q", s)
	}
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	n := func(a, b int) int { return int(s[a]-'0')*10 + int(s[b]-'0') }
	sec := n(1, 2)*3600 + n(3, 4)*60
	if len(s) == 7 {
		sec += n(5, 6)
	}
	return sign * sec
}

func TestVTimezoneNoTransitions(t *testing.T) {
	from, to := spanFor2026()
	for zone, off := range map[string]string{"UTC": "+0000", "Asia/Kolkata": "+0530", "Asia/Tokyo": "+0900"} {
		vtz, err := VTimezone(zone, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if len(vtz.Children) != 1 || vtz.Children[0].Name != ical.CompTimezoneStandard {
			t.Fatalf("%s: %d sub-components", zone, len(vtz.Children))
		}
		c := vtz.Children[0]
		if c.Props.Get(ical.PropTimezoneOffsetFrom).Value != off || c.Props.Get(ical.PropTimezoneOffsetTo).Value != off {
			t.Errorf("%s offsets %s/%s; want %s", zone, c.Props.Get(ical.PropTimezoneOffsetFrom).Value,
				c.Props.Get(ical.PropTimezoneOffsetTo).Value, off)
		}
	}
}

func TestResolveZone(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"Australia/Sydney", "Australia/Sydney", true},
		{"AUS Eastern Standard Time", "Australia/Sydney", true},
		{"Eastern Standard Time", "America/New_York", true},
		{"US/Eastern", "US/Eastern", true}, // IANA backward link
		{"Mars/Olympus_Mons", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, loc, ok := ResolveZone(tc.in)
		if got != tc.want || ok != tc.ok || (ok && loc == nil) {
			t.Errorf("ResolveZone(%q) = %q, %v, %v; want %q, %v", tc.in, got, loc, ok, tc.want, tc.ok)
		}
	}
	if _, err := VTimezone("Mars/Olympus_Mons", time.Now(), time.Now()); err == nil {
		t.Error("VTimezone accepted an unknown zone")
	}
}

func TestUTCOffset(t *testing.T) {
	for sec, want := range map[int]string{0: "+0000", 39600: "+1100", 37800: "+1030", -18000: "-0500", 36292: "+100452", -12600: "-0330"} {
		if got := utcOffset(sec); got != want {
			t.Errorf("utcOffset(%d) = %s; want %s", sec, got, want)
		}
	}
}

// TestDSTEdges: times in the repeated hour after clocks go back (Sydney,
// 5 Apr 2026 03:00 AEDT -> 02:00 AEST) and right after clocks go forward.
func TestDSTEdges(t *testing.T) {
	tests := []struct {
		name, start string
		wantProp    string
	}{
		// First 02:30 (still AEDT): local TZID time is unambiguous per RFC 5545.
		{"first of repeated hour", "2026-04-05T02:30:00+11:00", "DTSTART;TZID=Australia/Sydney:20260405T023000"},
		// Second 02:30 (AEST): a TZID local time would mean the first one, so UTC.
		{"second of repeated hour", "2026-04-05T02:30:00+10:00", "DTSTART:20260404T163000Z"},
		{"just after the gap", "2026-10-04T03:00:00+11:00", "DTSTART;TZID=Australia/Sydney:20261004T030000"},
		{"just before the gap", "2026-10-04T01:59:00+10:00", "DTSTART;TZID=Australia/Sydney:20261004T015900"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := loadEvent(t, "meet-invite")
			st, _ := time.Parse(time.RFC3339, tc.start)
			ev.Start = &calendar.EventDateTime{DateTime: tc.start, TimeZone: "Australia/Sydney"}
			ev.End = &calendar.EventDateTime{DateTime: st.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "Australia/Sydney"}
			cal, err := RenderSeries(ev, nil, RSVPView{}, nil, baseOpts())
			if err != nil {
				t.Fatal(err)
			}
			b, err := Encode(cal)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), tc.wantProp+"\r\n") {
				t.Errorf("want %q in:\n%s", tc.wantProp, b)
			}
			// Whatever form was chosen, it must denote the original instant
			// when read per RFC 5545 (ParseLocal; go-ical's reader picks the
			// second occurrence of a repeated hour).
			pr := decode(t, b).Children[0].Props.Get(ical.PropDateTimeStart)
			var got time.Time
			if tz := pr.Params.Get(ical.ParamTimezoneID); tz != "" {
				loc, _ := time.LoadLocation(tz)
				got, err = ParseLocal(pr.Value, loc)
			} else {
				got, err = time.Parse("20060102T150405Z", pr.Value)
			}
			if err != nil || !got.Equal(st) {
				t.Errorf("DTSTART decodes to %v (%v); want %v", got, err, st)
			}
		})
	}
}

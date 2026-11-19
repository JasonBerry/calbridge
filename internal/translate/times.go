package translate

import (
	"fmt"
	"time"

	"github.com/emersion/go-ical"
	calendar "google.golang.org/api/calendar/v3"
)

// setTimes writes DTSTART/DTEND (§7.3) and returns the TZIDs used and the
// start/end instants (zero for all-day events).
func setTimes(p ical.Props, start, end *calendar.EventDateTime) ([]string, time.Time, time.Time, error) {
	if start == nil || end == nil {
		return nil, time.Time{}, time.Time{}, fmt.Errorf("%w: missing start or end", ErrInvalidEvent)
	}
	if (start.Date == "") != (end.Date == "") {
		return nil, time.Time{}, time.Time{}, fmt.Errorf("%w: start and end mix all-day and timed", ErrInvalidEvent)
	}
	if start.Date != "" {
		for _, d := range []struct {
			name string
			v    string
		}{{ical.PropDateTimeStart, start.Date}, {ical.PropDateTimeEnd, end.Date}} {
			t, err := time.Parse("2006-01-02", d.v)
			if err != nil {
				return nil, time.Time{}, time.Time{}, fmt.Errorf("%w: bad date %q", ErrInvalidEvent, d.v)
			}
			pr := ical.NewProp(d.name)
			pr.SetValueType(ical.ValueDate)
			pr.Value = t.Format("20060102")
			p.Set(pr)
		}
		return nil, time.Time{}, time.Time{}, nil
	}
	endZone := end.TimeZone
	if endZone == "" {
		endZone = start.TimeZone
	}
	var (
		tzids   []string
		instant [2]time.Time
	)
	for i, d := range []struct {
		name string
		dt   *calendar.EventDateTime
		zone string
	}{{ical.PropDateTimeStart, start, start.TimeZone}, {ical.PropDateTimeEnd, end, endZone}} {
		pr, tzid, t, err := dateTimeProp(d.name, d.dt.DateTime, d.zone)
		if err != nil {
			return nil, time.Time{}, time.Time{}, err
		}
		instant[i] = t
		p.Set(pr)
		if tzid != "" && (len(tzids) == 0 || tzids[0] != tzid) {
			tzids = append(tzids, tzid)
		}
	}
	return tzids, instant[0], instant[1], nil
}

// dateTimeProp renders a Google dateTime as TZID local time in zone (after
// ResolveZone's alias mapping), or as UTC when the zone is empty, UTC or
// unknown. A local time that does not map back to the same instant (the
// second occurrence of a repeated hour when clocks go back) is also written
// in UTC, since a TZID local time denotes the first occurrence (RFC 5545
// §3.3.5).
func dateTimeProp(name, value, zone string) (*ical.Prop, string, time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("%w: bad dateTime %q", ErrInvalidEvent, value)
	}
	pr := ical.NewProp(name)
	if iana, loc, ok := ResolveZone(zone); ok && iana != "UTC" && iana != "Etc/UTC" {
		local := t.In(loc).Format("20060102T150405")
		if !repeatedWallTime(t, loc) {
			pr.Params.Set(ical.ParamTimezoneID, iana)
			pr.Value = local
			return pr, iana, t, nil
		}
	}
	pr.Value = t.UTC().Format("20060102T150405Z")
	return pr, "", t, nil
}

// repeatedWallTime reports whether t's local wall time in loc already
// occurred earlier (t falls in the repeated hour after clocks go back).
func repeatedWallTime(t time.Time, loc *time.Location) bool {
	_, before := t.Add(-12 * time.Hour).In(loc).Zone()
	_, after := t.Add(12 * time.Hour).In(loc).Zone()
	d := before - after
	if d <= 0 {
		return false
	}
	earlier := t.Add(-time.Duration(d) * time.Second)
	return earlier.In(loc).Format("20060102T150405") == t.In(loc).Format("20060102T150405")
}

// ParseLocal parses an iCalendar local date-time in loc. A wall time that
// occurs twice (clocks going back) means its first occurrence (RFC 5545
// §3.3.5); Go's time.ParseInLocation does not guarantee that, so it is
// corrected here. A wall time in a spring-forward gap is resolved as Go does.
//
// UNVERIFIED (P0-8 covered DST-spanning series, not the repeated hour):
// Apple is assumed to follow RFC 5545 here too.
func ParseLocal(v string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation("20060102T150405", v, loc)
	if err != nil {
		return t, err
	}
	_, off := t.Zone()
	_, before := t.Add(-12 * time.Hour).In(loc).Zone()
	if d := before - off; d > 0 {
		if earlier := t.Add(-time.Duration(d) * time.Second); earlier.In(loc).Format("20060102T150405") == v {
			return earlier, nil
		}
	}
	return t, nil
}

// NormalizeRecurrenceID returns the store key for a RECURRENCE-ID property:
// DATE values as YYYYMMDD, date-times as UTC YYYYMMDDTHHMMSSZ. A TZID that
// cannot be loaded or a floating time is returned unchanged.
func NormalizeRecurrenceID(pr *ical.Prop) string {
	v := pr.Value
	if pr.ValueType() == ical.ValueDate || len(v) == 8 {
		return v
	}
	if len(v) > 0 && v[len(v)-1] == 'Z' {
		return v
	}
	tz := pr.Params.Get(ical.ParamTimezoneID)
	if tz == "" {
		return v
	}
	_, loc, ok := ResolveZone(tz)
	if !ok {
		return v
	}
	t, err := ParseLocal(v, loc)
	if err != nil {
		return v
	}
	return t.UTC().Format("20060102T150405Z")
}

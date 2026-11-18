package translate

import (
	"fmt"
	"time"

	"github.com/emersion/go-ical"
	calendar "google.golang.org/api/calendar/v3"
)

// setTimes writes DTSTART/DTEND (§7.3) and returns the TZIDs used.
func setTimes(p ical.Props, start, end *calendar.EventDateTime) ([]string, error) {
	if start == nil || end == nil {
		return nil, fmt.Errorf("%w: missing start or end", ErrInvalidEvent)
	}
	if (start.Date == "") != (end.Date == "") {
		return nil, fmt.Errorf("%w: start and end mix all-day and timed", ErrInvalidEvent)
	}
	if start.Date != "" {
		for _, d := range []struct {
			name string
			v    string
		}{{ical.PropDateTimeStart, start.Date}, {ical.PropDateTimeEnd, end.Date}} {
			t, err := time.Parse("2006-01-02", d.v)
			if err != nil {
				return nil, fmt.Errorf("%w: bad date %q", ErrInvalidEvent, d.v)
			}
			pr := ical.NewProp(d.name)
			pr.SetValueType(ical.ValueDate)
			pr.Value = t.Format("20060102")
			p.Set(pr)
		}
		return nil, nil
	}
	endZone := end.TimeZone
	if endZone == "" {
		endZone = start.TimeZone
	}
	var tzids []string
	for _, d := range []struct {
		name string
		dt   *calendar.EventDateTime
		zone string
	}{{ical.PropDateTimeStart, start, start.TimeZone}, {ical.PropDateTimeEnd, end, endZone}} {
		pr, tzid, err := dateTimeProp(d.name, d.dt.DateTime, d.zone)
		if err != nil {
			return nil, err
		}
		p.Set(pr)
		if tzid != "" && (len(tzids) == 0 || tzids[0] != tzid) {
			tzids = append(tzids, tzid)
		}
	}
	return tzids, nil
}

// dateTimeProp renders a Google dateTime in zone as a TZID local time, or as
// UTC when zone is empty or unknown to the embedded tzdata.
func dateTimeProp(name, value, zone string) (*ical.Prop, string, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, "", fmt.Errorf("%w: bad dateTime %q", ErrInvalidEvent, value)
	}
	pr := ical.NewProp(name)
	if zone != "" && zone != "UTC" && zone != "Etc/UTC" {
		// Unknown/legacy zone names fall back to UTC; M6 adds the alias table.
		if loc, err := time.LoadLocation(zone); err == nil {
			pr.Params.Set(ical.ParamTimezoneID, zone)
			pr.Value = t.In(loc).Format("20060102T150405")
			return pr, zone, nil
		}
	}
	pr.Value = t.UTC().Format("20060102T150405Z")
	return pr, "", nil
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
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return v
	}
	t, err := time.ParseInLocation("20060102T150405", v, loc)
	if err != nil {
		return v
	}
	return t.UTC().Format("20060102T150405Z")
}

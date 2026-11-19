package translate

import (
	"fmt"
	"sort"
	"time"

	"github.com/emersion/go-ical"
)

// zoneAliases maps Windows and other legacy zone names Google can return to
// IANA names (§7.3). IANA backward links (US/Eastern, …) load directly.
var zoneAliases = map[string]string{
	"AUS Eastern Standard Time":    "Australia/Sydney",
	"AUS Central Standard Time":    "Australia/Darwin",
	"Cen. Australia Standard Time": "Australia/Adelaide",
	"E. Australia Standard Time":   "Australia/Brisbane",
	"Tasmania Standard Time":       "Australia/Hobart",
	"W. Australia Standard Time":   "Australia/Perth",
	"Lord Howe Standard Time":      "Australia/Lord_Howe",
	"New Zealand Standard Time":    "Pacific/Auckland",
	"Eastern Standard Time":        "America/New_York",
	"Central Standard Time":        "America/Chicago",
	"Mountain Standard Time":       "America/Denver",
	"US Mountain Standard Time":    "America/Phoenix",
	"Pacific Standard Time":        "America/Los_Angeles",
	"Alaskan Standard Time":        "America/Anchorage",
	"Hawaiian Standard Time":       "Pacific/Honolulu",
	"GMT Standard Time":            "Europe/London",
	"W. Europe Standard Time":      "Europe/Berlin",
	"Romance Standard Time":        "Europe/Paris",
	"Central Europe Standard Time": "Europe/Budapest",
	"India Standard Time":          "Asia/Kolkata",
	"China Standard Time":          "Asia/Shanghai",
	"Tokyo Standard Time":          "Asia/Tokyo",
	"Singapore Standard Time":      "Asia/Singapore",
	"Korea Standard Time":          "Asia/Seoul",
	"UTC":                          "UTC",
	"Coordinated Universal Time":   "UTC",
}

// ResolveZone returns the IANA name and location for a Google timeZone
// value, following zoneAliases. ok is false when the zone is unknown; the
// renderer then writes UTC (the syncer may log a warning).
func ResolveZone(name string) (iana string, loc *time.Location, ok bool) {
	if name == "" {
		return "", nil, false
	}
	if a, found := zoneAliases[name]; found {
		name = a
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		return "", nil, false
	}
	return name, l, true
}

// VTimezone builds a VTIMEZONE for tzid covering [from, to] (§7.3): one
// STANDARD/DAYLIGHT sub-component per distinct (kind, name, offset-from,
// offset-to), with DTSTART at its first onset in the span and RDATEs for
// the later ones. Onsets are local times in TZOFFSETFROM. A zone with no
// transitions in the span gets a single STANDARD starting at from.
// Output is deterministic for the same inputs.
func VTimezone(tzid string, from, to time.Time) (*ical.Component, error) {
	iana, loc, ok := ResolveZone(tzid)
	if !ok {
		return nil, fmt.Errorf("%w: unknown time zone %q", ErrInvalidEvent, tzid)
	}
	if to.Before(from) {
		from, to = to, from
	}
	type key struct {
		dst      bool
		name     string
		off, out int
	}
	type obs struct {
		key    key
		onsets []time.Time // local wall times (in the from-offset), UTC-located for formatting
	}
	byKey := map[key]*obs{}
	var order []key
	add := func(k key, onsetLocal time.Time) {
		o, found := byKey[k]
		if !found {
			o = &obs{key: k}
			byKey[k] = o
			order = append(order, k)
		}
		o.onsets = append(o.onsets, onsetLocal)
	}

	t := from.In(loc)
	for {
		_, end := t.ZoneBounds()
		if end.IsZero() || end.After(to) {
			break
		}
		before := end.Add(-time.Second).In(loc)
		after := end.In(loc)
		_, offFrom := before.Zone()
		name, offTo := after.Zone()
		if offFrom != offTo || before.IsDST() != after.IsDST() {
			local := end.UTC().Add(time.Duration(offFrom) * time.Second)
			add(key{dst: after.IsDST(), name: name, off: offFrom, out: offTo}, local)
		}
		t = after
	}
	if len(order) == 0 {
		start := from.In(loc)
		name, off := start.Zone()
		local := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
		add(key{dst: start.IsDST(), name: name, off: off, out: off}, local)
	}

	vtz := ical.NewComponent(ical.CompTimezone)
	setRaw(vtz.Props, ical.PropTimezoneID, iana)
	sort.SliceStable(order, func(i, j int) bool {
		return byKey[order[i]].onsets[0].Before(byKey[order[j]].onsets[0])
	})
	for _, k := range order {
		o := byKey[k]
		name := ical.CompTimezoneStandard
		if k.dst {
			name = ical.CompTimezoneDaylight
		}
		c := ical.NewComponent(name)
		setRaw(c.Props, ical.PropDateTimeStart, o.onsets[0].Format("20060102T150405"))
		for _, r := range o.onsets[1:] {
			c.Props.Add(&ical.Prop{Name: ical.PropRecurrenceDates, Params: ical.Params{}, Value: r.Format("20060102T150405")})
		}
		setRaw(c.Props, ical.PropTimezoneOffsetFrom, utcOffset(k.off))
		setRaw(c.Props, ical.PropTimezoneOffsetTo, utcOffset(k.out))
		if k.name != "" {
			setText(c.Props, ical.PropTimezoneName, k.name)
		}
		vtz.Children = append(vtz.Children, c)
	}
	return vtz, nil
}

// utcOffset formats seconds east of UTC as ±hhmm or ±hhmmss.
func utcOffset(sec int) string {
	sign := "+"
	if sec < 0 {
		sign, sec = "-", -sec
	}
	h, m, s := sec/3600, sec/60%60, sec%60
	if s != 0 {
		return fmt.Sprintf("%s%02d%02d%02d", sign, h, m, s)
	}
	return fmt.Sprintf("%s%02d%02d", sign, h, m)
}

// vtimezoneSpan is the period a VTIMEZONE must cover for an event from
// start to end: 1 Jan of the year before start to 31 Dec of the year after
// end (§7.3). Recurring series extend end themselves (M9).
func vtimezoneSpan(start, end time.Time) (time.Time, time.Time) {
	from := time.Date(start.UTC().Year()-1, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(end.UTC().Year()+1, 12, 31, 23, 59, 59, 0, time.UTC)
	return from, to
}

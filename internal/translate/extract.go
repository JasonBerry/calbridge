package translate

import (
	"fmt"
	"strings"

	"github.com/emersion/go-ical"
)

// ExtractSelfStatus reads the PARTSTAT of selfEmail from the master VEVENT
// and from each override (keyed by NormalizeRecurrenceID). Apple rewrites the
// attendee line when the user responds (adds CUTYPE and EMAIL=, may drop
// RSVP, P0-4/P0-5), so matching uses the mailto: value or the EMAIL
// parameter, case-insensitively. A missing PARTSTAT means NEEDS-ACTION.
// Overrides without a self attendee are left out.
func ExtractSelfStatus(cal *ical.Calendar, selfEmail string) (PartStat, map[string]PartStat, error) {
	if cal == nil {
		return "", nil, fmt.Errorf("%w: nil calendar", ErrInvalidEvent)
	}
	var (
		master    PartStat
		hasMaster bool
		overrides = map[string]PartStat{}
	)
	for _, c := range cal.Children {
		if c.Name != ical.CompEvent {
			continue
		}
		ps, ok := selfPartStat(c, selfEmail)
		if rid := c.Props.Get(ical.PropRecurrenceID); rid != nil {
			if ok {
				overrides[NormalizeRecurrenceID(rid)] = ps
			}
			continue
		}
		if hasMaster {
			return "", nil, fmt.Errorf("%w: more than one master VEVENT", ErrInvalidEvent)
		}
		hasMaster = true
		if !ok {
			return "", nil, ErrNoSelfAttendee
		}
		master = ps
	}
	if !hasMaster {
		return "", nil, fmt.Errorf("%w: no master VEVENT", ErrInvalidEvent)
	}
	return master, overrides, nil
}

func selfPartStat(ev *ical.Component, selfEmail string) (PartStat, bool) {
	want := strings.ToLower(strings.TrimSpace(selfEmail))
	for _, a := range ev.Props.Values(ical.PropAttendee) {
		if attendeeAddress(a.Value) != want && strings.ToLower(a.Params.Get("EMAIL")) != want {
			continue
		}
		ps := PartStat(strings.ToUpper(a.Params.Get(ical.ParamParticipationStatus)))
		if ps == "" {
			ps = PartStatNeedsAction
		}
		return ps, true
	}
	return "", false
}

func attendeeAddress(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 7 && strings.EqualFold(v[:7], "mailto:") {
		v = v[7:]
	}
	return strings.ToLower(v)
}

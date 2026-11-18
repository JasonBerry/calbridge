// Package translate converts Google Calendar events to iCalendar objects for
// the iCloud mirror and reads RSVP state back out of them (PLAN.md §7). It is
// pure: no network, no store, no logging.
package translate

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	calendar "google.golang.org/api/calendar/v3"

	_ "time/tzdata" // deterministic zone data regardless of the host
)

// RenderVersion is written as X-CALBRIDGE-VERSION. Bump it when rendering
// changes in a way that should rewrite existing mirrors.
const RenderVersion = 1

// ProdID is the PRODID of every object calbridge writes.
const ProdID = "-//calbridge//calbridge//EN"

// Custom properties.
const (
	PropGoogleID       = "X-CALBRIDGE-GID"
	PropGoogleCalendar = "X-CALBRIDGE-GCAL"
	PropRenderVersion  = "X-CALBRIDGE-VERSION"
)

// Footer is appended to every unredacted DESCRIPTION.
const Footer = "— mirrored from Google Calendar by calbridge"

// Errors returned by RenderSeries.
var (
	ErrCancelled          = errors.New("translate: event is cancelled")
	ErrNoSelfAttendee     = errors.New("translate: user is not an attendee")
	ErrNoOrganizer        = errors.New("translate: event has no organizer")
	ErrInvalidEvent       = errors.New("translate: invalid event")
	ErrRecurringNotYet    = errors.New("translate: recurring events are rendered from M9")
	ErrVTimezoneNotYet    = errors.New("translate: emit_vtimezone is implemented in M6")
	ErrUnknownPrivacyMode = errors.New("translate: unknown privacy mode")
)

// PartStat is an iCalendar PARTSTAT value.
type PartStat string

const (
	PartStatNeedsAction PartStat = "NEEDS-ACTION"
	PartStatAccepted    PartStat = "ACCEPTED"
	PartStatTentative   PartStat = "TENTATIVE"
	PartStatDeclined    PartStat = "DECLINED"
)

// GoogleToPartStat maps a Google responseStatus to PARTSTAT. Anything
// unknown is NEEDS-ACTION.
func GoogleToPartStat(s string) PartStat {
	switch s {
	case "accepted":
		return PartStatAccepted
	case "tentative":
		return PartStatTentative
	case "declined":
		return PartStatDeclined
	}
	return PartStatNeedsAction
}

// PartStatToGoogle maps PARTSTAT to a Google responseStatus. NEEDS-ACTION,
// DELEGATED and unknown values return ("", false): they are never propagated.
func PartStatToGoogle(p PartStat) (string, bool) {
	switch PartStat(strings.ToUpper(string(p))) {
	case PartStatAccepted:
		return "accepted", true
	case PartStatTentative:
		return "tentative", true
	case PartStatDeclined:
		return "declined", true
	}
	return "", false
}

// PrivacyMode controls what a mirror reveals (§7.9).
type PrivacyMode string

const (
	PrivacyFull         PrivacyMode = "full"
	PrivacyHonorPrivate PrivacyMode = "honor_private"
	PrivacyBusyOnly     PrivacyMode = "busy_only"
)

// AttendeeMode selects which attendees a mirror lists (§7.6).
type AttendeeMode string

const (
	// AttendeesAll lists every attendee Google returns, each with their own
	// response, so other guests' RSVPs show in Apple Calendar (P0-4 E).
	AttendeesAll AttendeeMode = "all"
	// AttendeesSelf lists only the user (the original §7.6 rule).
	AttendeesSelf AttendeeMode = "self"
)

// ErrUnknownAttendeeMode is returned for an unrecognised RenderOptions.Attendees.
var ErrUnknownAttendeeMode = errors.New("translate: unknown attendee mode")

// RenderOptions configure RenderSeries.
type RenderOptions struct {
	SelfEmail        string // attendee address written to iCloud
	ScheduleAgent    string // "", "CLIENT", "NONE" (Phase 0 P0-4: NONE)
	Privacy          PrivacyMode
	Attendees        AttendeeMode    // all (default) | self
	DefaultAlarms    []time.Duration // added only when prev == nil (create)
	EmitVTimezone    bool            // Phase 0 P0-8: false
	Now              time.Time       // DTSTAMP
	GoogleCalendarID string          // X-CALBRIDGE-GCAL; default "primary"
	UID              string          // overrides iCalUID (§7.1 collision rule)
	MinSequence      int64           // SEQUENCE never goes below this (§7.2)
}

// RSVPView says which PARTSTAT to render for self. An empty Master means
// "Google's current responseStatus" (§7.4); the syncer sets it to keep a
// pending Apple-side change from being clobbered (§8.4).
type RSVPView struct {
	Master    PartStat
	Overrides map[string]PartStat // normalised RECURRENCE-ID -> PARTSTAT (M9)
}

// SelfAttendee returns the attendee entry marked self, or nil.
func SelfAttendee(ev *calendar.Event) *calendar.EventAttendee {
	for _, a := range ev.Attendees {
		if a != nil && a.Self {
			return a
		}
	}
	return nil
}

// UID returns the iCalendar UID for ev (its iCalUID).
func UID(ev *calendar.Event) string { return ev.ICalUID }

// RenderSeries builds one VCALENDAR for a Google event. M5 handles single
// events only: a master with recurrence or any exceptions returns
// ErrRecurringNotYet.
//
// prev is the current iCloud object, if any: its VALARMs and X-APPLE-*,
// X-MOZ-*, X-WR-* properties are carried over so alarms and Apple state the
// user has survive our updates (§7.2, §7.7).
func RenderSeries(master *calendar.Event, exceptions []*calendar.Event, st RSVPView, prev *ical.Calendar, o RenderOptions) (*ical.Calendar, error) {
	if master == nil {
		return nil, fmt.Errorf("%w: nil event", ErrInvalidEvent)
	}
	if len(master.Recurrence) > 0 || len(exceptions) > 0 || master.RecurringEventId != "" {
		return nil, ErrRecurringNotYet
	}
	if master.Status == "cancelled" {
		return nil, ErrCancelled
	}
	switch o.Privacy {
	case PrivacyFull, PrivacyHonorPrivate, PrivacyBusyOnly:
	case "":
		o.Privacy = PrivacyFull // user decision 2026-11-19
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownPrivacyMode, o.Privacy)
	}
	switch o.Attendees {
	case AttendeesAll, AttendeesSelf:
	case "":
		o.Attendees = AttendeesAll
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownAttendeeMode, o.Attendees)
	}
	if o.SelfEmail == "" {
		return nil, fmt.Errorf("%w: RenderOptions.SelfEmail is empty", ErrInvalidEvent)
	}
	if o.GoogleCalendarID == "" {
		o.GoogleCalendarID = "primary"
	}
	uid := o.UID
	if uid == "" {
		uid = master.ICalUID
	}
	if uid == "" || master.Id == "" {
		return nil, fmt.Errorf("%w: event has no id or iCalUID", ErrInvalidEvent)
	}
	if master.Organizer == nil || master.Organizer.Email == "" {
		return nil, ErrNoOrganizer
	}
	self := SelfAttendee(master)
	if self == nil {
		return nil, ErrNoSelfAttendee
	}

	ev := ical.NewEvent()
	p := ev.Props
	setText(p, ical.PropUID, uid)
	setUTC(p, ical.PropDateTimeStamp, o.Now)
	if t, ok := parseGoogleTime(master.Created); ok {
		setUTC(p, ical.PropCreated, t)
	}
	if t, ok := parseGoogleTime(master.Updated); ok {
		setUTC(p, ical.PropLastModified, t)
	}
	seq := master.Sequence
	if seq < o.MinSequence {
		seq = o.MinSequence
	}
	setRaw(p, ical.PropSequence, strconv.FormatInt(seq, 10))

	tzids, err := setTimes(p, master.Start, master.End)
	if err != nil {
		return nil, err
	}
	if len(tzids) > 0 && o.EmitVTimezone {
		return nil, ErrVTimezoneNotYet
	}

	switch master.Status {
	case "tentative":
		setRaw(p, ical.PropStatus, "TENTATIVE")
	default:
		setRaw(p, ical.PropStatus, "CONFIRMED")
	}
	if master.Transparency == "transparent" {
		setRaw(p, ical.PropTransparency, "TRANSPARENT")
	} else {
		setRaw(p, ical.PropTransparency, "OPAQUE")
	}
	private := master.Visibility == "private" || master.Visibility == "confidential"
	if private {
		setRaw(p, ical.PropClass, "PRIVATE")
	} else {
		setRaw(p, ical.PropClass, "PUBLIC")
	}

	redact := o.Privacy == PrivacyBusyOnly || (o.Privacy == PrivacyHonorPrivate && private)
	if redact {
		setText(p, ical.PropSummary, "Busy")
	} else {
		summary := strings.TrimSpace(master.Summary)
		if summary == "" {
			summary = "(No title)"
		}
		setText(p, ical.PropSummary, summary)
		if loc := strings.TrimSpace(master.Location); loc != "" {
			setText(p, ical.PropLocation, loc)
		}
		if u := videoURL(master); u != "" {
			setRaw(p, ical.PropURL, u)
		}
		setText(p, ical.PropDescription, description(master))
	}

	p.Set(organizerProp(master.Organizer, o.ScheduleAgent))
	partstat := st.Master
	if partstat == "" {
		partstat = GoogleToPartStat(self.ResponseStatus)
	}
	// Redacted mirrors never list other guests.
	others := o.Attendees == AttendeesAll && !redact
	for _, a := range master.Attendees {
		switch {
		case a == nil:
		case a == self:
			p.Add(attendeeProp(a, o.SelfEmail, partstat, o.ScheduleAgent, true))
		case others && a.Email != "" && !strings.EqualFold(a.Email, o.SelfEmail):
			p.Add(attendeeProp(a, a.Email, GoogleToPartStat(a.ResponseStatus), o.ScheduleAgent, false))
		}
	}

	setRaw(p, PropGoogleID, master.Id)
	setRaw(p, PropGoogleCalendar, o.GoogleCalendarID)
	setRaw(p, PropRenderVersion, strconv.Itoa(RenderVersion))

	if pm := masterEvent(prev); pm != nil {
		carryOver(ev.Component, pm)
	} else if prev == nil {
		for _, d := range o.DefaultAlarms {
			ev.Children = append(ev.Children, displayAlarm(d))
		}
	}

	cal := ical.NewCalendar()
	setRaw(cal.Props, ical.PropProductID, ProdID)
	setRaw(cal.Props, ical.PropVersion, "2.0")
	setRaw(cal.Props, ical.PropCalendarScale, "GREGORIAN")
	cal.Children = append(cal.Children, ev.Component)
	return cal, nil
}

func organizerProp(org *calendar.EventOrganizer, agent string) *ical.Prop {
	pr := ical.NewProp(ical.PropOrganizer)
	pr.Value = "mailto:" + org.Email
	if name := paramSafe(org.DisplayName); name != "" {
		pr.Params.Set(ical.ParamCommonName, name)
	}
	if agent != "" {
		pr.Params.Set("SCHEDULE-AGENT", agent)
	}
	return pr
}

// attendeeProp renders one attendee. CN is only written when Google supplies
// a display name: Apple shows a CN verbatim (P0-7 showed a placeholder
// "User"), and without one it falls back to the address book. RSVP=TRUE is
// set on self only (the one whose reply is expected from this calendar);
// resources render as CUTYPE=RESOURCE. Every line carries SCHEDULE-AGENT so
// iCloud never mails other guests (P0-4 E).
func attendeeProp(a *calendar.EventAttendee, email string, ps PartStat, agent string, self bool) *ical.Prop {
	pr := ical.NewProp(ical.PropAttendee)
	pr.Value = "mailto:" + email
	if name := paramSafe(a.DisplayName); name != "" {
		pr.Params.Set(ical.ParamCommonName, name)
	}
	pr.Params.Set(ical.ParamParticipationStatus, string(ps))
	if self {
		pr.Params.Set(ical.ParamRSVP, "TRUE")
	}
	if a.Resource {
		pr.Params.Set(ical.ParamCalendarUserType, "RESOURCE")
	}
	role := "REQ-PARTICIPANT"
	if a.Optional {
		role = "OPT-PARTICIPANT"
	}
	pr.Params.Set(ical.ParamRole, role)
	if agent != "" {
		pr.Params.Set("SCHEDULE-AGENT", agent)
	}
	return pr
}

// paramSafe makes a string usable as a quoted parameter value (no DQUOTE,
// no control characters).
func paramSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '"':
			return '\''
		case r < 0x20 || r == 0x7f:
			return ' '
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

func masterEvent(cal *ical.Calendar) *ical.Component {
	if cal == nil {
		return nil
	}
	for _, c := range cal.Children {
		if c.Name == ical.CompEvent && c.Props.Get(ical.PropRecurrenceID) == nil {
			return c
		}
	}
	return nil
}

// preservedPrefixes are property-name prefixes copied from the previous
// iCloud object and excluded from ContentHash.
var preservedPrefixes = []string{"X-APPLE-", "X-MOZ-", "X-WR-"}

func isPreserved(name string) bool {
	for _, pfx := range preservedPrefixes {
		if strings.HasPrefix(strings.ToUpper(name), pfx) {
			return true
		}
	}
	return false
}

// carryOver copies VALARMs and Apple/Mozilla properties from prev into ev.
//
// UNVERIFIED (P0-5): Apple keeps X-APPLE-NEEDS-REPLY:TRUE even after an RSVP
// (seen with PARTSTAT=ACCEPTED); carrying it over is assumed harmless.
func carryOver(ev, prev *ical.Component) {
	for name, props := range prev.Props {
		if !isPreserved(name) {
			continue
		}
		ev.Props[name] = append([]ical.Prop(nil), props...)
	}
	for _, c := range prev.Children {
		if c.Name == ical.CompAlarm {
			ev.Children = append(ev.Children, c)
		}
	}
}

func displayAlarm(d time.Duration) *ical.Component {
	a := ical.NewComponent(ical.CompAlarm)
	setRaw(a.Props, ical.PropAction, "DISPLAY")
	setText(a.Props, ical.PropDescription, "Reminder")
	setRaw(a.Props, ical.PropTrigger, triggerValue(d))
	return a
}

// triggerValue renders a "before start" trigger, e.g. 10m -> -PT10M.
func triggerValue(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d == 0:
		return "PT0S"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("-P%dD", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("-PT%dH", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("-PT%dM", d/time.Minute)
	}
	return fmt.Sprintf("-PT%dS", d/time.Second)
}

func setRaw(p ical.Props, name, value string) {
	pr := ical.NewProp(name)
	pr.Value = value
	p.Set(pr)
}

func setText(p ical.Props, name, value string) {
	pr := ical.NewProp(name)
	pr.SetText(value)
	p.Set(pr)
}

func setUTC(p ical.Props, name string, t time.Time) {
	setRaw(p, name, t.UTC().Format("20060102T150405Z"))
}

func parseGoogleTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

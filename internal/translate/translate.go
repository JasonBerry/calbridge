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
	EmitVTimezone    bool            // embed generated VTIMEZONEs (P0-8: false; iCloud adds its own)
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

// RenderSeries builds one VCALENDAR for a Google event (§7.2) or series
// (§7.8): the master VEVENT (with RRULE/RDATE/EXDATE copied from
// recurrence[]), one override VEVENT per non-cancelled exception
// (RECURRENCE-ID = originalStartTime in the master's DTSTART format), and an
// EXDATE per cancelled exception. st.Overrides adds or overrides the
// PARTSTAT of single occurrences; an occurrence Google has no exception for
// yet gets an override derived from the master, so a per-occurrence
// response the user made in Apple survives our rewrite (P0-8).
//
// prev is the current iCloud object, if any: VALARMs and X-APPLE-*, X-MOZ-*,
// X-WR-* properties are carried over from its master and from overrides with
// the same RECURRENCE-ID (§7.2, §7.7).
func RenderSeries(master *calendar.Event, exceptions []*calendar.Event, st RSVPView, prev *ical.Calendar, o RenderOptions) (*ical.Calendar, error) {
	if master == nil {
		return nil, fmt.Errorf("%w: nil event", ErrInvalidEvent)
	}
	if master.RecurringEventId != "" {
		return nil, fmt.Errorf("%w: %s is an instance, not a master", ErrInvalidEvent, master.Id)
	}
	if master.Status == "cancelled" {
		return nil, ErrCancelled
	}
	recurring := len(master.Recurrence) > 0
	if !recurring && len(exceptions) > 0 {
		return nil, fmt.Errorf("%w: exceptions for a non-recurring event", ErrInvalidEvent)
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

	partstat := st.Master
	if partstat == "" {
		partstat = GoogleToPartStat(self.ResponseStatus)
	}
	mb, err := buildEvent(master, uid, partstat, o)
	if err != nil {
		return nil, err
	}
	tzids := append([]string(nil), mb.tzids...)
	spanStart, spanEnd := mb.start, mb.end

	var overrides []*built
	if recurring {
		if err := addRecurrence(mb.comp, master.Recurrence); err != nil {
			return nil, err
		}
		if u, ok := rruleUntil(master.Recurrence); ok {
			spanEnd = maxTime(spanEnd, u)
		} else if !mb.start.IsZero() {
			spanEnd = maxTime(spanEnd, mb.start.AddDate(5, 0, 0)) // open-ended or COUNT (§7.3)
		}
		overrides, err = buildOverrides(master, exceptions, st, mb, uid, o)
		if err != nil {
			return nil, err
		}
		for _, ov := range overrides {
			for _, id := range ov.tzids {
				if !contains(tzids, id) {
					tzids = append(tzids, id)
				}
			}
			if !ov.start.IsZero() && (spanStart.IsZero() || ov.start.Before(spanStart)) {
				spanStart = ov.start
			}
			spanEnd = maxTime(spanEnd, ov.end)
		}
	}

	var vtzs []*ical.Component
	if o.EmitVTimezone && len(tzids) > 0 {
		from, to := vtimezoneSpan(spanStart, spanEnd)
		for _, id := range tzids {
			vtz, err := VTimezone(id, from, to)
			if err != nil {
				return nil, err
			}
			vtzs = append(vtzs, vtz)
		}
	}

	if pm := masterEvent(prev); pm != nil {
		carryOver(mb.comp, pm)
	} else if prev == nil {
		for _, d := range o.DefaultAlarms {
			mb.comp.Children = append(mb.comp.Children, displayAlarm(d))
		}
	}
	prevOverrides := overrideEvents(prev)
	for _, ov := range overrides {
		if po, ok := prevOverrides[ov.rid]; ok {
			carryOver(ov.comp, po)
		} else if prev == nil {
			for _, d := range o.DefaultAlarms {
				ov.comp.Children = append(ov.comp.Children, displayAlarm(d))
			}
		}
	}

	cal := ical.NewCalendar()
	setRaw(cal.Props, ical.PropProductID, ProdID)
	setRaw(cal.Props, ical.PropVersion, "2.0")
	setRaw(cal.Props, ical.PropCalendarScale, "GREGORIAN")
	cal.Children = append(cal.Children, vtzs...)
	cal.Children = append(cal.Children, mb.comp)
	for _, ov := range overrides {
		cal.Children = append(cal.Children, ov.comp)
	}
	return cal, nil
}

// built is one rendered VEVENT.
type built struct {
	comp       *ical.Component
	tzids      []string
	start, end time.Time // instants; zero for all-day
	allDay     bool
	startZone  string // TZID of DTSTART ("" = UTC or all-day)
	rid        string // normalised RECURRENCE-ID (overrides only)
}

// buildEvent renders the properties shared by masters and overrides.
func buildEvent(ev *calendar.Event, uid string, partstat PartStat, o RenderOptions) (*built, error) {
	if ev.Organizer == nil || ev.Organizer.Email == "" {
		return nil, ErrNoOrganizer
	}
	self := SelfAttendee(ev)
	if self == nil {
		return nil, ErrNoSelfAttendee
	}
	comp := ical.NewEvent().Component
	p := comp.Props
	setText(p, ical.PropUID, uid)
	setUTC(p, ical.PropDateTimeStamp, o.Now)
	if t, ok := parseGoogleTime(ev.Created); ok {
		setUTC(p, ical.PropCreated, t)
	}
	if t, ok := parseGoogleTime(ev.Updated); ok {
		setUTC(p, ical.PropLastModified, t)
	}
	seq := ev.Sequence
	if seq < o.MinSequence {
		seq = o.MinSequence
	}
	setRaw(p, ical.PropSequence, strconv.FormatInt(seq, 10))

	tzids, startT, endT, err := setTimes(p, ev.Start, ev.End)
	if err != nil {
		return nil, err
	}
	b := &built{comp: comp, tzids: tzids, start: startT, end: endT, allDay: ev.Start.Date != ""}
	if b.allDay {
		b.start, _ = time.Parse("2006-01-02", ev.Start.Date)
		b.end, _ = time.Parse("2006-01-02", ev.End.Date)
	}
	b.startZone = p.Get(ical.PropDateTimeStart).Params.Get(ical.ParamTimezoneID)

	switch ev.Status {
	case "tentative":
		setRaw(p, ical.PropStatus, "TENTATIVE")
	default:
		setRaw(p, ical.PropStatus, "CONFIRMED")
	}
	if ev.Transparency == "transparent" {
		setRaw(p, ical.PropTransparency, "TRANSPARENT")
	} else {
		setRaw(p, ical.PropTransparency, "OPAQUE")
	}
	private := ev.Visibility == "private" || ev.Visibility == "confidential"
	if private {
		setRaw(p, ical.PropClass, "PRIVATE")
	} else {
		setRaw(p, ical.PropClass, "PUBLIC")
	}

	redact := o.Privacy == PrivacyBusyOnly || (o.Privacy == PrivacyHonorPrivate && private)
	if redact {
		setText(p, ical.PropSummary, "Busy")
	} else {
		summary := strings.TrimSpace(ev.Summary)
		if summary == "" {
			summary = "(No title)"
		}
		setText(p, ical.PropSummary, summary)
		if loc := strings.TrimSpace(ev.Location); loc != "" {
			setText(p, ical.PropLocation, loc)
		}
		if u := videoURL(ev); u != "" {
			setRaw(p, ical.PropURL, u)
		}
		setText(p, ical.PropDescription, description(ev))
	}

	p.Set(organizerProp(ev.Organizer, o.ScheduleAgent))
	// Redacted mirrors never list other guests.
	others := o.Attendees == AttendeesAll && !redact
	for _, a := range ev.Attendees {
		switch {
		case a == nil:
		case a == self:
			p.Add(attendeeProp(a, o.SelfEmail, partstat, o.ScheduleAgent, true))
		case others && a.Email != "" && !strings.EqualFold(a.Email, o.SelfEmail):
			p.Add(attendeeProp(a, a.Email, GoogleToPartStat(a.ResponseStatus), o.ScheduleAgent, false))
		}
	}

	setRaw(p, PropGoogleID, ev.Id)
	setRaw(p, PropGoogleCalendar, o.GoogleCalendarID)
	setRaw(p, PropRenderVersion, strconv.Itoa(RenderVersion))
	return b, nil
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
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

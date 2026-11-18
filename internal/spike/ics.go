package spike

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	calendar "google.golang.org/api/calendar/v3"
)

// objectHref mirrors PLAN.md §7.1: <calendar>/calbridge-<base32(sha256(uid))[:26]>.ics.
func objectHref(calendarHref, uid string) string {
	sum := sha256.Sum256([]byte(uid))
	enc := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))
	return strings.TrimSuffix(calendarHref, "/") + "/calbridge-" + enc[:26] + ".ics"
}

func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sydneyVTimezone is a hand-written VTIMEZONE for Australia/Sydney (DST from
// the first Sunday of October 02:00 to the first Sunday of April 03:00). The
// real generator arrives in M6; spikes only support this zone.
const sydneyVTimezone = `BEGIN:VTIMEZONE
TZID:Australia/Sydney
BEGIN:STANDARD
DTSTART:20080406T030000
RRULE:FREQ=YEARLY;BYMONTH=4;BYDAY=1SU
TZOFFSETFROM:+1100
TZOFFSETTO:+1000
TZNAME:AEST
END:STANDARD
BEGIN:DAYLIGHT
DTSTART:20081005T020000
RRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=1SU
TZOFFSETFROM:+1000
TZOFFSETTO:+1100
TZNAME:AEDT
END:DAYLIGHT
END:VTIMEZONE
`

// objectSpec describes one spike test object.
type objectSpec struct {
	UID           string
	Summary       string
	Organizer     string // email; empty = no ORGANIZER/ATTENDEE (plain event)
	OrganizerCN   string
	ScheduleAgent string // "", CLIENT, NONE
	Attendee      string
	PartStat      string // default NEEDS-ACTION
	ExtraGuests   []string
	Optional      bool
	TZ            string // IANA zone for timed events; "" = UTC
	VTimezone     bool
	Start, End    time.Time
	AllDay        bool
	Sequence      int64
	Recurrence    []string // RRULE/EXDATE/RDATE lines, verbatim
	Overrides     []override
	GoogleID      string
	Now           time.Time
}

type override struct {
	RecurrenceID time.Time
	Start, End   time.Time
	Summary      string
}

func fold(line string) string {
	// RFC 5545 folding at 75 octets.
	// Continuation lines start with a space, which counts toward the 75.
	var b strings.Builder
	limit := 75
	for len(line) > limit {
		cut := limit
		for cut > 0 && (line[cut]&0xC0) == 0x80 { // don't split UTF-8 sequences
			cut--
		}
		b.WriteString(line[:cut] + "\r\n ")
		line = line[cut:]
		limit = 74
	}
	b.WriteString(line + "\r\n")
	return b.String()
}

func escapeText(s string) string {
	r := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`)
	return r.Replace(s)
}

func quoteParam(s string) string {
	if strings.ContainsAny(s, ":;,") {
		return `"` + strings.ReplaceAll(s, `"`, "'") + `"`
	}
	return s
}

func (s objectSpec) dt(name string, t time.Time) string {
	switch {
	case s.AllDay:
		return fmt.Sprintf("%s;VALUE=DATE:%s", name, t.Format("20060102"))
	case s.TZ != "":
		loc, err := time.LoadLocation(s.TZ)
		if err != nil {
			return fmt.Sprintf("%s:%s", name, t.UTC().Format("20060102T150405Z"))
		}
		return fmt.Sprintf("%s;TZID=%s:%s", name, s.TZ, t.In(loc).Format("20060102T150405"))
	default:
		return fmt.Sprintf("%s:%s", name, t.UTC().Format("20060102T150405Z"))
	}
}

func (s objectSpec) render() []byte {
	var b strings.Builder
	w := func(line string) { b.WriteString(fold(line)) }
	w("BEGIN:VCALENDAR")
	w("VERSION:2.0")
	w("PRODID:-//calbridge//phase0//EN")
	if s.VTimezone && s.TZ == "Australia/Sydney" && !s.AllDay {
		for _, l := range strings.Split(strings.TrimSpace(sydneyVTimezone), "\n") {
			w(l)
		}
	}
	s.writeEvent(w, nil)
	for i := range s.Overrides {
		s.writeEvent(w, &s.Overrides[i])
	}
	w("END:VCALENDAR")
	return []byte(b.String())
}

func (s objectSpec) writeEvent(w func(string), ov *override) {
	partstat := s.PartStat
	if partstat == "" {
		partstat = "NEEDS-ACTION"
	}
	w("BEGIN:VEVENT")
	w("UID:" + s.UID)
	w("DTSTAMP:" + s.Now.UTC().Format("20060102T150405Z"))
	start, end, summary := s.Start, s.End, s.Summary
	if ov != nil {
		w(s.dt("RECURRENCE-ID", ov.RecurrenceID))
		start, end = ov.Start, ov.End
		if ov.Summary != "" {
			summary = ov.Summary
		}
	}
	w(s.dt("DTSTART", start))
	w(s.dt("DTEND", end))
	w(fmt.Sprintf("SEQUENCE:%d", s.Sequence))
	w("SUMMARY:" + escapeText(summary))
	if ov == nil {
		for _, r := range s.Recurrence {
			w(r)
		}
	}
	if s.Organizer != "" {
		org := "ORGANIZER"
		if s.OrganizerCN != "" {
			org += ";CN=" + quoteParam(s.OrganizerCN)
		}
		if s.ScheduleAgent != "" {
			org += ";SCHEDULE-AGENT=" + s.ScheduleAgent
		}
		w(org + ":mailto:" + s.Organizer)
		role := "REQ-PARTICIPANT"
		if s.Optional {
			role = "OPT-PARTICIPANT"
		}
		att := "ATTENDEE;CN=User;PARTSTAT=" + partstat + ";RSVP=TRUE;ROLE=" + role
		if s.ScheduleAgent != "" {
			att += ";SCHEDULE-AGENT=" + s.ScheduleAgent
		}
		w(att + ":mailto:" + s.Attendee)
		for _, g := range s.ExtraGuests {
			w("ATTENDEE;CN=Guest;PARTSTAT=ACCEPTED;ROLE=REQ-PARTICIPANT:mailto:" + g)
		}
	}
	gid := s.GoogleID
	if gid == "" {
		gid = "phase0-test"
	}
	w("X-CALBRIDGE-GID:" + gid)
	w("END:VEVENT")
}

// variantAgent maps P0-4 variant letters to the ORGANIZER SCHEDULE-AGENT.
func variantAgent(v string) (string, error) {
	switch strings.ToUpper(v) {
	case "A":
		return "", nil
	case "B":
		return "CLIENT", nil
	case "C":
		return "NONE", nil
	}
	return "", fmt.Errorf("variant %q has no fixed SCHEDULE-AGENT; use A, B or C (D and E take --schedule-agent)", v)
}

// googleToPartStat maps Google responseStatus to iCalendar PARTSTAT.
func googleToPartStat(s string) string {
	switch s {
	case "accepted":
		return "ACCEPTED"
	case "tentative":
		return "TENTATIVE"
	case "declined":
		return "DECLINED"
	}
	return "NEEDS-ACTION"
}

// specFromGoogle builds a P0-7 object from a real Google event (UID = iCalUID).
// It is deliberately minimal; the real translation is M5.
func specFromGoogle(ev *calendar.Event, attendee string) (objectSpec, error) {
	s := objectSpec{
		UID:      ev.ICalUID,
		Summary:  ev.Summary,
		Sequence: ev.Sequence,
		GoogleID: ev.Id,
		Attendee: attendee,
	}
	if s.UID == "" {
		return s, fmt.Errorf("event %s has no iCalUID", ev.Id)
	}
	if ev.Organizer != nil {
		s.Organizer, s.OrganizerCN = ev.Organizer.Email, ev.Organizer.DisplayName
	}
	for _, a := range ev.Attendees {
		if a.Self {
			s.PartStat = googleToPartStat(a.ResponseStatus)
			s.Optional = a.Optional
		}
	}
	if ev.Start == nil || ev.End == nil {
		return s, fmt.Errorf("event %s has no start/end", ev.Id)
	}
	var err error
	if ev.Start.Date != "" {
		s.AllDay = true
		if s.Start, err = time.Parse("2006-01-02", ev.Start.Date); err != nil {
			return s, err
		}
		if s.End, err = time.Parse("2006-01-02", ev.End.Date); err != nil {
			return s, err
		}
	} else {
		if s.Start, err = time.Parse(time.RFC3339, ev.Start.DateTime); err != nil {
			return s, err
		}
		if s.End, err = time.Parse(time.RFC3339, ev.End.DateTime); err != nil {
			return s, err
		}
		s.TZ = ev.Start.TimeZone
	}
	s.Recurrence = append(s.Recurrence, ev.Recurrence...)
	return s, nil
}

// unfoldLines splits iCalendar text into unfolded content lines.
func unfoldLines(raw []byte) []string {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\n ", "")
	text = strings.ReplaceAll(text, "\n\t", "")
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// lineDiff returns lines only in a (removed) and only in b (added), ignoring order.
func lineDiff(a, b []byte) (removed, added []string) {
	count := map[string]int{}
	for _, l := range unfoldLines(a) {
		count[l]++
	}
	for _, l := range unfoldLines(b) {
		if count[l] > 0 {
			count[l]--
			continue
		}
		added = append(added, l)
	}
	for l, n := range count {
		for ; n > 0; n-- {
			removed = append(removed, l)
		}
	}
	sort.Strings(removed)
	return removed, added
}

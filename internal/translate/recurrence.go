package translate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	calendar "google.golang.org/api/calendar/v3"
)

// recurrenceProps are the recurrence[] lines copied onto the master (§7.8
// rule 2; Google never sends DTSTART there).
var recurrenceProps = map[string]bool{"RRULE": true, "EXRULE": true, "RDATE": true, "EXDATE": true}

// addRecurrence copies Google's recurrence[] lines verbatim.
func addRecurrence(comp *ical.Component, lines []string) error {
	for _, l := range lines {
		pr, err := parseContentLine(l)
		if err != nil {
			return err
		}
		if recurrenceProps[pr.Name] {
			comp.Props.Add(pr)
		}
	}
	return nil
}

// parseContentLine parses one unfolded iCalendar content line
// (NAME;PARAM=V:value).
func parseContentLine(line string) (*ical.Prop, error) {
	line = strings.TrimSpace(line)
	inQuote, colon := false, -1
	for i, r := range line {
		if r == '"' {
			inQuote = !inQuote
		}
		if r == ':' && !inQuote {
			colon = i
			break
		}
	}
	if colon <= 0 {
		return nil, fmt.Errorf("%w: bad recurrence line %q", ErrInvalidEvent, line)
	}
	head, value := line[:colon], line[colon+1:]
	parts := strings.Split(head, ";")
	pr := &ical.Prop{Name: strings.ToUpper(parts[0]), Params: ical.Params{}, Value: value}
	for _, kv := range parts[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("%w: bad parameter in %q", ErrInvalidEvent, line)
		}
		pr.Params[strings.ToUpper(k)] = strings.Split(strings.Trim(v, `"`), ",")
	}
	return pr, nil
}

// rruleUntil returns the latest UNTIL among the RRULE lines.
func rruleUntil(lines []string) (time.Time, bool) {
	var last time.Time
	found := false
	for _, l := range lines {
		if !strings.HasPrefix(strings.ToUpper(l), "RRULE:") {
			continue
		}
		for _, part := range strings.Split(l[len("RRULE:"):], ";") {
			k, v, ok := strings.Cut(part, "=")
			if !ok || !strings.EqualFold(k, "UNTIL") {
				continue
			}
			for _, layout := range []string{"20060102T150405Z", "20060102T150405", "20060102"} {
				if t, err := time.Parse(layout, v); err == nil {
					if !found || t.After(last) {
						last = t
					}
					found = true
					break
				}
			}
		}
	}
	return last, found
}

// occurrence identifies one occurrence of a series by its original start.
type occurrence struct {
	date string    // YYYY-MM-DD for all-day series
	at   time.Time // instant for timed series
}

// originalStart returns an exception's originalStartTime, falling back to
// the instance-id suffix _YYYYMMDD[THHMMSSZ] for bare cancelled tombstones
// (§7.8 rule 4; only id/status/etag are guaranteed, P0-1).
//
// UNVERIFIED (P0-1 saw tombstones but not for instances): the instance-id
// suffix format.
func originalStart(ex *calendar.Event) (occurrence, bool) {
	if o := ex.OriginalStartTime; o != nil {
		if o.Date != "" {
			return occurrence{date: o.Date}, true
		}
		if t, err := time.Parse(time.RFC3339, o.DateTime); err == nil {
			return occurrence{at: t}, true
		}
	}
	i := strings.LastIndex(ex.Id, "_")
	if i < 0 {
		return occurrence{}, false
	}
	suffix := ex.Id[i+1:]
	if t, err := time.Parse("20060102T150405Z", suffix); err == nil {
		return occurrence{at: t}, true
	}
	if t, err := time.Parse("20060102", suffix); err == nil {
		return occurrence{date: t.Format("2006-01-02")}, true
	}
	return occurrence{}, false
}

// occurrenceProp renders an occurrence as RECURRENCE-ID or EXDATE in the
// master's DTSTART format (§7.8 rules 3–4) and returns its normalised key.
func occurrenceProp(name string, master *built, occ occurrence) (*ical.Prop, string, error) {
	if master.allDay {
		d := occ.date
		if d == "" {
			d = occ.at.Format("2006-01-02")
		}
		t, err := time.Parse("2006-01-02", d)
		if err != nil {
			return nil, "", fmt.Errorf("%w: bad original date %q", ErrInvalidEvent, d)
		}
		pr := ical.NewProp(name)
		pr.SetValueType(ical.ValueDate)
		pr.Value = t.Format("20060102")
		return pr, pr.Value, nil
	}
	at := occ.at
	if at.IsZero() {
		// A date-only original start on a timed series: the master's time of day that date.
		d, err := time.Parse("2006-01-02", occ.date)
		if err != nil {
			return nil, "", fmt.Errorf("%w: no original start", ErrInvalidEvent)
		}
		loc := time.UTC
		if _, l, ok := ResolveZone(master.startZone); ok {
			loc = l
		}
		ms := master.start.In(loc)
		at = time.Date(d.Year(), d.Month(), d.Day(), ms.Hour(), ms.Minute(), ms.Second(), 0, loc)
	}
	pr, _, _, err := dateTimeProp(name, at.Format(time.RFC3339), master.startZone)
	if err != nil {
		return nil, "", err
	}
	return pr, NormalizeRecurrenceID(pr), nil
}

// keyOccurrence turns a normalised RECURRENCE-ID key back into an occurrence.
func keyOccurrence(key string, allDay bool) (occurrence, bool) {
	if allDay {
		t, err := time.Parse("20060102", key)
		return occurrence{date: t.Format("2006-01-02")}, err == nil
	}
	t, err := time.Parse("20060102T150405Z", key)
	return occurrence{at: t}, err == nil
}

// buildOverrides renders exceptions (overrides and EXDATEs on the master)
// and synthetic overrides for st.Overrides keys Google has no exception for.
func buildOverrides(master *calendar.Event, exceptions []*calendar.Event, st RSVPView, mb *built, uid string, o RenderOptions) ([]*built, error) {
	exs := append([]*calendar.Event(nil), exceptions...)
	sort.SliceStable(exs, func(i, j int) bool { return exs[i] != nil && exs[j] != nil && exs[i].Id < exs[j].Id })
	var out []*built
	seen := map[string]bool{}
	for _, ex := range exs {
		if ex == nil {
			continue
		}
		occ, ok := originalStart(ex)
		if !ok {
			continue // cannot be placed; the syncer fetches it (§7.8 rule 4)
		}
		cancelled := ex.Status == "cancelled" || SelfAttendee(ex) == nil
		if cancelled {
			pr, key, err := occurrenceProp(ical.PropExceptionDates, mb, occ)
			if err != nil {
				return nil, err
			}
			if !seen[key] {
				mb.comp.Props.Add(pr)
			}
			seen[key] = true
			continue
		}
		rid, key, err := occurrenceProp(ical.PropRecurrenceID, mb, occ)
		if err != nil {
			return nil, err
		}
		if seen[key] {
			continue // exception of an exception: keyed by original start (§7.8 rule 6)
		}
		seen[key] = true
		ev := withMasterFallbacks(ex, master)
		ps := st.Overrides[key]
		if ps == "" {
			ps = GoogleToPartStat(SelfAttendee(ev).ResponseStatus)
		}
		b, err := buildEvent(ev, uid, ps, o)
		if err != nil {
			return nil, err
		}
		b.comp.Props.Set(rid)
		b.rid = key
		out = append(out, b)
	}

	keys := make([]string, 0, len(st.Overrides))
	for k := range st.Overrides {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		occ, ok := keyOccurrence(key, mb.allDay)
		if !ok {
			continue
		}
		syn, err := syntheticInstance(master, mb, occ, key)
		if err != nil {
			return nil, err
		}
		b, err := buildEvent(syn, uid, st.Overrides[key], o)
		if err != nil {
			return nil, err
		}
		rid, _, err := occurrenceProp(ical.PropRecurrenceID, mb, occ)
		if err != nil {
			return nil, err
		}
		b.comp.Props.Set(rid)
		b.rid = key
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].rid < out[j].rid })
	return out, nil
}

// withMasterFallbacks fills fields an instance may omit from the master.
func withMasterFallbacks(ex, master *calendar.Event) *calendar.Event {
	c := *ex
	if c.Organizer == nil {
		c.Organizer = master.Organizer
	}
	if c.Start == nil {
		c.Start = master.Start
	}
	if c.End == nil {
		c.End = master.End
	}
	return &c
}

// syntheticInstance is the master as it occurs at occ, for an occurrence
// Google has no exception for yet. Its X-CALBRIDGE-GID is Google's instance
// id format (<master>_<key>).
func syntheticInstance(master *calendar.Event, mb *built, occ occurrence, key string) (*calendar.Event, error) {
	c := *master
	c.Recurrence = nil
	c.Id = master.Id + "_" + key
	if mb.allDay {
		days := int(mb.end.Sub(mb.start).Hours() / 24)
		d, err := time.Parse("2006-01-02", occ.date)
		if err != nil {
			return nil, err
		}
		c.Start = &calendar.EventDateTime{Date: d.Format("2006-01-02")}
		c.End = &calendar.EventDateTime{Date: d.AddDate(0, 0, days).Format("2006-01-02")}
		return &c, nil
	}
	dur := mb.end.Sub(mb.start)
	c.Start = &calendar.EventDateTime{DateTime: occ.at.Format(time.RFC3339), TimeZone: master.Start.TimeZone}
	c.End = &calendar.EventDateTime{DateTime: occ.at.Add(dur).Format(time.RFC3339), TimeZone: master.End.TimeZone}
	return &c, nil
}

// overrideEvents indexes prev's override VEVENTs by normalised RECURRENCE-ID.
func overrideEvents(cal *ical.Calendar) map[string]*ical.Component {
	out := map[string]*ical.Component{}
	if cal == nil {
		return out
	}
	for _, c := range cal.Children {
		if c.Name != ical.CompEvent {
			continue
		}
		if rid := c.Props.Get(ical.PropRecurrenceID); rid != nil {
			out[NormalizeRecurrenceID(rid)] = c
		}
	}
	return out
}

// OccurrenceKey returns the normalised RECURRENCE-ID key (as
// NormalizeRecurrenceID and ExtractSelfStatus produce) of exception ex of
// master: YYYYMMDD for all-day series, the original start in UTC otherwise.
func OccurrenceKey(master, ex *calendar.Event) (string, bool) {
	occ, ok := originalStart(ex)
	if !ok {
		return "", false
	}
	if master.Start != nil && master.Start.Date != "" {
		d := occ.date
		if d == "" {
			d = occ.at.UTC().Format("2006-01-02")
		}
		t, err := time.Parse("2006-01-02", d)
		if err != nil {
			return "", false
		}
		return t.Format("20060102"), true
	}
	if occ.at.IsZero() {
		return "", false
	}
	return occ.at.UTC().Format("20060102T150405Z"), true
}

// InstanceSuffix reports whether s looks like the suffix of a Google
// instance id (YYYYMMDD or YYYYMMDDTHHMMSSZ).
//
// UNVERIFIED (P0-1 covered tombstones of single events only): Google's
// instance ids are <masterId>_<suffix>.
func InstanceSuffix(s string) bool {
	if _, err := time.Parse("20060102T150405Z", s); err == nil {
		return true
	}
	_, err := time.Parse("20060102", s)
	return err == nil
}

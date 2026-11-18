package spike

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/emersion/go-ical"

	"github.com/JasonBerry/calbridge/internal/config"
	"github.com/JasonBerry/calbridge/internal/googlecal"
	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/secrets"
)

// TestCalendarPrefix is the only display-name prefix spike commands write to
// without --i-know (CLAUDE.md rule 2).
const TestCalendarPrefix = "calbridge-test"

type caldavFlags struct {
	common
	base     string
	calendar string
	iKnow    bool
}

func (f *caldavFlags) register(fs *flag.FlagSet, writes bool) {
	f.common.register(fs)
	fs.StringVar(&f.base, "base", icloud.DefaultBaseURL, "CalDAV entry URL")
	fs.StringVar(&f.calendar, "calendar", TestCalendarPrefix, "calendar display name")
	if writes {
		fs.BoolVar(&f.iKnow, "i-know", false, "allow writing to a calendar not named "+TestCalendarPrefix+"* (never the shared main calendar without the user's say-so)")
	}
}

type caldavSession struct {
	cfg   config.Config
	c     *icloud.Client
	disc  icloud.Discovery
	cal   icloud.CalendarInfo
	flags *caldavFlags
}

// connect loads config and the app password, builds a client (dumping HTTP to
// --dump), discovers, and resolves --calendar when needed.
func connect(ctx context.Context, e *env, f *caldavFlags, name string, needCalendar, writes bool, transport http.RoundTripper) (*caldavSession, error) {
	cfg, err := f.load()
	if err != nil {
		return nil, err
	}
	pw, err := secrets.ICloudAppPassword(e.getenv, cfg.ICloud.AppPasswordFile, f.insecurePerms)
	if err != nil {
		return nil, err
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	if f.dumpDir != "" {
		transport = &icloud.DumpTransport{Base: transport, Dir: filepath.Join(f.dumpDir, "caldav"), Prefix: stamp() + "-" + name + "-"}
	}
	c, err := icloud.New(icloud.Options{
		BaseURL:      f.base,
		Username:     cfg.ICloud.Username,
		Password:     pw,
		HTTPClient:   &http.Client{Transport: transport, Timeout: 60 * time.Second},
		RateLimitRPS: cfg.ICloud.RateLimitRPS,
	})
	if err != nil {
		return nil, err
	}
	d, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}
	s := &caldavSession{cfg: cfg, c: c, disc: d, flags: f}
	if needCalendar {
		ci, ok := d.FindCalendar(f.calendar)
		if !ok {
			return nil, fmt.Errorf("no calendar named %q (run `calbridge spike caldav-discover`; create it in Apple Calendar if missing)", f.calendar)
		}
		if writes {
			if err := checkWritable(ci.DisplayName, f.iKnow); err != nil {
				return nil, err
			}
		}
		s.cal = ci
	}
	return s, nil
}

func checkWritable(displayName string, iKnow bool) error {
	if strings.HasPrefix(displayName, TestCalendarPrefix) || iKnow {
		return nil
	}
	return fmt.Errorf("refusing to write to calendar %q: spike commands only write to %s* calendars unless --i-know is given", displayName, TestCalendarPrefix)
}

// checkHrefInCalendar guards commands that take a raw --href.
func (s *caldavSession) checkHrefInCalendar(href string) error {
	if !strings.HasPrefix(href, strings.TrimSuffix(s.cal.Href, "/")+"/") {
		return fmt.Errorf("href %s is not inside calendar %q (%s)", href, s.cal.DisplayName, s.cal.Href)
	}
	return nil
}

// caldavDiscover implements P0-3.
func caldavDiscover(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "caldav-discover")
	var f caldavFlags
	f.register(fs, false)
	if err := parse(fs, args); err != nil {
		return err
	}
	s, err := connect(ctx, e, &f, "discover", false, false, nil)
	if err != nil {
		return err
	}
	d := s.disc
	out := e.stdout
	_, _ = fmt.Fprintf(out, "Partition base:  %s\nPrincipal:       %s\nCalendar home:   %s\nSchedule inbox:  %s\nSchedule outbox: %s\n",
		d.BaseURL, d.Principal, d.CalendarHome, d.ScheduleInbox, d.ScheduleOutbox)
	_, _ = fmt.Fprintln(out, "calendar-user-address-set:")
	for _, a := range d.AddressSet {
		_, _ = fmt.Fprintf(out, "  %s\n", a)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\nDISPLAYNAME\tHREF\tCOMPONENTS\tSYNC-COLLECTION\tSYNC-TOKEN\tCTAG\tPRIVILEGES")
	for _, ci := range d.Calendars {
		if !ci.IsCalendar {
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%v\t%v\t%s\n", ci.DisplayName, ci.Href, strings.Join(ci.Components, ","),
			ci.SupportsSync, ci.SyncToken != "", ci.CTag != "", strings.Join(ci.Privileges, ","))
	}
	_ = tw.Flush()

	_, _ = fmt.Fprintln(out, "\nP0-3 checks:")
	addr := s.cfg.ICloud.AttendeeEmail
	if d.HasAddress(addr) {
		_, _ = fmt.Fprintf(out, "  [ok]   address set contains mailto:%s\n", addr)
	} else {
		_, _ = fmt.Fprintf(out, "  [FAIL] address set does NOT contain mailto:%s (RSVP controls unlikely; see PLAN.md P0-3/§18 concern 3)\n", addr)
	}
	if ci, ok := d.FindCalendar(TestCalendarPrefix); ok {
		_, _ = fmt.Fprintf(out, "  [ok]   %s found at %s; sync-collection advertised: %v\n", TestCalendarPrefix, ci.Href, ci.SupportsSync)
	} else {
		_, _ = fmt.Fprintf(out, "  [FAIL] no calendar named %s — create it in Apple Calendar (iCloud, not shared)\n", TestCalendarPrefix)
	}
	if f.dumpDir != "" {
		_, _ = fmt.Fprintf(out, "\nRaw HTTP (auth redacted) in %s\n", filepath.Join(f.dumpDir, "caldav"))
	}
	return nil
}

// caldavPut implements P0-4 (--variant), P0-7 (--from-google), P0-8 (--case)
// and arbitrary objects (--file).
func caldavPut(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "caldav-put")
	var f caldavFlags
	f.register(fs, true)
	variant := fs.String("variant", "", "P0-4 variant A|B|C|D|E")
	agent := fs.String("schedule-agent", "", "ORGANIZER/ATTENDEE SCHEDULE-AGENT for D/E, --case and --from-google: '' | CLIENT | NONE")
	tcase := fs.String("case", "", "P0-8 case: tz-novtz | tz-vtz | allday | recurring")
	fromGoogle := fs.String("from-google", "", "P0-7: build from this Google event id (UID = iCalUID)")
	file := fs.String("file", "", "PUT this .ics file as-is")
	organizer := fs.String("organizer", "", "ORGANIZER email for --variant/--case (the test organizer account)")
	attendee := fs.String("attendee", "", "ATTENDEE email (default icloud.attendee_email; variant D: the Apple Account address)")
	partstat := fs.String("partstat", "", "override PARTSTAT (P0-7 Google→iCloud test)")
	vtz := fs.Bool("vtimezone", true, "embed VTIMEZONE (Australia/Sydney only in spikes)")
	tz := fs.String("tz", "Australia/Sydney", "time zone for --variant/--case")
	startFlag := fs.String("start", "", "local start 2006-01-02T15:04 (default: 2 days from now at 10:00)")
	uid := fs.String("uid", "", "fixed UID (default: random per run)")
	seq := fs.Int64("sequence", -1, "override SEQUENCE")
	if err := parse(fs, args); err != nil {
		return err
	}
	modes := 0
	for _, m := range []string{*variant, *tcase, *fromGoogle, *file} {
		if m != "" {
			modes++
		}
	}
	if modes != 1 {
		_, _ = fmt.Fprintln(e.stderr, "exactly one of --variant, --case, --from-google or --file is required")
		return ErrUsage
	}
	switch *agent {
	case "", "CLIENT", "NONE":
	default:
		return fmt.Errorf("invalid --schedule-agent %q", *agent)
	}

	s, err := connect(ctx, e, &f, "put", true, true, nil)
	if err != nil {
		return err
	}
	att := *attendee
	if att == "" {
		att = s.cfg.ICloud.AttendeeEmail
	}

	var body []byte
	var objUID string
	now := time.Now()
	switch {
	case *file != "":
		body, err = os.ReadFile(*file) // #nosec G304 -- user-supplied test file
		if err != nil {
			return err
		}
		cal, derr := ical.NewDecoder(bytes.NewReader(body)).Decode()
		if derr != nil {
			return fmt.Errorf("parse %s: %w", *file, derr)
		}
		if objUID = firstUID(cal); objUID == "" {
			return fmt.Errorf("%s has no VEVENT UID", *file)
		}
	case *fromGoogle != "":
		g, gerr := googlecal.NewAuthorized(ctx, s.cfg.Google.ClientFile, s.cfg.Google.TokenFile, s.cfg.Google.Scope, f.insecurePerms)
		if gerr != nil {
			return gerr
		}
		ev, gerr := g.Get(ctx, s.cfg.Google.CalendarID, *fromGoogle)
		if gerr != nil {
			return gerr
		}
		spec, serr := specFromGoogle(ev, att)
		if serr != nil {
			return serr
		}
		spec.ScheduleAgent, spec.VTimezone, spec.Now = *agent, *vtz, now
		if spec.TZ != "" && spec.TZ != "Australia/Sydney" && *vtz {
			_, _ = fmt.Fprintf(e.stderr, "note: no spike VTIMEZONE for %s; emitting TZID without VTIMEZONE\n", spec.TZ)
		}
		applyOverrides(&spec, *partstat, *seq)
		objUID, body = spec.UID, spec.render()
	default:
		if *organizer == "" {
			_, _ = fmt.Fprintln(e.stderr, "--organizer is required (the test organizer's address)")
			return ErrUsage
		}
		if *vtz && *tz != "Australia/Sydney" {
			return fmt.Errorf("spike VTIMEZONE is only available for Australia/Sydney; pass --vtimezone=false for %s", *tz)
		}
		loc, lerr := time.LoadLocation(*tz)
		if lerr != nil {
			return lerr
		}
		start := time.Date(now.Year(), now.Month(), now.Day()+2, 10, 0, 0, 0, loc)
		if *startFlag != "" {
			if start, err = time.ParseInLocation("2006-01-02T15:04", *startFlag, loc); err != nil {
				return fmt.Errorf("--start: %w", err)
			}
		}
		spec := objectSpec{
			Organizer: *organizer, OrganizerCN: "Organizer Test", Attendee: att,
			TZ: *tz, VTimezone: *vtz, Start: start, End: start.Add(30 * time.Minute), Now: now,
		}
		label := ""
		if *variant != "" {
			label = "P0-4 variant " + strings.ToUpper(*variant)
			switch strings.ToUpper(*variant) {
			case "A", "B", "C":
				if spec.ScheduleAgent, err = variantAgent(*variant); err != nil {
					return err
				}
			case "D":
				if *attendee == "" {
					return errors.New("variant D needs --attendee (the Apple Account / alternate address)")
				}
				spec.ScheduleAgent = *agent
			case "E":
				spec.ScheduleAgent = *agent
				spec.ExtraGuests = []string{"guest1@calbridge.invalid", "guest2@calbridge.invalid"}
			default:
				return fmt.Errorf("unknown --variant %q", *variant)
			}
		} else {
			label = "P0-8 " + *tcase
			spec.ScheduleAgent = *agent
			if err := applyCase(&spec, *tcase); err != nil {
				return err
			}
		}
		spec.UID = *uid
		if spec.UID == "" {
			spec.UID = fmt.Sprintf("p0-%s-%s@calbridge.invalid", strings.ToLower(strings.ReplaceAll(label, " ", "-")), randomID())
		}
		spec.Summary = label
		applyOverrides(&spec, *partstat, *seq)
		objUID, body = spec.UID, spec.render()
	}

	href := objectHref(s.cal.Href, objUID)
	cond := icloud.PutCond{IfNoneMatchAny: true}
	if prev, gerr := s.c.Get(ctx, href); gerr == nil {
		cond = icloud.PutCond{IfMatch: prev.ETag}
		_, _ = fmt.Fprintf(e.stdout, "Object exists (ETag %s): updating with If-Match.\n", prev.ETag)
	} else if !errors.Is(gerr, icloud.ErrNotFound) {
		return gerr
	}
	_, _ = writeDump(f.dumpDir, "put-"+stamp()+"-sent.ics", body)

	etag, perr := s.c.PutRaw(ctx, href, body, cond)
	var he *icloud.HTTPError
	switch {
	case perr == nil:
		_, _ = fmt.Fprintf(e.stdout, "PUT %s\n  ok; ETag header: %q\n", href, etag)
	case errors.As(perr, &he):
		_, _ = fmt.Fprintf(e.stdout, "PUT %s\n  HTTP %d\n%s\n", href, he.Status, he.Body)
		return perr
	default:
		return perr
	}
	got, err := s.c.Get(ctx, href)
	if err != nil {
		return fmt.Errorf("GET back: %w", err)
	}
	_, _ = writeDump(f.dumpDir, "put-"+stamp()+"-received.ics", got.Raw)
	_, _ = fmt.Fprintf(e.stdout, "  GET back ETag: %s (matches PUT ETag: %v)\n", got.ETag, icloud.SameETag(etag, got.ETag))
	removed, added := lineDiff(body, got.Raw)
	_, _ = fmt.Fprintln(e.stdout, "  Lines changed by the server (- sent only, + stored only):")
	for _, l := range removed {
		_, _ = fmt.Fprintf(e.stdout, "    - %s\n", l)
	}
	for _, l := range added {
		_, _ = fmt.Fprintf(e.stdout, "    + %s\n", l)
	}
	if len(removed)+len(added) == 0 {
		_, _ = fmt.Fprintln(e.stdout, "    (none)")
	}
	_, _ = fmt.Fprintf(e.stdout, "\nUID %s\nhref %s\nNow check Mac and iPhone (event shown? Accept/Maybe/Decline? organizer?), and the organizer's inbox. Record in docs/phase0-results.md.\n", objUID, href)
	return nil
}

func applyOverrides(spec *objectSpec, partstat string, seq int64) {
	if partstat != "" {
		spec.PartStat = strings.ToUpper(partstat)
	}
	if seq >= 0 {
		spec.Sequence = seq
	}
}

// applyCase sets up the P0-8 objects.
func applyCase(spec *objectSpec, c string) error {
	switch c {
	case "tz-novtz":
		spec.VTimezone = false
	case "tz-vtz":
		spec.VTimezone = true
	case "allday":
		spec.AllDay = true
		d := spec.Start
		spec.Start = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		spec.End = spec.Start.AddDate(0, 0, 2)
	case "recurring":
		// Weekly on Mondays 10:00 Sydney across the DST end (first Sunday of
		// April), one EXDATE and one moved occurrence.
		loc, err := time.LoadLocation("Australia/Sydney")
		if err != nil {
			return err
		}
		spec.TZ = "Australia/Sydney"
		year := time.Now().Year() + 1
		spec.Start = time.Date(year, time.March, 1, 10, 0, 0, 0, loc)
		for spec.Start.Weekday() != time.Monday {
			spec.Start = spec.Start.AddDate(0, 0, 1)
		}
		spec.Start = spec.Start.AddDate(0, 0, 14) // mid/late March
		spec.End = spec.Start.Add(30 * time.Minute)
		ex := spec.Start.AddDate(0, 0, 7)
		moved := spec.Start.AddDate(0, 0, 21)
		spec.Recurrence = []string{
			"RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=6",
			"EXDATE;TZID=Australia/Sydney:" + ex.Format("20060102T150405"),
		}
		spec.Overrides = []override{{
			RecurrenceID: moved,
			Start:        moved.Add(time.Hour),
			End:          moved.Add(90 * time.Minute),
			Summary:      "P0-8 recurring (moved)",
		}}
	default:
		return fmt.Errorf("unknown --case %q", c)
	}
	return nil
}

func firstUID(cal *ical.Calendar) string {
	for _, ch := range cal.Children {
		if ch.Name == ical.CompEvent {
			if p := ch.Props.Get(ical.PropUID); p != nil {
				return p.Value
			}
		}
	}
	return ""
}

// caldavGet prints one object by --href or --uid.
func caldavGet(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "caldav-get")
	var f caldavFlags
	f.register(fs, false)
	href := fs.String("href", "", "object href")
	uid := fs.String("uid", "", "find by UID instead")
	if err := parse(fs, args); err != nil {
		return err
	}
	if (*href == "") == (*uid == "") {
		_, _ = fmt.Fprintln(e.stderr, "exactly one of --href or --uid is required")
		return ErrUsage
	}
	s, err := connect(ctx, e, &f, "get", true, false, nil)
	if err != nil {
		return err
	}
	var o *icloud.Object
	if *uid != "" {
		o, err = s.c.FindByUID(ctx, s.cal.Href, *uid)
	} else {
		o, err = s.c.Get(ctx, *href)
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(e.stdout, "href %s\nETag %s\n", o.Href, o.ETag)
	printPartStats(e, o, s.cfg.ICloud.AttendeeEmail)
	_, _ = fmt.Fprintf(e.stdout, "\n%s", o.Raw)
	return nil
}

func printPartStats(e *env, o *icloud.Object, attendee string) {
	if o.Cal == nil {
		_, _ = fmt.Fprintln(e.stdout, "  (unparseable iCalendar)")
		return
	}
	for _, ch := range o.Cal.Children {
		if ch.Name != ical.CompEvent {
			continue
		}
		rid := "master"
		if p := ch.Props.Get(ical.PropRecurrenceID); p != nil {
			rid = "RECURRENCE-ID " + p.Value
		}
		seq, lm := "", ""
		if p := ch.Props.Get(ical.PropSequence); p != nil {
			seq = p.Value
		}
		if p := ch.Props.Get(ical.PropLastModified); p != nil {
			lm = p.Value
		}
		ps, sched := "(not an attendee)", ""
		for _, a := range ch.Props.Values(ical.PropAttendee) {
			if strings.EqualFold(strings.TrimPrefix(strings.ToLower(a.Value), "mailto:"), attendee) {
				ps = a.Params.Get("PARTSTAT")
				sched = a.Params.Get("SCHEDULE-STATUS")
			}
		}
		orgSched := ""
		if p := ch.Props.Get(ical.PropOrganizer); p != nil {
			orgSched = p.Params.Get("SCHEDULE-STATUS")
		}
		_, _ = fmt.Fprintf(e.stdout, "  %s: PARTSTAT=%s SEQUENCE=%s LAST-MODIFIED=%s attendee SCHEDULE-STATUS=%q organizer SCHEDULE-STATUS=%q\n",
			rid, ps, seq, lm, sched, orgSched)
	}
}

// caldavSync implements the P0-5 observation step: what changed since the
// last run, with PARTSTATs.
func caldavSync(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "caldav-sync")
	var f caldavFlags
	f.register(fs, false)
	reset := fs.Bool("reset", false, "forget the stored sync token (initial sync)")
	if err := parse(fs, args); err != nil {
		return err
	}
	s, err := connect(ctx, e, &f, "sync", true, false, nil)
	if err != nil {
		return err
	}
	stateFile := filepath.Join(f.dumpDir, "caldav-sync-token-"+slugName(s.cal.DisplayName))
	token := ""
	if !*reset {
		if b, rerr := os.ReadFile(stateFile); rerr == nil { // #nosec G304 -- spike state file
			token = strings.TrimSpace(string(b))
		}
	}
	ctag, cerr := s.c.CTag(ctx, s.cal.Href)
	if cerr != nil {
		_, _ = fmt.Fprintf(e.stdout, "getctag: error %v\n", cerr)
	} else {
		_, _ = fmt.Fprintf(e.stdout, "getctag: %s\n", ctag)
	}
	res, err := s.c.SyncCollection(ctx, s.cal.Href, token)
	switch {
	case errors.Is(err, icloud.ErrInvalidSyncToken):
		_, _ = fmt.Fprintln(e.stdout, "stored sync token rejected (valid-sync-token); run with --reset")
		return err
	case errors.Is(err, icloud.ErrSyncUnsupported):
		_, _ = fmt.Fprintf(e.stdout, "sync-collection NOT supported (%v); falling back to getetag listing\n", err)
		members, lerr := s.c.ListETags(ctx, s.cal.Href)
		if lerr != nil {
			return lerr
		}
		res = icloud.SyncResult{Changed: members}
	case err != nil:
		return err
	default:
		_, _ = fmt.Fprintf(e.stdout, "sync-collection ok (token %s): %d changed, %d removed\n", map[bool]string{true: "initial", false: "incremental"}[token == ""], len(res.Changed), len(res.Removed))
	}
	for _, r := range res.Removed {
		_, _ = fmt.Fprintf(e.stdout, "removed  %s\n", r)
	}
	hrefs := make([]string, 0, len(res.Changed))
	for _, m := range res.Changed {
		hrefs = append(hrefs, m.Href)
	}
	objs, err := s.c.MultiGet(ctx, s.cal.Href, hrefs)
	if err != nil {
		return err
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Href < objs[j].Href })
	for i := range objs {
		_, _ = fmt.Fprintf(e.stdout, "changed  %s  ETag %s  UID %s\n", objs[i].Href, objs[i].ETag, firstUIDSafe(objs[i].Cal))
		printPartStats(e, &objs[i], s.cfg.ICloud.AttendeeEmail)
	}
	if res.NewToken != "" {
		if _, err := writeDump(f.dumpDir, filepath.Base(stateFile), []byte(res.NewToken+"\n")); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(e.stdout, "new sync token stored in %s\n", stateFile)
	}
	return nil
}

func firstUIDSafe(cal *ical.Calendar) string {
	if cal == nil {
		return "?"
	}
	return firstUID(cal)
}

func slugName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

// caldavDelete implements P0-9.
func caldavDelete(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "caldav-delete")
	var f caldavFlags
	f.register(fs, true)
	href := fs.String("href", "", "object href (required)")
	mode := fs.String("mode", "", "plain | schedule_reply_f | neutralize_then_delete (required)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *href == "" || *mode == "" {
		_, _ = fmt.Fprintln(e.stderr, "--href and --mode are required")
		return ErrUsage
	}
	s, err := connect(ctx, e, &f, "delete", true, true, nil)
	if err != nil {
		return err
	}
	if err := s.checkHrefInCalendar(*href); err != nil {
		return err
	}
	prev, err := s.c.Get(ctx, *href)
	if err != nil {
		return err
	}
	opts := icloud.DeleteOpts{IfMatch: prev.ETag}
	switch *mode {
	case "plain":
	case "schedule_reply_f":
		no := false
		opts.ScheduleReply = &no
	case "neutralize_then_delete":
		body, nerr := neutralize(prev.Raw)
		if nerr != nil {
			return nerr
		}
		etag, perr := s.c.PutRaw(ctx, *href, body, icloud.PutCond{IfMatch: prev.ETag})
		if perr != nil {
			return fmt.Errorf("neutralize PUT: %w", perr)
		}
		_, _ = fmt.Fprintf(e.stdout, "re-PUT with SCHEDULE-AGENT=NONE: ETag %q\n", etag)
		opts.IfMatch = etag // empty: unconditional delete
	default:
		return fmt.Errorf("unknown --mode %q", *mode)
	}
	if err := s.c.Delete(ctx, *href, opts); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(e.stdout, "DELETE %s (%s) ok. Now check the organizer's inbox and Google guest status; record in docs/phase0-results.md.\n", *href, *mode)
	return nil
}

// neutralize sets SCHEDULE-AGENT=NONE on every ORGANIZER and ATTENDEE.
func neutralize(raw []byte) ([]byte, error) {
	cal, err := ical.NewDecoder(bytes.NewReader(raw)).Decode()
	if err != nil {
		return nil, err
	}
	for _, ch := range cal.Children {
		if ch.Name != ical.CompEvent {
			continue
		}
		for _, name := range []string{ical.PropOrganizer, ical.PropAttendee} {
			props := ch.Props[name]
			for i := range props {
				props[i].Params.Set("SCHEDULE-AGENT", "NONE")
			}
		}
	}
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// statsTransport counts raw HTTP statuses (before client retries) and latency.
type statsTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	byOp map[string]*opStats
}

type opStats struct {
	statuses map[int]int
	total    time.Duration
	n        int
	max      time.Duration
	retryAft int
}

func (s *statsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := s.base.RoundTrip(req)
	d := time.Since(start)
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.byOp[req.Method]
	if st == nil {
		st = &opStats{statuses: map[int]int{}}
		s.byOp[req.Method] = st
	}
	st.n++
	st.total += d
	if d > st.max {
		st.max = d
	}
	if err != nil {
		st.statuses[-1]++
		return resp, err
	}
	st.statuses[resp.StatusCode]++
	if resp.Header.Get("Retry-After") != "" {
		st.retryAft++
	}
	return resp, nil
}

// caldavVolume implements P0-10 with plain events (no ORGANIZER/ATTENDEE, so
// nothing can be scheduled), deleting only the objects it created.
func caldavVolume(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "caldav-volume")
	var f caldavFlags
	f.register(fs, true)
	n := fs.Int("n", 50, "objects to PUT and GET")
	syncs := fs.Int("syncs", 10, "sync-collection REPORTs")
	spacing := fs.Duration("spacing", 200*time.Millisecond, "delay between requests")
	if err := parse(fs, args); err != nil {
		return err
	}
	f.dumpDir = "" // thousands of dump files help nobody
	stats := &statsTransport{base: http.DefaultTransport, byOp: map[string]*opStats{}}
	s, err := connect(ctx, e, &f, "volume", true, true, stats)
	if err != nil {
		return err
	}
	now := time.Now()
	loc, _ := time.LoadLocation("Australia/Sydney")
	var created []string
	defer func() {
		cctx := context.WithoutCancel(ctx)
		for _, h := range created {
			if derr := s.c.Delete(cctx, h, icloud.DeleteOpts{}); derr != nil && !errors.Is(derr, icloud.ErrNotFound) {
				_, _ = fmt.Fprintf(e.stderr, "cleanup %s: %v\n", h, derr)
			}
			time.Sleep(*spacing)
		}
		printStats(e, stats)
	}()
	pause := func() error { return sleepCtx(ctx, *spacing) }
	for i := 0; i < *n; i++ {
		start := time.Date(now.Year(), now.Month(), now.Day()+3, 8, 0, 0, 0, loc).Add(time.Duration(i) * 15 * time.Minute)
		spec := objectSpec{UID: fmt.Sprintf("p0-10-%s-%03d@calbridge.invalid", randomID(), i), Summary: "P0-10 volume",
			TZ: "Australia/Sydney", VTimezone: true, Start: start, End: start.Add(10 * time.Minute), Now: now}
		h := objectHref(s.cal.Href, spec.UID)
		if _, err := s.c.PutRaw(ctx, h, spec.render(), icloud.PutCond{IfNoneMatchAny: true}); err != nil {
			_, _ = fmt.Fprintf(e.stderr, "PUT %d: %v\n", i, err)
		} else {
			created = append(created, h)
		}
		if err := pause(); err != nil {
			return err
		}
	}
	for _, h := range created {
		if _, err := s.c.Get(ctx, h); err != nil {
			_, _ = fmt.Fprintf(e.stderr, "GET %s: %v\n", h, err)
		}
		if err := pause(); err != nil {
			return err
		}
	}
	for i := 0; i < *syncs; i++ {
		if _, err := s.c.SyncCollection(ctx, s.cal.Href, ""); err != nil {
			_, _ = fmt.Fprintf(e.stderr, "REPORT %d: %v\n", i, err)
		}
		if err := pause(); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(e.stdout, "created %d object(s); cleaning up…\n", len(created))
	return nil
}

func printStats(e *env, s *statsTransport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "METHOD\tREQUESTS\tSTATUSES\tAVG\tMAX\tRETRY-AFTER")
	methods := make([]string, 0, len(s.byOp))
	for m := range s.byOp {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	for _, m := range methods {
		st := s.byOp[m]
		codes := make([]string, 0, len(st.statuses))
		for c, k := range st.statuses {
			codes = append(codes, fmt.Sprintf("%d×%d", c, k))
		}
		sort.Strings(codes)
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%d\n", m, st.n, strings.Join(codes, " "),
			(st.total / time.Duration(st.n)).Round(time.Millisecond), st.max.Round(time.Millisecond), st.retryAft)
	}
	_ = tw.Flush()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

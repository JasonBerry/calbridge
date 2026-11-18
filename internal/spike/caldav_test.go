package spike

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	calendar "google.golang.org/api/calendar/v3"
)

func TestCheckWritable(t *testing.T) {
	if checkWritable("calbridge-test", false) != nil || checkWritable("calbridge-test-2", false) != nil {
		t.Error("test calendars refused")
	}
	if checkWritable("Home", false) == nil {
		t.Error("main calendar allowed without --i-know")
	}
	if checkWritable("Home", true) != nil {
		t.Error("--i-know ignored")
	}
}

func TestObjectHref(t *testing.T) {
	h := objectHref("/1/calendars/abc/", "x@google.com")
	if h != objectHref("/1/calendars/abc", "x@google.com") || !strings.HasPrefix(h, "/1/calendars/abc/calbridge-") ||
		!strings.HasSuffix(h, ".ics") || len(strings.TrimSuffix(strings.TrimPrefix(h, "/1/calendars/abc/calbridge-"), ".ics")) != 26 {
		t.Errorf("href = %s", h)
	}
	if objectHref("/c/", "a") == objectHref("/c/", "b") {
		t.Error("hrefs collide")
	}
}

func decodeICS(t *testing.T, b []byte) *ical.Calendar {
	t.Helper()
	cal, err := ical.NewDecoder(bytes.NewReader(b)).Decode()
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	return cal
}

func baseSpec(t *testing.T) objectSpec {
	loc, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 20, 10, 0, 0, 0, loc)
	return objectSpec{UID: "u1@calbridge.invalid", Summary: "P0-4 variant B, with; commas", Organizer: "ORGANIZER_TEST@example.com",
		OrganizerCN: "Organizer Test", ScheduleAgent: "CLIENT", Attendee: "user@YOUR_DOMAIN", TZ: "Australia/Sydney",
		VTimezone: true, Start: start, End: start.Add(30 * time.Minute), Now: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)}
}

func TestRenderVariant(t *testing.T) {
	raw := baseSpec(t).render()
	s := strings.Join(unfoldLines(raw), "\r\n") + "\r\n"
	for _, want := range []string{
		"DTSTART;TZID=Australia/Sydney:20261120T100000\r\n",
		"ORGANIZER;CN=Organizer Test;SCHEDULE-AGENT=CLIENT:mailto:ORGANIZER_TEST@example.com\r\n",
		"PARTSTAT=NEEDS-ACTION;RSVP=TRUE;ROLE=REQ-PARTICIPANT;SCHEDULE-AGENT=CLIENT:mailto:user@YOUR_DOMAIN\r\n",
		"BEGIN:VTIMEZONE\r\n", "X-CALBRIDGE-GID:phase0-test\r\n", `SUMMARY:P0-4 variant B\, with\; commas`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	cal := decodeICS(t, raw)
	if firstUID(cal) != "u1@calbridge.invalid" {
		t.Error("UID")
	}
}

func TestRenderCases(t *testing.T) {
	for _, c := range []string{"tz-novtz", "tz-vtz", "allday", "recurring"} {
		t.Run(c, func(t *testing.T) {
			spec := baseSpec(t)
			if err := applyCase(&spec, c); err != nil {
				t.Fatal(err)
			}
			raw := spec.render()
			s := string(raw)
			decodeICS(t, raw)
			switch c {
			case "tz-novtz":
				if strings.Contains(s, "VTIMEZONE") {
					t.Error("VTIMEZONE present")
				}
			case "allday":
				if !strings.Contains(s, "DTSTART;VALUE=DATE:20261120") || !strings.Contains(s, "DTEND;VALUE=DATE:20261122") || strings.Contains(s, "VTIMEZONE") {
					t.Errorf("all-day wrong:\n%s", s)
				}
			case "recurring":
				if strings.Count(s, "BEGIN:VEVENT") != 2 || !strings.Contains(s, "RRULE:FREQ=WEEKLY") ||
					!strings.Contains(s, "EXDATE;TZID=Australia/Sydney:") || !strings.Contains(s, "RECURRENCE-ID;TZID=Australia/Sydney:") {
					t.Errorf("recurring wrong:\n%s", s)
				}
				// Series must span the first Sunday of April (DST end).
				loc, _ := time.LoadLocation("Australia/Sydney")
				last := spec.Start.AddDate(0, 0, 35)
				_, off1 := spec.Start.In(loc).Zone()
				_, off2 := last.In(loc).Zone()
				if off1 == off2 {
					t.Errorf("series %s..%s does not cross a DST change", spec.Start, last)
				}
			}
		})
	}
	spec := baseSpec(t)
	if applyCase(&spec, "nope") == nil {
		t.Error("unknown case accepted")
	}
}

func TestFoldLongLines(t *testing.T) {
	spec := baseSpec(t)
	spec.Summary = strings.Repeat("日本語のタイトル🎉", 12)
	raw := spec.render()
	for _, l := range strings.Split(string(raw), "\r\n") {
		if len(l) > 75 {
			t.Errorf("line longer than 75 octets: %q", l)
		}
	}
	cal := decodeICS(t, raw)
	got, _ := cal.Children[1].Props.Text("SUMMARY")
	if got != spec.Summary {
		t.Errorf("summary round trip = %q", got)
	}
}

func TestSpecFromGoogle(t *testing.T) {
	ev := &calendar.Event{
		Id: "gid", ICalUID: "gid@google.com", Summary: "Meet", Sequence: 2,
		Organizer: &calendar.EventOrganizer{Email: "ORGANIZER_TEST@example.com", DisplayName: "Org"},
		Attendees: []*calendar.EventAttendee{{Email: "user@YOUR_DOMAIN", Self: true, ResponseStatus: "tentative"}},
		Start:     &calendar.EventDateTime{DateTime: "2026-11-20T10:00:00+11:00", TimeZone: "Australia/Sydney"},
		End:       &calendar.EventDateTime{DateTime: "2026-11-20T10:30:00+11:00", TimeZone: "Australia/Sydney"},
	}
	spec, err := specFromGoogle(ev, "user@YOUR_DOMAIN")
	if err != nil {
		t.Fatal(err)
	}
	spec.Now = time.Now()
	s := string(spec.render())
	for _, want := range []string{"UID:gid@google.com", "SEQUENCE:2", "PARTSTAT=TENTATIVE", "DTSTART;TZID=Australia/Sydney:20261120T100000", "X-CALBRIDGE-GID:gid"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	if _, err := specFromGoogle(&calendar.Event{Id: "x"}, "u"); err == nil {
		t.Error("event without iCalUID accepted")
	}
}

func TestLineDiff(t *testing.T) {
	a := []byte("BEGIN:VEVENT\r\nSUMMARY:long\r\n  folded\r\nX-CALBRIDGE-GID:1\r\nEND:VEVENT\r\n")
	b := []byte("BEGIN:VEVENT\r\nSUMMARY:long folded\r\nX-APPLE-X:1\r\nEND:VEVENT\r\n")
	removed, added := lineDiff(a, b)
	if strings.Join(removed, "|") != "X-CALBRIDGE-GID:1" || strings.Join(added, "|") != "X-APPLE-X:1" {
		t.Errorf("removed=%v added=%v", removed, added)
	}
}

func TestNeutralize(t *testing.T) {
	out, err := neutralize(baseSpec(t).render())
	if err != nil {
		t.Fatal(err)
	}
	cal := decodeICS(t, out)
	for _, ch := range cal.Children {
		if ch.Name != ical.CompEvent {
			continue
		}
		for _, name := range []string{"ORGANIZER", "ATTENDEE"} {
			for _, p := range ch.Props.Values(name) {
				if p.Params.Get("SCHEDULE-AGENT") != "NONE" {
					t.Errorf("%s not neutralized: %v", name, p.Params)
				}
			}
		}
	}
}

// fakeICloud serves discovery fixtures from the icloud package and records PUTs.
type fakeICloud struct {
	mu   sync.Mutex
	puts []string
	objs map[string][]byte
}

func (f *fakeICloud) handler(t *testing.T, self *string) http.HandlerFunc {
	fixture := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "icloud", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "{{PARTITION}}", *self)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		ms := func(s string) { w.WriteHeader(http.StatusMultiStatus); _, _ = io.WriteString(w, s) }
		switch r.Method + " " + r.URL.Path {
		case "PROPFIND /":
			ms(fixture("principal.xml"))
		case "PROPFIND /123456789/principal/":
			ms(fixture("home.xml"))
		case "PROPFIND /123456789/calendars/":
			ms(fixture("calendars.xml"))
		default:
			switch r.Method {
			case http.MethodPut:
				f.puts = append(f.puts, r.URL.Path+" If-None-Match="+r.Header.Get("If-None-Match"))
				f.objs[r.URL.Path] = body
				w.Header().Set("ETag", `"e1"`)
				w.WriteHeader(http.StatusCreated)
			case http.MethodGet:
				b, ok := f.objs[r.URL.Path]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("ETag", `"e1"`)
				_, _ = w.Write(append(b, []byte("X-ADDED-BY-SERVER:1\r\n")...))
			default:
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusBadRequest)
			}
		}
	}
}

func setupSpikeEnv(t *testing.T) (*fakeICloud, string, string) {
	t.Helper()
	f := &fakeICloud{objs: map[string][]byte{}}
	var url string
	srv := httptest.NewServer(f.handler(t, &url))
	t.Cleanup(srv.Close)
	url = srv.URL
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	if err := os.WriteFile(pw, []byte("app-pw"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.yaml")
	y := "google:\n  self_email: user@example.com\nicloud:\n  username: apple@example.com\n  attendee_email: user@example.com\n  calendar: calbridge-test\n  app_password_file: " + pw + "\n"
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, cfg, srv.URL + "/"
}

func runSpike(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	e := &env{stdout: &out, stderr: &errb, getenv: func(string) string { return "" }}
	c := subcommands()[args[0]]
	err := c.run(context.Background(), e, args[1:])
	return out.String() + errb.String(), err
}

func TestCaldavPutRefusesNonTestCalendar(t *testing.T) {
	f, cfg, base := setupSpikeEnv(t)
	_, err := runSpike(t, "caldav-put", "--config", cfg, "--base", base, "--dump", "", "--calendar", "Home",
		"--variant", "A", "--organizer", "ORGANIZER_TEST@example.com")
	if err == nil || !strings.Contains(err.Error(), "refusing to write") {
		t.Fatalf("err = %v", err)
	}
	if len(f.puts) != 0 {
		t.Errorf("PUTs sent: %v", f.puts)
	}
}

func TestCaldavPutVariant(t *testing.T) {
	f, cfg, base := setupSpikeEnv(t)
	dump := t.TempDir()
	out, err := runSpike(t, "caldav-put", "--config", cfg, "--base", base, "--dump", dump,
		"--variant", "C", "--organizer", "ORGANIZER_TEST@example.com", "--start", "2026-11-20T10:00")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(f.puts) != 1 || !strings.HasPrefix(f.puts[0], "/123456789/calendars/0A1B2C3D-TEST/calbridge-") || !strings.HasSuffix(f.puts[0], "If-None-Match=*") {
		t.Fatalf("puts = %v", f.puts)
	}
	for _, b := range f.objs {
		if !strings.Contains(string(b), "SCHEDULE-AGENT=NONE") {
			t.Errorf("variant C object lacks SCHEDULE-AGENT=NONE:\n%s", b)
		}
	}
	if !strings.Contains(out, "+ X-ADDED-BY-SERVER:1") {
		t.Errorf("server diff not reported:\n%s", out)
	}
	entries, _ := os.ReadDir(filepath.Join(dump, "caldav"))
	if len(entries) == 0 {
		t.Error("no HTTP dumps written")
	}
	for _, en := range entries {
		b, _ := os.ReadFile(filepath.Join(dump, "caldav", en.Name()))
		if strings.Contains(string(b), "Basic ") {
			t.Errorf("credentials in dump %s", en.Name())
		}
	}
}

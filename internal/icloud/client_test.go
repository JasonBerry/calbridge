package icloud

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

const calHref = "/123456789/calendars/0A1B2C3D-TEST/"

type call struct {
	method, path string
	header       http.Header
	body         string
}

// fakeServer routes "METHOD path" to handlers and records calls.
type fakeServer struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []call
	routes map[string]func(w http.ResponseWriter, r *http.Request, body string)
}

func newFake(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, routes: map[string]func(http.ResponseWriter, *http.Request, string){}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, call{r.Method, r.URL.Path, r.Header.Clone(), string(b)})
		h, ok := f.routes[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		if u, p, ok := r.BasicAuth(); !ok || u != "apple@example.com" || p != "app-pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !ok {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h(w, r, string(b))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) on(method, path string, h func(w http.ResponseWriter, r *http.Request, body string)) {
	f.routes[method+" "+path] = h
}

func fixture(t *testing.T, name string, repl ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer(repl...).Replace(string(b))
}

func multistatusHandler(body string) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, _ *http.Request, _ string) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, body)
	}
}

func status(code int, hdr ...string) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, _ *http.Request, _ string) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], hdr[i+1])
		}
		w.WriteHeader(code)
	}
}

func newTestClient(t *testing.T, base string) (*Client, *[]time.Duration) {
	t.Helper()
	c, err := New(Options{BaseURL: base + "/", Username: "apple@example.com", Password: "app-pw", RateLimitRPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var sleeps []time.Duration
	c.SetSleep(func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil })
	return c, &sleeps
}

func TestDiscoverSwitchesToPartition(t *testing.T) {
	entry := newFake(t)
	partition := newFake(t)
	entry.on("PROPFIND", "/", multistatusHandler(fixture(t, "principal.xml")))
	entry.on("PROPFIND", "/123456789/principal/", multistatusHandler(fixture(t, "home.xml", "{{PARTITION}}", partition.srv.URL)))
	partition.on("PROPFIND", "/123456789/calendars/", multistatusHandler(fixture(t, "calendars.xml")))

	c, _ := newTestClient(t, entry.srv.URL)
	d, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Principal != "/123456789/principal/" || d.CalendarHome != "/123456789/calendars/" || d.BaseURL != partition.srv.URL+"/" {
		t.Errorf("discovery = %+v", d)
	}
	if c.BaseURL() != partition.srv.URL+"/" {
		t.Errorf("base not switched: %s", c.BaseURL())
	}
	if !d.HasAddress("user@YOUR_DOMAIN") || d.HasAddress("someone@else") {
		t.Errorf("address set = %v", d.AddressSet)
	}
	if d.ScheduleInbox == "" || d.ScheduleOutbox == "" {
		t.Error("schedule inbox/outbox missing")
	}
	if entry.calls[0].header.Get("Depth") != "0" || partition.calls[0].header.Get("Depth") != "1" {
		t.Error("wrong Depth headers")
	}

	want := []CalendarInfo{
		{Href: "/123456789/calendars/home/", DisplayName: "Home", Components: []string{"VEVENT"}, CTag: "HwoQEgwAAA111",
			SyncToken: "https://example.invalid/ns/sync/111", SupportsSync: true, Privileges: []string{"read", "write"}, IsCalendar: true},
		{Href: calHref, DisplayName: "calbridge-test", Components: []string{"VEVENT"}, CTag: "HwoQEgwAAA222",
			SyncToken: "https://example.invalid/ns/sync/222", SupportsSync: true, IsCalendar: true},
		{Href: "/123456789/calendars/tasks/", DisplayName: "Reminders", Components: []string{"VTODO"}, IsCalendar: true},
		{Href: "/123456789/calendars/inbox/"},
	}
	if diff := cmp.Diff(want, d.Calendars); diff != "" {
		t.Errorf("calendars (-want +got):\n%s", diff)
	}
	if ci, ok := d.FindCalendar("calbridge-test"); !ok || ci.Href != calHref {
		t.Errorf("FindCalendar = %+v %v", ci, ok)
	}
}

func TestDiscoverAuthFailure(t *testing.T) {
	f := newFake(t)
	c, err := New(Options{BaseURL: f.srv.URL + "/", Username: "apple@example.com", Password: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Discover(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v", err)
	}
}

func TestPut(t *testing.T) {
	href := calHref + "calbridge-x.ics"
	cases := []struct {
		name     string
		cond     PutCond
		handler  func(http.ResponseWriter, *http.Request, string)
		wantETag string
		wantErr  error
	}{
		{"create unquoted etag", PutCond{IfNoneMatchAny: true}, status(201, "ETag", "abc"), `"abc"`, nil},
		{"update quoted etag", PutCond{IfMatch: "old"}, status(204, "ETag", `"new"`), `"new"`, nil},
		{"no etag returned", PutCond{IfNoneMatchAny: true}, status(201), "", nil},
		{"exists", PutCond{IfNoneMatchAny: true}, status(412), "", ErrPreconditionFailed},
		{"uid conflict", PutCond{IfNoneMatchAny: true}, func(w http.ResponseWriter, _ *http.Request, _ string) {
			w.WriteHeader(403)
			_, _ = io.WriteString(w, fixture(t, "uid-conflict.xml"))
		}, "", ErrUIDConflict},
		{"forbidden", PutCond{}, status(403), "", ErrAuth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.on("PUT", href, tc.handler)
			c, _ := newTestClient(t, f.srv.URL)
			etag, err := c.PutRaw(context.Background(), href, []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"), tc.cond)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || etag != tc.wantETag {
				t.Fatalf("etag = %q, err = %v", etag, err)
			}
			h := f.calls[0].header
			if tc.cond.IfNoneMatchAny && h.Get("If-None-Match") != "*" {
				t.Error("If-None-Match missing")
			}
			if tc.cond.IfMatch != "" && h.Get("If-Match") != `"old"` {
				t.Errorf("If-Match = %q", h.Get("If-Match"))
			}
			if !strings.HasPrefix(h.Get("Content-Type"), "text/calendar") {
				t.Errorf("Content-Type = %q", h.Get("Content-Type"))
			}
		})
	}
}

func TestDelete(t *testing.T) {
	href := calHref + "calbridge-x.ics"
	f := newFake(t)
	f.on("DELETE", href, status(204))
	c, _ := newTestClient(t, f.srv.URL)
	no := false
	if err := c.Delete(context.Background(), href, DeleteOpts{IfMatch: "e1", ScheduleReply: &no}); err != nil {
		t.Fatal(err)
	}
	h := f.calls[0].header
	if h.Get("Schedule-Reply") != "F" || h.Get("If-Match") != `"e1"` {
		t.Errorf("headers = %v", h)
	}
	if err := c.Delete(context.Background(), href, DeleteOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.calls[1].header["Schedule-Reply"]; ok {
		t.Error("Schedule-Reply sent when nil")
	}

	f.on("DELETE", href, status(404))
	if err := c.Delete(context.Background(), href, DeleteOpts{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("404: err = %v", err)
	}
	f.on("DELETE", href, status(412))
	if err := c.Delete(context.Background(), href, DeleteOpts{IfMatch: "x"}); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("412: err = %v", err)
	}
}

func TestSyncCollection(t *testing.T) {
	f := newFake(t)
	var tokens []string
	f.on("REPORT", calHref, func(w http.ResponseWriter, r *http.Request, body string) {
		if r.Header.Get("Depth") != "0" {
			t.Errorf("Depth = %q", r.Header.Get("Depth"))
		}
		start := strings.Index(body, "<d:sync-token>") + len("<d:sync-token>")
		tok := body[start:strings.Index(body, "</d:sync-token>")]
		tokens = append(tokens, tok)
		switch tok {
		case "":
			multistatusHandler(fixture(t, "sync-initial.xml"))(w, r, body)
		case "https://example.invalid/ns/sync/300":
			multistatusHandler(fixture(t, "sync-truncated.xml"))(w, r, body)
		case "https://example.invalid/ns/sync/301":
			multistatusHandler(fixture(t, "sync-delta.xml"))(w, r, body)
		default:
			w.WriteHeader(403)
			_, _ = io.WriteString(w, fixture(t, "sync-invalid-token.xml"))
		}
	})
	c, _ := newTestClient(t, f.srv.URL)
	ctx := context.Background()

	res, err := c.SyncCollection(ctx, calHref, "")
	if err != nil {
		t.Fatal(err)
	}
	want := SyncResult{
		Changed: []Member{
			{Href: calHref + "calbridge-aaa.ics", ETag: `"etag-a1"`},
			{Href: calHref + "user event.ics", ETag: `"etag-b1"`},
		},
		NewToken: "https://example.invalid/ns/sync/300",
	}
	if diff := cmp.Diff(want, res); diff != "" {
		t.Errorf("initial (-want +got):\n%s", diff)
	}

	res, err = c.SyncCollection(ctx, calHref, res.NewToken)
	if err != nil {
		t.Fatal(err)
	}
	want = SyncResult{
		Changed: []Member{
			{Href: calHref + "calbridge-aaa.ics", ETag: `"etag-a2"`},
			{Href: calHref + "calbridge-ccc.ics", ETag: `W/"etag-c1"`},
		},
		Removed:  []string{calHref + "user event.ics"},
		NewToken: "https://example.invalid/ns/sync/302",
	}
	if diff := cmp.Diff(want, res); diff != "" {
		t.Errorf("truncated+delta (-want +got):\n%s", diff)
	}

	if _, err := c.SyncCollection(ctx, calHref, "stale"); !errors.Is(err, ErrInvalidSyncToken) {
		t.Errorf("stale token: err = %v", err)
	}
	if len(tokens) != 4 {
		t.Errorf("REPORT calls = %d", len(tokens))
	}
}

func TestRetryHonoursRetryAfter(t *testing.T) {
	f := newFake(t)
	n := 0
	f.on("GET", calHref+"a.ics", func(w http.ResponseWriter, r *http.Request, _ string) {
		n++
		switch n {
		case 1:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(503)
		case 2:
			w.WriteHeader(429)
		default:
			w.Header().Set("Content-Type", "text/plain") // iCloud quirk
			w.Header().Set("ETag", `W/"weak-1"`)
			_, _ = io.WriteString(w, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:u1\r\nDTSTAMP:20261118T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
		}
	})
	c, sleeps := newTestClient(t, f.srv.URL)
	o, err := c.Get(context.Background(), calHref+"a.ics")
	if err != nil {
		t.Fatal(err)
	}
	if o.ETag != `W/"weak-1"` || objectUID(o.Cal) != "u1" {
		t.Errorf("object = %+v", o)
	}
	if len(*sleeps) != 2 || (*sleeps)[0] != 7*time.Second || (*sleeps)[1] < 2*time.Second {
		t.Errorf("sleeps = %v", *sleeps)
	}
}

func TestRetryGivesUp(t *testing.T) {
	f := newFake(t)
	f.on("GET", calHref+"a.ics", status(503))
	c, _ := newTestClient(t, f.srv.URL)
	_, err := c.Get(context.Background(), calHref+"a.ics")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 503 || len(f.calls) != 5 {
		t.Fatalf("err = %v, calls = %d", err, len(f.calls))
	}
}

func TestGetNotFound(t *testing.T) {
	f := newFake(t)
	f.on("GET", calHref+"gone.ics", status(404))
	c, _ := newTestClient(t, f.srv.URL)
	if _, err := c.Get(context.Background(), calHref+"gone.ics"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestMultiGet(t *testing.T) {
	f := newFake(t)
	f.on("REPORT", calHref, func(w http.ResponseWriter, r *http.Request, body string) {
		if !strings.Contains(body, "calendar-multiget") || !strings.Contains(body, "<d:href>"+calHref+"missing.ics</d:href>") {
			t.Errorf("body = %s", body)
		}
		multistatusHandler(fixture(t, "multiget.xml"))(w, r, body)
	})
	c, _ := newTestClient(t, f.srv.URL)
	objs, err := c.MultiGet(context.Background(), calHref, []string{calHref + "calbridge-aaa.ics", calHref + "missing.ics"})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].ETag != `"etag-a2"` || objectUID(objs[0].Cal) != "abc123@google.com" {
		t.Fatalf("objects = %+v", objs)
	}
	ev := objs[0].Cal.Children[0]
	if s, _ := ev.Props.Text("SUMMARY"); s != "Test & check" {
		t.Errorf("summary = %q", s)
	}
	if ev.Props.Get("X-CALBRIDGE-GID") == nil {
		t.Error("X-prop lost in parse")
	}
}

func TestMultiGetBatches(t *testing.T) {
	f := newFake(t)
	f.on("REPORT", calHref, multistatusHandler(fixture(t, "empty-multistatus.xml")))
	c, _ := newTestClient(t, f.srv.URL)
	hrefs := make([]string, 120)
	for i := range hrefs {
		hrefs[i] = calHref + "x.ics"
	}
	if _, err := c.MultiGet(context.Background(), calHref, hrefs); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 {
		t.Errorf("batches = %d, want 3", len(f.calls))
	}
}

func TestListETagsAndCTag(t *testing.T) {
	f := newFake(t)
	f.on("PROPFIND", calHref, func(w http.ResponseWriter, r *http.Request, body string) {
		if r.Header.Get("Depth") == "1" {
			multistatusHandler(fixture(t, "etags.xml"))(w, r, body)
			return
		}
		multistatusHandler(fixture(t, "ctag.xml"))(w, r, body)
	})
	c, _ := newTestClient(t, f.srv.URL)
	ms, err := c.ListETags(context.Background(), calHref)
	if err != nil {
		t.Fatal(err)
	}
	want := []Member{{calHref + "calbridge-aaa.ics", `"etag-a2"`}, {calHref + "other.ics", `"etag-o1"`}}
	if diff := cmp.Diff(want, ms); diff != "" {
		t.Errorf("etags (-want +got):\n%s", diff)
	}
	ctag, err := c.CTag(context.Background(), calHref)
	if err != nil || ctag != "HwoQEgwAAA333" {
		t.Errorf("ctag = %q, %v", ctag, err)
	}
}

func TestFindByUIDFallsBackToScan(t *testing.T) {
	f := newFake(t)
	f.on("REPORT", calHref, func(w http.ResponseWriter, r *http.Request, body string) {
		if strings.Contains(body, "calendar-query") {
			w.WriteHeader(501) // pretend UID query unsupported
			return
		}
		multistatusHandler(fixture(t, "multiget.xml"))(w, r, body)
	})
	f.on("PROPFIND", calHref, multistatusHandler(fixture(t, "etags.xml")))
	c, _ := newTestClient(t, f.srv.URL)
	o, err := c.FindByUID(context.Background(), calHref, "abc123@google.com")
	if err != nil || o.Href != calHref+"calbridge-aaa.ics" {
		t.Fatalf("o = %+v, err = %v", o, err)
	}
	if _, err := c.FindByUID(context.Background(), calHref, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing uid: err = %v", err)
	}
}

func TestNormalizeETag(t *testing.T) {
	for in, want := range map[string]string{
		`abc`: `"abc"`, `"abc"`: `"abc"`, ` "abc" `: `"abc"`, `W/"abc"`: `W/"abc"`, `W/abc`: `W/"abc"`, ``: ``,
	} {
		if got := NormalizeETag(in); got != want {
			t.Errorf("NormalizeETag(%q) = %q, want %q", in, got, want)
		}
	}
	if !SameETag("abc", `"abc"`) || SameETag("", "") || SameETag(`"a"`, `"b"`) {
		t.Error("SameETag")
	}
}

func TestDumpTransportRedacts(t *testing.T) {
	f := newFake(t)
	f.on("GET", calHref+"a.ics", status(404))
	dir := t.TempDir()
	c, err := New(Options{
		BaseURL: f.srv.URL + "/", Username: "apple@example.com", Password: "app-pw",
		HTTPClient: &http.Client{Transport: &DumpTransport{Dir: dir}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Get(context.Background(), calHref+"a.ics")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dump files = %d", len(entries))
	}
	b, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	s := string(b)
	if !strings.Contains(s, "Authorization: REDACTED") || strings.Contains(s, "Basic ") || !strings.Contains(s, "404") {
		t.Errorf("dump = %s", s)
	}
}

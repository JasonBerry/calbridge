package googlecal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/api/option"
)

type recorded struct {
	method, path string
	query        map[string][]string
	body         []byte
}

type fakeAPI struct {
	t       *testing.T
	mu      sync.Mutex
	reqs    []recorded
	handler func(w http.ResponseWriter, r *http.Request, n int)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.Query(), body})
	n := len(f.reqs)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	f.handler(w, r, n)
}

func newClient(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int)) (*Client, *fakeAPI, *[]time.Duration) {
	t.Helper()
	f := &fakeAPI{t: t, handler: h}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(context.Background(), option.WithHTTPClient(srv.Client()), option.WithEndpoint(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	var sleeps []time.Duration
	c.SetSleep(func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil })
	return c, f, &sleeps
}

func apiError(w http.ResponseWriter, status int, reason string) {
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"x","errors":[{"reason":%q}]}}`, status, reason)
}

func TestListAllPaginates(t *testing.T) {
	c, f, _ := newClient(t, func(w http.ResponseWriter, r *http.Request, n int) {
		switch r.URL.Query().Get("pageToken") {
		case "":
			_, _ = io.WriteString(w, `{"items":[{"id":"a"},{"id":"b"}],"nextPageToken":"p2"}`)
		case "p2":
			_, _ = io.WriteString(w, `{"items":[{"id":"c","status":"cancelled"}],"nextSyncToken":"sync-2"}`)
		}
	})
	timeMin := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	items, tok, err := ListAll(context.Background(), c, ListRequest{CalendarID: "primary", TimeMin: timeMin})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[2].Status != "cancelled" || tok != "sync-2" {
		t.Fatalf("items=%d tok=%q", len(items), tok)
	}
	q := f.reqs[0].query
	if f.reqs[0].path != "/calendars/primary/events" || q["timeMin"][0] != "2026-10-19T00:00:00Z" ||
		q["showDeleted"][0] != "false" || q["syncToken"] != nil {
		t.Errorf("full sync request = %+v", f.reqs[0])
	}
	if f.reqs[1].query["timeMin"][0] != "2026-10-19T00:00:00Z" {
		t.Error("page 2 must repeat the same parameters")
	}
}

func TestListIncrementalUsesOnlySyncToken(t *testing.T) {
	c, f, _ := newClient(t, func(w http.ResponseWriter, r *http.Request, n int) {
		_, _ = io.WriteString(w, `{"items":[],"nextSyncToken":"s2"}`)
	})
	_, tok, err := ListAll(context.Background(), c, ListRequest{CalendarID: "primary", SyncToken: "s1", TimeMin: time.Now()})
	if err != nil || tok != "s2" {
		t.Fatal(tok, err)
	}
	q := f.reqs[0].query
	if q["syncToken"][0] != "s1" || q["timeMin"] != nil || q["showDeleted"] != nil {
		t.Errorf("incremental query = %v", q)
	}
}

func TestListErrors(t *testing.T) {
	cases := []struct {
		status int
		reason string
		want   error
	}{
		{410, "fullSyncRequired", ErrSyncTokenGone},
		{401, "authError", ErrAuth},
		{403, "forbidden", ErrAuth},
		{404, "notFound", ErrNotFound},
	}
	for _, tc := range cases {
		c, f, sleeps := newClient(t, func(w http.ResponseWriter, r *http.Request, n int) { apiError(w, tc.status, tc.reason) })
		_, err := c.List(context.Background(), ListRequest{CalendarID: "primary", SyncToken: "s"})
		if !errors.Is(err, tc.want) {
			t.Errorf("%d: err = %v, want %v", tc.status, err, tc.want)
		}
		if len(f.reqs) != 1 || len(*sleeps) != 0 {
			t.Errorf("%d: retried (%d requests)", tc.status, len(f.reqs))
		}
	}
}

func TestRetryBackoff(t *testing.T) {
	c, f, sleeps := newClient(t, func(w http.ResponseWriter, r *http.Request, n int) {
		switch n {
		case 1:
			apiError(w, 503, "backendError")
		case 2:
			apiError(w, 403, "rateLimitExceeded")
		case 3:
			apiError(w, 429, "rateLimitExceeded")
		default:
			_, _ = io.WriteString(w, `{"id":"e1"}`)
		}
	})
	ev, err := c.Get(context.Background(), "primary", "e1")
	if err != nil || ev.Id != "e1" {
		t.Fatal(ev, err)
	}
	if len(f.reqs) != 4 || len(*sleeps) != 3 {
		t.Fatalf("requests=%d sleeps=%v", len(f.reqs), *sleeps)
	}
	for i, d := range *sleeps {
		lo := time.Duration(1<<i) * time.Second
		if d < lo || d >= lo+time.Second {
			t.Errorf("sleep %d = %s, want [%s, %s)", i, d, lo, lo+time.Second)
		}
	}
}

func TestRetryGivesUp(t *testing.T) {
	c, f, _ := newClient(t, func(w http.ResponseWriter, r *http.Request, n int) { apiError(w, 500, "backendError") })
	if _, err := c.Get(context.Background(), "primary", "e1"); err == nil {
		t.Fatal("expected error")
	}
	if len(f.reqs) != 5 {
		t.Errorf("attempts = %d, want 5", len(f.reqs))
	}
}

const eventWithAttendees = `{
 "id":"e1","organizer":{"email":"organizer@example.com"},
 "attendees":[
  {"email":"organizer@example.com","organizer":true,"responseStatus":"accepted"},
  {"email":"user@example.com","self":true,"responseStatus":"needsAction","optional":true},
  {"email":"other@example.com","displayName":"Other","responseStatus":"tentative","comment":"maybe"}
 ]}`

func TestSetSelfResponseSendsFullAttendees(t *testing.T) {
	c, f, _ := newClient(t, func(w http.ResponseWriter, r *http.Request, n int) {
		_, _ = io.WriteString(w, eventWithAttendees)
	})
	if _, err := c.SetSelfResponse(context.Background(), "primary", "e1", "accepted", "none"); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 2 || f.reqs[0].method != http.MethodGet || f.reqs[1].method != http.MethodPatch {
		t.Fatalf("requests = %+v", f.reqs)
	}
	p := f.reqs[1]
	if p.path != "/calendars/primary/events/e1" || p.query["sendUpdates"][0] != "none" {
		t.Errorf("patch = %s %v", p.path, p.query)
	}
	var body map[string]any
	if err := json.Unmarshal(p.body, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"attendees": []any{
		map[string]any{"email": "organizer@example.com", "organizer": true, "responseStatus": "accepted"},
		map[string]any{"email": "user@example.com", "self": true, "responseStatus": "accepted", "optional": true},
		map[string]any{"email": "other@example.com", "displayName": "Other", "responseStatus": "tentative", "comment": "maybe"},
	}}
	if d := cmp.Diff(want, body); d != "" {
		t.Errorf("patch body (-want +got):\n%s", d)
	}
}

func TestSetSelfResponseNotAttendee(t *testing.T) {
	c, f, _ := newClient(t, func(w http.ResponseWriter, r *http.Request, n int) {
		_, _ = io.WriteString(w, `{"id":"e1","attendees":[{"email":"a@example.com"}]}`)
	})
	_, err := c.SetSelfResponse(context.Background(), "primary", "e1", "declined", "all")
	if !errors.Is(err, ErrNotAttendee) || len(f.reqs) != 1 {
		t.Fatalf("err = %v, requests = %d", err, len(f.reqs))
	}
}

func TestSetSelfResponseValidates(t *testing.T) {
	c, f, _ := newClient(t, func(w http.ResponseWriter, r *http.Request, n int) {})
	if _, err := c.SetSelfResponse(context.Background(), "primary", "e1", "yes", "all"); !errors.Is(err, ErrBadStatus) {
		t.Errorf("err = %v", err)
	}
	if _, err := c.SetSelfResponse(context.Background(), "primary", "e1", "accepted", "everyone"); err == nil {
		t.Error("bad sendUpdates accepted")
	}
	if len(f.reqs) != 0 {
		t.Error("requests sent for invalid input")
	}
}

func TestInstanceAt(t *testing.T) {
	c, f, _ := newClient(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			_, _ = io.WriteString(w, `{"items":[{"id":"m_20261120T000000Z"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	orig := time.Date(2026, 11, 20, 11, 0, 0, 0, time.FixedZone("AEDT", 11*3600))
	ev, err := c.InstanceAt(context.Background(), "primary", "m", orig)
	if err != nil || ev.Id != "m_20261120T000000Z" {
		t.Fatal(ev, err)
	}
	if f.reqs[0].path != "/calendars/primary/events/m/instances" || f.reqs[0].query["originalStart"][0] != "2026-11-20T00:00:00Z" {
		t.Errorf("request = %+v", f.reqs[0])
	}
	if _, err := c.InstanceAt(context.Background(), "primary", "m", orig); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty: err = %v", err)
	}
}

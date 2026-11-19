package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestHealthz(t *testing.T) {
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	s := New("v1", "/1/calendars/x/", 2*time.Minute, clock)
	h := s.Handler()

	if code, body := get(t, h, "/healthz"); code != 200 || !strings.Contains(body, "starting") {
		t.Fatalf("startup: %d %q", code, body)
	}
	now = now.Add(7 * time.Minute) // > 3 × interval, never succeeded
	s.Update(func(st *Status) { st.LastErrorClass = "google_auth" })
	if code, body := get(t, h, "/healthz"); code != 503 || !strings.Contains(body, "google_auth") {
		t.Fatalf("no success: %d %q", code, body)
	}
	s.Update(func(st *Status) { st.LastSuccess, st.LastErrorClass = now, "" })
	if code, _ := get(t, h, "/healthz"); code != 200 {
		t.Fatalf("after success: %d", code)
	}
	now = now.Add(5*time.Minute + 59*time.Second)
	if code, _ := get(t, h, "/healthz"); code != 200 {
		t.Fatalf("within 3 × interval: %d", code)
	}
	now = now.Add(2 * time.Second)
	if code, _ := get(t, h, "/healthz"); code != 503 {
		t.Fatalf("stale: %d", code)
	}
}

func TestStatusJSON(t *testing.T) {
	s := New("v1", "/1/calendars/x/", time.Minute, nil)
	s.Update(func(st *Status) {
		st.Mappings["synced"] = 3
		st.Parked = 1
		st.LastCycle.Created = 2
	})
	code, body := get(t, s.Handler(), "/status")
	if code != 200 {
		t.Fatal(code)
	}
	var st Status
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Mappings["synced"] != 3 || st.Parked != 1 || st.LastCycle.Created != 2 || st.Version != "v1" {
		t.Errorf("status %+v", st)
	}
	if code, _ := get(t, s.Handler(), "/nope"); code != 404 {
		t.Errorf("unknown path: %d", code)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/status", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /status: %d", rec.Code)
	}
}

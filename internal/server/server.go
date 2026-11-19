// Package server is calbridge's local HTTP status endpoint (PLAN.md §13):
// GET /healthz for liveness and GET /status for counts. It never exposes
// event content, addresses or tokens.
package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Status is what /status reports.
type Status struct {
	Version             string         `json:"version"`
	Calendar            string         `json:"calendar_href"`
	Interval            string         `json:"interval"`
	Started             time.Time      `json:"started"`
	LastAttempt         time.Time      `json:"last_attempt,omitzero"`
	LastSuccess         time.Time      `json:"last_success,omitzero"`
	LastErrorClass      string         `json:"last_error_class,omitempty"`
	ConsecutiveFailures int            `json:"consecutive_failures"`
	Mappings            map[string]int `json:"mappings"`
	Parked              int            `json:"parked"`
	PendingRSVPs        int            `json:"pending_rsvps"`
	LastCycle           CycleCounts    `json:"last_cycle"`
}

// CycleCounts summarises the last cycle.
type CycleCounts struct {
	Created     int `json:"created"`
	Updated     int `json:"updated"`
	Adopted     int `json:"adopted"`
	Deleted     int `json:"deleted"`
	Unchanged   int `json:"unchanged"`
	Skipped     int `json:"skipped"`
	UserDeleted int `json:"user_deleted"`
	Failed      int `json:"failed"`
	RSVPs       int `json:"rsvps_sent"`
}

// Server holds the latest status. Safe for concurrent use.
type Server struct {
	mu       sync.Mutex
	st       Status
	interval time.Duration
	now      func() time.Time
}

// New returns a Server for a sync loop running every interval.
func New(version, calendarHref string, interval time.Duration, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{interval: interval, now: now, st: Status{
		Version: version, Calendar: calendarHref, Interval: interval.String(), Started: now().UTC(), Mappings: map[string]int{},
	}}
}

// Update replaces the status with fn's changes.
func (s *Server) Update(fn func(st *Status)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
}

// Snapshot returns a copy of the status.
func (s *Server) Snapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Mappings = make(map[string]int, len(s.st.Mappings))
	for k, v := range s.st.Mappings {
		st.Mappings[k] = v
	}
	return st
}

// Healthy reports /healthz's answer: healthy when the last successful cycle
// is younger than 3 × interval, or, before any success, while the process is
// younger than that (startup grace).
func (s *Server) Healthy() (bool, string) {
	st := s.Snapshot()
	limit := 3 * s.interval
	now := s.now()
	switch {
	case !st.LastSuccess.IsZero() && now.Sub(st.LastSuccess) < limit:
		return true, "ok"
	case st.LastSuccess.IsZero() && now.Sub(st.Started) < limit:
		return true, "starting"
	case st.LastErrorClass != "":
		return false, "no successful sync recently (" + st.LastErrorClass + ")"
	}
	return false, "no successful sync recently"
}

// Handler serves /healthz and /status.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		ok, msg := s.Healthy()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = w.Write([]byte(msg + "\n"))
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(s.Snapshot())
	})
	return mux
}

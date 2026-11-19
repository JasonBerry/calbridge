package syncer

import (
	"context"
	"errors"
	"time"

	"github.com/JasonBerry/calbridge/internal/googlecal"
	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/store"
)

// Error classes (PLAN.md §13): logged, reported by /status, and stored in
// mappings.last_error. Never event content.
const (
	ClassGoogleAuth  = "google_auth"
	ClassICloudAuth  = "icloud_auth"
	ClassInterrupted = "interrupted"
	ClassTransient   = "transient"
)

// ErrorClass classifies a cycle or item error.
func ErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, googlecal.ErrAuth):
		return ClassGoogleAuth
	case errors.Is(err, icloud.ErrAuth):
		return ClassICloudAuth
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ClassInterrupted
	}
	return ClassTransient
}

// IsAuth reports whether err is an authorization failure. Auth failures abort
// the cycle (§8.7): every further call would fail the same way, and they are
// not the fault of any one event.
func IsAuth(err error) bool {
	c := ErrorClass(err)
	return c == ClassGoogleAuth || c == ClassICloudAuth
}

// itemGrace is how long an item already in progress may run after shutdown
// starts, so a started PUT finishes and is recorded (§9).
const itemGrace = 30 * time.Second

// item runs fn for one event unless shutdown has started. fn's context is
// not cancelled by shutdown itself, only itemGrace after it.
func (r *run) item(ctx context.Context, fn func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ictx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(itemGrace, cancel) })
	defer stop()
	return fn(ictx)
}

// backoff is the delay before retry n (1-based): 1, 2, 4 … minutes, at most an hour.
func backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	if n > 7 {
		return time.Hour
	}
	return min(time.Minute<<(n-1), time.Hour)
}

// recordFailure notes a failed item on its mapping row and reports whether
// the work is durably queued (a pending write or RSVP that retryPending will
// re-drive), in which case the sync token may still advance (§8.2).
func (r *run) recordFailure(ctx context.Context, googleID string, err error) bool {
	m, gerr := r.mapping(ctx, googleID)
	if gerr != nil || m == nil {
		return false
	}
	m.Failures++
	m.NextAttemptAt = r.now.Add(backoff(m.Failures))
	m.LastError = ErrorClass(err)
	if m.Failures == store.MaxFailures {
		r.log.WarnContext(ctx, "parking event after repeated failures; a new change to it retries",
			"google_id", googleID, "failures", m.Failures, "class", m.LastError)
	}
	if uerr := r.q.UpdateMapping(ctx, m); uerr != nil {
		return false
	}
	return m.State == store.StatePendingPut || m.State == store.StatePendingDelete || m.RSVPPending != ""
}

// clearFailures resets retry state after a success (the caller saves m).
func clearFailures(m *store.Mapping) {
	m.Failures, m.NextAttemptAt, m.LastError = 0, time.Time{}, ""
}

// due reports whether retryPending should touch m now.
func (r *run) due(m *store.Mapping) bool {
	return !m.Parked() && !m.NextAttemptAt.After(r.now)
}

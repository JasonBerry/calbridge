package syncer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	calendar "google.golang.org/api/calendar/v3"

	"github.com/JasonBerry/calbridge/internal/fakes/caldavfake"
	"github.com/JasonBerry/calbridge/internal/googlecal"
	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/store"
)

// failingPuts makes every PUT fail with err while on is true.
type failingPuts struct {
	*caldavfake.Fake
	err   error
	on    bool
	calls int
}

func (f *failingPuts) Put(ctx context.Context, href string, cal *ical.Calendar, cond icloud.PutCond) (string, error) {
	f.calls++
	if f.on {
		return "", f.err
	}
	return f.Fake.Put(ctx, href, cal, cond)
}

func TestBackoffAndParking(t *testing.T) {
	h := newHarness(t)
	fp := &failingPuts{Fake: h.c, err: errors.New("icloud 503"), on: true}
	h.e.C = fp
	h.g.Put(invite("a"))
	if _, err := h.e.Cycle(context.Background()); err == nil {
		t.Fatal("failure not reported")
	}
	if m := h.mapping("a"); m.Failures != 1 || m.LastError != ClassTransient || !m.NextAttemptAt.Equal(h.clock.t.Add(time.Minute)) {
		t.Fatalf("after first failure: %+v", m)
	}
	// Not retried before its backoff.
	calls := fp.calls
	_, _ = h.e.Cycle(context.Background())
	if fp.calls != calls {
		t.Fatal("retried before backoff elapsed")
	}
	// Retried with growing backoff until parked.
	for i := 2; i <= store.MaxFailures; i++ {
		h.clock.t = h.mapping("a").NextAttemptAt
		_, _ = h.e.Cycle(context.Background())
		m := h.mapping("a")
		if m.Failures != i {
			t.Fatalf("attempt %d: failures %d", i, m.Failures)
		}
		if want := backoff(i); !m.NextAttemptAt.Equal(h.clock.t.Add(want)) {
			t.Fatalf("attempt %d: next %v; want +%v", i, m.NextAttemptAt.Sub(h.clock.t), want)
		}
	}
	if !h.mapping("a").Parked() {
		t.Fatal("not parked")
	}
	calls = fp.calls
	h.clock.t = h.clock.t.Add(24 * time.Hour)
	_, _ = h.e.Cycle(context.Background())
	if fp.calls != calls {
		t.Fatal("parked row retried")
	}
	// A new change to the event retries it; success clears the counters.
	fp.on = false
	h.g.Update("a", func(ev *calendar.Event) { ev.Summary = "Renamed" })
	h.cycleTwice()
	if m := h.mapping("a"); m.State != store.StateSynced || m.Failures != 0 || m.LastError != "" || !m.NextAttemptAt.IsZero() {
		t.Fatalf("after success: %+v", m)
	}
}

func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour, time.Hour}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v; want %v", i+1, got, w)
		}
	}
}

func TestICloudAuthErrorAbortsCycle(t *testing.T) {
	h := newHarness(t)
	fp := &failingPuts{Fake: h.c, err: fmt.Errorf("PUT: %w", icloud.ErrAuth), on: true}
	h.e.C = fp
	h.g.Put(invite("a"))
	h.g.Put(invite("b"))
	rep, err := h.e.Cycle(context.Background())
	if !IsAuth(err) || rep.ErrorClass != ClassICloudAuth {
		t.Fatalf("err %v class %q", err, rep.ErrorClass)
	}
	if fp.calls != 1 {
		t.Fatalf("%d PUTs after an auth failure; want the cycle to stop at 1", fp.calls)
	}
	if m := h.mapping("a"); m.Failures != 0 {
		t.Errorf("auth failure charged to the event: %+v", m)
	}
	if st, err := h.s.GetSyncState(context.Background(), "google:primary"); err == nil && st.Token != "" {
		t.Error("google token advanced after an auth failure")
	}
}

type failingList struct {
	googlecal.GoogleCalendar
	err error
}

func (f failingList) List(context.Context, googlecal.ListRequest) (googlecal.ListPage, error) {
	return googlecal.ListPage{}, f.err
}

func TestGoogleAuthErrorClass(t *testing.T) {
	h := newHarness(t)
	h.e.G = failingList{GoogleCalendar: h.g, err: fmt.Errorf("list: %w", googlecal.ErrAuth)}
	rep, err := h.e.Cycle(context.Background())
	if !IsAuth(err) || rep.ErrorClass != ClassGoogleAuth {
		t.Fatalf("err %v class %q", err, rep.ErrorClass)
	}
	if h.c.Writes() != 0 {
		t.Error("wrote to iCloud after a Google auth failure")
	}
}

// cancellingPuts cancels the cycle (as SIGTERM would) while the first PUT
// is in flight.
type cancellingPuts struct {
	*caldavfake.Fake
	cancel context.CancelFunc
	calls  int
}

func (c *cancellingPuts) Put(ctx context.Context, href string, cal *ical.Calendar, cond icloud.PutCond) (string, error) {
	c.calls++
	c.cancel()
	time.Sleep(20 * time.Millisecond) // the request is still in flight
	if err := ctx.Err(); err != nil {
		return "", err // the item context must not be cancelled by shutdown
	}
	return c.Fake.Put(ctx, href, cal, cond)
}

func TestShutdownFinishesCurrentItem(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.g.Put(invite("b"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp := &cancellingPuts{Fake: h.c, cancel: cancel}
	h.e.C = cp
	_, err := h.e.Cycle(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
	if cp.calls != 1 {
		t.Fatalf("%d PUTs; the cycle should stop after the item in flight", cp.calls)
	}
	// The started PUT finished and was recorded.
	first, second := "a", "b"
	if _, err := h.s.GetMappingByGoogleID(context.Background(), "primary", first); err != nil {
		first, second = second, first
	}
	if m := h.mapping(first); m.State != store.StateSynced || h.c.Object(m.ICloudHref) == nil {
		t.Fatalf("in-flight item not completed: %+v", m)
	}
	if _, err := h.s.GetMappingByGoogleID(context.Background(), "primary", second); !store.IsNotFound(err) {
		t.Fatalf("second item processed after shutdown: %v", err)
	}
	if st, err := h.s.GetSyncState(context.Background(), "google:primary"); err == nil && st.Token != "" {
		t.Error("token advanced by an interrupted cycle")
	}
	// The next run picks up where it stopped, without duplicates.
	h.e.C = h.c
	h.cycleTwice()
	if len(h.c.Hrefs()) != 2 {
		t.Fatalf("hrefs %v", h.c.Hrefs())
	}
}

func TestErrorClass(t *testing.T) {
	for err, want := range map[error]string{
		nil:                                    "",
		fmt.Errorf("x: %w", googlecal.ErrAuth): ClassGoogleAuth,
		fmt.Errorf("x: %w", icloud.ErrAuth):    ClassICloudAuth,
		context.Canceled:                       ClassInterrupted,
		errors.New("boom"):                     ClassTransient,
	} {
		if got := ErrorClass(err); got != want {
			t.Errorf("ErrorClass(%v) = %q; want %q", err, got, want)
		}
	}
}

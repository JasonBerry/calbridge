// Package googlefake is an in-memory googlecal.GoogleCalendar for tests. It
// models what Phase 0 observed: incremental sync with sync tokens, bare
// cancelled tombstones (only id/status/etag/kind, P0-1), a forced 410, and
// pagination.
package googlefake

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	calendar "google.golang.org/api/calendar/v3"

	"github.com/JasonBerry/calbridge/internal/googlecal"
)

// Fake is safe for concurrent use.
type Fake struct {
	mu       sync.Mutex
	events   map[string]*calendar.Event // by id
	order    []string                   // insertion order, for stable listing
	changes  []change
	seq      int64
	gone     bool
	now      time.Time
	PageSize int // items per List page (default 2, to exercise paging)

	// Call counters.
	ListCalls, GetCalls, PatchCalls int
	// Patches records SetSelfResponse calls.
	Patches []Patch
}

// Patch is one recorded SetSelfResponse call.
type Patch struct {
	EventID, Status, SendUpdates string
}

type change struct {
	seq int64
	id  string
}

var _ googlecal.GoogleCalendar = (*Fake)(nil)

// New returns an empty fake.
func New() *Fake {
	return &Fake{events: map[string]*calendar.Event{}, PageSize: 2, now: time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)}
}

func clone(ev *calendar.Event) *calendar.Event {
	b, err := json.Marshal(ev)
	if err != nil {
		panic(err)
	}
	var out calendar.Event
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return &out
}

func (f *Fake) touch(ev *calendar.Event) {
	f.seq++
	f.now = f.now.Add(time.Second)
	ev.Etag = fmt.Sprintf(`"%d"`, f.seq)
	ev.Updated = f.now.Format(time.RFC3339Nano)
	if ev.Created == "" {
		ev.Created = ev.Updated
	}
	f.changes = append(f.changes, change{seq: f.seq, id: ev.Id})
}

// Put inserts or replaces an event (as the organizer or Google would) and
// records a change. Missing Kind/ICalUID/Status get defaults.
func (f *Fake) Put(ev *calendar.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := clone(ev)
	if c.Kind == "" {
		c.Kind = "calendar#event"
	}
	if c.Status == "" {
		c.Status = "confirmed"
	}
	if c.ICalUID == "" {
		c.ICalUID = c.Id + "@google.com"
	}
	if old, ok := f.events[c.Id]; ok && c.Created == "" {
		c.Created = old.Created
	} else if !ok {
		f.order = append(f.order, c.Id)
	}
	f.events[c.Id] = c
	f.touch(c)
}

// Update applies fn to a copy of the stored event and stores it.
func (f *Fake) Update(id string, fn func(ev *calendar.Event)) {
	f.mu.Lock()
	ev, ok := f.events[id]
	f.mu.Unlock()
	if !ok {
		panic("googlefake: no event " + id)
	}
	c := clone(ev)
	fn(c)
	f.Put(c)
}

// Cancel marks an event cancelled (organizer deleted it or removed the user).
func (f *Fake) Cancel(id string) {
	f.Update(id, func(ev *calendar.Event) { ev.Status = "cancelled" })
}

// Event returns a copy of the stored event, or nil.
func (f *Fake) Event(id string) *calendar.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ev, ok := f.events[id]; ok {
		return clone(ev)
	}
	return nil
}

// Events returns copies of all stored events (including cancelled), in
// insertion order.
func (f *Fake) Events() []*calendar.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*calendar.Event, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, clone(f.events[id]))
	}
	return out
}

// ExpireSyncTokens makes every outstanding sync token return 410 once.
func (f *Fake) ExpireSyncTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gone = true
}

func tombstone(ev *calendar.Event) *calendar.Event {
	return &calendar.Event{Id: ev.Id, Status: "cancelled", Etag: ev.Etag, Kind: "calendar#event"}
}

func eventEnd(ev *calendar.Event) time.Time {
	if ev.End == nil {
		return time.Time{}
	}
	if ev.End.Date != "" {
		t, _ := time.Parse("2006-01-02", ev.End.Date)
		return t
	}
	t, _ := time.Parse(time.RFC3339, ev.End.DateTime)
	return t
}

// List implements GoogleCalendar.
func (f *Fake) List(_ context.Context, req googlecal.ListRequest) (googlecal.ListPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ListCalls++
	var items []*calendar.Event
	if req.SyncToken != "" {
		since, err := strconv.ParseInt(strings.TrimPrefix(req.SyncToken, "sync-"), 10, 64)
		if f.gone || err != nil || since > f.seq {
			f.gone = false
			return googlecal.ListPage{}, googlecal.ErrSyncTokenGone
		}
		seen := map[string]bool{}
		var ids []string
		for _, c := range f.changes {
			if c.seq > since && !seen[c.id] {
				seen[c.id] = true
				ids = append(ids, c.id)
			}
		}
		for _, id := range ids {
			ev := f.events[id]
			if ev.Status == "cancelled" {
				items = append(items, tombstone(ev))
			} else {
				items = append(items, clone(ev))
			}
		}
	} else {
		for _, id := range f.order {
			ev := f.events[id]
			if ev.Status == "cancelled" {
				continue // showDeleted=false on a full sync
			}
			if !req.TimeMin.IsZero() && eventEnd(ev).Before(req.TimeMin) {
				continue
			}
			items = append(items, clone(ev))
		}
	}
	off := 0
	if req.PageToken != "" {
		off, _ = strconv.Atoi(strings.TrimPrefix(req.PageToken, "page-"))
	}
	size := f.PageSize
	if size <= 0 {
		size = len(items) + 1
	}
	end := min(off+size, len(items))
	page := googlecal.ListPage{Items: items[min(off, len(items)):end]}
	if end < len(items) {
		page.NextPageToken = "page-" + strconv.Itoa(end)
	} else {
		page.NextSyncToken = "sync-" + strconv.FormatInt(f.seq, 10)
	}
	return page, nil
}

// Get implements GoogleCalendar.
func (f *Fake) Get(_ context.Context, _ string, eventID string) (*calendar.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.GetCalls++
	ev, ok := f.events[eventID]
	if !ok {
		return nil, googlecal.ErrNotFound
	}
	return clone(ev), nil
}

// InstanceAt implements GoogleCalendar (recurring support arrives in M10).
func (f *Fake) InstanceAt(context.Context, string, string, time.Time) (*calendar.Event, error) {
	return nil, googlecal.ErrNotFound
}

// SetSelfResponse implements GoogleCalendar: it changes the self attendee's
// responseStatus without touching sequence (P0-2/P0-7).
func (f *Fake) SetSelfResponse(_ context.Context, _ string, eventID, status, sendUpdates string) (*calendar.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PatchCalls++
	ev, ok := f.events[eventID]
	if !ok {
		return nil, googlecal.ErrNotFound
	}
	if !googlecal.ValidResponseStatus(status) {
		return nil, googlecal.ErrBadStatus
	}
	self := googlecal.SelfAttendee(ev)
	if self == nil {
		return nil, googlecal.ErrNotAttendee
	}
	self.ResponseStatus = status
	f.Patches = append(f.Patches, Patch{EventID: eventID, Status: status, SendUpdates: sendUpdates})
	f.touch(ev)
	return clone(ev), nil
}

// SortedIDs returns the ids of non-cancelled events (test helper).
func (f *Fake) SortedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id, ev := range f.events {
		if ev.Status != "cancelled" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

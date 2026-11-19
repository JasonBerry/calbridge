package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	calendar "google.golang.org/api/calendar/v3"

	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/store"
)

// deleteMirror is the only code path that deletes an iCloud object
// (CLAUDE.md rule 5). It records the intent (pending_delete) first, deletes
// with If-Match on the stored ETag and the configured delete mode, treats
// 404 as success, and on 412 re-GETs and deletes anyway: Google is the
// source of truth for existence (§8.5).
func (r *run) deleteMirror(ctx context.Context, m *store.Mapping) error {
	if m.State != store.StatePendingDelete {
		m.State = store.StatePendingDelete
		if err := r.q.UpdateMapping(ctx, m); err != nil {
			return err
		}
	}
	opts := icloud.DeleteOpts{IfMatch: m.ICloudETag}
	if r.opts().DeleteMode == DeleteModeScheduleReplyF {
		f := false
		opts.ScheduleReply = &f
	}
	err := r.c.Delete(ctx, m.ICloudHref, opts)
	if errors.Is(err, icloud.ErrPreconditionFailed) {
		// Apple changed it (e.g. the user RSVP'd) since we last saw it.
		obj, gerr := r.c.Get(ctx, m.ICloudHref)
		switch {
		case errors.Is(gerr, icloud.ErrNotFound):
			err = gerr
		case gerr != nil:
			return fmt.Errorf("delete re-get %s: %w", m.ICloudHref, gerr)
		default:
			opts.IfMatch = obj.ETag
			err = r.c.Delete(ctx, m.ICloudHref, opts)
		}
	}
	if err != nil && !errors.Is(err, icloud.ErrNotFound) {
		return fmt.Errorf("delete %s: %w", m.ICloudHref, err)
	}
	r.rep.Deleted++
	r.logOp(ctx, "delete", m)
	m.State, m.DeletedAt = store.StateDeleted, r.now
	clearFailures(m)
	return r.q.UpdateMapping(ctx, m)
}

// reconcileMissing is §8.3 step 3, after a full listing: a live mirror whose
// Google event was not listed is gone from Google. Recent ones are deleted;
// ones that ended before the lookback window are only marked deleted (old
// mirrors are left alone in iCloud). More deletions than
// MaxFullResyncDeletes aborts the whole step (no bulk deletes, rule 5): an
// empty or truncated listing must never wipe the calendar.
func (r *run) reconcileMissing(ctx context.Context, seen map[string]bool) error {
	ms, err := r.q.ListMappings(ctx, store.StateSynced, store.StatePendingPut)
	if err != nil {
		return err
	}
	cutoff := r.now.Add(-r.opts().Lookback)
	var del, old []store.Mapping
	for _, m := range ms {
		if m.GoogleCalendarID != r.opts().GoogleCalendarID || seen[m.GoogleEventID] {
			continue
		}
		end, known := r.cachedEnd(ctx, m.GoogleEventID)
		if known && end.Before(cutoff) {
			old = append(old, m)
		} else {
			del = append(del, m)
		}
	}
	limit := r.opts().MaxFullResyncDeletes
	if limit <= 0 {
		limit = 5
	}
	if len(del) > limit {
		r.log.ErrorContext(ctx, "full resync wants to delete too many mirrors; refusing", "count", len(del), "limit", limit)
		return fmt.Errorf("%w (%d > %d)", ErrTooManyDeletes, len(del), limit)
	}
	var errs []error
	for i := range del {
		if err := r.item(ctx, func(ictx context.Context) error { return r.deleteMirror(ictx, &del[i]) }); err != nil {
			errs = append(errs, err)
		}
	}
	for i := range old {
		m := &old[i]
		m.State = store.StatePendingDelete
		if err := r.q.UpdateMapping(ctx, m); err != nil {
			errs = append(errs, err)
			continue
		}
		m.State, m.DeletedAt = store.StateDeleted, r.now
		if err := r.q.UpdateMapping(ctx, m); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// cachedEnd returns the end of a cached Google event.
func (r *run) cachedEnd(ctx context.Context, eventID string) (time.Time, bool) {
	ge, err := r.q.GetGoogleEvent(ctx, r.opts().GoogleCalendarID, eventID)
	if err != nil {
		return time.Time{}, false
	}
	var ev calendar.Event
	if json.Unmarshal(ge.JSON, &ev) != nil || ev.End == nil {
		return time.Time{}, false
	}
	if ev.End.Date != "" {
		t, err := time.Parse("2006-01-02", ev.End.Date)
		return t, err == nil
	}
	t, err := time.Parse(time.RFC3339, ev.End.DateTime)
	return t, err == nil
}

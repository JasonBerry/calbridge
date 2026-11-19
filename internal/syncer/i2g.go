package syncer

import (
	"context"
	"errors"
	"fmt"

	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/store"
)

func icloudKey(calHref string) string { return "icloud:" + calHref }

// iCloudToGoogle is §8.4: find mirrors Apple changed (sync-collection,
// skipping ETags we wrote ourselves), read the user's response from each and
// send changes to Google; mirrors that disappeared without us deleting them
// were deleted by the user (§8.5).
func (r *run) iCloudToGoogle(ctx context.Context) error {
	calHref := r.opts().CalendarHref
	st, err := r.q.GetSyncState(ctx, icloudKey(calHref))
	if err != nil && !store.IsNotFound(err) {
		return err
	}
	res, err := r.c.SyncCollection(ctx, calHref, st.Token)
	full := st.Token == ""
	if errors.Is(err, icloud.ErrInvalidSyncToken) {
		r.log.WarnContext(ctx, "iCloud sync token invalid; full listing", "calendar", calHref)
		res, err = r.c.SyncCollection(ctx, calHref, "")
		full = true
	}
	if err != nil {
		return fmt.Errorf("icloud sync: %w", err)
	}

	present := map[string]bool{}
	var hrefs []string
	byHref := map[string]*store.Mapping{}
	for _, mem := range res.Changed {
		present[mem.Href] = true
		m, err := r.q.GetMappingByHref(ctx, calHref, mem.Href)
		if store.IsNotFound(err) {
			continue // not ours
		}
		if err != nil {
			return err
		}
		// Only live mirrors, and only ETags we did not write (§7.10).
		if m.State != store.StateSynced || icloud.NormalizeETag(mem.ETag) == icloud.NormalizeETag(m.ICloudETag) {
			continue
		}
		mm := m
		byHref[mem.Href] = &mm
		hrefs = append(hrefs, mem.Href)
	}

	var errs []error
	if len(hrefs) > 0 {
		objs, err := r.c.MultiGet(ctx, calHref, hrefs)
		if err != nil {
			return fmt.Errorf("icloud multiget: %w", err)
		}
		for i := range objs {
			if err := r.handleICloudChange(ctx, byHref[objs[i].Href], &objs[i]); err != nil {
				errs = append(errs, err)
			}
		}
	}

	removed := res.Removed
	if full {
		// A full listing reports no removals: any live mirror not listed is gone.
		ms, err := r.q.ListMappings(ctx, store.StateSynced)
		if err != nil {
			return err
		}
		for _, m := range ms {
			if m.ICloudCalendar == calHref && !present[m.ICloudHref] {
				removed = append(removed, m.ICloudHref)
			}
		}
	}
	for _, href := range removed {
		m, err := r.q.GetMappingByHref(ctx, calHref, href)
		if store.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		// Rows we deleted are already deleted/pending_delete; only a live
		// mirror vanishing means the user deleted it.
		if m.State == store.StateSynced {
			if err := r.markUserDeleted(ctx, &m); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	st.Key, st.Token, st.LastSuccess = icloudKey(calHref), res.NewToken, r.now
	if full {
		st.LastFullSync = r.now
	}
	return r.q.PutSyncState(ctx, &st)
}

// handleICloudChange is §8.4 handleRSVP for the master. Apple-side edits
// other than the response (title, time, alarms) are not propagated: Google is
// the source of truth and the next Google change overwrites them (§8.5).
func (r *run) handleICloudChange(ctx context.Context, m *store.Mapping, obj *icloud.Object) error {
	if m == nil {
		return nil
	}
	m.ICloudETag = obj.ETag
	ps, i := r.appleStatus(obj)
	if ps == "" {
		r.log.DebugContext(ctx, "mirror changed but has no self attendee", "google_id", m.GoogleEventID)
		return r.q.UpdateMapping(ctx, m)
	}
	m.ICloudPartStat = string(ps)
	d := decideRSVP(m.AgreedStatus, m.GoogleStatus, i, !r.opts().NoGoogleRSVPToICloud)
	if d.push == "" {
		return r.q.UpdateMapping(ctx, m)
	}
	if d.conflict {
		r.log.InfoContext(ctx, "RSVP changed on both sides; keeping Apple's", "google_id", m.GoogleEventID)
	}
	return r.pushRSVP(ctx, m, d.push)
}

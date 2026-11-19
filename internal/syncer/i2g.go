package syncer

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/store"
	"github.com/JasonBerry/calbridge/internal/translate"
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
		if m.State != store.StateSynced || icloud.NormalizeETag(mem.ETag) == icloud.NormalizeETag(m.ICloudETag) || r.dryWritten(mem.Href) {
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
			m := byHref[objs[i].Href]
			err := r.item(ctx, func(ictx context.Context) error { return r.handleICloudChange(ictx, m, &objs[i]) })
			switch {
			case err == nil:
			case ctx.Err() != nil:
				return ctx.Err()
			case IsAuth(err):
				return err
			case m != nil && r.recordFailure(ctx, m.GoogleEventID, err):
				r.rep.Failed++
				r.soft = append(r.soft, fmt.Errorf("rsvp %s (queued for retry): %w", m.GoogleEventID, err))
			default:
				r.rep.Failed++
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
		if m.State == store.StateSynced && !r.dryWritten(href) {
			if err := r.markUserDeleted(ctx, &m); err != nil {
				errs = append(errs, err)
			}
		}
	}
	// Bring series our pushes changed in Google up to date now.
	touchedIDs := make([]string, 0, len(r.touched))
	for sid := range r.touched {
		touchedIDs = append(touchedIDs, sid)
	}
	sort.Strings(touchedIDs)
	for _, sid := range touchedIDs {
		delete(r.touched, sid)
		if err := r.item(ctx, func(ictx context.Context) error { return r.processSeries(ictx, sid, 0) }); err != nil {
			if IsAuth(err) || ctx.Err() != nil {
				return errors.Join(err, ctx.Err())
			}
			r.rep.Failed++
			if r.recordFailure(ctx, sid, err) {
				r.soft = append(r.soft, err)
			} else {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	// The token from before our own PUTs above is kept: those changes come
	// back next cycle with ETags we stored, and are skipped.
	st.Key, st.Token, st.LastSuccess = icloudKey(calHref), res.NewToken, r.now
	if full {
		st.LastFullSync = r.now
	}
	return r.q.PutSyncState(ctx, &st)
}

// handleICloudChange is §8.4 handleRSVP for the series and for each
// occurrence Apple shows separately (§7.8 rule 9: a change to the master is
// "accept all", an override is one occurrence). Apple-side edits other than
// the response (title, time, alarms) are not propagated: Google is the source
// of truth and the next Google change overwrites them (§8.5).
func (r *run) handleICloudChange(ctx context.Context, m *store.Mapping, obj *icloud.Object) error {
	if m == nil {
		return nil
	}
	m.ICloudETag = obj.ETag
	if obj.Cal == nil {
		return r.q.UpdateMapping(ctx, m)
	}
	ps, overrides, err := translate.ExtractSelfStatus(obj.Cal, r.opts().SelfEmail)
	if err != nil {
		r.log.DebugContext(ctx, "mirror changed but has no self attendee", "google_id", m.GoogleEventID)
		return r.q.UpdateMapping(ctx, m)
	}
	m.ICloudPartStat = string(ps)
	push := !r.opts().NoGoogleRSVPToICloud
	i, _ := translate.PartStatToGoogle(ps)
	var errs []error
	if d := decideRSVP(m.AgreedStatus, m.GoogleStatus, i, push); d.push != "" {
		if d.conflict {
			r.log.InfoContext(ctx, "RSVP changed on both sides; keeping Apple's", "google_id", m.GoogleEventID)
		}
		if err := r.pushRSVP(ctx, m, d.push); err != nil {
			errs = append(errs, err)
		}
	} else if err := r.q.UpdateMapping(ctx, m); err != nil {
		return err
	}
	if len(overrides) == 0 {
		return errors.Join(errs...)
	}

	rows, err := r.q.ListInstanceStates(ctx, m.ID)
	if err != nil {
		return err
	}
	byKey := map[string]store.InstanceState{}
	for _, row := range rows {
		byKey[row.RecurrenceID] = row
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		row, ok := byKey[k]
		if !ok {
			row = store.InstanceState{MappingID: m.ID, RecurrenceID: k}
		}
		gk, agreedK := row.GoogleStatus, row.AgreedStatus
		if gk == "" {
			gk = m.GoogleStatus // never answered separately: inherits the series
		}
		if agreedK == "" {
			agreedK = m.AgreedStatus
		}
		ik, _ := translate.PartStatToGoogle(overrides[k])
		d := decideRSVP(agreedK, gk, ik, push)
		if d.push != "" {
			if _, err := r.pushInstance(ctx, m, k, row.GoogleEventID, d.push); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		row.ICloudPartStat = string(overrides[k])
		if row.GoogleStatus == "" {
			row.GoogleStatus, row.AgreedStatus = gk, agreedK
		}
		if err := r.q.PutInstanceState(ctx, &row); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

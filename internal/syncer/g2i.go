package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	calendar "google.golang.org/api/calendar/v3"

	"github.com/JasonBerry/calbridge/internal/googlecal"
	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/store"
	"github.com/JasonBerry/calbridge/internal/translate"
)

// Skip reasons stored in mappings.skip_reason.
const (
	SkipOrganizerSelf = "organizer_self"
	SkipHrefCollision = "href_collision"
	skipEventTypePfx  = "event_type:"
	skipRenderPfx     = "render_error:"
)

func googleKey(calID string) string { return "google:" + calID }

// googleToICloud is §8.2: fetch changes, cache them, process each series,
// and advance the sync token only when every series succeeded (a failure
// re-fetches the same changes next cycle; processing is idempotent).
func (r *run) googleToICloud(ctx context.Context) error {
	cal := r.opts().GoogleCalendarID
	st, err := r.q.GetSyncState(ctx, googleKey(cal))
	if err != nil && !store.IsNotFound(err) {
		return err
	}
	full := st.Token == ""
	var (
		items []*calendar.Event
		token string
	)
	if !full {
		items, token, err = googlecal.ListAll(ctx, r.g, googlecal.ListRequest{CalendarID: cal, SyncToken: st.Token})
		if errors.Is(err, googlecal.ErrSyncTokenGone) {
			r.log.WarnContext(ctx, "google sync token gone; full resync", "calendar", cal)
			full = true
		} else if err != nil {
			return fmt.Errorf("google list: %w", err)
		}
	}
	if full {
		r.rep.FullResync = true
		items, token, err = googlecal.ListAll(ctx, r.g, googlecal.ListRequest{CalendarID: cal, TimeMin: r.now.Add(-r.opts().Lookback)})
		if err != nil {
			return fmt.Errorf("google full list: %w", err)
		}
	}

	var series []string
	seen := map[string]bool{}
	for _, it := range items {
		if err := r.cacheEvent(ctx, it); err != nil {
			return err
		}
		sid := seriesID(it)
		if !seen[sid] {
			seen[sid] = true
			series = append(series, sid)
		}
	}
	var errs []error
	for _, sid := range series {
		err := r.item(ctx, func(ictx context.Context) error { return r.processSeries(ictx, sid, 0) })
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		if IsAuth(err) {
			return err
		}
		r.rep.Failed++
		r.log.WarnContext(ctx, "series failed", "google_id", sid, "class", ErrorClass(err), "err", err)
		if r.recordFailure(ctx, sid, err) {
			r.soft = append(r.soft, fmt.Errorf("series %s (queued for retry): %w", sid, err))
		} else {
			errs = append(errs, fmt.Errorf("series %s: %w", sid, err))
		}
	}
	if full && ctx.Err() == nil {
		if err := r.reconcileMissing(ctx, seen); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	st.Key, st.Token, st.LastSuccess = googleKey(cal), token, r.now
	if full {
		st.LastFullSync = r.now
	}
	return r.q.PutSyncState(ctx, &st)
}

// seriesID groups Google items by series (§7.8 rule 1). A bare cancelled
// tombstone carries no recurringEventId (P0-1), so an instance-shaped id
// (<master>_<date>) is attributed to its master.
func seriesID(ev *calendar.Event) string {
	if ev.RecurringEventId != "" {
		return ev.RecurringEventId
	}
	if ev.Status == "cancelled" {
		if i := strings.LastIndex(ev.Id, "_"); i > 0 && translate.InstanceSuffix(ev.Id[i+1:]) {
			return ev.Id[:i]
		}
	}
	return ev.Id
}

func (r *run) cacheEvent(ctx context.Context, ev *calendar.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	ge := &store.GoogleEvent{
		CalendarID: r.opts().GoogleCalendarID, EventID: ev.Id, SeriesID: seriesID(ev),
		ICalUID: ev.ICalUID, Status: ev.Status, ETag: ev.Etag, JSON: b,
	}
	if ev.OriginalStartTime != nil {
		ge.OriginalStart = ev.OriginalStartTime.DateTime + ev.OriginalStartTime.Date
	}
	if t, err := time.Parse(time.RFC3339Nano, ev.Updated); err == nil {
		ge.Updated = t
	}
	if ge.Status == "" {
		ge.Status = "confirmed"
	}
	return r.q.PutGoogleEvent(ctx, ge)
}

// retryPending re-drives write-ahead intents left by a failed or crashed
// cycle (§8.6): pending PUTs and DELETEs, and RSVPs not yet sent to Google.
func (r *run) retryPending(ctx context.Context) error {
	ms, err := r.q.ListMappings(ctx, store.StatePendingPut, store.StatePendingDelete, store.StateSynced)
	if err != nil {
		return err
	}
	for i := range ms {
		m := ms[i]
		if m.GoogleCalendarID != r.opts().GoogleCalendarID || !r.due(&m) {
			continue
		}
		err := r.item(ctx, func(ictx context.Context) error {
			switch {
			case m.State == store.StatePendingDelete:
				return r.deleteMirror(ictx, &m)
			case m.State == store.StatePendingPut:
				return r.processSeries(ictx, m.GoogleEventID, 0)
			case m.RSVPPending != "":
				return r.pushRSVP(ictx, &m, m.RSVPPending)
			}
			return r.retryInstanceRSVPs(ictx, &m)
		})
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if IsAuth(err) {
			return err
		}
		r.rep.Failed++
		r.recordFailure(ctx, m.GoogleEventID, err)
		r.soft = append(r.soft, fmt.Errorf("retry %s: %w", m.GoogleEventID, err))
	}
	return nil
}

// loadSeries returns the cached master (nil if unknown) and exceptions.
func (r *run) loadSeries(ctx context.Context, sid string) (*calendar.Event, []*calendar.Event, error) {
	items, err := r.q.ListSeries(ctx, r.opts().GoogleCalendarID, sid)
	if err != nil {
		return nil, nil, err
	}
	var (
		master     *calendar.Event
		exceptions []*calendar.Event
	)
	for _, it := range items {
		var ev calendar.Event
		if err := json.Unmarshal(it.JSON, &ev); err != nil {
			return nil, nil, fmt.Errorf("cached event %s: %w", it.EventID, err)
		}
		if it.EventID == sid {
			master = &ev
		} else {
			exceptions = append(exceptions, &ev)
		}
	}
	return master, exceptions, nil
}

func (r *run) skipReason(ev *calendar.Event) string {
	switch {
	case ev.Organizer != nil && ev.Organizer.Self && !r.opts().IncludeOrganizedByMe:
		return SkipOrganizerSelf
	case ev.EventType != "" && ev.EventType != "default":
		return skipEventTypePfx + ev.EventType
	}
	return ""
}

func (r *run) mapping(ctx context.Context, sid string) (*store.Mapping, error) {
	m, err := r.q.GetMappingByGoogleID(ctx, r.opts().GoogleCalendarID, sid)
	if store.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// processSeries is §8.2 processSeries for one Google series id. attempt
// counts 412 retries (one re-GET-and-merge per cycle).
func (r *run) processSeries(ctx context.Context, sid string, attempt int) error {
	cal := r.opts().GoogleCalendarID
	master, exceptions, err := r.loadSeries(ctx, sid)
	if err != nil {
		return err
	}
	m, err := r.mapping(ctx, sid)
	if err != nil {
		return err
	}
	if master == nil {
		if m == nil && allCancelled(exceptions) {
			return nil // e.g. tombstones of a series never mirrored
		}
		// §7.8 rule 8: an exception arrived without its master (paging
		// order, or the master was never cached): fetch it once.
		ev, err := r.g.Get(ctx, cal, sid)
		switch {
		case errors.Is(err, googlecal.ErrNotFound):
			ev = &calendar.Event{Id: sid, Status: "cancelled"}
		case err != nil:
			return err
		default:
			if err := r.cacheEvent(ctx, ev); err != nil {
				return err
			}
		}
		master = ev
	}

	if master.Status == "cancelled" || translate.SelfAttendee(master) == nil {
		return r.goneFromGoogle(ctx, m)
	}
	if reason := r.skipReason(master); reason != "" {
		return r.markSkipped(ctx, m, master, reason)
	}
	g := translate.SelfAttendee(master).ResponseStatus
	if r.opts().DeclinedDelete && g == "declined" && len(master.Recurrence) == 0 &&
		(m == nil || (m.State == store.StateDeleted && m.SkipReason == SkipDeclined)) {
		return nil // hidden while declined (sync.declined: delete)
	}
	if m != nil && m.State == store.StateUserDeleted {
		// §8.5: never recreated, unless the organizer reschedules and the
		// user opted in.
		if !r.opts().RecreateOnReschedule || master.Sequence <= m.Sequence {
			return nil
		}
		r.log.InfoContext(ctx, "recreating user-deleted mirror after reschedule", "google_id", sid)
	}

	uid, href, err := r.identity(ctx, m, master)
	if err != nil {
		return err
	}

	var prev *icloud.Object
	if m != nil && m.ICloudETag != "" && m.State != store.StateUserDeleted && m.State != store.StateDeleted {
		prev, err = r.c.Get(ctx, href)
		if errors.Is(err, icloud.ErrNotFound) {
			// The mirror was ours and is gone, and we did not delete it:
			// the user deleted it in Apple Calendar (§8.5).
			return r.markUserDeleted(ctx, m)
		}
		if err != nil {
			return fmt.Errorf("get %s: %w", href, err)
		}
	}

	plan, err := r.planRSVP(ctx, m, master, exceptions, prev)
	if err != nil {
		return err
	}
	rendered, hash, err := r.render(master, exceptions, uid, m, prev, plan)
	if err != nil {
		return r.markSkipped(ctx, m, master, skipRenderPfx+err.Error())
	}
	if m != nil && m.State == store.StateSynced && m.ContentHash == hash && !r.rsvpDiffers(prev, plan) {
		r.rep.Unchanged++
		clearFailures(m)
		m.GoogleStatus, m.AgreedStatus = plan.google, plan.agreed
		if prev != nil {
			m.ICloudETag = prev.ETag
		}
		if err := r.q.UpdateMapping(ctx, m); err != nil {
			return err
		}
		if err := r.saveInstances(ctx, m, plan); err != nil {
			return err
		}
		return r.maybeHideDeclined(ctx, m)
	}

	// Write-ahead intent (§8.6): the row is pending_put before the PUT.
	if m == nil {
		m = &store.Mapping{
			GoogleCalendarID: cal, GoogleEventID: sid, ICalUID: uid,
			ICloudCalendar: r.opts().CalendarHref, ICloudHref: href, State: store.StatePendingPut,
		}
		if err := r.q.CreateMapping(ctx, m); err != nil {
			return err
		}
	} else {
		m.State, m.ICalUID, m.ICloudHref, m.SkipReason = store.StatePendingPut, uid, href, ""
		if err := r.q.UpdateMapping(ctx, m); err != nil {
			return err
		}
	}
	return r.putMirror(ctx, m, master, exceptions, rendered, prev, hash, plan, attempt)
}

// identity returns the UID and href for a series: the mapping's for an
// existing row, else iCalUID with the §7.1 collision rule.
func (r *run) identity(ctx context.Context, m *store.Mapping, ev *calendar.Event) (string, string, error) {
	calHref := r.opts().CalendarHref
	if m != nil {
		return m.ICalUID, m.ICloudHref, nil
	}
	uid := ev.ICalUID
	if uid == "" {
		uid = ev.Id + "@google.com"
	}
	other, err := r.q.GetMappingByUID(ctx, calHref, uid)
	switch {
	case err == nil && other.GoogleEventID != ev.Id:
		// §7.1: a different Google event with an iCalUID we already map
		// The M13 trial (T13) showed Google gives the new series of a "this
		// and following" change a new iCalUID, so this is a safety net.
		domain := r.opts().SelfEmail[strings.LastIndex(r.opts().SelfEmail, "@")+1:]
		uid = "calbridge-" + ev.Id + "@" + domain
		r.log.WarnContext(ctx, "iCalUID already mapped to another event; using a calbridge UID", "google_id", ev.Id)
	case err != nil && !store.IsNotFound(err):
		return "", "", err
	}
	return uid, translate.ObjectHref(calHref, uid), nil
}

func (r *run) renderOptions(uid string, m *store.Mapping) translate.RenderOptions {
	o := r.opts()
	ro := translate.RenderOptions{
		SelfEmail: o.SelfEmail, ScheduleAgent: o.ScheduleAgent, Privacy: o.Privacy, Attendees: o.Attendees,
		DefaultAlarms: o.DefaultAlarms, EmitVTimezone: o.EmitVTimezone, Now: r.now,
		GoogleCalendarID: o.GoogleCalendarID, UID: uid,
	}
	if m != nil {
		ro.MinSequence = m.Sequence
	}
	return ro
}

// render builds the mirror and its content hash (rendered content minus
// volatile and Apple-owned parts and the user's PARTSTATs, which are
// compared against what Apple shows instead: rsvpDiffers).
func (r *run) render(ev *calendar.Event, exceptions []*calendar.Event, uid string, m *store.Mapping, prev *icloud.Object, plan rsvpPlan) (*ical.Calendar, string, error) {
	var prevCal *ical.Calendar
	if prev != nil {
		prevCal = prev.Cal
	}
	cal, err := translate.RenderSeries(ev, exceptions, plan.view(), prevCal, r.renderOptions(uid, m))
	if err != nil {
		return nil, "", err
	}
	return cal, translate.ContentHash(cal, r.opts().SelfEmail), nil
}

func allCancelled(evs []*calendar.Event) bool {
	for _, ev := range evs {
		if ev.Status != "cancelled" {
			return false
		}
	}
	return true
}

// putMirror is §8.2 putMirror.
func (r *run) putMirror(ctx context.Context, m *store.Mapping, ev *calendar.Event, exceptions []*calendar.Event, cal *ical.Calendar,
	prev *icloud.Object, hash string, plan rsvpPlan, attempt int) error {
	op := "put_update"
	cond := icloud.PutCond{IfNoneMatchAny: true}
	if prev != nil {
		cond = icloud.PutCond{IfMatch: prev.ETag}
	} else {
		op = "put_create"
	}
	etag, err := r.c.Put(ctx, m.ICloudHref, cal, cond)
	switch {
	case err == nil:
	case prev == nil && errors.Is(err, icloud.ErrPreconditionFailed):
		// Exists already: a crash after an earlier PUT, or a leftover (§8.6).
		return r.adopt(ctx, m, ev, exceptions)
	case prev == nil && errors.Is(err, icloud.ErrUIDConflict):
		// UNVERIFIED (P0-4): iCloud reporting no-uid-conflict when the user
		// imported the invitation's .ics into this calendar.
		return r.adoptByUID(ctx, m, ev, exceptions)
	case prev != nil && errors.Is(err, icloud.ErrPreconditionFailed) && attempt == 0:
		// Apple changed the object between our GET and PUT: re-GET (which
		// carries the user's alarms over again) and retry once.
		r.log.DebugContext(ctx, "412 on update; retrying with a fresh GET", "google_id", m.GoogleEventID)
		return r.processSeries(ctx, m.GoogleEventID, attempt+1)
	default:
		return fmt.Errorf("put %s: %w", m.ICloudHref, err)
	}
	if etag == "" {
		obj, err := r.c.Get(ctx, m.ICloudHref)
		if err != nil {
			return fmt.Errorf("get after put %s: %w", m.ICloudHref, err)
		}
		etag = obj.ETag
	}
	if prev == nil {
		r.rep.Created++
	} else {
		r.rep.Updated++
	}
	r.logOp(ctx, op, m)
	return r.markSynced(ctx, m, ev, etag, hash, plan)
}

func (r *run) markSynced(ctx context.Context, m *store.Mapping, ev *calendar.Event, etag, hash string, plan rsvpPlan) error {
	m.State, m.ICloudETag, m.ContentHash, m.SkipReason = store.StateSynced, etag, hash, ""
	m.DeletedAt = time.Time{}
	clearFailures(m)
	if ev.Sequence > m.Sequence {
		m.Sequence = ev.Sequence
	}
	m.LastGoogleETag = ev.Etag
	if t, err := time.Parse(time.RFC3339Nano, ev.Updated); err == nil {
		m.LastGoogleUpdated = t
	}
	m.GoogleStatus, m.ICloudPartStat, m.AgreedStatus = plan.google, string(plan.render), plan.agreed
	if err := r.q.UpdateMapping(ctx, m); err != nil {
		return err
	}
	if err := r.saveInstances(ctx, m, plan); err != nil {
		return err
	}
	return r.maybeHideDeclined(ctx, m)
}

// adopt takes over an object already at our deterministic href (§8.6).
func (r *run) adopt(ctx context.Context, m *store.Mapping, ev *calendar.Event, exceptions []*calendar.Event) error {
	obj, err := r.c.Get(ctx, m.ICloudHref)
	if err != nil {
		return fmt.Errorf("adopt get %s: %w", m.ICloudHref, err)
	}
	if !ownedBy(obj.Cal, m.ICalUID, ev.Id) {
		r.log.ErrorContext(ctx, "object at our href has a different UID; not touching it", "href", m.ICloudHref, "google_id", ev.Id)
		return r.markSkipped(ctx, m, ev, SkipHrefCollision)
	}
	return r.overwrite(ctx, m, ev, exceptions, obj)
}

// adoptByUID merges a copy of the event the user imported elsewhere in the
// calendar into the mirror (§8.6).
func (r *run) adoptByUID(ctx context.Context, m *store.Mapping, ev *calendar.Event, exceptions []*calendar.Event) error {
	obj, err := r.c.FindByUID(ctx, r.opts().CalendarHref, m.ICalUID)
	if err != nil {
		return fmt.Errorf("adopt by uid: %w", err)
	}
	m.ICloudHref = obj.Href
	if err := r.q.UpdateMapping(ctx, m); err != nil {
		return err
	}
	return r.overwrite(ctx, m, ev, exceptions, obj)
}

// overwrite replaces obj with our rendering (keeping its alarms/Apple props)
// and marks the mapping synced.
func (r *run) overwrite(ctx context.Context, m *store.Mapping, ev *calendar.Event, exceptions []*calendar.Event, obj *icloud.Object) error {
	plan, err := r.planRSVP(ctx, m, ev, exceptions, obj)
	if err != nil {
		return err
	}
	cal, hash, err := r.render(ev, exceptions, m.ICalUID, m, obj, plan)
	if err != nil {
		return r.markSkipped(ctx, m, ev, skipRenderPfx+err.Error())
	}
	etag, err := r.c.Put(ctx, obj.Href, cal, icloud.PutCond{IfMatch: obj.ETag})
	if err != nil {
		return fmt.Errorf("adopt put %s: %w", obj.Href, err)
	}
	if etag == "" {
		got, err := r.c.Get(ctx, obj.Href)
		if err != nil {
			return err
		}
		etag = got.ETag
	}
	r.rep.Adopted++
	r.logOp(ctx, "adopt", m)
	return r.markSynced(ctx, m, ev, etag, hash, plan)
}

// ownedBy reports whether cal is the mirror for uid/googleID: same UID, and
// X-CALBRIDGE-GID (when present) naming the same Google event.
func ownedBy(cal *ical.Calendar, uid, googleID string) bool {
	if cal == nil {
		return false
	}
	for _, c := range cal.Children {
		if c.Name != ical.CompEvent {
			continue
		}
		if p := c.Props.Get(ical.PropUID); p == nil || p.Value != uid {
			return false
		}
		if p := c.Props.Get(translate.PropGoogleID); p != nil && p.Value != googleID {
			return false
		}
		return true
	}
	return false
}

func (r *run) markSkipped(ctx context.Context, m *store.Mapping, ev *calendar.Event, reason string) error {
	r.rep.Skipped++
	if m == nil {
		uid := ev.ICalUID
		if uid == "" {
			uid = ev.Id + "@google.com"
		}
		nm := &store.Mapping{
			GoogleCalendarID: r.opts().GoogleCalendarID, GoogleEventID: ev.Id, ICalUID: uid,
			ICloudCalendar: r.opts().CalendarHref, ICloudHref: translate.ObjectHref(r.opts().CalendarHref, uid),
			State: store.StateSkipped, SkipReason: reason,
		}
		err := r.q.CreateMapping(ctx, nm)
		if errors.Is(err, store.ErrConflict) {
			return nil // UID or href already used by another row; nothing to record
		}
		return err
	}
	switch m.State {
	case store.StateSkipped:
		if m.SkipReason == reason {
			return nil
		}
	case store.StateSynced, store.StatePendingPut:
		// A live mirror whose event became filtered (e.g. turned recurring
		// before M9) is left in place; only deleteMirror removes mirrors.
		r.log.InfoContext(ctx, "mirror's event is now filtered; leaving it", "google_id", m.GoogleEventID, "reason", reason)
		if m.State == store.StateSynced {
			return nil
		}
	}
	m.State, m.SkipReason = store.StateSkipped, reason
	return r.q.UpdateMapping(ctx, m)
}

func (r *run) markUserDeleted(ctx context.Context, m *store.Mapping) error {
	r.rep.UserDeleted++
	r.log.InfoContext(ctx, "mirror deleted in Apple Calendar; not recreating", "google_id", m.GoogleEventID, "href", m.ICloudHref)
	m.State = store.StateUserDeleted
	return r.q.UpdateMapping(ctx, m)
}

// goneFromGoogle handles a cancelled event or one the user is no longer
// invited to (§8.5).
func (r *run) goneFromGoogle(ctx context.Context, m *store.Mapping) error {
	if m == nil {
		return nil // e.g. a tombstone for an event never seen (P0-1)
	}
	switch m.State {
	case store.StateSynced, store.StatePendingPut, store.StatePendingDelete:
		return r.deleteMirror(ctx, m)
	case store.StateUserDeleted, store.StateSkipped:
		m.State, m.DeletedAt = store.StateDeleted, r.now
		return r.q.UpdateMapping(ctx, m)
	}
	return nil
}

// retryInstanceRSVPs re-sends per-occurrence responses left pending.
func (r *run) retryInstanceRSVPs(ctx context.Context, m *store.Mapping) error {
	rows, err := r.q.ListInstanceStates(ctx, m.ID)
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		if row.RSVPPending == "" {
			continue
		}
		if _, err := r.pushInstance(ctx, m, row.RecurrenceID, row.GoogleEventID, row.RSVPPending); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

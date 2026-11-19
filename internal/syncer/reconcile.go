package syncer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	calendar "google.golang.org/api/calendar/v3"

	"github.com/JasonBerry/calbridge/internal/googlecal"
	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/store"
	"github.com/JasonBerry/calbridge/internal/translate"
)

// Reconcile actions (PLAN.md §9 reconcile).
const (
	ActOK          = "ok"           // mapping and mirror agree
	ActAdopt       = "adopt"        // mirror matches Google; (re)build the mapping, no write
	ActUpdate      = "update"       // mirror exists but differs from Google; rewrite it
	ActCreate      = "create"       // Google event has no mirror; create it
	ActUserDeleted = "user_deleted" // mirror gone and the user deleted it: left deleted (--restore recreates)
	ActRestore     = "restore"      // --restore: recreate a mirror the user deleted
	ActOrphan      = "orphan"       // calbridge-owned object whose Google event is gone
	ActClose       = "close"        // stale mapping row with nothing to mirror: closed in the store only
)

// ReconcileItem is one line of the plan: ids and hrefs only.
type ReconcileItem struct {
	Action   string
	GoogleID string
	Href     string
	Note     string
}

// ReconcileOptions select what Reconcile may do.
type ReconcileOptions struct {
	Apply         bool // default is a dry run: print the plan only
	Restore       bool // recreate mirrors the user deleted
	DeleteOrphans bool // with Apply: delete orphan objects (capped like full-resync deletes)
}

// ReconcilePlan is Reconcile's result.
type ReconcilePlan struct {
	Items   []ReconcileItem
	Applied bool
	Report  Report // writes made when applied
}

// Count returns how many items have action a.
func (p ReconcilePlan) Count(a string) int {
	n := 0
	for _, it := range p.Items {
		if it.Action == a {
			n++
		}
	}
	return n
}

// Reconcile compares Google (a full listing; sync tokens untouched), the
// calbridge objects in the iCloud calendar and the store, and repairs the
// mappings (§9): a lost database is rebuilt without rewriting mirrors that
// already match, stale mirrors are rewritten, missing ones created, and
// orphans reported. Mirrors the user deleted stay deleted unless Restore
// (§8.5). Without Apply nothing is written anywhere.
func (e *Engine) Reconcile(ctx context.Context, o ReconcileOptions) (ReconcilePlan, error) {
	if err := e.Opts.Validate(); err != nil {
		return ReconcilePlan{}, err
	}
	if o.Apply {
		r := e.newRun(&e.S.Queries)
		plan, err := r.reconcile(ctx, o)
		plan.Report = *r.rep
		return plan, err
	}
	var (
		plan   ReconcilePlan
		recErr error
	)
	err := e.S.Tx(ctx, func(tx *store.Tx) error {
		r := e.newRun(&tx.Queries)
		r.c = &dryCalDAV{CalDAV: e.C}
		r.g = &dryGoogle{GoogleCalendar: e.G}
		plan, recErr = r.reconcile(ctx, o)
		return errRollback
	})
	if errors.Is(err, errRollback) {
		err = recErr
	}
	return plan, err
}

type reconSeries struct {
	sid        string
	master     *calendar.Event
	exceptions []*calendar.Event
}

func (r *run) reconcile(ctx context.Context, o ReconcileOptions) (ReconcilePlan, error) {
	cal, calHref := r.opts().GoogleCalendarID, r.opts().CalendarHref
	plan := ReconcilePlan{Applied: o.Apply}
	add := func(act, gid, href, note string) {
		plan.Items = append(plan.Items, ReconcileItem{Action: act, GoogleID: gid, Href: href, Note: note})
	}

	// 1. Google: full listing, cached; the sync token is not touched.
	items, _, err := googlecal.ListAll(ctx, r.g, googlecal.ListRequest{CalendarID: cal, TimeMin: r.now.Add(-r.opts().Lookback)})
	if err != nil {
		return plan, fmt.Errorf("google full list: %w", err)
	}
	var sids []string
	seen := map[string]bool{}
	for _, it := range items {
		if err := r.cacheEvent(ctx, it); err != nil {
			return plan, err
		}
		if sid := seriesID(it); !seen[sid] {
			seen[sid] = true
			sids = append(sids, sid)
		}
	}
	var series []reconSeries
	for _, sid := range sids {
		master, exc, err := r.loadSeries(ctx, sid)
		if err != nil {
			return plan, err
		}
		if master != nil && r.mirrorable(master) {
			series = append(series, reconSeries{sid: sid, master: master, exceptions: exc})
		}
	}

	// 2. iCloud: every object in the calendar.
	members, err := r.c.ListETags(ctx, calHref)
	if err != nil {
		return plan, fmt.Errorf("icloud list: %w", err)
	}
	hrefs := make([]string, len(members))
	for i, m := range members {
		hrefs[i] = m.Href
	}
	objs, err := r.c.MultiGet(ctx, calHref, hrefs)
	if err != nil {
		return plan, fmt.Errorf("icloud multiget: %w", err)
	}
	byHref := map[string]*icloud.Object{}
	byGID := map[string]*icloud.Object{}
	for i := range objs {
		ob := &objs[i]
		byHref[ob.Href] = ob
		if gid := googleIDOf(ob.Cal); gid != "" {
			byGID[gid] = ob
		}
	}
	used := map[string]bool{}

	// 3. Each mirrorable Google series.
	var errs []error
	for _, s := range series {
		m, err := r.mapping(ctx, s.sid)
		if err != nil {
			return plan, err
		}
		expected := translate.ObjectHref(calHref, s.master.ICalUID)
		if m != nil {
			expected = m.ICloudHref
		}
		obj := byGID[s.sid]
		if obj == nil {
			obj = byHref[expected]
		}
		if obj != nil {
			if gid := googleIDOf(obj.Cal); gid != "" && gid != s.sid {
				// Another event's mirror sits at this href (shared iCalUID,
				// §7.1). Never overwrite it: the create path's href-collision
				// guard handles this series.
				obj = nil
			} else {
				used[obj.Href] = true
			}
		}
		act, note := r.classify(s, m, obj, o)
		href := expected
		if obj != nil {
			href = obj.Href
		}
		add(act, s.sid, href, note)
		if o.Apply {
			if err := r.applyItem(ctx, act, s, m, obj); err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", act, s.sid, err))
			}
		}
	}

	// 4. calbridge-owned objects nobody claimed.
	var orphans []*icloud.Object
	for _, ob := range objs {
		if used[ob.Href] || !owned(ob) {
			continue
		}
		gid := googleIDOf(ob.Cal)
		if gid != "" && r.stillMirrorable(ctx, gid) {
			continue // outside the listing window (e.g. an older series): leave it
		}
		if ended(ob.Cal, r.now.Add(-r.opts().Lookback)) {
			continue // historical mirror: left alone, like full resync
		}
		ob := ob
		orphans = append(orphans, &ob)
		note := "Google event gone"
		if gid == "" {
			note = "no X-CALBRIDGE-GID"
		}
		add(ActOrphan, gid, ob.Href, note)
	}
	if o.Apply && o.DeleteOrphans && len(orphans) > 0 {
		limit := r.opts().MaxFullResyncDeletes
		if limit <= 0 {
			limit = 5
		}
		if len(orphans) > limit {
			errs = append(errs, fmt.Errorf("%w (%d orphans > %d)", ErrTooManyDeletes, len(orphans), limit))
		} else {
			for _, ob := range orphans {
				if err := r.deleteOrphan(ctx, ob); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}

	// 5. Live mapping rows nothing matched: close them in the store.
	ms, err := r.q.ListMappings(ctx, store.StateSynced, store.StatePendingPut)
	if err != nil {
		return plan, err
	}
	live := map[string]bool{}
	for _, s := range series {
		live[s.sid] = true
	}
	for i := range ms {
		m := ms[i]
		if m.ICloudCalendar != calHref || live[m.GoogleEventID] || byHref[m.ICloudHref] != nil {
			continue
		}
		add(ActClose, m.GoogleEventID, m.ICloudHref, "no Google event and no mirror")
		if o.Apply {
			m.State = store.StatePendingDelete
			uerr := r.q.UpdateMapping(ctx, &m)
			if uerr == nil {
				m.State, m.DeletedAt = store.StateDeleted, r.now
				uerr = r.q.UpdateMapping(ctx, &m)
			}
			if uerr != nil {
				errs = append(errs, uerr)
			}
		}
	}
	sort.SliceStable(plan.Items, func(i, j int) bool { return plan.Items[i].Action < plan.Items[j].Action })
	return plan, errors.Join(errs...)
}

// mirrorable reports whether g2i would mirror this master.
func (r *run) mirrorable(ev *calendar.Event) bool {
	self := translate.SelfAttendee(ev)
	if ev.Status == "cancelled" || self == nil || r.skipReason(ev) != "" {
		return false
	}
	hiddenDeclined := r.opts().DeclinedDelete && self.ResponseStatus == "declined" && len(ev.Recurrence) == 0
	return !hiddenDeclined
}

func (r *run) stillMirrorable(ctx context.Context, gid string) bool {
	ev, err := r.g.Get(ctx, r.opts().GoogleCalendarID, gid)
	return err == nil && r.mirrorable(ev)
}

// classify decides the action for one mirrorable Google series.
func (r *run) classify(s reconSeries, m *store.Mapping, obj *icloud.Object, o ReconcileOptions) (string, string) {
	if obj == nil {
		switch {
		case m != nil && (m.State == store.StateUserDeleted || m.State == store.StateSynced):
			// A live row whose mirror vanished: the user deleted it (§8.5).
			if o.Restore {
				return ActRestore, "user deleted it; --restore recreates"
			}
			return ActUserDeleted, "user deleted it; kept deleted (--restore to recreate)"
		case m != nil:
			return ActCreate, "mapping was " + string(m.State)
		}
		return ActCreate, ""
	}
	if m != nil && m.State == store.StateSynced && m.ICloudHref == obj.Href &&
		icloud.NormalizeETag(m.ICloudETag) == icloud.NormalizeETag(obj.ETag) {
		return ActOK, ""
	}
	if r.matches(s, obj) {
		if m == nil {
			return ActAdopt, "mapping missing"
		}
		return ActAdopt, "mapping was " + string(m.State)
	}
	return ActUpdate, "mirror differs from Google"
}

// matches reports whether obj already shows what calbridge would write,
// ignoring Apple's rewrites, and every response equals Google's.
func (r *run) matches(s reconSeries, obj *icloud.Object) bool {
	want, err := translate.RenderSeries(s.master, s.exceptions, translate.RSVPView{}, obj.Cal, r.renderOptions(uidOf(obj.Cal), nil))
	if err != nil {
		return false
	}
	self := r.opts().SelfEmail
	if translate.SemanticHash(want, self) != translate.SemanticHash(obj.Cal, self) {
		return false
	}
	wps, wov, err1 := translate.ExtractSelfStatus(want, self)
	gps, gov, err2 := translate.ExtractSelfStatus(obj.Cal, self)
	if err1 != nil || err2 != nil || wps != gps || len(wov) != len(gov) {
		return false
	}
	for k, v := range wov {
		if gov[k] != v {
			return false
		}
	}
	return true
}

// applyItem carries out one action for a mirrorable series.
func (r *run) applyItem(ctx context.Context, act string, s reconSeries, m *store.Mapping, obj *icloud.Object) error {
	switch act {
	case ActOK:
		return nil
	case ActUserDeleted:
		if m.State != store.StateUserDeleted {
			return r.markUserDeleted(ctx, m)
		}
		return nil
	case ActAdopt:
		return r.adoptInPlace(ctx, s, m, obj)
	case ActUpdate, ActCreate, ActRestore:
		if m == nil && obj != nil {
			// Record the row first so the rewrite is one conditional PUT.
			m = &store.Mapping{GoogleCalendarID: r.opts().GoogleCalendarID, GoogleEventID: s.sid, ICalUID: uidOf(obj.Cal),
				ICloudCalendar: r.opts().CalendarHref, ICloudHref: obj.Href, State: store.StatePendingPut}
			if err := r.q.CreateMapping(ctx, m); err != nil {
				return err
			}
		}
		if m != nil {
			m.State, m.SkipReason = store.StatePendingPut, ""
			clearFailures(m)
			m.ICloudETag = ""
			if obj != nil {
				m.ICloudHref, m.ICloudETag = obj.Href, obj.ETag
			}
			if err := r.q.UpdateMapping(ctx, m); err != nil {
				return err
			}
		}
		return r.item(ctx, func(ictx context.Context) error { return r.processSeries(ictx, s.sid, 0) })
	}
	return nil
}

// adoptInPlace records an object that already matches Google as the synced
// mirror, without writing to iCloud.
func (r *run) adoptInPlace(ctx context.Context, s reconSeries, m *store.Mapping, obj *icloud.Object) error {
	uid := uidOf(obj.Cal)
	if m == nil {
		m = &store.Mapping{GoogleCalendarID: r.opts().GoogleCalendarID, GoogleEventID: s.sid, ICalUID: uid,
			ICloudCalendar: r.opts().CalendarHref, ICloudHref: obj.Href, State: store.StatePendingPut}
		if err := r.q.CreateMapping(ctx, m); err != nil {
			return err
		}
	} else if m.State != store.StatePendingPut && m.State != store.StateSynced {
		m.State = store.StatePendingPut
		if err := r.q.UpdateMapping(ctx, m); err != nil {
			return err
		}
	}
	m.ICalUID, m.ICloudHref = uid, obj.Href
	// Responses match Google's (matches checked), so Google's values are the
	// agreed baseline for the series and every occurrence.
	plan, err := r.planRSVP(ctx, m, s.master, s.exceptions, obj)
	if err != nil {
		return err
	}
	_, hash, err := r.render(s.master, s.exceptions, uid, m, obj, plan)
	if err != nil {
		return err
	}
	r.rep.Adopted++
	if err := r.q.AppendOp(ctx, &store.Op{Direction: store.DirReconcile, Op: "adopt", MappingID: m.ID, Detail: "no write"}); err != nil {
		r.log.WarnContext(ctx, "ops_log append failed", "err", err)
	}
	return r.markSynced(ctx, m, s.master, obj.ETag, hash, plan)
}

// deleteOrphan removes a calbridge-owned object whose Google event is gone,
// through deleteMirror (rule 5).
func (r *run) deleteOrphan(ctx context.Context, ob *icloud.Object) error {
	m, err := r.q.GetMappingByHref(ctx, r.opts().CalendarHref, ob.Href)
	if store.IsNotFound(err) {
		// Keyed by href: several orphans can carry the same (or no)
		// X-CALBRIDGE-GID, and a mapping per Google id must be unique.
		m = store.Mapping{GoogleCalendarID: r.opts().GoogleCalendarID, GoogleEventID: "orphan:" + path.Base(ob.Href), ICalUID: uidOf(ob.Cal),
			ICloudCalendar: r.opts().CalendarHref, ICloudHref: ob.Href, State: store.StatePendingPut}
		if err := r.q.CreateMapping(ctx, &m); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	switch m.State {
	case store.StateDeleted, store.StateSkipped, store.StateUserDeleted:
		m.State = store.StatePendingPut // route to pending_delete via an allowed transition
		if err := r.q.UpdateMapping(ctx, &m); err != nil {
			return err
		}
	}
	m.ICloudETag = ob.ETag
	return r.item(ctx, func(ictx context.Context) error { return r.deleteMirror(ictx, &m) })
}

// owned reports whether calbridge wrote ob: it carries X-CALBRIDGE-GID or
// sits at a calbridge-* href.
func owned(ob icloud.Object) bool {
	return googleIDOf(ob.Cal) != "" || strings.HasPrefix(path.Base(ob.Href), "calbridge-")
}

func masterOf(cal *ical.Calendar) *ical.Component {
	if cal == nil {
		return nil
	}
	for _, c := range cal.Children {
		if c.Name == ical.CompEvent && c.Props.Get(ical.PropRecurrenceID) == nil {
			return c
		}
	}
	return nil
}

func googleIDOf(cal *ical.Calendar) string {
	if m := masterOf(cal); m != nil {
		if p := m.Props.Get(translate.PropGoogleID); p != nil {
			return p.Value
		}
	}
	return ""
}

func uidOf(cal *ical.Calendar) string {
	if m := masterOf(cal); m != nil {
		if p := m.Props.Get(ical.PropUID); p != nil {
			return p.Value
		}
	}
	return ""
}

// ended reports whether a stored object's last occurrence ended before
// cutoff (unknown or open-ended counts as not ended).
func ended(cal *ical.Calendar, cutoff time.Time) bool {
	m := masterOf(cal)
	if m == nil {
		return false
	}
	if rr := m.Props.Get(ical.PropRecurrenceRule); rr != nil {
		for _, part := range strings.Split(rr.Value, ";") {
			if k, v, ok := strings.Cut(part, "="); ok && strings.EqualFold(k, "UNTIL") {
				for _, layout := range []string{"20060102T150405Z", "20060102"} {
					if t, err := time.Parse(layout, v); err == nil {
						return t.Before(cutoff)
					}
				}
			}
		}
		return false
	}
	end, err := m.Props.DateTime(ical.PropDateTimeEnd, time.UTC)
	return err == nil && end.Before(cutoff)
}

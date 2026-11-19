package syncer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	calendar "google.golang.org/api/calendar/v3"

	"github.com/JasonBerry/calbridge/internal/googlecal"
	"github.com/JasonBerry/calbridge/internal/icloud"
	"github.com/JasonBerry/calbridge/internal/store"
	"github.com/JasonBerry/calbridge/internal/translate"
)

// rsvpDecision is the outcome of the §8.4 state machine for one response
// slot (the master; per-instance slots arrive in M10). Values use Google's
// vocabulary (accepted|tentative|declined|needsAction).
type rsvpDecision struct {
	render   translate.PartStat // PARTSTAT the mirror should show
	push     string             // status to send to Google ("" = none)
	agreed   string             // agreed_status once render is written (and push, if any, succeeded)
	conflict bool               // both sides changed to different values since they last agreed
}

// decideRSVP implements §8.4. agreed is the last value both sides were known
// to share, g Google's current response, i Apple's current response mapped
// to Google's vocabulary ("" when Apple shows NEEDS-ACTION or anything that
// is never propagated). A side "changed" when it differs from agreed; a value
// only moves when it differs from agreed, so writing it to the other side and
// seeing it come back is a no-op (no ping-pong).
//
// Conflicts (both changed, to different values) go to Apple: §8.4's
// last-change-wins falls back to iCloud when the iCloud change time is
// unknown, and P0-5 showed LAST-MODIFIED is unreliable (Apple's Maybe on the
// Mac did not update it), so it is always unknown.
func decideRSVP(agreed, g, i string, pushGoogleToICloud bool) rsvpDecision {
	appleChanged := i != "" && i != agreed
	googleChanged := g != agreed
	switch {
	case agreed == "":
		// First sight (new mirror, or adopting an object we did not write):
		// nothing was ever agreed, so Google's value is the baseline and
		// nothing is sent.
		return rsvpDecision{render: translate.GoogleToPartStat(g), agreed: g}
	case appleChanged && googleChanged && g == i:
		return rsvpDecision{render: translate.GoogleToPartStat(i), agreed: i} // converged on their own
	case appleChanged:
		return rsvpDecision{render: translate.GoogleToPartStat(i), push: i, agreed: agreed, conflict: googleChanged}
	case googleChanged && pushGoogleToICloud:
		// Organizer reset after a reschedule, a response made in Google's UI,
		// or the first sight of the event: Google decides (§7.4).
		return rsvpDecision{render: translate.GoogleToPartStat(g), agreed: g}
	case googleChanged:
		// sync.push_google_rsvp_to_icloud: false. Keep what Apple shows.
		show := i
		if show == "" {
			show = agreed
		}
		if show == "" {
			show = g
		}
		return rsvpDecision{render: translate.GoogleToPartStat(show), agreed: agreed}
	}
	return rsvpDecision{render: translate.GoogleToPartStat(g), agreed: agreed}
}

// rsvpPlan is what a PUT will write and what to store after it: the
// master slot and one slot per occurrence that has a Google exception or an
// Apple override (§7.8 rule 9).
type rsvpPlan struct {
	render translate.PartStat
	google string
	agreed string
	occ    []occPlan
}

type occPlan struct {
	key      string // normalised RECURRENCE-ID
	render   translate.PartStat
	google   string
	agreed   string
	googleID string // Google instance id, if known
}

func (p rsvpPlan) view() translate.RSVPView {
	v := translate.RSVPView{Master: p.render}
	if len(p.occ) > 0 {
		v.Overrides = map[string]translate.PartStat{}
		for _, o := range p.occ {
			v.Overrides[o.key] = o.render
		}
	}
	return v
}

// planRSVP runs the state machine for a Google→iCloud write (§8.4, Google
// side). prev is the fresh iCloud object, if any: Apple's current PARTSTATs
// are read from it, so a response the user just made (to the series or to
// one occurrence) is neither clobbered nor lost when our PUT changes the
// ETag. Apple-side changes are pushed to Google first.
func (r *run) planRSVP(ctx context.Context, m *store.Mapping, ev *calendar.Event, exceptions []*calendar.Event, prev *icloud.Object) (rsvpPlan, error) {
	g := translate.SelfAttendee(ev).ResponseStatus
	// Apple's side only counts when there is a live object to read it from:
	// a stored PARTSTAT of a deleted or never-written mirror is stale.
	agreed, i := "", ""
	if m != nil {
		agreed = m.AgreedStatus
	}
	var appleOv map[string]translate.PartStat
	if prev != nil && prev.Cal != nil {
		if ps, ov, err := translate.ExtractSelfStatus(prev.Cal, r.opts().SelfEmail); err == nil {
			i, _ = translate.PartStatToGoogle(ps)
			appleOv = ov
			if m != nil {
				m.ICloudPartStat = string(ps)
			}
		}
	}
	plan := rsvpPlan{google: g}
	d := decideRSVP(agreed, g, i, !r.opts().NoGoogleRSVPToICloud)
	if d.conflict {
		r.log.InfoContext(ctx, "RSVP changed on both sides; keeping Apple's", "google_id", ev.Id)
	}
	plan.render, plan.agreed = d.render, d.agreed
	if d.push != "" && m != nil {
		if err := r.pushRSVP(ctx, m, d.push); err != nil {
			r.log.WarnContext(ctx, "RSVP push failed; will retry", "google_id", ev.Id, "err", err)
		} else {
			plan.google, plan.agreed = m.GoogleStatus, m.AgreedStatus
		}
	}
	if len(ev.Recurrence) == 0 {
		return plan, nil
	}

	// Occurrences: Google exceptions and Apple overrides.
	rows := map[string]store.InstanceState{}
	if m != nil && m.ID != 0 {
		list, err := r.q.ListInstanceStates(ctx, m.ID)
		if err != nil {
			return plan, err
		}
		for _, row := range list {
			rows[row.RecurrenceID] = row
		}
	}
	gEx := map[string]*calendar.Event{}
	var keys []string
	for _, ex := range exceptions {
		if ex.Status == "cancelled" || translate.SelfAttendee(ex) == nil {
			continue
		}
		if k, ok := translate.OccurrenceKey(ev, ex); ok {
			if _, dup := gEx[k]; !dup {
				keys = append(keys, k)
			}
			gEx[k] = ex
		}
	}
	for k := range appleOv {
		if _, ok := gEx[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		row, hasRow := rows[k]
		gk, googleID := plan.google, row.GoogleEventID // an occurrence inherits the series' response
		if ex := gEx[k]; ex != nil {
			gk, googleID = translate.SelfAttendee(ex).ResponseStatus, ex.Id
		}
		ik := ""
		if ps, ok := appleOv[k]; ok {
			ik, _ = translate.PartStatToGoogle(ps)
		}
		agreedK := plan.agreed // never answered separately: it agreed with the series
		if hasRow && row.AgreedStatus != "" {
			agreedK = row.AgreedStatus
		}
		dk := decideRSVP(agreedK, gk, ik, !r.opts().NoGoogleRSVPToICloud)
		op := occPlan{key: k, render: dk.render, google: gk, agreed: dk.agreed, googleID: googleID}
		if dk.push != "" && m != nil {
			id, err := r.pushInstance(ctx, m, k, googleID, dk.push)
			if err != nil {
				r.log.WarnContext(ctx, "occurrence RSVP push failed; will retry", "google_id", ev.Id, "occurrence", k, "err", err)
			} else if id != "" {
				op.googleID, op.google, op.agreed = id, dk.push, dk.push
			}
		}
		plan.occ = append(plan.occ, op)
	}
	return plan, nil
}

// rsvpDiffers reports whether the PARTSTATs plan renders differ from what
// Apple shows in prev (an occurrence without an override shows the series').
func (r *run) rsvpDiffers(prev *icloud.Object, plan rsvpPlan) bool {
	if prev == nil || prev.Cal == nil {
		return true
	}
	ps, ov, err := translate.ExtractSelfStatus(prev.Cal, r.opts().SelfEmail)
	if err != nil || ps != plan.render {
		return true
	}
	for _, o := range plan.occ {
		shown, ok := ov[o.key]
		if !ok {
			shown = ps
		}
		if shown != o.render {
			return true
		}
	}
	return false
}

// saveInstances stores per-occurrence state after a write (or a confirmed
// no-op) and drops rows for occurrences no longer separately tracked.
func (r *run) saveInstances(ctx context.Context, m *store.Mapping, plan rsvpPlan) error {
	if m.ID == 0 {
		return nil
	}
	existing, err := r.q.ListInstanceStates(ctx, m.ID)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, o := range plan.occ {
		keep[o.key] = true
		row := &store.InstanceState{MappingID: m.ID, RecurrenceID: o.key, GoogleEventID: o.googleID,
			GoogleStatus: o.google, ICloudPartStat: string(o.render), AgreedStatus: o.agreed}
		for _, e := range existing {
			if e.RecurrenceID == o.key {
				row.RSVPPending = e.RSVPPending
				if row.GoogleEventID == "" {
					row.GoogleEventID = e.GoogleEventID
				}
			}
		}
		if err := r.q.PutInstanceState(ctx, row); err != nil {
			return err
		}
	}
	for _, e := range existing {
		if !keep[e.RecurrenceID] && e.RSVPPending == "" {
			if err := r.q.DeleteInstanceState(ctx, m.ID, e.RecurrenceID); err != nil {
				return err
			}
		}
	}
	return nil
}

// pushInstance sends the user's response for one occurrence (§7.8 rule 9):
// durable intent on the instance_state row, then SetSelfResponse on the
// Google instance (known id, else InstanceAt, else the <master>_<key> id).
// It returns the instance id ("" when the occurrence no longer exists).
func (r *run) pushInstance(ctx context.Context, m *store.Mapping, key, googleID, status string) (string, error) {
	cal := r.opts().GoogleCalendarID
	row := &store.InstanceState{MappingID: m.ID, RecurrenceID: key}
	if list, err := r.q.ListInstanceStates(ctx, m.ID); err == nil {
		for _, e := range list {
			if e.RecurrenceID == key {
				*row = e
			}
		}
	}
	row.RSVPPending = status
	if err := r.q.PutInstanceState(ctx, row); err != nil {
		return "", err
	}
	id := googleID
	if id == "" {
		id = row.GoogleEventID
	}
	if id == "" {
		// UNVERIFIED (no Phase 0 item covered per-occurrence RSVP via the
		// API): InstanceAt with an all-day series' date at 00:00Z, and the
		// <master>_<key> instance id fallback.
		at, ok := keyTime(key)
		if !ok {
			return "", fmt.Errorf("occurrence key %q", key)
		}
		ev, err := r.g.InstanceAt(ctx, cal, m.GoogleEventID, at)
		switch {
		case err == nil:
			id = ev.Id
		case errors.Is(err, googlecal.ErrNotFound):
			id = m.GoogleEventID + "_" + key
		default:
			return "", err
		}
	}
	ev, err := r.g.SetSelfResponse(ctx, cal, id, status, r.sendUpdates())
	switch {
	case errors.Is(err, googlecal.ErrNotFound), errors.Is(err, googlecal.ErrNotAttendee):
		r.log.WarnContext(ctx, "occurrence RSVP not sent: occurrence gone or user not invited", "google_id", id)
		row.RSVPPending = ""
		return "", r.q.PutInstanceState(ctx, row)
	case err != nil:
		return "", fmt.Errorf("rsvp %s: %w", id, err)
	}
	g := status
	if self := translate.SelfAttendee(ev); self != nil {
		g = self.ResponseStatus
	}
	row.GoogleEventID, row.GoogleStatus, row.AgreedStatus, row.RSVPPending = id, g, g, ""
	row.ICloudPartStat = string(translate.GoogleToPartStat(status))
	// Google now has an exception for this occurrence; cache it so the
	// series can be brought up to date in this cycle (no echo next cycle).
	if ev.RecurringEventId != "" {
		if err := r.cacheEvent(ctx, ev); err != nil {
			return "", err
		}
		r.touched[m.GoogleEventID] = true
	}
	r.rep.RSVPs++
	r.rep.Ops = append(r.rep.Ops, PlannedOp{Op: "patch_rsvp", GoogleID: id, Href: m.ICloudHref})
	r.log.DebugContext(ctx, "remote write", "op", "patch_rsvp", "google_id", id, "status", g)
	if err := r.q.AppendOp(ctx, &store.Op{Direction: store.DirI2G, Op: "patch_rsvp", MappingID: m.ID, Detail: "occurrence=" + key + " status=" + g}); err != nil {
		r.log.WarnContext(ctx, "ops_log append failed", "err", err)
	}
	return id, r.q.PutInstanceState(ctx, row)
}

func keyTime(key string) (time.Time, bool) {
	if t, err := time.Parse("20060102T150405Z", key); err == nil {
		return t, true
	}
	t, err := time.Parse("20060102", key)
	return t, err == nil
}

// pushRSVP sends the user's response to Google (§8.4 handleRSVP): durable
// intent first (rsvp_pending), then SetSelfResponse with the configured
// sendUpdates. On success both sides agree. Permanent failures (event gone,
// user no longer invited) clear the intent; others leave it for retry.
func (r *run) pushRSVP(ctx context.Context, m *store.Mapping, status string) error {
	m.RSVPPending = status
	if err := r.q.UpdateMapping(ctx, m); err != nil {
		return err
	}
	ev, err := r.g.SetSelfResponse(ctx, r.opts().GoogleCalendarID, m.GoogleEventID, status, r.sendUpdates())
	switch {
	case errors.Is(err, googlecal.ErrNotFound), errors.Is(err, googlecal.ErrNotAttendee):
		r.log.WarnContext(ctx, "RSVP not sent: event gone or user not invited", "google_id", m.GoogleEventID)
		m.RSVPPending = ""
		return r.q.UpdateMapping(ctx, m)
	case err != nil:
		return fmt.Errorf("rsvp %s: %w", m.GoogleEventID, err)
	}
	g := status
	if self := translate.SelfAttendee(ev); self != nil {
		g = self.ResponseStatus
	}
	m.GoogleStatus, m.AgreedStatus, m.RSVPPending = g, g, ""
	clearFailures(m)
	r.rep.RSVPs++
	r.rep.Ops = append(r.rep.Ops, PlannedOp{Op: "patch_rsvp", GoogleID: m.GoogleEventID, Href: m.ICloudHref})
	r.log.DebugContext(ctx, "remote write", "op", "patch_rsvp", "google_id", m.GoogleEventID, "status", g)
	if err := r.q.AppendOp(ctx, &store.Op{Direction: store.DirI2G, Op: "patch_rsvp", MappingID: m.ID, Detail: "status=" + g}); err != nil {
		r.log.WarnContext(ctx, "ops_log append failed", "err", err)
	}
	if err := r.q.UpdateMapping(ctx, m); err != nil {
		return err
	}
	return r.maybeHideDeclined(ctx, m)
}

func (r *run) sendUpdates() string {
	if s := r.opts().SendUpdates; s != "" {
		return s
	}
	return "all"
}

// SkipDeclined marks a mirror removed because the user declined
// (sync.declined: delete).
const SkipDeclined = "declined"

// maybeHideDeclined deletes the mirror of a declined non-recurring event when
// sync.declined is delete (§8.4). It comes back if Google's status later
// moves away from declined.
func (r *run) maybeHideDeclined(ctx context.Context, m *store.Mapping) error {
	if !r.opts().DeclinedDelete || m.AgreedStatus != "declined" || m.State != store.StateSynced {
		return nil
	}
	if master, _, err := r.loadSeries(ctx, m.GoogleEventID); err != nil || master == nil || len(master.Recurrence) > 0 {
		return err // non-recurring events only (§8.4)
	}
	if err := r.deleteMirror(ctx, m); err != nil {
		return err
	}
	m.SkipReason = SkipDeclined
	return r.q.UpdateMapping(ctx, m)
}

// dryGoogle records RSVP pushes instead of sending them; reads pass through.
type dryGoogle struct {
	googlecal.GoogleCalendar
}

func (d *dryGoogle) SetSelfResponse(ctx context.Context, calendarID, eventID, status, _ string) (*calendar.Event, error) {
	ev, err := d.Get(ctx, calendarID, eventID)
	if err != nil {
		return nil, err
	}
	if self := translate.SelfAttendee(ev); self != nil {
		self.ResponseStatus = status
	}
	return ev, nil
}

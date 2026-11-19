package syncer

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// appleStatus is Apple's response for self in obj, in Google's vocabulary.
func (r *run) appleStatus(obj *icloud.Object) (translate.PartStat, string) {
	if obj == nil || obj.Cal == nil {
		return "", ""
	}
	ps, _, err := translate.ExtractSelfStatus(obj.Cal, r.opts().SelfEmail)
	if err != nil {
		return "", ""
	}
	g, _ := translate.PartStatToGoogle(ps)
	return ps, g
}

// rsvpPlan is what a PUT will write and what to store after it.
type rsvpPlan struct {
	render translate.PartStat
	google string
	agreed string
}

// planRSVP runs the state machine for a Google→iCloud write (§8.4, Google
// side). prev is the fresh iCloud object, if any: Apple's current PARTSTAT
// is read from it, so a response the user just made is neither clobbered
// nor lost when our PUT changes the ETag. An Apple-side change is pushed to
// Google first.
func (r *run) planRSVP(ctx context.Context, m *store.Mapping, ev *calendar.Event, prev *icloud.Object) rsvpPlan {
	g := translate.SelfAttendee(ev).ResponseStatus
	// Apple's side only counts when there is a live object to read it from:
	// a stored PARTSTAT of a deleted or never-written mirror is stale.
	agreed, i := "", ""
	if m != nil {
		agreed = m.AgreedStatus
	}
	if prev != nil {
		ps, gi := r.appleStatus(prev)
		i = gi
		if m != nil && ps != "" {
			m.ICloudPartStat = string(ps)
		}
	}
	d := decideRSVP(agreed, g, i, !r.opts().NoGoogleRSVPToICloud)
	if d.conflict {
		r.log.InfoContext(ctx, "RSVP changed on both sides; keeping Apple's", "google_id", ev.Id)
	}
	if d.push != "" && m != nil {
		if err := r.pushRSVP(ctx, m, d.push); err != nil {
			r.log.WarnContext(ctx, "RSVP push failed; will retry", "google_id", ev.Id, "err", err)
			return rsvpPlan{render: d.render, google: g, agreed: agreed}
		}
		return rsvpPlan{render: d.render, google: m.GoogleStatus, agreed: m.AgreedStatus}
	}
	return rsvpPlan{render: d.render, google: g, agreed: d.agreed}
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
		r.rep.Failed++
		return fmt.Errorf("rsvp %s: %w", m.GoogleEventID, err)
	}
	g := status
	if self := translate.SelfAttendee(ev); self != nil {
		g = self.ResponseStatus
	}
	m.GoogleStatus, m.AgreedStatus, m.RSVPPending = g, g, ""
	// Apple already shows this response: keep the stored hash in step so
	// the next Google→iCloud pass does not rewrite the mirror.
	if base, _, ok := strings.Cut(m.ContentHash, "|"); ok {
		m.ContentHash = base + "|" + string(translate.GoogleToPartStat(g))
	}
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

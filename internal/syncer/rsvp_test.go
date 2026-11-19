package syncer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	calendar "google.golang.org/api/calendar/v3"

	"github.com/JasonBerry/calbridge/internal/fakes/googlefake"
	"github.com/JasonBerry/calbridge/internal/store"
	"github.com/JasonBerry/calbridge/internal/translate"
)

// TestDecideRSVPTable covers all 16 combinations of Google's and Apple's
// response against agreed=accepted (§8.4), plus first sight and the
// push_google_rsvp_to_icloud=false mode.
func TestDecideRSVPTable(t *testing.T) {
	const A = "accepted"
	tests := []struct {
		g, i         string
		render       translate.PartStat
		push, agreed string
		conflict     bool
	}{
		// Apple unchanged (same as agreed, or NEEDS-ACTION which never propagates).
		{A, A, "ACCEPTED", "", A, false},
		{A, "", "ACCEPTED", "", A, false},
		{"tentative", A, "TENTATIVE", "", "tentative", false},
		{"tentative", "", "TENTATIVE", "", "tentative", false},
		{"declined", A, "DECLINED", "", "declined", false},
		{"declined", "", "DECLINED", "", "declined", false},
		{"needsAction", A, "NEEDS-ACTION", "", "needsAction", false}, // organizer reset after reschedule
		{"needsAction", "", "NEEDS-ACTION", "", "needsAction", false},
		// Apple changed, Google did not: push.
		{A, "tentative", "TENTATIVE", "tentative", A, false},
		{A, "declined", "DECLINED", "declined", A, false},
		// Both changed to the same value: converged, nothing to send.
		{"tentative", "tentative", "TENTATIVE", "", "tentative", false},
		{"declined", "declined", "DECLINED", "", "declined", false},
		// Both changed differently: conflict, Apple wins (LAST-MODIFIED unreliable, P0-5).
		{"tentative", "declined", "DECLINED", "declined", A, true},
		{"declined", "tentative", "TENTATIVE", "tentative", A, true},
		{"needsAction", "tentative", "TENTATIVE", "tentative", A, true},
		{"needsAction", "declined", "DECLINED", "declined", A, true},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("g=%s,i=%s", tc.g, tc.i), func(t *testing.T) {
			d := decideRSVP(A, tc.g, tc.i, true)
			want := rsvpDecision{render: tc.render, push: tc.push, agreed: tc.agreed, conflict: tc.conflict}
			if d != want {
				t.Errorf("= %+v; want %+v", d, want)
			}
		})
	}
	for _, push := range []bool{true, false} {
		if d := decideRSVP("", "needsAction", "accepted", push); d != (rsvpDecision{render: "NEEDS-ACTION", agreed: "needsAction"}) {
			t.Errorf("first sight (push=%v): %+v", push, d)
		}
	}
	if d := decideRSVP(A, "tentative", "", false); d.render != "ACCEPTED" || d.agreed != A || d.push != "" {
		t.Errorf("no Google→iCloud push: %+v", d)
	}
	if d := decideRSVP(A, "tentative", "declined", false); d.push != "declined" {
		t.Errorf("Apple change still pushed when Google→iCloud is off: %+v", d)
	}
}

func (h *harness) rsvpSetup(id string) {
	h.t.Helper()
	h.g.Put(invite(id))
	h.cycleTwice()
}

func TestAppleRSVPPushedOnceWithoutEcho(t *testing.T) {
	h := newHarness(t)
	h.rsvpSetup("a")
	puts := h.c.Puts
	h.c.AppleRSVP(hrefFor("a"), selfEmail, "ACCEPTED")
	rep := h.cycleTwice()
	h.cycleTwice()
	if rep.RSVPs != 1 || len(h.g.Patches) != 1 {
		t.Fatalf("report %+v patches %+v", rep, h.g.Patches)
	}
	if p := h.g.Patches[0]; p != (googlefake.Patch{EventID: "a", Status: "accepted", SendUpdates: "all"}) {
		t.Errorf("patch %+v", p)
	}
	if h.c.Puts != puts {
		t.Errorf("mirror rewritten after the push (%d PUTs)", h.c.Puts-puts)
	}
	m := h.mapping("a")
	if m.AgreedStatus != "accepted" || m.GoogleStatus != "accepted" || m.RSVPPending != "" {
		t.Errorf("mapping %+v", m)
	}
	if g := h.g.Event("a").Attendees[1].ResponseStatus; g != "accepted" {
		t.Errorf("Google shows %s", g)
	}
}

func TestSendUpdatesFromOptions(t *testing.T) {
	h := newHarness(t)
	h.e.Opts.SendUpdates = "none"
	h.rsvpSetup("a")
	h.c.AppleRSVP(hrefFor("a"), selfEmail, "DECLINED")
	h.cycleTwice()
	if len(h.g.Patches) != 1 || h.g.Patches[0].SendUpdates != "none" || h.g.Patches[0].Status != "declined" {
		t.Fatalf("patches %+v", h.g.Patches)
	}
}

func TestGoogleResponseChangeReachesApple(t *testing.T) {
	h := newHarness(t)
	h.rsvpSetup("a")
	h.g.Update("a", func(ev *calendar.Event) { ev.Attendees[1].ResponseStatus = "tentative" })
	rep := h.cycleTwice()
	if rep.Updated != 1 || len(h.g.Patches) != 0 || selfPartStat(t, h.c.Object(hrefFor("a"))) != "TENTATIVE" {
		t.Fatalf("report %+v patches %v", rep, h.g.Patches)
	}
}

func TestRescheduleResetShowsNeedsAction(t *testing.T) {
	h := newHarness(t)
	h.rsvpSetup("a")
	h.c.AppleRSVP(hrefFor("a"), selfEmail, "ACCEPTED")
	h.cycleTwice()
	// P0-7: the organizer moves the event; Google resets self to needsAction.
	h.g.Update("a", func(ev *calendar.Event) {
		ev.Sequence++
		ev.Start.DateTime, ev.End.DateTime = "2026-11-26T10:00:00+11:00", "2026-11-26T11:00:00+11:00"
		ev.Attendees[1].ResponseStatus = "needsAction"
	})
	h.cycleTwice()
	if ps := selfPartStat(t, h.c.Object(hrefFor("a"))); ps != "NEEDS-ACTION" || len(h.g.Patches) != 1 {
		t.Fatalf("PARTSTAT %s, patches %d", ps, len(h.g.Patches))
	}
}

func TestAppleRSVPSurvivesSimultaneousGoogleEdit(t *testing.T) {
	h := newHarness(t)
	h.rsvpSetup("a")
	h.c.AppleRSVP(hrefFor("a"), selfEmail, "DECLINED")
	h.g.Update("a", func(ev *calendar.Event) { ev.Summary = "Renamed" })
	h.cycleTwice()
	obj := h.c.Object(hrefFor("a"))
	if summaryOf(t, obj) != "Renamed" || selfPartStat(t, obj) != "DECLINED" {
		t.Fatalf("summary %q PARTSTAT %s", summaryOf(t, obj), selfPartStat(t, obj))
	}
	if len(h.g.Patches) != 1 || h.g.Patches[0].Status != "declined" {
		t.Fatalf("patches %+v", h.g.Patches)
	}
}

func TestConflictAppleWins(t *testing.T) {
	h := newHarness(t)
	h.rsvpSetup("a")
	h.c.AppleRSVP(hrefFor("a"), selfEmail, "ACCEPTED")
	h.cycleTwice()
	h.c.AppleRSVP(hrefFor("a"), selfEmail, "TENTATIVE")
	h.g.Update("a", func(ev *calendar.Event) { ev.Attendees[1].ResponseStatus = "declined" })
	h.cycleTwice()
	if ps := selfPartStat(t, h.c.Object(hrefFor("a"))); ps != "TENTATIVE" {
		t.Errorf("mirror %s", ps)
	}
	if g := h.g.Event("a").Attendees[1].ResponseStatus; g != "tentative" {
		t.Errorf("Google %s", g)
	}
	if m := h.mapping("a"); m.AgreedStatus != "tentative" {
		t.Errorf("agreed %s", m.AgreedStatus)
	}
}

func TestNeedsActionFromAppleNotPropagated(t *testing.T) {
	h := newHarness(t)
	h.rsvpSetup("a")
	h.c.AppleRSVP(hrefFor("a"), selfEmail, "ACCEPTED")
	h.cycleTwice()
	h.c.AppleRSVP(hrefFor("a"), selfEmail, "NEEDS-ACTION")
	h.cycleTwice()
	if len(h.g.Patches) != 1 {
		t.Fatalf("patches %+v", h.g.Patches)
	}
}

func TestRSVPPushRetriedAfterFailure(t *testing.T) {
	h := newHarness(t)
	h.rsvpSetup("a")
	h.c.AppleRSVP(hrefFor("a"), selfEmail, "ACCEPTED")
	h.g.FailNextPatch = errors.New("google 503")
	if _, err := h.e.Cycle(context.Background()); err == nil {
		t.Fatal("failed push not reported")
	}
	if m := h.mapping("a"); m.RSVPPending != "accepted" {
		t.Fatalf("pending %q", m.RSVPPending)
	}
	h.cycleTwice()
	if len(h.g.Patches) != 1 || h.mapping("a").RSVPPending != "" {
		t.Fatalf("patches %+v mapping %+v", h.g.Patches, h.mapping("a"))
	}
}

func TestNoGoogleRSVPToICloud(t *testing.T) {
	h := newHarness(t)
	h.e.Opts.NoGoogleRSVPToICloud = true
	h.rsvpSetup("a")
	h.g.Update("a", func(ev *calendar.Event) { ev.Attendees[1].ResponseStatus = "tentative" })
	rep := h.cycleTwice()
	if rep.Writes() != 0 || selfPartStat(t, h.c.Object(hrefFor("a"))) != "NEEDS-ACTION" {
		t.Fatalf("report %+v", rep)
	}
}

func TestDeclinedDelete(t *testing.T) {
	h := newHarness(t)
	h.e.Opts.DeclinedDelete = true
	h.rsvpSetup("a")
	h.c.AppleRSVP(hrefFor("a"), selfEmail, "DECLINED")
	h.cycleTwice()
	m := h.mapping("a")
	if h.c.Object(hrefFor("a")) != nil || m.State != store.StateDeleted || m.SkipReason != SkipDeclined {
		t.Fatalf("mapping %+v", m)
	}
	if len(h.g.Patches) != 1 || h.g.Patches[0].Status != "declined" {
		t.Fatalf("patches %+v", h.g.Patches)
	}
	// The user changes their mind in Google: the mirror comes back.
	h.g.Update("a", func(ev *calendar.Event) { ev.Attendees[1].ResponseStatus = "accepted" })
	rep := h.cycleTwice()
	if rep.Created != 1 || selfPartStat(t, h.c.Object(hrefFor("a"))) != "ACCEPTED" {
		t.Fatalf("report %+v", rep)
	}
	// An invitation first seen as declined is not mirrored at all.
	d := invite("d")
	d.Attendees[1].ResponseStatus = "declined"
	h.g.Put(d)
	if rep := h.cycleTwice(); rep.Writes() != 0 {
		t.Fatalf("declined invite mirrored: %+v", rep)
	}
}

func TestUserDeletionSeenByICloudPass(t *testing.T) {
	h := newHarness(t)
	h.rsvpSetup("a")
	h.c.UserDelete(hrefFor("a"))
	rep := h.cycleTwice()
	if rep.UserDeleted != 1 || h.mapping("a").State != store.StateUserDeleted || len(h.g.Patches) != 0 {
		t.Fatalf("report %+v", rep)
	}
}

func TestInvalidICloudTokenFullListing(t *testing.T) {
	h := newHarness(t)
	h.rsvpSetup("a")
	h.rsvpSetup("b")
	ctx := context.Background()
	if err := h.s.PutSyncState(ctx, &store.SyncState{Key: "icloud:" + calHref, Token: "bogus"}); err != nil {
		t.Fatal(err)
	}
	h.c.UserDelete(hrefFor("b"))
	rep := h.cycleTwice()
	if rep.UserDeleted != 1 || len(h.g.Patches) != 0 || h.mapping("a").State != store.StateSynced {
		t.Fatalf("report %+v", rep)
	}
	if st, _ := h.s.GetSyncState(ctx, "icloud:"+calHref); st.Token == "bogus" || st.LastFullSync.IsZero() {
		t.Errorf("token not replaced: %+v", st)
	}
}

func TestDryRunRSVP(t *testing.T) {
	h := newHarness(t)
	h.rsvpSetup("a")
	h.c.AppleRSVP(hrefFor("a"), selfEmail, "ACCEPTED")
	h.e.DryRun = true
	rep := h.cycle()
	if len(h.g.Patches) != 0 || h.g.Event("a").Attendees[1].ResponseStatus != "needsAction" {
		t.Fatal("dry-run sent the RSVP")
	}
	if len(rep.Ops) != 1 || rep.Ops[0].Op != "patch_rsvp" {
		t.Fatalf("planned %+v", rep.Ops)
	}
	if m := h.mapping("a"); m.RSVPPending != "" || m.AgreedStatus != "needsAction" {
		t.Fatalf("dry-run changed the store: %+v", m)
	}
	h.e.DryRun = false
	h.cycleTwice()
	if len(h.g.Patches) != 1 {
		t.Fatalf("patches %+v", h.g.Patches)
	}
}

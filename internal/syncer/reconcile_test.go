package syncer

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	calendar "google.golang.org/api/calendar/v3"

	"github.com/JasonBerry/calbridge/internal/store"
)

// loseDB swaps in an empty database, as if the file were lost.
func loseDB(t *testing.T, h *harness) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "fresh.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h.s, h.e.S = s, s
}

func (h *harness) reconcile(o ReconcileOptions) ReconcilePlan {
	h.t.Helper()
	p, err := h.e.Reconcile(context.Background(), o)
	if err != nil {
		h.t.Fatalf("Reconcile: %v", err)
	}
	return p
}

func TestReconcileRebuildsLostDatabase(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.g.Put(invite("b"))
	h.g.Put(series("s"))
	h.cycleTwice()
	h.c.AppleRSVP(hrefFor("b"), selfEmail, "ACCEPTED")
	h.g.PutInstance("s", friday(1), func(ev *calendar.Event) { ev.Summary = "moved" })
	h.cycleTwice()
	appleAnswerOccurrence(h, "s", 2, "DECLINED")
	h.cycleTwice()
	writes, patches := h.c.Writes(), len(h.g.Patches)

	loseDB(t, h)
	dry := h.reconcile(ReconcileOptions{})
	if dry.Count(ActAdopt) != 3 || len(dry.Items) != 3 || h.c.Writes() != writes {
		t.Fatalf("dry plan %+v", dry.Items)
	}
	if ms, _ := h.s.ListMappings(context.Background()); len(ms) != 0 {
		t.Fatal("dry run wrote mappings")
	}
	applied := h.reconcile(ReconcileOptions{Apply: true})
	if applied.Count(ActAdopt) != 3 || h.c.Writes() != writes || len(h.g.Patches) != patches {
		t.Fatalf("apply %+v; %d writes, %d patches", applied.Items, h.c.Writes()-writes, len(h.g.Patches)-patches)
	}
	for _, id := range []string{"a", "b", "s"} {
		if m := h.mapping(id); m.State != store.StateSynced {
			t.Errorf("%s: %s", id, m.State)
		}
	}
	// The rebuilt database is in step: a full resync writes nothing.
	rep := h.cycleTwice()
	if rep.Writes() != 0 || h.c.Writes() != writes || len(h.g.Patches) != patches {
		t.Fatalf("after reconcile: %+v", rep)
	}
	// And a second reconcile finds everything ok.
	if again := h.reconcile(ReconcileOptions{}); again.Count(ActOK) != 3 {
		t.Fatalf("second reconcile %+v", again.Items)
	}
}

func TestReconcileRewritesStaleMirror(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.cycleTwice()
	loseDB(t, h)
	h.g.Update("a", func(ev *calendar.Event) { ev.Summary = "Renamed while the database was lost" })
	puts := h.c.Puts
	p := h.reconcile(ReconcileOptions{Apply: true})
	if p.Count(ActUpdate) != 1 || h.c.Puts != puts+1 || summaryOf(t, h.c.Object(hrefFor("a"))) != "Renamed while the database was lost" {
		t.Fatalf("plan %+v", p.Items)
	}
	if rep := h.cycleTwice(); rep.Writes() != 0 {
		t.Fatalf("not in step: %+v", rep)
	}
}

func TestReconcileCreatesMissingMirror(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.cycle()
	loseDB(t, h)
	h.c.UserDelete(hrefFor("a")) // no record of who deleted it: recreated
	p := h.reconcile(ReconcileOptions{Apply: true})
	if p.Count(ActCreate) != 1 || h.c.Object(hrefFor("a")) == nil {
		t.Fatalf("plan %+v", p.Items)
	}
}

func TestReconcileUserDeletedAndRestore(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.cycleTwice()
	h.c.UserDelete(hrefFor("a"))
	h.cycleTwice()
	writes := h.c.Writes()
	p := h.reconcile(ReconcileOptions{Apply: true})
	if p.Count(ActUserDeleted) != 1 || h.c.Writes() != writes || h.mapping("a").State != store.StateUserDeleted {
		t.Fatalf("plan %+v", p.Items)
	}
	p = h.reconcile(ReconcileOptions{Apply: true, Restore: true})
	if p.Count(ActRestore) != 1 || h.c.Object(hrefFor("a")) == nil || h.mapping("a").State != store.StateSynced {
		t.Fatalf("restore %+v", p.Items)
	}
	if rep := h.cycleTwice(); rep.Writes() != 0 {
		t.Fatalf("not in step: %+v", rep)
	}
}

func TestReconcileOrphans(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.g.Put(invite("b"))
	h.cycleTwice()
	loseDB(t, h)
	h.g.Cancel("b") // cancelled while the database was lost: its mirror is an orphan
	p := h.reconcile(ReconcileOptions{Apply: true})
	if p.Count(ActOrphan) != 1 || p.Count(ActAdopt) != 1 || h.c.Object(hrefFor("b")) == nil {
		t.Fatalf("plan %+v", p.Items)
	}
	p = h.reconcile(ReconcileOptions{Apply: true, DeleteOrphans: true})
	if p.Count(ActOrphan) != 1 || h.c.Object(hrefFor("b")) != nil || h.c.Object(hrefFor("a")) == nil {
		t.Fatalf("orphan not deleted: %+v", p.Items)
	}
	if d := h.c.DeleteLog[len(h.c.DeleteLog)-1]; d.IfMatch == "" || d.ScheduleReply != nil {
		t.Errorf("delete opts %+v", d)
	}
	if again := h.reconcile(ReconcileOptions{}); again.Count(ActOrphan) != 0 {
		t.Fatalf("orphan still reported: %+v", again.Items)
	}
}

func TestReconcileOrphanDeleteCap(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 7; i++ {
		h.g.Put(invite(fmt.Sprintf("e%d", i)))
	}
	h.cycleTwice()
	loseDB(t, h)
	for i := 0; i < 7; i++ {
		h.g.Cancel(fmt.Sprintf("e%d", i))
	}
	if _, err := h.e.Reconcile(context.Background(), ReconcileOptions{Apply: true, DeleteOrphans: true}); err == nil {
		t.Fatal("bulk orphan delete not refused")
	}
	if len(h.c.Hrefs()) != 7 {
		t.Fatalf("hrefs %v", h.c.Hrefs())
	}
}

func TestReconcileLeavesOldMirrors(t *testing.T) {
	h := newHarness(t)
	h.e.Opts.Lookback = 365 * 24 * time.Hour
	old := invite("old")
	old.Start = &calendar.EventDateTime{DateTime: "2026-08-01T10:00:00+10:00", TimeZone: sydney}
	old.End = &calendar.EventDateTime{DateTime: "2026-08-01T11:00:00+10:00", TimeZone: sydney}
	h.g.Put(old)
	h.cycleTwice()
	h.e.Opts.Lookback = 30 * 24 * time.Hour
	loseDB(t, h)
	if p := h.reconcile(ReconcileOptions{Apply: true, DeleteOrphans: true}); p.Count(ActOrphan) != 0 || h.c.Object(hrefFor("old")) == nil {
		t.Fatalf("old mirror treated as orphan: %+v", p.Items)
	}
	h.g.Cancel("old")
	if p := h.reconcile(ReconcileOptions{Apply: true, DeleteOrphans: true}); p.Count(ActOrphan) != 0 || h.c.Object(hrefFor("old")) == nil {
		t.Fatalf("ended mirror deleted: %+v", p.Items)
	}
}

func TestReconcileClosesStaleRows(t *testing.T) {
	h := newHarness(t)
	h.g.Put(invite("a"))
	h.cycleTwice()
	// Both sides gone without calbridge seeing either (no cycle in between).
	h.g.Cancel("a")
	h.c.UserDelete(hrefFor("a"))
	p := h.reconcile(ReconcileOptions{Apply: true})
	if p.Count(ActClose) != 1 || h.mapping("a").State != store.StateDeleted || h.c.Writes() != 1 {
		t.Fatalf("plan %+v state %s", p.Items, h.mapping("a").State)
	}
}

func TestReconcileNeverOverwritesAnotherEventsMirror(t *testing.T) {
	h := newHarness(t)
	a := invite("a")
	a.ICalUID = "shared@google.com"
	h.g.Put(a)
	h.cycleTwice()
	b := invite("b")
	b.ICalUID = "shared@google.com"
	h.g.Put(b)
	h.cycleTwice() // b gets its own calbridge UID (§7.1)
	loseDB(t, h)
	raw := string(h.c.Object(h.mappingOrHref(t, "a")).Raw)
	h.reconcile(ReconcileOptions{Apply: true})
	if string(h.c.Object(h.mappingOrHref(t, "a")).Raw) != raw {
		t.Fatal("a's mirror was rewritten")
	}
}

// mappingOrHref returns the deterministic href for an event's iCalUID.
func (h *harness) mappingOrHref(t *testing.T, id string) string {
	t.Helper()
	ev := h.g.Event(id)
	return hrefForUID(ev.ICalUID)
}

// TestReconcileOrphansSharingAnID: Phase 0 left several objects with the
// same X-CALBRIDGE-GID; all of them must be deletable in one run.
func TestReconcileOrphansSharingAnID(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 3; i++ {
		h.c.Import(fmt.Sprintf("%scalbridge-p0-%d.ics", calHref, i), []byte(fmt.Sprintf(
			"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:p0-%d@calbridge.invalid\r\n"+
				"DTSTAMP:20261118T000000Z\r\nDTSTART:20261125T000000Z\r\nDTEND:20261125T010000Z\r\n"+
				"X-CALBRIDGE-GID:phase0-test\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", i)))
	}
	p := h.reconcile(ReconcileOptions{Apply: true, DeleteOrphans: true})
	if p.Count(ActOrphan) != 3 || len(h.c.Hrefs()) != 0 {
		t.Fatalf("plan %+v, left %v", p.Items, h.c.Hrefs())
	}
}

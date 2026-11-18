package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// testClock returns strictly increasing times so stamps are distinguishable.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

func newClock() *testClock {
	return &testClock{t: time.Date(2026, 11, 19, 0, 0, 0, 123456789, time.UTC)}
}

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "calbridge.db")
	s, err := Open(context.Background(), path, Options{Now: newClock().Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func sampleMapping(n int) *Mapping {
	return &Mapping{
		GoogleCalendarID: "primary",
		GoogleEventID:    fmt.Sprintf("evt%d", n),
		ICalUID:          fmt.Sprintf("evt%d@google.com", n),
		ICloudCalendar:   "/123/calendars/test/",
		ICloudHref:       fmt.Sprintf("/123/calendars/test/calbridge-%d.ics", n),
	}
}

func TestOpenCreatesPrivateFileAndMigrates(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "with space", "nested")
	path := filepath.Join(dir, "calbridge.db")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil || v != LatestSchemaVersion() || v < 1 {
		t.Fatalf("SchemaVersion = %d, %v; want %d", v, err, LatestSchemaVersion())
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}
	var fk int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d, %v; want 1", fk, err)
	}
	// Force WAL/SHM creation, then check every file is owner-only.
	if err := s.AppendOp(ctx, &Op{Direction: DirG2I, Op: "skip"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s mode = %v; want no group/other access", filepath.Base(p), perm)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: no migration re-run, data kept.
	s2, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	ops, err := s2.RecentOps(ctx, 10)
	if err != nil || len(ops) != 1 {
		t.Fatalf("RecentOps after reopen = %v, %v; want 1 op", ops, err)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	s, path := openTest(t)
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	_, err := Open(ctx, path, Options{})
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Open = %v; want ErrSchemaTooNew", err)
	}
}

func TestMappingRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	m := sampleMapping(1)
	m.ICloudETag = `"etag-1"`
	m.LastGoogleETag = `"g1"`
	m.LastGoogleUpdated = time.Date(2026, 11, 18, 9, 30, 0, 500, time.UTC)
	m.Sequence = 2
	m.ContentHash = "abc"
	m.GoogleStatus = "needsAction"
	if err := s.CreateMapping(ctx, m); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if m.ID == 0 || m.State != StatePendingPut || m.CreatedAt.IsZero() || !m.CreatedAt.Equal(m.UpdatedAt) {
		t.Fatalf("after create: id=%d state=%s created=%v updated=%v", m.ID, m.State, m.CreatedAt, m.UpdatedAt)
	}
	got, err := s.GetMapping(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(*m, got); diff != "" {
		t.Fatalf("GetMapping mismatch (-want +got):\n%s", diff)
	}

	m.State = StateSynced
	m.ICloudETag = `"etag-2"`
	m.ICloudPartStat = "ACCEPTED"
	m.AgreedStatus = "accepted"
	m.RSVPPending = "accepted"
	if err := s.UpdateMapping(ctx, m); err != nil {
		t.Fatalf("UpdateMapping: %v", err)
	}
	if !m.UpdatedAt.After(m.CreatedAt) {
		t.Errorf("UpdatedAt %v not after CreatedAt %v", m.UpdatedAt, m.CreatedAt)
	}
	for name, get := range map[string]func() (Mapping, error){
		"id":     func() (Mapping, error) { return s.GetMapping(ctx, m.ID) },
		"google": func() (Mapping, error) { return s.GetMappingByGoogleID(ctx, "primary", "evt1") },
		"href":   func() (Mapping, error) { return s.GetMappingByHref(ctx, m.ICloudCalendar, m.ICloudHref) },
		"uid":    func() (Mapping, error) { return s.GetMappingByUID(ctx, m.ICloudCalendar, m.ICalUID) },
	} {
		got, err := get()
		if err != nil {
			t.Fatalf("get by %s: %v", name, err)
		}
		if diff := cmp.Diff(*m, got); diff != "" {
			t.Errorf("get by %s mismatch (-want +got):\n%s", name, diff)
		}
	}

	// Clearing optional fields stores NULL and reads back empty.
	m.RSVPPending = ""
	m.LastGoogleUpdated = time.Time{}
	if err := s.UpdateMapping(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMapping(ctx, m.ID)
	if got.RSVPPending != "" || !got.LastGoogleUpdated.IsZero() {
		t.Errorf("cleared fields read back as %q, %v", got.RSVPPending, got.LastGoogleUpdated)
	}
}

func TestMappingNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if _, err := s.GetMapping(ctx, 42); !IsNotFound(err) {
		t.Errorf("GetMapping = %v; want ErrNotFound", err)
	}
	if _, err := s.GetMappingByGoogleID(ctx, "primary", "nope"); !IsNotFound(err) {
		t.Errorf("GetMappingByGoogleID = %v; want ErrNotFound", err)
	}
	m := sampleMapping(1)
	m.ID = 42
	m.State = StateSynced
	if err := s.UpdateMapping(ctx, m); !IsNotFound(err) {
		t.Errorf("UpdateMapping(missing) = %v; want ErrNotFound", err)
	}
}

func TestMappingUniqueConstraints(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if err := s.CreateMapping(ctx, sampleMapping(1)); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(m *Mapping)
	}{
		{"same google id", func(m *Mapping) { m.GoogleEventID = "evt1" }},
		{"same href", func(m *Mapping) { m.ICloudHref = sampleMapping(1).ICloudHref }},
		{"same uid", func(m *Mapping) { m.ICalUID = sampleMapping(1).ICalUID }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleMapping(2)
			tc.mutate(m)
			if err := s.CreateMapping(ctx, m); !errors.Is(err, ErrConflict) {
				t.Fatalf("CreateMapping = %v; want ErrConflict", err)
			}
		})
	}
	// Same UID in a different calendar is fine.
	m := sampleMapping(3)
	m.ICalUID = sampleMapping(1).ICalUID
	m.ICloudCalendar = "/123/calendars/other/"
	if err := s.CreateMapping(ctx, m); err != nil {
		t.Fatalf("other calendar: %v", err)
	}
}

func TestCreateMappingValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	for _, st := range []State{StateSynced, StatePendingDelete, StateDeleted, StateUserDeleted, "bogus"} {
		m := sampleMapping(1)
		m.State = st
		if err := s.CreateMapping(ctx, m); err == nil {
			t.Errorf("CreateMapping in state %q succeeded; want error", st)
		}
	}
	m := sampleMapping(1)
	m.ICloudHref = ""
	if err := s.CreateMapping(ctx, m); err == nil {
		t.Error("CreateMapping without href succeeded")
	}
	m = sampleMapping(1)
	m.State = StateSkipped
	m.SkipReason = "organizer_self"
	if err := s.CreateMapping(ctx, m); err != nil {
		t.Errorf("CreateMapping as skipped: %v", err)
	}
}

// forceState sets a row's state directly, bypassing validation (test setup).
func forceState(t *testing.T, s *Store, id int64, st State) {
	t.Helper()
	if _, err := s.db.Exec("UPDATE mappings SET state=? WHERE id=?", string(st), id); err != nil {
		t.Fatal(err)
	}
}

func TestStateTransitions(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	m := sampleMapping(1)
	if err := s.CreateMapping(ctx, m); err != nil {
		t.Fatal(err)
	}
	for _, from := range States {
		for _, to := range States {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				forceState(t, s, m.ID, from)
				upd := *m
				upd.State = to
				upd.ContentHash = "changed-" + string(to)
				err := s.UpdateMapping(ctx, &upd)
				got, gerr := s.GetMapping(ctx, m.ID)
				if gerr != nil {
					t.Fatal(gerr)
				}
				if CanTransition(from, to) {
					if err != nil || got.State != to || got.ContentHash != upd.ContentHash {
						t.Fatalf("allowed transition: err=%v state=%s", err, got.State)
					}
					return
				}
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("err = %v; want ErrInvalidTransition", err)
				}
				if got.State != from || got.ContentHash == upd.ContentHash {
					t.Fatalf("rejected transition still wrote the row: state=%s hash=%s", got.State, got.ContentHash)
				}
			})
		}
	}
}

func TestTransitionTable(t *testing.T) {
	// The PLAN.md §6 / §8.5 moves the syncer relies on.
	must := [][2]State{
		{StatePendingPut, StateSynced},
		{StateSynced, StatePendingPut},
		{StateSynced, StatePendingDelete},
		{StatePendingDelete, StateDeleted},
		{StateSynced, StateUserDeleted},
		{StateDeleted, StatePendingPut},
		{StateUserDeleted, StatePendingPut}, // recreate_on_reschedule / reconcile --restore
		{StateSynced, StateSkipped},
	}
	for _, p := range must {
		if !CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s must be allowed", p[0], p[1])
		}
	}
	mustNot := [][2]State{
		{StateUserDeleted, StateSynced}, // never silently resurrect a user deletion
		{StatePendingDelete, StateSynced},
		{StateDeleted, StateSynced},
		{StateSkipped, StateSynced},
		{StatePendingDelete, StatePendingPut},
		{StateSynced, StateDeleted}, // deletion goes through pending_delete (deleteMirror)
		{"bogus", StateSynced},
		{StateSynced, "bogus"},
	}
	for _, p := range mustNot {
		if CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s must be rejected", p[0], p[1])
		}
	}
}

func TestListAndCountMappings(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	states := []State{StatePendingPut, StateSynced, StateSynced, StateUserDeleted}
	for i, st := range states {
		m := sampleMapping(i)
		if err := s.CreateMapping(ctx, m); err != nil {
			t.Fatal(err)
		}
		forceState(t, s, m.ID, st)
	}
	all, err := s.ListMappings(ctx)
	if err != nil || len(all) != 4 {
		t.Fatalf("ListMappings() = %d, %v", len(all), err)
	}
	synced, err := s.ListMappings(ctx, StateSynced, StateUserDeleted)
	if err != nil || len(synced) != 3 {
		t.Fatalf("ListMappings(synced,user_deleted) = %d, %v", len(synced), err)
	}
	if _, err := s.ListMappings(ctx, "bogus"); err == nil {
		t.Error("ListMappings(bogus) succeeded")
	}
	counts, err := s.CountMappingsByState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[State]int{StatePendingPut: 1, StateSynced: 2, StateUserDeleted: 1}
	if diff := cmp.Diff(want, counts); diff != "" {
		t.Errorf("counts (-want +got):\n%s", diff)
	}
}

func TestDeleteMappingRow(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	m := sampleMapping(1)
	if err := s.CreateMapping(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.PutInstanceState(ctx, &InstanceState{MappingID: m.ID, RecurrenceID: "20261120T000000Z"}); err != nil {
		t.Fatal(err)
	}
	for _, st := range []State{StatePendingPut, StateSynced, StatePendingDelete, StateUserDeleted} {
		forceState(t, s, m.ID, st)
		if err := s.DeleteMappingRow(ctx, m.ID); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("DeleteMappingRow in %s = %v; want ErrInvalidTransition", st, err)
		}
	}
	forceState(t, s, m.ID, StateDeleted)
	if err := s.DeleteMappingRow(ctx, m.ID); err != nil {
		t.Fatalf("DeleteMappingRow(deleted): %v", err)
	}
	if _, err := s.GetMapping(ctx, m.ID); !IsNotFound(err) {
		t.Errorf("row still present: %v", err)
	}
	is, err := s.ListInstanceStates(ctx, m.ID)
	if err != nil || len(is) != 0 {
		t.Errorf("instance_state not cascaded: %v, %v", is, err)
	}
	if err := s.DeleteMappingRow(ctx, m.ID); !IsNotFound(err) {
		t.Errorf("second DeleteMappingRow = %v; want ErrNotFound", err)
	}
}

func TestSyncState(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if _, err := s.GetSyncState(ctx, "google:primary"); !IsNotFound(err) {
		t.Fatalf("GetSyncState(empty) = %v; want ErrNotFound", err)
	}
	full := time.Date(2026, 11, 19, 1, 2, 3, 0, time.UTC)
	st := &SyncState{Key: "google:primary", Token: "tok1", LastFullSync: full, LastSuccess: full}
	if err := s.PutSyncState(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSyncState(ctx, "google:primary")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(*st, got); diff != "" {
		t.Errorf("round trip (-want +got):\n%s", diff)
	}
	st.Token, st.CTag = "tok2", "ctag"
	if err := s.PutSyncState(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSyncState(ctx, "google:primary")
	if got.Token != "tok2" || got.CTag != "ctag" || !got.LastFullSync.Equal(full) {
		t.Errorf("upsert = %+v", got)
	}
	if err := s.DeleteSyncState(ctx, "google:primary"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSyncState(ctx, "google:primary"); !IsNotFound(err) {
		t.Errorf("after delete: %v", err)
	}
	if err := s.DeleteSyncState(ctx, "google:primary"); err != nil {
		t.Errorf("deleting a missing key: %v", err)
	}
	if err := s.PutSyncState(ctx, &SyncState{}); err == nil {
		t.Error("PutSyncState with empty key succeeded")
	}
}

func TestGoogleEvents(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	upd := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	master := &GoogleEvent{CalendarID: "primary", EventID: "ser", ICalUID: "ser@google.com", Status: "confirmed",
		ETag: `"1"`, Updated: upd, JSON: []byte(`{"id":"ser"}`)}
	exc2 := &GoogleEvent{CalendarID: "primary", EventID: "ser_20261127", SeriesID: "ser", ICalUID: "ser@google.com",
		Status: "confirmed", OriginalStart: "2026-11-27T10:00:00+11:00", JSON: []byte(`{"id":"ser_20261127"}`)}
	exc1 := &GoogleEvent{CalendarID: "primary", EventID: "ser_20261120", SeriesID: "ser", ICalUID: "ser@google.com",
		Status: "confirmed", OriginalStart: "2026-11-20T10:00:00+11:00", JSON: []byte(`{"id":"ser_20261120"}`)}
	other := &GoogleEvent{CalendarID: "primary", EventID: "solo", Status: "confirmed", JSON: []byte(`{}`)}
	for _, e := range []*GoogleEvent{exc2, master, other, exc1} {
		if err := s.PutGoogleEvent(ctx, e); err != nil {
			t.Fatalf("PutGoogleEvent(%s): %v", e.EventID, err)
		}
	}
	if master.SeriesID != "ser" || master.SeenAt.IsZero() {
		t.Errorf("master defaults: series=%q seen=%v", master.SeriesID, master.SeenAt)
	}
	got, err := s.GetGoogleEvent(ctx, "primary", "ser")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(*master, got); diff != "" {
		t.Errorf("round trip (-want +got):\n%s", diff)
	}
	series, err := s.ListSeries(ctx, "primary", "ser")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range series {
		ids = append(ids, e.EventID)
	}
	if diff := cmp.Diff([]string{"ser", "ser_20261120", "ser_20261127"}, ids); diff != "" {
		t.Errorf("ListSeries order (-want +got):\n%s", diff)
	}

	// A bare tombstone (P0-1: only id/status/etag) keeps what we already knew.
	tomb := &GoogleEvent{CalendarID: "primary", EventID: "ser_20261120", Status: "cancelled", ETag: `"9"`,
		JSON: []byte(`{"id":"ser_20261120","status":"cancelled"}`)}
	if err := s.PutGoogleEvent(ctx, tomb); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetGoogleEvent(ctx, "primary", "ser_20261120")
	if got.Status != "cancelled" || got.SeriesID != "ser" || got.ICalUID != "ser@google.com" ||
		got.OriginalStart != exc1.OriginalStart || got.ETag != `"9"` {
		t.Errorf("tombstone merge = %+v", got)
	}
	// A tombstone for an id never seen is stored as its own series.
	unknown := &GoogleEvent{CalendarID: "primary", EventID: "ghost", Status: "cancelled", JSON: []byte(`{}`)}
	if err := s.PutGoogleEvent(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetGoogleEvent(ctx, "primary", "ghost"); got.SeriesID != "ghost" {
		t.Errorf("unknown tombstone series = %q", got.SeriesID)
	}

	if err := s.DeleteGoogleEvent(ctx, "primary", "solo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGoogleEvent(ctx, "primary", "solo"); !IsNotFound(err) {
		t.Errorf("after delete: %v", err)
	}
	if err := s.PutGoogleEvent(ctx, &GoogleEvent{CalendarID: "primary", EventID: "x", Status: "confirmed"}); err == nil {
		t.Error("PutGoogleEvent without JSON succeeded")
	}
}

func TestInstanceState(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	m := sampleMapping(1)
	if err := s.CreateMapping(ctx, m); err != nil {
		t.Fatal(err)
	}
	a := &InstanceState{MappingID: m.ID, RecurrenceID: "20261127T000000Z", GoogleEventID: "evt1_20261127",
		GoogleStatus: "needsAction", ICloudPartStat: "ACCEPTED", RSVPPending: "accepted"}
	b := &InstanceState{MappingID: m.ID, RecurrenceID: "20261120T000000Z", GoogleStatus: "accepted"}
	for _, x := range []*InstanceState{a, b} {
		if err := s.PutInstanceState(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	a.RSVPPending, a.AgreedStatus, a.GoogleStatus = "", "accepted", "accepted"
	if err := s.PutInstanceState(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListInstanceStates(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]InstanceState{*b, *a}, got); diff != "" {
		t.Errorf("ListInstanceStates (-want +got):\n%s", diff)
	}
	if err := s.DeleteInstanceState(ctx, m.ID, b.RecurrenceID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListInstanceStates(ctx, m.ID); len(got) != 1 {
		t.Errorf("after delete: %d rows", len(got))
	}
	// Foreign key: unknown mapping is rejected.
	if err := s.PutInstanceState(ctx, &InstanceState{MappingID: 999, RecurrenceID: "x"}); err == nil {
		t.Error("instance state for unknown mapping succeeded")
	}
}

func TestOpsLog(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	var first *Op
	for i := 0; i < 5; i++ {
		o := &Op{Direction: DirG2I, Op: "put_create", MappingID: int64(i), Detail: fmt.Sprintf("n=%d", i)}
		if err := s.AppendOp(ctx, o); err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = o
		}
	}
	ops, err := s.RecentOps(ctx, 3)
	if err != nil || len(ops) != 3 || ops[0].Detail != "n=4" || ops[2].Detail != "n=2" {
		t.Fatalf("RecentOps = %+v, %v", ops, err)
	}
	if ops[2].MappingID != 2 {
		t.Errorf("mapping id = %d", ops[2].MappingID)
	}
	all, _ := s.RecentOps(ctx, 10)
	if all[4].MappingID != 0 || all[4].ID != first.ID {
		t.Errorf("zero mapping id round trip: %+v", all[4])
	}
	if err := s.AppendOp(ctx, &Op{Direction: "sideways", Op: "x"}); err == nil {
		t.Error("invalid direction accepted")
	}
	// Prune everything before the third entry.
	n, err := s.PruneOps(ctx, all[2].At)
	if err != nil || n != 2 {
		t.Fatalf("PruneOps = %d, %v; want 2", n, err)
	}
	if rest, _ := s.RecentOps(ctx, 10); len(rest) != 3 {
		t.Errorf("after prune: %d", len(rest))
	}
}

func TestTxRollback(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	boom := errors.New("boom")
	err := s.Tx(ctx, func(tx *Tx) error {
		if err := tx.CreateMapping(ctx, sampleMapping(1)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx = %v", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic not propagated")
			}
		}()
		_ = s.Tx(ctx, func(tx *Tx) error {
			_ = tx.CreateMapping(ctx, sampleMapping(2))
			panic("crash after write")
		})
	}()
	if all, _ := s.ListMappings(ctx); len(all) != 0 {
		t.Fatalf("rolled-back rows visible: %d", len(all))
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.CreateMapping(ctx, sampleMapping(3)) }); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.ListMappings(ctx); len(all) != 1 {
		t.Fatalf("committed rows = %d", len(all))
	}
}

// TestConcurrentReadDuringWrite: with WAL, readers (same handle or another
// process-like handle) see the last committed state without blocking while a
// write transaction is open.
func TestConcurrentReadDuringWrite(t *testing.T) {
	ctx := context.Background()
	s, path := openTest(t)
	m := sampleMapping(1)
	m.ContentHash = "v1"
	if err := s.CreateMapping(ctx, m); err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()

	inTx := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Tx(ctx, func(tx *Tx) error {
			upd := *m
			upd.ContentHash = "v2"
			if err := tx.UpdateMapping(ctx, &upd); err != nil {
				return err
			}
			close(inTx)
			<-release
			return nil
		})
	}()
	<-inTx

	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for name, st := range map[string]*Store{"same handle": s, "other handle": other} {
		got, err := st.GetMapping(rctx, m.ID)
		if err != nil {
			t.Fatalf("%s: read during write: %v", name, err)
		}
		if got.ContentHash != "v1" {
			t.Errorf("%s: read uncommitted value %q", name, got.ContentHash)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := other.GetMapping(ctx, m.ID)
	if err != nil || got.ContentHash != "v2" {
		t.Fatalf("after commit: %q, %v", got.ContentHash, err)
	}
}

// TestConcurrentWriters: BEGIN IMMEDIATE + busy_timeout serialises writers
// from two handles instead of failing with SQLITE_BUSY.
func TestConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	s, path := openTest(t)
	other, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	const perWriter = 25
	var wg sync.WaitGroup
	errs := make(chan error, 2*perWriter)
	for _, st := range []*Store{s, other} {
		wg.Add(1)
		go func(st *Store) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				errs <- st.Tx(ctx, func(tx *Tx) error {
					return tx.AppendOp(ctx, &Op{Direction: DirI2G, Op: "patch_rsvp"})
				})
			}
		}(st)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("writer: %v", err)
		}
	}
	ops, err := s.RecentOps(ctx, 1000)
	if err != nil || len(ops) != 2*perWriter {
		t.Fatalf("ops = %d, %v; want %d", len(ops), err, 2*perWriter)
	}
}

func TestTimestampsSortLexically(t *testing.T) {
	a := fmtTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).String
	b := fmtTime(time.Date(2026, 1, 1, 0, 0, 0, 500, time.UTC)).String
	c := fmtTime(time.Date(2026, 1, 1, 0, 0, 1, 0, time.FixedZone("x", 3600))).String // 23:00:01 the day before
	if c >= a || a >= b {
		t.Errorf("not lexically ordered: %s %s %s", c, a, b)
	}
}

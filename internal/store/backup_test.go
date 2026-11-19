package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupIsReadableAndPruned(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	m := sampleMapping(1)
	if err := s.CreateMapping(ctx, m); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backups")
	day := time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)

	path, created, err := s.Backup(ctx, dir, day, 3)
	if err != nil || !created || filepath.Base(path) != "calbridge-20261119.db" {
		t.Fatalf("Backup = %s %v %v", path, created, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v %v", fi.Mode(), err)
	}
	// The snapshot is a complete, openable database.
	b, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.GetMappingByGoogleID(ctx, m.GoogleCalendarID, m.GoogleEventID)
	_ = b.Close()
	if err != nil || got.ICloudHref != m.ICloudHref {
		t.Fatalf("backup content: %+v %v", got, err)
	}

	// Same day: kept, not rewritten.
	if _, created, err := s.Backup(ctx, dir, day.Add(5*time.Hour), 3); err != nil || created {
		t.Fatalf("second backup same day: created=%v %v", created, err)
	}
	// Keep the newest 3.
	for i := 1; i <= 4; i++ {
		if _, _, err := s.Backup(ctx, dir, day.AddDate(0, 0, i), 3); err != nil {
			t.Fatal(err)
		}
	}
	all, err := Backups(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || filepath.Base(all[0]) != "calbridge-20261123.db" || filepath.Base(all[2]) != "calbridge-20261121.db" {
		t.Fatalf("after pruning: %v", all)
	}
	if _, _, err := s.Backup(ctx, "", day, 3); err == nil {
		t.Error("empty dir accepted")
	}
}

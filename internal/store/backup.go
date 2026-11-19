package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupPrefix and BackupSuffix frame backup file names: calbridge-YYYYMMDD.db.
const (
	BackupPrefix = "calbridge-"
	BackupSuffix = ".db"
)

// Backup writes a consistent snapshot of the database to
// dir/calbridge-YYYYMMDD.db with VACUUM INTO (§13), mode 0600, and prunes
// backups beyond keep (newest kept). A backup for the same day is left as is
// and its path returned with created=false.
func (s *Store) Backup(ctx context.Context, dir string, now time.Time, keep int) (path string, created bool, err error) {
	if dir == "" {
		return "", false, fmt.Errorf("store: backup dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, fmt.Errorf("store: backup dir: %w", err)
	}
	path = filepath.Join(dir, BackupPrefix+now.UTC().Format("20060102")+BackupSuffix)
	if _, err := os.Stat(path); err == nil {
		return path, false, s.pruneBackups(dir, keep)
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		_ = os.Remove(tmp)
		return "", false, fmt.Errorf("store: backup: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return "", false, fmt.Errorf("store: backup: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", false, fmt.Errorf("store: backup: %w", err)
	}
	return path, true, s.pruneBackups(dir, keep)
}

// Backups lists backup files in dir, newest first.
func Backups(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(n, BackupPrefix) && strings.HasSuffix(n, BackupSuffix) {
			out = append(out, filepath.Join(dir, n))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out))) // names sort by date
	return out, nil
}

func (s *Store) pruneBackups(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	all, err := Backups(dir)
	if err != nil {
		return err
	}
	for _, old := range all[min(keep, len(all)):] {
		if err := os.Remove(old); err != nil {
			return fmt.Errorf("store: prune backup: %w", err)
		}
	}
	return nil
}

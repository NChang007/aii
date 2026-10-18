package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/store"
)

// cmdBackup writes a compacted snapshot of the index and keeps the newest
// few. The index outlives the transcripts it was built from, so it may be
// the only copy of old sessions.
func cmdBackup(args []string) error {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	dir := fs.String("dir", filepath.Join(home, "Backups", "aii"), "folder for snapshots")
	keep := fs.Int("keep", 2, "snapshots to keep; older ones in --dir are removed")
	fs.Parse(reorderFlags(args))
	if *keep < 1 {
		return fmt.Errorf("--keep must be at least 1")
	}

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()

	path, err := backupDB(db, *dir, *keep, time.Now())
	if err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil {
		fmt.Printf("%s (%.1f MB)\n", path, float64(info.Size())/(1<<20))
	}
	return nil
}

// backupDB writes dir/aii-<UTC timestamp>.db with VACUUM INTO, which
// produces a consistent, compacted copy while other processes keep
// writing, then removes all but the newest keep snapshots in dir. Only
// files named aii-*.db are ever removed.
func backupDB(db *store.DB, dir string, keep int, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "aii-"+now.UTC().Format("20060102-150405")+".db")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists", path)
	}
	tmp := path + ".partial"
	os.Remove(tmp)
	if _, err := db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("snapshot: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}

	snaps, err := filepath.Glob(filepath.Join(dir, "aii-*.db"))
	if err != nil {
		return "", err
	}
	// The timestamped names sort chronologically.
	sort.Strings(snaps)
	for _, old := range snaps[:max(0, len(snaps)-keep)] {
		if strings.HasSuffix(old, ".db") {
			if err := os.Remove(old); err != nil {
				return "", err
			}
		}
	}
	return path, nil
}

package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	keep := fs.Int("keep", 2, "snapshots to keep; older aii snapshots in --dir are removed")
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

// snapshotName matches the files backupDB writes and is allowed to prune.
// The UTC timestamp makes name order chronological.
var snapshotName = regexp.MustCompile(`^aii-\d{8}-\d{6}\.db$`)

// backupDB writes dir/aii-<UTC timestamp>.db with VACUUM INTO, which
// produces a consistent, compacted copy while other processes keep
// writing. It checks the copy opens and passes quick_check before
// removing older snapshots, so a bad copy never displaces good ones, and
// keeps the newest keep. Only files named exactly like its own snapshots
// (or their leftover .partial files) are ever removed.
func backupDB(db *store.DB, dir string, keep int, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// Leftovers from a run that was killed mid-write.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if n := e.Name(); len(n) > 8 && n[len(n)-8:] == ".partial" && snapshotName.MatchString(n[:len(n)-8]) {
			os.Remove(filepath.Join(dir, n))
		}
	}

	name := "aii-" + now.UTC().Format("20060102-150405") + ".db"
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists", path)
	}
	// O_EXCL makes a second run in the same second fail fast instead of
	// sharing the temp name. VACUUM INTO needs the file absent, so the
	// claim is released just before it writes: this narrows, not closes,
	// the window (runs are weekly, so a same-second overlap is unlikely).
	tmp := path + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("another backup is running: %w", err)
	}
	f.Close()
	os.Remove(tmp)
	if _, err := db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("snapshot: %w", err)
	}
	if err := checkSnapshot(tmp); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("snapshot failed its check, older snapshots kept: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}

	entries, err = os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var snaps []string
	for _, e := range entries {
		if snapshotName.MatchString(e.Name()) {
			snaps = append(snaps, e.Name())
		}
	}
	sort.Strings(snaps)
	for _, old := range snaps[:max(0, len(snaps)-keep)] {
		if err := os.Remove(filepath.Join(dir, old)); err != nil {
			return "", err
		}
	}
	return path, nil
}

// checkSnapshot opens a snapshot and runs PRAGMA quick_check. A plain
// path, not a file: URI, so names with ? or # open correctly.
func checkSnapshot(path string) error {
	snap, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer snap.Close()
	var res string
	if err := snap.QueryRow(`PRAGMA quick_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("quick_check: %s", res)
	}
	return nil
}

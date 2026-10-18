package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ericmason/aii/internal/store"
)

func TestBackupKeepsNewestSnapshots(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.UpsertSession(&store.Session{Agent: "claude_code", UID: "s1", SourcePath: "/f"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backups")
	// Unrelated files in the folder are never touched.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"notes.txt", "aii-manual.db"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A leftover from a killed run is cleaned up.
	if err := os.WriteFile(filepath.Join(dir, "aii-20260901-030000.db.partial"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	var last string
	for i := 0; i < 3; i++ {
		last, err = backupDB(db, dir, 2, base.Add(time.Duration(i)*7*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
	}
	snaps, _ := filepath.Glob(filepath.Join(dir, "aii-2*.db"))
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %v, want the newest 2", snaps)
	}
	if filepath.Base(last) != "aii-20261015-030000.db" {
		t.Errorf("latest = %s", filepath.Base(last))
	}
	if _, err := os.Stat(filepath.Join(dir, "aii-20261001-030000.db")); !os.IsNotExist(err) {
		t.Errorf("oldest snapshot still present (err %v)", err)
	}
	for _, f := range []string{"notes.txt", "aii-manual.db"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("unrelated file %s removed: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "aii-20260901-030000.db.partial")); !os.IsNotExist(err) {
		t.Errorf("leftover .partial not cleaned (err %v)", err)
	}

	snap, err := store.Open(last)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if s, err := snap.SessionByUID("claude_code", "s1"); err != nil || s == nil {
		t.Errorf("snapshot is missing data: %v, %v", s, err)
	}
}

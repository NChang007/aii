// Package indexer orchestrates Source parsers into a single SQLite writer.
package indexer

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ericmason/aii/internal/redact"
	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

type Options struct {
	Full    bool
	Verbose bool
	// Redact controls whether message content is scrubbed of known
	// secret shapes on its way into the DB. Defaults to true; callers
	// opt out explicitly with --no-redact.
	Redact bool
}

type Stats struct {
	Sources        int
	SessionsTouch  int
	MessagesAdded  int
	Errors         int
	Duration       time.Duration
}

// Run launches each Source in its own goroutine, funnels batches into
// the single SQLite writer, and returns aggregate stats.
func Run(ctx context.Context, db *store.DB, sources []source.Source, opt Options) (Stats, error) {
	start := time.Now()
	stats := Stats{Sources: len(sources)}

	ch := make(chan source.Batch, 32)
	var wg sync.WaitGroup

	for _, s := range sources {
		wg.Add(1)
		go func(s source.Source) {
			defer wg.Done()
			if opt.Verbose {
				log.Printf("[%s] scanning", s.Name())
			}
			if err := s.Scan(ctx, db, opt.Full, ch); err != nil {
				log.Printf("[%s] scan error: %v", s.Name(), err)
				stats.Errors++
			}
			if opt.Verbose {
				log.Printf("[%s] scan complete", s.Name())
			}
		}(s)
	}

	go func() { wg.Wait(); close(ch) }()

	for b := range ch {
		if opt.Redact {
			redactBatch(&b)
		}
		if err := apply(db, b, opt.Verbose); err != nil {
			log.Printf("apply batch for %s: %v", b.State.SourcePath, err)
			stats.Errors++
			continue
		}
		stats.SessionsTouch++
		stats.MessagesAdded += len(b.Messages)
	}

	stats.Duration = time.Since(start)
	return stats, nil
}

func apply(db *store.DB, b source.Batch, verbose bool) error {
	// Indexing never loses history: a truncating batch (a --full re-read,
	// or a source that shrank or was rewritten) is reconciled with what is
	// stored for that session instead of deleting the file's sessions. The
	// index outlives the transcripts it was built from.
	replace, renumbered := false, false
	if b.Truncate && b.Session.UID != "" {
		var err error
		b.Messages, replace, renumbered, err = planTruncated(db, b)
		if err != nil {
			return fmt.Errorf("merge re-read: %w", err)
		}
	}

	// Some batches carry only a state update (e.g. codex history scan
	// advanced the file offset but produced no new sessions).
	if b.Session.UID == "" && len(b.Messages) == 0 {
		return db.SaveState(b.State)
	}

	id, err := db.UpsertSession(&b.Session)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}

	// Replacing a session's messages happens in the same transaction as
	// the insert, so a failure can't leave the session emptied.
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if replace {
		if err := db.DeleteSessionMessages(tx, id); err != nil {
			tx.Rollback()
			return fmt.Errorf("replace messages: %w", err)
		}
		// Task lines point at message ordinals; if those moved, drop the
		// tasks so the next digest rebuilds them against the new ones.
		if renumbered {
			if err := db.ResetTasks(tx, id); err != nil {
				tx.Rollback()
				return fmt.Errorf("reset tasks: %w", err)
			}
		}
	}
	if err := db.InsertMessages(tx, id, b.Messages); err != nil {
		tx.Rollback()
		return fmt.Errorf("insert messages: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if err := db.SaveState(b.State); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	if verbose {
		log.Printf("[%s] %s: +%d msgs (workspace=%s)",
			b.Session.Agent, shortUID(b.Session.UID), len(b.Messages), b.Session.Workspace)
	}
	return nil
}

// planTruncated reconciles a re-read of a whole session with the rows
// already stored for it. It returns the messages to insert, whether they
// replace the stored ones, and whether that replacement moves any stored
// message to a different ordinal.
//
// When every stored message is still in the re-read (counting repeats,
// ignoring role; a message that was still being written counts if the
// re-read has it grown, e.g. "Answ" -> "Answer"), the re-read is the
// source's current, complete version: it replaces the stored rows, so parser fixes and new turns
// apply and nothing is duplicated. Otherwise the source lost or changed
// content (an edit, a regenerated reply, a shrunk or rewritten file):
// every stored row stays, and only re-read messages not already stored
// are appended after them.
func planTruncated(db *store.DB, b source.Batch) (msgs []store.Message, replace, renumbered bool, err error) {
	prior, err := db.SessionByUID(b.Session.Agent, b.Session.UID)
	if err != nil || prior == nil {
		return b.Messages, false, false, err
	}
	stored, err := db.SessionMessages(prior.ID)
	if err != nil {
		return nil, false, false, err
	}
	if len(stored) == 0 {
		return b.Messages, false, false, nil
	}

	if covers(b.Messages, stored) {
		at := make(map[int]string, len(b.Messages))
		for _, m := range b.Messages {
			at[m.Ordinal] = m.Content
		}
		for _, r := range stored {
			if at[r.Ordinal] != r.Content {
				renumbered = true
				break
			}
		}
		return b.Messages, true, renumbered, nil
	}

	have := make(map[string]int, len(stored))
	next := 0
	for _, r := range stored {
		have[r.Content]++
		if r.Ordinal >= next {
			next = r.Ordinal + 1
		}
	}
	var out []store.Message
	for _, m := range b.Messages {
		if have[m.Content] > 0 {
			have[m.Content]--
			continue
		}
		m.Ordinal = next
		next++
		out = append(out, m)
	}
	return out, false, false, nil
}

// covers reports whether every stored message appears in the re-read,
// each re-read message accounting for at most one stored one. A stored
// message also matches a re-read message that it is a prefix of, or that
// is a prefix of it: a turn that was mid-write at the last run ("Answ" ->
// "Answer"), or one that had the start of the next record glued on.
func covers(reread []store.Message, stored []store.Row) bool {
	left := make(map[string]int, len(reread))
	for _, m := range reread {
		left[m.Content]++
	}
	var partial []string
	for _, r := range stored {
		if left[r.Content] > 0 {
			left[r.Content]--
			continue
		}
		partial = append(partial, r.Content)
	}
	for _, p := range partial {
		found := false
		for c, n := range left {
			if n > 0 && c != p && extends(c, p) {
				left[c]--
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// extends reports whether one of a and b is the other grown or cut:
// the shorter, without trailing whitespace and sentence punctuation, is
// a non-empty prefix of the longer ("Done." -> "Done, and tested.").
func extends(a, b string) bool {
	if len(a) < len(b) {
		a, b = b, a
	}
	b = strings.TrimRight(b, " \t\r\n.…")
	return b != "" && strings.HasPrefix(a, b)
}

// redactBatch scrubs secrets out of every user-visible text field before
// it hits the DB. Session metadata (title/summary) is covered too, since
// a pasted secret can end up there via the auto-derived title.
func redactBatch(b *source.Batch) {
	b.Session.Title = redact.Redact(b.Session.Title)
	b.Session.Summary = redact.Redact(b.Session.Summary)
	for i := range b.Messages {
		b.Messages[i].Content = redact.Redact(b.Messages[i].Content)
	}
}

func shortUID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

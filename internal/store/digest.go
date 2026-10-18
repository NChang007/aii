package store

import "time"

// maxDigestAttempts is how many failed digests a session gets before
// SessionsNeedingTasks stops offering it.
const maxDigestAttempts = 3

// SetSummary stores a session's summary. Later index runs keep it:
// UpsertSession only overwrites summary with a non-empty value.
func (d *DB) SetSummary(sessionID int64, summary string) error {
	_, err := d.Exec(`UPDATE sessions SET summary = ? WHERE id = ?`, summary, sessionID)
	return err
}

// RecordDigestFailure counts a failed digest attempt for a session.
func (d *DB) RecordDigestFailure(sessionID int64) error {
	_, err := d.Exec(`INSERT INTO digest_attempts (session_id, attempts, last_at) VALUES (?, 1, ?)
	                  ON CONFLICT(session_id) DO UPDATE SET attempts = attempts + 1, last_at = excluded.last_at`,
		sessionID, time.Now().Unix())
	return err
}

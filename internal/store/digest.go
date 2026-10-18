package store

import (
	"strings"
	"time"
)

// maxDigestAttempts is how many failed summaries a session gets before
// SessionsNeedingSummary stops offering it.
const maxDigestAttempts = 3

// SummaryFilter selects sessions that `aii digest` should summarise.
type SummaryFilter struct {
	Agent       string // optional: restrict to one agent
	SinceUnix   int64  // optional: sessions started at or after this time
	EndedBefore int64  // skip sessions still being written after this time
	MinMessages int    // skip sessions too short to be worth a summary
	Limit       int
}

// SessionsNeedingSummary returns finished sessions with no summary yet,
// newest first.
func (d *DB) SessionsNeedingSummary(f SummaryFilter) ([]*Session, error) {
	where := []string{
		`COALESCE(s.summary, '') = ''`,
		`COALESCE(s.ended_at, 0) < ?`,
		// Only prose counts: digest sends nothing else to the model.
		`(SELECT COUNT(*) FROM messages m WHERE m.session_id = s.id AND m.role IN ('user','assistant','agent')) >= ?`,
		`COALESCE((SELECT attempts FROM digest_attempts a WHERE a.session_id = s.id), 0) < ?`,
	}
	args := []any{f.EndedBefore, f.MinMessages, maxDigestAttempts}
	if f.Agent != "" {
		where = append(where, `s.agent = ?`)
		args = append(args, f.Agent)
	}
	if f.SinceUnix > 0 {
		where = append(where, `COALESCE(s.started_at, 0) >= ?`)
		args = append(args, f.SinceUnix)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	args = append(args, limit)

	rows, err := d.Query(`SELECT s.id, s.agent, s.session_uid, COALESCE(s.workspace,''), COALESCE(s.title,''),
	                             COALESCE(s.started_at,0), COALESCE(s.ended_at,0)
	                        FROM sessions s
	                       WHERE `+strings.Join(where, " AND ")+`
	                       ORDER BY COALESCE(s.ended_at, 0) DESC
	                       LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		s := &Session{}
		if err := rows.Scan(&s.ID, &s.Agent, &s.UID, &s.Workspace, &s.Title, &s.StartedAt, &s.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetSummary stores a session's summary. Later index runs keep it:
// UpsertSession only overwrites summary with a non-empty value.
func (d *DB) SetSummary(sessionID int64, summary string) error {
	_, err := d.Exec(`UPDATE sessions SET summary = ? WHERE id = ?`, summary, sessionID)
	return err
}

// RecordDigestFailure counts a failed summary attempt for a session.
func (d *DB) RecordDigestFailure(sessionID int64) error {
	_, err := d.Exec(`INSERT INTO digest_attempts (session_id, attempts, last_at) VALUES (?, 1, ?)
	                  ON CONFLICT(session_id) DO UPDATE SET attempts = attempts + 1, last_at = excluded.last_at`,
		sessionID, time.Now().Unix())
	return err
}

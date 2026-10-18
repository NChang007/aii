package store

import (
	"fmt"
	"strings"
	"time"
)

// Task is one task handled in a session, anchored to the message where it
// starts.
type Task struct {
	Ordinal int
	Text    string
}

// TaskFilter selects sessions whose tasks `aii digest` should record.
type TaskFilter struct {
	Agent       string
	SinceUnix   int64
	EndedBefore int64 // skip sessions still being written after this time
	MinMessages int   // prose messages a never-digested session needs
	UID         string // optional: only this session (still only if it needs tasks)
	Limit       int
}

// TaskSession is a session with messages not yet covered by its tasks.
type TaskSession struct {
	Session
	// TasksThrough is the highest ordinal already covered, -1 if none.
	TasksThrough int
}

// minNewMessages is how many new prose messages a resumed session needs
// before its tasks and summary are brought up to date.
const minNewMessages = 2

// SessionsNeedingTasks returns finished sessions with prose messages past
// their recorded tasks: never-digested ones with at least MinMessages,
// and resumed ones with at least minNewMessages new. Newest first.
func (d *DB) SessionsNeedingTasks(f TaskFilter) ([]*TaskSession, error) {
	const prose = `m.role IN ('user','assistant','agent')`
	where := []string{
		`COALESCE(s.ended_at, 0) < ?`,
		`COALESCE((SELECT attempts FROM digest_attempts a WHERE a.session_id = s.id), 0) < ?`,
		`(SELECT COUNT(*) FROM messages m WHERE m.session_id = s.id AND ` + prose + `
		    AND m.ordinal > COALESCE(s.tasks_through, -1))
		   >= CASE WHEN s.tasks_through IS NULL THEN ? ELSE ? END`,
	}
	args := []any{f.EndedBefore, maxDigestAttempts, f.MinMessages, minNewMessages}
	if f.Agent != "" {
		where = append(where, `s.agent = ?`)
		args = append(args, f.Agent)
	}
	if f.SinceUnix > 0 {
		where = append(where, `COALESCE(s.started_at, 0) >= ?`)
		args = append(args, f.SinceUnix)
	}
	if f.UID != "" {
		where = append(where, `s.session_uid = ?`)
		args = append(args, f.UID)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	args = append(args, limit)
	rows, err := d.Query(`SELECT s.id, s.agent, s.session_uid, COALESCE(s.workspace,''), COALESCE(s.title,''),
	                             COALESCE(s.summary,''), COALESCE(s.started_at,0), COALESCE(s.ended_at,0),
	                             COALESCE(s.tasks_through, -1)
	                        FROM sessions s
	                       WHERE `+strings.Join(where, " AND ")+`
	                       ORDER BY COALESCE(s.ended_at, 0) DESC
	                       LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TaskSession
	for rows.Next() {
		ts := &TaskSession{}
		if err := rows.Scan(&ts.ID, &ts.Agent, &ts.UID, &ts.Workspace, &ts.Title, &ts.Summary,
			&ts.StartedAt, &ts.EndedAt, &ts.TasksThrough); err != nil {
			return nil, err
		}
		out = append(out, ts)
	}
	return out, rows.Err()
}

// SaveTasks adds tasks to a session, records that its messages through
// ordinal `through` are covered, and replaces its summary when one is
// given, all in one transaction. A task at an ordinal that already has
// one replaces it.
func (d *DB) SaveTasks(sessionID int64, tasks []Task, through int, summary string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range tasks {
		if _, err := tx.Exec(`INSERT INTO session_tasks (session_id, ordinal, task) VALUES (?, ?, ?)
		                      ON CONFLICT(session_id, ordinal) DO UPDATE SET task = excluded.task`,
			sessionID, t.Ordinal, t.Text); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE sessions SET tasks_through = ? WHERE id = ?`, through, sessionID); err != nil {
		return err
	}
	if summary != "" {
		if _, err := tx.Exec(`UPDATE sessions SET summary = ? WHERE id = ?`, summary, sessionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SessionTasks returns a session's tasks in order.
func (d *DB) SessionTasks(sessionID int64) ([]Task, error) {
	rows, err := d.Query(`SELECT ordinal, task FROM session_tasks WHERE session_id = ? ORDER BY ordinal`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.Ordinal, &t.Text); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TaskQuery searches task lines. An empty Text lists the most recent.
type TaskQuery struct {
	Text      string
	Agent     string
	Workspace string
	SinceUnix int64
	Sort      string // mixed (default) | relevance | recent, as in Query
	Limit     int
	NowUnix   int64
}

// TaskHit is a matching task with its session.
type TaskHit struct {
	SessionUID string `json:"session_uid"`
	Agent      string `json:"agent"`
	Workspace  string `json:"workspace,omitempty"`
	Title      string `json:"title,omitempty"`
	ParentUID  string `json:"parent_uid,omitempty"`
	Ordinal    int    `json:"ordinal"`
	Text       string `json:"task"`
	StartedAt  int64  `json:"started_at"`
	EndedAt    int64  `json:"ended_at"`
}

// SearchTasks returns tasks matching q.Text, ranked like Search (bm25
// weighted toward recent sessions by default), or the most recent tasks
// when q.Text is empty.
func (d *DB) SearchTasks(q TaskQuery) ([]TaskHit, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	now := q.NowUnix
	if now == 0 {
		now = time.Now().Unix()
	}

	from := `session_tasks t JOIN sessions s2 ON s2.id = t.session_id`
	order := `COALESCE(s2.ended_at, s2.started_at, 0) DESC, t.session_id, t.ordinal DESC`
	var where []string
	var whereArgs, orderArgs []any
	if strings.TrimSpace(q.Text) != "" {
		match := normalizeMatch(q.Text)
		if match == "" {
			return nil, nil
		}
		from = `session_tasks_fts JOIN session_tasks t ON t.id = session_tasks_fts.rowid
		        JOIN sessions s2 ON s2.id = t.session_id`
		where = append(where, `session_tasks_fts MATCH ?`)
		whereArgs = append(whereArgs, match)
		key, keyArgs, err := sortKeySQL(q.Sort, now)
		if err != nil {
			return nil, err
		}
		// sortKeySQL scores a session by ss.best_score; a task by its own bm25.
		key = strings.ReplaceAll(key, "ss.best_score", "bm25(session_tasks_fts)")
		order = key + `, t.session_id, t.ordinal`
		orderArgs = keyArgs
	}
	if q.Agent != "" {
		where = append(where, `s2.agent = ?`)
		whereArgs = append(whereArgs, q.Agent)
	}
	if q.Workspace != "" {
		where = append(where, `s2.workspace = ?`)
		whereArgs = append(whereArgs, q.Workspace)
	}
	if q.SinceUnix > 0 {
		where = append(where, `COALESCE(s2.ended_at, s2.started_at, 0) >= ?`)
		whereArgs = append(whereArgs, q.SinceUnix)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}
	// Placeholders appear in WHERE, then ORDER BY, then LIMIT.
	args := append(append(whereArgs, orderArgs...), limit)
	rows, err := d.Query(fmt.Sprintf(`
	    SELECT s2.session_uid, s2.agent, COALESCE(s2.workspace,''), COALESCE(s2.title,''),
	           COALESCE(s2.parent_uid,''), t.ordinal, t.task,
	           COALESCE(s2.started_at,0), COALESCE(s2.ended_at,0)
	      FROM %s %s
	     ORDER BY %s
	     LIMIT ?`, from, whereSQL, order), args...)
	if err != nil {
		return nil, fmt.Errorf("search tasks: %w", err)
	}
	defer rows.Close()
	var out []TaskHit
	for rows.Next() {
		var h TaskHit
		if err := rows.Scan(&h.SessionUID, &h.Agent, &h.Workspace, &h.Title, &h.ParentUID,
			&h.Ordinal, &h.Text, &h.StartedAt, &h.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

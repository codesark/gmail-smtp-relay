package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create queue directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite queue: %w", err)
	}

	if _, err := db.Exec(`
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;
`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure sqlite pragmas: %w", err)
	}

	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS outbound_queue (
  id TEXT PRIMARY KEY,
  mail_from TEXT NOT NULL,
  recipients_json TEXT NOT NULL,
  rfc822 BLOB NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_outbound_queue_status_created
  ON outbound_queue(status, next_attempt_at, created_at);
`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create queue schema: %w", err)
	}

	for _, alter := range []string{
		"ALTER TABLE outbound_queue ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE outbound_queue ADD COLUMN next_attempt_at TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE outbound_queue ADD COLUMN last_error TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE outbound_queue ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''",
	} {
		if _, err := db.Exec(alter); err != nil && !isDuplicateColumnErr(err) {
			_ = db.Close()
			return nil, fmt.Errorf("migrate queue schema: %w", err)
		}
	}
	_, _ = db.Exec(`UPDATE outbound_queue SET next_attempt_at = created_at WHERE next_attempt_at = ''`)
	_, _ = db.Exec(`UPDATE outbound_queue SET updated_at = created_at WHERE updated_at = ''`)

	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Enqueue(ctx context.Context, msg Message) error {
	if msg.ID == "" {
		return errors.New("enqueue message missing id")
	}
	if msg.MailFrom == "" {
		return errors.New("enqueue message missing mail_from")
	}
	if len(msg.Recipients) == 0 {
		return errors.New("enqueue message missing recipients")
	}
	if len(msg.Data) == 0 {
		return errors.New("enqueue message missing data")
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	if msg.UpdatedAt.IsZero() {
		msg.UpdatedAt = msg.CreatedAt
	}

	recipientsJSON, err := json.Marshal(msg.Recipients)
	if err != nil {
		return fmt.Errorf("marshal recipients: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
INSERT INTO outbound_queue (id, mail_from, recipients_json, rfc822, status, attempts, next_attempt_at, last_error, created_at, updated_at)
VALUES (?, ?, ?, ?, 'pending', 0, ?, '', ?, ?)
`, msg.ID, msg.MailFrom, string(recipientsJSON), msg.Data, msg.CreatedAt.Format(time.RFC3339Nano), msg.CreatedAt.Format(time.RFC3339Nano), msg.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert queue message: %w", err)
	}
	return nil
}

func (s *SQLiteStore) BacklogCount(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM outbound_queue WHERE status IN ('pending', 'processing')`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count backlog queue rows: %w", err)
	}
	return count, nil
}

func (s *SQLiteStore) Stats(ctx context.Context, now time.Time) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(ctx, `
SELECT
  COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN status='processing' THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN status='sent' THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END), 0)
FROM outbound_queue
`).Scan(&st.PendingCount, &st.ProcessingCount, &st.SentCount, &st.FailedCount); err != nil {
		return Stats{}, fmt.Errorf("query queue stats: %w", err)
	}
	st.BacklogCount = st.PendingCount + st.ProcessingCount

	var oldestPendingRaw sql.NullString
	if err := s.db.QueryRowContext(ctx, `
SELECT created_at
FROM outbound_queue
WHERE status = 'pending'
ORDER BY created_at ASC
LIMIT 1
`).Scan(&oldestPendingRaw); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Stats{}, fmt.Errorf("query oldest pending row: %w", err)
	}
	if oldestPendingRaw.Valid && oldestPendingRaw.String != "" {
		ts, err := time.Parse(time.RFC3339Nano, oldestPendingRaw.String)
		if err != nil {
			return Stats{}, fmt.Errorf("parse oldest pending timestamp: %w", err)
		}
		st.OldestPendingSince = ts
		if now.After(ts) {
			st.OldestPendingAge = now.Sub(ts)
		}
	}

	return st, nil
}

func (s *SQLiteStore) ClaimNextPending(ctx context.Context, now time.Time) (*Message, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
SELECT id, mail_from, recipients_json, rfc822, attempts, created_at, updated_at
FROM outbound_queue
WHERE status = 'pending' AND next_attempt_at <= ?
ORDER BY created_at ASC
LIMIT 1
`, now.Format(time.RFC3339Nano))

	var (
		msg            Message
		recipientsJSON string
		createdAtRaw   string
		updatedAtRaw   string
	)
	switch err := row.Scan(&msg.ID, &msg.MailFrom, &recipientsJSON, &msg.Data, &msg.Attempts, &createdAtRaw, &updatedAtRaw); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("query next pending message: %w", err)
	}

	if err := json.Unmarshal([]byte(recipientsJSON), &msg.Recipients); err != nil {
		return nil, fmt.Errorf("unmarshal queue recipients: %w", err)
	}
	if msg.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAtRaw); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if msg.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAtRaw); err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}

	updatedAt := now.UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `
UPDATE outbound_queue
SET status = 'processing', attempts = attempts + 1, updated_at = ?
WHERE id = ? AND status = 'pending'
`, updatedAt, msg.ID)
	if err != nil {
		return nil, fmt.Errorf("mark row processing: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("check claim rows affected: %w", err)
	}
	if affected != 1 {
		return nil, nil
	}

	msg.Attempts++
	msg.UpdatedAt = now.UTC()
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim tx: %w", err)
	}

	return &msg, nil
}

func (s *SQLiteStore) RecoverStuckProcessing(ctx context.Context, olderThan time.Time, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE outbound_queue
SET status='pending',
    next_attempt_at=?,
    updated_at=?,
    last_error=?
WHERE status='processing' AND updated_at < ?
`, now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), "recovered after stale processing timeout", olderThan.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("recover stuck processing rows: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read recovered rows count: %w", err)
	}
	return int(affected), nil
}

func (s *SQLiteStore) MarkSent(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE outbound_queue
SET status = 'sent', updated_at = ?, last_error = ''
WHERE id = ?
`, now.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("mark sent: %w", err)
	}
	return nil
}

func (s *SQLiteStore) MarkRetry(ctx context.Context, id string, nextAttempt time.Time, lastErr string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE outbound_queue
SET status = 'pending', next_attempt_at = ?, last_error = ?, updated_at = ?
WHERE id = ?
`, nextAttempt.UTC().Format(time.RFC3339Nano), truncateErr(lastErr), now.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("mark retry: %w", err)
	}
	return nil
}

func (s *SQLiteStore) MarkFailed(ctx context.Context, id string, lastErr string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE outbound_queue
SET status = 'failed', last_error = ?, updated_at = ?
WHERE id = ?
`, truncateErr(lastErr), now.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	v := strings.ToLower(err.Error())
	return strings.Contains(v, "duplicate column name")
}

func truncateErr(v string) string {
	const max = 1024
	if len(v) <= max {
		return v
	}
	return v[:max]
}

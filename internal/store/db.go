// Package store manages the SQLite database used to track trigger_log
// and persisted review_watch state.
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS trigger_log (
    id           INTEGER PRIMARY KEY,
    owner        TEXT    NOT NULL,
    repo         TEXT    NOT NULL,
    pr           INTEGER NOT NULL,
    trigger      TEXT    NOT NULL,
    requested_at INTEGER NOT NULL DEFAULT (strftime('%s','now')), -- epoch seconds (UTC)
    completed_at INTEGER                                          -- epoch seconds (UTC), NULL while pending
);
CREATE INDEX IF NOT EXISTS idx_trigger_log_pr ON trigger_log(owner, repo, pr);

CREATE TABLE IF NOT EXISTS review_watch (
    id                  TEXT PRIMARY KEY,
    github_login        TEXT    NOT NULL,
    owner               TEXT    NOT NULL,
    repo                TEXT    NOT NULL,
    pr                  INTEGER NOT NULL,
    trigger_log_id      INTEGER,
    resource_uri        TEXT,
    watch_status        TEXT    NOT NULL,
    review_status       TEXT,
    failure_reason      TEXT,
    is_active           INTEGER NOT NULL DEFAULT 1,
    started_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    completed_at        INTEGER,
    stale_at            INTEGER,
    last_error          TEXT,
    rate_limit_reset_at INTEGER,
    recovery_hint       TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_review_watch_active_per_pr
    ON review_watch(github_login, owner, repo, pr)
    WHERE is_active = 1;
`

const reviewWatchLookupIndexSQL = `
DROP INDEX IF EXISTS idx_review_watch_lookup;
CREATE INDEX idx_review_watch_lookup
    ON review_watch(github_login, owner, repo, pr, updated_at DESC, started_at DESC);
`

const staleOnOpenMessage = "watch became stale because the review-raven process restarted"

// DB wraps a SQLite database for trigger_log operations.
type DB struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path and applies the schema.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// Limit to a single connection to avoid "database is locked" errors with SQLite.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// Enable WAL mode for better concurrent access.
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Migration: add prev_review_id column for ID-based review staleness detection.
	// Use PRAGMA table_info to check existence first; this avoids relying on SQLite
	// driver error message text ("duplicate column name") which can vary by version.
	var colExists bool
	rows, err := db.Query(`PRAGMA table_info(trigger_log)`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migration check table_info: %w", err)
	}
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dfltValue interface{}
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			_ = rows.Close()
			_ = db.Close()
			return nil, fmt.Errorf("migration scan table_info: %w", err)
		}
		if name == "prev_review_id" {
			colExists = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		_ = db.Close()
		return nil, fmt.Errorf("migration iterate table_info: %w", err)
	}
	if err := rows.Close(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migration close table_info rows: %w", err)
	}
	if !colExists {
		if _, err := db.Exec(`ALTER TABLE trigger_log ADD COLUMN prev_review_id TEXT`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migration add prev_review_id: %w", err)
		}
	}
	// Migration: add recovery_hint column to review_watch for persisting auth-failure recovery hints.
	var recoveryHintExists bool
	rwRows, err := db.Query(`PRAGMA table_info(review_watch)`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migration check review_watch table_info: %w", err)
	}
	for rwRows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dfltValue interface{}
		if err := rwRows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			_ = rwRows.Close()
			_ = db.Close()
			return nil, fmt.Errorf("migration scan review_watch table_info: %w", err)
		}
		if name == "recovery_hint" {
			recoveryHintExists = true
			break
		}
	}
	if err := rwRows.Err(); err != nil {
		_ = rwRows.Close()
		_ = db.Close()
		return nil, fmt.Errorf("migration iterate review_watch table_info: %w", err)
	}
	if err := rwRows.Close(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migration close review_watch table_info rows: %w", err)
	}
	if !recoveryHintExists {
		if _, err := db.Exec(`ALTER TABLE review_watch ADD COLUMN recovery_hint TEXT`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migration add recovery_hint: %w", err)
		}
	}
	if _, err := db.Exec(reviewWatchLookupIndexSQL); err != nil {
		_ = db.Close()
		return nil, err
	}
	d := &DB{db: db}
	if _, err := d.MarkActiveReviewWatchesStale(staleOnOpenMessage); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// Close releases the database connection.
func (d *DB) Close() error { return d.db.Close() }

// TriggerEntry is a row from trigger_log.
type TriggerEntry struct {
	ID           int64
	Trigger      string    // "MANUAL" or "AUTO"
	RequestedAt  time.Time // when the review was requested
	CompletedAt  *time.Time
	PrevReviewID *string // ID of the Copilot review that existed when the request was made (nil for backward compat)
}

// ReviewWatchEntry is a persisted watch snapshot in review_watch.
type ReviewWatchEntry struct {
	ID               string
	GitHubLogin      string
	Owner            string
	Repo             string
	PR               int
	TriggerLogID     *int64
	ResourceURI      *string
	WatchStatus      string
	ReviewStatus     *string
	FailureReason    *string
	IsActive         bool
	StartedAt        time.Time
	UpdatedAt        time.Time
	CompletedAt      *time.Time
	StaleAt          *time.Time
	LastError        *string
	RateLimitResetAt *time.Time
	RecoveryHint     *string
}

// ReviewWatchFilter scopes a review_watch listing query.
type ReviewWatchFilter struct {
	GitHubLogin string
	Owner       string
	Repo        string
	PR          int
	ActiveOnly  bool
	Limit       int
}

// Insert adds a new trigger_log entry and returns the assigned ID.
func (d *DB) Insert(owner, repo string, pr int, trigger string) (int64, error) {
	res, err := d.db.Exec(
		`INSERT INTO trigger_log (owner, repo, pr, trigger) VALUES (?, ?, ?, ?)`,
		owner, repo, pr, trigger,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// InsertWithTime adds a new trigger_log entry with an explicit requested_at timestamp
// and returns the assigned ID. The timestamp is stored at epoch-second precision;
// sub-second components are truncated by the conversion to Unix seconds. Use this
// when the logical request time must align with an existing event timestamp (e.g.
// a Copilot review's SubmittedAt) so the stale-guard in DeriveStatusWithThreshold
// passes on the next status check.
func (d *DB) InsertWithTime(owner, repo string, pr int, trigger string, requestedAt time.Time) (int64, error) {
	res, err := d.db.Exec(
		`INSERT INTO trigger_log (owner, repo, pr, trigger, requested_at) VALUES (?, ?, ?, ?, ?)`,
		owner, repo, pr, trigger, requestedAt.UTC().Unix(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// InsertWithPrevReviewID adds a new trigger_log entry with an explicit requested_at timestamp
// and the ID of the Copilot review that existed when the request was made.
// prevReviewID enables ID-based staleness detection in DeriveStatusWithThreshold:
// if the current review has the same ID, it is the old review (stale);
// a different ID means Copilot has submitted a new review.
// The timestamp is stored at epoch-second precision (sub-second components truncated).
func (d *DB) InsertWithPrevReviewID(owner, repo string, pr int, trigger string, requestedAt time.Time, prevReviewID string) (int64, error) {
	res, err := d.db.Exec(
		`INSERT INTO trigger_log (owner, repo, pr, trigger, requested_at, prev_review_id) VALUES (?, ?, ?, ?, ?, ?)`,
		owner, repo, pr, trigger, requestedAt.UTC().Unix(), prevReviewID,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetLatest returns the most recent trigger_log entry for the given PR,
// or nil if none exists.
func (d *DB) GetLatest(owner, repo string, pr int) (*TriggerEntry, error) {
	row := d.db.QueryRow(
		`SELECT id, trigger, requested_at, completed_at, prev_review_id
		 FROM trigger_log
		 WHERE owner = ? AND repo = ? AND pr = ?
		 ORDER BY requested_at DESC, id DESC
		 LIMIT 1`,
		owner, repo, pr,
	)
	var e TriggerEntry
	var requestedAtUnix int64
	var completedAtUnix sql.NullInt64
	var prevReviewID sql.NullString
	if err := row.Scan(&e.ID, &e.Trigger, &requestedAtUnix, &completedAtUnix, &prevReviewID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	e.RequestedAt = time.Unix(requestedAtUnix, 0)
	if completedAtUnix.Valid {
		t := time.Unix(completedAtUnix.Int64, 0)
		e.CompletedAt = &t
	}
	if prevReviewID.Valid {
		e.PrevReviewID = &prevReviewID.String
	}
	return &e, nil
}

// UpdateCompletedAt marks the given trigger_log row as completed (now).
// The update is conditional on completed_at IS NULL so the original completion
// time is preserved across retries or concurrent calls.
func (d *DB) UpdateCompletedAt(id int64) error {
	_, err := d.db.Exec(
		`UPDATE trigger_log
		 SET completed_at = strftime('%s','now')
		 WHERE id = ? AND completed_at IS NULL`,
		id,
	)
	return err
}

// HasPending returns true if there is an unfinished trigger_log entry for the PR.
func (d *DB) HasPending(owner, repo string, pr int) (bool, error) {
	var count int
	err := d.db.QueryRow(
		`SELECT COUNT(*) FROM trigger_log
		 WHERE owner = ? AND repo = ? AND pr = ? AND completed_at IS NULL`,
		owner, repo, pr,
	).Scan(&count)
	return count > 0, err
}

// UpsertReviewWatch inserts or updates a persisted review_watch snapshot by watch ID.
func (d *DB) UpsertReviewWatch(entry ReviewWatchEntry) error {
	_, err := d.db.Exec(
		`INSERT INTO review_watch (
		    id, github_login, owner, repo, pr, trigger_log_id, resource_uri,
		    watch_status, review_status, failure_reason, is_active,
		    started_at, updated_at, completed_at, stale_at, last_error, rate_limit_reset_at,
		    recovery_hint
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    trigger_log_id      = excluded.trigger_log_id,
		    resource_uri        = excluded.resource_uri,
		    watch_status        = excluded.watch_status,
		    review_status       = excluded.review_status,
		    failure_reason      = excluded.failure_reason,
		    is_active           = excluded.is_active,
		    updated_at          = excluded.updated_at,
		    completed_at        = excluded.completed_at,
		    stale_at            = excluded.stale_at,
		    last_error          = excluded.last_error,
		    rate_limit_reset_at = excluded.rate_limit_reset_at,
		    recovery_hint       = excluded.recovery_hint`,
		entry.ID,
		entry.GitHubLogin,
		entry.Owner,
		entry.Repo,
		entry.PR,
		nullInt64(entry.TriggerLogID),
		nullString(entry.ResourceURI),
		entry.WatchStatus,
		nullString(entry.ReviewStatus),
		nullString(entry.FailureReason),
		boolToInt(entry.IsActive),
		entry.StartedAt.UTC().Unix(),
		entry.UpdatedAt.UTC().Unix(),
		nullTime(entry.CompletedAt),
		nullTime(entry.StaleAt),
		nullString(entry.LastError),
		nullTime(entry.RateLimitResetAt),
		nullString(entry.RecoveryHint),
	)
	return err
}

// GetReviewWatchByID returns a persisted review_watch row by watch ID.
func (d *DB) GetReviewWatchByID(id string) (*ReviewWatchEntry, error) {
	row := d.db.QueryRow(
		`SELECT id, github_login, owner, repo, pr, trigger_log_id, resource_uri,
		        watch_status, review_status, failure_reason, is_active,
		        started_at, updated_at, completed_at, stale_at, last_error, rate_limit_reset_at,
		        recovery_hint
		   FROM review_watch
		  WHERE id = ?`,
		id,
	)
	return scanReviewWatch(row)
}

// GetLatestReviewWatch returns the most recently updated watch for the user/PR key.
func (d *DB) GetLatestReviewWatch(login, owner, repo string, pr int) (*ReviewWatchEntry, error) {
	row := d.db.QueryRow(
		`SELECT id, github_login, owner, repo, pr, trigger_log_id, resource_uri,
		        watch_status, review_status, failure_reason, is_active,
		        started_at, updated_at, completed_at, stale_at, last_error, rate_limit_reset_at,
		        recovery_hint
		   FROM review_watch
		  WHERE github_login = ? AND owner = ? AND repo = ? AND pr = ?
		  ORDER BY updated_at DESC, started_at DESC, rowid DESC
		  LIMIT 1`,
		login, owner, repo, pr,
	)
	return scanReviewWatch(row)
}

// ListReviewWatches returns persisted review_watch snapshots for one GitHub login.
func (d *DB) ListReviewWatches(filter ReviewWatchFilter) ([]ReviewWatchEntry, error) {
	query := `SELECT id, github_login, owner, repo, pr, trigger_log_id, resource_uri,
	                 watch_status, review_status, failure_reason, is_active,
	                 started_at, updated_at, completed_at, stale_at, last_error, rate_limit_reset_at,
	                 recovery_hint
	            FROM review_watch
	           WHERE github_login = ?`
	args := []any{filter.GitHubLogin}

	if filter.Owner != "" {
		query += ` AND owner = ?`
		args = append(args, filter.Owner)
	}
	if filter.Repo != "" {
		query += ` AND repo = ?`
		args = append(args, filter.Repo)
	}
	if filter.PR > 0 {
		query += ` AND pr = ?`
		args = append(args, filter.PR)
	}
	if filter.ActiveOnly {
		query += ` AND is_active = 1`
	}

	query += ` ORDER BY is_active DESC, updated_at DESC, started_at DESC, rowid DESC`

	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var entries []ReviewWatchEntry
	for rows.Next() {
		entry, err := scanReviewWatch(rows)
		if err != nil {
			return nil, err
		}
		if entry != nil {
			entries = append(entries, *entry)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// MarkActiveReviewWatchesStale deactivates any persisted active watches.
// Used on startup because worker state is memory-only and cannot survive process restart.
func (d *DB) MarkActiveReviewWatchesStale(lastError string) (int64, error) {
	res, err := d.db.Exec(
		`UPDATE review_watch
		    SET watch_status = 'STALE',
		        failure_reason = NULL,
		        is_active = 0,
		        updated_at = strftime('%s','now'),
		        completed_at = COALESCE(completed_at, strftime('%s','now')),
		        stale_at = COALESCE(stale_at, strftime('%s','now')),
		        last_error = CASE
		                       WHEN ? = '' THEN last_error
		                       WHEN last_error IS NULL OR last_error = '' THEN ?
		                       ELSE last_error
		                     END
		  WHERE is_active = 1`,
		lastError,
		lastError,
	)
	if err != nil {
		return 0, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return rows, nil
}

func scanReviewWatch(row scanner) (*ReviewWatchEntry, error) {
	var entry ReviewWatchEntry
	var (
		triggerLogID     sql.NullInt64
		resourceURI      sql.NullString
		reviewStatus     sql.NullString
		failureReason    sql.NullString
		completedAt      sql.NullInt64
		staleAt          sql.NullInt64
		lastError        sql.NullString
		rateLimitResetAt sql.NullInt64
		recoveryHint     sql.NullString
		isActive         int
		startedAtUnix    int64
		updatedAtUnix    int64
	)
	if err := row.Scan(
		&entry.ID,
		&entry.GitHubLogin,
		&entry.Owner,
		&entry.Repo,
		&entry.PR,
		&triggerLogID,
		&resourceURI,
		&entry.WatchStatus,
		&reviewStatus,
		&failureReason,
		&isActive,
		&startedAtUnix,
		&updatedAtUnix,
		&completedAt,
		&staleAt,
		&lastError,
		&rateLimitResetAt,
		&recoveryHint,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	entry.IsActive = isActive != 0
	entry.StartedAt = time.Unix(startedAtUnix, 0).UTC()
	entry.UpdatedAt = time.Unix(updatedAtUnix, 0).UTC()
	entry.TriggerLogID = fromNullInt64(triggerLogID)
	entry.ResourceURI = fromNullString(resourceURI)
	entry.ReviewStatus = fromNullString(reviewStatus)
	entry.FailureReason = fromNullString(failureReason)
	entry.CompletedAt = fromNullUnix(completedAt)
	entry.StaleAt = fromNullUnix(staleAt)
	entry.LastError = fromNullString(lastError)
	entry.RateLimitResetAt = fromNullUnix(rateLimitResetAt)
	entry.RecoveryHint = fromNullString(recoveryHint)
	return &entry, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nullInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func nullString(v *string) sql.NullString {
	if v == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *v, Valid: true}
}

func nullTime(v *time.Time) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v.UTC().Unix(), Valid: true}
}

func fromNullInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	value := v.Int64
	return &value
}

func fromNullString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	value := v.String
	return &value
}

func fromNullUnix(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

package reviewrequest

import (
	"context"
	"database/sql"
	"fmt"
)

// The ledger tables are independent of the legacy trigger_log / review_watch
// tables: those belong to the Copilot-era watch feature and must not be
// conflated with the new review-request cycles.
const ledgerSchema = `
CREATE TABLE IF NOT EXISTS review_requests (
    cycle_id            TEXT PRIMARY KEY,
    owner               TEXT    NOT NULL,
    repo                TEXT    NOT NULL,
    pr                  INTEGER NOT NULL,
    round               INTEGER NOT NULL,
    expected_head       TEXT    NOT NULL,
    reason              TEXT    NOT NULL,
    requested_by        TEXT    NOT NULL,

    dispatch_state      TEXT    NOT NULL,
    candidate_id        TEXT,
    dispatch_attempts   INTEGER NOT NULL DEFAULT 0,
    dispatch_error      TEXT,

    cancel_state        TEXT    NOT NULL DEFAULT 'none',
    cancel_id           TEXT,
    cancel_requested_at TEXT,
    cancel_confirmed_at TEXT,
    cancel_error        TEXT,

    exec_state          TEXT    NOT NULL DEFAULT 'unknown',
    exec_hold           TEXT,
    exec_producer       TEXT,
    exec_epoch          INTEGER,
    exec_seq            INTEGER,
    exec_observed_at    TEXT,
    exec_received_at    TEXT,
    exec_outcome        TEXT,
    exec_exit_code      INTEGER,
    exec_stopped        INTEGER NOT NULL DEFAULT 0,

    result_kind         TEXT,
    result_head         TEXT,
    result_version      INTEGER,
    result_comment_id   INTEGER,
    result_url          TEXT,

    terminal_reason     TEXT,
    superseded_by       TEXT,
    terminal_at         TEXT,

    revision            INTEGER NOT NULL,
    created_at          TEXT    NOT NULL,
    updated_at          TEXT    NOT NULL,
    UNIQUE (owner, repo, pr, round)
);
CREATE INDEX IF NOT EXISTS idx_review_requests_pr ON review_requests(owner, repo, pr, round);

-- Idempotency scope is (principal, PR, kind, key); a cancellation key is scoped
-- the same way, with the cycle folded into the payload hash.
CREATE TABLE IF NOT EXISTS request_keys (
    principal    TEXT NOT NULL,
    owner        TEXT NOT NULL,
    repo         TEXT NOT NULL,
    pr           INTEGER NOT NULL,
    kind         TEXT NOT NULL,
    idem_key     TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    cycle_id     TEXT NOT NULL,
    cancel_id    TEXT,
    created_at   TEXT NOT NULL,
    PRIMARY KEY (principal, owner, repo, pr, kind, idem_key)
);

-- Per-PR round counter. It outlives the cycles it numbered, so a round is never
-- reused after a purge or a restart.
CREATE TABLE IF NOT EXISTS pr_rounds (
    owner      TEXT    NOT NULL,
    repo       TEXT    NOT NULL,
    pr         INTEGER NOT NULL,
    last_round INTEGER NOT NULL,
    PRIMARY KEY (owner, repo, pr)
);

-- A webhook delivery / comment ID stays bound to its cycle forever (within
-- retention), so a late redelivery of an old source never opens a new round.
CREATE TABLE IF NOT EXISTS source_bindings (
    source_id  TEXT PRIMARY KEY,
    cycle_id   TEXT NOT NULL,
    created_at TEXT NOT NULL
);

-- Written in the same transaction as the state change it announces
-- (transactional outbox). Delivery to the reviewer side is at-least-once.
CREATE TABLE IF NOT EXISTS request_outbox (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    cycle_id        TEXT    NOT NULL,
    kind            TEXT    NOT NULL,
    payload         TEXT    NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT    NOT NULL,
    delivered_at    TEXT,
    outcome         TEXT,
    created_at      TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_request_outbox_pending ON request_outbox(delivered_at, next_attempt_at);

-- Cycle IDs that were purged after retention. Reads return REQUEST_EXPIRED
-- instead of REQUEST_NOT_FOUND, and the URI is never reused.
CREATE TABLE IF NOT EXISTS expired_cycles (
    cycle_id   TEXT PRIMARY KEY,
    expired_at TEXT NOT NULL
);
`

// Outbox message kinds.
const (
	outboxAccept = "accept"
	outboxCancel = "cancel"
)

func applySchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, ledgerSchema); err != nil {
		return fmt.Errorf("apply review request ledger schema: %w", err)
	}
	// CREATE TABLE IF NOT EXISTS は既存の表へ列を足さない。台帳を先に有効にした DB を移行する。
	if err := ensureColumn(ctx, db, "request_outbox", "outcome", "TEXT"); err != nil {
		return err
	}
	return nil
}

// ensureColumn adds a column to an existing table when it is missing. The table
// info is read first rather than matching a driver error message.
func ensureColumn(ctx context.Context, db *sql.DB, table, column, definition string) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return fmt.Errorf("read %s columns: %w", table, err)
	}
	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan %s columns: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate %s columns: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close %s columns: %w", table, err)
	}
	if found {
		return nil
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

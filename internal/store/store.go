package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Binding struct {
	GroupChatID   int64
	ChannelChatID int64
	BoundByUserID int64
	BoundAt       time.Time
	Epoch         int64
}

type VerifiedMember struct {
	GroupChatID   int64
	ChannelChatID int64
	UserID        int64
	ExpiresAt     time.Time
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := path
	if !isMemoryDSN(path) {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc.org/sqlite gives each connection its own private :memory: database,
	// so a shared schema requires pinning to a single connection for in-memory DSNs.
	if isMemoryDSN(path) {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(2)
	}

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// isMemoryDSN reports whether the given sqlite path/DSN backs an in-memory database.
func isMemoryDSN(path string) bool {
	if path == "" || path == ":memory:" {
		return true
	}
	return strings.Contains(strings.ToLower(path), "mode=memory")
}

func (s *Store) Close() error { return s.db.Close() }

// DB returns the underlying *sql.DB for tests and diagnostics.
func (s *Store) DB() *sql.DB { return s.db }

const schemaV1 = `
CREATE TABLE IF NOT EXISTS bindings (
  group_chat_id    INTEGER PRIMARY KEY,
  channel_chat_id  INTEGER NOT NULL,
  bound_by_user_id INTEGER NOT NULL,
  bound_at         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS bindings_channel_idx ON bindings(channel_chat_id);

CREATE TABLE IF NOT EXISTS verified_members (
  group_chat_id INTEGER NOT NULL,
  user_id       INTEGER NOT NULL,
  expires_at    INTEGER NOT NULL,
  PRIMARY KEY (group_chat_id, user_id)
);
CREATE INDEX IF NOT EXISTS verified_expires_idx ON verified_members(expires_at);
`

const schemaV2 = `
DROP TABLE IF EXISTS verified_members;
CREATE TABLE verified_members (
  group_chat_id   INTEGER NOT NULL,
  channel_chat_id INTEGER NOT NULL,
  user_id         INTEGER NOT NULL,
  expires_at      INTEGER NOT NULL,
  PRIMARY KEY (group_chat_id, channel_chat_id, user_id)
);
CREATE INDEX IF NOT EXISTS verified_expires_idx ON verified_members(expires_at);
`

const schemaV3 = `
ALTER TABLE bindings ADD COLUMN epoch INTEGER NOT NULL DEFAULT 1;
`

const schemaV4 = `
CREATE TABLE IF NOT EXISTS binding_epochs (
  group_chat_id INTEGER PRIMARY KEY,
  last_epoch    INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO binding_epochs (group_chat_id, last_epoch)
SELECT group_chat_id, epoch FROM bindings;
`

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if v < 1 {
		if _, err := s.db.ExecContext(ctx, schemaV1); err != nil {
			return fmt.Errorf("apply schema v1: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
			return fmt.Errorf("set user_version: %w", err)
		}
		v = 1
	}
	if v < 2 {
		if _, err := s.db.ExecContext(ctx, schemaV2); err != nil {
			return fmt.Errorf("apply schema v2: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
			return fmt.Errorf("set user_version: %w", err)
		}
		v = 2
	}
	if v < 3 {
		if _, err := s.db.ExecContext(ctx, schemaV3); err != nil {
			return fmt.Errorf("apply schema v3: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 3"); err != nil {
			return fmt.Errorf("set user_version: %w", err)
		}
		v = 3
	}
	if v < 4 {
		if _, err := s.db.ExecContext(ctx, schemaV4); err != nil {
			return fmt.Errorf("apply schema v4: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 4"); err != nil {
			return fmt.Errorf("set user_version: %w", err)
		}
	}
	return nil
}

// ---- Bindings ----

// UpsertBinding returns created=true when a new row was inserted,
// created=false when an existing row was updated. channelChanged=true indicates an
// existing row's channel_chat_id was replaced; in that case any verified_members rows
// for the group are wiped in the same transaction so that approvals granted against
// the old channel cannot leak into the new binding.
func (s *Store) UpsertBinding(ctx context.Context, b Binding) (created bool, channelChanged bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var existingChannel int64
	err = tx.QueryRowContext(ctx,
		`SELECT channel_chat_id FROM bindings WHERE group_chat_id = ?`, b.GroupChatID,
	).Scan(&existingChannel)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		created = true
	case err != nil:
		return false, false, err
	default:
		created = false
		channelChanged = existingChannel != b.ChannelChatID
	}

	// Bump per-group epoch counter (creates row on first use). The counter is
	// maintained outside the bindings table so it survives DeleteBinding and
	// provides strictly monotonic epochs across unbind/rebind cycles.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO binding_epochs (group_chat_id, last_epoch) VALUES (?, 1)
		 ON CONFLICT(group_chat_id) DO UPDATE SET last_epoch = last_epoch + 1`,
		b.GroupChatID,
	); err != nil {
		return false, false, err
	}
	var newEpoch int64
	if err := tx.QueryRowContext(ctx,
		`SELECT last_epoch FROM binding_epochs WHERE group_chat_id = ?`, b.GroupChatID,
	).Scan(&newEpoch); err != nil {
		return false, false, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO bindings (group_chat_id, channel_chat_id, bound_by_user_id, bound_at, epoch)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(group_chat_id) DO UPDATE SET
		   channel_chat_id = excluded.channel_chat_id,
		   bound_by_user_id = excluded.bound_by_user_id,
		   bound_at = excluded.bound_at,
		   epoch = excluded.epoch`,
		b.GroupChatID, b.ChannelChatID, b.BoundByUserID, b.BoundAt.Unix(), newEpoch,
	); err != nil {
		return false, false, err
	}

	if channelChanged {
		if _, err = tx.ExecContext(ctx,
			`DELETE FROM verified_members WHERE group_chat_id = ?`, b.GroupChatID); err != nil {
			return false, false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return false, false, err
	}
	return created, channelChanged, nil
}

// GetBinding returns nil when no binding exists for groupID.
func (s *Store) GetBinding(ctx context.Context, groupID int64) (*Binding, error) {
	var (
		b  Binding
		ts int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT group_chat_id, channel_chat_id, bound_by_user_id, bound_at, epoch
		 FROM bindings WHERE group_chat_id = ?`, groupID,
	).Scan(&b.GroupChatID, &b.ChannelChatID, &b.BoundByUserID, &ts, &b.Epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.BoundAt = time.Unix(ts, 0)
	return &b, nil
}

func (s *Store) ListBindings(ctx context.Context) ([]Binding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT group_chat_id, channel_chat_id, bound_by_user_id, bound_at, epoch FROM bindings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Binding
	for rows.Next() {
		var (
			b  Binding
			ts int64
		)
		if err := rows.Scan(&b.GroupChatID, &b.ChannelChatID, &b.BoundByUserID, &ts, &b.Epoch); err != nil {
			return nil, err
		}
		b.BoundAt = time.Unix(ts, 0)
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteBinding removes the binding row and cascades verified_members for that group.
// Returns true if a binding was actually removed.
func (s *Store) DeleteBinding(ctx context.Context, groupID int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM bindings WHERE group_chat_id = ?`, groupID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM verified_members WHERE group_chat_id = ?`, groupID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ---- Verified members ----

// GetVerified returns (expiresAt, true) if the row exists and is not yet expired.
// Expired rows are treated as miss (not returned, not auto-deleted).
func (s *Store) GetVerified(ctx context.Context, groupID, channelID, userID int64, now time.Time) (time.Time, bool, error) {
	var ts int64
	err := s.db.QueryRowContext(ctx,
		`SELECT expires_at FROM verified_members WHERE group_chat_id = ? AND channel_chat_id = ? AND user_id = ?`,
		groupID, channelID, userID,
	).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	exp := time.Unix(ts, 0)
	if !exp.After(now) {
		return exp, false, nil
	}
	return exp, true, nil
}

func (s *Store) UpsertVerified(ctx context.Context, groupID, channelID, userID int64, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO verified_members (group_chat_id, channel_chat_id, user_id, expires_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(group_chat_id, channel_chat_id, user_id) DO UPDATE SET expires_at = excluded.expires_at`,
		groupID, channelID, userID, expiresAt.Unix(),
	)
	return err
}

// UpsertVerifiedIfBound inserts/updates a verified row ONLY if a matching
// (group, channel) binding still exists at statement-execution time. The
// binding check and the row write are performed in a single atomic SQLite
// statement, so no concurrent /unbind or rebind can leave a stale approval.
//
// Returns applied=true when a row was inserted or updated; applied=false
// means no matching binding exists and nothing was written.
func (s *Store) UpsertVerifiedIfBound(ctx context.Context, groupID, channelID, userID int64, epoch int64, expiresAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO verified_members (group_chat_id, channel_chat_id, user_id, expires_at)
		 SELECT ?, ?, ?, ?
		 WHERE EXISTS (SELECT 1 FROM bindings WHERE group_chat_id = ? AND channel_chat_id = ? AND epoch = ?)
		 ON CONFLICT(group_chat_id, channel_chat_id, user_id) DO UPDATE SET expires_at = excluded.expires_at`,
		groupID, channelID, userID, expiresAt.Unix(),
		groupID, channelID, epoch,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) DeleteExpiredVerified(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM verified_members WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountVerifiedInChannel returns the count of non-expired verified rows for a
// (group, channel) pair. Scoping by channel ensures stale rows from a previous
// binding do not inflate counts reported after a rebind.
func (s *Store) CountVerifiedInChannel(ctx context.Context, groupID, channelID int64, now time.Time) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM verified_members WHERE group_chat_id = ? AND channel_chat_id = ? AND expires_at > ?`,
		groupID, channelID, now.Unix(),
	).Scan(&n)
	return n, err
}

// LoadAllValidVerified is used at startup to warm an in-memory cache.
func (s *Store) LoadAllValidVerified(ctx context.Context, now time.Time) ([]VerifiedMember, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT group_chat_id, channel_chat_id, user_id, expires_at FROM verified_members WHERE expires_at > ?`,
		now.Unix(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VerifiedMember
	for rows.Next() {
		var (
			v  VerifiedMember
			ts int64
		)
		if err := rows.Scan(&v.GroupChatID, &v.ChannelChatID, &v.UserID, &ts); err != nil {
			return nil, err
		}
		v.ExpiresAt = time.Unix(ts, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}

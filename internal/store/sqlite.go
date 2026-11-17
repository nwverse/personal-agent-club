package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/nwverse/personal-agent-club/internal/protocol"
)

// schemaVersion is the SQLite schema's version (PRAGMA user_version).
const schemaVersion = 1

// The tables and columns are the Lark gateway's agent_* tables, so an import can be checked row for row. Booleans
// are 0/1, JSON is text, the Postgres tsvector is an FTS5 index over search_text and the pre-split cjk_tokens.
// assistant_control and show_quota are club's own.
const schema = `
CREATE TABLE IF NOT EXISTS agent_accounts (
  account_id text PRIMARY KEY NOT NULL,
  provider text NOT NULL,
  display_name text NOT NULL,
  status text NOT NULL,
  external_account_id text,
  bridge_token_hash text,
  last_synced_at integer,
  last_error text,
  created_at integer NOT NULL,
  updated_at integer NOT NULL,
  assistant_control text NOT NULL DEFAULT 'off',
  show_quota integer NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS agent_sessions (
  session_key text PRIMARY KEY NOT NULL,
  account_id text NOT NULL,
  provider text NOT NULL,
  external_id text NOT NULL,
  title text NOT NULL,
  kind text NOT NULL,
  parent_session_key text,
  can_send integer DEFAULT 0 NOT NULL,
  ref_json text DEFAULT '{}' NOT NULL,
  rank_time_ms integer DEFAULT 0 NOT NULL,
  on_top_rank_time_ms integer DEFAULT 0 NOT NULL,
  unread_count integer DEFAULT 0 NOT NULL,
  read_time_ms integer DEFAULT 0 NOT NULL,
  is_hidden integer DEFAULT 0 NOT NULL,
  preview_text text DEFAULT '' NOT NULL,
  preview_is_mine integer DEFAULT 0 NOT NULL,
  updated_at integer NOT NULL
);
CREATE INDEX IF NOT EXISTS agent_sessions_rank_idx
  ON agent_sessions (on_top_rank_time_ms DESC, rank_time_ms DESC, session_key DESC);
CREATE INDEX IF NOT EXISTS agent_sessions_account_idx ON agent_sessions (account_id);
CREATE TABLE IF NOT EXISTS agent_messages (
  session_key text NOT NULL,
  external_id text NOT NULL,
  position integer DEFAULT 0 NOT NULL,
  role text NOT NULL,
  blocks_json text NOT NULL,
  create_time_ms integer NOT NULL,
  search_text text DEFAULT '' NOT NULL,
  cjk_tokens text DEFAULT '' NOT NULL,
  PRIMARY KEY (session_key, external_id)
);
CREATE INDEX IF NOT EXISTS agent_messages_order_idx ON agent_messages (session_key, create_time_ms, position);
CREATE INDEX IF NOT EXISTS agent_messages_recency_idx ON agent_messages (create_time_ms DESC);
CREATE VIRTUAL TABLE IF NOT EXISTS agent_messages_fts USING fts5(search_text, cjk_tokens,
  content='agent_messages', content_rowid='rowid', tokenize='unicode61 remove_diacritics 0');
CREATE TRIGGER IF NOT EXISTS agent_messages_fts_insert AFTER INSERT ON agent_messages BEGIN
  INSERT INTO agent_messages_fts (rowid, search_text, cjk_tokens) VALUES (new.rowid, new.search_text, new.cjk_tokens);
END;
CREATE TRIGGER IF NOT EXISTS agent_messages_fts_delete AFTER DELETE ON agent_messages BEGIN
  INSERT INTO agent_messages_fts (agent_messages_fts, rowid, search_text, cjk_tokens)
    VALUES ('delete', old.rowid, old.search_text, old.cjk_tokens);
END;
CREATE TRIGGER IF NOT EXISTS agent_messages_fts_update AFTER UPDATE ON agent_messages BEGIN
  INSERT INTO agent_messages_fts (agent_messages_fts, rowid, search_text, cjk_tokens)
    VALUES ('delete', old.rowid, old.search_text, old.cjk_tokens);
  INSERT INTO agent_messages_fts (rowid, search_text, cjk_tokens) VALUES (new.rowid, new.search_text, new.cjk_tokens);
END;
CREATE TABLE IF NOT EXISTS agent_outbox (
  client_id text PRIMARY KEY NOT NULL,
  session_key text NOT NULL,
  account_id text NOT NULL,
  text text NOT NULL,
  status text NOT NULL,
  attempt_count integer DEFAULT 0 NOT NULL,
  last_error text,
  created_at integer NOT NULL,
  updated_at integer NOT NULL
);
CREATE INDEX IF NOT EXISTS agent_outbox_session_idx ON agent_outbox (session_key, created_at);
CREATE TABLE IF NOT EXISTS club_settings (
  key text PRIMARY KEY NOT NULL,
  value text NOT NULL
);
`

// SQLite is a Store in one SQLite file (WAL).
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// OpenSQLite opens (creating it, mode 0600, and its directory, 0700) the store at path.
func OpenSQLite(path string) (*SQLite, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	q := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One connection: writes are serialized in-process, and another process (an import) waits on busy_timeout.
	db.SetMaxOpenConns(1)
	s := &SQLite{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return s, nil
}

// OpenSQLiteReadOnly opens an existing store for reading only; it is never migrated or written. A WAL database
// read this way gets its -wal and -shm files created when they are missing (SQLite needs them to read alongside a
// writer); immutable instead reads the database file alone and creates nothing, which is only right when no
// process has the store open for writing and no -wal file is left over.
func OpenSQLiteReadOnly(path string, immutable bool) (*SQLite, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "query_only(1)")
	q.Set("mode", "ro")
	if immutable {
		q.Set("immutable", "1")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if version != schemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: schema version %d, this program reads %d", path, version, schemaVersion)
	}
	return &SQLite{db: db}, nil
}

func (s *SQLite) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("the store has schema version %d; this program knows up to %d", version, schemaVersion)
	}
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	_, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	return err
}

// Close closes the database.
func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) tx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

const accountColumns = `account_id, provider, display_name, status, external_account_id, last_synced_at, last_error,
  assistant_control, show_quota, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanAccount(r scanner) (Account, error) {
	var a Account
	var ext, lastErr sql.NullString
	var synced sql.NullInt64
	var show int
	err := r.Scan(&a.AccountID, &a.Provider, &a.DisplayName, &a.Status, &ext, &synced, &lastErr, &a.AssistantControl,
		&show, &a.CreatedAt, &a.UpdatedAt)
	a.ExternalAccountID, a.LastError, a.LastSyncedAt, a.ShowQuota = ext.String, lastErr.String, synced.Int64, show != 0
	return a, err
}

func (s *SQLite) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountColumns+` FROM agent_accounts WHERE status <> 'removed'
		ORDER BY created_at, account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLite) Account(ctx context.Context, id string) (Account, bool, error) {
	a, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM agent_accounts
		WHERE account_id = ? AND status <> 'removed'`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	return a, err == nil, err
}

func putAccount(ctx context.Context, q querier, a Account) error {
	control := a.AssistantControl
	if control == "" {
		control = "off"
	}
	_, err := q.ExecContext(ctx, `INSERT OR REPLACE INTO agent_accounts (`+accountColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, a.AccountID, a.Provider, a.DisplayName, a.Status,
		nullString(a.ExternalAccountID), nullInt(a.LastSyncedAt), nullString(a.LastError), control,
		boolInt(a.ShowQuota), a.CreatedAt, a.UpdatedAt)
	return err
}

func (s *SQLite) InsertAccount(ctx context.Context, a Account) error { return putAccount(ctx, s.db, a) }

func (s *SQLite) UpdateAccount(ctx context.Context, id string, p AccountPatch, now int64) error {
	set := []string{"updated_at = ?"}
	args := []any{now}
	add := func(col string, v any) {
		set = append(set, col+" = ?")
		args = append(args, v)
	}
	switch {
	case p.Status != nil && p.PreservePaused:
		set = append(set, "status = CASE WHEN status = ? THEN status ELSE ? END")
		args = append(args, protocol.AccountPaused, *p.Status)
	case p.Status != nil:
		add("status", *p.Status)
	}
	if p.DisplayName != nil {
		add("display_name", *p.DisplayName)
	}
	if p.LastSyncedAt != nil {
		add("last_synced_at", nullInt(*p.LastSyncedAt))
	}
	if p.LastError != nil {
		add("last_error", nullString(*p.LastError))
	}
	if p.AssistantControl != nil {
		add("assistant_control", *p.AssistantControl)
	}
	if p.ShowQuota != nil {
		add("show_quota", boolInt(*p.ShowQuota))
	}
	res, err := s.db.ExecContext(ctx, `UPDATE agent_accounts SET `+strings.Join(set, ", ")+` WHERE account_id = ?`,
		append(args, id)...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) RemoveAccount(ctx context.Context, id string, deleteData bool, now int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_accounts WHERE account_id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if !deleteData {
			_, err := tx.ExecContext(ctx, `UPDATE agent_accounts SET status = 'removed', updated_at = ? WHERE account_id = ?`, now, id)
			return err
		}
		for _, q := range []string{
			`DELETE FROM agent_messages WHERE session_key IN (SELECT session_key FROM agent_sessions WHERE account_id = ?)`,
			`DELETE FROM agent_sessions WHERE account_id = ?`,
			`DELETE FROM agent_outbox WHERE account_id = ?`,
			`DELETE FROM agent_accounts WHERE account_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

const sessionColumns = `s.session_key, s.account_id, s.provider, s.external_id, s.title, s.kind, s.parent_session_key,
  s.can_send, s.ref_json, s.rank_time_ms, s.on_top_rank_time_ms, s.unread_count, s.read_time_ms, s.is_hidden,
  s.preview_text, s.preview_is_mine, s.updated_at`

func scanSession(r scanner) (Session, error) {
	var x Session
	var parent sql.NullString
	var ref string
	var canSend, hidden, mine int
	err := r.Scan(&x.SessionKey, &x.AccountID, &x.Provider, &x.ExternalID, &x.Title, &x.Kind, &parent, &canSend, &ref,
		&x.RankTimeMs, &x.OnTopRankTimeMs, &x.UnreadCount, &x.ReadTimeMs, &hidden, &x.PreviewText, &mine, &x.UpdatedAt)
	if err != nil {
		return x, err
	}
	x.ParentSessionKey, x.CanSend, x.IsHidden, x.PreviewIsMine = parent.String, canSend != 0, hidden != 0, mine != 0
	x.Ref, x.AvatarURL = readRef(ref)
	return x, nil
}

// readRef splits ref_json into the provider's ids and the avatar the Lark gateway keeps among them.
func readRef(raw string) (map[string]string, string) {
	var all map[string]any
	_ = json.Unmarshal([]byte(raw), &all)
	ref := map[string]string{}
	for k, v := range all {
		if s, ok := v.(string); ok {
			ref[k] = s
		}
	}
	avatar := ref["avatarUrl"]
	delete(ref, "avatarUrl")
	return ref, avatar
}

func refJSON(ref map[string]string, avatar string) string {
	all := maps.Clone(ref)
	if all == nil {
		all = map[string]string{}
	}
	if avatar != "" {
		all["avatarUrl"] = avatar
	}
	b, _ := json.Marshal(all)
	return string(b)
}

func putSession(ctx context.Context, q querier, x Session) error {
	_, err := q.ExecContext(ctx, `INSERT OR REPLACE INTO agent_sessions (session_key, account_id, provider, external_id,
		title, kind, parent_session_key, can_send, ref_json, rank_time_ms, on_top_rank_time_ms, unread_count,
		read_time_ms, is_hidden, preview_text, preview_is_mine, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, x.SessionKey, x.AccountID, x.Provider, x.ExternalID,
		x.Title, x.Kind, nullString(x.ParentSessionKey), boolInt(x.CanSend), refJSON(x.Ref, x.AvatarURL), x.RankTimeMs,
		x.OnTopRankTimeMs, x.UnreadCount, x.ReadTimeMs, boolInt(x.IsHidden), x.PreviewText, boolInt(x.PreviewIsMine),
		x.UpdatedAt)
	return err
}

func sessionByKey(ctx context.Context, q querier, key string) (Session, bool, error) {
	x, err := scanSession(q.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM agent_sessions s WHERE s.session_key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	return x, err == nil, err
}

func (s *SQLite) UpsertSessions(ctx context.Context, sessions []Session, now int64) (int, error) {
	changed := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		changed = 0
		for _, in := range sessions {
			old, ok, err := sessionByKey(ctx, tx, in.SessionKey)
			if err != nil {
				return err
			}
			next := in
			if !ok {
				next.ReadTimeMs, next.UpdatedAt = max(now, in.RankTimeMs), now
				next.OnTopRankTimeMs, next.UnreadCount, next.IsHidden, next.PreviewText, next.PreviewIsMine = 0, 0, false, "", false
			} else {
				next = old
				next.Title, next.Kind, next.ParentSessionKey, next.CanSend = in.Title, in.Kind, in.ParentSessionKey, in.CanSend
				next.AvatarURL, next.Ref = in.AvatarURL, cloneRef(in.Ref)
				next.RankTimeMs = max(old.RankTimeMs, in.RankTimeMs)
				if !sessionChanged(old, next) {
					continue
				}
				next.UpdatedAt = now
			}
			if err := putSession(ctx, tx, next); err != nil {
				return err
			}
			changed++
		}
		return nil
	})
	return changed, err
}

const messageColumns = `session_key, external_id, position, role, blocks_json, create_time_ms`

func scanMessage(r scanner) (Message, error) {
	var m Message
	var blocks string
	if err := r.Scan(&m.SessionKey, &m.ExternalID, &m.Position, &m.Role, &blocks, &m.CreateTimeMs); err != nil {
		return m, err
	}
	if json.Unmarshal([]byte(blocks), &m.Blocks) != nil || m.Blocks == nil {
		m.Blocks = []protocol.Block{}
	}
	return m, nil
}

// putMessage upserts one message; it reports whether anything changed.
func putMessage(ctx context.Context, q querier, key string, m Message, now int64) (bool, error) {
	m.SessionKey = key
	old, err := scanMessage(q.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM agent_messages
		WHERE session_key = ? AND external_id = ?`, key, m.ExternalID))
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if m.CreateTimeMs == 0 {
		m.CreateTimeMs = now
		if exists {
			m.CreateTimeMs = old.CreateTimeMs
		}
	}
	if exists && sameMessage(old, m) {
		return false, nil
	}
	text := BlocksText(m.Blocks)
	_, err = q.ExecContext(ctx, `INSERT INTO agent_messages (session_key, external_id, position, role, blocks_json,
		create_time_ms, search_text, cjk_tokens) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (session_key, external_id) DO UPDATE SET position = excluded.position, role = excluded.role,
		blocks_json = excluded.blocks_json, create_time_ms = excluded.create_time_ms,
		search_text = excluded.search_text, cjk_tokens = excluded.cjk_tokens`,
		key, m.ExternalID, m.Position, m.Role, blocksJSON(m.Blocks), m.CreateTimeMs, text, CJKTokens(text))
	return err == nil, err
}

func (s *SQLite) UpsertMessages(ctx context.Context, key string, messages []Message, now int64) (int, error) {
	changed := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		changed = 0
		for _, m := range messages {
			ok, err := putMessage(ctx, tx, key, m, now)
			if err != nil {
				return err
			}
			if ok {
				changed++
			}
		}
		return refreshSession(ctx, tx, key, now)
	})
	return changed, err
}

// refreshSession recomputes a session's rank, preview and unread count from its messages.
func refreshSession(ctx context.Context, tx *sql.Tx, key string, now int64) error {
	x, ok, err := sessionByKey(ctx, tx, key)
	if err != nil || !ok {
		return err
	}
	next := x
	var created int64
	var text, role string
	err = tx.QueryRowContext(ctx, `SELECT create_time_ms, search_text, role FROM agent_messages
		WHERE session_key = ? AND role IN ('user', 'assistant') AND search_text <> ''
		ORDER BY create_time_ms DESC, position DESC, external_id DESC LIMIT 1`, key).Scan(&created, &text, &role)
	switch {
	case err == nil:
		next.RankTimeMs = max(x.RankTimeMs, created)
		next.PreviewText, next.PreviewIsMine = text, role == protocol.RoleUser
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_messages m
		WHERE m.session_key = ?1 AND m.role = 'assistant' AND m.create_time_ms > max(?2, coalesce((
		  SELECT max(u.create_time_ms) FROM agent_messages u WHERE u.session_key = ?1 AND u.role = 'user'), 0))`,
		key, x.ReadTimeMs).Scan(&next.UnreadCount); err != nil {
		return err
	}
	if next.RankTimeMs == x.RankTimeMs && next.PreviewText == x.PreviewText && next.PreviewIsMine == x.PreviewIsMine &&
		next.UnreadCount == x.UnreadCount {
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE agent_sessions SET rank_time_ms = ?, preview_text = ?, preview_is_mine = ?,
		unread_count = ?, updated_at = ? WHERE session_key = ?`, next.RankTimeMs, next.PreviewText,
		boolInt(next.PreviewIsMine), next.UnreadCount, now, key)
	return err
}

const listedAccount = `JOIN agent_accounts a ON a.account_id = s.account_id AND a.status IN ('active', 'paused', 'error')`

func (s *SQLite) Session(ctx context.Context, key string) (Session, bool, error) {
	x, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM agent_sessions s `+listedAccount+`
		WHERE s.session_key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	return x, err == nil, err
}

func (s *SQLite) sessions(ctx context.Context, q string, args ...any) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *SQLite) Sessions(ctx context.Context, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = -1
	}
	return s.sessions(ctx, `SELECT `+sessionColumns+` FROM agent_sessions s `+listedAccount+`
		WHERE s.is_hidden = 0
		ORDER BY (s.on_top_rank_time_ms <> 0) DESC, s.on_top_rank_time_ms DESC,
		  (CASE WHEN s.on_top_rank_time_ms = 0 THEN s.rank_time_ms ELSE 0 END) DESC, s.session_key DESC
		LIMIT ?`, limit)
}

func (s *SQLite) AccountSessions(ctx context.Context, id string) ([]Session, error) {
	return s.sessions(ctx, `SELECT `+sessionColumns+` FROM agent_sessions s WHERE s.account_id = ?
		ORDER BY s.rank_time_ms DESC, s.session_key DESC`, id)
}

func (s *SQLite) ActiveSessionKeys(ctx context.Context, id string, since int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.session_key FROM agent_sessions s WHERE s.account_id = ?1
		AND (EXISTS (SELECT 1 FROM agent_messages m WHERE m.session_key = s.session_key AND m.create_time_ms >= ?2)
		  OR EXISTS (SELECT 1 FROM agent_outbox o WHERE o.session_key = s.session_key AND o.created_at >= ?2))
		ORDER BY s.session_key`, id, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *SQLite) Messages(ctx context.Context, key string, limit int, before *Cursor) ([]Message, bool, error) {
	q := `SELECT ` + messageColumns + ` FROM agent_messages WHERE session_key = ?`
	args := []any{key}
	if before != nil {
		q += ` AND (create_time_ms, position, external_id) < (?, ?, ?)`
		args = append(args, before.CreateTimeMs, before.Position, before.ExternalID)
	}
	q += ` ORDER BY create_time_ms DESC, position DESC, external_id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit+1)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := limit > 0 && len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, hasMore, nil
}

func (s *SQLite) MarkRead(ctx context.Context, key string, now int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agent_sessions SET read_time_ms = max(read_time_ms, ?1, rank_time_ms),
		unread_count = 0, updated_at = ?1 WHERE session_key = ?2`, now, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) Counts(ctx context.Context, id string) (Counts, error) {
	var c Counts
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM agent_sessions WHERE account_id = ?1),
		(SELECT count(*) FROM agent_messages m JOIN agent_sessions s ON s.session_key = m.session_key WHERE s.account_id = ?1),
		(SELECT coalesce(max(m.create_time_ms), 0) FROM agent_messages m JOIN agent_sessions s ON s.session_key = m.session_key
		  WHERE s.account_id = ?1)`, id).Scan(&c.Sessions, &c.Messages, &c.LatestMessageMs)
	return c, err
}

const outboxColumns = `client_id, session_key, account_id, text, status, attempt_count, last_error, created_at, updated_at`

func scanOutbox(r scanner) (Outbox, error) {
	var o Outbox
	var lastErr sql.NullString
	err := r.Scan(&o.ClientID, &o.SessionKey, &o.AccountID, &o.Text, &o.Status, &o.AttemptCount, &lastErr, &o.CreatedAt,
		&o.UpdatedAt)
	o.LastError = lastErr.String
	return o, err
}

func (s *SQLite) outboxRows(ctx context.Context, q string, args ...any) ([]Outbox, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outbox
	for rows.Next() {
		o, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *SQLite) one(ctx context.Context, q string, args ...any) (Outbox, bool, error) {
	rows, err := s.outboxRows(ctx, q, args...)
	if err != nil || len(rows) == 0 {
		return Outbox{}, false, err
	}
	return rows[0], true, nil
}

func (s *SQLite) EnqueueOutbox(ctx context.Context, o Outbox) (Outbox, bool, error) {
	row, ok, err := s.one(ctx, `INSERT INTO agent_outbox (client_id, session_key, account_id, text, status, created_at,
		updated_at) VALUES (?, ?, ?, ?, 'pending', ?, ?) ON CONFLICT (client_id) DO NOTHING RETURNING `+outboxColumns,
		o.ClientID, o.SessionKey, o.AccountID, o.Text, o.CreatedAt, o.CreatedAt)
	if err != nil || ok {
		return row, ok, err
	}
	existing, _, err := s.Outbox(ctx, o.ClientID)
	return existing, false, err
}

func (s *SQLite) Outbox(ctx context.Context, clientID string) (Outbox, bool, error) {
	return s.one(ctx, `SELECT `+outboxColumns+` FROM agent_outbox WHERE client_id = ?`, clientID)
}

func (s *SQLite) ClaimOutbox(ctx context.Context, clientID string, now int64) (Outbox, bool, error) {
	return s.one(ctx, `UPDATE agent_outbox SET status = 'claimed', attempt_count = attempt_count + 1, updated_at = ?
		WHERE client_id = ? AND status = 'pending' RETURNING `+outboxColumns, now, clientID)
}

func (s *SQLite) SettleOutbox(ctx context.Context, clientID, status, lastError string, now int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agent_outbox SET status = ?, last_error = ?, updated_at = ? WHERE client_id = ?`,
		status, nullString(lastError), now, clientID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) affected(ctx context.Context, q string, args ...any) (bool, error) {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *SQLite) CancelOutbox(ctx context.Context, clientID, key string, now int64) (bool, error) {
	return s.affected(ctx, `UPDATE agent_outbox SET status = 'failed', last_error = 'CANCELLED', updated_at = ?
		WHERE client_id = ? AND session_key = ? AND status = 'pending'`, now, clientID, key)
}

func (s *SQLite) DismissOutbox(ctx context.Context, clientID, key string) (bool, error) {
	return s.affected(ctx, `DELETE FROM agent_outbox WHERE client_id = ? AND session_key = ? AND status = 'failed'`,
		clientID, key)
}

func (s *SQLite) AbandonClaimedOutbox(ctx context.Context, now int64) ([]Outbox, error) {
	out, err := s.outboxRows(ctx, `UPDATE agent_outbox SET status = 'unconfirmed', last_error = 'SEND_UNCONFIRMED',
		updated_at = ? WHERE status = 'claimed' RETURNING `+outboxColumns, now)
	sortOutbox(out)
	return out, err
}

func (s *SQLite) ConfirmOutbox(ctx context.Context, clientID, status, lastError string, now int64) (bool, error) {
	return s.affected(ctx, `UPDATE agent_outbox SET status = ?, last_error = ?, updated_at = ?
		WHERE client_id = ? AND status = 'unconfirmed'`, status, nullString(lastError), now, clientID)
}

func (s *SQLite) PendingOutbox(ctx context.Context) ([]Outbox, error) {
	return s.outboxRows(ctx, `SELECT `+outboxColumns+` FROM agent_outbox WHERE status = 'pending'
		ORDER BY created_at, client_id`)
}

func (s *SQLite) LocalMessages(ctx context.Context, key string, since int64) ([]Outbox, error) {
	return s.outboxRows(ctx, `SELECT `+outboxColumns+` FROM agent_outbox o WHERE o.session_key = ?1
		AND (o.status <> 'sent' OR o.updated_at >= ?2)
		AND NOT EXISTS (SELECT 1 FROM agent_messages m WHERE m.session_key = o.session_key AND m.role = 'user'
		  AND m.create_time_ms >= o.created_at - ?3 AND trim(m.search_text, ' ') = trim(o.text, ' '))
		ORDER BY o.created_at, o.client_id`, key, since, echoWindowMs)
}

const hitColumns = `m.session_key, m.external_id, m.search_text, m.role, m.create_time_ms, s.title, s.provider`

func (s *SQLite) hits(ctx context.Context, q string, args ...any) ([]protocol.SearchHit, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.SearchHit
	for rows.Next() {
		var h protocol.SearchHit
		if err := rows.Scan(&h.SessionKey, &h.ExternalID, &h.Text, &h.Role, &h.CreateTimeMs, &h.Title, &h.Provider); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *SQLite) Search(ctx context.Context, q string, limit int) ([]protocol.SearchHit, error) {
	query := parseQuery(q)
	if query.text == "" {
		return []protocol.SearchHit{}, nil
	}
	hits := []protocol.SearchHit{}
	fts := query.fts()
	if fts != "" {
		matched, err := s.hits(ctx, `SELECT `+hitColumns+` FROM agent_messages_fts f
			JOIN agent_messages m ON m.rowid = f.rowid
			JOIN agent_sessions s ON s.session_key = m.session_key `+listedAccount+`
			WHERE agent_messages_fts MATCH ?
			ORDER BY m.create_time_ms DESC, m.session_key, m.external_id LIMIT ?`, fts, limit)
		if err != nil {
			return nil, err
		}
		hits = append(hits, matched...)
	}
	if len(hits) < limit {
		notMatched := ``
		args := []any{substringScan, query.text}
		if fts != "" {
			notMatched = ` AND m.rowid NOT IN (SELECT rowid FROM agent_messages_fts WHERE agent_messages_fts MATCH ?)`
			args = append(args, fts)
		}
		more, err := s.hits(ctx, `WITH recent AS (SELECT rowid, * FROM agent_messages
			  ORDER BY create_time_ms DESC, session_key, external_id LIMIT ?)
			SELECT `+hitColumns+` FROM recent m
			JOIN agent_sessions s ON s.session_key = m.session_key `+listedAccount+`
			WHERE instr(lower(m.search_text), lower(?)) > 0`+notMatched+`
			ORDER BY m.create_time_ms DESC, m.session_key, m.external_id LIMIT ?`, append(args, limit-len(hits))...)
		if err != nil {
			return nil, err
		}
		hits = append(hits, more...)
	}
	sortHits(hits)
	return hits, nil
}

func (s *SQLite) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM club_settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *SQLite) PutSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO club_settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *SQLite) Import(ctx context.Context, rows Import) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, a := range rows.Accounts {
			if err := putAccount(ctx, tx, a); err != nil {
				return err
			}
		}
		for _, x := range rows.Sessions {
			if err := putSession(ctx, tx, x); err != nil {
				return err
			}
		}
		for _, m := range rows.Messages {
			if _, err := putMessage(ctx, tx, m.SessionKey, m, m.CreateTimeMs); err != nil {
				return err
			}
		}
		for _, o := range rows.Outbox {
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_outbox (`+outboxColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (client_id) DO NOTHING`, o.ClientID, o.SessionKey, o.AccountID, o.Text, o.Status,
				o.AttemptCount, nullString(o.LastError), o.CreatedAt, o.UpdatedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// Package store keeps one space's accounts, sessions, messages and outbox. Store is the interface the core uses;
// SQLite keeps a space in one file with the same tables and columns as the Lark gateway's agent_accounts /
// agent_sessions / agent_messages / agent_outbox, and Memory keeps it in memory for tests. One test suite runs
// against both.
package store

import (
	"context"
	"errors"

	"github.com/nwverse/personal-agent-club/internal/protocol"
)

// ErrNotFound is returned by updates of a row that does not exist.
var ErrNotFound = errors.New("store: not found")

// AccountRemoved is the status of an unpaired account whose data was kept: it and its sessions are no longer
// listed, as in the Lark gateway.
const AccountRemoved = "removed"

// Account is a paired account. Credentials are not here: they live in the vault.
type Account struct {
	AccountID         string
	Provider          string
	DisplayName       string
	ExternalAccountID string
	Status            string // protocol.AccountActive, AccountPaused, AccountError, or AccountRemoved
	LastSyncedAt      int64
	LastError         string
	// AssistantControl is the account's assistant control level (core.Control), "off" when new.
	AssistantControl string
	ShowQuota        bool
	CreatedAt        int64
	UpdatedAt        int64
}

// AccountPatch changes the fields that are set.
type AccountPatch struct {
	Status           *string
	PreservePaused   bool // Leave a paused account's status alone when applying Status.
	DisplayName      *string
	LastSyncedAt     *int64
	LastError        *string
	AssistantControl *string
	ShowQuota        *bool
}

// Session is one stored session.
type Session struct {
	SessionKey       string
	AccountID        string
	Provider         string
	ExternalID       string
	Title            string
	Kind             string
	ParentSessionKey string
	CanSend          bool
	AvatarURL        string
	Ref              map[string]string
	RankTimeMs       int64
	OnTopRankTimeMs  int64
	ReadTimeMs       int64
	UnreadCount      int
	IsHidden         bool
	PreviewText      string
	PreviewIsMine    bool
	UpdatedAt        int64
}

// Message is one stored message.
type Message struct {
	SessionKey   string
	ExternalID   string
	Role         string
	Blocks       []protocol.Block
	CreateTimeMs int64
	Position     int64
}

// Text is the message's searchable text (search_text).
func (m Message) Text() string { return BlocksText(m.Blocks) }

// Cursor is a position in a session's messages: the page before it is older.
type Cursor struct {
	CreateTimeMs int64
	Position     int64
	ExternalID   string
}

// Outbox is one send. Status is one of protocol.LocalPending, LocalClaimed, LocalSent, LocalUnconfirmed,
// LocalFailed.
type Outbox struct {
	ClientID     string
	SessionKey   string
	AccountID    string
	Text         string
	Status       string
	LastError    string
	AttemptCount int
	CreatedAt    int64
	UpdatedAt    int64
}

// Counts are what one account holds.
type Counts struct {
	Sessions        int
	Messages        int
	LatestMessageMs int64
}

// Import is rows to write as they are (club.import). Accounts and sessions replace rows with the same key, messages
// are upserted, and outbox rows whose client id is taken are left alone.
type Import struct {
	Accounts []Account
	Sessions []Session
	Messages []Message
	Outbox   []Outbox
}

// Store is one space's data.
type Store interface {
	// ListAccounts are the paired accounts (not removed), oldest first.
	ListAccounts(ctx context.Context) ([]Account, error)
	// Account is a paired account; a removed one is not found.
	Account(ctx context.Context, accountID string) (Account, bool, error)
	InsertAccount(ctx context.Context, a Account) error
	UpdateAccount(ctx context.Context, accountID string, p AccountPatch, now int64) error
	// RemoveAccount unpairs the account: with deleteData its sessions, messages and outbox go too; otherwise it is
	// marked removed and its data stays (unlisted) for a later export.
	RemoveAccount(ctx context.Context, accountID string, deleteData bool, now int64) error

	// UpsertSessions writes the sessions an account lists; it returns how many were new or changed. A new session
	// starts read, so a backfill does not light up as unread.
	UpsertSessions(ctx context.Context, sessions []Session, now int64) (int, error)
	// UpsertMessages writes one page of a session's messages and refreshes the session's rank, preview and unread
	// count; it returns how many were new or changed. A message without a time keeps the time it was first seen.
	UpsertMessages(ctx context.Context, sessionKey string, messages []Message, now int64) (int, error)
	// Session is a session of a listed account.
	Session(ctx context.Context, sessionKey string) (Session, bool, error)
	// Sessions are the listed accounts' sessions that are not hidden: pinned first, then most recent.
	Sessions(ctx context.Context, limit int) ([]Session, error)
	// AccountSessions are all of an account's sessions, most recent first.
	AccountSessions(ctx context.Context, accountID string) ([]Session, error)
	// ActiveSessionKeys are the account's sessions with a stored message or a send since `since`.
	ActiveSessionKeys(ctx context.Context, accountID string, since int64) ([]string, error)
	// Messages is the page of limit messages before the cursor (nil: the newest), oldest first.
	Messages(ctx context.Context, sessionKey string, limit int, before *Cursor) (page []Message, hasMore bool, err error)
	MarkRead(ctx context.Context, sessionKey string, now int64) error
	// Counts are an account's sessions and messages, removed or not.
	Counts(ctx context.Context, accountID string) (Counts, error)

	// EnqueueOutbox writes a pending send; inserted is false when its client id is taken (the existing row is
	// returned).
	EnqueueOutbox(ctx context.Context, o Outbox) (row Outbox, inserted bool, err error)
	Outbox(ctx context.Context, clientID string) (Outbox, bool, error)
	// ClaimOutbox moves pending to claimed once; a second claimer gets ok false.
	ClaimOutbox(ctx context.Context, clientID string, now int64) (Outbox, bool, error)
	SettleOutbox(ctx context.Context, clientID, status, lastError string, now int64) error
	// CancelOutbox fails a pending send of the session; ok is false when there is none.
	CancelOutbox(ctx context.Context, clientID, sessionKey string, now int64) (bool, error)
	// DismissOutbox deletes a failed send of the session, releasing its client id; ok is false when there is none.
	DismissOutbox(ctx context.Context, clientID, sessionKey string) (bool, error)
	// AbandonClaimedOutbox marks every claimed send unconfirmed (after a restart it may have gone out).
	AbandonClaimedOutbox(ctx context.Context, now int64) ([]Outbox, error)
	// ConfirmOutbox moves an unconfirmed send to sent or failed; ok is false when it was not unconfirmed.
	ConfirmOutbox(ctx context.Context, clientID, status, lastError string, now int64) (bool, error)
	PendingOutbox(ctx context.Context) ([]Outbox, error)
	// LocalMessages are the session's sends the agent has not echoed yet: not sent, or sent since `since`, with no
	// user message of the same text from a minute before the send on.
	LocalMessages(ctx context.Context, sessionKey string, since int64) ([]Outbox, error)

	// Search finds messages of listed accounts, newest first: full-text matches of the query's words or of its
	// CJK n-grams, then substring matches among the most recent messages.
	Search(ctx context.Context, q string, limit int) ([]protocol.SearchHit, error)

	// Setting is a space setting's JSON, ok false when unset.
	Setting(ctx context.Context, key string) (value string, ok bool, err error)
	PutSetting(ctx context.Context, key, value string) error

	// Import writes rows as they are.
	Import(ctx context.Context, rows Import) error
}

// listed says whether an account's sessions are shown.
func listed(status string) bool {
	return status == protocol.AccountActive || status == protocol.AccountPaused || status == protocol.AccountError
}

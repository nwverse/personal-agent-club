// Package protocol holds the wire shapes of the Persona agent_sources protocol (agentsrc.*), field for field as the
// Persona hub reads them, plus the backward-compatible additions club makes (preview fields, local.error, quota) and
// the shapes of club's own management namespace (club.*).
//
// JSON field names are part of the contract: results use the hub's camelCase names, call parameters use snake_case.
// Golden tests pin both.
package protocol

import (
	"encoding/json"
	"errors"
)

// Version is the version of the protocol club speaks, reported by club.info. It changes only when a field or method
// changes incompatibly.
const Version = 1

// MaxText is the longest message text a source accepts, in UTF-16 code units (as JavaScript counts characters).
const MaxText = 20000

// Namespaces.
const (
	AgentSourceNamespace = "agentsrc"
	ClubNamespace        = "club"
)

// agentsrc.* methods, without the namespace.
const (
	MethodStatus        = "status"
	MethodSessions      = "sessions"
	MethodMessages      = "messages"
	MethodSend          = "send"
	MethodDeleteMessage = "delete_message"
	MethodMarkRead      = "mark_read"
	MethodSearch        = "search"
)

// AgentSourceMethods are every agentsrc.* method, in the order the protocol lists them.
var AgentSourceMethods = []string{MethodStatus, MethodSessions, MethodMessages, MethodSend, MethodDeleteMessage,
	MethodMarkRead, MethodSearch}

// EventUpdated is the event a source sends when sessions changed (full name, with the namespace).
const EventUpdated = "agentsrc.updated"

// club.* methods, full names.
const (
	MethodClubInfo                  = "club.info"
	MethodClubAccountsAdd           = "club.accounts.add"
	MethodClubAccountsUpdate        = "club.accounts.update"
	MethodClubAccountsRemove        = "club.accounts.remove"
	MethodClubAccountsReplaceCred   = "club.accounts.replace_credential"
	MethodClubAccountsSync          = "club.accounts.sync"
	MethodClubLive                  = "club.live"
	MethodClubSettingsGet           = "club.settings.get"
	MethodClubSettingsSet           = "club.settings.set"
	MethodClubSessionsStart         = "club.sessions.start"
	MethodClubSessionsInterrupt     = "club.sessions.interrupt"
	MethodClubImport                = "club.import"
	MethodClubExport                = "club.export"
	MethodClubLoginsGrokBotStart    = "club.logins.grok_bot.start"
	MethodClubLoginsGrokBotPoll     = "club.logins.grok_bot.poll"
	MethodClubLoginsGrokBotCancel   = "club.logins.grok_bot.cancel"
	MethodClubGrokBotTakeoverStart  = "club.grok_bot.takeover.start"
	MethodClubGrokBotTakeoverStatus = "club.grok_bot.takeover.status"
	MethodClubGrokBotTakeoverPlan   = "club.grok_bot.takeover.plan"
	MethodClubGrokBotTakeoverApply  = "club.grok_bot.takeover.apply"
	MethodClubLoginsBrowserStart    = "club.logins.browser.start"
	MethodClubLoginsBrowserFrame    = "club.logins.browser.frame"
	MethodClubLoginsBrowserInput    = "club.logins.browser.input"
	MethodClubLoginsBrowserFinish   = "club.logins.browser.finish"
	MethodClubLoginsBrowserCancel   = "club.logins.browser.cancel"
	MethodClubOutboxDismiss         = "club.outbox.dismiss"
)

// ClubMethods are the club.* methods this version answers.
var ClubMethods = []string{MethodClubInfo, MethodClubAccountsAdd, MethodClubAccountsUpdate, MethodClubAccountsRemove,
	MethodClubAccountsReplaceCred, MethodClubAccountsSync, MethodClubLive, MethodClubSettingsGet, MethodClubSettingsSet,
	MethodClubImport, MethodClubLoginsGrokBotStart, MethodClubLoginsGrokBotPoll, MethodClubLoginsGrokBotCancel,
	MethodClubGrokBotTakeoverStart, MethodClubGrokBotTakeoverStatus, MethodClubGrokBotTakeoverPlan,
	MethodClubGrokBotTakeoverApply, MethodClubOutboxDismiss}

// ClubStubMethods are the club.* methods the design names that this version answers with unsupported.
var ClubStubMethods = []string{MethodClubSessionsStart, MethodClubSessionsInterrupt, MethodClubExport,
	MethodClubLoginsBrowserStart, MethodClubLoginsBrowserFrame, MethodClubLoginsBrowserInput,
	MethodClubLoginsBrowserFinish, MethodClubLoginsBrowserCancel}

// Error codes a source answers with. Any other error carries only a message.
const (
	CodeNotFound    = "not_found"
	CodeInvalid     = "invalid"
	CodeConflict    = "conflict"
	CodeUnsupported = "unsupported"
)

// Error is a refusal on the wire: a code the caller acts on, a message for people (never a secret or a message
// body) and, optionally, a reason: a stable upper-case code naming the refusal more precisely (the Lark gateway's
// agent bridge error codes, so the gateway can answer its own HTTP status). Hubs that do not know reason ignore it.
type Error struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// WireError lets the Persona SDK put the code on the wire (ext.Coded); the reason goes in the error's data.
func (e *Error) WireError() (string, json.RawMessage) {
	if e.Reason == "" {
		return e.Code, nil
	}
	data, _ := json.Marshal(map[string]string{"reason": e.Reason})
	return e.Code, data
}

// NotFound, Invalid, Conflict and Unsupported build an Error with that code.
func NotFound(msg string) *Error    { return &Error{Code: CodeNotFound, Message: msg} }
func Invalid(msg string) *Error     { return &Error{Code: CodeInvalid, Message: msg} }
func Conflict(msg string) *Error    { return &Error{Code: CodeConflict, Message: msg} }
func Unsupported(msg string) *Error { return &Error{Code: CodeUnsupported, Message: msg} }

// Because returns a copy of e with reason set.
func (e *Error) Because(reason string) *Error {
	c := *e
	c.Reason = reason
	return &c
}

// Failure is an error with a reason and no protocol code (a provider failure: HTTP 502 over serve).
func Failure(reason, msg string) *Error { return &Error{Reason: reason, Message: msg} }

// ReasonOf is err's reason, "" when it has none.
func ReasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// Reasons. They are the error codes of the Lark gateway's built-in agent bridge (service and routes), with the
// HTTP status the gateway answers them with; a provider's own code (AUTH_FAILED, RATE_LIMITED, MODEL_UNAVAILABLE…)
// may appear as a reason too, where the gateway passes it on.
const (
	ReasonDisabled            = "AGENT_BRIDGE_DISABLED"      // 404; never sent by club, which is never off
	ReasonInvalidInput        = "INVALID_AGENT_BRIDGE_INPUT" // 400
	ReasonInvalidCursor       = "INVALID_CURSOR"             // 400
	ReasonSessionNotFound     = "AGENT_SESSION_NOT_FOUND"    // 404
	ReasonAccountNotFound     = "AGENT_ACCOUNT_NOT_FOUND"    // 404
	ReasonLoginNotFound       = "LOGIN_NOT_FOUND"            // 404
	ReasonSessionReadOnly     = "SESSION_READ_ONLY"          // 409
	ReasonAccountPaused       = "ACCOUNT_PAUSED"             // 409
	ReasonClientIDInUse       = "CLIENT_ID_IN_USE"           // 409
	ReasonNotPending          = "NOT_PENDING"                // 409
	ReasonAccountMismatch     = "ACCOUNT_MISMATCH"           // 409
	ReasonProviderMismatch    = "PROVIDER_MISMATCH"          // 409
	ReasonLoginLimit          = "LOGIN_LIMIT"                // 429
	ReasonProviderUnavailable = "PROVIDER_UNAVAILABLE"       // 422
	ReasonNotSignedIn         = "NOT_SIGNED_IN"              // 422; browser sign-ins, not in this version
	ReasonBrowserUnavailable  = "BROWSER_UNAVAILABLE"        // 503; browser sign-ins, not in this version
	ReasonBrowserLoginLimit   = "BROWSER_LOGIN_LIMIT"        // 429; browser sign-ins, not in this version
	// ReasonAlreadyPaired is club's own: the same remote account is already paired in this space (409).
	ReasonAlreadyPaired = "ACCOUNT_ALREADY_PAIRED"
)

// CodeOf is err's protocol code, "" when it has none.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Provider is one provider a source knows, and whether it can be used now.
type Provider struct {
	Provider   string `json:"provider"`
	Connection string `json:"connection,omitempty"` // server | browser
	Available  bool   `json:"available"`
}

// Connections.
const (
	ConnectionServer  = "server"
	ConnectionBrowser = "browser"
)

// Account statuses.
const (
	AccountActive = "active"
	AccountPaused = "paused"
	AccountError  = "error"
)

// Account is a paired account. Quota is club's optional addition (§6.5 of the design).
type Account struct {
	AccountID         string `json:"accountId"`
	Provider          string `json:"provider"`
	DisplayName       string `json:"displayName,omitempty"`
	Status            string `json:"status,omitempty"`
	ExternalAccountID string `json:"externalAccountId,omitempty"`
	LastSyncedAt      int64  `json:"lastSyncedAt,omitempty"`
	LastError         string `json:"lastError,omitempty"`
	SessionCount      int    `json:"sessionCount"`
	AssistantControl  string `json:"assistantControl,omitempty"`
	ShowQuota         bool   `json:"showQuota,omitempty"`
	Quota             *Quota `json:"quota,omitempty"`
}

// Session kinds.
const (
	KindMain = "main"
	KindSide = "side"
	KindTask = "task"
)

// Session is one conversation with an agent. AvatarURL is in the protocol; PreviewText, PreviewIsMine,
// OnTopRankTimeMs and Quota are club's backward-compatible additions, ignored by hubs that do not know them.
type Session struct {
	SessionKey       string `json:"sessionKey"`
	AccountID        string `json:"accountId"`
	Provider         string `json:"provider"`
	Title            string `json:"title"`
	Kind             string `json:"kind"`
	ParentSessionKey string `json:"parentSessionKey,omitempty"`
	CanSend          bool   `json:"canSend"`
	UnreadCount      int    `json:"unreadCount"`
	RankTimeMs       int64  `json:"rankTimeMs"`
	AvatarURL        string `json:"avatarUrl,omitempty"`
	PreviewText      string `json:"previewText,omitempty"`
	PreviewIsMine    bool   `json:"previewIsMine,omitempty"`
	OnTopRankTimeMs  int64  `json:"onTopRankTimeMs,omitempty"`
	Quota            *Quota `json:"quota,omitempty"`
}

// Block is one block of a message: text, tool-call {name,status,summary,input}, tool-result {name,text}, status
// {status,text}, attachment {name,kind,url}, error {text}, or anything newer, whose JSON is kept in Raw and written
// back unchanged.
type Block struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	Name    string          `json:"name,omitempty"`
	Status  string          `json:"status,omitempty"`
	Summary string          `json:"summary,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Kind    string          `json:"kind,omitempty"`
	URL     string          `json:"url,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

func (b *Block) UnmarshalJSON(data []byte) error {
	type plain Block
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*b = Block(p)
	b.Raw = append(json.RawMessage(nil), data...)
	return nil
}

func (b Block) MarshalJSON() ([]byte, error) {
	if len(b.Raw) > 0 {
		return b.Raw, nil
	}
	type plain Block
	return json.Marshal(plain(b))
}

// TextBlock is a text block.
func TextBlock(text string) Block { return Block{Type: "text", Text: text} }

// Outbox statuses, as carried in Local.Status.
const (
	LocalPending     = "pending"
	LocalClaimed     = "claimed"
	LocalSent        = "sent"
	LocalUnconfirmed = "unconfirmed"
	LocalFailed      = "failed"
)

// Local marks a message sent through the source that the agent has not echoed yet. Error is club's addition.
type Local struct {
	ClientID string `json:"clientId"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

// Message roles seen so far; others are possible.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	RoleSystem    = "system"
)

// Message is one message of a session.
type Message struct {
	ExternalID   string  `json:"externalId"`
	SessionKey   string  `json:"sessionKey"`
	Role         string  `json:"role"`
	Blocks       []Block `json:"blocks"`
	CreateTimeMs int64   `json:"createTimeMs"`
	Position     int64   `json:"position"`
	Local        *Local  `json:"local,omitempty"`
}

// MessagePage is agentsrc.messages' answer.
type MessagePage struct {
	Session    *Session  `json:"session,omitempty"`
	Messages   []Message `json:"messages"`
	HasMore    bool      `json:"hasMore"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

// SearchHit is one hit of agentsrc.search.
type SearchHit struct {
	SessionKey   string `json:"sessionKey"`
	ExternalID   string `json:"externalId"`
	Text         string `json:"text"`
	Role         string `json:"role"`
	CreateTimeMs int64  `json:"createTimeMs"`
	Title        string `json:"title"`
	Provider     string `json:"provider"`
}

// Setup values of Status.
const (
	SetupNoAccounts = "no_accounts"
	SetupDisabled   = "disabled"
)

// StatusResult is agentsrc.status' answer.
type StatusResult struct {
	Enabled   bool       `json:"enabled"`
	Setup     string     `json:"setup,omitempty"`
	Providers []Provider `json:"providers"`
	Accounts  []Account  `json:"accounts"`
}

// SessionsResult is agentsrc.sessions' answer.
type SessionsResult struct {
	Sessions []Session `json:"sessions"`
}

// SearchResult is agentsrc.search's answer.
type SearchResult struct {
	Hits []SearchHit `json:"hits"`
}

// Empty is the {} answer of delete_message and mark_read.
type Empty struct{}

// Call parameters. The hub adds "contribution" to each; it is accepted and ignored here.

type MessagesParams struct {
	Contribution string `json:"contribution,omitempty"`
	SessionKey   string `json:"session_key"`
	Cursor       string `json:"cursor,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

type SendParams struct {
	Contribution string `json:"contribution,omitempty"`
	SessionKey   string `json:"session_key"`
	ClientID     string `json:"client_id"`
	Text         string `json:"text"`
}

type DeleteMessageParams struct {
	Contribution string `json:"contribution,omitempty"`
	SessionKey   string `json:"session_key"`
	ClientID     string `json:"client_id"`
}

type MarkReadParams struct {
	Contribution string `json:"contribution,omitempty"`
	SessionKey   string `json:"session_key"`
}

type SearchParams struct {
	Contribution string `json:"contribution,omitempty"`
	Q            string `json:"q"`
}

// UpdatedEvent is agentsrc.updated's params: SessionKeys omitted means the list itself changed.
type UpdatedEvent struct {
	Contribution string   `json:"contribution,omitempty"`
	SessionKeys  []string `json:"session_keys,omitempty"`
}

// Quota states, kinds and sources (§6.5 of the design). A source that cannot tell leaves Quota out entirely.
const (
	QuotaOK        = "ok"
	QuotaLow       = "low"
	QuotaExhausted = "exhausted"

	QuotaKindTokens   = "tokens"
	QuotaKindRequests = "requests"
	QuotaKindCredits  = "credits"
	QuotaKindWindow   = "window"

	QuotaSourceProvider = "provider"
	QuotaSourceGateway  = "gateway"
	QuotaSourceResponse = "response"
)

// Quota is how much of an account's allowance is left. Optional everywhere; nothing in this version fills it.
type Quota struct {
	State     string   `json:"state"`
	Remaining *float64 `json:"remaining,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Label     string   `json:"label,omitempty"`
	ResetsAt  int64    `json:"resetsAt,omitempty"`
	CheckedAt int64    `json:"checkedAt,omitempty"`
	Source    string   `json:"source,omitempty"`
	Stale     bool     `json:"stale"`
	Show      bool     `json:"show"`
	Details   []Quota  `json:"details,omitempty"`
}

// ClubInfo is club.info's answer.
type ClubInfo struct {
	Version         string     `json:"version"`
	ProtocolVersion int        `json:"protocolVersion"`
	Providers       []Provider `json:"providers"`
	Browser         bool       `json:"browser"`
}

// AddAccountParams are club.accounts.add's. Credential goes in and never comes back out.
type AddAccountParams struct {
	Provider    string `json:"provider"`
	Credential  string `json:"credential"`
	DisplayName string `json:"display_name,omitempty"`
}

// RemoveAccountParams are club.accounts.remove's.
type RemoveAccountParams struct {
	AccountID  string `json:"account_id"`
	DeleteData bool   `json:"delete_data,omitempty"`
}

// AccountParams address one account (club.accounts.sync).
type AccountParams struct {
	AccountID string `json:"account_id"`
}

// UpdateAccountParams are club.accounts.update's: the fields given change.
type UpdateAccountParams struct {
	AccountID        string  `json:"account_id"`
	Paused           *bool   `json:"paused,omitempty"`
	DisplayName      *string `json:"display_name,omitempty"`
	AssistantControl *string `json:"assistant_control,omitempty"`
	ShowQuota        *bool   `json:"show_quota,omitempty"`
}

// ReplaceCredentialParams are club.accounts.replace_credential's.
type ReplaceCredentialParams struct {
	AccountID  string `json:"account_id"`
	Credential string `json:"credential"`
}

// GrokBotLoginStartParams are club.logins.grok_bot.start's: account_id signs that account in again.
type GrokBotLoginStartParams struct {
	AccountID string `json:"account_id,omitempty"`
}

// GrokBotLoginStart is club.logins.grok_bot.start's answer (the gateway's GrokBotLoginStart).
type GrokBotLoginStart struct {
	LoginID  string `json:"loginId"`
	LoginURL string `json:"loginUrl"`
}

// LoginParams address one sign-in (club.logins.grok_bot.poll and cancel); DisplayName names a newly paired account.
type LoginParams struct {
	LoginID     string `json:"login_id"`
	DisplayName string `json:"display_name,omitempty"`
}

// TakeoverStartParams are club.grok_bot.takeover.start's: account_id is the paired Grok Bot.
type TakeoverStartParams struct {
	AccountID string `json:"account_id"`
}

// TakeoverStart is club.grok_bot.takeover.start's answer: the id to ask about, and where it stands.
type TakeoverStart struct {
	TakeoverID string `json:"takeoverId"`
	Phase      string `json:"phase"`
	Detail     string `json:"detail"`
}

// TakeoverParams address one takeover (status, plan, apply).
type TakeoverParams struct {
	TakeoverID string `json:"takeover_id"`
}

// TakeoverStatus is club.grok_bot.takeover.status's answer. Nothing here is a secret: no token, no key, no address
// the hub would dial, only whether each step is done and a line safe to show.
type TakeoverStatus struct {
	Phase   string `json:"phase"`
	Detail  string `json:"detail"`
	Node    bool   `json:"node"`
	Paired  bool   `json:"paired"`
	SSH     bool   `json:"ssh"`
	PlanID  string `json:"planId,omitempty"`
	Waiting bool   `json:"waiting"`
}

// TakeoverPlan is club.grok_bot.takeover.plan's answer: a description of the hub change waiting for the user, never
// the change itself. Nothing is written until apply, and apply refuses a plan the user has not been shown.
type TakeoverPlan struct {
	PlanID  string `json:"planId"`
	Summary string `json:"summary"`
	Node    string `json:"node"`
	Kind    string `json:"kind"`
	Host    string `json:"host"`
	// AlreadyLinked is true when the hub already has this machine and nothing would be written.
	AlreadyLinked bool `json:"alreadyLinked"`
}

// TakeoverApply is club.grok_bot.takeover.apply's answer. Applied is false when the user has not confirmed: the
// file is untouched and the hub is not restarted.
type TakeoverApply struct {
	Applied bool   `json:"applied"`
	Detail  string `json:"detail"`
	Backup  string `json:"backup,omitempty"`
}

// Login poll statuses.
const (
	LoginPending = "pending"
	LoginDone    = "done"
)

// GrokBotLoginPoll is club.logins.grok_bot.poll's answer (the gateway's GrokBotLoginPoll): pending, or done with
// the account.
type GrokBotLoginPoll struct {
	Status  string   `json:"status"`
	Account *Account `json:"account,omitempty"`
}

// SessionParams address one session (club.live).
type SessionParams struct {
	SessionKey string `json:"session_key"`
}

// LiveState is club.live's answer (the gateway's AgentLiveState); Supported is false when the provider cannot say.
type LiveState struct {
	Supported bool `json:"supported"`
	Running   bool `json:"running"`
	Composing bool `json:"composing"`
}

// BrowserIdentity is how a browser presents itself to sign-in pages (the gateway's AgentBrowserIdentity). Club
// only keeps it in this version.
type BrowserIdentity struct {
	UserAgent string   `json:"userAgent"`
	Locale    string   `json:"locale"`
	Timezone  string   `json:"timezone"`
	ExtraArgs []string `json:"extraArgs"`
}

// Settings is club.settings.get's and set's answer.
type Settings struct {
	BrowserIdentity BrowserIdentity `json:"browserIdentity"`
}

// SettingsParams are club.settings.set's.
type SettingsParams struct {
	BrowserIdentity *BrowserIdentity `json:"browser_identity"`
}

// OutboxParams address one send (club.outbox.dismiss).
type OutboxParams struct {
	SessionKey string `json:"session_key"`
	ClientID   string `json:"client_id"`
}

// ImportParams are club.import's: the path of an import bundle on club's machine, the providers to take (all when
// empty) and whether to only check the bundle.
type ImportParams struct {
	Path          string   `json:"path"`
	OnlyProviders []string `json:"only_providers,omitempty"`
	DryRun        bool     `json:"dry_run,omitempty"`
}

// ImportedAccount is what club.import checked for one account.
type ImportedAccount struct {
	AccountID       string `json:"accountId"`
	Provider        string `json:"provider"`
	DisplayName     string `json:"displayName,omitempty"`
	Verified        bool   `json:"verified"`
	Sessions        int    `json:"sessions"`
	Messages        int    `json:"messages"`
	LatestMessageMs int64  `json:"latestMessageMs"`
	// Error is why Verified is false: a provider code, NO_CREDENTIAL, ACCOUNT_MISMATCH or REMOVED.
	Error string `json:"error,omitempty"`
	// HasCredential says whether the bundle carries the account's credential.
	HasCredential bool `json:"hasCredential,omitempty"`
}

// SkippedAccount is an account of a bundle that was not imported, with its sessions, messages and credential.
type SkippedAccount struct {
	AccountID   string `json:"accountId"`
	Provider    string `json:"provider"`
	DisplayName string `json:"displayName,omitempty"`
	// Reason is PROVIDER_UNSUPPORTED (this build cannot reach the provider, e.g. a browser one) or NOT_SELECTED.
	Reason string `json:"reason"`
}

// ImportResult is club.import's answer. A dry run's counts are the bundle's and nothing is verified.
type ImportResult struct {
	Accounts []ImportedAccount `json:"accounts"`
	Skipped  []SkippedAccount  `json:"skipped"`
	DryRun   bool              `json:"dryRun,omitempty"`
}

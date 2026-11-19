// Package provider is the seam between club's core and each agent service: an Adapter reads an account's sessions
// and messages and sends into them; optional capabilities are small interfaces an adapter may also implement.
package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
)

// RemoteAccount is what Verify learns about the account a credential belongs to.
type RemoteAccount struct {
	ExternalAccountID string
	DisplayName       string
	// Quota is what the answer said about the account's allowance, when it says (as SendResult.Quota).
	Quota []QuotaReading
}

// SessionRef addresses a session at the provider: its id and the provider-specific ids kept with it (never secrets).
type SessionRef struct {
	ExternalID string
	Ref        map[string]string
}

// RemoteSession is a session as the provider lists it.
type RemoteSession struct {
	ExternalID       string
	Title            string
	Kind             string // protocol.KindMain, KindSide, KindTask
	ParentExternalID string
	LastActivityMs   int64
	CanSend          bool
	AvatarURL        string
	Ref              map[string]string
}

// SessionRef is the session's address.
func (s RemoteSession) SessionRef() SessionRef {
	return SessionRef{ExternalID: s.ExternalID, Ref: s.Ref}
}

// RemoteMessage is a message as the provider gives it.
type RemoteMessage struct {
	ExternalID string
	Role       string
	Blocks     []protocol.Block
	// CreateTimeMs is 0 when the provider gives no time; the store keeps its first-seen time.
	CreateTimeMs int64
	// Position is the provider's own order within the session, when it has one.
	Position int64
}

// ListOptions bound ListMessages.
type ListOptions struct{ Limit int }

// RemotePage is one read of a session's messages.
type RemotePage struct {
	Messages []RemoteMessage
	// Raw are the provider's own responses, for replay and debugging; never shown to callers.
	Raw []any
}

// Outgoing is one message to send. ClientID is the caller's dedupe id; providers that take one pass it on.
type Outgoing struct {
	ClientID string
	Text     string
}

// Turn is one stored turn handed to a provider that keeps no transcript (WantsHistory).
type Turn struct {
	Role string // user | assistant
	Text string
}

// SendContext is what the core adds to a send.
type SendContext struct {
	History []Turn
}

// SendStatus is how a send ended.
type SendStatus string

const (
	// Sent: the provider accepted it.
	Sent SendStatus = "sent"
	// Unconfirmed: whether the provider accepted it is unknown. The core never sends it again on its own.
	Unconfirmed SendStatus = "unconfirmed"
)

// SendResult is a send that did not fail outright.
type SendResult struct {
	Status SendStatus
	// ExternalID is the message's id at the provider, when it says.
	ExternalID string
	// Messages the provider answered with in the same call (a provider that answers synchronously).
	Messages []RemoteMessage
	// Error is why a send is unconfirmed (a code safe to show).
	Error string
	// Quota is what the provider's answer said about the account's allowance, when it says.
	Quota []QuotaReading
}

// QuotaReading is one dimension of an account's allowance as a provider's answer reported it (Groq's rate limit
// headers, for example).
type QuotaReading struct {
	Kind      string // protocol.QuotaKindTokens, QuotaKindRequests, ...
	Limit     float64
	Remaining float64
	// ResetsAt is when the window resets (Unix ms), 0 when not said.
	ResetsAt int64
	// Window names the window ("minute", "day"), when known.
	Window string
}

// Adapter is one provider. A call that returns an *Error was refused by the provider (nothing was accepted); any
// other error from Send leaves the send unconfirmed.
type Adapter interface {
	Name() string
	Connection() string // protocol.ConnectionServer or ConnectionBrowser
	Verify(ctx context.Context, credential string) (RemoteAccount, error)
	ListSessions(ctx context.Context, credential string) ([]RemoteSession, error)
	ListMessages(ctx context.Context, credential string, session SessionRef, opts ListOptions) (RemotePage, error)
	Send(ctx context.Context, credential string, session SessionRef, msg Outgoing, sc SendContext) (SendResult, error)
}

// ConfirmStatus is ConfirmSend's answer.
type ConfirmStatus string

const (
	ConfirmSent    ConfirmStatus = "sent"
	ConfirmFailed  ConfirmStatus = "failed"
	ConfirmUnknown ConfirmStatus = "unknown"
)

// Confirmation is what ConfirmSend learned.
type Confirmation struct {
	Status ConfirmStatus
	Error  string
}

// SendConfirmer asks, read-only, whether an unconfirmed send arrived. Unknown leaves it unconfirmed; nothing is ever
// sent again.
type SendConfirmer interface {
	ConfirmSend(ctx context.Context, credential string, session SessionRef, clientID string) (Confirmation, error)
}

// LiveState is whether the agent is working on a session right now.
type LiveState struct {
	Running   bool
	Composing bool
}

// LiveStater reports a session's live state.
type LiveStater interface {
	LiveState(ctx context.Context, credential string, session SessionRef) (LiveState, error)
}

// Interrupter stops the turn the agent is working on, keeping the session.
type Interrupter interface {
	Interrupt(ctx context.Context, credential string, session SessionRef) error
}

// SessionCreator opens a new session and sends its first message.
type SessionCreator interface {
	CreateSession(ctx context.Context, credential string, title, text string) (RemoteSession, SendResult, error)
}

// HistoryWanter is a provider that keeps no transcript: the core passes the stored turns with each send and keeps
// what it answers.
type HistoryWanter interface {
	WantsHistory() bool
}

// CredentialRefresher swaps a refused credential for a fresh one ("" when it cannot be refreshed). An error means
// the provider refused the refresh.
type CredentialRefresher interface {
	RefreshCredential(ctx context.Context, credential string) (string, error)
}

// WatchEvent is what a watch reports about a session.
type WatchEvent string

const (
	// WatchChanged: the session changed; read it now.
	WatchChanged WatchEvent = "changed"
	// WatchIdle: the agent stopped working on the session.
	WatchIdle WatchEvent = "idle"
)

// Watcher pushes, while a reply is awaited, that a session changed or went idle. Watch calls event for each until
// ctx ends, the provider closes the stream, or event returns false.
type Watcher interface {
	Watch(ctx context.Context, credential string, session SessionRef, event func(WatchEvent) bool) error
}

// LoginFlow is one sign-in in progress: the URL the user approves at, and the provider's own state (never shown).
type LoginFlow struct {
	URL   string
	State any
}

// DeviceLogin is a sign-in the user approves in their own browser while club polls for the credential.
type DeviceLogin interface {
	StartLogin() (LoginFlow, error)
	// PollLogin answers the credential once the user approved, "" while it is still pending.
	PollLogin(ctx context.Context, flow LoginFlow) (credential string, err error)
}

// CodeLogin is a sign-in the user approves in their own browser and finishes by pasting the authorization code the
// approval page shows. StartLogin's URL carries no secret; the provider's state (the PKCE verifier) never leaves club.
type CodeLogin interface {
	StartLogin() (LoginFlow, error)
	// CompleteLogin exchanges the pasted code for the credential.
	CompleteLogin(ctx context.Context, flow LoginFlow, code string) (credential string, err error)
}

// Common error codes.
const (
	CodeAuthFailed  = "AUTH_FAILED"
	CodeRateLimited = "RATE_LIMITED"
	CodeNotFound    = "NOT_FOUND"
	CodeUnavailable = "UNAVAILABLE"
	CodeBadResponse = "BAD_RESPONSE"
)

// More codes the adapters use.
const (
	CodeNetworkError  = "NETWORK_ERROR"
	CodeUpstreamError = "UPSTREAM_ERROR"
	CodeRequestFailed = "REQUEST_FAILED"
)

// Error is a provider's refusal or failure. Code is safe to show and log; it never carries a credential or a
// message body.
type Error struct {
	Code       string
	HTTPStatus int
	Retryable  bool
	// RetryAfter is how long a rate-limited provider asked to wait (Retry-After), 0 when not said.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("provider: %s (HTTP %d)", e.Code, e.HTTPStatus)
	}
	return "provider: " + e.Code
}

// CodeOf is err's provider code, or "SYNC_FAILED" for an error that is not a provider's.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "SYNC_FAILED"
}

// IsRefusal says whether err is a provider's own refusal (an *Error).
func IsRefusal(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

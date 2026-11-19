package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/store"
	"github.com/nwverse/personal-agent-club/internal/vault"
)

const (
	messagePage       = 50
	maxMessagePage    = 100
	sessionsListed    = 500
	searchLimit       = 20
	maxQuery          = 200
	previewChars      = 200
	localEchoWindowMs = 10 * 60_000
	historyMessages   = 40
	sendTimeout       = 2 * time.Minute
	minCredential     = 8
	maxCredential     = 8192
	maxDisplayName    = 80
)

// Space is one isolated set of accounts, sessions and credentials (spaces/<space>).
type Space struct {
	name  string
	club  *Club
	store store.Store
	vault vault.Vault

	mu         sync.Mutex
	syncing    map[string]*flight[struct{}]
	refreshing map[string]*flight[string]
	fetchedAt  map[string]int64
	watching   map[string]context.CancelFunc
	live       map[string]*liveEntry
	logins     map[string]*login
	takeovers  map[string]*takeoverRun
	quota      map[string]*quotaState
}

func newSpace(name string, c *Club, st store.Store, v vault.Vault) *Space {
	return &Space{name: name, club: c, store: st, vault: v, syncing: map[string]*flight[struct{}]{},
		refreshing: map[string]*flight[string]{}, fetchedAt: map[string]int64{}, watching: map[string]context.CancelFunc{},
		live: map[string]*liveEntry{}, logins: map[string]*login{}, quota: map[string]*quotaState{}}
}

// flight is one call others may join.
type flight[T any] struct {
	done chan struct{}
	val  T
	err  error
}

// join runs f once per key at a time: a second caller waits for the running one and gets its answer.
func join[T any](mu *sync.Mutex, m map[string]*flight[T], key string, f func() (T, error)) (T, error) {
	mu.Lock()
	if running := m[key]; running != nil {
		mu.Unlock()
		<-running.done
		return running.val, running.err
	}
	fl := &flight[T]{done: make(chan struct{})}
	m[key] = fl
	mu.Unlock()
	fl.val, fl.err = f()
	mu.Lock()
	delete(m, key)
	mu.Unlock()
	close(fl.done)
	return fl.val, fl.err
}

// Name is the space's name.
func (s *Space) Name() string { return s.name }

// Store is the space's store (for tests and tools that read it directly).
func (s *Space) Store() store.Store { return s.store }

func (s *Space) now() int64 { return s.club.now() }

func (s *Space) notify(keys []string) { s.club.publish(Event{Space: s.name, SessionKeys: keys}) }

func (s *Space) log() interface {
	Warn(string, ...any)
	Debug(string, ...any)
} {
	return s.club.opts.Log
}

func badInput(msg string) *protocol.Error {
	return protocol.Invalid(msg).Because(protocol.ReasonInvalidInput)
}

func noSession() *protocol.Error {
	return protocol.NotFound("no such session").Because(protocol.ReasonSessionNotFound)
}

func noAccount() *protocol.Error {
	return protocol.NotFound("no such account").Because(protocol.ReasonAccountNotFound)
}

// Status is agentsrc.status.
func (s *Space) Status(ctx context.Context) (protocol.StatusResult, error) {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return protocol.StatusResult{}, err
	}
	out := protocol.StatusResult{Enabled: true, Providers: s.club.opts.Registry.List(), Accounts: []protocol.Account{}}
	for _, a := range accounts {
		pa, err := s.accountView(ctx, a)
		if err != nil {
			return protocol.StatusResult{}, err
		}
		out.Accounts = append(out.Accounts, pa)
	}
	if len(out.Accounts) == 0 {
		out.Setup = protocol.SetupNoAccounts
	}
	return out, nil
}

func (s *Space) accountView(ctx context.Context, a store.Account) (protocol.Account, error) {
	sessions, err := s.store.AccountSessions(ctx, a.AccountID)
	if err != nil {
		return protocol.Account{}, err
	}
	control, _ := ParseControl(a.AssistantControl)
	return protocol.Account{AccountID: a.AccountID, Provider: a.Provider, DisplayName: cmpOr(a.DisplayName, a.Provider),
		Status: a.Status, ExternalAccountID: a.ExternalAccountID, LastSyncedAt: a.LastSyncedAt, LastError: a.LastError,
		SessionCount: len(sessions), AssistantControl: string(control), ShowQuota: a.ShowQuota,
		Quota: s.quotaView(a, s.now())}, nil
}

// Sessions is agentsrc.sessions.
func (s *Space) Sessions(ctx context.Context) (protocol.SessionsResult, error) {
	rows, err := s.store.Sessions(ctx, sessionsListed)
	if err != nil {
		return protocol.SessionsResult{}, err
	}
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return protocol.SessionsResult{}, err
	}
	quotas := map[string]*protocol.Quota{}
	now := s.now()
	for _, a := range accounts {
		quotas[a.AccountID] = s.quotaView(a, now)
	}
	out := protocol.SessionsResult{Sessions: make([]protocol.Session, 0, len(rows))}
	for _, r := range rows {
		v := sessionView(r)
		v.Quota = quotas[r.AccountID]
		out.Sessions = append(out.Sessions, v)
	}
	return out, nil
}

func sessionView(r store.Session) protocol.Session {
	return protocol.Session{SessionKey: r.SessionKey, AccountID: r.AccountID, Provider: r.Provider, Title: r.Title,
		Kind: r.Kind, ParentSessionKey: r.ParentSessionKey, CanSend: r.CanSend, UnreadCount: r.UnreadCount,
		RankTimeMs: r.RankTimeMs, AvatarURL: r.AvatarURL, PreviewText: prefix16(r.PreviewText, previewChars),
		PreviewIsMine: r.PreviewIsMine, OnTopRankTimeMs: r.OnTopRankTimeMs}
}

// prefix16 is the first n UTF-16 code units of s, as the gateway's JavaScript cuts a preview.
func prefix16(s string, n int) string {
	if len(s) <= n {
		return s
	}
	u := utf16.Encode([]rune(s))
	if len(u) <= n {
		return s
	}
	return string(utf16.Decode(u[:n]))
}

func (s *Space) requireSession(ctx context.Context, key string) (store.Session, error) {
	if key == "" {
		return store.Session{}, badInput("session_key is required")
	}
	sess, ok, err := s.store.Session(ctx, key)
	if err != nil {
		return store.Session{}, err
	}
	if !ok {
		return store.Session{}, noSession()
	}
	return sess, nil
}

func (s *Space) requireAccount(ctx context.Context, id string) (store.Account, error) {
	if id == "" {
		return store.Account{}, badInput("account_id is required")
	}
	a, ok, err := s.store.Account(ctx, id)
	if err != nil {
		return store.Account{}, err
	}
	if !ok {
		return store.Account{}, noAccount()
	}
	return a, nil
}

// Messages is agentsrc.messages.
func (s *Space) Messages(ctx context.Context, p protocol.MessagesParams) (protocol.MessagePage, error) {
	sess, err := s.requireSession(ctx, p.SessionKey)
	if err != nil {
		return protocol.MessagePage{}, err
	}
	var before *store.Cursor
	if p.Cursor != "" {
		c, ok := decodeCursor(p.Cursor)
		if !ok {
			return protocol.MessagePage{}, protocol.Invalid("invalid cursor").Because(protocol.ReasonInvalidCursor)
		}
		before = &c
	}
	if p.Limit < 0 {
		return protocol.MessagePage{}, badInput("limit must not be negative")
	}
	limit := p.Limit
	if limit == 0 {
		limit = messagePage
	}
	limit = min(limit, maxMessagePage)
	rows, hasMore, err := s.store.Messages(ctx, sess.SessionKey, limit, before)
	if err != nil {
		return protocol.MessagePage{}, err
	}
	view := sessionView(sess)
	page := protocol.MessagePage{Session: &view, Messages: make([]protocol.Message, 0, len(rows)), HasMore: hasMore}
	for _, r := range rows {
		page.Messages = append(page.Messages, protocol.Message{ExternalID: r.ExternalID, SessionKey: r.SessionKey,
			Role: r.Role, Blocks: nonNil(r.Blocks), CreateTimeMs: r.CreateTimeMs, Position: r.Position})
	}
	if hasMore && len(rows) > 0 {
		page.NextCursor = encodeCursor(rows[0])
	}
	if before == nil {
		local, err := s.store.LocalMessages(ctx, sess.SessionKey, s.now()-localEchoWindowMs)
		if err != nil {
			return protocol.MessagePage{}, err
		}
		for _, o := range local {
			page.Messages = append(page.Messages, localMessage(o))
		}
	}
	return page, nil
}

func nonNil(b []protocol.Block) []protocol.Block {
	if b == nil {
		return []protocol.Block{}
	}
	return b
}

func encodeCursor(m store.Message) string {
	b, _ := json.Marshal([]any{m.CreateTimeMs, m.Position, m.ExternalID})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(v string) (store.Cursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(v, "="))
	if err != nil {
		return store.Cursor{}, false
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) != 3 {
		return store.Cursor{}, false
	}
	var c store.Cursor
	if json.Unmarshal(parts[0], &c.CreateTimeMs) != nil || json.Unmarshal(parts[1], &c.Position) != nil ||
		json.Unmarshal(parts[2], &c.ExternalID) != nil {
		return store.Cursor{}, false
	}
	return c, true
}

func localMessage(o store.Outbox) protocol.Message {
	return protocol.Message{ExternalID: "local:" + o.ClientID, SessionKey: o.SessionKey, Role: protocol.RoleUser,
		Blocks: []protocol.Block{protocol.TextBlock(o.Text)}, CreateTimeMs: o.CreatedAt,
		Local: &protocol.Local{ClientID: o.ClientID, Status: o.Status, Error: o.LastError}}
}

// Send is agentsrc.send: it writes the outbox row, then hands it to the provider once. The same client_id again
// answers the row as it is and sends nothing. A send whose outcome is unknown stays unconfirmed and is never sent
// again on its own.
func (s *Space) Send(ctx context.Context, p protocol.SendParams) (protocol.Message, error) {
	text := strings.TrimSpace(p.Text)
	switch {
	case p.SessionKey == "":
		return protocol.Message{}, badInput("session_key is required")
	case !ValidClientID(p.ClientID):
		return protocol.Message{}, badInput("client_id must be a UUID")
	case text == "":
		return protocol.Message{}, badInput("text is required")
	case len(utf16.Encode([]rune(text))) > protocol.MaxText:
		return protocol.Message{}, badInput(fmt.Sprintf("text is longer than %d characters", protocol.MaxText))
	}
	sess, err := s.requireSession(ctx, p.SessionKey)
	if err != nil {
		return protocol.Message{}, err
	}
	if !sess.CanSend {
		return protocol.Message{}, protocol.Conflict("this session is read-only").Because(protocol.ReasonSessionReadOnly)
	}
	acct, err := s.requireAccount(ctx, sess.AccountID)
	if err != nil {
		return protocol.Message{}, err
	}
	if acct.Status == protocol.AccountPaused {
		return protocol.Message{}, protocol.Conflict("the account is paused").Because(protocol.ReasonAccountPaused)
	}
	now := s.now()
	row, inserted, err := s.store.EnqueueOutbox(ctx, store.Outbox{ClientID: p.ClientID, SessionKey: sess.SessionKey,
		AccountID: sess.AccountID, Text: text, CreatedAt: now})
	if err != nil {
		return protocol.Message{}, err
	}
	if !inserted {
		if row.SessionKey != sess.SessionKey {
			return protocol.Message{}, protocol.Conflict("client_id is in use in another session").Because(protocol.ReasonClientIDInUse)
		}
		return localMessage(row), nil
	}
	if err := s.store.MarkRead(ctx, sess.SessionKey, now); err != nil {
		return protocol.Message{}, err
	}
	settled := s.dispatch(ctx, row, sess)
	s.notify([]string{sess.SessionKey})
	adapter, _ := s.club.opts.Registry.Get(sess.Provider)
	switch settled.Status {
	case protocol.LocalSent:
		if !wantsHistory(adapter) {
			s.followUp(sess)
			s.watchReply(sess)
		}
	case protocol.LocalUnconfirmed:
		s.confirmLater(settled, sess)
	}
	return localMessage(settled), nil
}

func wantsHistory(a provider.Adapter) bool {
	h, ok := a.(provider.HistoryWanter)
	return ok && h.WantsHistory()
}

// dispatch claims a pending row and hands it to the provider once.
func (s *Space) dispatch(ctx context.Context, row store.Outbox, sess store.Session) store.Outbox {
	// The send outlives a caller that hangs up: its outcome must still be recorded.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	claimed, ok, err := s.store.ClaimOutbox(ctx, row.ClientID, s.now())
	if err != nil || !ok {
		if cur, found, _ := s.store.Outbox(ctx, row.ClientID); found {
			return cur
		}
		return row
	}
	status, code := s.attempt(ctx, claimed, sess)
	if err := s.store.SettleOutbox(ctx, claimed.ClientID, status, code, s.now()); err != nil {
		s.log().Warn("settle outbox failed", "space", s.name, "err", err)
	}
	claimed.Status, claimed.LastError = status, code
	return claimed
}

// attempt is one send at the provider: sent, unconfirmed or failed, and an error code.
func (s *Space) attempt(ctx context.Context, row store.Outbox, sess store.Session) (string, string) {
	adapter, ok := s.club.opts.Registry.Get(sess.Provider)
	cred, found, err := s.vault.Get(ctx, sess.AccountID)
	if !ok || err != nil || !found {
		return protocol.LocalFailed, "NOT_PAIRED"
	}
	var sc provider.SendContext
	if wantsHistory(adapter) {
		if sc.History, err = s.history(ctx, sess.SessionKey); err != nil {
			return protocol.LocalFailed, "HISTORY_UNAVAILABLE"
		}
	}
	ref := provider.SessionRef{ExternalID: sess.ExternalID, Ref: sess.Ref}
	msg := provider.Outgoing{ClientID: row.ClientID, Text: row.Text}
	res, err := adapter.Send(ctx, cred, ref, msg, sc)
	if provider.CodeOf(err) == provider.CodeAuthFailed {
		// A refused credential never reached the agent: one retry with a fresh one, same client id.
		if fresh := s.refresh(ctx, sess.AccountID, adapter, cred); fresh != "" {
			res, err = adapter.Send(ctx, fresh, ref, msg, sc)
		}
	}
	s.observe(sess.AccountID, err)
	if err != nil {
		if !provider.IsRefusal(err) {
			return protocol.LocalUnconfirmed, "SEND_UNCONFIRMED"
		}
		code := provider.CodeOf(err)
		if code == provider.CodeAuthFailed {
			st := protocol.AccountError
			_ = s.store.UpdateAccount(ctx, sess.AccountID, store.AccountPatch{Status: &st, LastError: &code}, s.now())
		}
		return protocol.LocalFailed, code
	}
	s.noteQuota(sess.AccountID, res.Quota)
	switch res.Status {
	case provider.Sent:
		if len(res.Messages) > 0 {
			if _, err := s.store.UpsertMessages(ctx, sess.SessionKey, storeMessages(sess.SessionKey, res.Messages, 0), s.now()); err != nil {
				s.log().Warn("store answered messages failed", "space", s.name, "err", err)
			}
		}
		return protocol.LocalSent, ""
	case provider.Unconfirmed:
		return protocol.LocalUnconfirmed, cmpOr(res.Error, "SEND_UNCONFIRMED")
	}
	return protocol.LocalUnconfirmed, "SEND_UNCONFIRMED"
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// refresh swaps a refused credential once, shared by every caller that hit it: when another caller already stored
// a fresh one that is answered; a refused refresh answers "".
func (s *Space) refresh(ctx context.Context, accountID string, adapter provider.Adapter, used string) string {
	r, ok := adapter.(provider.CredentialRefresher)
	if !ok {
		return ""
	}
	fresh, _ := join(&s.mu, s.refreshing, accountID, func() (string, error) {
		if stored, found, err := s.vault.Get(ctx, accountID); err == nil && found && stored != used {
			return stored, nil
		}
		fresh, err := r.RefreshCredential(ctx, used)
		s.observe(accountID, err)
		if err != nil || fresh == "" {
			return "", err
		}
		if err := s.vault.Put(ctx, accountID, fresh); err != nil {
			return "", err
		}
		return fresh, nil
	})
	return fresh
}

// history is the stored turns a provider without its own transcript is sent: text blocks only.
func (s *Space) history(ctx context.Context, key string) ([]provider.Turn, error) {
	rows, _, err := s.store.Messages(ctx, key, historyMessages, nil)
	if err != nil {
		return nil, err
	}
	var out []provider.Turn
	for _, m := range rows {
		if m.Role != protocol.RoleUser && m.Role != protocol.RoleAssistant {
			continue
		}
		parts := make([]string, len(m.Blocks))
		for i, b := range m.Blocks {
			if b.Type == "text" {
				parts[i] = b.Text
			}
		}
		if t := strings.TrimSpace(strings.Join(parts, "\n")); t != "" {
			out = append(out, provider.Turn{Role: m.Role, Text: t})
		}
	}
	return out, nil
}

// storeMessages are a provider's messages as stored; one without a time takes fallbackMs (the session's activity
// time), else the store keeps its first-seen time.
func storeMessages(key string, in []provider.RemoteMessage, fallbackMs int64) []store.Message {
	out := make([]store.Message, len(in))
	for i, m := range in {
		out[i] = store.Message{SessionKey: key, ExternalID: m.ExternalID, Role: m.Role, Blocks: m.Blocks,
			CreateTimeMs: cmpOrInt(m.CreateTimeMs, fallbackMs), Position: m.Position}
	}
	return out
}

func cmpOrInt(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

// followUp reads the session again at the follow-up delays, to pick up the agent's answer.
func (s *Space) followUp(sess store.Session) {
	for _, d := range s.club.opts.FollowUps {
		s.club.after(d, func() { s.sync(s.club.ctx, sess.AccountID, sess.SessionKey) })
	}
}

// watchReply, while a reply is awaited, reads the session as soon as a provider that can push (Grok Bot's
// transcript watch) says it changed, instead of at the next follow-up. One watch per session, closed when the agent
// goes idle after a change, after the watch limit, or when the club closes; the follow-ups stay as the fallback.
func (s *Space) watchReply(sess store.Session) {
	adapter, _ := s.club.opts.Registry.Get(sess.Provider)
	w, ok := adapter.(provider.Watcher)
	if !ok || s.club.opts.WatchLimit < 0 {
		return
	}
	s.mu.Lock()
	if _, running := s.watching[sess.SessionKey]; running {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(s.club.ctx, s.club.opts.WatchLimit)
	s.watching[sess.SessionKey] = cancel
	s.mu.Unlock()
	s.club.goTracked(func() {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.watching, sess.SessionKey)
			s.mu.Unlock()
		}()
		cred, found, err := s.vault.Get(ctx, sess.AccountID)
		if err != nil || !found {
			return
		}
		changed := false
		err = w.Watch(ctx, cred, provider.SessionRef{ExternalID: sess.ExternalID, Ref: sess.Ref}, func(e provider.WatchEvent) bool {
			if ctx.Err() != nil {
				return false
			}
			if e == provider.WatchChanged {
				changed = true
				s.sync(context.WithoutCancel(ctx), sess.AccountID, sess.SessionKey)
				return true
			}
			return !changed
		})
		if err != nil && ctx.Err() == nil {
			s.observe(sess.AccountID, err)
			s.log().Warn("watch failed", "space", s.name, "provider", sess.Provider, "code", provider.CodeOf(err))
		}
	})
}

// confirmLater asks the provider, read-only, whether an unconfirmed send arrived. The message itself is never sent
// again.
func (s *Space) confirmLater(row store.Outbox, sess store.Session) {
	adapter, _ := s.club.opts.Registry.Get(sess.Provider)
	if _, ok := adapter.(provider.SendConfirmer); !ok {
		return
	}
	for _, d := range s.club.opts.FollowUps {
		s.club.after(d, func() { s.confirm(s.club.ctx, row.ClientID, sess) })
	}
}

func (s *Space) confirm(ctx context.Context, clientID string, sess store.Session) {
	row, ok, err := s.store.Outbox(ctx, clientID)
	if err != nil || !ok || row.Status != protocol.LocalUnconfirmed {
		return
	}
	adapter, _ := s.club.opts.Registry.Get(sess.Provider)
	c, isConfirmer := adapter.(provider.SendConfirmer)
	cred, found, err := s.vault.Get(ctx, sess.AccountID)
	if !isConfirmer || err != nil || !found {
		return
	}
	answer, err := c.ConfirmSend(ctx, cred, provider.SessionRef{ExternalID: sess.ExternalID, Ref: sess.Ref}, clientID)
	s.observe(sess.AccountID, err)
	if err != nil {
		s.log().Warn("send confirmation failed", "space", s.name, "provider", sess.Provider, "code", provider.CodeOf(err))
		return
	}
	if answer.Status == provider.ConfirmUnknown {
		return
	}
	status, code := protocol.LocalSent, ""
	if answer.Status == provider.ConfirmFailed {
		status, code = protocol.LocalFailed, answer.Error
	}
	if changed, _ := s.store.ConfirmOutbox(ctx, clientID, status, code, s.now()); changed {
		s.notify([]string{sess.SessionKey})
		if status == protocol.LocalSent {
			s.sync(ctx, sess.AccountID, sess.SessionKey)
		}
	}
}

// recover runs when a space opens: claimed sends become unconfirmed (asked about later, never resent); pending ones,
// which never reached a provider, go out.
func (s *Space) recover(ctx context.Context) error {
	abandoned, err := s.store.AbandonClaimedOutbox(ctx, s.now())
	if err != nil {
		return err
	}
	var keys []string
	for _, row := range abandoned {
		if sess, ok, _ := s.store.Session(ctx, row.SessionKey); ok {
			s.confirmLater(row, sess)
		}
		keys = append(keys, row.SessionKey)
	}
	pending, err := s.store.PendingOutbox(ctx)
	if err != nil {
		return err
	}
	for _, row := range pending {
		if sess, ok, _ := s.store.Session(ctx, row.SessionKey); ok {
			s.dispatch(ctx, row, sess)
			keys = append(keys, row.SessionKey)
		}
	}
	if len(keys) > 0 {
		s.notify(keys)
	}
	return nil
}

// Recover reruns outbox recovery (what opening a space does after a restart).
func (s *Space) Recover(ctx context.Context) error { return s.recover(ctx) }

// DeleteMessage is agentsrc.delete_message: it cancels a send that has not gone out yet.
func (s *Space) DeleteMessage(ctx context.Context, p protocol.DeleteMessageParams) error {
	if p.ClientID == "" {
		return badInput("client_id is required")
	}
	sess, err := s.requireSession(ctx, p.SessionKey)
	if err != nil {
		return err
	}
	ok, err := s.store.CancelOutbox(ctx, p.ClientID, sess.SessionKey, s.now())
	if err != nil {
		return err
	}
	if !ok {
		return protocol.Conflict("no pending send with that client_id").Because(protocol.ReasonNotPending)
	}
	s.notify([]string{sess.SessionKey})
	return nil
}

// DismissOutbox is club.outbox.dismiss: it removes a failed send, releasing its client id.
func (s *Space) DismissOutbox(ctx context.Context, p protocol.OutboxParams) error {
	if p.ClientID == "" {
		return badInput("client_id is required")
	}
	sess, err := s.requireSession(ctx, p.SessionKey)
	if err != nil {
		return err
	}
	ok, err := s.store.DismissOutbox(ctx, p.ClientID, sess.SessionKey)
	if err != nil {
		return err
	}
	if !ok {
		return protocol.Conflict("no failed send with that client_id").Because(protocol.ReasonNotPending)
	}
	s.notify([]string{sess.SessionKey})
	return nil
}

// MarkRead is agentsrc.mark_read.
func (s *Space) MarkRead(ctx context.Context, p protocol.MarkReadParams) error {
	sess, err := s.requireSession(ctx, p.SessionKey)
	if err != nil {
		return err
	}
	if err := s.store.MarkRead(ctx, sess.SessionKey, s.now()); err != nil {
		return err
	}
	s.notify([]string{sess.SessionKey})
	return nil
}

// Search is agentsrc.search.
func (s *Space) Search(ctx context.Context, p protocol.SearchParams) (protocol.SearchResult, error) {
	q := strings.TrimSpace(p.Q)
	if r := []rune(q); len(r) > maxQuery {
		q = string(r[:maxQuery])
	}
	hits, err := s.store.Search(ctx, q, searchLimit)
	if err != nil {
		return protocol.SearchResult{}, err
	}
	if hits == nil {
		hits = []protocol.SearchHit{}
	}
	return protocol.SearchResult{Hits: hits}, nil
}

func (s *Space) adapter(name string) (provider.Adapter, error) {
	adapter, ok := s.club.opts.Registry.Get(name)
	if ok {
		return adapter, nil
	}
	if s.club.opts.Registry.IsKnown(name) {
		return nil, protocol.Unsupported(fmt.Sprintf("provider %s is not available in this build", name)).
			Because(protocol.ReasonProviderUnavailable)
	}
	return nil, badInput(fmt.Sprintf("unknown provider %q", name))
}

func verifyError(err error) error {
	if provider.IsRefusal(err) {
		code := provider.CodeOf(err)
		return protocol.Invalid("the provider refused the credential: " + code).Because(code)
	}
	return protocol.Failure("SYNC_FAILED", "verify failed")
}

func checkCredential(cred string) error {
	if len(cred) < minCredential || len(cred) > maxCredential {
		return badInput("credential must be 8 to 8192 characters")
	}
	return nil
}

// AddAccount pairs an account: the provider checks the credential, which then goes into the vault and never comes
// back out. The account starts with assistant control off and is synced once. The same remote account twice in a
// space is a conflict.
func (s *Space) AddAccount(ctx context.Context, p protocol.AddAccountParams) (protocol.Account, error) {
	cred := strings.TrimSpace(p.Credential)
	name := strings.TrimSpace(p.DisplayName)
	if p.Provider == "" {
		return protocol.Account{}, badInput("provider is required")
	}
	if err := checkCredential(cred); err != nil {
		return protocol.Account{}, err
	}
	if len(utf16.Encode([]rune(name))) > maxDisplayName {
		return protocol.Account{}, badInput("display_name is longer than 80 characters")
	}
	adapter, err := s.adapter(p.Provider)
	if err != nil {
		return protocol.Account{}, err
	}
	remote, err := adapter.Verify(ctx, cred)
	s.observe("", err)
	if err != nil {
		return protocol.Account{}, verifyError(err)
	}
	existing, err := s.store.ListAccounts(ctx)
	if err != nil {
		return protocol.Account{}, err
	}
	for _, a := range existing {
		if a.Provider == p.Provider && remote.ExternalAccountID != "" && a.ExternalAccountID == remote.ExternalAccountID {
			return protocol.Account{}, protocol.Conflict("this account is already paired as " + a.AccountID).
				Because(protocol.ReasonAlreadyPaired)
		}
	}
	id := NewAccountID()
	if err := s.vault.Put(ctx, id, cred); err != nil {
		return protocol.Account{}, err
	}
	now := s.now()
	if err := s.store.InsertAccount(ctx, store.Account{AccountID: id, Provider: p.Provider, DisplayName: cmpOr(name, remote.DisplayName),
		ExternalAccountID: remote.ExternalAccountID, Status: protocol.AccountActive,
		AssistantControl: string(DefaultControl), CreatedAt: now, UpdatedAt: now}); err != nil {
		_ = s.vault.Delete(ctx, id)
		return protocol.Account{}, err
	}
	s.noteQuota(id, remote.Quota)
	s.sync(ctx, id, "")
	s.notify(nil)
	return s.Account(ctx, id)
}

// Account is one account's view.
func (s *Space) Account(ctx context.Context, id string) (protocol.Account, error) {
	a, err := s.requireAccount(ctx, id)
	if err != nil {
		return protocol.Account{}, err
	}
	return s.accountView(ctx, a)
}

// AssistantControl is an account's assistant control level.
func (s *Space) AssistantControl(ctx context.Context, id string) (Control, error) {
	a, err := s.requireAccount(ctx, id)
	if err != nil {
		return "", err
	}
	return ParseControl(a.AssistantControl)
}

// RemoveAccount unpairs an account: its credential is deleted; with deleteData its sessions and messages too,
// otherwise they are kept, unlisted.
func (s *Space) RemoveAccount(ctx context.Context, p protocol.RemoveAccountParams) error {
	if _, err := s.requireAccount(ctx, p.AccountID); err != nil {
		return err
	}
	if err := s.vault.Delete(ctx, p.AccountID); err != nil {
		return err
	}
	if err := s.store.RemoveAccount(ctx, p.AccountID, p.DeleteData, s.now()); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.quota, p.AccountID)
	s.mu.Unlock()
	s.notify(nil)
	return nil
}

// errorIsProtocol says whether err is one of the four protocol refusals.
func errorIsProtocol(err error) bool {
	var e *protocol.Error
	return errors.As(err, &e) && e.Code != ""
}

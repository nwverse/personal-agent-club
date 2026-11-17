package store

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/nwverse/personal-agent-club/internal/protocol"
)

// Memory is a Store in memory.
type Memory struct {
	mu       sync.Mutex
	accounts map[string]Account
	sessions map[string]Session
	messages map[string]map[string]Message // by session key, then external id
	outbox   map[string]Outbox
	settings map[string]string
}

// NewMemory is an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{accounts: map[string]Account{}, sessions: map[string]Session{},
		messages: map[string]map[string]Message{}, outbox: map[string]Outbox{}, settings: map[string]string{}}
}

var _ Store = (*Memory)(nil)

func (m *Memory) ListAccounts(context.Context) ([]Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Account
	for _, a := range m.accounts {
		if a.Status != AccountRemoved {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b Account) int {
		return cmp.Or(cmp.Compare(a.CreatedAt, b.CreatedAt), strings.Compare(a.AccountID, b.AccountID))
	})
	return out, nil
}

func (m *Memory) Account(_ context.Context, id string) (Account, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok || a.Status == AccountRemoved {
		return Account{}, false, nil
	}
	return a, true, nil
}

func (m *Memory) InsertAccount(_ context.Context, a Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts[a.AccountID] = a
	return nil
}

func (m *Memory) UpdateAccount(_ context.Context, id string, p AccountPatch, now int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		return ErrNotFound
	}
	if p.Status != nil && (!p.PreservePaused || a.Status != protocol.AccountPaused) {
		a.Status = *p.Status
	}
	if p.DisplayName != nil {
		a.DisplayName = *p.DisplayName
	}
	if p.LastSyncedAt != nil {
		a.LastSyncedAt = *p.LastSyncedAt
	}
	if p.LastError != nil {
		a.LastError = *p.LastError
	}
	if p.AssistantControl != nil {
		a.AssistantControl = *p.AssistantControl
	}
	if p.ShowQuota != nil {
		a.ShowQuota = *p.ShowQuota
	}
	a.UpdatedAt = now
	m.accounts[id] = a
	return nil
}

func (m *Memory) RemoveAccount(_ context.Context, id string, deleteData bool, now int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		return ErrNotFound
	}
	if !deleteData {
		a.Status, a.UpdatedAt = AccountRemoved, now
		m.accounts[id] = a
		return nil
	}
	delete(m.accounts, id)
	for key, s := range m.sessions {
		if s.AccountID == id {
			delete(m.sessions, key)
			delete(m.messages, key)
		}
	}
	for cid, o := range m.outbox {
		if o.AccountID == id {
			delete(m.outbox, cid)
		}
	}
	return nil
}

func (m *Memory) UpsertSessions(_ context.Context, sessions []Session, now int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := 0
	for _, s := range sessions {
		old, ok := m.sessions[s.SessionKey]
		if !ok {
			s.ReadTimeMs = max(now, s.RankTimeMs)
			s.UpdatedAt = now
			s.Ref = cloneRef(s.Ref)
			m.sessions[s.SessionKey] = s
			changed++
			continue
		}
		next := old
		next.Title, next.Kind, next.ParentSessionKey, next.CanSend = s.Title, s.Kind, s.ParentSessionKey, s.CanSend
		next.AvatarURL, next.Ref = s.AvatarURL, cloneRef(s.Ref)
		next.RankTimeMs = max(old.RankTimeMs, s.RankTimeMs)
		if sessionChanged(old, next) {
			next.UpdatedAt = now
			m.sessions[s.SessionKey] = next
			changed++
		}
	}
	return changed, nil
}

func cloneRef(r map[string]string) map[string]string {
	if r == nil {
		return map[string]string{}
	}
	return maps.Clone(r)
}

func sessionChanged(old, next Session) bool {
	return next.Title != old.Title || next.Kind != old.Kind || next.ParentSessionKey != old.ParentSessionKey ||
		next.CanSend != old.CanSend || next.AvatarURL != old.AvatarURL || !maps.Equal(next.Ref, old.Ref) ||
		next.RankTimeMs != old.RankTimeMs
}

func (m *Memory) UpsertMessages(_ context.Context, key string, messages []Message, now int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := 0
	for _, msg := range messages {
		if m.putMessage(key, msg, now) {
			changed++
		}
	}
	m.refresh(key, now)
	return changed, nil
}

// putMessage upserts one message. m.mu is held.
func (m *Memory) putMessage(key string, msg Message, now int64) bool {
	byID := m.messages[key]
	if byID == nil {
		byID = map[string]Message{}
		m.messages[key] = byID
	}
	msg.SessionKey = key
	msg.Blocks = slices.Clone(msg.Blocks)
	old, ok := byID[msg.ExternalID]
	if msg.CreateTimeMs == 0 {
		msg.CreateTimeMs = now
		if ok {
			msg.CreateTimeMs = old.CreateTimeMs
		}
	}
	if ok && sameMessage(old, msg) {
		return false
	}
	byID[msg.ExternalID] = msg
	return true
}

func sameMessage(a, b Message) bool {
	return a.Role == b.Role && a.CreateTimeMs == b.CreateTimeMs && a.Position == b.Position &&
		blocksJSON(a.Blocks) == blocksJSON(b.Blocks)
}

// refresh recomputes a session's rank, preview and unread count. m.mu is held.
func (m *Memory) refresh(key string, now int64) {
	s, ok := m.sessions[key]
	if !ok {
		return
	}
	var latest *Message
	var lastUser int64
	for _, msg := range m.messages[key] {
		if msg.Role == protocol.RoleUser {
			lastUser = max(lastUser, msg.CreateTimeMs)
		}
		if (msg.Role == protocol.RoleUser || msg.Role == protocol.RoleAssistant) && msg.Text() != "" {
			if latest == nil || comparePreview(msg, *latest) > 0 {
				x := msg
				latest = &x
			}
		}
	}
	next := s
	if latest != nil {
		next.RankTimeMs = max(s.RankTimeMs, latest.CreateTimeMs)
		next.PreviewText = latest.Text()
		next.PreviewIsMine = latest.Role == protocol.RoleUser
	}
	since := max(s.ReadTimeMs, lastUser)
	next.UnreadCount = 0
	for _, msg := range m.messages[key] {
		if msg.Role == protocol.RoleAssistant && msg.CreateTimeMs > since {
			next.UnreadCount++
		}
	}
	if next.RankTimeMs != s.RankTimeMs || next.PreviewText != s.PreviewText || next.PreviewIsMine != s.PreviewIsMine ||
		next.UnreadCount != s.UnreadCount {
		next.UpdatedAt = now
		m.sessions[key] = next
	}
}

func compareMessages(a, b Message) int {
	return cmp.Or(cmp.Compare(a.CreateTimeMs, b.CreateTimeMs), cmp.Compare(a.Position, b.Position),
		strings.Compare(a.ExternalID, b.ExternalID))
}

// comparePreview orders preview candidates as the SQLite store does: time, position, then external id.
func comparePreview(a, b Message) int { return compareMessages(a, b) }

func (m *Memory) listedSession(s Session) bool {
	a, ok := m.accounts[s.AccountID]
	return ok && listed(a.Status)
}

func (m *Memory) Session(_ context.Context, key string) (Session, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[key]
	if !ok || !m.listedSession(s) {
		return Session{}, false, nil
	}
	return s, true, nil
}

// compareFeed is the feed order: pinned first (most recently pinned first), then rank time, then key, newest
// first.
func compareFeed(a, b Session) int {
	pinned := func(s Session) int {
		if s.OnTopRankTimeMs != 0 {
			return 1
		}
		return 0
	}
	rank := func(s Session) int64 {
		if s.OnTopRankTimeMs == 0 {
			return s.RankTimeMs
		}
		return 0
	}
	return cmp.Or(cmp.Compare(pinned(b), pinned(a)), cmp.Compare(b.OnTopRankTimeMs, a.OnTopRankTimeMs),
		cmp.Compare(rank(b), rank(a)), strings.Compare(b.SessionKey, a.SessionKey))
}

func (m *Memory) Sessions(_ context.Context, limit int) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for _, s := range m.sessions {
		if m.listedSession(s) && !s.IsHidden {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, compareFeed)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) AccountSessions(_ context.Context, id string) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for _, s := range m.sessions {
		if s.AccountID == id {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b Session) int {
		return cmp.Or(cmp.Compare(b.RankTimeMs, a.RankTimeMs), strings.Compare(b.SessionKey, a.SessionKey))
	})
	return out, nil
}

func (m *Memory) ActiveSessionKeys(_ context.Context, id string, since int64) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	active := map[string]bool{}
	for key, s := range m.sessions {
		if s.AccountID != id {
			continue
		}
		for _, msg := range m.messages[key] {
			if msg.CreateTimeMs >= since {
				active[key] = true
				break
			}
		}
	}
	for _, o := range m.outbox {
		if s, ok := m.sessions[o.SessionKey]; ok && s.AccountID == id && o.CreatedAt >= since {
			active[o.SessionKey] = true
		}
	}
	return slices.Sorted(maps.Keys(active)), nil
}

func (m *Memory) Messages(_ context.Context, key string, limit int, before *Cursor) ([]Message, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []Message
	for _, msg := range m.messages[key] {
		if before != nil && compareMessages(msg, Message{CreateTimeMs: before.CreateTimeMs, Position: before.Position,
			ExternalID: before.ExternalID}) >= 0 {
			continue
		}
		all = append(all, msg)
	}
	slices.SortFunc(all, compareMessages)
	hasMore := limit > 0 && len(all) > limit
	if hasMore {
		all = all[len(all)-limit:]
	}
	return all, hasMore, nil
}

func (m *Memory) MarkRead(_ context.Context, key string, now int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[key]
	if !ok {
		return ErrNotFound
	}
	s.ReadTimeMs = max(s.ReadTimeMs, now, s.RankTimeMs)
	s.UnreadCount = 0
	s.UpdatedAt = now
	m.sessions[key] = s
	return nil
}

func (m *Memory) Counts(_ context.Context, id string) (Counts, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var c Counts
	for key, s := range m.sessions {
		if s.AccountID != id {
			continue
		}
		c.Sessions++
		for _, msg := range m.messages[key] {
			c.Messages++
			c.LatestMessageMs = max(c.LatestMessageMs, msg.CreateTimeMs)
		}
	}
	return c, nil
}

func (m *Memory) EnqueueOutbox(_ context.Context, o Outbox) (Outbox, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.outbox[o.ClientID]; ok {
		return old, false, nil
	}
	o.Status, o.LastError, o.AttemptCount = protocol.LocalPending, "", 0
	o.UpdatedAt = o.CreatedAt
	m.outbox[o.ClientID] = o
	return o, true, nil
}

func (m *Memory) Outbox(_ context.Context, clientID string) (Outbox, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.outbox[clientID]
	return o, ok, nil
}

func (m *Memory) ClaimOutbox(_ context.Context, clientID string, now int64) (Outbox, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.outbox[clientID]
	if !ok || o.Status != protocol.LocalPending {
		return Outbox{}, false, nil
	}
	o.Status, o.AttemptCount, o.UpdatedAt = protocol.LocalClaimed, o.AttemptCount+1, now
	m.outbox[clientID] = o
	return o, true, nil
}

func (m *Memory) SettleOutbox(_ context.Context, clientID, status, lastError string, now int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.outbox[clientID]
	if !ok {
		return ErrNotFound
	}
	o.Status, o.LastError, o.UpdatedAt = status, lastError, now
	m.outbox[clientID] = o
	return nil
}

func (m *Memory) CancelOutbox(_ context.Context, clientID, key string, now int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.outbox[clientID]
	if !ok || o.SessionKey != key || o.Status != protocol.LocalPending {
		return false, nil
	}
	o.Status, o.LastError, o.UpdatedAt = protocol.LocalFailed, "CANCELLED", now
	m.outbox[clientID] = o
	return true, nil
}

func (m *Memory) DismissOutbox(_ context.Context, clientID, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.outbox[clientID]
	if !ok || o.SessionKey != key || o.Status != protocol.LocalFailed {
		return false, nil
	}
	delete(m.outbox, clientID)
	return true, nil
}

func (m *Memory) AbandonClaimedOutbox(_ context.Context, now int64) ([]Outbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Outbox
	for id, o := range m.outbox {
		if o.Status == protocol.LocalClaimed {
			o.Status, o.LastError, o.UpdatedAt = protocol.LocalUnconfirmed, "SEND_UNCONFIRMED", now
			m.outbox[id] = o
			out = append(out, o)
		}
	}
	sortOutbox(out)
	return out, nil
}

func (m *Memory) ConfirmOutbox(_ context.Context, clientID, status, lastError string, now int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.outbox[clientID]
	if !ok || o.Status != protocol.LocalUnconfirmed {
		return false, nil
	}
	o.Status, o.LastError, o.UpdatedAt = status, lastError, now
	m.outbox[clientID] = o
	return true, nil
}

func (m *Memory) PendingOutbox(context.Context) ([]Outbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Outbox
	for _, o := range m.outbox {
		if o.Status == protocol.LocalPending {
			out = append(out, o)
		}
	}
	sortOutbox(out)
	return out, nil
}

func (m *Memory) LocalMessages(_ context.Context, key string, since int64) ([]Outbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Outbox
	for _, o := range m.outbox {
		if o.SessionKey != key || (o.Status == protocol.LocalSent && o.UpdatedAt < since) {
			continue
		}
		echoed := false
		for _, msg := range m.messages[key] {
			if msg.Role == protocol.RoleUser && msg.CreateTimeMs >= o.CreatedAt-echoWindowMs &&
				echoText(msg.Text()) == echoText(o.Text) {
				echoed = true
				break
			}
		}
		if !echoed {
			out = append(out, o)
		}
	}
	sortOutbox(out)
	return out, nil
}

func sortOutbox(out []Outbox) {
	slices.SortFunc(out, func(a, b Outbox) int {
		return cmp.Or(cmp.Compare(a.CreatedAt, b.CreatedAt), strings.Compare(a.ClientID, b.ClientID))
	})
}

func (m *Memory) Search(_ context.Context, q string, limit int) ([]protocol.SearchHit, error) {
	query := parseQuery(q)
	if query.text == "" {
		return []protocol.SearchHit{}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []Message
	for _, byID := range m.messages {
		for _, msg := range byID {
			all = append(all, msg)
		}
	}
	slices.SortFunc(all, func(a, b Message) int {
		return cmp.Or(cmp.Compare(b.CreateTimeMs, a.CreateTimeMs), strings.Compare(a.SessionKey, b.SessionKey),
			strings.Compare(a.ExternalID, b.ExternalID))
	})
	hit := func(msg Message) (protocol.SearchHit, bool) {
		s, ok := m.sessions[msg.SessionKey]
		if !ok || !m.listedSession(s) {
			return protocol.SearchHit{}, false
		}
		return protocol.SearchHit{SessionKey: msg.SessionKey, ExternalID: msg.ExternalID, Text: msg.Text(), Role: msg.Role,
			CreateTimeMs: msg.CreateTimeMs, Title: s.Title, Provider: s.Provider}, true
	}
	hits := []protocol.SearchHit{}
	for _, msg := range all {
		if len(hits) == limit {
			break
		}
		if query.matches(msg.Text()) {
			if h, ok := hit(msg); ok {
				hits = append(hits, h)
			}
		}
	}
	for _, msg := range all[:min(len(all), substringScan)] {
		if len(hits) >= limit {
			break
		}
		if query.matches(msg.Text()) || !query.containsText(msg.Text()) {
			continue
		}
		if h, ok := hit(msg); ok {
			hits = append(hits, h)
		}
	}
	sortHits(hits)
	return hits, nil
}

func sortHits(hits []protocol.SearchHit) {
	slices.SortStableFunc(hits, func(a, b protocol.SearchHit) int { return cmp.Compare(b.CreateTimeMs, a.CreateTimeMs) })
}

func (m *Memory) Setting(_ context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.settings[key]
	return v, ok, nil
}

func (m *Memory) PutSetting(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings[key] = value
	return nil
}

func (m *Memory) Import(_ context.Context, rows Import) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range rows.Accounts {
		m.accounts[a.AccountID] = a
	}
	for _, s := range rows.Sessions {
		s.Ref = cloneRef(s.Ref)
		m.sessions[s.SessionKey] = s
	}
	for _, msg := range rows.Messages {
		m.putMessage(msg.SessionKey, msg, msg.CreateTimeMs)
	}
	for _, o := range rows.Outbox {
		if _, taken := m.outbox[o.ClientID]; !taken {
			m.outbox[o.ClientID] = o
		}
	}
	return nil
}

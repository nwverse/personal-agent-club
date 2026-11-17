package core

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/store"
)

// The sync schedule, as the Lark gateway's agent bridge has it.
const (
	sessionsPerSync = 20
	hotWindowMs     = 30 * 60_000
	rotatePerSync   = 3
	authRetryMs     = 15 * 60_000
)

// poll syncs every account of the space at each interval until ctx ends.
func (s *Space) poll(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	s.SyncAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SyncAll(ctx)
		}
	}
}

// SyncAll is one poll round: every active account, and every account in error whose last attempt is more than 15
// minutes old (a refused credential is not retried at every poll; club.accounts.sync still retries at once).
func (s *Space) SyncAll(ctx context.Context) {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		s.log().Warn("list accounts failed", "space", s.name, "err", err)
		return
	}
	for _, a := range accounts {
		if ctx.Err() != nil {
			return
		}
		if a.Status != protocol.AccountActive && a.Status != protocol.AccountError {
			continue
		}
		if a.Status == protocol.AccountError && a.LastSyncedAt > s.now()-authRetryMs {
			continue
		}
		s.sync(ctx, a.AccountID, "")
	}
}

// SyncAccount is club.accounts.sync: one sync round of the account now. A failure is recorded on the account
// (lastError), not returned.
func (s *Space) SyncAccount(ctx context.Context, id string) error {
	if _, err := s.requireAccount(ctx, id); err != nil {
		return err
	}
	s.sync(ctx, id, "")
	return nil
}

// sync is one sync of an account, or of one of its sessions only; a second caller joins the one running.
func (s *Space) sync(ctx context.Context, id, only string) {
	_, _ = join(&s.mu, s.syncing, id+"|"+only, func() (struct{}, error) {
		if err := s.runSync(ctx, id, only, false); err != nil {
			s.log().Warn("sync failed", "space", s.name, "account", id, "err", err)
		}
		return struct{}{}, nil
	})
}

// syncable says whether the account may still be written by a sync (it was not paused or unpaired meanwhile).
func (s *Space) syncable(ctx context.Context, id string) bool {
	a, ok, err := s.store.Account(ctx, id)
	return err == nil && ok && a.Status != protocol.AccountPaused
}

// runSync reads the account's sessions, then the messages of the sessions due. A provider's failure is recorded on
// the account; an AUTH_FAILED is retried once with a refreshed credential. The returned error is the store's.
func (s *Space) runSync(ctx context.Context, id, only string, refreshed bool) error {
	acct, ok, err := s.store.Account(ctx, id)
	if err != nil || !ok || acct.Status == protocol.AccountPaused {
		return err
	}
	adapter, ok := s.club.opts.Registry.Get(acct.Provider)
	cred, found, err := s.vault.Get(ctx, id)
	if err != nil || !ok || !found {
		return err
	}
	before, err := s.store.AccountSessions(ctx, id)
	if err != nil {
		return err
	}
	var touched []string
	listChanged := 0
	perr := func() error {
		if wantsHistory(adapter) {
			// Nothing remote to poll: the session only has to exist. The status is left alone, so a key a send
			// found refused stays in error.
			if len(before) > 0 {
				return nil
			}
			remote, err := adapter.ListSessions(ctx, cred)
			if err != nil {
				return err
			}
			if !s.syncable(ctx, id) {
				return nil
			}
			if listChanged, err = s.store.UpsertSessions(ctx, sessionRows(acct, remote), s.now()); err != nil {
				return storeError{err}
			}
			return nil
		}
		sessions := before
		activity := map[string]int64{}
		if only == "" {
			remote, err := adapter.ListSessions(ctx, cred)
			if err != nil {
				return err
			}
			for _, r := range remote {
				if r.LastActivityMs > 0 {
					activity[r.ExternalID] = r.LastActivityMs
				}
			}
			if !s.syncable(ctx, id) {
				return nil
			}
			if listChanged, err = s.store.UpsertSessions(ctx, sessionRows(acct, remote), s.now()); err != nil {
				return storeError{err}
			}
			if sessions, err = s.store.AccountSessions(ctx, id); err != nil {
				return storeError{err}
			}
		}
		var due []store.Session
		if only != "" {
			for _, x := range sessions {
				if x.SessionKey == only {
					due = append(due, x)
				}
			}
		} else {
			if due, err = s.sessionsDue(ctx, acct, before, sessions); err != nil {
				return storeError{err}
			}
		}
		for _, x := range due {
			page, err := adapter.ListMessages(ctx, cred, provider.SessionRef{ExternalID: x.ExternalID, Ref: x.Ref},
				provider.ListOptions{Limit: messagePage})
			if err != nil {
				return err
			}
			if !s.syncable(ctx, id) {
				return nil
			}
			n, err := s.store.UpsertMessages(ctx, x.SessionKey, storeMessages(x.SessionKey, page.Messages, activity[x.ExternalID]), s.now())
			if err != nil {
				return storeError{err}
			}
			s.mu.Lock()
			s.fetchedAt[x.SessionKey] = s.now()
			s.mu.Unlock()
			if n > 0 {
				touched = append(touched, x.SessionKey)
			}
		}
		if !s.syncable(ctx, id) {
			return nil
		}
		active, empty, now := protocol.AccountActive, "", s.now()
		patch := store.AccountPatch{Status: &active, LastError: &empty}
		if only == "" {
			patch.LastSyncedAt = &now
		}
		if err := s.store.UpdateAccount(ctx, id, patch, now); err != nil {
			return storeError{err}
		}
		return nil
	}()
	if se, ok := perr.(storeError); ok {
		return se.err
	}
	if perr != nil {
		s.observe(id, perr)
		code := provider.CodeOf(perr)
		auth := code == provider.CodeAuthFailed
		if auth && !refreshed {
			if fresh := s.refresh(ctx, id, adapter, cred); fresh != "" {
				return s.runSync(ctx, id, only, true)
			}
		}
		if acct.Status == protocol.AccountError {
			s.log().Debug("sync failed", "space", s.name, "account", id, "provider", acct.Provider, "code", code)
		} else {
			s.log().Warn("sync failed", "space", s.name, "account", id, "provider", acct.Provider, "code", code)
		}
		if !s.syncable(ctx, id) {
			return nil
		}
		patch := store.AccountPatch{LastError: &code}
		if auth {
			st := protocol.AccountError
			patch.Status = &st
		}
		if (auth || acct.Status == protocol.AccountError) && only == "" {
			now := s.now()
			patch.LastSyncedAt = &now
		}
		if err := s.store.UpdateAccount(ctx, id, patch, s.now()); err != nil {
			return err
		}
		s.notify(changedKeys(1, touched))
		return nil
	}
	if listChanged > 0 || len(touched) > 0 || acct.Status != protocol.AccountActive || acct.LastError != "" {
		s.notify(changedKeys(listChanged, touched))
	}
	return nil
}

type storeError struct{ err error }

func (e storeError) Error() string { return e.err.Error() }

func sessionRows(acct store.Account, remote []provider.RemoteSession) []store.Session {
	rows := make([]store.Session, len(remote))
	for i, r := range remote {
		rows[i] = store.Session{SessionKey: SessionKey(acct.Provider, acct.AccountID, r.ExternalID), AccountID: acct.AccountID,
			Provider: acct.Provider, ExternalID: r.ExternalID, Title: r.Title, Kind: cmpOr(r.Kind, protocol.KindMain),
			CanSend: r.CanSend, AvatarURL: r.AvatarURL, Ref: r.Ref, RankTimeMs: r.LastActivityMs}
		if r.ParentExternalID != "" {
			rows[i].ParentSessionKey = SessionKey(acct.Provider, acct.AccountID, r.ParentExternalID)
		}
	}
	return rows
}

// sessionsDue are the sessions a sync reads, at most 20, in priority order: sessions whose activity time moved (or
// new ones), sessions with a message or send in the last 30 minutes (a bot's profile time does not move when it
// posts), then other sessions among the three read least recently. The latter have reserved slots.
func (s *Space) sessionsDue(ctx context.Context, acct store.Account, before, sessions []store.Session) ([]store.Session, error) {
	known := map[string]bool{}
	for _, x := range before {
		known[x.ExternalID] = true
	}
	var changed []store.Session
	for _, x := range sessions {
		if acct.LastSyncedAt == 0 || x.RankTimeMs >= acct.LastSyncedAt || !known[x.ExternalID] {
			changed = append(changed, x)
		}
	}
	activeKeys, err := s.store.ActiveSessionKeys(ctx, acct.AccountID, s.now()-hotWindowMs)
	if err != nil {
		return nil, err
	}
	var hot []store.Session
	for _, x := range sessions {
		if slices.Contains(activeKeys, x.SessionKey) {
			hot = append(hot, x)
		}
	}
	s.mu.Lock()
	rotating := slices.Clone(sessions)
	slices.SortStableFunc(rotating, func(a, b store.Session) int {
		return cmp.Or(cmp.Compare(s.fetchedAt[a.SessionKey], s.fetchedAt[b.SessionKey]), strings.Compare(a.SessionKey, b.SessionKey))
	})
	s.mu.Unlock()
	rotating = rotating[:min(len(rotating), rotatePerSync)]
	reserved := make(map[string]bool, len(rotating))
	for _, x := range rotating {
		reserved[x.SessionKey] = true
	}
	var out []store.Session
	seen := map[string]bool{}
	for _, x := range slices.Concat(changed, hot, rotating) {
		if seen[x.SessionKey] {
			continue
		}
		if !reserved[x.SessionKey] && len(out)+len(reserved) >= sessionsPerSync {
			continue
		}
		delete(reserved, x.SessionKey)
		seen[x.SessionKey] = true
		out = append(out, x)
	}
	return out, nil
}

// changedKeys is what an updated event names: nil (the list) when sessions were added or changed.
func changedKeys(listChanged int, touched []string) []string {
	if listChanged > 0 {
		return nil
	}
	return touched
}

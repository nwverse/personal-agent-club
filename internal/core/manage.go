package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/store"
)

const (
	grokBotLoginTTLMs    = 10 * 60_000
	grokBotLoginsWaiting = 2
	liveStateTTLMs       = 3_000
	grokBot              = "grok_bot"
	settingIdentity      = "browserIdentity"
)

// background runs f after the call that started it, as the gateway's "void this.syncAccount(...)".
func (s *Space) background(f func(ctx context.Context)) { s.club.after(0, func() { f(s.club.ctx) }) }

// UpdateAccount is club.accounts.update: pause or resume (which clears lastError and, resuming, syncs), rename,
// and set the assistant control level and the quota display switch.
func (s *Space) UpdateAccount(ctx context.Context, p protocol.UpdateAccountParams) (protocol.Account, error) {
	a, err := s.requireAccount(ctx, p.AccountID)
	if err != nil {
		return protocol.Account{}, err
	}
	var patch store.AccountPatch
	if p.DisplayName != nil {
		name := strings.TrimSpace(*p.DisplayName)
		if name == "" || len(utf16.Encode([]rune(name))) > maxDisplayName {
			return protocol.Account{}, badInput("display_name must be 1 to 80 characters")
		}
		patch.DisplayName = &name
	}
	if p.AssistantControl != nil {
		c, err := ParseControl(*p.AssistantControl)
		if err != nil || *p.AssistantControl == "" {
			return protocol.Account{}, badInput("assistant_control must be off, read, confirm or auto")
		}
		level := string(c)
		patch.AssistantControl = &level
	}
	patch.ShowQuota = p.ShowQuota
	if p.Paused != nil {
		st, empty := protocol.AccountActive, ""
		if *p.Paused {
			st = protocol.AccountPaused
		}
		patch.Status, patch.LastError = &st, &empty
	}
	if err := s.store.UpdateAccount(ctx, a.AccountID, patch, s.now()); err != nil {
		return protocol.Account{}, err
	}
	if p.Paused != nil && !*p.Paused {
		s.background(func(ctx context.Context) { s.sync(ctx, a.AccountID, "") })
	}
	s.notify(nil)
	return s.Account(ctx, a.AccountID)
}

// ReplaceCredential is club.accounts.replace_credential: a new credential for the same remote account (a credential
// of another remote account is refused). The account becomes active again unless it is paused.
func (s *Space) ReplaceCredential(ctx context.Context, p protocol.ReplaceCredentialParams) (protocol.Account, error) {
	a, err := s.requireAccount(ctx, p.AccountID)
	if err != nil {
		return protocol.Account{}, err
	}
	cred := strings.TrimSpace(p.Credential)
	if err := checkCredential(cred); err != nil {
		return protocol.Account{}, err
	}
	adapter, err := s.adapter(a.Provider)
	if err != nil {
		return protocol.Account{}, err
	}
	remote, err := adapter.Verify(ctx, cred)
	s.observe(a.AccountID, err)
	if err != nil {
		return protocol.Account{}, verifyError(err)
	}
	s.noteQuota(a.AccountID, remote.Quota)
	if a.ExternalAccountID != "" && remote.ExternalAccountID != a.ExternalAccountID {
		return protocol.Account{}, protocol.Conflict("the credential belongs to another account").Because(protocol.ReasonAccountMismatch)
	}
	if err := s.vault.Put(ctx, a.AccountID, cred); err != nil {
		return protocol.Account{}, err
	}
	st, empty := protocol.AccountActive, ""
	patch := store.AccountPatch{Status: &st, PreservePaused: true, LastError: &empty}
	if err := s.store.UpdateAccount(ctx, a.AccountID, patch, s.now()); err != nil {
		return protocol.Account{}, err
	}
	s.notify(nil)
	updated, err := s.Account(ctx, a.AccountID)
	if err == nil && updated.Status != protocol.AccountPaused {
		s.background(func(ctx context.Context) { s.sync(ctx, a.AccountID, "") })
	}
	return updated, err
}

type liveEntry struct {
	at    int64
	done  chan struct{}
	state protocol.LiveState
	err   error
}

// Live is club.live: whether the agent is working on the session, asked at most every 3 s per session; callers
// joining a lookup get its answer or its error.
func (s *Space) Live(ctx context.Context, p protocol.SessionParams) (protocol.LiveState, error) {
	sess, err := s.requireSession(ctx, p.SessionKey)
	if err != nil {
		return protocol.LiveState{}, err
	}
	adapter, _ := s.club.opts.Registry.Get(sess.Provider)
	ls, ok := adapter.(provider.LiveStater)
	if !ok {
		return protocol.LiveState{}, nil
	}
	now := s.now()
	s.mu.Lock()
	for k, e := range s.live {
		if now-e.at > liveStateTTLMs {
			delete(s.live, k)
		}
	}
	if e := s.live[sess.SessionKey]; e != nil {
		s.mu.Unlock()
		<-e.done
		return e.state, e.err
	}
	e := &liveEntry{at: now, done: make(chan struct{})}
	s.live[sess.SessionKey] = e
	s.mu.Unlock()
	e.state, e.err = s.lookupLive(ctx, sess, ls)
	if e.err != nil {
		s.mu.Lock()
		delete(s.live, sess.SessionKey)
		s.mu.Unlock()
	}
	close(e.done)
	return e.state, e.err
}

func (s *Space) lookupLive(ctx context.Context, sess store.Session, ls provider.LiveStater) (protocol.LiveState, error) {
	a, err := s.requireAccount(ctx, sess.AccountID)
	if err != nil {
		return protocol.LiveState{}, err
	}
	cred, found, err := s.vault.Get(ctx, sess.AccountID)
	if err != nil {
		return protocol.LiveState{}, err
	}
	if a.Status == protocol.AccountPaused || !found {
		return protocol.LiveState{}, nil
	}
	st, err := ls.LiveState(ctx, cred, provider.SessionRef{ExternalID: sess.ExternalID, Ref: sess.Ref})
	s.observe(sess.AccountID, err)
	if err != nil {
		return protocol.LiveState{}, protocol.Failure(provider.CodeOf(err), "the provider did not answer: "+provider.CodeOf(err))
	}
	return protocol.LiveState{Supported: true, Running: st.Running, Composing: st.Composing}, nil
}

type login struct {
	flow      provider.LoginFlow
	createdAt int64
	accountID string
}

func newLoginID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "g" + hex.EncodeToString(b[:])
}

func (s *Space) deviceLogin() (provider.DeviceLogin, error) {
	adapter, err := s.adapter(grokBot)
	if err != nil {
		return nil, err
	}
	d, ok := adapter.(provider.DeviceLogin)
	if !ok {
		return nil, protocol.Unsupported("grok_bot has no sign-in in this build").Because(protocol.ReasonProviderUnavailable)
	}
	return d, nil
}

// StartGrokBotLogin is club.logins.grok_bot.start: the approval URL only, the verifier and the token stay in club.
// A sign-in lasts 10 minutes; at most two wait at once. account_id signs that Grok Bot account in again.
func (s *Space) StartGrokBotLogin(ctx context.Context, p protocol.GrokBotLoginStartParams) (protocol.GrokBotLoginStart, error) {
	d, err := s.deviceLogin()
	if err != nil {
		return protocol.GrokBotLoginStart{}, err
	}
	if p.AccountID != "" {
		a, err := s.requireAccount(ctx, p.AccountID)
		if err != nil {
			return protocol.GrokBotLoginStart{}, err
		}
		if a.Provider != grokBot {
			return protocol.GrokBotLoginStart{}, protocol.Conflict("the account is not a Grok Bot account").Because(protocol.ReasonProviderMismatch)
		}
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.logins {
		if now-l.createdAt > grokBotLoginTTLMs {
			delete(s.logins, id)
		}
	}
	if len(s.logins) >= grokBotLoginsWaiting {
		return protocol.GrokBotLoginStart{}, protocol.Conflict("two sign-ins are already waiting").Because(protocol.ReasonLoginLimit)
	}
	flow, err := d.StartLogin()
	if err != nil {
		return protocol.GrokBotLoginStart{}, err
	}
	id := newLoginID()
	s.logins[id] = &login{flow: flow, createdAt: now, accountID: p.AccountID}
	return protocol.GrokBotLoginStart{LoginID: id, LoginURL: flow.URL}, nil
}

func noLogin() *protocol.Error {
	return protocol.NotFound("no such sign-in").Because(protocol.ReasonLoginNotFound)
}

// PollGrokBotLogin is club.logins.grok_bot.poll: pending until the user approved, then the account, paired anew or
// (for a sign-in again) with its credential replaced. The token is never answered.
func (s *Space) PollGrokBotLogin(ctx context.Context, p protocol.LoginParams) (protocol.GrokBotLoginPoll, error) {
	d, err := s.deviceLogin()
	if err != nil {
		return protocol.GrokBotLoginPoll{}, err
	}
	s.mu.Lock()
	l := s.logins[p.LoginID]
	if l == nil || s.now()-l.createdAt > grokBotLoginTTLMs {
		delete(s.logins, p.LoginID)
		s.mu.Unlock()
		return protocol.GrokBotLoginPoll{}, noLogin()
	}
	s.mu.Unlock()
	cred, err := d.PollLogin(ctx, l.flow)
	if err != nil {
		var pe *provider.Error
		if !errors.As(err, &pe) || !pe.Retryable {
			s.dropLogin(p.LoginID)
		}
		return protocol.GrokBotLoginPoll{}, protocol.Invalid("the sign-in failed: " + provider.CodeOf(err)).Because(provider.CodeOf(err))
	}
	if cred == "" {
		return protocol.GrokBotLoginPoll{Status: protocol.LoginPending}, nil
	}
	s.dropLogin(p.LoginID)
	var acct protocol.Account
	if l.accountID != "" {
		acct, err = s.ReplaceCredential(ctx, protocol.ReplaceCredentialParams{AccountID: l.accountID, Credential: cred})
	} else {
		acct, err = s.AddAccount(ctx, protocol.AddAccountParams{Provider: grokBot, Credential: cred, DisplayName: p.DisplayName})
	}
	if err != nil {
		return protocol.GrokBotLoginPoll{}, err
	}
	return protocol.GrokBotLoginPoll{Status: protocol.LoginDone, Account: &acct}, nil
}

func (s *Space) dropLogin(id string) {
	s.mu.Lock()
	delete(s.logins, id)
	s.mu.Unlock()
}

// CancelGrokBotLogin is club.logins.grok_bot.cancel.
func (s *Space) CancelGrokBotLogin(_ context.Context, p protocol.LoginParams) error {
	s.dropLogin(p.LoginID)
	return nil
}

var (
	localeID   = regexp.MustCompile(`^([A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*)?$`)
	timezoneID = regexp.MustCompile(`^([A-Za-z_]+(/[A-Za-z0-9_+-]+)*)?$`)
	browserArg = regexp.MustCompile(`^--(window-size=\d{2,5},\d{2,5}|lang=[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*|accept-lang=[A-Za-z0-9-]+(,[A-Za-z0-9\-;=.]+)*|disable-blink-features=AutomationControlled|force-device-scale-factor=\d+(\.\d+)?|force-color-profile=[A-Za-z0-9_-]+|hide-scrollbars|mute-audio)$`)
)

func len16(s string) int { return len(utf16.Encode([]rune(s))) }

// cleanIdentity checks a browser identity as the gateway's schema does, trimming its fields.
func cleanIdentity(in protocol.BrowserIdentity) (protocol.BrowserIdentity, error) {
	out := protocol.BrowserIdentity{UserAgent: strings.TrimSpace(in.UserAgent), Locale: strings.TrimSpace(in.Locale),
		Timezone: strings.TrimSpace(in.Timezone), ExtraArgs: []string{}}
	switch {
	case len16(out.UserAgent) > 512:
		return out, badInput("userAgent is longer than 512 characters")
	case !localeID.MatchString(out.Locale):
		return out, badInput("locale is not a language tag")
	case len16(out.Timezone) > 64 || !timezoneID.MatchString(out.Timezone):
		return out, badInput("timezone is not a time zone name")
	case len(in.ExtraArgs) > 32:
		return out, badInput("at most 32 extraArgs")
	}
	for _, a := range in.ExtraArgs {
		a = strings.TrimSpace(a)
		if len16(a) > 512 || !browserArg.MatchString(a) {
			return out, badInput("extraArgs may only hold the presentation switches the Lark gateway allows")
		}
		out.ExtraArgs = append(out.ExtraArgs, a)
	}
	return out, nil
}

// Settings is club.settings.get. Club only keeps the browser identity in this version.
func (s *Space) Settings(ctx context.Context) (protocol.Settings, error) {
	raw, ok, err := s.store.Setting(ctx, settingIdentity)
	if err != nil {
		return protocol.Settings{}, err
	}
	var id protocol.BrowserIdentity
	if ok {
		_ = json.Unmarshal([]byte(raw), &id)
	}
	clean, err := cleanIdentity(id)
	if err != nil {
		clean = protocol.BrowserIdentity{ExtraArgs: []string{}}
	}
	return protocol.Settings{BrowserIdentity: clean}, nil
}

// SetSettings is club.settings.set.
func (s *Space) SetSettings(ctx context.Context, p protocol.SettingsParams) (protocol.Settings, error) {
	if p.BrowserIdentity == nil {
		return protocol.Settings{}, badInput("browser_identity is required")
	}
	if err := s.putIdentity(ctx, *p.BrowserIdentity); err != nil {
		return protocol.Settings{}, err
	}
	s.notify(nil)
	return s.Settings(ctx)
}

func (s *Space) putIdentity(ctx context.Context, in protocol.BrowserIdentity) error {
	clean, err := cleanIdentity(in)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(clean)
	return s.store.PutSetting(ctx, settingIdentity, string(b))
}

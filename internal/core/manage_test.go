package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/store"
)

// scripted is a provider whose every answer a test can change; it counts calls.
type scripted struct {
	name string
	mu   sync.Mutex

	ext      string
	verify   func(cred string) error
	sessions []provider.RemoteSession
	listErr  func(cred string) error
	messages map[string][]provider.RemoteMessage
	listed   []string
	send     func(cred string, msg provider.Outgoing) (provider.SendResult, error)
	sends    []string
	history  bool
	live     func() (provider.LiveState, error)
	lives    int
	refresh  func(cred string) (string, error)
	poll     []string
	watch    func(event func(provider.WatchEvent) bool) error
}

func newScripted(name string) *scripted {
	return &scripted{name: name, ext: "remote-1", messages: map[string][]provider.RemoteMessage{}}
}

func (p *scripted) Name() string       { return p.name }
func (p *scripted) Connection() string { return protocol.ConnectionServer }
func (p *scripted) Verify(_ context.Context, cred string) (provider.RemoteAccount, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.verify != nil {
		if err := p.verify(cred); err != nil {
			return provider.RemoteAccount{}, err
		}
	}
	return provider.RemoteAccount{ExternalAccountID: p.ext, DisplayName: "Scripted"}, nil
}
func (p *scripted) ListSessions(_ context.Context, cred string) ([]provider.RemoteSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listErr != nil {
		if err := p.listErr(cred); err != nil {
			return nil, err
		}
	}
	return append([]provider.RemoteSession(nil), p.sessions...), nil
}
func (p *scripted) ListMessages(_ context.Context, _ string, ref provider.SessionRef, _ provider.ListOptions) (provider.RemotePage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listed = append(p.listed, ref.ExternalID)
	return provider.RemotePage{Messages: append([]provider.RemoteMessage(nil), p.messages[ref.ExternalID]...)}, nil
}
func (p *scripted) Send(_ context.Context, cred string, _ provider.SessionRef, msg provider.Outgoing, _ provider.SendContext) (provider.SendResult, error) {
	p.mu.Lock()
	p.sends = append(p.sends, msg.ClientID)
	f := p.send
	p.mu.Unlock()
	if f == nil {
		return provider.SendResult{Status: provider.Sent}, nil
	}
	return f(cred, msg)
}
func (p *scripted) WantsHistory() bool { return p.history }
func (p *scripted) LiveState(context.Context, string, provider.SessionRef) (provider.LiveState, error) {
	p.mu.Lock()
	p.lives++
	f := p.live
	p.mu.Unlock()
	return f()
}
func (p *scripted) RefreshCredential(_ context.Context, cred string) (string, error) {
	if p.refresh == nil {
		return "", nil
	}
	return p.refresh(cred)
}
func (p *scripted) StartLogin() (provider.LoginFlow, error) {
	return provider.LoginFlow{URL: "https://example.com/approve?challenge=x", State: "verifier-secret"}, nil
}
func (p *scripted) PollLogin(context.Context, provider.LoginFlow) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.poll) == 0 {
		return "", &provider.Error{Code: "LOGIN_REFUSED"}
	}
	next := p.poll[0]
	p.poll = p.poll[1:]
	return next, nil
}
func (p *scripted) Watch(ctx context.Context, _ string, _ provider.SessionRef, event func(provider.WatchEvent) bool) error {
	if p.watch == nil {
		<-ctx.Done()
		return nil
	}
	return p.watch(event)
}

func (p *scripted) listedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.listed)
}

type env struct {
	club  *Club
	space *Space
	p     *scripted
	clock *clock
}

func newEnv(t *testing.T, name string, followUps ...time.Duration) *env {
	t.Helper()
	p := newScripted(name)
	clk := &clock{t: time.UnixMilli(1_800_000_000_000)}
	if followUps == nil {
		followUps = []time.Duration{}
	}
	club := New(Options{Version: "test", Registry: provider.NewRegistry(p), Now: clk.Now, FollowUps: followUps, WatchLimit: time.Second})
	t.Cleanup(club.Close)
	space, err := club.Space(context.Background(), DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	return &env{club: club, space: space, p: p, clock: clk}
}

func (e *env) advance(d time.Duration) {
	e.clock.mu.Lock()
	e.clock.t = e.clock.t.Add(d)
	e.clock.mu.Unlock()
}

func (e *env) nowMs() int64 { return e.clock.Now().UnixMilli() }

func (e *env) pair(t *testing.T, cred string) protocol.Account {
	t.Helper()
	a, err := e.space.AddAccount(context.Background(), protocol.AddAccountParams{Provider: e.p.name, Credential: cred})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func reason(err error) string { return protocol.ReasonOf(err) }

func TestReasonsNameTheGatewaysErrorCodes(t *testing.T) {
	e := newEnv(t, "cue")
	e.p.sessions = []provider.RemoteSession{{ExternalID: "main", Title: "Cue", CanSend: true},
		{ExternalID: "sub", Title: "Sub", Kind: protocol.KindTask, ParentExternalID: "main"}}
	ctx := context.Background()
	a := e.pair(t, "fixture-credential-1")
	main, sub := SessionKey("cue", a.AccountID, "main"), SessionKey("cue", a.AccountID, "sub")
	for _, tc := range []struct {
		err          error
		code, reason string
	}{
		{second(e.space.Send(ctx, protocol.SendParams{SessionKey: "agent:cue:a000000000000:x", ClientID: uuid(1), Text: "x"})), "not_found", "AGENT_SESSION_NOT_FOUND"},
		{second(e.space.Send(ctx, protocol.SendParams{SessionKey: sub, ClientID: uuid(1), Text: "x"})), "conflict", "SESSION_READ_ONLY"},
		{second(e.space.Send(ctx, protocol.SendParams{SessionKey: main, ClientID: "nope", Text: "x"})), "invalid", "INVALID_AGENT_BRIDGE_INPUT"},
		{second(e.space.Messages(ctx, protocol.MessagesParams{SessionKey: main, Cursor: "!!"})), "invalid", "INVALID_CURSOR"},
		{e.space.DeleteMessage(ctx, protocol.DeleteMessageParams{SessionKey: main, ClientID: uuid(9)}), "conflict", "NOT_PENDING"},
		{e.space.DismissOutbox(ctx, protocol.OutboxParams{SessionKey: main, ClientID: uuid(9)}), "conflict", "NOT_PENDING"},
		{second(e.space.Account(ctx, "a000000000000")), "not_found", "AGENT_ACCOUNT_NOT_FOUND"},
		{second(e.space.AddAccount(ctx, protocol.AddAccountParams{Provider: "cue", Credential: "fixture-credential-2"})), "conflict", "ACCOUNT_ALREADY_PAIRED"},
		{second(e.space.AddAccount(ctx, protocol.AddAccountParams{Provider: "muse", Credential: "fixture-credential-2"})), "unsupported", "PROVIDER_UNAVAILABLE"},
		{second(e.club.Call(ctx, DefaultSpace, "club.sessions.interrupt", nil)), "unsupported", "PROVIDER_UNAVAILABLE"},
	} {
		if protocol.CodeOf(tc.err) != tc.code || reason(tc.err) != tc.reason {
			t.Errorf("%v: code %q reason %q, want %s %s", tc.err, protocol.CodeOf(tc.err), reason(tc.err), tc.code, tc.reason)
		}
	}
	paused := true
	if _, err := e.space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: a.AccountID, Paused: &paused}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.space.Send(ctx, protocol.SendParams{SessionKey: main, ClientID: uuid(2), Text: "x"}); reason(err) != "ACCOUNT_PAUSED" {
		t.Fatal(err)
	}
	e.p.verify = func(string) error { return &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: 401} }
	_, err := e.space.AddAccount(ctx, protocol.AddAccountParams{Provider: "cue", Credential: "fixture-credential-3"})
	if protocol.CodeOf(err) != "invalid" || reason(err) != "AUTH_FAILED" {
		t.Fatal(err)
	}
}

func second[T any](_ T, err error) error { return err }

func TestUpdateAccount(t *testing.T) {
	e := newEnv(t, "cue")
	ctx := context.Background()
	a := e.pair(t, "fixture-credential-1")
	if a.AssistantControl != "off" || a.ShowQuota {
		t.Fatalf("%+v", a)
	}
	paused, name, level, show := true, " Mine ", "confirm", true
	got, err := e.space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: a.AccountID, Paused: &paused,
		DisplayName: &name, AssistantControl: &level, ShowQuota: &show})
	if err != nil || got.Status != protocol.AccountPaused || got.DisplayName != "Mine" || got.AssistantControl != "confirm" || !got.ShowQuota {
		t.Fatalf("%+v %v", got, err)
	}
	if c, _ := e.space.AssistantControl(ctx, a.AccountID); c != ControlConfirm {
		t.Fatal(c)
	}
	for _, bad := range []protocol.UpdateAccountParams{{AccountID: a.AccountID, AssistantControl: new("always")},
		{AccountID: a.AccountID, AssistantControl: new("")}, {AccountID: a.AccountID, DisplayName: new(" ")},
		{AccountID: a.AccountID, DisplayName: new(strings.Repeat("x", 81))}} {
		if _, err := e.space.UpdateAccount(ctx, bad); protocol.CodeOf(err) != "invalid" {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	resume := false
	got, _ = e.space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: a.AccountID, Paused: &resume})
	if got.Status != protocol.AccountActive {
		t.Fatalf("%+v", got)
	}
}

func TestReplaceCredentialKeepsTheRemoteAccount(t *testing.T) {
	e := newEnv(t, "cue")
	ctx := context.Background()
	a := e.pair(t, "fixture-credential-1")
	e.p.verify = func(string) error { return &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: 401} }
	if _, err := e.space.ReplaceCredential(ctx, protocol.ReplaceCredentialParams{AccountID: a.AccountID, Credential: "fixture-credential-2"}); reason(err) != "AUTH_FAILED" {
		t.Fatal(err)
	}
	e.p.verify, e.p.ext = nil, "someone-else"
	if _, err := e.space.ReplaceCredential(ctx, protocol.ReplaceCredentialParams{AccountID: a.AccountID, Credential: "fixture-credential-2"}); reason(err) != "ACCOUNT_MISMATCH" {
		t.Fatal(err)
	}
	if c, _, _ := e.space.vault.Get(ctx, a.AccountID); c != "fixture-credential-1" {
		t.Fatal("a refused credential replaced the stored one")
	}
	e.p.ext = "remote-1"
	st, code := protocol.AccountError, "AUTH_FAILED"
	_ = e.space.store.UpdateAccount(ctx, a.AccountID, store.AccountPatch{Status: &st, LastError: &code}, 1)
	got, err := e.space.ReplaceCredential(ctx, protocol.ReplaceCredentialParams{AccountID: a.AccountID, Credential: "fixture-credential-2"})
	if err != nil || got.Status != protocol.AccountActive || got.LastError != "" {
		t.Fatalf("%+v %v", got, err)
	}
	if c, _, _ := e.space.vault.Get(ctx, a.AccountID); c != "fixture-credential-2" {
		t.Fatal(c)
	}
	if b, _ := json.Marshal(got); strings.Contains(string(b), "fixture-credential") {
		t.Fatalf("a credential came back: %s", b)
	}
}

func TestDismissReleasesAFailedClientID(t *testing.T) {
	e := newEnv(t, "cue")
	e.p.sessions = []provider.RemoteSession{{ExternalID: "main", CanSend: true}}
	ctx := context.Background()
	a := e.pair(t, "fixture-credential-1")
	main := SessionKey("cue", a.AccountID, "main")
	e.p.send = func(string, provider.Outgoing) (provider.SendResult, error) {
		return provider.SendResult{}, &provider.Error{Code: "UPSTREAM_ERROR", HTTPStatus: 502}
	}
	m, _ := e.space.Send(ctx, protocol.SendParams{SessionKey: main, ClientID: uuid(7), Text: "hello"})
	if m.Local.Status != protocol.LocalFailed {
		t.Fatalf("%+v", m.Local)
	}
	if err := e.space.DismissOutbox(ctx, protocol.OutboxParams{SessionKey: main, ClientID: uuid(7)}); err != nil {
		t.Fatal(err)
	}
	page, _ := e.space.Messages(ctx, protocol.MessagesParams{SessionKey: main})
	if len(page.Messages) != 0 {
		t.Fatalf("%+v", page.Messages)
	}
	e.p.send = nil
	if m, _ := e.space.Send(ctx, protocol.SendParams{SessionKey: main, ClientID: uuid(7), Text: "hello"}); m.Local.Status != protocol.LocalSent || len(e.p.sends) != 2 {
		t.Fatalf("%+v %v", m.Local, e.p.sends)
	}
}

func TestLiveAsksAtMostEveryThreeSeconds(t *testing.T) {
	e := newEnv(t, "grok_bot")
	e.p.sessions = []provider.RemoteSession{{ExternalID: "bot/", CanSend: true}}
	ctx := context.Background()
	a := e.pair(t, "fixture-credential-1")
	key := SessionKey("grok_bot", a.AccountID, "bot/")
	e.p.live = func() (provider.LiveState, error) { return provider.LiveState{Running: true}, nil }
	if got, err := e.space.Live(ctx, protocol.SessionParams{SessionKey: key}); err != nil || got != (protocol.LiveState{Supported: true, Running: true}) {
		t.Fatalf("%+v %v", got, err)
	}
	_, _ = e.space.Live(ctx, protocol.SessionParams{SessionKey: key})
	if e.p.lives != 1 {
		t.Fatalf("asked %d times", e.p.lives)
	}
	e.advance(3100 * time.Millisecond)
	e.p.live = func() (provider.LiveState, error) {
		return provider.LiveState{}, &provider.Error{Code: "UPSTREAM_ERROR", HTTPStatus: 502, Retryable: true}
	}
	_, err := e.space.Live(ctx, protocol.SessionParams{SessionKey: key})
	if protocol.CodeOf(err) != "" || reason(err) != "UPSTREAM_ERROR" {
		t.Fatalf("%#v", err)
	}
	fx := setup(t)
	if got, err := fx.space.Live(ctx, protocol.SessionParams{SessionKey: fx.main}); err != nil || got != (protocol.LiveState{}) {
		t.Fatalf("a provider that cannot say: %+v %v", got, err)
	}
}

func TestGrokBotLogin(t *testing.T) {
	e := newEnv(t, "grok_bot")
	ctx := context.Background()
	token := `{"accessToken":"fixture-grok-token-0001","refreshToken":"fixture-refresh"}`
	e.p.poll = []string{"", token}
	start, err := e.space.StartGrokBotLogin(ctx, protocol.GrokBotLoginStartParams{})
	if err != nil || !strings.Contains(start.LoginURL, "challenge=") || !strings.HasPrefix(start.LoginID, "g") {
		t.Fatalf("%+v %v", start, err)
	}
	if _, err := e.space.StartGrokBotLogin(ctx, protocol.GrokBotLoginStartParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.space.StartGrokBotLogin(ctx, protocol.GrokBotLoginStartParams{}); reason(err) != "LOGIN_LIMIT" {
		t.Fatalf("a third waiting sign-in: %v", err)
	}
	if got, err := e.space.PollGrokBotLogin(ctx, protocol.LoginParams{LoginID: start.LoginID}); err != nil || got.Status != "pending" {
		t.Fatalf("%+v %v", got, err)
	}
	done, err := e.space.PollGrokBotLogin(ctx, protocol.LoginParams{LoginID: start.LoginID, DisplayName: "Grok"})
	if err != nil || done.Status != "done" || done.Account.DisplayName != "Grok" || done.Account.Provider != "grok_bot" {
		t.Fatalf("%+v %v", done, err)
	}
	if b, _ := json.Marshal(done); strings.Contains(string(b), "fixture-grok-token") || strings.Contains(string(b), "verifier") {
		t.Fatalf("the token came back: %s", b)
	}
	if c, _, _ := e.space.vault.Get(ctx, done.Account.AccountID); c != token {
		t.Fatal(c)
	}
	if _, err := e.space.PollGrokBotLogin(ctx, protocol.LoginParams{LoginID: start.LoginID}); reason(err) != "LOGIN_NOT_FOUND" {
		t.Fatal(err)
	}
	e.advance(11 * time.Minute)
	again, err := e.space.StartGrokBotLogin(ctx, protocol.GrokBotLoginStartParams{AccountID: done.Account.AccountID})
	if err != nil {
		t.Fatalf("expired sign-ins still count: %v", err)
	}
	e.p.poll = []string{`{"accessToken":"fixture-grok-token-0002","refreshToken":"r"}`}
	redone, err := e.space.PollGrokBotLogin(ctx, protocol.LoginParams{LoginID: again.LoginID})
	if err != nil || redone.Account.AccountID != done.Account.AccountID {
		t.Fatalf("signing in again pairs the same account: %+v %v", redone, err)
	}
	refused, _ := e.space.StartGrokBotLogin(ctx, protocol.GrokBotLoginStartParams{})
	if _, err := e.space.PollGrokBotLogin(ctx, protocol.LoginParams{LoginID: refused.LoginID}); reason(err) != "LOGIN_REFUSED" {
		t.Fatal(err)
	}
	if _, err := e.space.PollGrokBotLogin(ctx, protocol.LoginParams{LoginID: refused.LoginID}); reason(err) != "LOGIN_NOT_FOUND" {
		t.Fatalf("a refused sign-in is dropped: %v", err)
	}
	cue := newEnv(t, "grok_bot")
	cue.club.Registry().Register(newScripted("cue"))
	c, err := cue.space.AddAccount(ctx, protocol.AddAccountParams{Provider: "cue", Credential: "fixture-credential-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cue.space.StartGrokBotLogin(ctx, protocol.GrokBotLoginStartParams{AccountID: c.AccountID}); reason(err) != "PROVIDER_MISMATCH" {
		t.Fatal(err)
	}
}

func TestSettingsKeepTheBrowserIdentity(t *testing.T) {
	e := newEnv(t, "cue")
	ctx := context.Background()
	got, err := e.space.Settings(ctx)
	if err != nil || got.BrowserIdentity.ExtraArgs == nil || got.BrowserIdentity.Locale != "" {
		t.Fatalf("%+v %v", got, err)
	}
	set, err := e.space.SetSettings(ctx, protocol.SettingsParams{BrowserIdentity: &protocol.BrowserIdentity{UserAgent: " UA ",
		Locale: "zh-CN", Timezone: "Asia/Shanghai", ExtraArgs: []string{"--mute-audio", "--accept-lang=zh-CN,zh;q=0.9"}}})
	if err != nil || set.BrowserIdentity.UserAgent != "UA" || len(set.BrowserIdentity.ExtraArgs) != 2 {
		t.Fatalf("%+v %v", set, err)
	}
	for _, bad := range []protocol.BrowserIdentity{{Locale: "zh_CN"}, {Timezone: "Asia/上海"}, {ExtraArgs: []string{"--no-sandbox"}},
		{UserAgent: strings.Repeat("u", 513)}} {
		if _, err := e.space.SetSettings(ctx, protocol.SettingsParams{BrowserIdentity: &bad}); protocol.CodeOf(err) != "invalid" {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if got, _ := e.space.Settings(ctx); got.BrowserIdentity.Locale != "zh-CN" {
		t.Fatalf("%+v", got)
	}
}

func sessionsOf(n int, at int64) []provider.RemoteSession {
	var out []provider.RemoteSession
	for i := range n {
		out = append(out, provider.RemoteSession{ExternalID: fmt.Sprintf("s%02d", i), Title: "s", CanSend: true, LastActivityMs: at})
	}
	return out
}

func TestSyncReadsAtMostTwentySessionsAndRotatesQuietOnes(t *testing.T) {
	e := newEnv(t, "cue")
	ctx := context.Background()
	e.p.sessions = sessionsOf(25, e.nowMs()-60_000)
	a := e.pair(t, "fixture-credential-1")
	if n := e.p.listedCount(); n != 20 {
		t.Fatalf("the first sync read %d sessions", n)
	}
	e.p.listed = nil
	e.advance(2 * time.Hour)
	e.space.SyncAll(ctx)
	if n := e.p.listedCount(); n != 3 {
		t.Fatalf("a quiet sync read %d sessions, want the 3 read least recently", n)
	}
	seen := map[string]bool{}
	for range 9 {
		e.advance(time.Minute)
		e.space.SyncAll(ctx)
	}
	for _, id := range e.p.listed {
		seen[id] = true
	}
	if len(seen) != 25 {
		t.Fatalf("rotation reached %d of 25 sessions", len(seen))
	}
	e.p.listed = nil
	e.p.messages["s24"] = []provider.RemoteMessage{{ExternalID: "m", Role: "assistant", Blocks: []protocol.Block{protocol.TextBlock("hi")}, CreateTimeMs: e.nowMs()}}
	_, _ = e.space.store.UpsertMessages(ctx, SessionKey("cue", a.AccountID, "s24"), storeMessages("", e.p.messages["s24"], 0), e.nowMs())
	e.advance(time.Minute)
	e.space.SyncAll(ctx)
	found := false
	for _, id := range e.p.listed {
		found = found || id == "s24"
	}
	if !found {
		t.Fatalf("a session with a recent message is read: %v", e.p.listed)
	}
}

func TestAuthFailuresBackOffAndRefreshOnce(t *testing.T) {
	e := newEnv(t, "cue")
	ctx := context.Background()
	e.p.sessions = sessionsOf(1, 0)
	a := e.pair(t, "fixture-credential-1")
	calls := 0
	e.p.listErr = func(string) error {
		calls++
		return &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: 401}
	}
	e.space.SyncAll(ctx)
	for range 10 {
		e.advance(time.Minute)
		e.space.SyncAll(ctx)
	}
	got, _ := e.space.Account(ctx, a.AccountID)
	if calls != 1 || got.Status != protocol.AccountError || got.LastError != "AUTH_FAILED" {
		t.Fatalf("%d calls, %+v", calls, got)
	}
	e.advance(5 * time.Minute)
	e.space.SyncAll(ctx)
	if calls != 2 {
		t.Fatalf("%d calls after 15 minutes", calls)
	}
	if err := e.space.SyncAccount(ctx, a.AccountID); err != nil || calls != 3 {
		t.Fatalf("an explicit sync retries at once: %d %v", calls, err)
	}

	var used []string
	e.p.listErr = func(cred string) error {
		used = append(used, cred)
		if cred == "fixture-credential-1" {
			return &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: 401}
		}
		return nil
	}
	refreshes := 0
	e.p.refresh = func(string) (string, error) {
		refreshes++
		return "fixture-credential-2", nil
	}
	_ = e.space.SyncAccount(ctx, a.AccountID)
	got, _ = e.space.Account(ctx, a.AccountID)
	if refreshes != 1 || strings.Join(used, ",") != "fixture-credential-1,fixture-credential-2" || got.Status != protocol.AccountActive {
		t.Fatalf("%d %v %+v", refreshes, used, got)
	}
	if c, _, _ := e.space.vault.Get(ctx, a.AccountID); c != "fixture-credential-2" {
		t.Fatal(c)
	}
}

func TestATranscriptlessAccountStaysInErrorAfterARefusedSend(t *testing.T) {
	e := newEnv(t, "groq")
	e.p.history = true
	e.p.sessions = []provider.RemoteSession{{ExternalID: "chat", CanSend: true}}
	ctx := context.Background()
	a := e.pair(t, "fixture-credential-1")
	e.p.send = func(string, provider.Outgoing) (provider.SendResult, error) {
		return provider.SendResult{}, &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: 401}
	}
	m, _ := e.space.Send(ctx, protocol.SendParams{SessionKey: SessionKey("groq", a.AccountID, "chat"), ClientID: uuid(1), Text: "在吗"})
	if m.Local.Status != protocol.LocalFailed || m.Local.Error != "AUTH_FAILED" {
		t.Fatalf("%+v", m.Local)
	}
	e.advance(20 * time.Minute)
	e.space.SyncAll(ctx)
	if got, _ := e.space.Account(ctx, a.AccountID); got.Status != protocol.AccountError || got.LastError != "AUTH_FAILED" {
		t.Fatalf("%+v", got)
	}
}

func TestQuotaFromAnswersAnd429s(t *testing.T) {
	e := newEnv(t, "groq")
	e.p.history = true
	e.p.sessions = []provider.RemoteSession{{ExternalID: "chat", CanSend: true}}
	ctx := context.Background()
	a := e.pair(t, "fixture-credential-1")
	key := SessionKey("groq", a.AccountID, "chat")
	if got, _ := e.space.Account(ctx, a.AccountID); got.Quota != nil {
		t.Fatalf("nothing known yet: %+v", got.Quota)
	}
	reset := e.nowMs() + 60_000
	e.p.send = func(_ string, msg provider.Outgoing) (provider.SendResult, error) {
		return provider.SendResult{Status: provider.Sent, Quota: []provider.QuotaReading{
			{Kind: "requests", Limit: 1000, Remaining: 150, ResetsAt: reset, Window: "day"},
			{Kind: "tokens", Limit: 8000, Remaining: 7000, Window: "minute"}}}, nil
	}
	sub := e.club.Subscribe(DefaultSpace)
	defer sub.Close()
	_, _ = e.space.Send(ctx, protocol.SendParams{SessionKey: key, ClientID: uuid(1), Text: "x"})
	got, _ := e.space.Account(ctx, a.AccountID)
	q := got.Quota
	if q == nil || q.State != "low" || *q.Remaining != 0.15 || q.Kind != "requests" || q.Source != "provider" || q.Show ||
		q.ResetsAt != reset || len(q.Details) != 1 || q.Details[0].Kind != "tokens" {
		t.Fatalf("%+v", q)
	}
	listChanged := false
	for _, ev := range sub.Take() {
		listChanged = listChanged || ev.SessionKeys == nil
	}
	if !listChanged {
		t.Fatal("a new quota level is announced")
	}
	show := true
	_, _ = e.space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: a.AccountID, ShowQuota: &show})
	list, _ := e.space.Sessions(ctx)
	if list.Sessions[0].Quota == nil || !list.Sessions[0].Quota.Show {
		t.Fatalf("sessions carry their account's quota: %+v", list.Sessions[0])
	}
	e.p.send = func(string, provider.Outgoing) (provider.SendResult, error) {
		return provider.SendResult{}, &provider.Error{Code: provider.CodeRateLimited, HTTPStatus: 429, RetryAfter: 30 * time.Second}
	}
	_, _ = e.space.Send(ctx, protocol.SendParams{SessionKey: key, ClientID: uuid(2), Text: "x"})
	got, _ = e.space.Account(ctx, a.AccountID)
	if got.Quota == nil || got.Quota.State != "exhausted" || got.Quota.Source != "response" || got.Quota.Remaining != nil ||
		got.Quota.ResetsAt == 0 {
		t.Fatalf("%+v", got.Quota)
	}
	e.advance(31 * time.Second)
	if got, _ := e.space.Account(ctx, a.AccountID); got.Quota != nil {
		t.Fatalf("after the reset nothing is known: %+v", got.Quota)
	}
}

func TestWatchReadsTheSessionWhenTheProviderPushes(t *testing.T) {
	e := newEnv(t, "grok_bot", time.Hour)
	e.p.sessions = []provider.RemoteSession{{ExternalID: "bot/", CanSend: true}}
	release := make(chan struct{})
	finished := make(chan struct{})
	e.p.watch = func(event func(provider.WatchEvent) bool) error {
		defer close(finished)
		<-release
		event(provider.WatchChanged)
		if event(provider.WatchIdle) {
			t.Error("idle after a change ends the watch")
		}
		return nil
	}
	ctx := context.Background()
	a := e.pair(t, "fixture-credential-1")
	if _, err := e.space.Send(ctx, protocol.SendParams{SessionKey: SessionKey("grok_bot", a.AccountID, "bot/"), ClientID: uuid(1), Text: "继续"}); err != nil {
		t.Fatal(err)
	}
	before := e.p.listedCount()
	e.p.mu.Lock()
	e.p.messages["bot/"] = []provider.RemoteMessage{{ExternalID: "m3", Role: "assistant", Blocks: []protocol.Block{protocol.TextBlock("已完成")}, CreateTimeMs: e.nowMs()}}
	e.p.mu.Unlock()
	close(release)
	<-finished
	if e.p.listedCount() != before+1 {
		t.Fatalf("read %d times", e.p.listedCount()-before)
	}
	page, _ := e.space.Messages(ctx, protocol.MessagesParams{SessionKey: SessionKey("grok_bot", a.AccountID, "bot/")})
	if len(page.Messages) == 0 || page.Messages[0].ExternalID != "m3" {
		t.Fatalf("%+v", page.Messages)
	}
}

func writeBundle(t *testing.T, b map[string]any, mode os.FileMode) string {
	t.Helper()
	raw, _ := json.Marshal(b)
	path := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(path, raw, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	grokAcct = "a00000000000a"
	museAcct = "a00000000000b"
)

func testBundle() map[string]any {
	grokMain := "agent:grok_bot:" + grokAcct + ":bot/"
	museMain := "agent:muse:" + museAcct + ":c1"
	return map[string]any{
		"format": BundleFormat, "version": 1,
		"accounts": []any{
			map[string]any{"account_id": grokAcct, "provider": "grok_bot", "display_name": "Grok Bot", "status": "active",
				"external_account_id": "remote-1", "last_synced_at": "1761900000000", "last_error": nil, "created_at": 1, "updated_at": "2"},
			map[string]any{"account_id": museAcct, "provider": "muse", "display_name": "Muse", "status": "active",
				"external_account_id": "p0123", "created_at": 3, "updated_at": 4},
		},
		"sessions": []any{
			map[string]any{"session_key": grokMain, "account_id": grokAcct, "provider": "grok_bot", "external_id": "bot/",
				"title": "Grok 秘书", "kind": "main", "parent_session_key": nil, "can_send": true,
				"ref_json":     map[string]any{"botId": "bot", "sessionId": "", "avatarUrl": "https://example.com/a.png"},
				"rank_time_ms": "1761900000000", "on_top_rank_time_ms": 0, "unread_count": 2, "read_time_ms": 1761800000000,
				"is_hidden": false, "preview_text": "明早 9 点提醒你开会。", "preview_is_mine": false, "updated_at": 5},
			map[string]any{"session_key": museMain, "account_id": museAcct, "provider": "muse", "external_id": "c1",
				"title": "Muse", "kind": "main", "can_send": true, "ref_json": map[string]any{}, "rank_time_ms": 9, "updated_at": 9},
		},
		"messages": []any{
			map[string]any{"session_key": grokMain, "external_id": "e-user", "position": "10", "role": "user",
				"blocks_json": []any{map[string]any{"type": "text", "text": "明天早上提醒我开会"}}, "create_time_ms": "1761801990000"},
			map[string]any{"session_key": grokMain, "external_id": "e-reply", "position": 12, "role": "assistant",
				"blocks_json":    []any{map[string]any{"type": "text", "text": "明早 9 点提醒你开会。"}, map[string]any{"type": "reaction", "emoji": "👍", "from": "user"}},
				"create_time_ms": 1761900000000},
			map[string]any{"session_key": museMain, "external_id": "x", "position": 0, "role": "user", "blocks_json": []any{}, "create_time_ms": 1},
		},
		"outbox": []any{
			map[string]any{"client_id": uuid(1), "session_key": grokMain, "account_id": grokAcct, "text": "在路上", "status": "claimed",
				"attempt_count": 1, "last_error": nil, "created_at": 10, "updated_at": 11},
			map[string]any{"client_id": uuid(2), "session_key": grokMain, "account_id": grokAcct, "text": "还没发", "status": "pending",
				"attempt_count": 0, "created_at": 12, "updated_at": 12},
			map[string]any{"client_id": uuid(3), "session_key": grokMain, "account_id": grokAcct, "text": "早发了", "status": "sent",
				"attempt_count": 1, "created_at": 13, "updated_at": 14},
		},
		"credentials":     map[string]any{grokAcct: `{"accessToken":"fixture-grok-token-0001","refreshToken":"fixture-refresh"}`, museAcct: "p0123456789"},
		"browserIdentity": map[string]any{"userAgent": "", "locale": "zh-CN", "timezone": "", "extraArgs": []any{}},
	}
}

func TestImportKeepsIDsSkipsBrowserAccountsAndSendsNothing(t *testing.T) {
	e := newEnv(t, "grok_bot")
	ctx := context.Background()
	path := writeBundle(t, testBundle(), 0o600)

	dry, err := e.space.Import(ctx, path, ImportOptions{DryRun: true})
	if err != nil || !dry.DryRun || len(dry.Accounts) != 1 || dry.Accounts[0].Messages != 2 || !dry.Accounts[0].HasCredential ||
		dry.Accounts[0].Verified || len(dry.Skipped) != 1 || dry.Skipped[0].Reason != SkipUnsupported {
		t.Fatalf("%+v %v", dry, err)
	}
	if list, _ := e.space.store.ListAccounts(ctx); len(list) != 0 {
		t.Fatal("a dry run wrote")
	}

	res, err := e.space.Import(ctx, path, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.ImportedAccount{AccountID: grokAcct, Provider: "grok_bot", DisplayName: "Grok Bot", Verified: true, Sessions: 1, Messages: 2,
		LatestMessageMs: 1761900000000, HasCredential: true}
	if len(res.Accounts) != 1 || res.Accounts[0] != want || len(res.Skipped) != 1 || res.Skipped[0].AccountID != museAcct {
		t.Fatalf("%+v", res)
	}
	again, err := e.space.Import(ctx, path, ImportOptions{})
	if err != nil || again.Accounts[0] != want {
		t.Fatalf("a second import is the same: %+v %v", again, err)
	}
	a, _ := e.space.Account(ctx, grokAcct)
	if a.AssistantControl != "off" || a.LastSyncedAt != 1761900000000 || a.Status != "active" {
		t.Fatalf("%+v", a)
	}
	if _, err := e.space.Account(ctx, museAcct); reason(err) != "AGENT_ACCOUNT_NOT_FOUND" {
		t.Fatal("the browser account was imported")
	}
	if _, ok, _ := e.space.vault.Get(ctx, museAcct); ok {
		t.Fatal("the skipped account's credential was stored")
	}
	key := "agent:grok_bot:" + grokAcct + ":bot/"
	list, _ := e.space.Sessions(ctx)
	if len(list.Sessions) != 1 || list.Sessions[0].SessionKey != key || list.Sessions[0].UnreadCount != 2 ||
		list.Sessions[0].AvatarURL != "https://example.com/a.png" {
		t.Fatalf("%+v", list.Sessions)
	}
	page, _ := e.space.Messages(ctx, protocol.MessagesParams{SessionKey: key})
	if page.Messages[0].ExternalID != "e-user" || page.Messages[0].Position != 10 || page.Messages[1].Position != 12 {
		t.Fatalf("%+v", page.Messages)
	}
	if b, _ := json.Marshal(page.Messages[1].Blocks[1]); string(b) != `{"emoji":"👍","from":"user","type":"reaction"}` {
		t.Fatalf("an unknown block is kept: %s", b)
	}
	for id, want := range map[string]string{uuid(1): "unconfirmed/SEND_UNCONFIRMED", uuid(2): "failed/IMPORTED_NOT_SENT", uuid(3): "sent/"} {
		o, _, _ := e.space.store.Outbox(ctx, id)
		if o.Status+"/"+o.LastError != want {
			t.Errorf("%s: %+v", id, o)
		}
	}
	if err := e.space.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.p.sends) != 0 {
		t.Fatalf("an import led to sends: %v", e.p.sends)
	}
	if s, _ := e.space.Settings(ctx); s.BrowserIdentity.Locale != "zh-CN" {
		t.Fatalf("%+v", s)
	}
	if hits, _ := e.space.Search(ctx, protocol.SearchParams{Q: "提醒"}); len(hits.Hits) != 2 {
		t.Fatalf("%+v", hits)
	}

	only, err := newEnv(t, "grok_bot").space.Import(ctx, path, ImportOptions{OnlyProviders: []string{"cue"}})
	if err != nil || len(only.Accounts) != 0 || len(only.Skipped) != 2 || only.Skipped[0].Reason != SkipNotSelected {
		t.Fatalf("%+v %v", only, err)
	}
}

func TestImportRefusals(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "grok_bot")
	if _, err := e.space.Import(ctx, writeBundle(t, testBundle(), 0o644), ImportOptions{}); protocol.CodeOf(err) != "invalid" ||
		!strings.Contains(err.Error(), "0600") {
		t.Fatalf("a bundle readable by others: %v", err)
	}
	for name, change := range map[string]func(b map[string]any){
		"version": func(b map[string]any) { b["version"] = 2 },
		"bad id":  func(b map[string]any) { b["accounts"].([]any)[0].(map[string]any)["account_id"] = "x" },
		"key":     func(b map[string]any) { b["sessions"].([]any)[0].(map[string]any)["external_id"] = "other" },
		"orphan": func(b map[string]any) {
			b["messages"].([]any)[0].(map[string]any)["session_key"] = "agent:grok_bot:a0000000000ff:x"
		},
		"credential":  func(b map[string]any) { b["credentials"].(map[string]any)["a0000000000ff"] = "fixture-credential" },
		"send status": func(b map[string]any) { b["outbox"].([]any)[0].(map[string]any)["status"] = "queued" },
		"identity":    func(b map[string]any) { b["browserIdentity"] = map[string]any{"extraArgs": []any{"--no-sandbox"}} },
	} {
		b := testBundle()
		change(b)
		if _, err := e.space.Import(ctx, writeBundle(t, b, 0o600), ImportOptions{}); protocol.CodeOf(err) != "invalid" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if list, _ := e.space.store.ListAccounts(ctx); len(list) != 0 {
		t.Fatal("a refused import wrote")
	}
	e.p.sessions = nil
	e.pair(t, "fixture-credential-9")
	if _, err := e.space.Import(ctx, writeBundle(t, testBundle(), 0o600), ImportOptions{}); reason(err) != "ACCOUNT_ALREADY_PAIRED" {
		t.Fatalf("the same remote account paired twice: %v", err)
	}
	f := newEnv(t, "grok_bot")
	f.p.verify = func(string) error { return &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: 401} }
	res, err := f.space.Import(ctx, writeBundle(t, testBundle(), 0o600), ImportOptions{})
	if err != nil || res.Accounts[0].Verified || res.Accounts[0].Error != "AUTH_FAILED" {
		t.Fatalf("%+v %v", res, err)
	}
}

// Every provider the registry has no adapter for, known (the browser ones) or not, is skipped with all its rows:
// sessions, messages, sends and the credential.
func TestImportSkipsEveryProviderThisBuildCannotReach(t *testing.T) {
	ctx := context.Background()
	b := testBundle()
	for i, p := range []string{"dots", "claude_duet", "someday"} {
		id := fmt.Sprintf("a00000000001%d", i)
		key := "agent:" + p + ":" + id + ":s"
		b["accounts"] = append(b["accounts"].([]any), map[string]any{"account_id": id, "provider": p, "status": "error",
			"display_name": p})
		b["sessions"] = append(b["sessions"].([]any), map[string]any{"session_key": key, "account_id": id, "provider": p,
			"external_id": "s", "title": p, "kind": "main", "can_send": true})
		b["messages"] = append(b["messages"].([]any), map[string]any{"session_key": key, "external_id": "m", "role": "user",
			"blocks_json": []any{map[string]any{"type": "text", "text": "跳过的消息"}}, "create_time_ms": 5})
		b["outbox"] = append(b["outbox"].([]any), map[string]any{"client_id": uuid(10 + i), "session_key": key,
			"account_id": id, "text": "跳过的发送", "status": "pending", "created_at": 5, "updated_at": 5})
		b["credentials"].(map[string]any)[id] = "fixture-skipped-credential"
	}
	e := newEnv(t, "grok_bot")
	path := writeBundle(t, b, 0o600)
	plan, err := PlanImport(path, e.club.Registry(), nil, ImportOptions{})
	if err != nil || len(plan.Accounts) != 1 || len(plan.Skipped) != 4 {
		t.Fatalf("%+v %v", plan, err)
	}
	res, err := e.space.Import(ctx, path, ImportOptions{})
	if err != nil || len(res.Accounts) != 1 || len(res.Skipped) != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	for _, s := range res.Skipped {
		if s.Reason != SkipUnsupported || s.DisplayName == "" {
			t.Errorf("%+v", s)
		}
		if _, ok, _ := e.space.vault.Get(ctx, s.AccountID); ok {
			t.Errorf("the credential of skipped %s was stored", s.AccountID)
		}
		if c, _ := e.space.store.Counts(ctx, s.AccountID); c.Sessions != 0 || c.Messages != 0 {
			t.Errorf("rows of skipped %s were stored: %+v", s.AccountID, c)
		}
	}
	for i := range 3 {
		if _, ok, _ := e.space.store.Outbox(ctx, uuid(10+i)); ok {
			t.Errorf("a send of a skipped account was stored")
		}
	}
	if hits, _ := e.space.Search(ctx, protocol.SearchParams{Q: "跳过"}); len(hits.Hits) != 0 {
		t.Fatalf("%+v", hits)
	}
}

// listEvents is whether sub got a "list changed" event since the last call.
func listEvents(sub *Subscription) bool {
	changed := false
	for _, ev := range sub.Take() {
		changed = changed || ev.SessionKeys == nil
	}
	return changed
}

func TestQuotaStatesStalenessAndAnnouncements(t *testing.T) {
	e := newEnv(t, "groq")
	e.p.history = true
	e.p.sessions = []provider.RemoteSession{{ExternalID: "chat", CanSend: true}}
	ctx := context.Background()
	a := e.pair(t, "fixture-credential-1")
	key := SessionKey("groq", a.AccountID, "chat")
	sub := e.club.Subscribe(DefaultSpace)
	defer sub.Close()
	n := 0
	sendWith := func(left float64) *protocol.Quota {
		t.Helper()
		e.p.send = func(string, provider.Outgoing) (provider.SendResult, error) {
			return provider.SendResult{Status: provider.Sent, Quota: []provider.QuotaReading{{Kind: "requests", Limit: 100, Remaining: left}}}, nil
		}
		n++
		if _, err := e.space.Send(ctx, protocol.SendParams{SessionKey: key, ClientID: uuid(n), Text: "x"}); err != nil {
			t.Fatal(err)
		}
		got, _ := e.space.Account(ctx, a.AccountID)
		return got.Quota
	}
	for _, tc := range []struct {
		left     float64
		state    string
		announce bool
	}{{21, "ok", true}, {50, "ok", false}, {20, "low", true}, {1, "low", false}, {0, "exhausted", true}, {90, "ok", true}} {
		q := sendWith(tc.left)
		if q == nil || q.State != tc.state || q.Stale || q.Source != "provider" {
			t.Fatalf("%v left: %+v", tc.left, q)
		}
		if got := listEvents(sub); got != tc.announce {
			t.Fatalf("%v left: announced %v", tc.left, got)
		}
	}
	// 15 minutes without an answer: stale, announced once by time alone
	e.advance(15*time.Minute + time.Millisecond)
	e.space.mu.Lock()
	q := e.space.quota[a.AccountID]
	e.space.mu.Unlock()
	e.space.recheckQuota(a.AccountID, q)
	if got, _ := e.space.Account(ctx, a.AccountID); got.Quota == nil || !got.Quota.Stale || got.Quota.State != "ok" {
		t.Fatalf("%+v", got.Quota)
	}
	if !listEvents(sub) {
		t.Fatal("going stale was not announced")
	}
	e.space.recheckQuota(a.AccountID, q)
	if listEvents(sub) {
		t.Fatal("announced twice")
	}
	// a 429 without Retry-After: exhausted, then unknown (no quota at all) after five minutes
	e.p.send = func(string, provider.Outgoing) (provider.SendResult, error) {
		return provider.SendResult{}, &provider.Error{Code: provider.CodeRateLimited, HTTPStatus: 429}
	}
	n++
	if _, err := e.space.Send(ctx, protocol.SendParams{SessionKey: key, ClientID: uuid(n), Text: "x"}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.space.Account(ctx, a.AccountID)
	if got.Quota == nil || got.Quota.State != "exhausted" || got.Quota.Source != "response" || got.Quota.ResetsAt != 0 || got.Quota.Stale {
		t.Fatalf("%+v", got.Quota)
	}
	if !listEvents(sub) {
		t.Fatal("exhausted was not announced")
	}
	e.space.mu.Lock()
	q = e.space.quota[a.AccountID]
	e.space.mu.Unlock()
	e.advance(4 * time.Minute)
	if got, _ := e.space.Account(ctx, a.AccountID); got.Quota == nil || got.Quota.State != "exhausted" {
		t.Fatalf("before five minutes: %+v", got.Quota)
	}
	e.advance(time.Minute)
	e.space.recheckQuota(a.AccountID, q)
	if got, _ := e.space.Account(ctx, a.AccountID); got.Quota != nil {
		t.Fatalf("after five minutes nothing is known: %+v", got.Quota)
	}
	if !listEvents(sub) {
		t.Fatal("the end of the wait was not announced")
	}
	list, _ := e.space.Sessions(ctx)
	if list.Sessions[0].Quota != nil {
		t.Fatalf("%+v", list.Sessions[0].Quota)
	}
}

// confirming is a scripted provider that can also confirm sends.
type confirming struct {
	*scripted
	confirm func() (provider.Confirmation, error)
}

func (c confirming) ConfirmSend(context.Context, string, provider.SessionRef, string) (provider.Confirmation, error) {
	return c.confirm()
}

// A 429 on any call about an account, whichever path made it, makes the account exhausted.
func TestEveryCallPathNotesA429(t *testing.T) {
	limited := &provider.Error{Code: provider.CodeRateLimited, HTTPStatus: 429, RetryAfter: time.Minute}
	ctx := context.Background()
	exhausted := func(t *testing.T, s *Space, id string) {
		t.Helper()
		got, err := s.Account(ctx, id)
		if err != nil || got.Quota == nil || got.Quota.State != "exhausted" || got.Quota.ResetsAt == 0 {
			t.Fatalf("%+v %v", got.Quota, err)
		}
		s.mu.Lock()
		delete(s.quota, id)
		s.mu.Unlock()
	}
	p := newScripted("grok_bot")
	p.sessions = []provider.RemoteSession{{ExternalID: "bot/", CanSend: true}}
	c := confirming{scripted: p, confirm: func() (provider.Confirmation, error) { return provider.Confirmation{}, limited }}
	clk := &clock{t: time.UnixMilli(1_800_000_000_000)}
	club := New(Options{Registry: provider.NewRegistry(c), Now: clk.Now, FollowUps: []time.Duration{}, WatchLimit: -1})
	defer club.Close()
	s, _ := club.Space(ctx, DefaultSpace)
	a, err := s.AddAccount(ctx, protocol.AddAccountParams{Provider: "grok_bot", Credential: "fixture-credential-1"})
	if err != nil {
		t.Fatal(err)
	}
	key := SessionKey("grok_bot", a.AccountID, "bot/")

	t.Run("sync", func(t *testing.T) {
		p.mu.Lock()
		p.listErr = func(string) error { return limited }
		p.mu.Unlock()
		_ = s.SyncAccount(ctx, a.AccountID)
		exhausted(t, s, a.AccountID)
		p.mu.Lock()
		p.listErr = nil
		p.mu.Unlock()
	})
	t.Run("send", func(t *testing.T) {
		p.mu.Lock()
		p.send = func(string, provider.Outgoing) (provider.SendResult, error) { return provider.SendResult{}, limited }
		p.mu.Unlock()
		m, _ := s.Send(ctx, protocol.SendParams{SessionKey: key, ClientID: uuid(1), Text: "x"})
		if m.Local == nil || m.Local.Status != "failed" || m.Local.Error != "RATE_LIMITED" {
			t.Fatalf("%+v", m.Local)
		}
		exhausted(t, s, a.AccountID)
	})
	t.Run("confirm", func(t *testing.T) {
		p.mu.Lock()
		p.send = func(string, provider.Outgoing) (provider.SendResult, error) {
			return provider.SendResult{Status: provider.Unconfirmed}, nil
		}
		p.mu.Unlock()
		_, _ = s.Send(ctx, protocol.SendParams{SessionKey: key, ClientID: uuid(2), Text: "x"})
		sess, _, _ := s.store.Session(ctx, key)
		s.confirm(ctx, uuid(2), sess)
		exhausted(t, s, a.AccountID)
	})
	t.Run("live", func(t *testing.T) {
		p.mu.Lock()
		p.live = func() (provider.LiveState, error) { return provider.LiveState{}, limited }
		p.mu.Unlock()
		if _, err := s.Live(ctx, protocol.SessionParams{SessionKey: key}); err == nil {
			t.Fatal("no error")
		}
		exhausted(t, s, a.AccountID)
	})
	t.Run("replace credential", func(t *testing.T) {
		p.mu.Lock()
		p.verify = func(string) error { return limited }
		p.mu.Unlock()
		_, _ = s.ReplaceCredential(ctx, protocol.ReplaceCredentialParams{AccountID: a.AccountID, Credential: "fixture-credential-2"})
		exhausted(t, s, a.AccountID)
	})
	t.Run("refresh", func(t *testing.T) {
		p.mu.Lock()
		p.verify = nil
		p.listErr = func(string) error { return &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: 401} }
		p.mu.Unlock()
		p.refresh = func(string) (string, error) { return "", limited }
		_ = s.SyncAccount(ctx, a.AccountID)
		exhausted(t, s, a.AccountID)
		p.refresh = nil
		p.mu.Lock()
		p.listErr = nil
		p.mu.Unlock()
	})
	t.Run("import verify", func(t *testing.T) {
		e := newEnv(t, "grok_bot")
		e.p.verify = func(string) error { return limited }
		res, err := e.space.Import(ctx, writeBundle(t, testBundle(), 0o600), ImportOptions{})
		if err != nil || res.Accounts[0].Error != "RATE_LIMITED" {
			t.Fatalf("%+v %v", res, err)
		}
		exhausted(t, e.space, grokAcct)
	})
}

// Readings that come with a verification count: pairing a Groq key shows its quota at once.
func TestVerifyReadingsAreKept(t *testing.T) {
	e := newEnv(t, "groq")
	e.p.history = true
	e.p.sessions = []provider.RemoteSession{{ExternalID: "chat", CanSend: true}}
	v := &verifyingQuota{scripted: e.p}
	e.club.Registry().Register(v)
	a, err := e.space.AddAccount(context.Background(), protocol.AddAccountParams{Provider: "groq", Credential: "fixture-credential-1"})
	if err != nil || a.Quota == nil || a.Quota.State != "low" || a.Quota.Kind != "tokens" {
		t.Fatalf("%+v %v", a.Quota, err)
	}
}

type verifyingQuota struct{ *scripted }

func (v *verifyingQuota) Verify(ctx context.Context, cred string) (provider.RemoteAccount, error) {
	r, err := v.scripted.Verify(ctx, cred)
	r.Quota = []provider.QuotaReading{{Kind: "tokens", Limit: 100, Remaining: 10}}
	return r, err
}

func TestReplaceCredentialPreservesPauseDuringVerification(t *testing.T) {
	e := newEnv(t, "cue")
	ctx := context.Background()
	account := e.pair(t, "fixture-original")
	entered, release := make(chan struct{}), make(chan struct{})
	e.p.mu.Lock()
	e.p.verify = func(string) error {
		close(entered)
		<-release
		return nil
	}
	e.p.mu.Unlock()

	type result struct {
		account protocol.Account
		err     error
	}
	done := make(chan result, 1)
	go func() {
		updated, err := e.space.ReplaceCredential(ctx, protocol.ReplaceCredentialParams{
			AccountID: account.AccountID, Credential: "fixture-replacement",
		})
		done <- result{updated, err}
	}()

	select {
	case <-entered:
	case early := <-done:
		t.Fatalf("replacement returned before verification: %v", early.err)
	}
	paused := true
	updated, err := e.space.UpdateAccount(ctx, protocol.UpdateAccountParams{
		AccountID: account.AccountID, Paused: &paused,
	})
	close(release)
	got := <-done
	if err != nil || updated.Status != protocol.AccountPaused {
		t.Fatalf("explicit pause: status = %q, err = %v", updated.Status, err)
	}
	if got.err != nil || got.account.Status != protocol.AccountPaused {
		t.Fatalf("after replacement: status = %q, err = %v", got.account.Status, got.err)
	}
	stored, err := e.space.Account(ctx, account.AccountID)
	if err != nil || stored.Status != protocol.AccountPaused {
		t.Fatalf("stored account: status = %q, err = %v", stored.Status, err)
	}
	cred, found, err := e.space.vault.Get(ctx, account.AccountID)
	if err != nil || !found || cred != "fixture-replacement" {
		t.Fatalf("replacement credential was not stored: found = %v, err = %v", found, err)
	}
}

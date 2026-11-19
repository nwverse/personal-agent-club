package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/cue"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
	"github.com/nwverse/personal-agent-club/internal/provider/grokbot"
	"github.com/nwverse/personal-agent-club/internal/provider/groq"
	"github.com/nwverse/personal-agent-club/internal/provider/providertest"
)

// Synthetic credentials and the remote accounts the fake upstream says they belong to.
const (
	groqKey   = "fixture-groq-key-0001"
	cueKey    = "fixture-cue-key-0001"
	grokToken = "fixture-grok-token-0001"
	grokExt   = "auth0|fixture" // GetMe in grok-bot-0.78.1.json

	grokAcct = "a0000000000a1"
	groqAcct = "a0000000000a2"
	cueAcct  = "a0000000000a3"
	museAcct = "a0000000000a4"
)

// upstream is one fake host for all three API providers, answering from the recorded fixtures and checking each
// provider's credential. A test may answer a call itself through override.
type upstream struct {
	*providertest.Upstream
	mu       sync.Mutex
	override func(providertest.Call) (providertest.Answer, bool)
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	fx := map[string]map[string]json.RawMessage{
		"groq": providertest.Fixture(t, "groq-openai-v1.json"),
		"grok": providertest.Fixture(t, "grok-bot-0.78.1.json"),
		"cue":  providertest.Fixture(t, "manus-api-v2.json"),
	}
	u := &upstream{}
	u.Upstream = providertest.Serve(t, func(c providertest.Call) providertest.Answer {
		u.mu.Lock()
		f := u.override
		u.mu.Unlock()
		if f != nil {
			if a, ok := f(c); ok {
				return a
			}
		}
		switch {
		case strings.HasPrefix(c.Path, "/openai/v1/"):
			if c.Header.Get("Authorization") != "Bearer "+groqKey {
				return providertest.Answer{Status: 401, Body: fx["groq"]["error.invalid_api_key"]}
			}
			switch c.Path {
			case "/openai/v1/models":
				return providertest.Answer{Body: fx["groq"]["models"]}
			case "/openai/v1/chat/completions":
				return providertest.Answer{Body: fx["groq"]["chat.completion"], Header: groqLimits("14400", "14399", "8000", "1200")}
			}
		case strings.HasPrefix(c.Path, "/aiserver.v1."):
			if c.Header.Get("Authorization") != "Bearer "+grokToken {
				return providertest.Answer{Status: 401}
			}
			name := c.Path[strings.LastIndex(c.Path, "/")+1:]
			if body, ok := fx["grok"][name]; ok {
				return providertest.Answer{Body: body}
			}
		case strings.HasPrefix(c.Path, "/v2/"):
			if c.Header.Get("x-manus-api-key") != cueKey {
				return providertest.Answer{Status: 401, Body: fx["cue"]["error.unauthorized"]}
			}
			name := strings.TrimPrefix(c.Path, "/v2/")
			if ids := c.Query["task_id"]; len(ids) == 1 {
				name += ":" + ids[0]
			}
			if body, ok := fx["cue"][name]; ok {
				return providertest.Answer{Body: body}
			}
		}
		return providertest.Answer{Status: 404}
	})
	return u
}

func (u *upstream) answer(f func(providertest.Call) (providertest.Answer, bool)) {
	u.mu.Lock()
	u.override = f
	u.mu.Unlock()
}

// calls are the calls to a provider's API. Others are left out: another program on the machine may probe a test
// server's port.
func (u *upstream) calls() []providertest.Call {
	var out []providertest.Call
	for _, c := range u.Calls() {
		for _, p := range []string{"/openai/v1/", "/aiserver.v1.", "/v2/", "/auth/", "/oauth/"} {
			if strings.HasPrefix(c.Path, p) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// count is how many calls went to path.
func (u *upstream) count(path string) int {
	n := 0
	for _, c := range u.calls() {
		if c.Path == path {
			n++
		}
	}
	return n
}

func groqLimits(limitReq, leftReq, limitTok, leftTok string) http.Header {
	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", limitReq)
	h.Set("x-ratelimit-remaining-requests", leftReq)
	h.Set("x-ratelimit-reset-requests", "6s")
	h.Set("x-ratelimit-limit-tokens", limitTok)
	h.Set("x-ratelimit-remaining-tokens", leftTok)
	h.Set("x-ratelimit-reset-tokens", "30s")
	return h
}

// registry is the API providers pointed at u.
func (u *upstream) registry(demoEcho bool) (*provider.Registry, error) {
	r := provider.NewRegistry(
		cue.New(cue.Options{Origin: u.URL}),
		grokbot.New(grokbot.Options{Origin: u.URL, LoginOrigin: u.URL}),
		groq.New(groq.Options{Origin: u.URL + "/openai/v1"}),
	)
	if demoEcho {
		r.Register(echo.New())
	}
	return r, nil
}

// testDeps are the commands' deps with the fake upstream's providers and captured output.
func testDeps(u *upstream) (deps, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return deps{stdout: &out, stderr: &errOut, ctx: context.Background(), registry: u.registry}, &out, &errOut
}

// shortDir is a directory whose socket path stays under the unix socket limit.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pac")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// testBundle is a gateway export with one account per API provider and one browser (muse) account.
func testBundle() map[string]any {
	grokSess := "agent:grok_bot:" + grokAcct + ":bot_alpha/"
	groqSess := "agent:groq:" + groqAcct + ":chat"
	cueSess := "agent:cue:" + cueAcct + ":task-main-1"
	museSess := "agent:muse:" + museAcct + ":c1"
	text := func(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }
	return map[string]any{
		"format": "personal-agent-club/import", "version": 1,
		"accounts": []any{
			map[string]any{"account_id": grokAcct, "provider": "grok_bot", "display_name": "Grok Bot", "status": "active",
				"external_account_id": grokExt, "last_synced_at": "1761900000000", "created_at": 1, "updated_at": 2},
			map[string]any{"account_id": groqAcct, "provider": "groq", "display_name": "Groq", "status": "active",
				"external_account_id": "groq", "last_synced_at": 1761900000000, "created_at": 1, "updated_at": 2},
			map[string]any{"account_id": cueAcct, "provider": "cue", "display_name": "Cue · 小助手", "status": "error",
				"external_account_id": "manus", "last_error": "AUTH_FAILED", "created_at": 1, "updated_at": 2},
			map[string]any{"account_id": museAcct, "provider": "muse", "display_name": "Muse", "status": "active",
				"external_account_id": "p0123", "created_at": 3, "updated_at": 4},
		},
		"sessions": []any{
			map[string]any{"session_key": grokSess, "account_id": grokAcct, "provider": "grok_bot", "external_id": "bot_alpha/",
				"title": "Grok 秘书", "kind": "main", "can_send": true, "ref_json": map[string]any{"botId": "bot_alpha", "sessionId": ""},
				"rank_time_ms": "1761900000000", "unread_count": 1, "read_time_ms": 1761800000000, "updated_at": 5},
			map[string]any{"session_key": groqSess, "account_id": groqAcct, "provider": "groq", "external_id": "chat",
				"title": "Groq", "kind": "main", "can_send": true, "ref_json": map[string]any{"model": "openai/gpt-oss-120b"},
				"rank_time_ms": 1761900000000, "updated_at": 5},
			map[string]any{"session_key": cueSess, "account_id": cueAcct, "provider": "cue", "external_id": "task-main-1",
				"title": "小助手", "kind": "main", "can_send": true, "ref_json": map[string]any{"agentId": "agent-default", "taskId": "task-main-1"},
				"rank_time_ms": 1761800000000, "updated_at": 5},
			map[string]any{"session_key": museSess, "account_id": museAcct, "provider": "muse", "external_id": "c1",
				"title": "Muse", "kind": "main", "can_send": true, "rank_time_ms": 9, "updated_at": 9},
		},
		"messages": []any{
			map[string]any{"session_key": grokSess, "external_id": "e1", "position": 1, "role": "user",
				"blocks_json": text("明天早上提醒我开会"), "create_time_ms": 1761800000000},
			map[string]any{"session_key": grokSess, "external_id": "e2", "position": 2, "role": "assistant",
				"blocks_json": text("明早 9 点提醒你开会。"), "create_time_ms": 1761900000000},
			map[string]any{"session_key": groqSess, "external_id": "u:1", "position": 0, "role": "user",
				"blocks_json": text("hello"), "create_time_ms": 1761800000000},
			map[string]any{"session_key": museSess, "external_id": "x", "role": "user", "blocks_json": text("muse"), "create_time_ms": 1},
		},
		"outbox": []any{
			map[string]any{"client_id": "00000000-0000-4000-8000-000000000001", "session_key": groqSess, "account_id": groqAcct,
				"text": "在路上", "status": "claimed", "attempt_count": 1, "created_at": 10, "updated_at": 11},
			map[string]any{"client_id": "00000000-0000-4000-8000-000000000002", "session_key": groqSess, "account_id": groqAcct,
				"text": "还没发", "status": "pending", "created_at": 12, "updated_at": 12},
			map[string]any{"client_id": "00000000-0000-4000-8000-000000000003", "session_key": museSess, "account_id": museAcct,
				"text": "muse", "status": "pending", "created_at": 12, "updated_at": 12},
		},
		"credentials": map[string]any{
			grokAcct: `{"accessToken":"` + grokToken + `","refreshToken":"fixture-grok-refresh-0001"}`,
			groqAcct: groqKey, cueAcct: cueKey, museAcct: "fixture-muse-cookie-0001",
		},
	}
}

func writeBundle(t *testing.T, b map[string]any, mode os.FileMode) string {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(path, raw, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// secrets are every credential of the fixtures: none may ever be printed.
var secrets = []string{groqKey, cueKey, grokToken, "fixture-grok-refresh-0001", "fixture-muse-cookie-0001"}

func noSecrets(t *testing.T, outputs ...string) {
	t.Helper()
	for _, o := range outputs {
		for _, s := range secrets {
			if strings.Contains(o, s) {
				t.Fatalf("a credential was printed: %q", o)
			}
		}
	}
}

// serveHTTP is an HTTP client for a serve's unix socket.
func serveHTTP(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
}

// startServe runs the serve command on data until the test (or stop) ends it; it answers once serve listens.
func startServe(t *testing.T, u *upstream, args ...string) (stop func() (int, string)) {
	t.Helper()
	d, _, errOut := testDeps(u)
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	ready := make(chan struct{})
	d.listening = func([]net.Addr) { close(ready) }
	code := make(chan int, 1)
	var mu sync.Mutex
	d.stderr = lockedWriter{&mu, errOut}
	go func() { code <- runWith(append([]string{"serve"}, args...), d) }()
	select {
	case <-ready:
	case c := <-code:
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("serve ended at once (%d): %s", c, errOut.String())
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	stopped := false
	var result int
	stop = func() (int, string) {
		if !stopped {
			stopped = true
			cancel()
			result = <-code
		}
		mu.Lock()
		defer mu.Unlock()
		return result, errOut.String()
	}
	t.Cleanup(func() { stop() })
	return stop
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

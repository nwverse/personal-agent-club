package extension

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider/grokbot"
)

// takeoverState reads the takeover id the page kept after an action.
func takeoverState(t *testing.T, raw string) pageState {
	t.Helper()
	var st pageState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("state %q: %v", raw, err)
	}
	return st
}

// The grok box is offline, so this drives the takeover with the lines the box's agent would answer and checks the
// accounts page: starting, progress, and that the machine list is written only by the confirm action, never by
// progress, and never when the node is already there.
func TestTakeoverOnTheAccountsPage(t *testing.T) {
	h, club := start(t)
	club.Registry().Register(grokNamed{})
	dir := t.TempDir()
	club.SetPersonaDir(dir)
	list := filepath.Join(dir, "managers", "default", "manager.json")
	if err := os.MkdirAll(filepath.Dir(list), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := []byte("{\"machines\": []}\n")
	if err := os.WriteFile(list, orig, 0o600); err != nil {
		t.Fatal(err)
	}

	answers := []string{
		"name=grok node=missing pair=missing ssh=missing already=no",
		"name=grok node=ready pair=ready ssh=ready already=no",
	}
	club.SetTakeover(func(_ context.Context, _, step string) (string, error) {
		low := strings.ToLower(step)
		for _, bad := range []string{"private key", "database_url", "begin "} {
			if strings.Contains(low, bad) {
				t.Errorf("the step sent to the bot carries %q", bad)
			}
		}
		line := answers[0]
		if len(answers) > 1 {
			answers = answers[1:]
		}
		return line, nil
	})

	space, err := club.Space(context.Background(), core.DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := space.AddAccount(context.Background(), protocol.AddAccountParams{Provider: grokbot.Name, Credential: "fixture-credential-1"})
	if err != nil {
		t.Fatal(err)
	}
	page := pageState{Page: pageAccount, Account: acct.AccountID}
	if v := render(t, h, page); !strings.Contains(text(v), "夷舍") {
		t.Fatalf("no takeover button:\n%s", text(v))
	}
	if v := render(t, h, page); !strings.Contains(text(v), "hub 的机器列表要等你确认后才会写") {
		t.Fatal("the takeover button does not ask first")
	}

	started := act(t, h, page, actTakeover, nil)
	if !strings.Contains(started.Toast, "已开始夷舍") || started.View == nil {
		t.Fatalf("start: %+v", started.Toast)
	}
	st := takeoverState(t, started.View.State)
	if st.Takeover == "" {
		t.Fatal("the page forgot the takeover")
	}

	progress := act(t, h, st, actTakeoverGo, nil)
	if !strings.Contains(progress.Toast, "node") {
		t.Fatalf("progress: %s", progress.Toast)
	}
	if same, _ := os.ReadFile(list); string(same) != string(orig) {
		t.Fatal("progress wrote the machine list before confirmation")
	}

	st = takeoverState(t, progress.View.State)
	ready := act(t, h, st, actTakeoverGo, nil)
	if !strings.Contains(ready.Toast, "确认后才会写入") {
		t.Fatalf("ready: %s", ready.Toast)
	}
	if same, _ := os.ReadFile(list); string(same) != string(orig) {
		t.Fatal("showing the plan wrote the machine list")
	}

	wrote := act(t, h, takeoverState(t, ready.View.State), actTakeoverYes, nil)
	if !strings.Contains(wrote.Toast, "已写入机器列表") {
		t.Fatalf("confirm: %s", wrote.Toast)
	}
	got, _ := os.ReadFile(list)
	if !strings.Contains(string(got), `"name": "grok"`) {
		t.Fatalf("not written:\n%s", got)
	}

	// confirming again does not overwrite
	again := act(t, h, takeoverState(t, wrote.View.State), actTakeoverYes, nil)
	if strings.Contains(again.Toast, "已写入机器列表") {
		t.Fatalf("a second confirm rewrote it: %s", again.Toast)
	}
}

// TestTakeoverLeavesAnExistingNode covers a box that is already linked: the page says so and the machine list is
// byte for byte what it was.
func TestTakeoverLeavesAnExistingNode(t *testing.T) {
	h, club := start(t)
	club.Registry().Register(grokNamed{})
	dir := t.TempDir()
	club.SetPersonaDir(dir)
	list := filepath.Join(dir, "managers", "default", "manager.json")
	os.MkdirAll(filepath.Dir(list), 0o700)
	orig := []byte("{\"machines\": [{\"name\": \"grok\", \"kind\": \"ssh\", \"ssh_host\": \"grok\"}]}\n")
	os.WriteFile(list, orig, 0o600)
	club.SetTakeover(func(context.Context, string, string) (string, error) {
		return "name=grok node=ready pair=ready ssh=ready already=yes", nil
	})
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	acct, err := space.AddAccount(context.Background(), protocol.AddAccountParams{Provider: grokbot.Name, Credential: "fixture-credential-1"})
	if err != nil {
		t.Fatal(err)
	}
	page := pageState{Page: pageAccount, Account: acct.AccountID}
	st := takeoverState(t, act(t, h, page, actTakeover, nil).View.State)
	progress := act(t, h, st, actTakeoverGo, nil)
	if !strings.Contains(progress.Toast, "已接入") {
		t.Fatalf("progress: %s", progress.Toast)
	}
	confirm := act(t, h, takeoverState(t, progress.View.State), actTakeoverYes, nil)
	if !strings.Contains(confirm.Toast, "不会改动") {
		t.Fatalf("confirm: %s", confirm.Toast)
	}
	if same, _ := os.ReadFile(list); string(same) != string(orig) {
		t.Fatalf("an existing node was rewritten:\n%s", same)
	}
}

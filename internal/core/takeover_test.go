package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	testing "testing"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider/grokbot"
)

// drive answers the bot with one line per step, in the order the takeover asks: node, then pair, then ssh.
func drive(lines ...string) func(context.Context, string, string) (string, error) {
	i := 0
	return func(_ context.Context, _, step string) (string, error) {
		if strings.Contains(step, "PRIVATE KEY") || strings.Contains(step, "database_url") {
			return "", errSecret(step)
		}
		if i >= len(lines) {
			return lines[len(lines)-1], nil
		}
		line := lines[i]
		i++
		return line, nil
	}
}

type errSecret string

func (e errSecret) Error() string { return "the step carried a secret: " + string(e) }

func TestTakeoverStartProgressAndConfirm(t *testing.T) {
	e := newEnv(t, grokbot.Name)
	dir := t.TempDir()
	e.club.opts.PersonaDir = dir
	list := filepath.Join(dir, "managers", "default", "manager.json")
	os.MkdirAll(filepath.Dir(list), 0o700)
	orig := []byte("{\"kept\": true, \"machines\": []}\n")
	os.WriteFile(list, orig, 0o600)
	e.club.opts.Takeover = drive(
		"name=grok node=missing pair=missing ssh=missing already=no",
		"name=grok node=ready pair=missing ssh=missing already=no",
		"name=grok node=ready pair=ready ssh=missing already=no",
		"name=grok node=ready pair=ready ssh=ready already=no",
	)
	ctx := context.Background()
	a := e.pair(t, "fixture-credential-1")

	started, err := e.space.StartTakeover(ctx, protocol.TakeoverStartParams{AccountID: a.AccountID})
	if err != nil || started.Phase != "node" {
		t.Fatalf("start: %+v %v", started, err)
	}
	id := started.TakeoverID

	want := []string{"node", "pair", "ssh", "plan"}
	for i, phase := range want {
		st, err := e.space.TakeoverStatus(ctx, protocol.TakeoverParams{TakeoverID: id})
		if err != nil || st.Phase != phase {
			t.Fatalf("poll %d: %+v %v", i, st, err)
		}
	}

	// before the plan is shown, apply writes nothing
	if res, err := e.space.ApplyTakeover(ctx, protocol.TakeoverParams{TakeoverID: id}); err != nil || res.Applied {
		t.Fatalf("apply before confirm: %+v %v", res, err)
	}
	if same, _ := os.ReadFile(list); string(same) != string(orig) {
		t.Fatalf("the machine list changed before confirmation:\n%s", same)
	}

	plan, err := e.space.TakeoverPlan(ctx, protocol.TakeoverParams{TakeoverID: id})
	if err != nil || plan.Node != "grok" || plan.Kind != "ssh" || plan.AlreadyLinked {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	applied, err := e.space.ApplyTakeover(ctx, protocol.TakeoverParams{TakeoverID: id})
	if err != nil || !applied.Applied || applied.Backup == "" {
		t.Fatalf("apply: %+v %v", applied, err)
	}
	wrote, _ := os.ReadFile(list)
	if !strings.Contains(string(wrote), `"name": "grok"`) || !strings.Contains(string(wrote), `"kept": true`) {
		t.Fatalf("written:\n%s", wrote)
	}

	// a second apply does not overwrite
	if _, err := e.space.ApplyTakeover(ctx, protocol.TakeoverParams{TakeoverID: id}); err == nil {
		t.Fatal("a second apply was accepted")
	}
}

func TestTakeoverDoesNotTouchAnExistingNode(t *testing.T) {
	e := newEnv(t, grokbot.Name)
	dir := t.TempDir()
	e.club.opts.PersonaDir = dir
	list := filepath.Join(dir, "managers", "default", "manager.json")
	os.MkdirAll(filepath.Dir(list), 0o700)
	orig := []byte("{\"machines\": [{\"name\": \"grok\", \"kind\": \"ssh\", \"ssh_host\": \"grok\"}]}\n")
	os.WriteFile(list, orig, 0o600)
	e.club.opts.Takeover = drive("name=grok node=ready pair=ready ssh=ready already=yes")
	ctx := context.Background()
	a := e.pair(t, "fixture-credential-1")
	started, err := e.space.StartTakeover(ctx, protocol.TakeoverStartParams{AccountID: a.AccountID})
	if err != nil {
		t.Fatal(err)
	}
	st, err := e.space.TakeoverStatus(ctx, protocol.TakeoverParams{TakeoverID: started.TakeoverID})
	if err != nil || st.Phase != "linked" {
		t.Fatalf("status: %+v %v", st, err)
	}
	plan, err := e.space.TakeoverPlan(ctx, protocol.TakeoverParams{TakeoverID: started.TakeoverID})
	if err != nil || !plan.AlreadyLinked {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	res, err := e.space.ApplyTakeover(ctx, protocol.TakeoverParams{TakeoverID: started.TakeoverID})
	if err != nil || res.Applied {
		t.Fatalf("apply: %+v %v", res, err)
	}
	if same, _ := os.ReadFile(list); string(same) != string(orig) {
		t.Fatal("an existing entry was rewritten")
	}
}

func TestTakeoverRefusesOtherProvidersAndASecret(t *testing.T) {
	e := newEnv(t, "cue")
	a := e.pair(t, "fixture-credential-1")
	if _, err := e.space.StartTakeover(context.Background(), protocol.TakeoverStartParams{AccountID: a.AccountID}); err == nil {
		t.Fatal("a cue account was accepted")
	}
	g := newEnv(t, grokbot.Name)
	g.club.opts.Takeover = func(_ context.Context, _, _ string) (string, error) {
		return "name=grok BEGIN OPENSSH PRIVATE KEY", nil
	}
	b := g.pair(t, "fixture-credential-1")
	started, err := g.space.StartTakeover(context.Background(), protocol.TakeoverStartParams{AccountID: b.AccountID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.space.TakeoverStatus(context.Background(), protocol.TakeoverParams{TakeoverID: started.TakeoverID}); err == nil {
		t.Fatal("a report carrying a key was accepted")
	}
}

func TestTakeoverExpires(t *testing.T) {
	e := newEnv(t, grokbot.Name)
	a := e.pair(t, "fixture-credential-1")
	started, err := e.space.StartTakeover(context.Background(), protocol.TakeoverStartParams{AccountID: a.AccountID})
	if err != nil {
		t.Fatal(err)
	}
	e.advance(31 * time.Minute)
	if _, err := e.space.TakeoverStatus(context.Background(), protocol.TakeoverParams{TakeoverID: started.TakeoverID}); err == nil {
		t.Fatal("an expired takeover was still there")
	}
}

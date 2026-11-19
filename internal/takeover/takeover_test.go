package takeover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseReport(t *testing.T) {
	r, err := ParseReport("name=grok node=ready pair=missing ssh=missing already=no")
	if err != nil || r.Node != "grok" || !r.NodeReady || r.Paired || r.Phase() != PhasePair {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := ParseReport("name=grok node=ready pair=ready ssh=ready already=no token=secret"); err == nil {
		t.Fatal("an unknown field was accepted")
	}
	for _, line := range []string{
		"name=grok BEGIN OPENSSH PRIVATE KEY",
		"name=grok database_url=postgres://x",
		`name=grok "psk"=abc`,
	} {
		if _, err := ParseReport(line); err == nil {
			t.Fatalf("a secret was accepted: %s", line)
		}
	}
	already, err := ParseReport("name=grok node=ready pair=ready ssh=ready already=yes")
	if err != nil || already.Phase() != PhaseLinked {
		t.Fatalf("already: %+v %v", already, err)
	}
}

func TestStepNamesNoSecret(t *testing.T) {
	for _, phase := range []string{PhaseNode, PhasePair, PhaseSSH} {
		text, err := Step(phase, "grok")
		if err != nil {
			t.Fatal(err)
		}
		low := strings.ToLower(text)
		for _, bad := range []string{"private key", "database_url", "password", "10.", "192.168", "BEGIN "} {
			if strings.Contains(low, bad) {
				t.Fatalf("%s step contains %q:\n%s", phase, bad, text)
			}
		}
	}
	if _, err := Step(PhasePlan, "grok"); err == nil {
		t.Fatal("a plan is not a step for the box")
	}
	if _, err := Step(PhaseNode, "Bad Name"); err == nil {
		t.Fatal("a bad node name was accepted")
	}
}

func TestPrepareAndApplyAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.json")
	orig := []byte("{\n  \"hub\": \"kept\",\n  \"machines\": [{\"name\": \"other\", \"kind\": \"ssh\", \"ssh_host\": \"other\"}]\n}\n")
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := Prepare(path, "grok", "/home/box/.persona/bin/persona-node")
	if err != nil || plan.AlreadyLinked || plan.Kind != "ssh" || plan.SSHHost != "grok" {
		t.Fatalf("prepare: %+v %v", plan, err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(orig) {
		t.Fatal("prepare wrote the file")
	}

	backup, err := Apply(path, plan, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(backup)
	if string(saved) != string(orig) {
		t.Fatalf("backup is not the original:\n%s", saved)
	}
	wrote, _ := os.ReadFile(path)
	if !strings.Contains(string(wrote), `"name": "grok"`) || !strings.Contains(string(wrote), `"hub": "kept"`) {
		t.Fatalf("written:\n%s", wrote)
	}
	if _, err := Apply(path, plan, time.Now()); err == nil {
		t.Fatal("a second apply overwrote the entry")
	}
	again, err := Prepare(path, "grok", "/home/box/.persona/bin/persona-node")
	if err != nil || !again.AlreadyLinked {
		t.Fatalf("second prepare: %+v %v", again, err)
	}
	if _, err := Apply(path, again, time.Now()); err == nil {
		t.Fatal("an already-linked plan was written")
	}
}

func TestApplyRefusesAPartialPlan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.json")
	os.WriteFile(path, []byte(`{"machines":[]}`), 0o600)
	if _, err := Apply(path, Plan{Node: "grok"}, time.Now()); err == nil {
		t.Fatal("a plan without a kind was written")
	}
}

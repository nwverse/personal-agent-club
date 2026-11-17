package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/groq"
)

func TestConcurrentGroqSendsIncludeCompletedExchange(t *testing.T) {
	const first = "Remember the word cedar"
	const reply = "I will remember cedar"
	const second = "Which word did I give you?"
	type turn struct {
		Role string `json:"role"`
		Text string `json:"content"`
	}

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondHistory := make(chan []turn, 2)
	release := sync.OnceFunc(func() { close(releaseFirst) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": groq.DefaultModel}}})
			return
		}
		var request struct{ Messages []turn }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) == 0 {
			t.Errorf("invalid completion request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch request.Messages[len(request.Messages)-1].Text {
		case first:
			close(firstEntered)
			<-releaseFirst
		case second:
			secondHistory <- request.Messages
		default:
			t.Error("unexpected prompt")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": reply}}}})
	}))
	defer server.Close()

	club := New(Options{Registry: provider.NewRegistry(groq.New(groq.Options{Origin: server.URL})), FollowUps: []time.Duration{}})
	defer club.Close()
	defer release()
	ctx := context.Background()
	space, err := club.Space(ctx, DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	account, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "groq", Credential: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	key := SessionKey("groq", account.AccountID, "chat")
	send := func(id string, text string) <-chan string {
		done := make(chan string, 1)
		go func() {
			message, err := space.Send(ctx, protocol.SendParams{SessionKey: key, ClientID: id, Text: text})
			if err != nil || message.Local == nil {
				t.Errorf("send %q: message=%#v, err=%v", text, message, err)
				done <- ""
				return
			}
			done <- message.Local.Status
		}()
		return done
	}

	firstDone := send(uuid(1), first)
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first completion did not reach the provider")
	}
	secondDone := send(uuid(2), second)
	select {
	case history := <-secondHistory:
		t.Fatalf("second completion reached the provider before the first response: %v", history)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	for _, done := range []<-chan string{firstDone, secondDone} {
		select {
		case status := <-done:
			if status != protocol.LocalSent {
				t.Fatalf("send status = %q, want sent", status)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("send did not settle")
		}
	}
	want := []turn{{protocol.RoleUser, first}, {protocol.RoleAssistant, reply}, {protocol.RoleUser, second}}
	select {
	case history := <-secondHistory:
		if !reflect.DeepEqual(history, want) {
			t.Fatalf("second request history = %v, want %v", history, want)
		}
	default:
		t.Fatal("missing second completion")
	}
}

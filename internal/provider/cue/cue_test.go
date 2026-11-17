package cue

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/providertest"
)

const key = "fixture-key-not-real"

var ctx = context.Background()

func routes(t *testing.T) func(providertest.Call) providertest.Answer {
	manus := providertest.Fixture(t, "manus-api-v2.json")
	return func(c providertest.Call) providertest.Answer {
		method := strings.TrimPrefix(c.Path, "/v2/")
		if method == "task.listMessages" {
			return providertest.Answer{Body: manus["task.listMessages:"+c.Query["task_id"][0]]}
		}
		return providertest.Answer{Body: manus[method]}
	}
}

func TestListsMainTaskAndLiveSubtasksWithTheKeyHeaderOnly(t *testing.T) {
	up := providertest.Serve(t, routes(t))
	a := New(Options{Origin: up.URL})
	acct, err := a.Verify(ctx, key)
	if err != nil || !reflect.DeepEqual(acct, provider.RemoteAccount{ExternalAccountID: "manus", DisplayName: "Cue · 小助手"}) {
		t.Fatalf("%+v %v", acct, err)
	}
	sessions, err := a.ListSessions(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.RemoteSession{
		{ExternalID: "task-main-1", Title: "小助手", Kind: protocol.KindMain, CanSend: true,
			Ref: map[string]string{"agentId": "agent-default", "taskId": "task-main-1"}},
		{ExternalID: "task-sub-1", Title: "订机票", Kind: protocol.KindTask, ParentExternalID: "task-main-1",
			LastActivityMs: 1761801000000, Ref: map[string]string{"agentId": "agent-default", "taskId": "task-sub-1"}},
	}
	if !reflect.DeepEqual(sessions, want) {
		t.Fatalf("%+v", sessions)
	}
	for _, c := range up.Calls() {
		if c.Header.Get("x-manus-api-key") != key || c.Header.Get("authorization") != "" {
			t.Fatalf("headers of %s: %v", c.Path, c.Header)
		}
		if c.Path == "/v2/task.list" && (c.Query["scope"][0] != "agent_subtask" || c.Query["agent_id"][0] != "agent-default") {
			t.Fatalf("task.list query %v", c.Query)
		}
	}
}

func blocksJSON(t *testing.T, b []protocol.Block) string {
	t.Helper()
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestMapsTaskEventsOldestFirstIntoBlocks(t *testing.T) {
	a := New(Options{Origin: providertest.Serve(t, routes(t)).URL})
	page, err := a.ListMessages(ctx, key, provider.SessionRef{ExternalID: "task-main-1",
		Ref: map[string]string{"taskId": "task-main-1"}}, provider.ListOptions{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var got [][2]string
	for _, m := range page.Messages {
		got = append(got, [2]string{m.ExternalID, m.Role})
	}
	if !reflect.DeepEqual(got, [][2]string{{"ev-1", "user"}, {"ev-2", "assistant"}, {"ev-3", "tool"}, {"ev-4", "system"}}) {
		t.Fatalf("%v", got)
	}
	if j := blocksJSON(t, page.Messages[2].Blocks); j != `[{"type":"tool-call","name":"browser","status":"success","summary":"查询航班","input":"https://example.com/flights"}]` {
		t.Fatal(j)
	}
	if j := blocksJSON(t, page.Messages[3].Blocks); j != `[{"type":"status","text":"需要你确认发送邮件","status":"waiting"}]` {
		t.Fatal(j)
	}
	if page.Messages[0].CreateTimeMs != 1761802001000 || len(page.Raw) != 1 {
		t.Fatalf("%+v", page.Messages[0])
	}
	sub, err := a.ListMessages(ctx, key, provider.SessionRef{ExternalID: "task-sub-1",
		Ref: map[string]string{"taskId": "task-sub-1"}}, provider.ListOptions{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if j := blocksJSON(t, sub.Messages[0].Blocks); j != `[{"type":"text","text":"找到 3 个航班"},{"type":"attachment","name":"flights.md","kind":"file","url":"https://files.manus.im/flights.md"}]` {
		t.Fatal(j)
	}
	if j := blocksJSON(t, sub.Messages[1].Blocks); j != `[{"type":"tool-result","text":"✓ 搜索航班\n… 比较价格","name":"plan"}]` {
		t.Fatal(j)
	}
}

func TestEventKinds(t *testing.T) {
	for _, tc := range []struct {
		event map[string]any
		role  string
		json  string
	}{
		{map[string]any{"id": "x", "type": "assistant_message", "question_expectation": map[string]any{"options": []any{"A", "B", 3}}},
			"assistant", `[{"type":"text","text":"· A\n· B"}]`},
		{map[string]any{"id": "x", "type": "error_message", "error_type": "quota"}, "system", `[{"type":"error","text":"quota"}]`},
		{map[string]any{"id": "x", "type": "new_plan_step", "title": "s"}, "system", `[{"type":"status","text":"s","status":"step"}]`},
		{map[string]any{"id": "x", "type": "user_stop"}, "system", `[{"type":"status","status":"stopped"}]`},
		{map[string]any{"id": "x", "type": "explanation", "content": "why"}, "tool", `[{"type":"tool-result","text":"why","name":"explanation"}]`},
		{map[string]any{"id": "x", "type": "structured_output_result", "success": true, "value": map[string]any{"a": 1}},
			"tool", `[{"type":"tool-result","text":"{\"a\":1}","name":"structured_output"}]`},
		{map[string]any{"id": "x", "type": "structured_output_result", "success": false}, "tool",
			`[{"type":"tool-result","text":"failed","name":"structured_output"}]`},
	} {
		m, ok := Message(tc.event)
		if !ok || m.Role != tc.role || blocksJSON(t, m.Blocks) != tc.json {
			t.Errorf("%v: %v %s %s", tc.event, ok, m.Role, blocksJSON(t, m.Blocks))
		}
	}
	for _, ev := range []map[string]any{{"type": "user_message"}, {"id": "x", "type": "nope"},
		{"id": "x", "type": "explanation"}, {"id": "x", "type": "plan_update", "steps": []any{}}} {
		if _, ok := Message(ev); ok {
			t.Errorf("%v kept", ev)
		}
	}
}

func TestSendsOnceAndReportsLostAnswersAsUnconfirmed(t *testing.T) {
	manus := providertest.Fixture(t, "manus-api-v2.json")
	session := provider.SessionRef{ExternalID: "task-main-1", Ref: map[string]string{"taskId": "task-main-1"}}
	msg := provider.Outgoing{ClientID: "6f1d1c51-8a5e-4ac5-9a77-3c7c1d1b7c11", Text: "好"}
	for _, tc := range []struct {
		name   string
		answer providertest.Answer
		want   provider.SendResult
		code   string
	}{
		{"ok", providertest.Answer{Body: manus["task.sendMessage"]}, provider.SendResult{Status: provider.Sent}, ""},
		{"lost", providertest.Answer{Drop: true}, provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}, ""},
		{"empty success", providertest.Answer{Body: []byte{}}, provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}, ""},
		{"malformed success", providertest.Answer{Body: []byte("{")}, provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}, ""},
		{"unrecognized success", providertest.Answer{Body: map[string]any{"unrecognized": true}}, provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}, ""},
		{"wrong success field type", providertest.Answer{Body: map[string]any{"ok": "false"}}, provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}, ""},
		{"explicit refusal with HTTP 200", providertest.Answer{Body: manus["error.invalid_argument"]}, provider.SendResult{}, "INVALID_ARGUMENT"},
		{"explicit refusal without a code with HTTP 200", providertest.Answer{Body: map[string]any{"ok": false}}, provider.SendResult{}, "SEND_REFUSED"},
		{"busy", providertest.Answer{Status: 502}, provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}, ""},
		{"refused", providertest.Answer{Status: 400, Body: manus["error.invalid_argument"]}, provider.SendResult{}, "INVALID_ARGUMENT"},
		{"refused without a code", providertest.Answer{Status: 400, Body: map[string]any{"ok": false}}, provider.SendResult{}, "SEND_REFUSED"},
		{"expired", providertest.Answer{Status: 401, Body: manus["error.unauthorized"]}, provider.SendResult{}, "UNAUTHENTICATED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := providertest.Serve(t, func(providertest.Call) providertest.Answer { return tc.answer })
			res, err := New(Options{Origin: up.URL}).Send(ctx, key, session, msg, provider.SendContext{})
			if provider.CodeOf(err) != tc.code && !(tc.code == "" && err == nil) {
				t.Fatalf("err %v, want %s", err, tc.code)
			}
			if !reflect.DeepEqual(res, tc.want) {
				t.Fatalf("%+v", res)
			}
			calls := up.Calls()
			if len(calls) != 1 {
				t.Fatalf("%d calls", len(calls))
			}
			if string(calls[0].Body) != `{"message":{"content":"好"},"task_id":"task-main-1"}` {
				t.Fatalf("%s", calls[0].Body)
			}
		})
	}
}

func TestReadErrors(t *testing.T) {
	manus := providertest.Fixture(t, "manus-api-v2.json")
	for status, code := range map[int]string{401: "AUTH_FAILED", 403: "AUTH_FAILED", 429: "RATE_LIMITED", 500: "UPSTREAM_ERROR", 404: "REQUEST_FAILED"} {
		up := providertest.Serve(t, func(providertest.Call) providertest.Answer {
			return providertest.Answer{Status: status, Body: manus["error.unauthorized"]}
		})
		if _, err := New(Options{Origin: up.URL}).Verify(ctx, key); provider.CodeOf(err) != code {
			t.Errorf("%d: %v", status, err)
		}
	}
	up := providertest.Serve(t, func(providertest.Call) providertest.Answer {
		return providertest.Answer{Body: map[string]any{"ok": false}}
	})
	if _, err := New(Options{Origin: up.URL}).ListSessions(ctx, key); provider.CodeOf(err) != "REQUEST_FAILED" {
		t.Fatal(err)
	}
	up = providertest.Serve(t, func(providertest.Call) providertest.Answer { return providertest.Answer{Drop: true} })
	if _, err := New(Options{Origin: up.URL}).Verify(ctx, key); provider.CodeOf(err) != "NETWORK_ERROR" {
		t.Fatal(err)
	}
}

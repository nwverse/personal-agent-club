// Package cue is the Cue (Manus) provider: the official Manus API v2. Every Cue agent has a main task (its
// persistent conversation) and may spawn subtasks (task_type agent_subtask), which are read-only here. There are no
// webhooks, so club polls. The API key goes in x-manus-api-key and nowhere else.
package cue

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/web"
)

// Name is the provider's name.
const Name = "cue"

// Origin is the Manus API's.
const Origin = "https://api.manus.ai"

const subtaskPages = 5

var errorCode = regexp.MustCompile(`^[a-z_]{1,48}$`)

// Options configure the adapter; zero values are the real API and http.DefaultClient's behavior.
type Options struct {
	Client *http.Client
	Origin string
}

// Adapter is the Cue provider.
type Adapter struct {
	client *http.Client
	origin string
}

// New is the adapter.
func New(o Options) *Adapter {
	if o.Client == nil {
		o.Client, _ = web.NewClient("")
	}
	if o.Origin == "" {
		o.Origin = Origin
	}
	return &Adapter{client: o.Client, origin: strings.TrimRight(o.Origin, "/")}
}

var _ provider.Adapter = (*Adapter)(nil)

func (a *Adapter) Name() string       { return Name }
func (a *Adapter) Connection() string { return protocol.ConnectionServer }

type param struct{ name, value string }

func (a *Adapter) get(ctx context.Context, key, method string, query ...param) (map[string]any, error) {
	q := url.Values{}
	for _, p := range query {
		q.Set(p.name, p.value)
	}
	target := a.origin + "/v2/" + method
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	r := web.Request(ctx, a.client, http.MethodGet, target, map[string]string{"accept": "application/json",
		"x-manus-api-key": key}, nil, 0)
	if !r.OK {
		return nil, web.ReadError(r)
	}
	body := web.Record(r.Data)
	if body["ok"] != true {
		return nil, &provider.Error{Code: provider.CodeRequestFailed}
	}
	return body, nil
}

func (a *Adapter) agents(ctx context.Context, key string) ([]map[string]any, error) {
	body, err := a.get(ctx, key, "agent.list")
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(web.Records(body["data"]), func(agent map[string]any) bool {
		return web.Text(agent["id"]) == "" || web.Text(agent["task_id"]) == ""
	}), nil
}

// Verify lists the agents: Manus names no account, so every key of the owner's is the account "manus".
func (a *Adapter) Verify(ctx context.Context, key string) (provider.RemoteAccount, error) {
	list, err := a.agents(ctx, key)
	if err != nil {
		return provider.RemoteAccount{}, err
	}
	return provider.RemoteAccount{ExternalAccountID: "manus", DisplayName: accountName(list)}, nil
}

func accountName(agents []map[string]any) string {
	var names []string
	for _, agent := range agents {
		if n := web.Text(agent["nickname"]); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 1 {
		return "Cue · " + names[0]
	}
	return "Cue"
}

// ListSessions is each agent's main task, then its subtasks that are not deleted.
func (a *Adapter) ListSessions(ctx context.Context, key string) ([]provider.RemoteSession, error) {
	agents, err := a.agents(ctx, key)
	if err != nil {
		return nil, err
	}
	var out []provider.RemoteSession
	for _, agent := range agents {
		agentID, main := web.Text(agent["id"]), web.Text(agent["task_id"])
		out = append(out, provider.RemoteSession{ExternalID: main, Title: or(web.Text(agent["nickname"]), "Cue"),
			Kind: protocol.KindMain, CanSend: true, Ref: map[string]string{"agentId": agentID, "taskId": main}})
		cursor := ""
		for range subtaskPages {
			query := []param{{"scope", "agent_subtask"}, {"agent_id", agentID}, {"limit", "100"}}
			if cursor != "" {
				query = append(query, param{"cursor", cursor})
			}
			body, err := a.get(ctx, key, "task.list", query...)
			if err != nil {
				return nil, err
			}
			for _, task := range web.Records(body["data"]) {
				id := web.Text(task["id"])
				if id == "" || task["status"] == "deleted" {
					continue
				}
				activity := task["updated_at"]
				if activity == nil {
					activity = task["created_at"]
				}
				out = append(out, provider.RemoteSession{ExternalID: id, Title: or(web.Text(task["title"]), id),
					Kind: protocol.KindTask, ParentExternalID: main, LastActivityMs: seconds(activity), CanSend: false,
					Ref: map[string]string{"agentId": agentID, "taskId": id}})
			}
			cursor = ""
			if body["has_more"] == true {
				cursor = web.Text(body["next_cursor"])
			}
			if cursor == "" {
				break
			}
		}
	}
	return out, nil
}

func taskID(s provider.SessionRef) string {
	if id, ok := s.Ref["taskId"]; ok {
		return id
	}
	return s.ExternalID
}

// ListMessages reads the newest events of the task, oldest first.
func (a *Adapter) ListMessages(ctx context.Context, key string, s provider.SessionRef, o provider.ListOptions) (provider.RemotePage, error) {
	body, err := a.get(ctx, key, "task.listMessages", param{"task_id", taskID(s)},
		param{"limit", strconv.Itoa(min(200, max(1, o.Limit)))}, param{"order", "desc"}, param{"verbose", "true"})
	if err != nil {
		return provider.RemotePage{}, err
	}
	var msgs []provider.RemoteMessage
	for _, ev := range web.Records(body["messages"]) {
		if m, ok := Message(ev); ok {
			msgs = append(msgs, m)
		}
	}
	slices.Reverse(msgs)
	return provider.RemotePage{Messages: msgs, Raw: []any{body}}, nil
}

// Send posts the message once. Manus takes no idempotency key: a send whose answer was lost may have landed, so it
// is reported unconfirmed, never retried.
func (a *Adapter) Send(ctx context.Context, key string, s provider.SessionRef, msg provider.Outgoing, _ provider.SendContext) (provider.SendResult, error) {
	r := web.Request(ctx, a.client, http.MethodPost, a.origin+"/v2/task.sendMessage", map[string]string{
		"accept": "application/json", "content-type": "application/json", "x-manus-api-key": key},
		map[string]any{"task_id": taskID(s), "message": map[string]any{"content": msg.Text}}, 0)
	body := web.Record(r.Data)
	if r.OK && body["ok"] == true {
		return provider.SendResult{Status: provider.Sent}, nil
	}
	code := web.Text(web.Record(body["error"])["code"])
	hasErrorCode := errorCode.MatchString(code)
	explicitRefusal := body["ok"] == false || hasErrorCode
	if r.Transport || r.Status >= 500 || (r.Status >= 200 && r.Status < 300 && !explicitRefusal) {
		return provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}, nil
	}
	if hasErrorCode {
		code = strings.ToUpper(code)
	} else {
		code = "SEND_REFUSED"
	}
	e := &provider.Error{Code: code, HTTPStatus: r.Status}
	if r.Status == http.StatusTooManyRequests {
		e.RetryAfter = web.RetryAfter(r.Header, time.Now())
	}
	return provider.SendResult{}, e
}

func seconds(v any) int64 {
	n, ok := web.Integer(v)
	if !ok || n <= 0 {
		return 0
	}
	return n * 1000
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func textBlock(t string) protocol.Block { return protocol.TextBlock(t) }

func attachments(v any) []protocol.Block {
	var out []protocol.Block
	for _, item := range web.Records(v) {
		b := protocol.Block{Type: "attachment", Name: or(or(web.Text(item["filename"]), web.Text(item["type"])), "attachment"),
			Kind: web.Text(item["type"])}
		if u := web.Text(item["url"]); strings.HasPrefix(u, "https://") {
			b.URL = u
		}
		out = append(out, b)
	}
	return out
}

func inputOf(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	b, _ := json.Marshal(s)
	return b
}

// Message is one Manus task event as a message; ok is false for an event without id or of an unknown type.
func Message(ev map[string]any) (provider.RemoteMessage, bool) {
	id := web.Text(ev["id"])
	if id == "" {
		return provider.RemoteMessage{}, false
	}
	m := provider.RemoteMessage{ExternalID: id, CreateTimeMs: web.Millis(ev["timestamp"])}
	content := web.Text(ev["content"])
	switch ev["type"] {
	case "user_message":
		m.Role = protocol.RoleUser
		if content != "" {
			m.Blocks = append(m.Blocks, textBlock(content))
		}
		m.Blocks = append(m.Blocks, attachments(ev["attachments"])...)
	case "assistant_message":
		m.Role = protocol.RoleAssistant
		lines := []string{}
		if content != "" {
			lines = append(lines, content)
		}
		for _, o := range web.Strings(web.Record(ev["question_expectation"])["options"]) {
			lines = append(lines, "· "+o)
		}
		if len(lines) > 0 {
			m.Blocks = append(m.Blocks, textBlock(strings.Join(lines, "\n")))
		}
		m.Blocks = append(m.Blocks, attachments(ev["attachments"])...)
	case "error_message":
		m.Role = protocol.RoleSystem
		m.Blocks = []protocol.Block{{Type: "error", Text: or(content, web.Text(ev["error_type"]))}}
	case "status_update":
		m.Role = protocol.RoleSystem
		detail := web.Record(ev["status_detail"])
		m.Blocks = []protocol.Block{{Type: "status", Status: or(web.Text(ev["agent_status"]), "unknown"),
			Text: or(web.Text(detail["waiting_description"]), web.Text(ev["brief"]))}}
	case "tool_used":
		m.Role = protocol.RoleTool
		m.Blocks = []protocol.Block{{Type: "tool-call", Name: or(web.Text(ev["tool"]), "tool"), Status: web.Text(ev["status"]),
			Summary: or(web.Text(ev["brief"]), web.Text(ev["description"])),
			Input:   inputOf(web.Text(web.Record(ev["message"])["param"]))}}
	case "plan_update":
		var steps []string
		for _, step := range web.Records(ev["steps"]) {
			steps = append(steps, planMark(step["status"])+" "+web.Text(step["title"]))
		}
		if len(steps) == 0 {
			return provider.RemoteMessage{}, false
		}
		m.Role = protocol.RoleTool
		m.Blocks = []protocol.Block{{Type: "tool-result", Name: "plan", Text: strings.Join(steps, "\n")}}
	case "new_plan_step":
		m.Role = protocol.RoleSystem
		m.Blocks = []protocol.Block{{Type: "status", Status: "step", Text: web.Text(ev["title"])}}
	case "explanation":
		if content == "" {
			return provider.RemoteMessage{}, false
		}
		m.Role = protocol.RoleTool
		m.Blocks = []protocol.Block{{Type: "tool-result", Name: "explanation", Text: content}}
	case "user_stop":
		m.Role = protocol.RoleSystem
		m.Blocks = []protocol.Block{{Type: "status", Status: "stopped"}}
	case "structured_output_result":
		m.Role = protocol.RoleTool
		text := or(web.Text(ev["error"]), "failed")
		if ev["success"] == true {
			value := ev["value"]
			if value == nil {
				value = map[string]any{}
			}
			b, _ := json.Marshal(value)
			text = string(b)
		}
		m.Blocks = []protocol.Block{{Type: "tool-result", Name: "structured_output", Text: text}}
	default:
		return provider.RemoteMessage{}, false
	}
	if m.Blocks == nil {
		m.Blocks = []protocol.Block{}
	}
	return m, true
}

func planMark(status any) string {
	switch status {
	case "done":
		return "✓"
	case "doing":
		return "…"
	case "failed":
		return "✗"
	}
	return "○"
}

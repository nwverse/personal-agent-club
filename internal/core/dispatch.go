package core

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/nwverse/personal-agent-club/internal/protocol"
)

// Call answers one protocol call in a space: method is the full name (agentsrc.status, club.info, …), params its
// JSON (empty or null for none). Errors are *protocol.Error for the four protocol codes; anything else carries only a
// message. Both shells answer through here.
func (c *Club) Call(ctx context.Context, space, method string, params json.RawMessage) (any, error) {
	if method == protocol.MethodClubInfo {
		return c.Info(), nil
	}
	ns, name, _ := strings.Cut(method, ".")
	if ns != protocol.AgentSourceNamespace && ns != protocol.ClubNamespace {
		return nil, protocol.Unsupported("unknown method " + method)
	}
	if ns == protocol.ClubNamespace && !slices.Contains(clubMethods, method) {
		return nil, protocol.Unsupported("unknown method " + method)
	}
	if ns == protocol.AgentSourceNamespace && !slices.Contains(protocol.AgentSourceMethods, name) {
		return nil, protocol.Unsupported("unknown method " + method)
	}
	s, err := c.Space(ctx, space)
	if err != nil {
		return nil, err
	}
	res, err := s.call(ctx, method, name, params)
	if err != nil && !errorIsProtocol(err) {
		c.opts.Log.Warn("call failed", "space", space, "method", method, "err", err)
	}
	return res, err
}

// clubMethods are the club.* methods this version knows (answered or stubbed).
var clubMethods = append(slices.Clone(protocol.ClubMethods), protocol.ClubStubMethods...)

func (s *Space) call(ctx context.Context, method, name string, params json.RawMessage) (any, error) {
	if strings.HasPrefix(method, protocol.ClubNamespace+".") {
		return s.callClub(ctx, method, params)
	}
	switch name {
	case protocol.MethodStatus:
		return s.Status(ctx)
	case protocol.MethodSessions:
		return s.Sessions(ctx)
	case protocol.MethodMessages:
		var p protocol.MessagesParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return s.Messages(ctx, p)
	case protocol.MethodSend:
		var p protocol.SendParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return s.Send(ctx, p)
	case protocol.MethodDeleteMessage:
		var p protocol.DeleteMessageParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return protocol.Empty{}, s.DeleteMessage(ctx, p)
	case protocol.MethodMarkRead:
		var p protocol.MarkReadParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return protocol.Empty{}, s.MarkRead(ctx, p)
	case protocol.MethodSearch:
		var p protocol.SearchParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return s.Search(ctx, p)
	}
	return nil, protocol.Unsupported("unknown method " + method)
}

func (s *Space) callClub(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case protocol.MethodClubAccountsAdd:
		return call(params, func(p protocol.AddAccountParams) (any, error) { return s.AddAccount(ctx, p) })
	case protocol.MethodClubAccountsUpdate:
		return call(params, func(p protocol.UpdateAccountParams) (any, error) { return s.UpdateAccount(ctx, p) })
	case protocol.MethodClubAccountsReplaceCred:
		return call(params, func(p protocol.ReplaceCredentialParams) (any, error) { return s.ReplaceCredential(ctx, p) })
	case protocol.MethodClubAccountsRemove:
		return call(params, func(p protocol.RemoveAccountParams) (any, error) { return protocol.Empty{}, s.RemoveAccount(ctx, p) })
	case protocol.MethodClubAccountsSync:
		return call(params, func(p protocol.AccountParams) (any, error) {
			if err := s.SyncAccount(ctx, p.AccountID); err != nil {
				return nil, err
			}
			return s.Account(ctx, p.AccountID)
		})
	case protocol.MethodClubLive:
		return call(params, func(p protocol.SessionParams) (any, error) { return s.Live(ctx, p) })
	case protocol.MethodClubSettingsGet:
		return s.Settings(ctx)
	case protocol.MethodClubSettingsSet:
		return call(params, func(p protocol.SettingsParams) (any, error) { return s.SetSettings(ctx, p) })
	case protocol.MethodClubOutboxDismiss:
		return call(params, func(p protocol.OutboxParams) (any, error) { return protocol.Empty{}, s.DismissOutbox(ctx, p) })
	case protocol.MethodClubLoginsGrokBotStart:
		return call(params, func(p protocol.GrokBotLoginStartParams) (any, error) { return s.StartGrokBotLogin(ctx, p) })
	case protocol.MethodClubLoginsGrokBotPoll:
		return call(params, func(p protocol.LoginParams) (any, error) { return s.PollGrokBotLogin(ctx, p) })
	case protocol.MethodClubLoginsGrokBotCancel:
		return call(params, func(p protocol.LoginParams) (any, error) { return protocol.Empty{}, s.CancelGrokBotLogin(ctx, p) })
	case protocol.MethodClubGrokBotTakeoverStart:
		return call(params, func(p protocol.TakeoverStartParams) (any, error) { return s.StartTakeover(ctx, p) })
	case protocol.MethodClubGrokBotTakeoverStatus:
		return call(params, func(p protocol.TakeoverParams) (any, error) { return s.TakeoverStatus(ctx, p) })
	case protocol.MethodClubGrokBotTakeoverPlan:
		return call(params, func(p protocol.TakeoverParams) (any, error) { return s.TakeoverPlan(ctx, p) })
	case protocol.MethodClubGrokBotTakeoverApply:
		return call(params, func(p protocol.TakeoverParams) (any, error) { return s.ApplyTakeover(ctx, p) })
	case protocol.MethodClubImport:
		return call(params, func(p protocol.ImportParams) (any, error) {
			return s.Import(ctx, p.Path, ImportOptions{OnlyProviders: p.OnlyProviders, DryRun: p.DryRun})
		})
	case protocol.MethodClubSessionsStart, protocol.MethodClubSessionsInterrupt:
		return nil, protocol.Unsupported(method + ": no provider in this build can start or interrupt a session").
			Because(protocol.ReasonProviderUnavailable)
	}
	return nil, protocol.Unsupported(method + " is not implemented yet")
}

func call[P any](params json.RawMessage, f func(P) (any, error)) (any, error) {
	var p P
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return f(p)
}

func decode(params json.RawMessage, v any) error {
	params = bytes.TrimSpace(params)
	if len(params) == 0 || string(params) == "null" {
		return nil
	}
	if err := json.Unmarshal(params, v); err != nil {
		return badInput("invalid params: " + err.Error())
	}
	return nil
}

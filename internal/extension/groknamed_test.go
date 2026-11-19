package extension

import (
	"context"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/grokbot"
)

// grokNamed is an in-memory provider that answers as grok_bot, so a takeover can be exercised without the grok box,
// which is offline, and without a real credential.
type grokNamed struct{}

func (grokNamed) Name() string       { return grokbot.Name }
func (grokNamed) Connection() string { return protocol.ConnectionServer }
func (grokNamed) Verify(context.Context, string) (provider.RemoteAccount, error) {
	return provider.RemoteAccount{ExternalAccountID: "grok-1", DisplayName: "Grok Bot"}, nil
}
func (grokNamed) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{{ExternalID: "bot/", Title: "Grok Bot", Kind: protocol.KindMain, CanSend: true}}, nil
}
func (grokNamed) ListMessages(context.Context, string, provider.SessionRef, provider.ListOptions) (provider.RemotePage, error) {
	return provider.RemotePage{}, nil
}
func (grokNamed) Send(context.Context, string, provider.SessionRef, provider.Outgoing, provider.SendContext) (provider.SendResult, error) {
	return provider.SendResult{Status: provider.Sent}, nil
}

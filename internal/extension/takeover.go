package extension

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
)

// driveTakeover runs one linking step through the Grok Bot's own main session and returns the one line its agent
// answered. The step carries no secret; a line that does is refused by the caller before it is kept.
func driveTakeover(club *core.Club) func(context.Context, string, string) (string, error) {
	return func(ctx context.Context, accountID, step string) (string, error) {
		space, err := club.Space(ctx, core.DefaultSpace)
		if err != nil {
			return "", err
		}
		sessions, err := space.Sessions(ctx)
		if err != nil {
			return "", err
		}
		var key string
		for _, s := range sessions.Sessions {
			if s.AccountID == accountID && s.CanSend {
				key = s.SessionKey
				break
			}
		}
		if key == "" {
			return "", protocol.NotFound("the bot has no session to ask")
		}
		sent, err := space.Send(ctx, protocol.SendParams{SessionKey: key, ClientID: uuid.NewString(), Text: step})
		if err != nil {
			return "", err
		}
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			page, err := space.Messages(ctx, protocol.MessagesParams{SessionKey: key, Limit: 20})
			if err != nil {
				return "", err
			}
			for _, m := range page.Messages {
				if m.Role != protocol.RoleAssistant || m.CreateTimeMs < sent.CreateTimeMs {
					continue
				}
				for _, b := range m.Blocks {
					if b.Type == "text" {
						return strings.TrimSpace(b.Text), nil
					}
				}
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		return "", protocol.Conflict("the bot did not answer in time")
	}
}

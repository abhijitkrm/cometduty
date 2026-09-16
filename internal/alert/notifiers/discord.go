package notifiers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cometduty/cometduty/internal/alert"
	"github.com/cometduty/cometduty/internal/config"
)

// Discord posts to a channel webhook with embeds.
type Discord struct{}

func (Discord) Kind() string { return "discord" }

type discordPayload struct {
	Username string         `json:"username"`
	Content  string         `json:"content"`
	Embeds   []discordEmbed `json:"embeds,omitempty"`
}

type discordEmbed struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description"`
	Color       int    `json:"color"`
}

func (Discord) Send(ctx context.Context, a *alert.Alert, cfg any) error {
	c, ok := cfg.(config.DiscordConfig)
	if !ok {
		return errors.New("invalid discord config")
	}
	if c.Webhook == "" {
		return errors.New("discord webhook not configured")
	}
	prefix := "🚨 ALERT"
	color := 0xd9480f // red-orange
	if a.Resolved {
		prefix = "💜 RESOLVED"
		color = 0x845ef7
	}
	// Mentions go in content (embeds don't notify).
	content := fmt.Sprintf("%s %s %s", prefix, a.Chain, strings.Join(c.Mentions, " "))
	return postJSON(ctx, c.Webhook, nil, discordPayload{
		Username: "cometduty",
		Content:  strings.TrimSpace(content),
		Embeds: []discordEmbed{{
			Title:       fmt.Sprintf("%s · %s", a.ChainID, a.Severity),
			Description: a.Message,
			Color:       color,
		}},
	})
}

func (d Discord) Test(ctx context.Context, cfg any) error {
	return d.Send(ctx, &alert.Alert{
		Chain: "test", ChainID: "test-1", Key: "test",
		Message:  "test alert from cometduty (this is not a real alert)",
		Severity: "info", Time: time.Now(),
	}, cfg)
}

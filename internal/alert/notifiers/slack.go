package notifiers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/config"
)

// Slack posts to an incoming webhook.
type Slack struct{}

func (Slack) Kind() string { return "slack" }

type slackPayload struct {
	Text        string            `json:"text"`
	Attachments []slackAttachment `json:"attachments,omitempty"`
}

type slackAttachment struct {
	Color string `json:"color"`
	Title string `json:"title"`
	Text  string `json:"text"`
	Ts    int64  `json:"ts"`
}

func (Slack) Send(ctx context.Context, a *alert.Alert, cfg any) error {
	c, ok := cfg.(config.SlackConfig)
	if !ok {
		return errors.New("invalid slack config")
	}
	if c.Webhook == "" {
		return errors.New("slack webhook not configured")
	}
	title := "🚨 cometduty alert"
	color := "danger"
	if a.Resolved {
		title = "💜 resolved"
		color = "good"
	}
	mentions := strings.Join(c.Mentions, " ")
	text := fmt.Sprintf("%s %s — %s %s", title, a.Chain, a.Message, mentions)
	return postJSON(ctx, c.Webhook, nil, slackPayload{
		Text: strings.TrimSpace(text),
		Attachments: []slackAttachment{{
			Color: color,
			Title: fmt.Sprintf("%s · %s · %s", a.Chain, a.ChainID, a.Severity),
			Text:  a.Message,
			Ts:    a.Time.Unix(),
		}},
	})
}

func (s Slack) Test(ctx context.Context, cfg any) error {
	return s.Send(ctx, &alert.Alert{
		Chain: "test", ChainID: "test-1", Key: "test",
		Message:  "test alert from cometduty (this is not a real alert)",
		Severity: "info", Time: time.Now(),
	}, cfg)
}

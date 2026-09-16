package notifiers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"text/template"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/config"
)

// Webhook is a generic JSON webhook whose body is a Go text/template rendered
// with the alert fields — covers Teams, Mattermost, Gotify, Home Assistant,
// and anything else that accepts a POST.
type Webhook struct{}

func (Webhook) Kind() string { return "webhook" }

const defaultWebhookBody = `{"chain":{{printf "%q" .Chain}},"chain_id":{{printf "%q" .ChainID}},"severity":{{printf "%q" .Severity}},"resolved":{{.Resolved}},"message":{{printf "%q" .Message}},"moniker":{{printf "%q" .Moniker}},"valcons":{{printf "%q" .Valcons}},"time":"{{.Time.Format "2006-01-02T15:04:05Z07:00"}}"}`

// RenderBody renders the configured template (or the default) for an alert.
func RenderBody(tpl string, a *alert.Alert) ([]byte, error) {
	if tpl == "" {
		tpl = defaultWebhookBody
	}
	t, err := template.New("webhook").Option("missingkey=zero").Parse(tpl)
	if err != nil {
		return nil, fmt.Errorf("webhook template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, a); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (Webhook) Send(ctx context.Context, a *alert.Alert, cfg any) error {
	c, ok := cfg.(config.WebhookConfig)
	if !ok {
		return errors.New("invalid webhook config")
	}
	if c.URL == "" {
		return errors.New("webhook url not configured")
	}
	body, err := RenderBody(c.TemplateBody, a)
	if err != nil {
		return err
	}
	return post(ctx, c.URL, "application/json", c.Headers, body)
}

func (w Webhook) Test(ctx context.Context, cfg any) error {
	return w.Send(ctx, &alert.Alert{
		Chain: "test", ChainID: "test-1", Key: "test",
		Message:  "test alert from cometduty (this is not a real alert)",
		Severity: "info", Time: time.Now(),
	}, cfg)
}

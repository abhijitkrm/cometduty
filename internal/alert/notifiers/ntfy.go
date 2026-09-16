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

// Ntfy posts to an ntfy.sh-compatible topic (public or self-hosted).
type Ntfy struct{}

func (Ntfy) Kind() string { return "ntfy" }

func (Ntfy) Send(ctx context.Context, a *alert.Alert, cfg any) error {
	c, ok := cfg.(config.NtfyConfig)
	if !ok {
		return errors.New("invalid ntfy config")
	}
	if c.URL == "" || c.Topic == "" {
		return errors.New("ntfy url/topic not configured")
	}
	url := strings.TrimRight(c.URL, "/") + "/" + c.Topic
	title := fmt.Sprintf("🚨 %s: %s", a.Chain, a.Severity)
	prio := c.Priority
	if prio == "" {
		prio = "default"
	}
	if a.Resolved {
		title = fmt.Sprintf("💜 resolved: %s", a.Chain)
		if prio != "urgent" {
			prio = "low"
		}
	}
	headers := map[string]string{
		"Title":    title,
		"Priority": prio,
		"Tags":     "cometduty," + a.ChainID,
	}
	if c.Token != "" {
		headers["Authorization"] = "Bearer " + c.Token
	}
	return post(ctx, url, "text/plain; charset=utf-8", headers, []byte(a.Message))
}

func (n Ntfy) Test(ctx context.Context, cfg any) error {
	return n.Send(ctx, &alert.Alert{
		Chain: "test", ChainID: "test-1", Key: "test",
		Message:  "test alert from cometduty (this is not a real alert)",
		Severity: "info", Time: time.Now(),
	}, cfg)
}

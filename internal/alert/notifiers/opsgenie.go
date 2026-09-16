package notifiers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/config"
)

// Opsgenie creates/closes alerts via the v2 alerts API.
type Opsgenie struct{}

func (Opsgenie) Kind() string { return "opsgenie" }

const defaultOpsgenieURL = "https://api.opsgenie.com"

func (Opsgenie) Send(ctx context.Context, a *alert.Alert, cfg any) error {
	c, ok := cfg.(config.OpsgenieConfig)
	if !ok {
		return errors.New("invalid opsgenie config")
	}
	if c.APIKey == "" {
		return errors.New("opsgenie api_key not configured")
	}
	base := c.APIURL
	if base == "" {
		base = defaultOpsgenieURL
	}
	headers := map[string]string{"Authorization": "GenieKey " + c.APIKey}
	alias := a.Chain + "|" + a.Key

	if a.Resolved {
		return postJSON(ctx, fmt.Sprintf("%s/v2/alerts/%s/close?identifierType=alias", base, url.PathEscape(alias)),
			headers, map[string]string{"source": "cometduty"})
	}
	body := map[string]any{
		"message":     fmt.Sprintf("%s: %s", a.Chain, a.Message),
		"alias":       alias,
		"description": a.Message,
		"source":      "cometduty",
		"priority":    opsgeniePriority(a.Severity),
		"tags":        []string{"cometduty", a.ChainID},
	}
	if c.Team != "" {
		body["responders"] = []map[string]string{{"type": "team", "name": c.Team}}
	}
	return postJSON(ctx, base+"/v2/alerts", headers, body)
}

func opsgeniePriority(sev string) string {
	switch sev {
	case "critical", "error":
		return "P1"
	case "warning":
		return "P3"
	default:
		return "P4"
	}
}

func (o Opsgenie) Test(ctx context.Context, cfg any) error {
	return o.Send(ctx, &alert.Alert{
		Chain: "test", ChainID: "test-1", Key: "test",
		Message:  "test alert from cometduty (this is not a real alert)",
		Severity: "info", Time: time.Now(),
	}, cfg)
}

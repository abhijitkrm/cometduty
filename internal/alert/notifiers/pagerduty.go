package notifiers

import (
	"context"
	"errors"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/config"
)

// PagerDuty uses the V2 Events API directly — a single POST, no SDK needed.
type PagerDuty struct{}

func (PagerDuty) Kind() string { return "pagerduty" }

// pdEventsURL is a var so tests can point it at a stub server.
var pdEventsURL = "https://events.pagerduty.com/v2/enqueue"

type pdEvent struct {
	RoutingKey  string    `json:"routing_key"`
	EventAction string    `json:"event_action"` // trigger | resolve
	DedupKey    string    `json:"dedup_key"`
	Payload     pdPayload `json:"payload,omitempty"`
}

type pdPayload struct {
	Summary  string `json:"summary"`
	Source   string `json:"source"`
	Severity string `json:"severity"` // critical | error | warning | info
}

func (PagerDuty) Send(ctx context.Context, a *alert.Alert, cfg any) error {
	c, ok := cfg.(config.PagerdutyConfig)
	if !ok {
		return errors.New("invalid pagerduty config")
	}
	if c.APIKey == "" {
		return errors.New("pagerduty api_key not configured")
	}
	ev := pdEvent{
		RoutingKey: c.APIKey,
		DedupKey:   a.Chain + "|" + a.Key,
	}
	if a.Resolved {
		ev.EventAction = "resolve"
	} else {
		ev.EventAction = "trigger"
		sev := a.Severity
		if sev == "" {
			sev = c.DefaultSeverity
		}
		if sev == "" {
			sev = "error"
		}
		ev.Payload = pdPayload{Summary: a.Message, Source: a.Chain, Severity: sev}
	}
	return postJSON(ctx, pdEventsURL, nil, ev)
}

func (p PagerDuty) Test(ctx context.Context, cfg any) error {
	return p.Send(ctx, &alert.Alert{
		Chain: "test", ChainID: "test-1", Key: "test",
		Message:  "test alert from cometduty (this is not a real alert)",
		Severity: "info", Time: time.Now(),
	}, cfg)
}

package notifiers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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

// PayloadVersion is the version of the default webhook body. Fields are only
// ever added within a version; receivers should ignore ones they don't know.
const PayloadVersion = "1"

// Payload is the default webhook body (no template_body): the alert as a
// stable JSON document any receiver can act on — docs/WEBHOOK.md.
type Payload struct {
	Version  string            `json:"version"`
	Source   string            `json:"source"` // always "cometduty"
	Status   string            `json:"status"` // firing | resolved
	Key      string            `json:"key"`    // pairs a raise with its resolve
	Kind     string            `json:"kind"`   // node-down, stalled, signer-regressed, …
	Target   string            `json:"target"` // node URL, valcons, valoper or chain id
	Severity string            `json:"severity"`
	Chain    string            `json:"chain"`
	ChainID  string            `json:"chain_id"`
	Node     string            `json:"node,omitempty"`
	Moniker  string            `json:"moniker"`
	Valcons  string            `json:"valcons"`
	Labels   map[string]string `json:"labels,omitempty"`
	Message  string            `json:"message"`
	Resolved bool              `json:"resolved"`
	Reminder bool              `json:"reminder,omitempty"`
	Time     string            `json:"time"`
}

// NewPayload builds the default body for an alert.
func NewPayload(a *alert.Alert) Payload {
	status := "firing"
	if a.Resolved {
		status = "resolved"
	}
	return Payload{
		Version: PayloadVersion, Source: "cometduty", Status: status,
		Key: a.Key, Kind: a.Kind, Target: a.Target, Severity: a.Severity,
		Chain: a.Chain, ChainID: a.ChainID, Node: a.Node, Moniker: a.Moniker,
		Valcons: a.Valcons, Labels: a.Labels, Message: a.Message,
		Resolved: a.Resolved, Reminder: a.Reminder,
		Time: a.Time.Format(time.RFC3339),
	}
}

// RenderBody renders the configured template, or the default JSON payload.
func RenderBody(tpl string, a *alert.Alert) ([]byte, error) {
	if tpl == "" {
		return json.Marshal(NewPayload(a))
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

// Signature headers, set when the webhook has a secret. The signature is
// HMAC-SHA256 over "<timestamp>.<body>", so a receiver can refuse replays.
const (
	TimestampHeader = "X-Cometduty-Timestamp"
	SignatureHeader = "X-Cometduty-Signature"
)

// Sign returns the signature header value for a body sent at ts.
func Sign(secret string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
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
	headers := c.Headers
	if c.Secret != "" {
		ts := time.Now().Unix()
		headers = map[string]string{}
		for k, v := range c.Headers {
			headers[k] = v
		}
		headers[TimestampHeader] = strconv.FormatInt(ts, 10)
		headers[SignatureHeader] = Sign(c.Secret, ts, body)
	}
	return post(ctx, c.URL, "application/json", headers, body)
}

func (w Webhook) Test(ctx context.Context, cfg any) error {
	return w.Send(ctx, &alert.Alert{
		Chain: "test", ChainID: "test-1", Key: "test", Kind: "test",
		Message:  "test alert from cometduty (this is not a real alert)",
		Severity: "info", Time: time.Now(),
	}, cfg)
}

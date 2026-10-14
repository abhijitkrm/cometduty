package notifiers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/config"
)

type captured struct {
	method      string
	path        string
	body        []byte
	contentType string
	header      http.Header
}

// stub returns a server that records requests.
func stub(t *testing.T) (*httptest.Server, *captured) {
	t.Helper()
	cap := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.path = r.URL.Path
		cap.contentType = r.Header.Get("Content-Type")
		cap.header = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		cap.body = b
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func testAlert() *alert.Alert {
	return &alert.Alert{
		Chain: "cosmoshub", ChainID: "cosmoshub-4", Key: "missed:ABCD",
		Message: "validator missed 5 blocks", Severity: "critical",
		Moniker: "myval", Time: time.Now(),
	}
}

func TestSlackPayloadAndMentions(t *testing.T) {
	srv, cap := stub(t)
	n := Slack{}
	err := n.Send(context.Background(), testAlert(), config.SlackConfig{
		Webhook: srv.URL, Mentions: []string{"<@U123>", "<@U456>"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var p slackPayload
	if err := json.Unmarshal(cap.body, &p); err != nil {
		t.Fatalf("bad json: %v\n%s", err, cap.body)
	}
	if !strings.Contains(p.Text, "<@U123>") || !strings.Contains(p.Text, "<@U456>") {
		t.Errorf("mentions missing from text: %q", p.Text)
	}
	if len(p.Attachments) != 1 || p.Attachments[0].Color != "danger" {
		t.Errorf("attachment: %+v", p.Attachments)
	}
}

func TestSlackResolved(t *testing.T) {
	srv, cap := stub(t)
	a := testAlert()
	a.Resolved = true
	if err := (Slack{}).Send(context.Background(), a, config.SlackConfig{Webhook: srv.URL}); err != nil {
		t.Fatal(err)
	}
	var p slackPayload
	_ = json.Unmarshal(cap.body, &p)
	if p.Attachments[0].Color != "good" {
		t.Errorf("resolved color: %s", p.Attachments[0].Color)
	}
}

func TestDiscordMentionsInContent(t *testing.T) {
	srv, cap := stub(t)
	err := (Discord{}).Send(context.Background(), testAlert(), config.DiscordConfig{
		Webhook: srv.URL, Mentions: []string{"<@123>"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var p discordPayload
	if err := json.Unmarshal(cap.body, &p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Content, "<@123>") {
		t.Errorf("mention not in content: %q", p.Content)
	}
	if len(p.Embeds) != 1 || !strings.Contains(p.Embeds[0].Description, "missed 5 blocks") {
		t.Errorf("embed: %+v", p.Embeds)
	}
}

func TestWebhookDefaultBody(t *testing.T) {
	srv, cap := stub(t)
	if err := (Webhook{}).Send(context.Background(), testAlert(), config.WebhookConfig{URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(cap.body, &m); err != nil {
		t.Fatalf("default body not json: %v\n%s", err, cap.body)
	}
	if m["chain"] != "cosmoshub" || m["resolved"] != false || m["severity"] != "critical" {
		t.Errorf("payload: %v", m)
	}
}

func TestWebhookCustomTemplate(t *testing.T) {
	srv, cap := stub(t)
	cfg := config.WebhookConfig{
		URL:          srv.URL,
		TemplateBody: `{"text":"{{.Chain}} {{.Message}}"}`,
		Headers:      map[string]string{"X-Custom": "yes"},
	}
	if err := (Webhook{}).Send(context.Background(), testAlert(), cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cap.body), "cosmoshub validator missed 5 blocks") {
		t.Errorf("template output: %s", cap.body)
	}
	if cap.header.Get("X-Custom") != "yes" {
		t.Error("custom header not sent")
	}
}

func TestNtfyHeadersAndPath(t *testing.T) {
	srv, cap := stub(t)
	cfg := config.NtfyConfig{
		URL: srv.URL, Topic: "mytopic", Priority: "urgent", Token: "tk",
	}
	if err := (Ntfy{}).Send(context.Background(), testAlert(), cfg); err != nil {
		t.Fatal(err)
	}
	if cap.path != "/mytopic" {
		t.Errorf("path %q", cap.path)
	}
	if cap.header.Get("Authorization") != "Bearer tk" {
		t.Error("token header missing")
	}
	if cap.header.Get("Priority") != "urgent" {
		t.Errorf("priority %q", cap.header.Get("Priority"))
	}
	if !strings.Contains(cap.header.Get("Title"), "cosmoshub") {
		t.Errorf("title %q", cap.header.Get("Title"))
	}
	if string(cap.body) != "validator missed 5 blocks" {
		t.Errorf("body %q", cap.body)
	}
}

func TestPagerDutyTriggerAndResolve(t *testing.T) {
	srv, cap := stub(t)
	old := pdEventsURL
	pdEventsURL = srv.URL
	defer func() { pdEventsURL = old }()

	cfg := config.PagerdutyConfig{APIKey: "routingkey123"}
	if err := (PagerDuty{}).Send(context.Background(), testAlert(), cfg); err != nil {
		t.Fatal(err)
	}
	var ev pdEvent
	if err := json.Unmarshal(cap.body, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.EventAction != "trigger" || ev.RoutingKey != "routingkey123" {
		t.Errorf("event: %+v", ev)
	}
	if ev.DedupKey != "cosmoshub|missed:ABCD" {
		t.Errorf("dedup key %q", ev.DedupKey)
	}
	if ev.Payload.Severity != "critical" {
		t.Errorf("severity %q", ev.Payload.Severity)
	}

	a := testAlert()
	a.Resolved = true
	if err := (PagerDuty{}).Send(context.Background(), a, cfg); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(cap.body, &ev)
	if ev.EventAction != "resolve" || ev.DedupKey != "cosmoshub|missed:ABCD" {
		t.Errorf("resolve event: %+v", ev)
	}
}

func TestOpsgenieCreateAndClose(t *testing.T) {
	srv, cap := stub(t)
	cfg := config.OpsgenieConfig{APIKey: "geniekey", APIURL: srv.URL, Team: "oncall"}

	if err := (Opsgenie{}).Send(context.Background(), testAlert(), cfg); err != nil {
		t.Fatal(err)
	}
	if cap.path != "/v2/alerts" {
		t.Errorf("create path %q", cap.path)
	}
	if cap.header.Get("Authorization") != "GenieKey geniekey" {
		t.Error("auth header wrong")
	}
	var m map[string]any
	_ = json.Unmarshal(cap.body, &m)
	if m["priority"] != "P1" || m["alias"] != "cosmoshub|missed:ABCD" {
		t.Errorf("body: %v", m)
	}

	a := testAlert()
	a.Resolved = true
	if err := (Opsgenie{}).Send(context.Background(), a, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cap.path, "/v2/alerts/") || !strings.HasSuffix(cap.path, "/close") {
		t.Errorf("close path %q", cap.path)
	}
}

func TestNotifierErrorsOnMissingConfig(t *testing.T) {
	ctx := context.Background()
	a := testAlert()
	if err := (Slack{}).Send(ctx, a, config.SlackConfig{}); err == nil {
		t.Error("slack without webhook succeeded")
	}
	if err := (Discord{}).Send(ctx, a, config.DiscordConfig{}); err == nil {
		t.Error("discord without webhook succeeded")
	}
	if err := (Webhook{}).Send(ctx, a, config.WebhookConfig{}); err == nil {
		t.Error("webhook without url succeeded")
	}
	if err := (Ntfy{}).Send(ctx, a, config.NtfyConfig{}); err == nil {
		t.Error("ntfy without topic succeeded")
	}
	if err := (PagerDuty{}).Send(ctx, a, config.PagerdutyConfig{}); err == nil {
		t.Error("pagerduty without key succeeded")
	}
	if err := (Opsgenie{}).Send(ctx, a, config.OpsgenieConfig{}); err == nil {
		t.Error("opsgenie without key succeeded")
	}
	// wrong config type
	if err := (Slack{}).Send(ctx, a, config.DiscordConfig{Webhook: "x"}); err == nil {
		t.Error("slack accepted discord config")
	}
}

func TestNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()
	err := (Webhook{}).Send(context.Background(), testAlert(), config.WebhookConfig{URL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "418") {
		t.Errorf("non-2xx not an error: %v", err)
	}
}

func TestWebhookPayloadV1(t *testing.T) {
	srv, cap := stub(t)
	a := testAlert()
	a.Kind, a.Target, a.Node = "consecutive", "ABCD", "val1"
	a.Labels = map[string]string{"profile": "val1"}
	if err := (Webhook{}).Send(context.Background(), a, config.WebhookConfig{URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	var p Payload
	if err := json.Unmarshal(cap.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Version != "1" || p.Source != "cometduty" || p.Status != "firing" || p.Key != "missed:ABCD" ||
		p.Kind != "consecutive" || p.Node != "val1" || p.Labels["profile"] != "val1" {
		t.Errorf("payload: %+v", p)
	}
	if cap.header.Get(SignatureHeader) != "" {
		t.Error("signed without a secret")
	}
}

func TestWebhookSignature(t *testing.T) {
	srv, cap := stub(t)
	if err := (Webhook{}).Send(context.Background(), testAlert(), config.WebhookConfig{URL: srv.URL, Secret: "s3cret"}); err != nil {
		t.Fatal(err)
	}
	ts, err := strconv.ParseInt(cap.header.Get(TimestampHeader), 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)) > time.Minute {
		t.Fatalf("timestamp header: %q", cap.header.Get(TimestampHeader))
	}
	if got, want := cap.header.Get(SignatureHeader), Sign("s3cret", ts, cap.body); got != want {
		t.Errorf("signature %q, want %q", got, want)
	}
	if Sign("other", ts, cap.body) == cap.header.Get(SignatureHeader) {
		t.Error("signature does not depend on the secret")
	}
}

func TestTelegramText(t *testing.T) {
	a := &alert.Alert{ChainID: "primium-6", Kind: "node-down", Target: "http://127.0.0.1:26657", Node: "v6-arch", Severity: "critical",
		Message: "v6-arch (http://127.0.0.1:26657) has been down for 3 minutes"}
	want := "🔴 <b>CRITICAL · Node down</b>\n<b>v6-arch</b> · primium-6\n\nv6-arch has been down for 3 minutes\n@ops"
	if got := telegramText(a, []string{"@ops"}); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	a = &alert.Alert{ChainID: "primium-6", Kind: "evm-down", Target: "http://127.0.0.1:8545", Labels: map[string]string{"profile": "v6-val2"},
		Severity: "info", Resolved: true, Message: "EVM RPC http://127.0.0.1:8545 recovered on primium-6 <ok>"}
	want = "✅ <b>RESOLVED · EVM RPC down</b>\n<b>v6-val2</b> · primium-6\n\nEVM RPC v6-val2:8545 recovered on primium-6 &lt;ok&gt;"
	if got := telegramText(a, []string{"@ops"}); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	a = &alert.Alert{ChainID: "c-1", Kind: "no-nodes", Severity: "critical", Message: "no endpoint answers (tcp://localhost:26657)"}
	if got := telegramText(a, nil); !strings.Contains(got, "No reachable nodes</b>\n<b>c-1</b>\n\nno endpoint answers (localhost:26657)") {
		t.Fatalf("chain alert:\n%s", got)
	}
}

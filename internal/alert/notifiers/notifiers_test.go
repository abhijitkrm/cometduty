package notifiers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// The archive outage seen live: four conditions on one node, then the
// chain instance's own view. One card per node, edited; a reply when it
// recovers.
func TestTelegramIncidentCards(t *testing.T) {
	type call struct {
		method string
		body   map[string]any
	}
	var mu sync.Mutex
	var calls []call
	next := 100
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		defer mu.Unlock()
		m := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		calls = append(calls, call{m, b})
		next++
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":` + strconv.Itoa(next) + `}}`))
	}))
	defer srv.Close()
	old := tgAPI
	tgAPI = func(token, method string) string { return srv.URL + "/bot" + token + "/" + method }
	defer func() { tgAPI = old }()

	path := filepath.Join(t.TempDir(), "cards.json")
	tg := NewTelegram(path)
	cfg := config.TelegramConfig{Enabled: true, APIKey: "tok", Channel: "-100", Mentions: []string{"@ops"}}
	t0 := time.Now().Add(-10 * time.Minute)
	send := func(a alert.Alert) {
		t.Helper()
		if err := tg.Send(context.Background(), &a, cfg); err != nil {
			t.Fatal(err)
		}
	}
	node := func(key, kind, sev, msg string, resolved bool, at time.Duration) alert.Alert {
		return alert.Alert{ChainID: "primium-6", Key: key, Kind: kind, Target: "http://127.0.0.1:26657", Node: "v6-arch",
			Severity: sev, Message: msg, Resolved: resolved, Time: t0.Add(at)}
	}
	var wg sync.WaitGroup // raised in the same instant, sent concurrently
	for _, a := range []alert.Alert{
		node("node-down:http://127.0.0.1:26657", "node-down", "critical", "RPC node v6-arch down for > 3 minutes on primium-6: dial tcp 127.0.0.1:26657: connect: connection refused", false, 0),
		node("evm-down:http://127.0.0.1:8545", "evm-down", "warning", "EVM RPC http://127.0.0.1:8545 down for > 3 minutes", false, 0),
	} {
		wg.Add(1)
		go func(a alert.Alert) { defer wg.Done(); send(a) }(a)
	}
	wg.Wait()
	send(node("catching-up:http://127.0.0.1:26657", "catching-up", "warning", "RPC node v6-arch is catching up", false, 3*time.Minute))
	send(node("node-down:http://127.0.0.1:26657", "node-down", "info", "RPC node v6-arch reachable again", true, 3*time.Minute))
	send(node("evm-down:http://127.0.0.1:8545", "evm-down", "info", "recovered", true, 3*time.Minute))
	// a restart forgets nothing: the card is found again from the state file
	tg = NewTelegram(path)
	send(node("catching-up:http://127.0.0.1:26657", "catching-up", "info", "finished syncing", true, 4*time.Minute))
	send(alert.Alert{ChainID: "primium-6", Key: "no-nodes:primium-6", Kind: "no-nodes", Severity: "critical", Message: "no usable endpoint", Time: t0})

	var methods []string
	for _, c := range calls {
		methods = append(methods, c.method)
	}
	want := "sendMessage editMessageText editMessageText editMessageText editMessageText editMessageText sendMessage sendMessage"
	if strings.Join(methods, " ") != want {
		t.Fatalf("calls:\n%s\nwant\n%s", strings.Join(methods, " "), want)
	}
	first := calls[0].body["text"].(string)
	if strings.Contains(first, "127.0.0.1") || !strings.Contains(first, "<blockquote expandable>") || !strings.Contains(first, "@ops") {
		t.Fatalf("first card:\n%s", first)
	}
	// the second alert joined the card, which leads with the worse one
	both := calls[1].body["text"].(string)
	if !strings.HasPrefix(both, "<b>v6-arch is down</b>\ncritical · primium-6\n\nThe node stopped answering RPC") ||
		!strings.Contains(both, "Also: the node's EVM JSON-RPC isn't answering.") || !strings.Contains(both, "v6-arch:8545 down") ||
		strings.Contains(both, "127.0.0.1") {
		t.Fatalf("joined card:\n%s", both)
	}
	if calls[2].body["message_id"] != calls[1].body["message_id"] {
		t.Fatal("edited another message")
	}
	if last := calls[5].body["text"].(string); !strings.HasPrefix(last, "<b>v6-arch recovered</b>\nprimium-6 · all clear after 4m") || strings.Contains(last, "@ops") {
		t.Fatalf("recovered card:\n%s", last)
	}
	if r := calls[6].body; r["reply_to_message_id"] != calls[0].body["message_id"] && r["reply_to_message_id"] == nil ||
		!strings.Contains(r["text"].(string), "v6-arch recovered") {
		t.Fatalf("recovery reply: %v", r)
	}
	if c := calls[7].body["text"].(string); !strings.HasPrefix(c, "<b>primium-6: no node reachable</b>") {
		t.Fatalf("chain card:\n%s", c)
	}
	for _, c := range calls {
		if s, _ := c.body["text"].(string); regexp.MustCompile(`[\x{1F300}-\x{1FAFF}\x{2600}-\x{27BF}]`).MatchString(s) {
			t.Fatalf("emoji in %q", s)
		}
	}
}

func TestTelegramWorseConditionIsAnnounced(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer srv.Close()
	old := tgAPI
	tgAPI = func(token, method string) string { return srv.URL + "/" + method }
	defer func() { tgAPI = old }()
	tg := NewTelegram("")
	cfg := config.TelegramConfig{APIKey: "tok", Channel: "-100"}
	for _, a := range []alert.Alert{
		{ChainID: "c-1", Node: "val1", Key: "catching-up:x", Kind: "catching-up", Severity: "warning", Time: time.Now().Add(-10 * time.Minute)},
		{ChainID: "c-1", Node: "val1", Key: "signer-regressed:x", Kind: "signer-regressed", Severity: "critical", Time: time.Now()},
		{ChainID: "c-1", Node: "val1", Key: "catching-up:x", Kind: "catching-up", Severity: "warning", Reminder: true},
	} {
		if err := tg.Send(context.Background(), &a, cfg); err != nil {
			t.Fatal(err)
		}
	}
	// new card; worse condition: a reply (sound) + the edit; reminder: a reply
	if got := strings.Join(methods, " "); got != "sendMessage sendMessage editMessageText sendMessage" {
		t.Fatalf("calls %s", got)
	}
}

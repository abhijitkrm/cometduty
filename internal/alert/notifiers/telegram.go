package notifiers

import (
	"context"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/config"
)

// Telegram posts via a bot to a channel/group.
type Telegram struct{}

var tgAPIEndpoint = tgbotapi.APIEndpoint

func (Telegram) Kind() string { return "telegram" }

func (Telegram) Send(_ context.Context, a *alert.Alert, cfg any) error {
	c, ok := cfg.(config.TelegramConfig)
	if !ok {
		return errors.New("invalid telegram config")
	}
	if c.APIKey == "" || c.Channel == "" {
		return errors.New("telegram api_key/channel not configured")
	}
	// tgAPIEndpoint is a var so tests can point it at a stub server.
	bot, err := tgbotapi.NewBotAPIWithClient(c.APIKey, tgAPIEndpoint, httpClient)
	if err != nil {
		return err
	}
	mc := tgbotapi.NewMessageToChannel(c.Channel, telegramText(a, c.Mentions))
	mc.ParseMode = tgbotapi.ModeHTML
	mc.DisableWebPagePreview = true
	if _, err = bot.Send(mc); err != nil && strings.Contains(err.Error(), "can't parse entities") {
		mc.ParseMode, mc.Text = "", html.UnescapeString(htmlTag.ReplaceAllString(mc.Text, ""))
		_, err = bot.Send(mc)
	}
	return err
}

// kindTitles name alert kinds for people; others are spelled out.
var kindTitles = map[string]string{
	"node-down": "Node down", "no-nodes": "No reachable nodes", "no-servers": "No reachable nodes",
	"stalled": "Chain stalled", "lag": "Node lagging", "consecutive": "Missed blocks in a row",
	"window-pct": "Missing blocks", "percentage": "Missing blocks", "inactive": "Validator inactive",
	"evm-down": "EVM RPC down", "evm-lag": "EVM lagging", "evm-txpool": "EVM txpool backlog",
	"mempool-txs": "Mempool backlog", "consensus-round": "Consensus rounds climbing",
	"catching-up": "Node catching up", "catching-up-stuck": "Node stuck catching up",
	"validator-new": "Validator joined", "validator-gone": "Validator left", "validator-jailed": "Validator jailed",
	"stake-change": "Stake changed", "cpu-high": "CPU high", "mem-high": "Memory high", "disk-low": "Disk low",
	"signer-state": "Signer state unreadable", "signer-stalled": "Signer stalled", "signer-regressed": "Signer went backwards",
}

var (
	htmlTag  = regexp.MustCompile(`<[^>]+>`)
	loopback = regexp.MustCompile(`(?:https?|tcp|wss?)://(?:127\.0\.0\.1|localhost|\[::1\]|0\.0\.0\.0)(:\d+)?(?:/[^\s)]*)?`)
)

// telegramText is an alert as a Telegram card (HTML): what and how bad,
// which node on which chain, then the message, with loopback URLs —
// meaningless to someone reading elsewhere — named by the node instead.
func telegramText(a *alert.Alert, mentions []string) string {
	title := kindTitles[a.Kind]
	if strings.HasPrefix(a.Kind, "log-") {
		title = "Log: " + strings.TrimPrefix(a.Kind, "log-")
	}
	if title == "" && a.Kind != "" {
		title = strings.ToUpper(a.Kind[:1]) + strings.ReplaceAll(a.Kind[1:], "-", " ")
	}
	if title == "" {
		title = "Alert"
	}
	icon, level := "🔵", strings.ToUpper(a.Severity)
	switch {
	case a.Resolved:
		icon, level = "✅", "RESOLVED"
	case a.Severity == "critical":
		icon = "🔴"
	case a.Severity == "warning":
		icon = "🟠"
	}
	if level == "" {
		level = "ALERT"
	}
	esc := html.EscapeString
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s · %s</b>\n", icon, level, esc(title))
	node := a.Node
	if node == "" {
		node = a.Labels["profile"]
	}
	chain := a.ChainID
	if chain == "" {
		chain = a.Chain
	}
	switch {
	case node != "" && chain != "":
		fmt.Fprintf(&b, "<b>%s</b> · %s\n", esc(node), esc(chain))
	case node != "" || chain != "":
		fmt.Fprintf(&b, "<b>%s</b>\n", esc(node+chain))
	}
	msg := a.Message
	if node != "" {
		msg = strings.ReplaceAll(msg, " ("+a.Target+")", "") // "v6-arch (http://127.0.0.1:26657)"
	}
	msg = loopback.ReplaceAllStringFunc(msg, func(u string) string {
		port := loopback.FindStringSubmatch(u)[1]
		if node != "" {
			return node + port
		}
		return "localhost" + port
	})
	fmt.Fprintf(&b, "\n%s", esc(msg))
	if a.Reminder {
		b.WriteString("\n<i>still open</i>")
	}
	if len(mentions) > 0 && !a.Resolved {
		fmt.Fprintf(&b, "\n%s", esc(strings.Join(mentions, " ")))
	}
	return b.String()
}

func (t Telegram) Test(ctx context.Context, cfg any) error {
	return t.Send(ctx, &alert.Alert{
		Chain: "test", ChainID: "test-1", Key: "test",
		Message:  "test alert from cometduty (this is not a real alert)",
		Severity: "info", Time: time.Now(),
	}, cfg)
}

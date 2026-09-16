package notifiers

import (
	"context"
	"errors"
	"fmt"
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
	prefix := "🚨 ALERT"
	if a.Resolved {
		prefix = "💜 RESOLVED"
	}
	mentions := strings.Join(c.Mentions, " ")
	text := fmt.Sprintf("%s: %s — %s %s", prefix, a.Chain, a.Message, mentions)
	mc := tgbotapi.NewMessageToChannel(c.Channel, strings.TrimSpace(text))
	_, err = bot.Send(mc)
	return err
}

func (t Telegram) Test(ctx context.Context, cfg any) error {
	return t.Send(ctx, &alert.Alert{
		Chain: "test", ChainID: "test-1", Key: "test",
		Message:  "test alert from cometduty (this is not a real alert)",
		Severity: "info", Time: time.Now(),
	}, cfg)
}

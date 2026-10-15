package notifiers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/config"
)

// Telegram posts each incident as one card that is edited as it changes,
// instead of a message per raise and resolve:
//
//   - alerts about the same node share its card (a stopped node is down,
//     its EVM RPC is down and then it catches up: one card, not six posts);
//     alerts about no node get a card each
//   - a resolve edits the card; when the last condition clears the card
//     says so and a short reply marks the recovery, so the chat shows it
//   - a condition worse than the card's current one, and reminders, are
//     replies to the card (edits make no sound)
//
// Every card has a plain-language reason and, collapsed, the raw messages.
// Card message ids are kept in StatePath (when set) to survive restarts.
type Telegram struct {
	StatePath string

	mu    sync.Mutex
	cards map[string]*tgCard
	load  sync.Once
}

// NewTelegram keeps its cards in path ("" = memory only).
func NewTelegram(path string) *Telegram { return &Telegram{StatePath: path} }

type tgCard struct {
	MsgID   int       `json:"msg_id"`
	Channel string    `json:"channel"`
	Node    string    `json:"node,omitempty"`
	ChainID string    `json:"chain_id"`
	Opened  time.Time `json:"opened"`
	Conds   []tgCond  `json:"conds"`
}

type tgCond struct {
	Key      string    `json:"key"`
	Kind     string    `json:"kind"`
	Severity string    `json:"severity"`
	Raw      string    `json:"raw"`
	Since    time.Time `json:"since"`
	Cleared  time.Time `json:"cleared,omitempty"`
}

// burst: conditions raised this soon after a card opened are one event
// (a stopped node trips several checks at once) and only edit the card.
const burst = 2 * time.Minute

// tgAPI is the Bot API URL for a token and method; a var so tests can stub it.
var tgAPI = func(token, method string) string {
	return "https://api.telegram.org/bot" + token + "/" + method
}

func (*Telegram) Kind() string { return "telegram" }

func (t *Telegram) Send(ctx context.Context, a *alert.Alert, cfg any) error {
	c, ok := cfg.(config.TelegramConfig)
	if !ok {
		return errors.New("invalid telegram config")
	}
	if c.APIKey == "" || c.Channel == "" {
		return errors.New("telegram api_key/channel not configured")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.load.Do(t.restore)

	node := alertNode(a)
	gk := c.Channel + "|" + a.ChainID + "|" + node
	if node == "" {
		gk = c.Channel + "|" + a.Key // no node: a card per condition
	}
	card := t.cards[gk]
	now := a.Time
	if now.IsZero() {
		now = time.Now()
	}

	if a.Reminder {
		if card == nil {
			return nil
		}
		return t.reply(ctx, c, card, fmt.Sprintf("<b>%s</b>\nstill open after %s", tgEsc(headline(card, false)), dur(now.Sub(card.Opened))))
	}
	if a.Resolved {
		if card == nil || card.open(a.Key) < 0 {
			return nil // never shown, or already cleared
		}
		card.Conds[card.open(a.Key)].Cleared = now
		if err := t.edit(ctx, c, card); err != nil {
			return err
		}
		if card.allClear() {
			delete(t.cards, gk)
			err := t.reply(ctx, c, card, fmt.Sprintf("<b>%s</b>\n%s", tgEsc(headline(card, false)), tgEsc(recoveryLine(card))))
			t.save()
			return err
		}
		t.save()
		return nil
	}

	cond := tgCond{Key: a.Key, Kind: a.Kind, Severity: a.Severity, Raw: humanize(a.Message, a.Target, node), Since: now}
	if card == nil {
		card = &tgCard{Channel: c.Channel, Node: node, ChainID: chainOf(a), Opened: now, Conds: []tgCond{cond}}
		id, err := t.post(ctx, c, render(card, c.Mentions), 0)
		if err != nil {
			return err
		}
		card.MsgID = id
		if t.cards == nil {
			t.cards = map[string]*tgCard{}
		}
		t.cards[gk] = card
		t.save()
		return nil
	}
	if i := card.open(a.Key); i >= 0 {
		card.Conds[i] = cond // re-raised with a newer message
	} else {
		// worse, and not part of the burst that opened the card: announce
		worse := rank(a.Severity) > card.worst() && now.Sub(card.Opened) > burst
		card.Conds = append(card.Conds, cond)
		if worse {
			// edits are silent: say it out loud
			if err := t.reply(ctx, c, card, fmt.Sprintf("<b>%s</b>\n%s", tgEsc(title(cond.Kind, card)), tgEsc(plain(cond.Kind)))); err != nil {
				return err
			}
		}
	}
	err := t.edit(ctx, c, card)
	t.save()
	return err
}

func (t *Telegram) Test(ctx context.Context, cfg any) error {
	return (&Telegram{}).Send(ctx, &alert.Alert{
		Chain: "test", ChainID: "test-1", Key: "test", Kind: "test",
		Message:  "test alert from cometduty (this is not a real alert)",
		Severity: "info", Time: time.Now(),
	}, cfg)
}

// --- card state ---

func (c *tgCard) open(key string) int {
	for i, x := range c.Conds {
		if x.Key == key && x.Cleared.IsZero() {
			return i
		}
	}
	return -1
}

func (c *tgCard) allClear() bool {
	for _, x := range c.Conds {
		if x.Cleared.IsZero() {
			return false
		}
	}
	return true
}

func (c *tgCard) worst() int {
	w := 0
	for _, x := range c.Conds {
		if x.Cleared.IsZero() && rank(x.Severity) > w {
			w = rank(x.Severity)
		}
	}
	return w
}

// lead is the condition the card is about: the worst open one (the
// earliest among equals), or the first when everything has cleared.
func (c *tgCard) lead() tgCond {
	best := -1
	for i, x := range c.Conds {
		if x.Cleared.IsZero() && (best < 0 || rank(x.Severity) > rank(c.Conds[best].Severity)) {
			best = i
		}
	}
	if best < 0 {
		return c.Conds[0]
	}
	return c.Conds[best]
}

func rank(sev string) int {
	switch sev {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	}
	return 0
}

func (t *Telegram) restore() {
	t.cards = map[string]*tgCard{}
	if t.StatePath == "" {
		return
	}
	if b, err := os.ReadFile(t.StatePath); err == nil {
		_ = json.Unmarshal(b, &t.cards)
	}
	for k, c := range t.cards {
		if time.Since(c.Opened) > 7*24*time.Hour {
			delete(t.cards, k) // one-shot alerts never resolve
		}
	}
}

func (t *Telegram) save() {
	if t.StatePath == "" {
		return
	}
	if b, err := json.Marshal(t.cards); err == nil {
		_ = os.WriteFile(t.StatePath, b, 0o600)
	}
}

// --- rendering ---

func alertNode(a *alert.Alert) string {
	if a.Node != "" {
		return a.Node
	}
	return a.Labels["profile"]
}

func chainOf(a *alert.Alert) string {
	if a.ChainID != "" {
		return a.ChainID
	}
	return a.Chain
}

// kinds: a headline (with %s = the node, or the chain) and one plain
// sentence on what it means.
var kinds = map[string][2]string{
	"node-down":         {"%s is down", "The node stopped answering RPC, so it can't follow the chain or serve requests."},
	"no-nodes":          {"%s: no node reachable", "cometduty can't reach any node it watches, so it can't see the chain."},
	"no-servers":        {"%s: no node reachable", "cometduty can't reach any node it watches, so it can't see the chain."},
	"stalled":           {"%s has stalled", "No new block has been produced for several minutes."},
	"lag":               {"%s is falling behind", "The node is well behind the chain's latest block."},
	"consecutive":       {"%s is missing blocks", "The validator hasn't signed several blocks in a row."},
	"window-pct":        {"%s is missing blocks", "The validator missed too many blocks in the slashing window and is heading for jail."},
	"percentage":        {"%s is missing blocks", "The validator missed too many blocks in the slashing window and is heading for jail."},
	"inactive":          {"%s is not in the active set", "The validator is jailed, tombstoned or unbonded."},
	"evm-down":          {"%s: EVM RPC down", "The node's EVM JSON-RPC isn't answering."},
	"evm-lag":           {"%s: EVM lagging", "EVM execution is behind consensus: blocks finalize but transactions aren't executing."},
	"evm-txpool":        {"%s: EVM txpool backlog", "Many transactions are queued in the EVM txpool: a nonce gap or an execution stall."},
	"mempool-txs":       {"%s: mempool backlog", "Transactions are piling up in the mempool."},
	"consensus-round":   {"%s: consensus rounds climbing", "Proposals are timing out and rounds keep going up."},
	"catching-up":       {"%s is catching up", "The node is syncing old blocks and isn't at the chain head yet."},
	"catching-up-stuck": {"%s is stuck syncing", "The node is syncing but its height isn't moving."},
	"validator-new":     {"%s: validator joined", "A validator joined the active set."},
	"validator-gone":    {"%s: validator left", "A validator left the active set."},
	"validator-jailed":  {"%s: validator jailed", "A validator was jailed."},
	"stake-change":      {"%s: stake changed", "The validator's bonded stake moved sharply."},
	"cpu-high":          {"%s: CPU high", "The node's CPU use is above the limit."},
	"mem-high":          {"%s: memory high", "The node's memory use is above the limit."},
	"disk-low":          {"%s: disk low", "The node's disk is nearly full; it will stop when it fills."},
	"disk-error":        {"%s: disk failing", "The node's disk can't be read: it failed, was unmounted or detached. The node will stop or crash."},
	"disk-inodes":       {"%s: inodes low", "The disk is running out of inodes (file slots); the node can't create files once they run out, even with space left."},
	"disk-filling":      {"%s: disk filling fast", "At the rate the disk filled over the last hour, it will be full soon."},
	"log-disk-full":     {"%s: disk full", "The node logged \"no space left on device\": its disk is full and it can't write blocks."},
	"log-disk-io-error": {"%s: disk I/O errors", "The node logged I/O errors: the disk is failing or was detached."},
	"log-read-only-fs":  {"%s: disk read-only", "The node's filesystem went read-only, usually after disk errors. The node can't write and will stop."},
	"log-db-corruption": {"%s: database corrupted", "The node logged database corruption. It needs a restore from a snapshot or a state sync."},
	"signer-state":      {"%s: signer state unreadable", "priv_validator_state.json can't be read; a restart could sign from height 0."},
	"signer-corrupt":    {"%s: signer state corrupt", "priv_validator_state.json is corrupt. Don't restart the validator on it: double-sign risk."},
	"signer-stalled":    {"%s: signer stalled", "The validator's signer state hasn't moved while the chain advances."},
	"signer-regressed":  {"%s: signer went backwards", "The signer state height moved backwards: state was copied or restored. Double-sign risk."},
	"test":              {"%s: test alert", "A test notification from cometduty."},
}

func title(kind string, c *tgCard) string {
	who := c.Node
	if who == "" {
		who = c.ChainID
	}
	if k, ok := kinds[kind]; ok {
		return fmt.Sprintf(k[0], who)
	}
	if strings.HasPrefix(kind, "log-") {
		return fmt.Sprintf("%s: log matched %s", who, strings.TrimPrefix(kind, "log-"))
	}
	if kind == "" {
		return who + ": alert"
	}
	return fmt.Sprintf("%s: %s", who, strings.ReplaceAll(kind, "-", " "))
}

func plain(kind string) string {
	if k, ok := kinds[kind]; ok {
		return k[1]
	}
	if strings.HasPrefix(kind, "log-") {
		return "A node log line matched the " + strings.TrimPrefix(kind, "log-") + " rule."
	}
	return ""
}

func headline(c *tgCard, _ bool) string {
	if c.allClear() {
		who := c.Node
		if who == "" {
			return title(c.Conds[0].Kind, c) + " — cleared"
		}
		return who + " recovered"
	}
	return title(c.lead().Kind, c)
}

func recoveryLine(c *tgCard) string {
	var last time.Time
	for _, x := range c.Conds {
		if x.Cleared.After(last) {
			last = x.Cleared
		}
	}
	return "all clear after " + dur(last.Sub(c.Opened))
}

// render is a card in Telegram HTML.
func render(c *tgCard, mentions []string) string {
	esc := tgEsc
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n", esc(headline(c, false)))
	if c.allClear() {
		fmt.Fprintf(&b, "%s · %s\n", esc(c.ChainID), esc(recoveryLine(c)))
	} else {
		lead := c.lead()
		fmt.Fprintf(&b, "%s · %s\n", esc(orDefault(lead.Severity, "alert")), esc(c.ChainID))
		if p := plain(lead.Kind); p != "" {
			fmt.Fprintf(&b, "\n%s\n", esc(p))
		}
		for _, x := range c.Conds {
			if x.Key != lead.Key && x.Cleared.IsZero() {
				fmt.Fprintf(&b, "Also: %s\n", esc(lowerFirst(strings.TrimSuffix(plain(x.Kind), "."))+"."))
			}
		}
	}
	conds := append([]tgCond(nil), c.Conds...)
	sort.SliceStable(conds, func(i, j int) bool { return conds[i].Since.Before(conds[j].Since) })
	b.WriteString("<blockquote expandable>")
	for i, x := range conds {
		if i > 0 {
			b.WriteString("\n")
		}
		state := "open " + dur(time.Since(x.Since))
		if !x.Cleared.IsZero() {
			state = "cleared after " + dur(x.Cleared.Sub(x.Since))
		}
		fmt.Fprintf(&b, "<b>%s</b> (%s, %s): %s", esc(x.Kind), esc(x.Severity), esc(state), esc(x.Raw))
	}
	b.WriteString("</blockquote>")
	if len(mentions) > 0 && !c.allClear() {
		fmt.Fprintf(&b, "\n%s", esc(strings.Join(mentions, " ")))
	}
	return b.String()
}

var loopback = regexp.MustCompile(`(?:(?:https?|tcp|wss?)://)?\b(?:127\.0\.0\.1|localhost|\[::1\]|0\.0\.0\.0)(:\d+)?(?:/[^\s)"]*)?`)

// humanize names loopback URLs — meaningless to whoever reads the chat —
// by the node they belong to.
func humanize(msg, target, node string) string {
	if node != "" && target != "" {
		msg = strings.ReplaceAll(msg, node+" ("+target+")", node)
	}
	return loopback.ReplaceAllStringFunc(msg, func(u string) string {
		port := loopback.FindStringSubmatch(u)[1]
		if node != "" {
			return node + port
		}
		return "localhost" + port
	})
}

func dur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// --- Bot API ---

func (t *Telegram) post(ctx context.Context, c config.TelegramConfig, text string, replyTo int) (int, error) {
	m := map[string]any{"chat_id": c.Channel, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true}
	if replyTo != 0 {
		m["reply_to_message_id"], m["allow_sending_without_reply"] = replyTo, true
	}
	var out struct {
		MessageID int `json:"message_id"`
	}
	err := tgCall(ctx, c.APIKey, "sendMessage", m, &out)
	if err != nil && strings.Contains(err.Error(), "can't parse entities") {
		m["text"], m["parse_mode"] = html.UnescapeString(htmlTag.ReplaceAllString(text, "")), ""
		err = tgCall(ctx, c.APIKey, "sendMessage", m, &out)
	}
	return out.MessageID, err
}

func (t *Telegram) reply(ctx context.Context, c config.TelegramConfig, card *tgCard, text string) error {
	_, err := t.post(ctx, c, text, card.MsgID)
	return err
}

func (t *Telegram) edit(ctx context.Context, c config.TelegramConfig, card *tgCard) error {
	text := render(card, c.Mentions)
	err := tgCall(ctx, c.APIKey, "editMessageText", map[string]any{
		"chat_id": c.Channel, "message_id": card.MsgID, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true,
	}, nil)
	switch {
	case err == nil, strings.Contains(err.Error(), "message is not modified"):
		return nil
	case strings.Contains(err.Error(), "message to edit not found"), strings.Contains(err.Error(), "message can't be edited"):
		id, err := t.post(ctx, c, text, 0) // the card is gone: start a new one
		card.MsgID = id
		return err
	}
	return err
}

var htmlTag = regexp.MustCompile(`<[^>]+>`)

// tgEsc escapes what Telegram's HTML mode requires (only &, < and >).
var tgEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace

func tgCall(ctx context.Context, token, method string, body map[string]any, out any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tgAPI(token, method), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return redactToken(err, token)
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: http %d", method, resp.StatusCode)
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// redactToken keeps the bot token (it is in the URL) out of error logs.
func redactToken(err error, token string) error {
	return errors.New(strings.ReplaceAll(err.Error(), token, "<token>"))
}

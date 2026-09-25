package monitor

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/abhijitkrm/cometduty/internal/config"
	"github.com/abhijitkrm/cometduty/internal/logs"
)

// logsLoop starts one log watcher per node with logs.enabled. Log alerts are
// node-local — the chain role never watches them.
func (c *Chain) logsLoop(ctx context.Context) {
	c.mu.Lock()
	chainRole := c.cfg.IsChainOnly()
	type work struct {
		n     *nodeState
		src   logs.Source
		rules []*logs.Rule
	}
	var ws []work
	for _, n := range c.nodes {
		lc := n.cfg.Logs
		if chainRole || lc == nil || !lc.Enabled {
			continue
		}
		rules, err := logs.Compile(append(append([]config.LogRule{}, logs.DefaultRules()...), lc.Rules...))
		if err != nil {
			c.log.Warn("bad log rules, using defaults", "node", nodeLabel(n), "err", err)
			rules, _ = logs.Compile(logs.DefaultRules())
		}
		src, err := logs.NewSource(lc)
		if err != nil {
			c.log.Warn("log source unusable", "node", nodeLabel(n), "err", err)
			continue
		}
		ws = append(ws, work{n, src, rules})
	}
	c.mu.Unlock()

	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		go func(w work) {
			defer wg.Done()
			c.watchLogs(ctx, w.n, w.src, w.rules)
		}(w)
	}
	wg.Wait()
}

// watchLogs streams a node's log forever, evaluating rules per line and
// reconnecting after stream failures with a fixed backoff.
func (c *Chain) watchLogs(ctx context.Context, n *nodeState, src logs.Source, rules []*logs.Rule) {
	c.log.Info("log watcher started", "node", nodeLabel(n), "source", src.String(), "rules", len(rules))
	for ctx.Err() == nil {
		out := make(chan string, 512)
		go func() {
			_ = src.Stream(ctx, out)
			close(out)
		}()
		for line := range out {
			c.evalLogLine(n, rules, line)
		}
		// stream ended — brief backoff then reconnect (sources resume from
		// their position, so reconnects don't replay history)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// levelRe pulls a severity hint out of the line — cometbft JSON logs
// (`"level":"error"`), text format (`level=error`), and the console
// short-form (`INF`, `ERR`, `DBG`, `WRN`, `FTL`, `PAN` — matched only in
// the line's first bytes where the level token sits after the timestamp).
var levelRe = regexp.MustCompile(`"level":"(trace|debug|info|warn|warning|error|fatal|panic)"|\blevel=(trace|debug|info|warn|warning|error|fatal|panic)\b`)

var consoleLevelRe = regexp.MustCompile(`^\S+\s+(INF|ERR|DBG|WRN|FTL|PAN)\b`)
var consoleLevel = map[string]string{
	"DBG": "debug", "INF": "info", "WRN": "warning",
	"ERR": "error", "FTL": "fatal", "PAN": "panic",
}

// evalLogLine runs every rule over one line; a firing rule alerts once then
// auto-resolves after its cooldown (log alerts are notify-once events, not
// latched conditions). ANSI escapes are stripped first — colored console
// logs wrap tokens (`module=<ESC>[0mevm`) and break pattern matching.
func (c *Chain) evalLogLine(n *nodeState, rules []*logs.Rule, line string) {
	now := time.Now()
	chainID := c.cfg.ChainID
	line = ansiRe.ReplaceAllString(line, "")
	level := ""
	if m := levelRe.FindStringSubmatch(line); m != nil {
		level = m[1]
		if level == "" {
			level = m[2]
		}
	}
	if level == "" {
		if m := consoleLevelRe.FindStringSubmatch(line); m != nil {
			level = consoleLevel[m[1]]
		}
	}
	if c.met != nil {
		c.met.LogLine(c.name, chainID, n.cfg.URL, n.cfg.Validator, level)
	}
	for _, r := range rules {
		if !r.Re.MatchString(line) || !r.Match(now) {
			continue
		}
		key := "log-" + r.Name + ":" + n.cfg.URL
		c.alert(chainID, nil, key, false, r.Severity,
			fmt.Sprintf("node %s log pattern %q fired on %s — %s", nodeLabel(n), r.Name, chainID, logSample(line)))
		if c.met != nil {
			c.met.LogMatch(c.name, chainID, n.cfg.URL, n.cfg.Validator, r.Name, r.Severity)
		}
		cooldown := r.Cooldown
		// auto-resolve once the cooldown elapses — the alert is an event,
		// the latch resolves so the open-alert set doesn't grow stale.
		time.AfterFunc(cooldown, func() {
			c.alert(chainID, nil, key, true, "info",
				fmt.Sprintf("node %s log pattern %q quieted on %s", nodeLabel(n), r.Name, chainID))
		})
	}
}

// logSample truncates a line for alert text — enough to identify, never the
// whole dump. Strips ANSI escapes so colored logs don't garble the message.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

func logSample(line string) string {
	s := strings.TrimSpace(ansiRe.ReplaceAllString(line, ""))
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

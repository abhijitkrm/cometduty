package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/alert/notifiers"
	"github.com/abhijitkrm/cometduty/internal/config"
	"github.com/abhijitkrm/cometduty/internal/history"
	"github.com/abhijitkrm/cometduty/internal/logging"
	"github.com/abhijitkrm/cometduty/internal/metrics"
	"github.com/abhijitkrm/cometduty/internal/monitor"
	"github.com/abhijitkrm/cometduty/internal/state"
)

// runMonitor is the default command: load config, start everything, serve until
// interrupted.
func runMonitor(cmd *cobra.Command) error {
	lvl := slog.LevelInfo
	if verbose {
		lvl = slog.LevelDebug
	}
	logging.Setup(lvl, jsonLogs)

	if password == "" {
		password = os.Getenv("PASSWORD")
	}
	cfg, err := config.Load(cfgFile, chainsDir, password)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	// wipe the password var now that config is loaded
	password = ""
	_ = os.Setenv("PASSWORD", "")

	fatal, problems := config.Validate(cfg)
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, p)
	}
	if fatal {
		return errors.New("configuration is invalid, refusing to start")
	}
	slog.Info("configuration OK", "chains", len(cfg.Chains))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- state restore ---
	store := state.Open(stateFile)
	snap := store.Load()

	// --- alert engine ---
	eng := alert.NewEngine(nil,
		time.Duration(cfg.FlapMinutes)*time.Minute,
		time.Duration(cfg.RemindMinutes)*time.Minute)
	for _, n := range []alert.Notifier{
		notifiers.Slack{}, notifiers.Discord{}, notifiers.Telegram{},
		notifiers.PagerDuty{}, notifiers.Webhook{}, notifiers.Ntfy{}, notifiers.Opsgenie{},
	} {
		eng.Register(n)
	}
	eng.Restore(snap.Alarms, 24*time.Hour)
	if cfg.ResolveOnStart {
		// close out alerts that were open when we last ran so downstream
		// incidents (PagerDuty etc.) don't linger; still-broken conditions
		// will re-alert through normal detection.
		for _, m := range snap.Alarms {
			for key, sa := range m {
				a := sa.Alert
				a.Key, a.Resolved, a.Time = key, true, time.Now()
				a.Message = "restarting cometduty: resolving " + a.Message
				eng.Dispatch(ctx, a)
			}
		}
	}

	// --- metrics ---
	var exp *metrics.Exporter
	var sink monitor.MetricsSink
	if cfg.PrometheusEnabled {
		exp = metrics.New(Version)
		sink = exp
	}

	// --- alert history + notify metrics observer ---
	hist, herr := history.Open(alertLog)
	switch {
	case herr != nil:
		slog.Warn("alert history disabled", "path", alertLog, "err", herr)
	case hist != nil:
		defer hist.Close()
		slog.Info("alert history", "path", alertLog)
	}
	eng.SetObserver(func(r alert.NotifyResult) {
		if exp != nil {
			exp.NotifyResult(r.Dest, r.Err == nil)
		}
		if err := hist.Record(history.FromNotify(r)); err != nil {
			slog.Warn("alert history write failed", "err", err)
		}
	})

	// --- supervisor ---
	sup := monitor.NewSupervisor(cfg, eng, sink)
	eng.SetResolver(sup.ResolveDestinations)

	if exp != nil {
		go func() {
			if err := metrics.Serve(ctx.Done(), cfg.PrometheusBind, cfg.PrometheusListenPort, sup.Ready); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("prometheus exporter failed", "err", err)
			}
		}()
		slog.Info("prometheus metrics", "addr", fmt.Sprintf("%s:%d/metrics", cfg.PrometheusBind, cfg.PrometheusListenPort))
	}
	sup.Start(ctx)

	go eng.RunReminders(ctx)

	// --- healthcheck pings ---
	if cfg.Healthcheck.Enabled && cfg.Healthcheck.PingURL != "" {
		go healthcheckLoop(ctx, cfg.Healthcheck.PingURL, cfg.Healthcheck.PingRate)
	}

	// --- periodic state save (so a crash doesn't reflood alerts) ---
	collect := func() *state.Snapshot {
		return &state.Snapshot{
			Alarms:    eng.Snapshot(),
			NodesDown: sup.SnapshotNodesDown(),
		}
	}
	go store.StartPeriodic(ctx, 2*time.Minute, collect)

	// --- signals: INT/TERM shutdown, HUP reload ---
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		sig := <-sigCh
		if sig == syscall.SIGHUP {
			slog.Info("SIGHUP — reloading configuration")
			newCfg, err := config.Load(cfgFile, chainsDir, "")
			if err != nil {
				slog.Error("reload failed", "err", err)
				continue
			}
			if fatal, problems := config.Validate(newCfg); fatal {
				for _, p := range problems {
					slog.Error("config problem", "problem", p)
				}
				slog.Error("reloaded config is invalid — keeping the old one")
				continue
			}
			sup.Reload(newCfg)
			continue
		}
		break
	}

	slog.Info("shutting down — saving state")
	sup.Stop()
	if err := store.Save(collect()); err != nil {
		slog.Warn("state save failed", "err", err)
	}
	cancel()
	time.Sleep(300 * time.Millisecond) // let in-flight notifications flush
	return nil
}

// healthcheckLoop pings a URL on an interval (dead man's switch).
func healthcheckLoop(ctx context.Context, url string, every time.Duration) {
	if every <= 0 {
		every = 60 * time.Second
	}
	t := time.NewTicker(every * time.Second)
	defer t.Stop()
	client := &http.Client{Timeout: 10 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				slog.Warn("healthcheck ping failed", "err", err)
				continue
			}
			_ = resp.Body.Close()
		}
	}
}

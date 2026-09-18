package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/abhijitkrm/cometduty/internal/alert"
	"github.com/abhijitkrm/cometduty/internal/alert/notifiers"
	"github.com/abhijitkrm/cometduty/internal/assets"
	"github.com/abhijitkrm/cometduty/internal/config"
	"github.com/abhijitkrm/cometduty/internal/monitor"
)

func validateCmd() *cobra.Command {
	var live bool
	c := &cobra.Command{
		Use:   "validate",
		Short: "check the configuration file and exit",
		RunE: func(cmd *cobra.Command, args []string) error {
			if password == "" {
				password = os.Getenv("PASSWORD")
			}
			cfg, err := config.Load(cfgFile, chainsDir, password)
			if err != nil {
				return err
			}
			fatal, problems := config.Validate(cfg)
			for _, p := range problems {
				fmt.Println(p)
			}
			if fatal {
				return errors.New("configuration has fatal problems")
			}
			fmt.Printf("ok: %d chain(s) configured\n", len(cfg.Chains))
			if !live {
				return nil
			}
			// --live: probe every node + resolve every validator, the same code
			// paths the monitor uses. Any failure fails the command.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			failures := 0
			for name, cc := range cfg.Chains {
				fmt.Printf("\n%s (%s):\n", name, cc.ChainID)
				for _, r := range monitor.ProbeChain(ctx, cc) {
					mark := "ok  "
					if !r.OK {
						mark = "FAIL"
						failures++
					}
					fmt.Printf("  %s %s — %s\n", mark, r.Subject, r.Detail)
				}
			}
			if failures > 0 {
				return fmt.Errorf("%d live check(s) failed", failures)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&live, "live", false, "also probe nodes and resolve validators against the live network")
	return c
}

func exampleConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "example-config",
		Short: "print an annotated example config.yml and exit",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Print(string(assets.ExampleConfig))
		},
	}
}

func testAlertCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test-alert [kind]",
		Short: "send a test notification (kinds: slack discord telegram pagerduty webhook ntfy opsgenie; empty = all enabled)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if password == "" {
				password = os.Getenv("PASSWORD")
			}
			cfg, err := config.Load(cfgFile, chainsDir, password)
			if err != nil {
				return err
			}
			all := map[string]alert.Notifier{
				"slack": notifiers.Slack{}, "discord": notifiers.Discord{},
				"telegram": notifiers.Telegram{}, "pagerduty": notifiers.PagerDuty{},
				"webhook": notifiers.Webhook{}, "ntfy": notifiers.Ntfy{},
				"opsgenie": notifiers.Opsgenie{},
			}
			cfgs := map[string]any{
				"slack": cfg.Slack, "discord": cfg.Discord, "telegram": cfg.Telegram,
				"pagerduty": cfg.Pagerduty, "webhook": cfg.Webhook,
				"ntfy": cfg.Ntfy, "opsgenie": cfg.Opsgenie,
			}
			enabled := map[string]bool{
				"slack": cfg.Slack.Enabled, "discord": cfg.Discord.Enabled,
				"telegram": cfg.Telegram.Enabled, "pagerduty": cfg.Pagerduty.Enabled,
				"webhook": cfg.Webhook.Enabled, "ntfy": cfg.Ntfy.Enabled,
				"opsgenie": cfg.Opsgenie.Enabled,
			}
			ctx := cmd.Context()
			var failed bool
			for kind, n := range all {
				if len(args) == 1 && args[0] != kind {
					continue
				}
				if !enabled[kind] {
					if len(args) == 1 {
						fmt.Printf("%s: not enabled in config\n", kind)
					}
					continue
				}
				if err := n.Test(ctx, cfgs[kind]); err != nil {
					failed = true
					fmt.Printf("%s: FAILED: %v\n", kind, err)
				} else {
					fmt.Printf("%s: test alert sent\n", kind)
				}
			}
			if failed {
				return errors.New("one or more test alerts failed")
			}
			return nil
		},
	}
}

func encryptCmd() *cobra.Command {
	var out string
	c := &cobra.Command{
		Use:   "encrypt",
		Short: "encrypt the config file with a passphrase (age/scrypt)",
		RunE: func(cmd *cobra.Command, args []string) error {
			pass, err := getPassword()
			if err != nil {
				return err
			}
			plain, err := os.ReadFile(cfgFile) //nolint:gosec -- operator path
			if err != nil {
				return err
			}
			enc, err := config.EncryptAge(plain, pass)
			if err != nil {
				return err
			}
			return os.WriteFile(out, enc, 0o600)
		},
	}
	c.Flags().StringVarP(&out, "out", "o", "config.yml.age", "output file")
	return c
}

func decryptCmd() *cobra.Command {
	var in, out string
	c := &cobra.Command{
		Use:   "decrypt",
		Short: "decrypt an age-encrypted config file",
		RunE: func(cmd *cobra.Command, args []string) error {
			pass, err := getPassword()
			if err != nil {
				return err
			}
			enc, err := os.ReadFile(in) //nolint:gosec -- operator path
			if err != nil {
				return err
			}
			dec, err := config.DecryptAge(enc, pass)
			if err != nil {
				return err
			}
			return os.WriteFile(out, dec, 0o600)
		},
	}
	c.Flags().StringVarP(&in, "in", "i", "config.yml.age", "encrypted input file")
	c.Flags().StringVarP(&out, "out", "o", "config.yml", "plaintext output file")
	return c
}

func getPassword() (string, error) {
	if password != "" {
		return password, nil
	}
	if p := os.Getenv("PASSWORD"); p != "" {
		return p, nil
	}
	fmt.Fprint(os.Stderr, "password: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print version and exit",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("cometduty %s (%s %s %s)\n", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		},
	}
}

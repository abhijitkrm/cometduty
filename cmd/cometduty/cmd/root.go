// Package cmd defines the cometduty CLI.
package cmd

import (
	"github.com/spf13/cobra"
)

// Version is injected from main (set via -ldflags).
var Version = "dev"

var (
	cfgFile   string
	stateFile string
	alertLog  string
	chainsDir string
	password  string
	jsonLogs  bool
	verbose   bool
)

// Root builds the command tree. Default action is `run`.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "cometduty",
		Short: "cometduty — CometBFT validator missed-block monitor",
		Long: "cometduty monitors validators on CometBFT chains (including Cosmos-EVM)\n" +
			"and alerts on missed blocks, stalls, jailing, and unhealthy RPC endpoints.",
		SilenceUsage:  true, // runtime errors shouldn't dump help text
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMonitor(cmd)
		},
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&cfgFile, "config", "f", "config.yml", "configuration file or https:// URL (age-encrypted)")
	pf.StringVar(&stateFile, "state", ".cometduty-state.json", "file for storing state between restarts")
	pf.StringVar(&alertLog, "alert-log", ".cometduty-alerts.jsonl", "append-only JSONL alert delivery log (empty disables)")
	pf.StringVar(&chainsDir, "chains-dir", "chains.d", "directory with per-chain config files (.yml/.yaml)")
	pf.StringVar(&password, "password", "", "config decryption password (or PASSWORD env var)")
	pf.BoolVar(&jsonLogs, "log-json", false, "emit structured JSON logs")
	pf.BoolVarP(&verbose, "verbose", "v", false, "debug logging")

	root.AddCommand(validateCmd(), exampleConfigCmd(), testAlertCmd(), encryptCmd(), decryptCmd(), versionCmd())
	return root
}

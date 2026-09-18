package main

import (
	"fmt"
	"os"

	"github.com/abhijitkrm/cometduty/cmd/cometduty/cmd"
)

var version = "dev"

func main() {
	cmd.Version = version
	if err := cmd.Root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

package main

import (
	"fmt"
	"os"

	"github.com/goodsmileduck/dockrail/cmd"
)

// version is stamped via -ldflags at release time; propagated to the CLI.
var version = "dev"

func main() {
	cmd.Version = version
	if err := cmd.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

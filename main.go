package main

import (
	"os"

	"github.com/b-open-io/bananablocks-cli/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

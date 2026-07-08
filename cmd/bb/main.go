package main

import (
	"os"

	"github.com/b-open-io/bananablocks-cli/internal/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

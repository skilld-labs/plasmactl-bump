// Package executes Launchr application.
package main

import (
	"os"

	"github.com/launchrctl/launchr"

	_ "github.com/skilld-labs/components-bump"
)

func main() {
	os.Exit(launchr.Run())
}

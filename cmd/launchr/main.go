// Package executes Launchr application.
package main

import (
	"github.com/launchrctl/launchr"

	_ "github.com/skilld-labs/plasmactl-bump/v2"
)

func main() {
	launchr.RunAndExit()
}

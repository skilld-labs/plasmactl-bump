// Package test contains testscript-based integration tests.
package test

import (
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	_ "github.com/launchrctl/compose" // registers compose plugin
	"github.com/launchrctl/launchr"
	launchrtest "github.com/launchrctl/launchr/test"
	_ "github.com/skilld-labs/plasmactl-bump/v2" // registers bump plugin
)

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"launchr": launchr.RunAndExit,
	})
}

func TestBump(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:                 "testdata/bump",
		Cmds:                launchrtest.CmdsTestScript(),
		RequireExplicitExec: true,
	})
}

func TestSync(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:                 "testdata/sync",
		Cmds:                launchrtest.CmdsTestScript(),
		RequireExplicitExec: true,
	})
}

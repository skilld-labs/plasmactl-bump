// Package plasmactlbump implements a launchr plugin with bump functionality
package plasmactlbump

import (
	"context"
	_ "embed"

	"github.com/launchrctl/keyring"
	"github.com/launchrctl/launchr"
	"github.com/launchrctl/launchr/pkg/action"
)

//go:embed action.yaml
var actionYaml []byte

func init() {
	launchr.RegisterPlugin(&Plugin{})
}

// Plugin is [launchr.Plugin] plugin providing bump functionality.
type Plugin struct {
	k   keyring.Keyring
	cfg launchr.Config
}

// PluginInfo implements [launchr.Plugin] interface.
func (p *Plugin) PluginInfo() launchr.PluginInfo {
	return launchr.PluginInfo{
		Weight: 10,
	}
}

// OnAppInit implements [launchr.Plugin] interface.
func (p *Plugin) OnAppInit(app launchr.App) error {
	app.GetService(&p.cfg)
	app.GetService(&p.k)
	return nil
}

// DiscoverActions implements [launchr.ActionDiscoveryPlugin] interface.
func (p *Plugin) DiscoverActions(_ context.Context) ([]*action.Action, error) {
	a := action.NewFromYAML("bump", actionYaml)
	a.SetRuntime(action.NewFnRuntime(func(_ context.Context, a *action.Action) error {
		input := a.Input()
		doSync := input.Opt("sync").(bool)
		dryRun := input.Opt("dry-run").(bool)
		allowOverride := input.Opt("allow-override").(bool)
		filterByResourceUsage := input.Opt("playbook-filter").(bool)
		timeDepth := input.Opt("time-depth").(string)
		vaultpass := input.Opt("vault-pass").(string)
		last := input.Opt("last").(bool)

		hideProgress := input.Opt("hide-progress").(bool)
		if launchr.Log().Level() > 0 {
			hideProgress = true
		}

		if !doSync {
			bumpAction := BumpAction{last: last, dryRun: dryRun}
			return bumpAction.Execute()
		}

		if !doSync {
			bumpAction := BumpAction{last: last, dryRun: dryRun}
			return bumpAction.Execute()
		}

		syncAction := SyncAction{
			keyring: p.k,

			domainDir:   ".",
			buildDir:    ".compose/build",
			packagesDir: ".compose/packages",

			dryRun:                dryRun,
			filterByResourceUsage: filterByResourceUsage,
			timeDepth:             timeDepth,
			allowOverride:         allowOverride,
			vaultPass:             vaultpass,
			showProgress:          !hideProgress,
		}

		err := syncAction.Execute()
		if err != nil {
			return err
		}

		return nil
	}))
	return []*action.Action{a}, nil
}

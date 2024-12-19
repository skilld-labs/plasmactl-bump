// Package plasmactlbump implements a launchr plugin with bump functionality
package plasmactlbump

import (
	"github.com/launchrctl/keyring"
	"github.com/launchrctl/launchr"
	"github.com/spf13/cobra"
)

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

// CobraAddCommands implements launchr.CobraPlugin interface to provide bump functionality.
func (p *Plugin) CobraAddCommands(rootCmd *cobra.Command) error {
	var doSync bool
	var dryRun bool
	var allowOverride bool
	var vaultpass string
	var last bool

	var bumpCmd = &cobra.Command{
		Use:   "bump",
		Short: "Bump or sync versions of updated components",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Don't show usage help on a runtime error.
			cmd.SilenceUsage = true

			verboseCount, err := rootCmd.Flags().GetCount("verbose")
			if err != nil {
				return err
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

				dryRun:        dryRun,
				allowOverride: allowOverride,
				vaultPass:     vaultpass,
				verbosity:     verboseCount,
			}

			err = syncAction.Execute()
			if err != nil {
				return err
			}

			return nil
		},
	}

	bumpCmd.Flags().BoolVarP(&doSync, "sync", "s", false, "Propagate versions of updated components to their dependencies")
	bumpCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Simulate bump or sync without updating anything")
	bumpCmd.Flags().BoolVar(&allowOverride, "allow-override", false, "Allow override committed version by current build value")
	bumpCmd.Flags().StringVar(&vaultpass, "vault-pass", "", "Password for Ansible Vault")
	bumpCmd.Flags().BoolVarP(&last, "last", "l", false, "Bump resources modified in last commit only")

	rootCmd.AddCommand(bumpCmd)
	return nil
}

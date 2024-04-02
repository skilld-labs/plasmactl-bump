// Package plasmactlbump implements a launchr plugin with bump functionality
package plasmactlbump

import (
	"github.com/spf13/cobra"

	"github.com/launchrctl/launchr"
)

func init() {
	launchr.RegisterPlugin(&Plugin{})
}

// Plugin is launchr plugin providing bump action.
type Plugin struct {
	b   BumpAction
	cfg launchr.Config
}

// PluginInfo implements launchr.Plugin interface.
func (p *Plugin) PluginInfo() launchr.PluginInfo {
	return launchr.PluginInfo{
		Weight: 10,
	}
}

// OnAppInit implements launchr.Plugin interface.
func (p *Plugin) OnAppInit(app launchr.App) error {
	app.GetService(&p.cfg)
	p.b = newBumpService(p.cfg)
	app.AddService(p.b)
	return nil
}

// CobraAddCommands implements launchr.CobraPlugin interface to provide bump functionality.
func (p *Plugin) CobraAddCommands(rootCmd *cobra.Command) error {
	var sync bool
	var dryRun bool
	var override string
	var username string
	var password string
	var vaultpass string
	var last bool

	var bumpCmd = &cobra.Command{
		Use:   "bump",
		Short: "Bump or sync versions of updated components",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Don't show usage help on a runtime error.
			cmd.SilenceUsage = true

			if sync {
				syncAction := SyncAction{
					sourceDir:     ".compose/build",
					comparisonDir: ".compose/comparison-artifact",
					dryRun:        dryRun,
				}

				return syncAction.Execute(username, password, override, vaultpass)
			}

			return p.b.Bump(last)
		},
	}

	bumpCmd.Flags().BoolVarP(&sync, "sync", "s", false, "Propagate versions of updated components to their dependencies")
	bumpCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Simulate propagate without doing anything")
	bumpCmd.Flags().StringVar(&override, "override", "", "Override comparison artifact name (commit)")
	bumpCmd.Flags().StringVar(&username, "username", "", "Username for artifact repository")
	bumpCmd.Flags().StringVar(&password, "password", "", "Password for artifact repository")
	bumpCmd.Flags().StringVar(&vaultpass, "vault-pass", "", "Password for Ansible Vault")
	bumpCmd.Flags().BoolVarP(&last, "last", "l", false, "Bump resources modified in last commit only")

	rootCmd.AddCommand(bumpCmd)
	return nil
}

// Package plasmactlbump implements a launchr plugin with bump functionality
package plasmactlbump

import (
	"github.com/launchrctl/keyring"
	"github.com/launchrctl/launchr"
	"github.com/spf13/cobra"

	"github.com/skilld-labs/plasmactl-bump/v2/pkg/sync"
)

func init() {
	launchr.RegisterPlugin(&Plugin{})
}

// Plugin is [launchr.Plugin] plugin providing bump functionality.
type Plugin struct {
	b   BumpAction
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
	p.b = newBumpService(p.cfg)
	app.AddService(p.b)
	return nil
}

// CobraAddCommands implements launchr.CobraPlugin interface to provide bump functionality.
func (p *Plugin) CobraAddCommands(rootCmd *cobra.Command) error {
	var doSync bool
	var dryRun bool
	var listImpacted bool
	var override string
	var username string
	var password string
	var vaultpass string
	var last bool

	var bumpCmd = &cobra.Command{
		Use:   "bump",
		Short: "Bump or sync versions of updated components",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Don't show usage help on a runtime error.
			cmd.SilenceUsage = true

			if !doSync {
				return p.b.Bump(last)
			}

			syncAction := SyncAction{
				keyring: p.k,

				domainDir:        ".",
				buildDir:         ".compose/build",
				comparisonDir:    ".compose/comparison-artifact",
				packagesDir:      ".compose/packages",
				artifactsDir:     ".compose/artifacts",
				artifactsRepoURL: "https://repositories.skilld.cloud",

				dryRun:           dryRun,
				listImpacted:     listImpacted,
				vaultPass:        vaultpass,
				artifactOverride: truncateOverride(override),
			}

			return syncAction.Execute(username, password)
		},
	}

	bumpCmd.Flags().BoolVarP(&doSync, "sync", "s", false, "Propagate versions of updated components to their dependencies")
	bumpCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Simulate propagate without doing anything")
	bumpCmd.Flags().BoolVar(&listImpacted, "list-impacted", false, "Print list of impacted resources")
	bumpCmd.Flags().StringVar(&override, "override", "", "Override comparison artifact name (commit)")
	bumpCmd.Flags().StringVar(&username, "username", "", "Username for artifact repository")
	bumpCmd.Flags().StringVar(&password, "password", "", "Password for artifact repository")
	bumpCmd.Flags().StringVar(&vaultpass, "vault-pass", "", "Password for Ansible Vault")
	bumpCmd.Flags().BoolVarP(&last, "last", "l", false, "Bump resources modified in last commit only")

	rootCmd.AddCommand(bumpCmd)
	return nil
}

func truncateOverride(override string) string {
	truncateLength := sync.ArtifactTruncateLength

	if len(override) > truncateLength {
		launchr.Term().Info().Printfln("Truncated override value to %d chars: %s", truncateLength, override)
		return override[:truncateLength]
	}
	return override
}

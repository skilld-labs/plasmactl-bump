// Package componentsbump implements a launchr plugin with bumper functionality
package componentsbump

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
	return launchr.PluginInfo{}
}

// OnAppInit implements launchr.Plugin interface.
func (p *Plugin) OnAppInit(app launchr.App) error {
	app.GetService(&p.cfg)
	p.b = newComponentsBumpService(p.cfg)
	app.AddService(p.b)
	return nil
}

// CobraAddCommands implements launchr.CobraPlugin interface to provide bump functionality.
func (p *Plugin) CobraAddCommands(rootCmd *cobra.Command) error {
	var bumpCmd = &cobra.Command{
		Use:   "components-bump",
		Short: "Bump versions of updated roles",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Don't show usage help on a runtime error.
			cmd.SilenceUsage = true
			return p.b.Bump()
		},
	}

	rootCmd.AddCommand(bumpCmd)
	return nil
}

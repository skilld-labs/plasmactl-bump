package plasmactlbump

import (
	"path/filepath"
	"strings"

	"github.com/launchrctl/launchr/pkg/action"
	"github.com/skilld-labs/plasmactl-bump/v2/pkg/repository"
	"github.com/skilld-labs/plasmactl-bump/v2/pkg/sync"
)

var unversionedFiles = map[string]struct{}{
	"README.md":  {},
	"README.svg": {},
}

// bumpAction is an action representing versions update of committed resources.
type bumpAction struct {
	action.WithLogger
	action.WithTerm

	last   bool
	dryRun bool
}

func (b *bumpAction) printMemo() {
	b.Log().Info("List of non-versioned files:")
	for k := range unversionedFiles {
		b.Log().Info(k)
	}
}

// Execute the bump action to update committed resources.
func (b *bumpAction) Execute() error {
	b.Term().Info().Println("Bumping updated resources...")
	b.printMemo()

	bumper, err := repository.NewBumper()
	if err != nil {
		return err
	}

	if bumper.IsOwnCommit() {
		b.Term().Info().Println("skipping bump, as the latest commit is already by the bumper tool")
		return nil
	}

	commits, err := bumper.GetCommits(b.last)
	if err != nil {
		return err
	}

	resources := b.collectResources(commits)
	if len(resources) == 0 {
		b.Term().Info().Println("No resource to update")
		return nil
	}

	err = b.updateResources(resources)
	if err != nil {
		b.Log().Error("There is an error during resources update")
		return err
	}

	if b.dryRun {
		return nil
	}

	return bumper.Commit()
}

func (b *bumpAction) getResource(path string) *sync.Resource {
	if !isVersionableFile(path) {
		return nil
	}

	platform, kind, role, err := sync.ProcessResourcePath(path)
	if err != nil || (platform == "" || kind == "" || role == "") {
		return nil
	}

	// skip actions dir from triggering bump.
	resourceActionsDir := filepath.Join(platform, kind, "roles", role, "actions")
	if strings.Contains(path, resourceActionsDir) {
		return nil
	}

	resource := sync.NewResource(sync.PrepareMachineResourceName(platform, kind, role), ".")
	if !resource.IsValidResource() {
		return nil
	}

	return resource
}

func (b *bumpAction) collectResources(commits []*repository.Commit) map[string]map[string]*sync.Resource {
	uniqueVersion := map[string]string{}

	resources := make(map[string]map[string]*sync.Resource)
	for _, c := range commits {
		hash := c.Hash[:13]
		for _, path := range c.Files {
			resource := b.getResource(path)
			if resource == nil {
				continue
			}

			if _, ok := resources[hash]; !ok {
				resources[hash] = make(map[string]*sync.Resource)
			}

			if _, ok := uniqueVersion[resource.GetName()]; ok {
				continue
			}

			b.Term().Printfln("Processing resource %s", resource.GetName())
			resources[hash][resource.GetName()] = resource
			uniqueVersion[resource.GetName()] = hash
		}
	}

	return resources
}

func (b *bumpAction) updateResources(hashResourcesMap map[string]map[string]*sync.Resource) error {
	if len(hashResourcesMap) == 0 {
		return nil
	}

	b.Term().Printf("Updating versions:\n")
	for version, resources := range hashResourcesMap {
		for mrn, r := range resources {
			currentVersion, debug, err := r.GetVersion()
			for _, d := range debug {
				b.Log().Debug("error", "message", d)
			}
			if err != nil {
				return err
			}

			b.Term().Printfln("- %s from %s to %s", mrn, currentVersion, version)
			if b.dryRun {
				continue
			}

			debug, err = r.UpdateVersion(version)
			for _, d := range debug {
				b.Log().Debug("error", "message", d)
			}
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func isVersionableFile(path string) bool {
	name := filepath.Base(path)
	_, ok := unversionedFiles[name]
	return !ok
}

package plasmactlbump

import (
	"path/filepath"

	"github.com/launchrctl/launchr"

	"github.com/skilld-labs/plasmactl-bump/v2/pkg/repository"
	"github.com/skilld-labs/plasmactl-bump/v2/pkg/sync"
)

var unversionedFiles = map[string]struct{}{
	"README.md":  {},
	"README.svg": {},
}

// BumpAction is an action representing versions update of committed resources.
type BumpAction struct {
	last   bool
	dryRun bool
}

func printMemo() {
	launchr.Log().Info("List of non-versioned files:")
	for k := range unversionedFiles {
		launchr.Log().Info(k)
	}
}

// Execute the bump action to update committed resources.
func (b *BumpAction) Execute() error {
	launchr.Term().Info().Println("Bumping updated resources...")
	printMemo()

	bumper, err := repository.NewBumper()
	if err != nil {
		return err
	}

	if bumper.IsOwnCommit() {
		launchr.Term().Info().Println("skipping bump, as the latest commit is already by the bumper tool")
		return nil
	}

	commits, err := bumper.GetCommits(b.last)
	if err != nil {
		return err
	}

	resources := b.collectResources(commits)
	if len(resources) == 0 {
		launchr.Term().Info().Println("No resource to update")
		return nil
	}

	err = b.updateResources(resources)
	if err != nil {
		launchr.Log().Error("There is an error during resources update")
		return err
	}

	if b.dryRun {
		return nil
	}

	return bumper.Commit()
}

func (b *BumpAction) getResource(path string) *sync.Resource {
	if !isVersionableFile(path) {
		return nil
	}

	return sync.BuildResourceFromPath(path, ".")
}

func (b *BumpAction) collectResources(commits []*repository.Commit) map[string]map[string]*sync.Resource {
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

			launchr.Term().Printfln("Processing resource %s", resource.GetName())
			resources[hash][resource.GetName()] = resource
			uniqueVersion[resource.GetName()] = hash
		}
	}

	return resources
}

func (b *BumpAction) updateResources(hashResourcesMap map[string]map[string]*sync.Resource) error {
	if len(hashResourcesMap) == 0 {
		return nil
	}

	launchr.Term().Printf("Updating versions:\n")
	for version, resources := range hashResourcesMap {
		for mrn, r := range resources {
			currentVersion, err := r.GetVersion()
			if err != nil {
				return err
			}

			launchr.Term().Printfln("- %s from %s to %s", mrn, currentVersion, version)
			if b.dryRun {
				continue
			}

			err = r.UpdateVersion(version)
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

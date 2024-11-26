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

	files, err := bumper.GetModifiedFiles(b.last)
	if err != nil {
		return err
	}

	resources := b.collectResources(files)
	if len(resources) == 0 {
		launchr.Term().Info().Println("No resource to update")
		return nil
	}

	version, err := bumper.GetLastCommitShortHash()
	if err != nil {
		launchr.Log().Error("Can't retrieve commit hash")
		return err
	}

	err = b.updateResources(resources, version)
	if err != nil {
		launchr.Log().Error("There is an error during resources update")
		return err
	}

	if b.dryRun {
		return nil
	}

	return bumper.Commit()
}

func (b *BumpAction) collectResources(files []string) map[string]*sync.Resource {
	// @TODO re-use inventory.GetChangedResources()
	resources := make(map[string]*sync.Resource)
	for _, path := range files {
		if !isVersionableFile(path) {
			continue
		}

		platform, kind, role, err := sync.ProcessResourcePath(path)
		if err != nil {
			continue
		}

		if platform == "" || kind == "" || role == "" {
			continue
		}

		if sync.IsUpdatableKind(kind) {
			resource := sync.NewResource(sync.PrepareMachineResourceName(platform, kind, role), "")
			if _, ok := resources[resource.GetName()]; !ok {
				// Check is meta/plasma.yaml exists for resource
				if !resource.IsValidResource() {
					continue
				}

				launchr.Term().Printfln("Processing resource %s", resource.GetName())
				resources[resource.GetName()] = resource
			}
		}

	}

	return resources
}

func (b *BumpAction) updateResources(resources map[string]*sync.Resource, version string) error {
	if len(resources) == 0 {
		return nil
	}

	launchr.Term().Printf("Updating versions:\n")
	for _, r := range resources {
		currentVersion, err := r.GetVersion()
		if err != nil {
			return err
		}

		launchr.Term().Printfln("- %s from %s to %s", r.GetName(), currentVersion, version)
		if b.dryRun {
			continue
		}

		err = r.UpdateVersion(version)
		if err != nil {
			return err
		}
	}

	return nil
}

func isVersionableFile(path string) bool {
	name := filepath.Base(path)
	_, ok := unversionedFiles[name]
	return !ok
}

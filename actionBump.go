package plasmactlbump

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/launchrctl/launchr"
	"github.com/skilld-labs/plasmactl-bump/pkg/repository"
	"github.com/skilld-labs/plasmactl-bump/pkg/sync"
)

var unversionedFiles = map[string]struct{}{
	"README.md":  {},
	"README.svg": {},
}

// PromptError prints an error.
func PromptError(err error) {
	if err == nil {
		return
	}

	fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("error: %s\n", err))
}

// BumpAction is a launchr.Service providing bumper functionality.
type BumpAction interface {
	launchr.Service
	Bump(last bool) error
}

type bumpService struct {
	cfg launchr.Config
}

func newBumpService(cfg launchr.Config) BumpAction {
	return &bumpService{
		cfg: cfg,
	}
}

// ServiceInfo implements launchr.Service interface.
func (k *bumpService) ServiceInfo() launchr.ServiceInfo {
	return launchr.ServiceInfo{}
}

func printMemo() {
	launchr.Log().Info("List of non-versioned files:")
	for k := range unversionedFiles {
		launchr.Log().Info(k)
	}
}

func (k *bumpService) Bump(last bool) error {
	launchr.Term().Println("Bump updated versions...")
	printMemo()

	bumper, err := repository.NewBumper()
	if err != nil {
		return err
	}

	if bumper.IsOwnCommit() {
		return errors.New("skipping bump, as the latest commit is already by the bumper tool")
	}

	files, err := bumper.GetModifiedFiles(last)
	if err != nil {
		return err
	}

	resources := k.collectResources(files)
	if len(resources) == 0 {
		return errors.New("no resources to update")
	}

	version, err := bumper.GetLastCommitShortHash()
	if err != nil {
		launchr.Log().Error("Can't retrieve commit hash")
		return err
	}

	err = k.updateResources(resources, version)
	if err != nil {
		launchr.Log().Error("There is an error during resources update")
		return err
	}

	return bumper.Commit()
}

func (k *bumpService) collectResources(files []string) map[string]*sync.Resource {
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

func (k *bumpService) updateResources(resources map[string]*sync.Resource, version string) error {
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

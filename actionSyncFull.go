package plasmactlbump

import (
	"errors"
	"fmt"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/launchrctl/launchr"
	"github.com/skilld-labs/plasmactl-bump/v2/pkg/sync"
	"strings"
	"time"
)

// ExecuteFromZero the sync action to propagate resources' versions.
func (s *SyncAction) ExecuteFromZero() error {
	err := s.ensureVaultpassExists()
	if err != nil {
		return err
	}

	err = s.propagateZero()
	if err != nil {
		return err
	}

	if s.saveKeyring {
		err = s.keyring.Save()
	}

	return err
}

func (s *SyncAction) propagateZero() error {
	inv, err := sync.NewInventory(s.buildDir)
	if err != nil {
		return err
	}

	// build timeline and resources to copy.
	timeline, err := s.buildTimelineZero(inv)
	if err != nil {
		return fmt.Errorf("building timeline > %w", err)
	}

	if len(timeline) == 0 {
		launchr.Term().Warning().Println("No resources were found for propagation")
		return nil
	}
	launchr.Term().Warning().Println("Exit timecheck")
	return nil

	// sort and iterate timeline, create propagation map.
	toPropagate, resourceVersionMap, err := s.buildPropagationMap(inv, timeline)
	if err != nil {
		return fmt.Errorf("building propagation map > %w", err)
	}

	history := sync.NewOrderedMap[*sync.Resource]()

	// update resources.
	err = s.updateResources(resourceVersionMap, toPropagate, history)
	if err != nil {
		return fmt.Errorf("propagate > %w", err)
	}

	return nil
}

func (s *SyncAction) buildTimelineZero(buildInv *sync.Inventory) ([]sync.TimelineItem, error) {
	timeline := sync.CreateTimeline()

	launchr.Term().Info().Printfln("Gathering domain and package resources")
	//resourcesMap, packagePathMap, err := s.getResourcesMaps()
	//if err != nil {
	//	return timeline, fmt.Errorf("build resource map > %w", err)
	//}

	//// Find new or updated resources in diff.
	//launchr.Term().Info().Printfln("Checking domain resources")
	//timeline, err = s.populateTimelineResourcesZero(resourcesMap[domainNamespace], timeline, s.domainDir)
	//if err != nil {
	//	return nil, fmt.Errorf("iteraring domain resources > %w", err)
	//}

	//domains := packagePathMap
	//domains[domainNamespace] = s.domainDir

	// Iterate each package, find new or updated resources in diff.
	//launchr.Term().Info().Printfln("Checking packages resources")
	//for name, packagePath := range domains {
	//	timeline, err = s.populateTimelineResourcesZero(resourcesMap[name], timeline, packagePath)
	//	if err != nil {
	//		return nil, fmt.Errorf("iteraring package %s resources > %w", name, err)
	//	}
	//}

	launchr.Term().Info().Printfln("Checking variables change")
	timeline, err := s.populateTimelineVarsZero(buildInv, timeline, s.domainDir)
	if err != nil {
		return nil, fmt.Errorf("iteraring variables > %w", err)
	}

	//sync.SortTimeline(timeline)
	//for _, item := range timeline {
	//	launchr.Term().Printfln("timeline item (resources) %s %s %s", item.GetVersion(), item.GetDate(), item.GetCommit())
	//
	//	switch i := item.(type) {
	//	case *sync.TimelineResourcesItem:
	//		resources := i.GetResources()
	//		resources.SortKeysAlphabetically()
	//		for _, key := range resources.Keys() {
	//			launchr.Term().Printfln("- %s", key)
	//		}
	//
	//	case *sync.TimelineVariablesItem:
	//		launchr.Term().Printfln("timeline item (variables) %s %s %s", item.GetVersion(), item.GetDate(), item.GetCommit())
	//		variables := i.GetVariables()
	//		variables.SortKeysAlphabetically()
	//		for _, key := range variables.Keys() {
	//			launchr.Term().Printfln("- %s", key)
	//		}
	//	}
	//}

	return timeline, nil
}

func (s *SyncAction) populateTimelineResourcesZero(namespaceResources *sync.OrderedMap[bool], timeline []sync.TimelineItem, gitPath string) ([]sync.TimelineItem, error) {
	repo, err := git.PlainOpen(gitPath)
	launchr.Term().Info().Printfln(gitPath)
	if err != nil {
		return nil, fmt.Errorf("%s - %w", gitPath, err)
	}

	// @todo temp
	resourcesOrderedMap := sync.NewOrderedMap[*sync.Resource]()

	for _, resourceName := range namespaceResources.Keys() {
		resource := sync.NewResource(resourceName, gitPath)
		if resource == nil {
			return nil, fmt.Errorf("some error with resource creation at %s", gitPath)
		}

		resourcesOrderedMap.Set(resource.GetName(), resource)
	}

	timeline, err = s.findResourcesChangeTimeZero(resourcesOrderedMap, repo, timeline)
	if err != nil {
		return timeline, fmt.Errorf("error > %w", err)
	}

	return timeline, nil
}

type hashStruct struct {
	prevHash     string
	prevHashTime time.Time
	author       string
}

func (s *SyncAction) findResourcesChangeTimeZero(namespaceResources *sync.OrderedMap[*sync.Resource], repo *git.Repository, timeline []sync.TimelineItem) ([]sync.TimelineItem, error) {
	//timelineMap := sync.NewOrderedMap[*sync.TimelineItem]()
	//ref, err := s.ensureResourceIsVersioned(resourceVersion, resourceMetaPath, repo)
	//if err != nil {
	//	return fmt.Errorf("ensure versioned > %w", err)
	//}

	ref, err := repo.Head()
	if err != nil {
		return timeline, err
	}

	//@TODO use git log -S'value' -- path/to/file instead of full history search?
	//  or git log -- path/to/file
	//  go-git log -- filepath looks abysmally slow, to research.
	// start from the latest commit and iterate to the past
	cIter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return timeline, err
	}

	hashesMap := make(map[string]*hashStruct)
	toIterate := namespaceResources.ToDict()

	//toIterate := make(map[string]*sync.Resource)
	//toIterate["cognition__libraries__person"] = toIterate2["cognition__libraries__person"]
	//toIterate["cognition__executors__data"] = toIterate2["cognition__executors__data"]

	test := len(toIterate)
	err = cIter.ForEach(func(c *object.Commit) error {
		//for k, resource := range toIterate {
		//launchr.Term().Error().Printfln("commit - %s", c.Hash.String())

		if len(toIterate) == 0 {
			return storer.ErrStop
		}

		if len(toIterate) != test {
			test = len(toIterate)
			launchr.Log().Debug(fmt.Sprintf("Remaining unidentified resources %d", test))
		}

		//toIterateInner := make(map[string]*sync.Resource)
		//for k, resource := range toIterate {
		//	toIterateInner[k] = resource
		//}

		//launchr.Term().Warning().Printfln("%v", toIterateInner)
		//for k, resource := range toIterateInner {
		//	launchr.Term().Printfln("%s - %s", k, resource.BuildMetaPath())
		//}

		for k, resource := range toIterate {
			if _, ok := hashesMap[k]; !ok {
				hashesMap[k] = &hashStruct{}
			}

			resourceMetaPath := resource.BuildMetaPath()
			resourceVersion, err := resource.GetVersion()
			if err != nil {
				return err
			}

			//launchr.Term().Warning().Printfln(resourceMetaPath)
			//launchr.Term().Warning().Printfln(resourceVersion)

			file, errIt := c.File(resourceMetaPath)
			if errIt != nil {
				if !errors.Is(errIt, object.ErrFileNotFound) {
					return fmt.Errorf("open file %s in commit %s > %w", resourceMetaPath, c.Hash, errIt)
				}
				if hashesMap[k].prevHash == "" {
					hashesMap[k].prevHash = c.Hash.String()
					hashesMap[k].prevHashTime = c.Author.When
					hashesMap[k].author = c.Author.Name
				}

				launchr.Log().Debug("File didn't exist before, take current hash as version", "version", resourceMetaPath)
				//launchr.Term().Error().Printfln("%s - %s - %s", c.Hash.String(), c.Author.When.String(), c.Author.Name)
				delete(toIterate, k)
				continue
			}

			metaFile, errIt := s.loadYamlFileFromBytes(file, resourceMetaPath)
			if errIt != nil {
				return fmt.Errorf("commit %s > %w", c.Hash, errIt)
			}

			prevVer := sync.GetMetaVersion(metaFile)
			//launchr.Term().Warning().Printfln(prevVer)
			if resourceVersion != prevVer {
				delete(toIterate, k)
				continue
			}

			hashesMap[k].prevHash = c.Hash.String()
			hashesMap[k].prevHashTime = c.Author.When
			hashesMap[k].author = c.Author.Name
		}

		return nil
	})

	if err != nil {
		return timeline, err
	}

	for n, hm := range hashesMap {
		launchr.Term().Printfln("%s - %s - %s - %s", n, hm.prevHash, hm.prevHashTime.String(), hm.author)
		r, _ := namespaceResources.Get(n)
		resourceVersion, err := r.GetVersion()
		if err != nil {
			return timeline, err
		}

		tri := sync.NewTimelineResourcesItem(resourceVersion, hm.prevHash, hm.prevHashTime)
		tri.AddResource(r)

		timeline = sync.AddToTimeline(timeline, tri)
	}

	//if author != repository.Author {
	//	launchr.Term().Warning().Printfln("Non-bump version selected for %s resource", resourceMetaPath)
	//}
	//
	//tri := sync.NewTimelineResourcesItem(resourceVersion, prevHash, prevHashTime)

	return timeline, err
}

func (s *SyncAction) populateTimelineVarsZero(buildInv *sync.Inventory, timeline []sync.TimelineItem, gitPath string) ([]sync.TimelineItem, error) {
	varsFiles, err := buildInv.FindVariablesFiles()
	if err != nil {
		return nil, err
	}

	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return nil, fmt.Errorf("%s - %w", gitPath, err)
	}

	launchr.Term().Info().Printfln("List of vars files:")
	for _, f := range varsFiles {
		launchr.Term().Printfln(f)
		timeline, err = s.findVariableUpdateTimeZero(f, timeline, repo)
		if err != nil {
			return timeline, fmt.Errorf("error > %w", err)
		}
	}

	//updatedVariables, deletedVariables, err := buildInv.GetChangedVariables(modifiedFiles, s.comparisonDir, s.vaultPass)
	//if err != nil {
	//	return timeline, err
	//}
	//
	//if updatedVariables.Len() == 0 && deletedVariables.Len() == 0 {
	//	launchr.Term().Printf("- no variables were updated or deleted\n")
	//	return timeline, nil
	//}
	//
	//repo, err := git.PlainOpen(gitPath)
	//if err != nil {
	//	return nil, err
	//}
	//
	//for _, varName := range updatedVariables.Keys() {
	//	variable, _ := updatedVariables.Get(varName)
	//	ti, errTi := s.findVariableUpdateTime(variable, repo)
	//	if errTi != nil {
	//		launchr.Term().Error().Printfln("find variable `%s` timeline (update) > %s", variable.GetName(), errTi.Error())
	//		launchr.Term().Warning().Printfln("skipping %s", variable.GetName())
	//		continue
	//	}
	//
	//	ti.AddVariable(variable)
	//	timeline = sync.AddToTimeline(timeline, ti)
	//
	//	launchr.Term().Printfln("- %s - new or updated variable from %s", variable.GetName(), variable.GetPath())
	//}

	return timeline, nil
}

func (s *SyncAction) findVariableUpdateTimeZero(varsFile string, timeline []sync.TimelineItem, repo *git.Repository) ([]sync.TimelineItem, error) {
	// @TODO look for several vars during iteration?
	//ref, err := s.ensureVariableIsVersioned(variable, repo)
	//if err != nil {
	//	return nil, fmt.Errorf("ensure versioned > %w", err)
	//}

	ref, err := repo.Head()
	if err != nil {
		return timeline, err
	}

	isVault := sync.IsVaultFile(varsFile)
	varsYaml, err := sync.LoadVariablesFile(varsFile, s.vaultPass, sync.IsVaultFile(varsFile))
	if err != nil {
		return timeline, err
	}

	platform, _, _, err := sync.ProcessResourcePath(varsFile)
	if err != nil {
		return timeline, fmt.Errorf("kind issue")
	}

	variablesMap := sync.NewOrderedMap[*sync.Variable]()
	for k, value := range varsYaml {
		v := sync.NewVariable(varsFile, platform, k, sync.HashString(fmt.Sprint(value)), isVault)
		variablesMap.Set(k, v)
	}

	toIterate := variablesMap.ToDict()
	hashesMap := make(map[string]*hashStruct)

	//@TODO use git log -S'value' -- path/to/file instead of full history search?
	//  or git log -- path/to/file
	//  go-git log -- filepath looks abysmally slow, to research.
	cIter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil, err
	}

	test := len(toIterate)
	err = cIter.ForEach(func(c *object.Commit) error {
		if len(toIterate) == 0 {
			return storer.ErrStop
		}

		if len(toIterate) != test {
			test = len(toIterate)
			launchr.Log().Debug(fmt.Sprintf("Remaining unidentified variables %d", test))
		}

		if ref.Hash().String() == c.Hash.String() {
			for k := range toIterate {
				if _, ok := hashesMap[k]; !ok {
					hashesMap[k] = &hashStruct{}
				}

				hashesMap[k].prevHash = c.Hash.String()
				hashesMap[k].prevHashTime = c.Author.When
				hashesMap[k].author = c.Author.Name
			}
		}

		//launchr.Term().Info().Printfln(c.Hash.String())

		file, errIt := c.File(varsFile)
		if errIt != nil {
			if !errors.Is(errIt, object.ErrFileNotFound) {
				return fmt.Errorf("open file %s in commit %s > %w", varsFile, c.Hash, errIt)
			}

			return storer.ErrStop

			//return fmt.Errorf("something wrong %s > %w", c.Hash.String(), errIt)
		}

		varFile, errIt := s.loadVariablesFileFromBytes(file, varsFile, isVault)
		if errIt != nil {
			if strings.Contains(errIt.Error(), "did not find expected key") || strings.Contains(errIt.Error(), "could not find expected") {
				launchr.Term().Warning().Printfln("Broken file %s in commit %s detected", varsFile, c.Hash.String())
				return nil
			}

			if strings.Contains(errIt.Error(), "invalid password for vault") || strings.Contains(errIt.Error(), "could not find expected") {
				launchr.Term().Warning().Printfln("invalid password for vault - %s in commit %s detected", varsFile, c.Hash.String())
				//return storer.ErrStop
				return nil
			}

			return fmt.Errorf("commit %s > %w", c.Hash, errIt)
		}

		for k, hh := range toIterate {
			if _, ok := hashesMap[k]; !ok {
				hashesMap[k] = &hashStruct{}
			}

			prevVar, exists := varFile[k]
			if !exists {
				// Variable didn't exist before, take current hash as version
				delete(toIterate, k)
				continue
			}

			prevVarHash := sync.HashString(fmt.Sprint(prevVar))
			if hh.GetHash() != prevVarHash {
				// Variable exists, hashes don't match, stop iterating
				delete(toIterate, k)
				continue
			}

			hashesMap[k].prevHash = c.Hash.String()
			hashesMap[k].prevHashTime = c.Author.When
			hashesMap[k].author = c.Author.Name
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	launchr.Term().Info().Printfln("Iterating %s variables", varsFile)
	for n, hm := range hashesMap {
		launchr.Term().Printfln("%s - %s - %s", n, hm.prevHash, hm.prevHashTime.String())
		v, _ := variablesMap.Get(n)

		tri := sync.NewTimelineVariablesItem(hm.prevHash[:13], hm.prevHash, hm.prevHashTime)
		tri.AddVariable(v)

		timeline = sync.AddToTimeline(timeline, tri)
	}

	return timeline, err
}

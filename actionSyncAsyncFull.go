package plasmactlbump

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/launchrctl/compose/compose"
	"github.com/launchrctl/launchr"
	"github.com/skilld-labs/plasmactl-bump/v2/pkg/sync"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	async "sync"
)

// ExecuteFromZeroAsync the sync action to propagate resources' versions.
func (s *SyncAction) ExecuteFromZeroAsync() error {
	s.mx = async.Mutex{}

	err := s.ensureVaultpassExists()
	if err != nil {
		return err
	}

	err = s.propagateZeroAsync()
	if err != nil {
		return err
	}

	if s.saveKeyring {
		err = s.keyring.Save()
	}

	return err
}

func (s *SyncAction) propagateZeroAsync() error {
	inv, err := sync.NewInventory(s.buildDir)
	if err != nil {
		return err
	}

	// build timeline and resources to copy.
	err = s.buildTimelineZeroAsync(inv)
	if err != nil {
		return fmt.Errorf("building timeline > %w", err)
	}

	if len(s.timeline) == 0 {
		launchr.Term().Warning().Println("No resources were found for propagation")
		return nil
	}

	// sort and iterate timeline, create propagation map.
	toPropagate, resourceVersionMap, err := s.buildPropagationMap(inv, s.timeline)
	if err != nil {
		return fmt.Errorf("building propagation map > %w", err)
	}

	history := sync.NewOrderedMap[*sync.Resource]()

	launchr.Term().Warning().Println("Exit timecheck")
	//return nil

	// update resources.
	err = s.updateResources(resourceVersionMap, toPropagate, history)
	if err != nil {
		return fmt.Errorf("propagate > %w", err)
	}

	return nil
}

func (s *SyncAction) buildTimelineZeroAsync(buildInv *sync.Inventory) error {
	s.timeline = sync.CreateTimeline()

	launchr.Term().Info().Printfln("Gathering domain and package resources")
	resourcesMap, packagePathMap, err := s.getResourcesMapsAsync(buildInv)
	if err != nil {
		return fmt.Errorf("build resource map > %w", err)
	}

	launchr.Term().Info().Printfln("Checking resources change")
	err = s.populateTimelineResourcesZeroAsync(resourcesMap, packagePathMap)
	if err != nil {
		return fmt.Errorf("iteraring resources > %w", err)
	}

	launchr.Term().Info().Printfln("Checking variables change")
	err = s.populateTimelineVarsZeroAsync(buildInv, s.domainDir)
	if err != nil {
		return fmt.Errorf("iteraring variables > %w", err)
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

	return nil
}

func (s *SyncAction) populateTimelineResourcesZeroAsync(resources map[string]*sync.OrderedMap[*sync.Resource], packagePathMap map[string]string) error {
	var wg async.WaitGroup

	// Create a context for cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	domains := packagePathMap
	domains[domainNamespace] = s.domainDir

	// Error channel to collect errors
	errorChan := make(chan error, 1)
	maxWorkers := min(runtime.NumCPU(), len(packagePathMap))
	workChan := make(chan map[string]string, len(domains))

	for i := 0; i < maxWorkers; i++ {
		go func(workerID int) {
			for {
				select {
				case <-ctx.Done():
					// Exit if context is canceled
					return
				case domain, ok := <-workChan:
					if !ok {
						return // Channel closed
					}

					if err := s.findResourcesChangeTimeZeroAsync(resources[domain["name"]], domain["path"]); err != nil {
						select {
						case errorChan <- fmt.Errorf("worker %d error processing %s: %w", workerID, domain["name"], err):
							cancel() // Cancel all workers
						default:
						}
						return
					}
					wg.Done()
				}
			}
		}(i)
	}

	// Send work to the workers
	for name, path := range domains {
		wg.Add(1)
		workChan <- map[string]string{"name": name, "path": path}
	}
	close(workChan) // Signal no more work

	// Wait for all goroutines to finish
	go func() {
		wg.Wait()
		close(errorChan) // Close the error channel when all work is done
	}()

	// Collect errors and finish
	for err := range errorChan {
		if err != nil {
			fmt.Println("Error occurred:", err)
			return err
		}
	}

	return nil
}

func (s *SyncAction) findResourcesChangeTimeZeroAsync(namespaceResources *sync.OrderedMap[*sync.Resource], gitPath string) error {
	repo, err := git.PlainOpen(gitPath)
	launchr.Term().Info().Printfln(gitPath)
	if err != nil {
		return fmt.Errorf("%s - %w", gitPath, err)
	}

	//timelineMap := sync.NewOrderedMap[*sync.TimelineItem]()
	//ref, err := s.ensureResourceIsVersioned(resourceVersion, resourceMetaPath, repo)
	//if err != nil {
	//	return fmt.Errorf("ensure versioned > %w", err)
	//}

	ref, err := repo.Head()
	if err != nil {
		return err
	}

	//@TODO use git log -S'value' -- path/to/file instead of full history search?
	//  or git log -- path/to/file
	//  go-git log -- filepath looks abysmally slow, to research.
	// start from the latest commit and iterate to the past
	cIter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return err
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
		return err
	}

	s.mx.Lock()
	for n, hm := range hashesMap {
		launchr.Term().Printfln("%s - %s - %s - %s - %s", gitPath, n, hm.prevHash, hm.prevHashTime.String(), hm.author)
		r, _ := namespaceResources.Get(n)
		resourceVersion, err := r.GetVersion()
		if err != nil {
			return err
		}

		tri := sync.NewTimelineResourcesItem(resourceVersion, hm.prevHash, hm.prevHashTime)
		tri.AddResource(r)

		s.timeline = sync.AddToTimeline(s.timeline, tri)
	}
	s.mx.Unlock()

	//if author != repository.Author {
	//	launchr.Term().Warning().Printfln("Non-bump version selected for %s resource", resourceMetaPath)
	//}
	//
	//tri := sync.NewTimelineResourcesItem(resourceVersion, prevHash, prevHashTime)

	return err
}

func (s *SyncAction) getResourcesMapsAsync(buildInv *sync.Inventory) (map[string]*sync.OrderedMap[*sync.Resource], map[string]string, error) {
	resourcesMap := make(map[string]*sync.OrderedMap[*sync.Resource])
	packagePathMap := make(map[string]string)

	plasmaCompose, err := compose.Lookup(os.DirFS(s.domainDir))
	if err != nil {
		return nil, nil, err
	}

	var priorityOrder []string
	for _, dep := range plasmaCompose.Dependencies {
		pkg := dep.ToPackage(dep.Name)
		tag := pkg.GetTag()
		branch := pkg.GetRef()
		var version string
		if tag != "" {
			version = tag
		} else if branch != "" {
			version = branch
		} else {
			return nil, nil, errors.New("can't find package version")
		}

		packagePathMap[dep.Name] = filepath.Join(s.packagesDir, pkg.GetName(), version)
		priorityOrder = append(priorityOrder, dep.Name)
	}

	priorityOrder = append(priorityOrder, domainNamespace)

	maxWorkers := min(runtime.NumCPU(), len(packagePathMap))
	var wg async.WaitGroup
	mx := async.Mutex{}
	workChan := make(chan map[string]string, len(packagePathMap))

	//resourcesMap[domainNamespace], err = s.getResourcesMapFrom(s.domainDir)
	//if err != nil {
	//	return resourcesMap, packagePathMap, err
	//}

	errorChan := make(chan error, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()

		resources, errRes := s.getResourcesMapFrom(s.domainDir)
		if errRes != nil {
			errorChan <- errRes
			return
		}

		mx.Lock()
		defer mx.Unlock()
		resourcesMap[domainNamespace] = resources
	}()

	// Worker pool for package repos
	for i := 0; i < maxWorkers; i++ {
		go func() {
			for repo := range workChan {
				resources, errRes := s.getResourcesMapFrom(repo["path"])
				if errRes != nil {
					errorChan <- errRes
					return
				}

				mx.Lock()
				mx.Unlock()
				resourcesMap[repo["package"]] = resources
				wg.Done()
			}
		}()
	}

	// Send package repos to the worker pool
	for pkg, path := range packagePathMap {
		wg.Add(1)
		workChan <- map[string]string{"path": path, "package": pkg}
	}

	close(workChan)

	// Wait for completion or error
	go func() {
		wg.Wait()
		close(errorChan)
	}()

	//for name, packagePath := range packagePathMap {
	//	resources, err := s.getResourcesMapFrom(packagePath)
	//	if err != nil {
	//		return nil, nil, err
	//	}
	//	resourcesMap[name] = resources
	//}

	for err = range errorChan {
		if err != nil {
			return nil, nil, err
		}
	}

	//buildResources, err := s.getResourcesMapFrom(s.buildDir)
	buildResources := buildInv.GetResourcesMap()
	if err != nil {
		return nil, nil, err
	}
	for _, resourceName := range buildResources.Keys() {
		conflicts := make(map[string]string)
		for name, resources := range resourcesMap {
			if _, ok := resources.Get(resourceName); ok {
				conflicts[name] = ""
			}
		}

		if len(conflicts) < 2 {
			continue
		}

		buildResourceEntity := sync.NewResource(resourceName, s.buildDir)
		buildVersion, err := buildResourceEntity.GetVersion()
		if err != nil {
			return nil, nil, err
		}

		var sameVersionNamespaces []string
		for conflictingNamespace := range conflicts {
			var conflictEntity *sync.Resource
			if conflictingNamespace == domainNamespace {
				conflictEntity = sync.NewResource(resourceName, s.buildDir)
			} else {
				conflictEntity = sync.NewResource(resourceName, packagePathMap[conflictingNamespace])
			}

			baseVersion, _, err := conflictEntity.GetBaseVersion()
			if err != nil {
				return nil, nil, err
			}

			if baseVersion != buildVersion {
				launchr.Log().Debug("removing resource from namespace as other version was used during composition",
					"resource", resourceName, "version", baseVersion, "buildVersion", buildVersion, "namespace", conflictingNamespace)
				resourcesMap[conflictingNamespace].Unset(resourceName)
			} else {
				sameVersionNamespaces = append(sameVersionNamespaces, conflictingNamespace)
			}
		}

		if len(sameVersionNamespaces) > 1 {
			launchr.Log().Debug("resolving additional strategies conflict for resource", "resource", resourceName)
			var highest string
			for i := len(priorityOrder) - 1; i >= 0; i-- {
				if _, ok := resourcesMap[priorityOrder[i]]; ok {
					highest = priorityOrder[i]
					break
				}
			}

			for i := len(priorityOrder) - 1; i >= 0; i-- {
				if priorityOrder[i] != highest {
					if _, ok := resourcesMap[priorityOrder[i]]; ok {
						resourcesMap[priorityOrder[i]].Unset(resourceName)
					}
				}
			}
		}
	}

	return resourcesMap, packagePathMap, nil
}

func (s *SyncAction) populateTimelineVarsZeroAsync(buildInv *sync.Inventory, gitPath string) error {
	// todo temporary
	domainInv, err := sync.NewInventory(s.domainDir)
	if err != nil {
		return err
	}

	varsFiles, err := domainInv.FindVariablesFiles()
	if err != nil {
		return err
	}

	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return fmt.Errorf("%s - %w", gitPath, err)
	}

	var wg async.WaitGroup

	// Create a context for cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Error channel to collect errors
	errorChan := make(chan error, 1)
	maxWorkers := min(runtime.NumCPU(), len(varsFiles))
	workChan := make(chan string, len(varsFiles))

	for i := 0; i < maxWorkers; i++ {
		go func(workerID int) {
			for {
				select {
				case <-ctx.Done():
					// Exit if context is canceled
					return
				case varsFile, ok := <-workChan:
					if !ok {
						return // Channel closed
					}
					if err = s.findVariableUpdateTimeZeroAsync(varsFile, repo); err != nil {
						select {
						case errorChan <- fmt.Errorf("worker %d error processing %s: %w", workerID, varsFile, err):
							cancel() // Cancel all workers
						default:
						}
						return
					}
					wg.Done()
				}
			}
		}(i)
	}

	// Send work to the workers
	for _, f := range varsFiles {
		wg.Add(1)
		workChan <- f
	}
	close(workChan) // Signal no more work

	// Wait for all goroutines to finish
	go func() {
		wg.Wait()
		close(errorChan) // Close the error channel when all work is done
	}()

	// Collect errors and finish
	for err = range errorChan {
		if err != nil {
			fmt.Println("Error occurred:", err)
			return err
		}
	}

	//launchr.Term().Info().Printfln("List of vars files:")
	//for _, f := range varsFiles {
	//	launchr.Term().Printfln(f)
	//	err = s.findVariableUpdateTimeZeroAsync(f, repo)
	//	if err != nil {
	//		return fmt.Errorf("error > %w", err)
	//	}
	//}

	return nil
}

func (s *SyncAction) findVariableUpdateTimeZeroAsync(varsFile string, _ *git.Repository) error {
	// @TODO look for several vars during iteration?
	//ref, err := s.ensureVariableIsVersioned(variable, repo)
	//if err != nil {
	//	return nil, fmt.Errorf("ensure versioned > %w", err)
	//}

	repo, err := git.PlainOpen(s.domainDir)
	if err != nil {
		return fmt.Errorf("%s - %w", s.domainDir, err)
	}

	ref, err := repo.Head()
	if err != nil {
		return err
	}

	isVault := sync.IsVaultFile(varsFile)
	varsYaml, err := sync.LoadVariablesFile(varsFile, s.vaultPass, sync.IsVaultFile(varsFile))
	if err != nil {
		return err
	}

	platform, _, _, err := sync.ProcessResourcePath(varsFile)
	if err != nil {
		return fmt.Errorf("kind issue")
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
		launchr.Term().Printfln("something")
		return err
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
		return err
	}

	s.mx.Lock()
	launchr.Term().Info().Printfln("Iterating %s variables", varsFile)
	for n, hm := range hashesMap {
		launchr.Term().Printfln("%s - %s - %s", n, hm.prevHash, hm.prevHashTime.String())
		v, _ := variablesMap.Get(n)

		tri := sync.NewTimelineVariablesItem(hm.prevHash[:13], hm.prevHash, hm.prevHashTime)
		tri.AddVariable(v)

		s.timeline = sync.AddToTimeline(s.timeline, tri)
	}
	s.mx.Unlock()

	return err
}

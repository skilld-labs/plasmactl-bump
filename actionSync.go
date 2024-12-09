package plasmactlbump

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	async "sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/launchrctl/compose/compose"
	"github.com/launchrctl/keyring"
	"github.com/launchrctl/launchr"

	"github.com/skilld-labs/plasmactl-bump/v2/pkg/repository"
	"github.com/skilld-labs/plasmactl-bump/v2/pkg/sync"
)

var (
	errMalformedKeyring = errors.New("the keyring is malformed or wrong passphrase provided")
)

const (
	vaultpassKey    = "vaultpass"
	domainNamespace = "domain"
)

// SyncAction is a type representing a resources version synchronization action.
type SyncAction struct {
	// services.
	keyring keyring.Keyring

	// target dirs.
	buildDir    string
	packagesDir string
	domainDir   string

	// internal.
	saveKeyring bool
	timeline    []sync.TimelineItem

	// options.
	dryRun    bool
	vaultPass string
}

type hashStruct struct {
	hash     string
	hashTime time.Time
	author   string
}

// Execute the sync action to propagate resources' versions.
func (s *SyncAction) Execute() error {
	err := s.ensureVaultpassExists()
	if err != nil {
		return err
	}

	err = s.propagate()
	if err != nil {
		return err
	}

	if s.saveKeyring {
		err = s.keyring.Save()
	}

	return err
}

func (s *SyncAction) ensureVaultpassExists() error {
	keyValueItem, errGet := s.getVaultPass(s.vaultPass)
	if errGet != nil {
		return errGet
	}

	s.vaultPass = keyValueItem.Value

	return nil
}

func (s *SyncAction) getVaultPass(vaultpass string) (keyring.KeyValueItem, error) {
	keyValueItem, errGet := s.keyring.GetForKey(vaultpassKey)
	if errGet != nil {
		if errors.Is(errGet, keyring.ErrEmptyPass) {
			return keyValueItem, errGet
		} else if !errors.Is(errGet, keyring.ErrNotFound) {
			launchr.Log().Debug("keyring error", "error", errGet)
			return keyValueItem, errMalformedKeyring
		}

		keyValueItem.Key = vaultpassKey
		keyValueItem.Value = vaultpass

		if keyValueItem.Value == "" {
			launchr.Term().Printf("- Ansible vault password\n")
			err := keyring.RequestKeyValueFromTty(&keyValueItem)
			if err != nil {
				return keyValueItem, err
			}
		}

		err := s.keyring.AddItem(keyValueItem)
		if err != nil {
			return keyValueItem, err
		}
		s.saveKeyring = true
	}

	return keyValueItem, nil
}

func (s *SyncAction) propagate() error {
	s.timeline = sync.CreateTimeline()

	inv, err := sync.NewInventory(s.buildDir)
	if err != nil {
		return err
	}

	err = inv.CalculateVariablesUsage(s.vaultPass)
	if err != nil {
		return err
	}

	err = s.buildTimeline(inv)
	if err != nil {
		return fmt.Errorf("building timeline > %w", err)
	}

	if len(s.timeline) == 0 {
		launchr.Term().Warning().Println("No resources were found for propagation")
		return nil
	}

	toPropagate, resourceVersionMap, err := s.buildPropagationMap(inv, s.timeline)
	if err != nil {
		return fmt.Errorf("building propagation map > %w", err)
	}

	err = s.updateResources(resourceVersionMap, toPropagate)
	if err != nil {
		return fmt.Errorf("propagate > %w", err)
	}

	return nil
}

func (s *SyncAction) buildTimeline(buildInv *sync.Inventory) error {
	launchr.Term().Info().Printfln("Gathering domain and package resources")
	resourcesMap, packagePathMap, err := s.getResourcesMaps(buildInv)
	if err != nil {
		return fmt.Errorf("build resource map > %w", err)
	}

	launchr.Term().Info().Printfln("Checking resources change")
	err = s.populateTimelineResources(resourcesMap, packagePathMap)
	if err != nil {
		return fmt.Errorf("iteraring resources > %w", err)
	}

	launchr.Term().Info().Printfln("Checking variables change")
	err = s.populateTimelineVars()
	if err != nil {
		return fmt.Errorf("iteraring variables > %w", err)
	}

	return nil
}

func (s *SyncAction) getResourcesMaps(buildInv *sync.Inventory) (map[string]*sync.OrderedMap[*sync.Resource], map[string]string, error) {
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

	packagePathMap[domainNamespace] = s.domainDir

	priorityOrder = append(priorityOrder, domainNamespace)

	var wg async.WaitGroup
	var mx async.Mutex

	maxWorkers := min(runtime.NumCPU(), len(packagePathMap))
	workChan := make(chan map[string]string, len(packagePathMap))
	errorChan := make(chan error, 1)

	for i := 0; i < maxWorkers; i++ {
		go func() {
			for repo := range workChan {
				resources, errRes := getResourcesMapFrom(repo["path"])
				if errRes != nil {
					errorChan <- errRes
					return
				}

				mx.Lock()
				resourcesMap[repo["package"]] = resources
				mx.Unlock()
				wg.Done()
			}
		}()
	}

	for pkg, path := range packagePathMap {
		wg.Add(1)
		workChan <- map[string]string{"path": path, "package": pkg}
	}

	close(workChan)

	go func() {
		wg.Wait()
		close(errorChan)
	}()

	for err = range errorChan {
		if err != nil {
			return nil, nil, err
		}
	}

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
			conflictEntity := sync.NewResource(resourceName, packagePathMap[conflictingNamespace])

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

func (s *SyncAction) populateTimelineResources(resources map[string]*sync.OrderedMap[*sync.Resource], packagePathMap map[string]string) error {
	var wg async.WaitGroup
	var mx async.Mutex

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errorChan := make(chan error, 1)
	maxWorkers := min(runtime.NumCPU(), len(packagePathMap))
	workChan := make(chan map[string]string, len(packagePathMap))

	for i := 0; i < maxWorkers; i++ {
		go func(workerID int) {
			for {
				select {
				case <-ctx.Done():
					return
				case domain, ok := <-workChan:
					if !ok {
						return // Channel closed
					}

					if err := s.findResourcesChangeTime(resources[domain["name"]], domain["path"], &mx); err != nil {
						select {
						case errorChan <- fmt.Errorf("worker %d error processing %s: %w", workerID, domain["name"], err):
							cancel()
						default:
						}
						return
					}
					wg.Done()
				}
			}
		}(i)
	}

	for name, path := range packagePathMap {
		wg.Add(1)
		workChan <- map[string]string{"name": name, "path": path}
	}
	close(workChan)

	go func() {
		wg.Wait()
		close(errorChan)
	}()

	for err := range errorChan {
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *SyncAction) findResourcesChangeTime(namespaceResources *sync.OrderedMap[*sync.Resource], gitPath string, mx *async.Mutex) error {
	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return fmt.Errorf("%s - %w", gitPath, err)
	}

	ref, err := repo.Head()
	if err != nil {
		return err
	}

	// start from the latest commit and iterate to the past
	cIter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return err
	}

	hashesMap := make(map[string]*hashStruct)
	toIterate := namespaceResources.ToDict()

	remainingDebug := len(toIterate)
	err = cIter.ForEach(func(c *object.Commit) error {
		if len(toIterate) == 0 {
			return storer.ErrStop
		}

		if len(toIterate) != remainingDebug {
			remainingDebug = len(toIterate)
			launchr.Log().Debug(fmt.Sprintf("Remaining unidentified resources, %s - %d", gitPath, remainingDebug))
		}

		for k, resource := range toIterate {
			if _, ok := hashesMap[k]; !ok {
				hashesMap[k] = &hashStruct{}
			}

			resourceMetaPath := resource.BuildMetaPath()
			resourceVersion, err := resource.GetVersion()
			if err != nil {
				return err
			}

			file, errIt := c.File(resourceMetaPath)
			if errIt != nil {
				if !errors.Is(errIt, object.ErrFileNotFound) {
					return fmt.Errorf("open file %s in commit %s > %w", resourceMetaPath, c.Hash, errIt)
				}
				if hashesMap[k].hash == "" {
					hashesMap[k].hash = c.Hash.String()
					hashesMap[k].hashTime = c.Author.When
					hashesMap[k].author = c.Author.Name
				}

				launchr.Log().Debug("File didn't exist before, take current hash as version", "version", resourceMetaPath)
				delete(toIterate, k)
				continue
			}

			metaFile, errIt := s.loadYamlFileFromBytes(file, resourceMetaPath)
			if errIt != nil {
				return fmt.Errorf("commit %s > %w", c.Hash, errIt)
			}

			prevVer := sync.GetMetaVersion(metaFile)
			if resourceVersion != prevVer {
				delete(toIterate, k)
				continue
			}

			hashesMap[k].hash = c.Hash.String()
			hashesMap[k].hashTime = c.Author.When
			hashesMap[k].author = c.Author.Name
		}

		return nil
	})

	if err != nil {
		return err
	}

	mx.Lock()
	defer mx.Unlock()

	for n, hm := range hashesMap {
		//launchr.Term().Printfln("%s - %s - %s - %s - %s", gitPath, n, hm.hash, hm.hashTime.String(), hm.author)
		r, _ := namespaceResources.Get(n)
		resourceVersion, err := r.GetVersion()
		if err != nil {
			return err
		}

		if hm.author != repository.Author {
			launchr.Term().Warning().Printfln("Non-bump version selected for %s resource", r.GetName())
		}

		tri := sync.NewTimelineResourcesItem(resourceVersion, hm.hash, hm.hashTime)
		tri.AddResource(r)

		s.timeline = sync.AddToTimeline(s.timeline, tri)
	}

	return err
}

func (s *SyncAction) populateTimelineVars() error {
	filesCrawler := sync.NewFilesCrawler(s.domainDir)
	groupedFiles, err := filesCrawler.FindVarsFiles("")
	if err != nil {
		return err
	}

	var varsFiles []string
	for _, paths := range groupedFiles {
		varsFiles = append(varsFiles, paths...)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg async.WaitGroup
	var mx async.Mutex

	maxWorkers := min(runtime.NumCPU(), len(varsFiles))
	workChan := make(chan string, len(varsFiles))
	errorChan := make(chan error, 1)

	for i := 0; i < maxWorkers; i++ {
		go func(workerID int) {
			for {
				select {
				case <-ctx.Done():
					return
				case varsFile, ok := <-workChan:
					if !ok {
						return // Channel closed
					}
					if err = s.findVariableUpdateTime(varsFile, s.domainDir, &mx); err != nil {
						select {
						case errorChan <- fmt.Errorf("worker %d error processing %s: %w", workerID, varsFile, err):
							cancel()
						default:
						}
						return
					}
					wg.Done()
				}
			}
		}(i)
	}

	for _, f := range varsFiles {
		wg.Add(1)
		workChan <- f
	}
	close(workChan)

	go func() {
		wg.Wait()
		close(errorChan)
	}()

	for err = range errorChan {
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *SyncAction) findVariableUpdateTime(varsFile string, gitPath string, mx *async.Mutex) error {
	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return fmt.Errorf("%s - %w", gitPath, err)
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

	variablesMap := sync.NewOrderedMap[*sync.Variable]()
	for k, value := range varsYaml {
		v := sync.NewVariable(varsFile, k, HashString(fmt.Sprint(value)), isVault)
		variablesMap.Set(k, v)
	}

	toIterate := variablesMap.ToDict()
	hashesMap := make(map[string]*hashStruct)

	cIter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return err
	}

	remainingDebug := len(toIterate)
	err = cIter.ForEach(func(c *object.Commit) error {
		if len(toIterate) == 0 {
			return storer.ErrStop
		}

		if len(toIterate) != remainingDebug {
			remainingDebug = len(toIterate)
			launchr.Log().Debug(fmt.Sprintf("Remaining unidentified variables, %s - %d", varsFile, remainingDebug))
		}

		if ref.Hash().String() == c.Hash.String() {
			for k := range toIterate {
				if _, ok := hashesMap[k]; !ok {
					hashesMap[k] = &hashStruct{}
				}

				hashesMap[k].hash = c.Hash.String()
				hashesMap[k].hashTime = c.Author.When
				hashesMap[k].author = c.Author.Name
			}
		}

		file, errIt := c.File(varsFile)
		if errIt != nil {
			if !errors.Is(errIt, object.ErrFileNotFound) {
				return fmt.Errorf("open file %s in commit %s > %w", varsFile, c.Hash, errIt)
			}

			return storer.ErrStop
		}

		varFile, errIt := s.loadVariablesFileFromBytes(file, varsFile, isVault)
		if errIt != nil {
			if strings.Contains(errIt.Error(), "did not find expected key") || strings.Contains(errIt.Error(), "could not find expected") {
				launchr.Term().Warning().Printfln("Broken file %s in commit %s detected > %w", varsFile, c.Hash.String(), err)
				return nil
			}

			if strings.Contains(errIt.Error(), "invalid password for vault") || strings.Contains(errIt.Error(), "could not find expected") {
				launchr.Term().Warning().Printfln("invalid password for vault - %s in commit %s detected stopping iteration", varsFile, c.Hash.String())
				return storer.ErrStop
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

			prevVarHash := HashString(fmt.Sprint(prevVar))
			if hh.GetHash() != prevVarHash {
				// Variable exists, hashes don't match, stop iterating
				delete(toIterate, k)
				continue
			}

			hashesMap[k].hash = c.Hash.String()
			hashesMap[k].hashTime = c.Author.When
			hashesMap[k].author = c.Author.Name
		}

		return nil
	})

	if err != nil {
		return err
	}

	mx.Lock()
	defer mx.Unlock()

	//launchr.Term().Info().Printfln("Iterating %s variables", varsFile)
	for n, hm := range hashesMap {
		//launchr.Term().Printfln("%s - %s - %s", n, hm.hash, hm.hashTime.String())
		v, _ := variablesMap.Get(n)

		tri := sync.NewTimelineVariablesItem(hm.hash[:13], hm.hash, hm.hashTime)
		tri.AddVariable(v)

		s.timeline = sync.AddToTimeline(s.timeline, tri)
	}

	return err
}

func (s *SyncAction) buildPropagationMap(buildInv *sync.Inventory, timeline []sync.TimelineItem) (*sync.OrderedMap[*sync.Resource], map[string]string, error) {
	resourceVersionMap := make(map[string]string)
	toPropagate := sync.NewOrderedMap[*sync.Resource]()
	resourcesMap := buildInv.GetResourcesMap()

	sync.SortTimeline(timeline)
	launchr.Term().Info().Printfln("Iterating timeline:")
	for _, item := range timeline {
		launchr.Log().Debug("timeline item", "version", item.GetVersion(), "date", item.GetDate(), "commit", item.GetCommit())
		switch i := item.(type) {
		case *sync.TimelineResourcesItem:
			resources := i.GetResources()
			resources.SortKeysAlphabetically()

			for _, key := range resources.Keys() {
				r, ok := resources.Get(key)
				if !ok {
					return nil, nil, fmt.Errorf("unknown key %s detected during timeline iteration", key)
				}

				launchr.Term().Info().Printfln("Collecting %s dependencies", r.GetName())
				dependentResources := buildInv.GetRequiredByResources(r.GetName(), -1)
				for dep := range dependentResources {
					launchr.Term().Printfln("- %s", dep)
					depResource, okR := resourcesMap.Get(dep)
					if !okR {
						continue
					}

					toPropagate.Set(dep, depResource)
					resourceVersionMap[dep] = i.GetVersion()
				}
			}

			for _, key := range resources.Keys() {
				// Ensure new version removes previous propagation for that resource.
				toPropagate.Unset(key)
				delete(resourceVersionMap, key)
			}

		case *sync.TimelineVariablesItem:
			variables := i.GetVariables()
			variables.SortKeysAlphabetically()

			var resources []string
			for _, v := range variables.Keys() {
				vr, err := buildInv.GetVariableResources(v)
				if err != nil {
					return nil, nil, err
				}
				resources = append(resources, vr...)
			}

			slices.Sort(resources)
			resources = slices.Compact(resources)

			for _, r := range resources {
				// First set version for main resource.
				mainResource, okM := resourcesMap.Get(r)
				if !okM {
					launchr.Log().Warn(fmt.Sprintf("skipping %s (direct vars dependency)", r))
					continue
				}
				toPropagate.Set(r, mainResource)
				resourceVersionMap[r] = i.GetVersion()

				// Set versions for dependent resources.
				dependentResources := buildInv.GetRequiredByResources(r, -1)
				for dep := range dependentResources {
					launchr.Term().Printfln("- %s", dep)
					depResource, okR := resourcesMap.Get(dep)
					if !okR {
						launchr.Log().Warn(fmt.Sprintf("skipping %s (dependency of %s)", dep, r))
						continue
					}

					toPropagate.Set(dep, depResource)
					resourceVersionMap[dep] = i.GetVersion()
				}
			}
		}
	}

	return toPropagate, resourceVersionMap, nil
}

func (s *SyncAction) updateResources(resourceVersionMap map[string]string, toPropagate *sync.OrderedMap[*sync.Resource]) error {
	var sortList []string
	updateMap := make(map[string]map[string]string)
	stopPropagation := false

	for _, key := range toPropagate.Keys() {
		r, _ := toPropagate.Get(key)
		baseVersion, currentVersion, errVersion := r.GetBaseVersion()
		if errVersion != nil {
			return errVersion
		}

		if currentVersion == "" {
			launchr.Term().Warning().Printfln("resource %s has no version", r.GetName())
			stopPropagation = true
		}

		newVersion := composeVersion(currentVersion, resourceVersionMap[r.GetName()])
		if baseVersion == resourceVersionMap[r.GetName()] {
			launchr.Log().Debug("skip identical",
				"baseVersion", baseVersion, "currentVersion", currentVersion, "propagateVersion", resourceVersionMap[r.GetName()], "newVersion", newVersion)
			launchr.Term().Warning().Printfln("- skip %s (identical versions)", r.GetName())
			continue
		}

		if _, ok := updateMap[r.GetName()]; !ok {
			updateMap[r.GetName()] = make(map[string]string)
		}

		updateMap[r.GetName()]["new"] = newVersion
		updateMap[r.GetName()]["current"] = currentVersion
		sortList = append(sortList, r.GetName())
	}

	if stopPropagation {
		return errors.New("empty version has been detected, please check log")
	}

	if len(updateMap) == 0 {
		launchr.Term().Printfln("No version to propagate")
		return nil
	}

	sort.Strings(sortList)
	launchr.Term().Info().Printfln("Propagating versions:")
	for _, key := range sortList {
		val := updateMap[key]

		r, ok := toPropagate.Get(key)
		currentVersion := val["current"]
		newVersion := val["new"]
		if !ok {
			return fmt.Errorf("unidentified resource found during update %s", key)
		}

		launchr.Term().Printfln("- %s from %s to %s", r.GetName(), currentVersion, newVersion)
		if s.dryRun {
			continue
		}

		err := r.UpdateVersion(newVersion)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *SyncAction) loadYamlFileFromBytes(file *object.File, path string) (map[string]any, error) {
	reader, errIt := file.Blob.Reader()
	if errIt != nil {
		return nil, fmt.Errorf("can't read %s > %w", path, errIt)
	}

	contents, errIt := io.ReadAll(reader)
	if errIt != nil {
		return nil, fmt.Errorf("can't read %s > %w", path, errIt)
	}

	yamlFile, errIt := sync.LoadYamlFileFromBytes(contents)
	if errIt != nil {
		return nil, fmt.Errorf("YAML load %s > %w", path, errIt)
	}

	return yamlFile, nil
}

func (s *SyncAction) loadVariablesFileFromBytes(file *object.File, path string, isVault bool) (map[string]any, error) {
	reader, errIt := file.Blob.Reader()
	if errIt != nil {
		return nil, fmt.Errorf("can't read %s > %w", path, errIt)
	}

	contents, errIt := io.ReadAll(reader)
	if errIt != nil {
		return nil, fmt.Errorf("can't read %s > %w", path, errIt)
	}

	varFile, errIt := sync.LoadVariablesFileFromBytes(contents, s.vaultPass, isVault)
	if errIt != nil {
		return nil, fmt.Errorf("YAML load %s > %w", path, errIt)
	}

	return varFile, nil
}

func composeVersion(oldVersion string, newVersion string) string {
	var version string
	if len(strings.Split(newVersion, "-")) > 1 {
		version = newVersion
	} else {
		split := strings.Split(oldVersion, "-")
		if len(split) == 1 {
			version = fmt.Sprintf("%s-%s", oldVersion, newVersion)
		} else if len(split) > 1 {
			version = fmt.Sprintf("%s-%s", split[0], newVersion)
		} else {
			version = newVersion
		}
	}

	return version
}

func getResourcesMapFrom(dir string) (*sync.OrderedMap[*sync.Resource], error) {
	inv, err := sync.NewInventory(dir)
	if err != nil {
		return nil, err
	}

	rm := inv.GetResourcesMap()
	rm.SortKeysAlphabetically()
	return rm, nil
}

// HashString is wrapper for hashing string.
func HashString(item string) uint64 {
	return xxhash.Sum64String(item)
}

//func (s *SyncAction) ensureResourceIsVersioned(resourceVersion, resourceMetaPath string, repo *git.Repository) (*plumbing.Reference, error) {
//	ref, err := repo.Head()
//	if err != nil {
//		return nil, err
//	}
//
//	headCommit, err := repo.CommitObject(ref.Hash())
//	if err != nil {
//		return nil, err
//	}
//	headMeta, err := headCommit.File(resourceMetaPath)
//	if err != nil {
//		return nil, fmt.Errorf("meta %s doesn't exist in HEAD commit", resourceMetaPath)
//	}
//
//	metaFile, err := s.loadYamlFileFromBytes(headMeta, resourceMetaPath)
//	if err != nil {
//		return nil, fmt.Errorf("%w", err)
//	}
//
//	headVersion := sync.GetMetaVersion(metaFile)
//	if resourceVersion != headVersion {
//		return nil, fmt.Errorf("version from %s doesn't match any existing commit", resourceMetaPath)
//	}
//
//	return ref, nil
//}

//func (s *SyncAction) ensureResourceNonVersioned(mrn string, repo *git.Repository) error {
//	resourcePath, err := sync.ConvertMRNtoPath(mrn)
//	if err != nil {
//		return err
//	}
//
//	buildPath := filepath.Join(s.buildDir, resourcePath)
//	resourceFiles, err := sync.GetFiles(buildPath, []string{})
//	if err != nil {
//		return err
//	}
//
//	ref, err := repo.Head()
//	if err != nil {
//		return err
//	}
//
//	headCommit, err := repo.CommitObject(ref.Hash())
//	if err != nil {
//		return err
//	}
//
//	for f := range resourceFiles {
//		buildHash, err := sync.HashFileByPath(filepath.Join(buildPath, f))
//		if err != nil {
//			return err
//		}
//
//		launchr.Term().Warning().Printfln(filepath.Join(resourcePath, f))
//		headFile, err := headCommit.File(filepath.Join(resourcePath, f))
//		if err != nil {
//			return err
//		}
//
//		reader, err := headFile.Blob.Reader()
//		if err != nil {
//			return err
//		}
//
//		headHash, err := sync.HashFileFromReader(reader)
//		if err != nil {
//			return err
//		}
//
//		if buildHash != headHash {
//			return fmt.Errorf("resource %s has unversioned changes. You need to commit these changes", mrn)
//		}
//	}
//
//	return nil
//}

//func (s *SyncAction) ensureVariableIsVersioned(variable *sync.Variable, repo *git.Repository) (*plumbing.Reference, error) {
//	ref, err := repo.Head()
//	if err != nil {
//		return nil, err
//	}
//
//	headCommit, err := repo.CommitObject(ref.Hash())
//	if err != nil {
//		return nil, err
//	}
//	headVarsFile, err := headCommit.File(variable.GetPath())
//	if err != nil {
//		return nil, fmt.Errorf("file %s doesn't exist in HEAD", variable.GetPath())
//	}
//
//	varFile, errIt := s.loadVariablesFileFromBytes(headVarsFile, variable.GetPath(), variable.IsVault())
//	if errIt != nil {
//		return nil, fmt.Errorf("%w", errIt)
//	}
//
//	headVar, exists := varFile[variable.GetName()]
//	if !exists {
//		return nil, fmt.Errorf("variable from %s doesn't exist in HEAD", variable.GetPath())
//	}
//
//	headVarHash := sync.HashString(fmt.Sprint(headVar))
//	if variable.GetHash() != headVarHash {
//		return nil, fmt.Errorf("variable from %s is an unversioned change. You need to commit variable change", variable.GetPath())
//	}
//
//	return ref, nil
//}

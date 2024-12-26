package plasmactlbump

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-git/go-git/v5/utils/merkletrie"
	"io"
	"log/slog"
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
	"github.com/pterm/pterm"

	"github.com/skilld-labs/plasmactl-bump/v2/pkg/repository"
	"github.com/skilld-labs/plasmactl-bump/v2/pkg/sync"
)

var (
	errMalformedKeyring = errors.New("the keyring is malformed or wrong passphrase provided")
)

const (
	vaultpassKey    = "vaultpass"
	domainNamespace = "domain"
	buildHackAuthor = "override"
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
	dryRun        bool
	allowOverride bool
	vaultPass     string
	verbosity     int
}

type hashStruct struct {
	hash     string
	hashTime time.Time
	author   string
}

// Execute the sync action to propagate resources' versions.
func (s *SyncAction) Execute() error {
	launchr.Term().Info().Println("Processing propagation...")

	err := s.ensureVaultpassExists()
	if err != nil {
		return err
	}

	err = s.propagate()
	if err != nil {
		return err
	}
	launchr.Term().Info().Println("Propagation has been finished")

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

	launchr.Log().Info("Initializing build inventory")
	inv, err := sync.NewInventory(s.buildDir)
	if err != nil {
		return err
	}

	launchr.Log().Info("Calculating variables usage")
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
	launchr.Log().Info("Gathering domain and package resources")
	resourcesMap, packagePathMap, err := s.getResourcesMaps(buildInv)
	if err != nil {
		return fmt.Errorf("build resource map > %w", err)
	}

	launchr.Log().Info("Populate timeline with resources")
	err = s.populateTimelineResources(resourcesMap, packagePathMap)
	if err != nil {
		return fmt.Errorf("iteraring resources > %w", err)
	}

	return fmt.Errorf("fast exit")

	//launchr.Log().Info("Populate timeline with variables")
	//err = s.populateTimelineVars()
	//if err != nil {
	//	return fmt.Errorf("iteraring variables > %w", err)
	//}

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
		packagePathMap[dep.Name] = filepath.Join(s.packagesDir, pkg.GetName(), pkg.GetTarget())
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
	workChan := make(chan map[string]any, len(packagePathMap))

	multi := pterm.DefaultMultiPrinter

	for i := 0; i < maxWorkers; i++ {
		go func(workerID int) {
			for {
				select {
				case <-ctx.Done():
					return
				case domain, ok := <-workChan:
					if !ok {
						return
					}

					name := domain["name"].(string)
					path := domain["path"].(string)
					pb := domain["pb"].(*pterm.ProgressbarPrinter)

					if err := s.findResourcesChangeTime(resources[name], path, &mx, pb); err != nil {
						select {
						case errorChan <- fmt.Errorf("worker %d error processing %s: %w", workerID, name, err):
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

	delete(packagePathMap, domainNamespace)
	//delete(packagePathMap, "plasma-core")
	delete(packagePathMap, "plasma-work")

	for name, path := range packagePathMap {
		if resources[name].Len() == 0 {
			// Skipping packages with 0 composed resources.
			continue
		}

		wg.Add(1)

		var p *pterm.ProgressbarPrinter
		var err error
		if s.verbosity < 1 {
			p, err = pterm.DefaultProgressbar.WithTotal(resources[name].Len()).WithWriter(multi.NewWriter()).Start(fmt.Sprintf("Collecting resources from %s", name))
			if err != nil {
				return err
			}
		}

		workChan <- map[string]any{"name": name, "path": path, "pb": p}
	}
	close(workChan)
	go func() {
		if s.verbosity < 1 {
			_, err := multi.Start()
			if err != nil {
				errorChan <- fmt.Errorf("error starting multi progress bar: %w", err)
			}
		}

		wg.Wait()
		close(errorChan)
	}()

	for err := range errorChan {
		if err != nil {
			return err
		}
	}

	// Sleep to re-render progress bar. Needed to achieve latest state.
	if s.verbosity < 1 {
		time.Sleep(multi.UpdateDelay)
		multi.Stop() //nolint
	}

	return nil
}

func (s *SyncAction) collectCommits(r *git.Repository, resources *sync.OrderedMap[*sync.Resource]) (map[string]bool, error) {
	toIterate := resources.ToDict()

	temp := make(map[string]map[string]string)
	for _, value := range toIterate {
		path := value.BuildMetaPath()

		_, ok := temp[path]
		if !ok {
			temp[path] = make(map[string]string)
		}
	}

	// @todo get commits per files meta/vars to iterate only commits where files were changed.
	result := make(map[string]bool)
	ref, err := r.Head()
	if err != nil {
		return result, err
	}

	// start from the latest commit and iterate to the past
	cIter, err := r.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return result, err
	}

	_ = cIter.ForEach(func(c *object.Commit) error {
		hash := c.Hash.String()
		hash = hash[:13]
		result[hash] = true

		// Get the tree of the current commit
		tree, err := c.Tree()
		if err != nil {
			return fmt.Errorf("Error getting tree for commit %s: %v", c.Hash, err)
		}

		// Get the parent tree (if it exists)
		var parentTree *object.Tree
		if c.NumParents() > 0 {
			parentCommit, err := c.Parents().Next()
			if err != nil {
				return fmt.Errorf("Error getting parent commit: %v", err)
			}
			parentTree, err = parentCommit.Tree()
			if err != nil {
				return fmt.Errorf("Error getting parent tree: %v", err)
			}
		}

		// Get the changes between the two trees
		if parentTree != nil {
			changes, err := object.DiffTree(parentTree, tree)
			if err != nil {
				return fmt.Errorf("Error diffing trees: %v", err)
			}

			for _, change := range changes {
				action, err := change.Action()
				if err != nil {
					return fmt.Errorf("Error getting change action: %v", err)
				}
				var path string

				switch action {
				case merkletrie.Delete:
					path = change.From.Name
				case merkletrie.Modify:
					path = change.From.Name
				case merkletrie.Insert:
					path = change.To.Name
				}

				if _, ok := temp[path]; ok {
					temp[path][c.Hash.String()] = c.Author.When.String()
				}
			}
		}

		return nil
	})

	tests := 0
	for k, v := range temp {
		tests = tests + 1
		launchr.Term().Info().Printfln(k)
		for c, t := range v {
			launchr.Term().Warning().Printfln("%s - %s", t, c)
		}
	}
	launchr.Term().Error().Printfln("%d", tests)

	return result, nil
}

func (s *SyncAction) findResourcesChangeTime(namespaceResources *sync.OrderedMap[*sync.Resource], gitPath string, mx *async.Mutex, p *pterm.ProgressbarPrinter) error {
	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return fmt.Errorf("%s - %w", gitPath, err)
	}

	commitsMap, err := s.collectCommits(repo, namespaceResources)
	if err != nil {
		return err
	}

	// fast exit
	return nil

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
	currentVersions := map[string]string{}

	for k, resource := range toIterate {
		if _, ok := hashesMap[k]; !ok {
			hashesMap[k] = &hashStruct{}
		}

		hashesMap[k].hash = buildHackAuthor
		hashesMap[k].hashTime = time.Now()
		hashesMap[k].author = buildHackAuthor

		buildResource := sync.NewResource(resource.GetName(), s.buildDir)
		version, err := buildResource.GetVersion()
		if err != nil {
			return err
		}

		currentVersions[k] = version
	}

	remainingDebug := len(toIterate)
	err = cIter.ForEach(func(c *object.Commit) error {
		if len(toIterate) == 0 {
			return storer.ErrStop
		}

		if len(toIterate) != remainingDebug {
			remainingDebug = len(toIterate)
			launchr.Log().Debug(fmt.Sprintf("Remaining unidentified resources %d", remainingDebug), slog.String("source", gitPath))
		}

		for k, resource := range toIterate {
			if _, ok := hashesMap[k]; !ok {
				hashesMap[k] = &hashStruct{}
			}

			resourceMetaPath := resource.BuildMetaPath()
			resourceVersion, ok := currentVersions[k]
			if !ok {
				resourceVersion, err = resource.GetVersion()
				if err != nil {
					return err
				}
				currentVersions[k] = resourceVersion
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

				// File didn't exist before, take current hash as version,
				delete(toIterate, k)

				if p != nil {
					p.Increment()
				}

				continue
			}

			metaFile, errIt := s.loadYamlFileFromBytes(file, resourceMetaPath)
			if errIt != nil {
				return fmt.Errorf("commit %s > %w", c.Hash, errIt)
			}

			prevVer := sync.GetMetaVersion(metaFile)
			if resourceVersion != prevVer {
				delete(toIterate, k)
				if p != nil {
					p.Increment()
				}
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

	// Ensure progress bar showing correct progress.
	if p != nil && p.Total != p.Current {
		p.Add(p.Total - p.Current)
	}

	mx.Lock()
	defer mx.Unlock()

	for n, hm := range hashesMap {
		r, _ := namespaceResources.Get(n)
		resourceVersion := currentVersions[n]

		launchr.Log().Debug("add resource to timeline",
			slog.String("mrn", r.GetName()),
			slog.String("commit", hm.hash),
			slog.String("version", resourceVersion),
			slog.Time("date", hm.hashTime),
		)

		if hm.author == buildHackAuthor {
			msg := fmt.Sprintf("Version of `%s` doesn't match HEAD commit", n)
			if !s.allowOverride {
				return errors.New(msg)
			}

			launchr.Log().Warn(msg)
		} else if hm.author != repository.Author {
			launchr.Log().Warn(fmt.Sprintf("Latest commit of %s is not a bump commit", r.GetName()))
		}

		if _, ok := commitsMap[resourceVersion]; !ok {
			launchr.Log().Warn(fmt.Sprintf("Latest version of `%s` doesn't match any existing commit", r.GetName()))
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

	var p *pterm.ProgressbarPrinter
	if s.verbosity < 1 {
		p, _ = pterm.DefaultProgressbar.WithTotal(len(varsFiles)).WithTitle("Processing variables files").Start()
	}

	for i := 0; i < maxWorkers; i++ {
		go func(workerID int) {
			for {
				select {
				case <-ctx.Done():
					return
				case varsFile, ok := <-workChan:
					if !ok {
						return
					}
					if err = s.findVariableUpdateTime(varsFile, s.domainDir, &mx, p); err != nil {
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

func (s *SyncAction) findVariableUpdateTime(varsFile string, gitPath string, mx *async.Mutex, p *pterm.ProgressbarPrinter) error {
	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return fmt.Errorf("%s - %w", gitPath, err)
	}

	ref, err := repo.Head()
	if err != nil {
		return err
	}

	var varsYaml map[string]any
	hashesMap := make(map[string]*hashStruct)
	variablesMap := sync.NewOrderedMap[*sync.Variable]()
	isVault := sync.IsVaultFile(varsFile)

	varsYaml, err = sync.LoadVariablesFile(filepath.Join(s.buildDir, varsFile), s.vaultPass, isVault)
	if err != nil {
		return err
	}

	for k, value := range varsYaml {
		v := sync.NewVariable(varsFile, k, HashString(fmt.Sprint(value)), isVault)
		variablesMap.Set(k, v)

		if _, ok := hashesMap[k]; !ok {
			hashesMap[k] = &hashStruct{}
		}

		hashesMap[k].hash = fmt.Sprint(v.GetHash())
		hashesMap[k].hashTime = time.Now()
		hashesMap[k].author = buildHackAuthor
	}

	toIterate := variablesMap.ToDict()

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
				launchr.Log().Warn("Bad YAML structured detected",
					slog.String("file", varsFile),
					slog.String("commit", c.Hash.String()),
					slog.String("error", errIt.Error()),
				)

				return nil
			}

			if strings.Contains(errIt.Error(), "invalid password for vault") {
				launchr.Log().Warn("Invalid password for vault",
					slog.String("file", varsFile),
					slog.String("commit", c.Hash.String()),
				)

				return storer.ErrStop
			}

			if strings.Contains(errIt.Error(), "invalid secret format") {
				launchr.Log().Warn("invalid secret format for vault",
					slog.String("file", varsFile),
					slog.String("commit", c.Hash.String()),
				)
				return nil
			}

			return fmt.Errorf("commit %s > %w", c.Hash, errIt)
		}

		for k, hh := range toIterate {
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

	for n, hm := range hashesMap {
		v, _ := variablesMap.Get(n)
		version := hm.hash[:13]
		launchr.Log().Debug("add variable to timeline",
			slog.String("variable", v.GetName()),
			slog.String("version", version),
			slog.Time("date", hm.hashTime),
			slog.String("path", v.GetPath()),
		)

		if hm.author == buildHackAuthor {
			msg := fmt.Sprintf("Value of `%s` doesn't match HEAD commit", n)
			if !s.allowOverride {
				if p != nil {
					p.Stop() //nolint
				}

				return errors.New(msg)
			}

			launchr.Log().Warn(msg)
		}

		tri := sync.NewTimelineVariablesItem(version, hm.hash, hm.hashTime)
		tri.AddVariable(v)

		s.timeline = sync.AddToTimeline(s.timeline, tri)
	}

	if p != nil {
		p.Increment()
	}

	return err
}

func (s *SyncAction) buildPropagationMap(buildInv *sync.Inventory, timeline []sync.TimelineItem) (*sync.OrderedMap[*sync.Resource], map[string]string, error) {
	resourceVersionMap := make(map[string]string)
	toPropagate := sync.NewOrderedMap[*sync.Resource]()
	resourcesMap := buildInv.GetResourcesMap()
	processed := make(map[string]bool)
	sync.SortTimelineDesc(timeline)

	launchr.Log().Info("Iterating timeline")
	for _, item := range timeline {
		dependenciesLog := sync.NewOrderedMap[bool]()

		switch i := item.(type) {
		case *sync.TimelineResourcesItem:
			resources := i.GetResources()
			resources.SortKeysAlphabetically()

			var toProcess []string
			for _, key := range resources.Keys() {
				if processed[key] {
					continue
				}
				toProcess = append(toProcess, key)
			}

			if len(toProcess) == 0 {
				continue
			}

			for _, key := range toProcess {
				r, ok := resources.Get(key)
				if !ok {
					return nil, nil, fmt.Errorf("unknown key %s detected during timeline iteration", key)
				}

				processed[key] = true

				dependentResources := buildInv.GetRequiredByResources(r.GetName(), -1)
				for dep := range dependentResources {
					depResource, okR := resourcesMap.Get(dep)
					if !okR {
						continue
					}

					// Skip resource if it was processed by previous timeline item or previous resource (via deps).
					if processed[dep] {
						continue
					} else {
						processed[dep] = true
					}

					toPropagate.Set(dep, depResource)
					resourceVersionMap[dep] = i.GetVersion()

					if _, okD := resources.Get(dep); !okD {
						dependenciesLog.Set(dep, true)
					}
				}
			}

			for _, key := range toProcess {
				// Ensure new version removes previous propagation for that resource.
				toPropagate.Unset(key)
				delete(resourceVersionMap, key)
			}

			if dependenciesLog.Len() > 0 {
				launchr.Log().Debug("timeline item (resources)",
					slog.String("version", item.GetVersion()),
					slog.Time("date", item.GetDate()),
					slog.String("resources", fmt.Sprintf("%v", toProcess)),
					slog.String("dependencies", fmt.Sprintf("%v", dependenciesLog.Keys())),
				)
			}

		case *sync.TimelineVariablesItem:
			variables := i.GetVariables()
			variables.SortKeysAlphabetically()

			var resources []string
			for _, v := range variables.Keys() {
				variable, _ := variables.Get(v)
				vr, err := buildInv.GetVariableResources(variable.GetName(), variable.GetPlatform())
				if err != nil {
					return nil, nil, err
				}
				resources = append(resources, vr...)
			}

			slices.Sort(resources)
			resources = slices.Compact(resources)

			var toProcess []string
			for _, key := range resources {
				if processed[key] {
					continue
				}
				toProcess = append(toProcess, key)
			}

			if len(toProcess) == 0 {
				continue
			}

			for _, r := range toProcess {
				// First set version for main resource.
				mainResource, okM := resourcesMap.Get(r)
				if !okM {
					launchr.Log().Warn(fmt.Sprintf("skipping not valid resource %s (direct vars dependency)", r))
					continue
				}

				processed[r] = true
				toPropagate.Set(r, mainResource)
				resourceVersionMap[r] = i.GetVersion()
				dependenciesLog.Set(r, true)

				// Set versions for dependent resources.
				dependentResources := buildInv.GetRequiredByResources(r, -1)
				for dep := range dependentResources {
					depResource, okR := resourcesMap.Get(dep)
					if !okR {
						launchr.Log().Warn(fmt.Sprintf("skipping not valid resource %s (dependency of %s)", dep, r))
						continue
					}

					// Skip resource if it was processed by previous timeline item or previous resource (via deps).
					if processed[dep] {
						continue
					} else {
						processed[dep] = true
					}

					toPropagate.Set(dep, depResource)
					resourceVersionMap[dep] = i.GetVersion()

					dependenciesLog.Set(dep, true)
				}
			}

			if dependenciesLog.Len() > 0 {
				launchr.Log().Debug("timeline item (variables)",
					slog.String("version", item.GetVersion()),
					slog.Time("date", item.GetDate()),
					slog.String("variables", fmt.Sprintf("%v", variables.Keys())),
					slog.String("resources", fmt.Sprintf("%v", dependenciesLog.Keys())),
				)
			}
		}
	}

	return toPropagate, resourceVersionMap, nil
}

func (s *SyncAction) updateResources(resourceVersionMap map[string]string, toPropagate *sync.OrderedMap[*sync.Resource]) error {
	var sortList []string
	updateMap := make(map[string]map[string]string)
	stopPropagation := false

	launchr.Log().Info("Sorting resources before update")
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
	launchr.Log().Info("Propagating versions")

	var p *pterm.ProgressbarPrinter
	if s.verbosity < 1 {
		p, _ = pterm.DefaultProgressbar.WithTotal(len(sortList)).WithTitle("Updating resources").Start()
	}
	for _, key := range sortList {
		if p != nil {
			p.Increment()
		}

		val := updateMap[key]

		r, ok := toPropagate.Get(key)
		currentVersion := val["current"]
		newVersion := val["new"]
		if !ok {
			return fmt.Errorf("unidentified resource found during update %s", key)
		}

		launchr.Log().Info(fmt.Sprintf("%s from %s to %s", r.GetName(), currentVersion, newVersion))
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

package plasmactlbump

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
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
	buildDir         string
	comparisonDir    string
	packagesDir      string
	domainDir        string
	artifactsDir     string
	artifactsRepoURL string

	// internal.
	saveKeyring bool

	// options.
	dryRun           bool
	listImpacted     bool
	vaultPass        string
	artifactOverride string
}

// Execute executes the sync action to propagate resources' versions.
func (s *SyncAction) Execute(username, password string) error {
	err := s.prepareArtifact(username, password)
	if err != nil {
		return err
	}

	modifiedFiles, err := sync.CompareDirs(s.buildDir, s.comparisonDir, sync.InventoryExcluded)
	if err != nil {
		return err
	}

	sort.Strings(modifiedFiles)
	launchr.Term().Info().Printf("Build and Artifact diff:\n")
	for _, file := range modifiedFiles {
		launchr.Term().Printfln("- %s", file)
	}

	err = s.ensureVaultpassExists()
	if err != nil {
		return err
	}

	err = s.propagate(modifiedFiles)
	if err != nil {
		return err
	}

	if s.saveKeyring {
		err = s.keyring.Save()
	}

	return err
}

func (s *SyncAction) prepareArtifact(username, password string) error {
	// Get artifact repository credentials or store new.
	ci, errGet := s.keyring.GetForURL(s.artifactsRepoURL)
	if errGet != nil {
		if errors.Is(errGet, keyring.ErrEmptyPass) {
			return errGet
		} else if !errors.Is(errGet, keyring.ErrNotFound) {
			launchr.Log().Debug("error", "error", errGet)
			return errMalformedKeyring
		}

		ci.URL = s.artifactsRepoURL
		ci.Username = username
		ci.Password = password

		if ci.Username == "" || ci.Password == "" {
			launchr.Term().Printfln("Please add login and password for URL - %s\n", ci.URL)
			err := keyring.RequestCredentialsFromTty(&ci)
			if err != nil {
				return err
			}
		}

		err := s.keyring.AddItem(ci)
		if err != nil {
			return err
		}
		s.saveKeyring = true
	}

	artifact, err := sync.NewArtifact(s.artifactsDir, s.artifactsRepoURL, s.artifactOverride, s.comparisonDir)
	if err != nil {
		return err
	}

	err = artifact.Get(ci.Username, ci.Password)
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
			launchr.Log().Debug("error", "error", errGet)
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

func (s *SyncAction) propagate(modifiedFiles []string) error {
	inv, err := sync.NewInventory(s.buildDir)
	if err != nil {
		return err
	}

	// build timeline and resources to copy.
	timeline, history, err := s.buildTimeline(inv, modifiedFiles)
	if err != nil {
		return err
	}

	if len(timeline) == 0 {
		launchr.Term().Warning().Println("No resources were found for propagation")
		return nil
	}

	// sort and iterate timeline, create propagation map.
	toPropagate, resourceVersionMap, err := s.buildPropagationMap(inv, timeline)
	if err != nil {
		return err
	}

	// copy history from artifact.
	err = s.copyHistory(history)
	if err != nil {
		return err
	}

	if s.listImpacted {
		s.printResources("List of impacted resources:", toPropagate)
	}

	// update resources.
	err = s.updateResources(toPropagate, resourceVersionMap)

	return err
}

func (s *SyncAction) findResourceChangeTime(resourceVersion, resourceMetaPath string, repo *git.Repository) (*sync.TimelineResourcesItem, error) {
	ref, err := s.ensureResourceIsVersioned(resourceVersion, resourceMetaPath, repo)
	if err != nil {
		launchr.Log().Error("1")
		return nil, err
	}

	//@TODO use git log -S'value' -- path/to/file instead of full history search?
	//  or git log -- path/to/file
	//  go-git log -- filepath looks abysmally slow, to research.
	// start from the latest commit and iterate to the past
	cIter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		launchr.Log().Error("2")
		return nil, err
	}

	var prevHash string
	var prevHashTime time.Time
	var author string
	err = cIter.ForEach(func(c *object.Commit) error {
		file, err := c.File(resourceMetaPath)
		if err != nil {
			if !errors.Is(err, object.ErrFileNotFound) {
				return err
			}
			if prevHash == "" {
				prevHash = c.Hash.String()
				prevHashTime = c.Author.When
			}

			launchr.Log().Debug("File didn't exist before, take current hash as version", "version", resourceMetaPath)
			return storer.ErrStop
		}

		reader, err := file.Blob.Reader()
		if err != nil {
			return err
		}

		contents, err := io.ReadAll(reader)
		if err != nil {
			return err
		}

		metaFile, err := sync.LoadYamlFileFromBytes(contents)
		if err != nil {
			return err
		}

		prevVer := sync.GetMetaVersion(metaFile)
		if resourceVersion != prevVer {
			return storer.ErrStop
		}

		prevHashTime = c.Author.When
		prevHash = c.Hash.String()
		author = c.Author.Name
		return nil
	})

	if err != nil {
		launchr.Log().Error("3")
		return nil, err
	}

	if author != repository.Author {
		launchr.Term().Warning().Printfln("Non-bump version selected for %s resource", resourceMetaPath)
	}

	tri := sync.NewTimelineResourcesItem(resourceVersion, prevHash, prevHashTime)

	return tri, err
}

func (s *SyncAction) ensureResourceIsVersioned(resourceVersion, resourceMetaPath string, repo *git.Repository) (*plumbing.Reference, error) {
	ref, err := repo.Head()
	if err != nil {
		return nil, err
	}

	headCommit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, err
	}
	headMeta, err := headCommit.File(resourceMetaPath)
	if err != nil {
		return nil, fmt.Errorf("meta %s doesn't exist in HEAD commit", resourceMetaPath)
	}

	reader, err := headMeta.Blob.Reader()
	if err != nil {
		return nil, err
	}

	contents, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	metaFile, err := sync.LoadYamlFileFromBytes(contents)
	if err != nil {
		return nil, err
	}

	headVersion := sync.GetMetaVersion(metaFile)
	if resourceVersion != headVersion {
		return nil, fmt.Errorf("version of %s doesn't match any existing commit", resourceMetaPath)
	}

	return ref, nil
}

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

func (s *SyncAction) findVariableUpdateTime(variable *sync.Variable, repo *git.Repository) (*sync.TimelineVariablesItem, error) {
	//launchr.Log().Debug("find variable update: var, path", "var", variable.GetName(), "path", variable.GetPath())

	// @TODO look for several vars during iteration?
	ref, err := s.ensureVariableIsVersioned(variable, repo)
	if err != nil {
		return nil, err
	}

	//@TODO use git log -S'value' -- path/to/file instead of full history search?
	//  or git log -- path/to/file
	//  go-git log -- filepath looks abysmally slow, to research.
	cIter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil, err
	}

	var currentHash string
	var currentHashTime time.Time
	err = cIter.ForEach(func(c *object.Commit) error {
		file, err := c.File(variable.GetPath())
		if err != nil {
			if !errors.Is(err, object.ErrFileNotFound) {
				return err
			}
			if currentHash == "" {
				currentHash = c.Hash.String()
				currentHashTime = c.Author.When
			}

			return storer.ErrStop
		}

		reader, err := file.Blob.Reader()
		if err != nil {
			return err
		}
		contents, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		varFile, err := sync.LoadVariablesFileFromBytes(contents, s.vaultPass, variable.IsVault())
		if err != nil {
			return err
		}

		prevVar, exists := varFile[variable.GetName()]
		if !exists {
			//cli.Println("Variable didn't exist before, take current hash as version")
			return storer.ErrStop
		}

		prevVarHash := sync.HashString(fmt.Sprint(prevVar))
		if variable.GetHash() != prevVarHash {
			//cli.Println("Variable exists, hashes don't match, stop iterating")
			return storer.ErrStop
		}

		currentHash = c.Hash.String()
		currentHashTime = c.Author.When
		return nil
	})

	tvi := sync.NewTimelineVariablesItem(currentHash[:13], currentHash, currentHashTime)

	return tvi, err
}

func (s *SyncAction) ensureVariableIsVersioned(variable *sync.Variable, repo *git.Repository) (*plumbing.Reference, error) {
	ref, err := repo.Head()
	if err != nil {
		return nil, err
	}

	headCommit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, err
	}
	headVarsFile, err := headCommit.File(variable.GetPath())
	if err != nil {
		return nil, fmt.Errorf("variable %s file %s doesn't exist in HEAD", variable.GetName(), variable.GetPath())
	}

	reader, err := headVarsFile.Blob.Reader()
	if err != nil {
		return nil, err
	}
	contents, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	varFile, err := sync.LoadVariablesFileFromBytes(contents, s.vaultPass, variable.IsVault())
	if err != nil {
		return nil, err
	}

	headVar, exists := varFile[variable.GetName()]
	if !exists {
		return nil, fmt.Errorf("variable %s from %s doesn't exist in HEAD", variable.GetName(), variable.GetPath())
	}

	headVarHash := sync.HashString(fmt.Sprint(headVar))
	if variable.GetHash() != headVarHash {
		return nil, fmt.Errorf("variable %s from %s is an unversioned change. You need to commit this variable change", variable.GetName(), variable.GetPath())
	}

	return ref, nil
}

func (s *SyncAction) findVariableDeletionTime(variable *sync.Variable, repo *git.Repository) (*sync.TimelineVariablesItem, error) {
	//launchr.Log().Debug("find variable delete: var, path", "var", variable.GetName(), "path", variable.GetPath())

	// @TODO look for several vars during iteration?
	// @TODO ensure variable existed at first place, before starting to search.

	ref, err := repo.Head()
	if err != nil {
		return nil, err
	}
	cIter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil, err
	}

	// Ensure variable existed in first place before propagating.
	varExisted := false
	var currentHash string
	var currentHashTime time.Time
	err = cIter.ForEach(func(c *object.Commit) error {
		file, errIt := c.File(variable.GetPath())
		if errors.Is(errIt, object.ErrFileNotFound) {
			currentHash = c.Hash.String()
			currentHashTime = c.Author.When
			return nil
		}

		reader, errIt := file.Blob.Reader()
		if errIt != nil {
			return errIt
		}
		contents, errIt := io.ReadAll(reader)
		if errIt != nil {
			return errIt
		}
		varFile, errIt := sync.LoadVariablesFileFromBytes(contents, s.vaultPass, variable.IsVault())
		if errIt != nil {
			return errIt
		}

		_, exists := varFile[variable.GetName()]
		if !exists {
			currentHash = c.Hash.String()
			currentHashTime = c.Author.When
			return nil
		}

		varExisted = true
		return storer.ErrStop
	})

	if !varExisted {
		return nil, fmt.Errorf("variable %s from %s never existed in repository, please ensure your build is correct", variable.GetName(), variable.GetPath())
	}

	tvi := sync.NewTimelineVariablesItem(currentHash[:13], currentHash, currentHashTime)

	return tvi, err
}

func (s *SyncAction) getResourcesMaps() (map[string]*sync.OrderedMap[bool], map[string]string, error) {
	resourcesMap := make(map[string]*sync.OrderedMap[bool])
	packagePathMap := make(map[string]string)

	buildResources, err := s.getResourcesMapFrom(s.buildDir)
	if err != nil {
		return nil, nil, err
	}
	resourcesMap[domainNamespace], err = s.getResourcesMapFrom(s.domainDir)
	if err != nil {
		return resourcesMap, packagePathMap, err
	}

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

	for name, packagePath := range packagePathMap {
		resources, err := s.getResourcesMapFrom(packagePath)
		if err != nil {
			return nil, nil, err
		}
		resourcesMap[name] = resources
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
				launchr.Log().Debug("removing resource (version) from namespace as other version was used during composition",
					"resource", resourceName, "version", baseVersion, "namespace", conflictingNamespace)
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

func (s *SyncAction) getResourcesMapFrom(dir string) (*sync.OrderedMap[bool], error) {
	inv, err := sync.NewInventory(dir)
	if err != nil {
		return nil, err
	}

	rm := inv.GetResourcesMap()
	rm.SortKeysAlphabetically()
	return rm, nil
}

func (s *SyncAction) buildTimeline(buildInv *sync.Inventory, modifiedFiles []string) ([]sync.TimelineItem, *sync.OrderedMap[*sync.Resource], error) {
	timeline := sync.CreateTimeline()

	allDiffResources := buildInv.GetChangedResources(modifiedFiles)
	if allDiffResources.Len() > 0 {
		allDiffResources.SortKeysAlphabetically()
		launchr.Term().Info().Printfln("Resources diff between build and artifact")
		for _, key := range allDiffResources.Keys() {
			r, ok := allDiffResources.Get(key)
			if !ok {
				continue
			}
			launchr.Term().Printfln("- %s", r.GetName())
		}

		launchr.Term().Info().Printfln("Gathering domain and package resources")
		resourcesMap, packagePathMap, err := s.getResourcesMaps()
		if err != nil {
			return timeline, nil, err
		}

		// Find new or updated resources in diff.
		launchr.Term().Info().Printfln("Checking domain resources")
		timeline, err = s.populateTimelineResources(allDiffResources, resourcesMap[domainNamespace], timeline, s.domainDir)
		if err != nil {
			return nil, nil, err
		}

		// Iterate each package, find new or updated resources in diff.
		launchr.Term().Info().Printfln("Checking packages resources")
		for name, packagePath := range packagePathMap {
			timeline, err = s.populateTimelineResources(allDiffResources, resourcesMap[name], timeline, packagePath)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	launchr.Term().Info().Printfln("Checking variables change")
	timeline, err := s.populateTimelineVars(buildInv, modifiedFiles, timeline, s.domainDir)
	if err != nil {
		return nil, nil, err
	}

	return timeline, allDiffResources, nil
}

func (s *SyncAction) populateTimelineResources(allUpdatedResources *sync.OrderedMap[*sync.Resource], namespaceResources *sync.OrderedMap[bool], timeline []sync.TimelineItem, gitPath string) ([]sync.TimelineItem, error) {
	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		launchr.Log().Debug("error opening git by path", "path", gitPath)
		return nil, err
	}

	for _, resourceName := range namespaceResources.Keys() {
		buildResource, ok := allUpdatedResources.Get(resourceName)
		if !ok {
			continue
		}

		// Return in case when Inventory.GetChangedResources() stops checking IsValidResource().
		//if !domainResource.IsValidResource() {
		//	allUpdatedResources.Unset(domainResource.GetName())
		//	continue
		//}

		buildBaseVersion, buildFullVersion, err := buildResource.GetBaseVersion()
		if err != nil {
			launchr.Log().Debug("not possible to get version for resource", "resource", resourceName)
			return timeline, err
		}

		// If resource doesn't exist in artifact, consider it as new and add to timeline.
		artifactResource := sync.NewResource(buildResource.GetName(), s.comparisonDir)
		if !artifactResource.IsValidResource() {
			ti, errTi := s.findResourceChangeTime(buildBaseVersion, buildResource.BuildMetaPath(), repo)
			if errTi != nil {
				launchr.Log().Debug("error building timeline item (new) for resource", "resource", resourceName)
				return timeline, errTi
			}
			ti.AddResource(buildResource)
			timeline = sync.AddToTimeline(timeline, ti)

			allUpdatedResources.Unset(buildResource.GetName())

			launchr.Term().Printfln("- %s - new resource from %s", buildResource.GetName(), gitPath)
			continue
		}

		artifactBaseVersion, artifactFullVersion, err := artifactResource.GetBaseVersion()
		if err != nil {
			launchr.Log().Debug("error getting base version for resource", "resource", resourceName)
			return timeline, err
		}

		// If domain and artifact resource have different versions, consider it as an update. Push update into timeline.
		if buildBaseVersion != artifactBaseVersion {
			ti, errTi := s.findResourceChangeTime(buildBaseVersion, buildResource.BuildMetaPath(), repo)
			if errTi != nil {
				launchr.Log().Debug("error building timeline item (update) for resource", "resource", resourceName)
				return timeline, errTi
			}
			ti.AddResource(buildResource)

			timeline = sync.AddToTimeline(timeline, ti)
			allUpdatedResources.Unset(buildResource.GetName())

			launchr.Term().Printfln("- %s - updated resource from %s", buildResource.GetName(), gitPath)
			continue
		}

		if buildFullVersion == artifactFullVersion {
			//errNotVersioned := s.ensureResourceNonVersioned(resourceName, repo)
			//if errNotVersioned != nil {
			//	launchr.Term().Printfln("ensureResourceNonVersioned")
			//	return nil, errNotVersioned
			//}

			launchr.Term().Warning().Printfln("- skipping %s (identical build and artifact version)", resourceName)
			allUpdatedResources.Unset(buildResource.GetName())
		}
	}

	return timeline, nil
}

func (s *SyncAction) populateTimelineVars(buildInv *sync.Inventory, modifiedFiles []string, timeline []sync.TimelineItem, gitPath string) ([]sync.TimelineItem, error) {
	updatedVariables, deletedVariables, err := buildInv.GetChangedVariables(modifiedFiles, s.comparisonDir, s.vaultPass)
	if err != nil {
		return timeline, err
	}

	if updatedVariables.Len() == 0 && deletedVariables.Len() == 0 {
		launchr.Term().Printf("- no variables were updated or deleted\n")
		return timeline, nil
	}

	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return nil, err
	}

	for _, varName := range updatedVariables.Keys() {
		variable, _ := updatedVariables.Get(varName)
		ti, errTi := s.findVariableUpdateTime(variable, repo)
		if errTi != nil {
			return timeline, errTi
		}
		ti.AddVariable(variable)
		timeline = sync.AddToTimeline(timeline, ti)

		launchr.Term().Printfln("- %s - new or updated variable from %s", variable.GetName(), variable.GetPath())
	}

	for _, varName := range deletedVariables.Keys() {
		variable, _ := deletedVariables.Get(varName)
		ti, errTi := s.findVariableDeletionTime(variable, repo)
		if errTi != nil {
			return timeline, errTi
		}
		ti.AddVariable(variable)
		timeline = sync.AddToTimeline(timeline, ti)

		launchr.Term().Printfln("- %s - deleted variable from %s", variable.GetName(), variable.GetPath())
	}

	return timeline, nil
}

func (s *SyncAction) buildPropagationMap(buildInv *sync.Inventory, timeline []sync.TimelineItem) (*sync.OrderedMap[*sync.Resource], map[string]string, error) {
	resourceVersionMap := make(map[string]string)
	toPropagate := sync.NewOrderedMap[*sync.Resource]()

	sync.SortTimeline(timeline)
	launchr.Term().Info().Printfln("Iterating timeline:")
	for _, item := range timeline {
		launchr.Log().Debug("version date commit", "version", item.GetVersion(), "date", item.GetDate(), "commit", item.GetCommit())
		switch i := item.(type) {
		case *sync.TimelineResourcesItem:
			resources := i.GetResources()
			resources.SortKeysAlphabetically()
			for _, key := range resources.Keys() {
				r, ok := resources.Get(key)
				if !ok {
					return nil, nil, fmt.Errorf("unknown key %s detected during timeline iteration", key)
				}

				launchr.Term().Info().Printfln("Collecting %s dependencies:", r.GetName())
				err := s.propagateResourceDeps(r, i.GetVersion(), toPropagate, buildInv.GetRequiredMap(), resourceVersionMap)
				if err != nil {
					return toPropagate, resourceVersionMap, err
				}
			}

			for _, key := range resources.Keys() {
				r, _ := resources.Get(key)
				_, ok := toPropagate.Get(r.GetName())
				if ok {
					// Ensure new version removes previous propagation for that resource.
					toPropagate.Unset(r.GetName())
					delete(resourceVersionMap, r.GetName())
				}
			}

		case *sync.TimelineVariablesItem:
			variables := i.GetVariables()
			variables.SortKeysAlphabetically()
			resources, _, err := buildInv.SearchVariablesAffectedResources(variables.ToList())
			if err != nil {
				return toPropagate, resourceVersionMap, err
			}

			launchr.Term().Info().Printfln("Variables:")
			for _, variable := range variables.Keys() {
				launchr.Term().Info().Printfln("- %s", variable)
			}

			launchr.Term().Info().Printfln("Collecting dependencies:")
			version := i.GetVersion()

			resources.SortKeysAlphabetically()
			for _, key := range resources.Keys() {
				r, ok := resources.Get(key)
				if !ok {
					continue
				}

				s.propagateDepsRecursively(r, version, toPropagate, buildInv.GetRequiredMap(), resourceVersionMap)
			}
		}
	}

	return toPropagate, resourceVersionMap, nil
}

func (s *SyncAction) copyHistory(history *sync.OrderedMap[*sync.Resource]) error {
	launchr.Term().Info().Printfln("Copying history from artifact:")
	for _, key := range history.Keys() {
		r, ok := history.Get(key)
		if !ok {
			continue
		}

		// set version from artifact to build dir
		artifactResource := sync.NewResource(r.GetName(), s.comparisonDir)
		if artifactResource.IsValidResource() {
			artifactVersion, err := artifactResource.GetVersion()
			if err != nil {
				return err
			}

			launchr.Term().Printfln("- copy %s - %s", r.GetName(), artifactVersion)
			if s.dryRun {
				continue
			}

			err = r.UpdateVersion(artifactVersion)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *SyncAction) updateResources(toPropagate *sync.OrderedMap[*sync.Resource], resourceVersionMap map[string]string) error {
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

		newVersion := s.composeVersion(currentVersion, resourceVersionMap[r.GetName()])
		if baseVersion == resourceVersionMap[r.GetName()] {
			launchr.Log().Debug("base - current - propagate - new",
				"base", baseVersion, "current", currentVersion, "propagate", resourceVersionMap[r.GetName()], "new", newVersion)
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

		r, _ := toPropagate.Get(key)
		currentVersion := val["current"]
		newVersion := val["new"]

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

func (s *SyncAction) printResources(message string, resources *sync.OrderedMap[*sync.Resource]) {
	if message != "" {
		launchr.Term().Info().Println(message)
	}

	for _, key := range resources.Keys() {
		value, _ := resources.Get(key)
		launchr.Term().Printfln("- %s", value.GetName())
	}
}

//
//func (s *SyncAction) printVariablesInfo(rvm map[string]map[string]bool) {
//	if len(rvm) == 0 {
//		return
//	}
//
//	info := make(map[string][]string)
//	for resourceName, vars := range rvm {
//		for variable := range vars {
//			info[variable] = append(info[variable], resourceName)
//		}
//	}
//
//	log.Info("Modified variables in diff (group_vars and vaults):")
//	for k, v := range info {
//		log.Info("- %s used in: %s", k, strings.Join(v, ", "))
//	}
//}

func (s *SyncAction) propagateResourceDeps(resource *sync.Resource, version string, toPropagate *sync.OrderedMap[*sync.Resource], resourcesGraph map[string]*sync.OrderedMap[bool], resourceVersionMap map[string]string) error {
	if itemsMap, ok := resourcesGraph[resource.GetName()]; ok {
		itemsMap.SortKeysAlphabetically()
		for _, resourceName := range itemsMap.Keys() {
			depResource, resourceExists := toPropagate.Get(resourceName)
			if !resourceExists {
				depResource = sync.NewResource(resourceName, s.buildDir)
				if !depResource.IsValidResource() {
					continue
				}
			}

			s.propagateDepsRecursively(depResource, version, toPropagate, resourcesGraph, resourceVersionMap)
		}
	}

	return nil
}

func (s *SyncAction) propagateDepsRecursively(resource *sync.Resource, version string, toPropagate *sync.OrderedMap[*sync.Resource], resourcesGraph map[string]*sync.OrderedMap[bool], resourceVersionMap map[string]string) {
	if _, ok := toPropagate.Get(resource.GetName()); !ok {
		toPropagate.Set(resource.GetName(), resource)
	}
	resourceVersionMap[resource.GetName()] = version
	launchr.Term().Printfln("- %s", resource.GetName())

	if itemsMap, ok := resourcesGraph[resource.GetName()]; ok {
		itemsMap.SortKeysAlphabetically()
		for _, resourceName := range itemsMap.Keys() {
			depResource, resourceExists := toPropagate.Get(resourceName)
			if !resourceExists {
				depResource = sync.NewResource(resourceName, s.buildDir)
				if !depResource.IsValidResource() {
					continue
				}
			}
			s.propagateDepsRecursively(depResource, version, toPropagate, resourcesGraph, resourceVersionMap)
		}
	}
}

func (s *SyncAction) composeVersion(oldVersion string, newVersion string) string {
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

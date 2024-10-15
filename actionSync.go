package plasmactlbump

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/launchrctl/compose/compose"
	"github.com/launchrctl/keyring"
	"github.com/launchrctl/launchr/pkg/cli"
	"github.com/launchrctl/launchr/pkg/log"
	"gopkg.in/yaml.v3"

	"github.com/skilld-labs/plasmactl-bump/pkg/sync"
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
	log.Info("Build and Artifact diff:")
	for _, file := range modifiedFiles {
		log.Info("- %s", file)
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
			log.Debug("%s", errGet)
			return errMalformedKeyring
		}

		ci.URL = s.artifactsRepoURL
		ci.Username = username
		ci.Password = password

		if ci.Username == "" || ci.Password == "" {
			fmt.Printf("Please add login and password for URL - %s\n", ci.URL)
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
			log.Debug("%s", errGet)
			return keyValueItem, errMalformedKeyring
		}

		keyValueItem.Key = vaultpassKey
		keyValueItem.Value = vaultpass

		if keyValueItem.Value == "" {
			cli.Println("- Ansible vault password")
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
		log.Warn("No resources were found for propagation")
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

	// update resources.
	err = s.updateResources(toPropagate, resourceVersionMap)

	// @todo
	//    determine which resources are coming from artifact and copy their versions from artifact
	//    propagate resources that were changes in commits.
	//    ideally commit by commit.
	//updatedResources, err := s.getResourcesToPropagate(inv, modifiedFiles, hash)
	//if err != nil {
	//	return err
	//}
	//
	//updatedVarResources, resourceVarsMap, variables, err := s.getVarResourcesToPropagate(inv, modifiedFiles)
	//if err != nil {
	//	return err
	//}
	//
	//if updatedResources.Len() == 0 && updatedVarResources.Len() == 0 {
	//	cli.Println("WARNING: No resources were found for propagation")
	//	return nil
	//}
	////panic("123")
	//
	//if updatedResources.Len() > 0 {
	//	updatedResources.OrderBy(inv.GetResourcesOrder())
	//	s.printResources("Resources whose version need to be propagated:", updatedResources)
	//}
	//
	//if updatedVarResources.Len() > 0 {
	//	updatedVarResources.OrderBy(inv.GetResourcesOrder())
	//	s.printResources("Resources whose version need to be updated and propagated:", updatedVarResources)
	//}
	//
	//log.Info("Collecting resources dependencies:")
	//toPropagate := NewOrderedResourceMap()
	//resourceVersionMap := make(map[string]string)
	//
	//for _, key := range updatedResources.OrderedKeys() {
	//	r, ok := updatedResources.Get(key)
	//	if !ok {
	//		continue
	//	}
	//
	//	errCollectDeps := s.collectResourceDependencies(r, toPropagate, inv.GetRequiredMap(), resourceVersionMap)
	//	if errCollectDeps != nil {
	//		return errCollectDeps
	//	}
	//}
	//
	//for _, key := range updatedVarResources.OrderedKeys() {
	//	r, ok := updatedVarResources.Get(key)
	//	if !ok {
	//		continue
	//	}
	//
	//	// @todo, recheck and improve
	//	// Get version from variable, compare versions by date, select latest one
	//
	//	vars := resourceVarsMap[r.GetName()]
	//	_, version := s.getLatestVersionBetweenVars(vars, variables)
	//	version = version[:13]
	//	cli.Println("Version for resource %s is %s", r.GetName(), version)
	//	cli.Println("Selected from list of %v", vars)
	//	s.collectDependenciesRecursively(r, version, toPropagate, inv.GetRequiredMap(), resourceVersionMap)
	//}

	//cli.Println("%v", resourceVersionMap)

	//s.printVariablesInfo(resourceVarsMap)

	return err
}

//func (s *SyncAction) getResourcesToPropagate(composeInventory *sync.Inventory, modifiedFiles []string, hash *plumbing.Hash) (*sync.OrderedResourceMap, error) {
//	allUpdatedResources := composeInventory.GetChangedResources(modifiedFiles)
//	cli.Println("-----All modified resources-----")
//	for _, key := range allUpdatedResources.OrderedKeys() {
//		r, ok := allUpdatedResources.Get(key)
//		if !ok {
//			continue
//		}
//		cli.Println(r.GetName())
//	}
//
//	//commits := testGitChanges(hash)
//	//var commitsModifiedFiles []string
//	//for i := len(commits) - 1; i >= 0; i-- {
//	//	commit := commits[i]
//	//	commitsModifiedFiles = append(commit.ChangedFiles)
//	//}
//
//	commitsModifiedFiles := []string{}
//
//	updatedResources := sync.NewOrderedResourceMap()
//
//	cli.Println("-----Platform modified resources-----")
//	// @todo filter later by compose results (merge strategies)
//	platformUpdatedResources := composeInventory.GetChangedResources(commitsModifiedFiles)
//	for _, key := range platformUpdatedResources.OrderedKeys() {
//		r, ok := platformUpdatedResources.Get(key)
//		if !ok {
//			continue
//		}
//
//		if _, okBld := allUpdatedResources.Get(key); okBld {
//			allUpdatedResources.Unset(key)
//		}
//
//		// @todo to merge properly in the end instead of creating many OrderedResourceMap?
//		updatedResources.Set(key, r)
//		cli.Println(r.GetName())
//	}
//
//	// @todo compose update?
//	//   prepare compose result file with selected package versions, resolved conflicts between files, dir
//	//   so we can determine from which place resources came (platform code or package code)
//
//	// For now parse compose yaml, collect packages and their versions.
//	// It will allow us to prepare inventories for each entry.
//
//	plasmaCompose, err := composeLookup(os.DirFS("."))
//	if err != nil {
//		panic("error getting plasma compose")
//	}
//
//	// Get list of packages from plasma-compose with their versions.
//	packagePathMap := make(map[string]string)
//	for _, dep := range plasmaCompose.Dependencies {
//		pkg := dep.ToPackage(dep.Name)
//		tag := pkg.GetTag()
//		branch := pkg.GetRef()
//		var version string
//		if tag != "" {
//			version = tag
//		} else if branch != "" {
//			version = branch
//		} else {
//			panic("can't find package version")
//		}
//
//		packagePathMap[dep.Name] = filepath.Join(s.packagesDir, pkg.GetName(), version)
//	}
//
//	// Iterate each package, generate inventory, check if there was change between artifact and build.
//	cli.Println("-----Packages modified resources-----")
//	for name, packagePath := range packagePathMap {
//		cli.Println("---%s - %s---", name, packagePath)
//		packageResources, _ := s.getResourcesMapFrom(packagePath)
//
//		for resourceName := range packageResources {
//			// if resource not in global updated list -> skip it
//			// if updated:
//			// 1) check if resource exists in composed repo, if not - skip
//			// 2) check if version is different
//			//   a) if base version is the same, copy artifact version to build
//			//   b) if base version is different, put resource to propagation
//			if _, okBld := allUpdatedResources.Get(resourceName); !okBld {
//				continue
//			}
//
//			buildResource, _ := allUpdatedResources.Get(resourceName)
//			if !buildResource.IsValidResource() {
//				//  @TODO to recheck Deleted resources skipped.
//				cli.Println("- Deleted resource, to skip %s", buildResource.GetName())
//				allUpdatedResources.Unset(buildResource.GetName())
//				continue
//			}
//
//			artifactResource := sync.NewResource(buildResource.GetName(), s.comparisonDir)
//			if !artifactResource.IsValidResource() {
//				// @TODO to recheck New resource, propagate it
//				cli.Println("- New resource, to propagate %s", buildResource.GetName())
//				updatedResources.Set(buildResource.GetName(), buildResource)
//				allUpdatedResources.Unset(buildResource.GetName())
//				continue
//			}
//
//			buildVersion, err := buildResource.GetBaseVersion()
//			if err != nil {
//				panic(err)
//			}
//
//			artifactVersion, err := artifactResource.GetBaseVersion()
//			if err != nil {
//				panic(err)
//			}
//
//			if buildVersion != artifactVersion {
//				cli.Println("- Base versions are different, to propagate %s", buildResource.GetName())
//				updatedResources.Set(buildResource.GetName(), buildResource)
//				allUpdatedResources.Unset(buildResource.GetName())
//			}
//		}
//
//		cli.Println("")
//	}
//
//	cli.Println("-----Artifact modified resources-----")
//	for _, key := range allUpdatedResources.OrderedKeys() {
//		r, ok := allUpdatedResources.Get(key)
//		if !ok {
//			continue
//		}
//
//		cli.Println(r.GetName())
//		// @todo temporary
//		// set version from artifact to build dir
//
//		artifactResource := sync.NewResource(r.GetName(), s.comparisonDir)
//		if artifactResource.IsValidResource() {
//			artifactVersion, err := artifactResource.GetVersion()
//			if err != nil {
//				return nil, err
//			}
//			cli.Println("- Artifact version of %s is %s", artifactResource.GetName(), artifactVersion)
//			if s.dryRun {
//				continue
//			}
//
//			cli.Println("- Copy version %s", artifactVersion)
//			err = r.UpdateVersion(artifactVersion)
//			if err != nil {
//				return nil, err
//			}
//		}
//	}
//
//	return updatedResources, nil
//}
//
//func (s *SyncAction) getVarResourcesToPropagate(composeInventory *sync.Inventory, modifiedFiles []string) (*sync.OrderedResourceMap, map[string]map[string]bool, map[string]*sync.Variable, error) {
//	variables, _, err := composeInventory.GetChangedVariables(modifiedFiles)
//	if err != nil {
//		panic(err)
//	}
//
//	// find all new versions of updated resources to propagate (commit)
//	cli.Println("\nVariables list:")
//	for _, variable := range variables {
//		_, err := s.FindUpdatedVariableVersion(variable)
//		if err != nil {
//			panic(err)
//		}
//	}
//
//	updatedVarResources, resourceVarsMap, err := composeInventory.GetChangedVarsResources(modifiedFiles)
//	if err != nil {
//		return updatedVarResources, resourceVarsMap, variables, err
//	}
//
//	for _, key := range updatedVarResources.OrderedKeys() {
//		r, ok := updatedVarResources.Get(key)
//		if !ok {
//			continue
//		}
//
//		cli.Println("%s", r.GetName())
//	}
//
//	for outerKey, innerMap := range resourceVarsMap {
//		fmt.Println("Resource: ", outerKey)
//		for innerKey := range innerMap {
//			fmt.Println("    Variable: ", innerKey)
//		}
//	}
//
//	return updatedVarResources, resourceVarsMap, variables, err
//}

func (s *SyncAction) FindResourceChangeTime(resourceVersion, resourceMetaPath string, repo *git.Repository) (*sync.TimelineResourcesItem, error) {
	ref, err := repo.Head()
	if err != nil {
		return nil, err
	}

	// start from the latest commit and iterate to the past
	cIter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil, err
	}

	var prevHash string
	var prevHashTime time.Time
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

			cli.Println("File didn't exist before, take current hash as version")
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
		return nil
	})

	if err != nil {
		return nil, err
	}

	tri := sync.NewTimelineResourcesItem(resourceVersion, prevHash, prevHashTime)

	return tri, err
}

func (s *SyncAction) FindVariableUpdateTime(variable *sync.Variable, repo *git.Repository) (*sync.TimelineVariablesItem, error) {
	ref, err := repo.Head()
	if err != nil {
		return nil, err
	}

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

			cli.Println("File didn't exist before, take current hash as version")
			return storer.ErrStop
		}

		reader, _ := file.Blob.Reader()
		contents, _ := io.ReadAll(reader)
		varFile, err := sync.LoadVariablesFileFromBytes(contents, s.vaultPass, variable.IsVault())
		if err != nil {
			return err
		}

		prevVar, exists := varFile[variable.GetName()]
		if !exists {
			cli.Println("Variable didn't exist before, take current hash as version")
			return storer.ErrStop
		}

		prevVarHash := sync.HashString(fmt.Sprint(prevVar))
		if variable.GetHash() != prevVarHash {
			cli.Println("Variable exists, hashes don't match, stop iterating")
			return storer.ErrStop

		}

		currentHash = c.Hash.String()
		currentHashTime = c.Author.When
		return nil
	})

	tvi := sync.NewTimelineVariablesItem(currentHash, currentHash, currentHashTime)

	return tvi, err
}

func (s *SyncAction) FindVariableDeletionTime(variable *sync.Variable, repo *git.Repository) (*sync.TimelineVariablesItem, error) {
	ref, err := repo.Head()
	if err != nil {
		return nil, err
	}
	cIter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil, err
	}

	var currentHash string
	var currentHashTime time.Time
	err = cIter.ForEach(func(c *object.Commit) error {
		file, err := c.File(variable.GetPath())
		if errors.Is(err, object.ErrFileNotFound) {
			cli.Println("File doesn't exists, continue searching for file with variable")
			currentHash = c.Hash.String()
			currentHashTime = c.Author.When
			return nil
		}

		reader, _ := file.Blob.Reader()
		contents, _ := io.ReadAll(reader)
		varFile, err := sync.LoadVariablesFileFromBytes(contents, s.vaultPass, variable.IsVault())
		if err != nil {
			panic(err)
		}

		_, exists := varFile[variable.GetName()]
		if !exists {
			cli.Println("Variable doesn't exist %s, continue search", currentHash)
			currentHash = c.Hash.String()
			currentHashTime = c.Author.When
			return nil
		}

		cli.Println("Variable exists, stop search")
		return storer.ErrStop
	})

	tvi := sync.NewTimelineVariablesItem(currentHash, currentHash, currentHashTime)

	return tvi, err
}

func (s *SyncAction) getResourcesMaps() (map[string]map[string]bool, map[string]string, error) {
	resourcesMap := make(map[string]map[string]bool)
	packagePathMap := make(map[string]string)

	buildResources, err := s.getResourcesMapFrom(s.buildDir)
	resourcesMap[domainNamespace], err = s.getResourcesMapFrom(s.domainDir)
	if err != nil {
		return resourcesMap, packagePathMap, err
	}

	// For now parse compose yaml, collect packages and their versions.
	// It will allow us to prepare inventories for each entry.
	plasmaCompose, err := composeLookup(os.DirFS(s.domainDir))
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
		resources, _ := s.getResourcesMapFrom(packagePath)
		resourcesMap[name] = resources
	}

	for resourceName := range buildResources {
		conflicts := make(map[string]string)
		for name, resources := range resourcesMap {
			if _, ok := resources[resourceName]; ok {
				conflicts[name] = ""
			}
		}

		if len(conflicts) < 2 {
			continue
		}

		buildResourceEntity := sync.NewResource(resourceName, s.buildDir)
		buildVersion, _ := buildResourceEntity.GetVersion()

		var sameVersionNamespaces []string
		for conflictingNamespace, _ := range conflicts {
			var conflictEntity *sync.Resource
			if conflictingNamespace == domainNamespace {
				conflictEntity = sync.NewResource(resourceName, s.buildDir)
			} else {
				conflictEntity = sync.NewResource(resourceName, packagePathMap[conflictingNamespace])
			}

			baseVersion, _, _ := conflictEntity.GetBaseVersion()

			if baseVersion != buildVersion {
				cli.Println("removing from %s, %s, %s", conflictingNamespace, resourceName, baseVersion)
				delete(resourcesMap[conflictingNamespace], resourceName)
			} else {
				sameVersionNamespaces = append(sameVersionNamespaces, conflictingNamespace)
			}
		}

		if len(sameVersionNamespaces) > 1 {
			// Should be super rare situation
			cli.Println("conflicts for %s", resourceName)
			cli.Println("%v", conflicts)
			cli.Println("same version namespaces: %v", sameVersionNamespaces)

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
						delete(resourcesMap[priorityOrder[i]], resourceName)
					}
				}
			}
		}
	}

	return resourcesMap, packagePathMap, nil
}

// @TODO move to compose as service or leave?
func composeLookup(fsys fs.FS) (*compose.YamlCompose, error) {
	f, err := fs.ReadFile(fsys, "plasma-compose.yaml")
	if err != nil {
		return &compose.YamlCompose{}, errors.New("plasma-compose.yaml doesn't exist")
	}

	cfg, err := parseComposeYaml(f)
	if err != nil {
		return &compose.YamlCompose{}, errors.New("incorrect mapping for plasma-compose.yaml, ensure structure is correct")
	}

	return cfg, nil
}

func parseComposeYaml(input []byte) (*compose.YamlCompose, error) {
	cfg := compose.YamlCompose{}
	err := yaml.Unmarshal(input, &cfg)
	return &cfg, err
}

func (s *SyncAction) getResourcesMapFrom(dir string) (map[string]bool, error) {
	inv, err := sync.NewInventory(dir)
	if err != nil {
		return nil, err
	}

	//@TODO simplify to not crawl all resources with dependencies.
	return inv.GetResourcesMap(), nil
}

func (s *SyncAction) buildTimeline(buildInv *sync.Inventory, modifiedFiles []string) ([]sync.TimelineItem, *sync.OrderedResourceMap, error) {
	var timeline []sync.TimelineItem

	allUpdatedResources := buildInv.GetChangedResources(modifiedFiles)
	cli.Println("-----All different resources between build and artifact-----")
	for _, key := range allUpdatedResources.OrderedKeys() {
		r, ok := allUpdatedResources.Get(key)
		if !ok {
			continue
		}
		cli.Println("- %s", r.GetName())
	}

	if allUpdatedResources.Len() == 0 {
		return timeline, allUpdatedResources, nil
	}

	resourcesMap, packagePathMap, err := s.getResourcesMaps()
	if err != nil {
		return timeline, nil, err
	}

	// Find new or updated resources in diff.
	timeline, err = s.someCoolNameForResTimelineItems(allUpdatedResources, resourcesMap[domainNamespace], timeline, s.domainDir)
	if err != nil {
		return nil, nil, err
	}

	// Iterate each package, generate inventory, check if there were changes between artifact and build.
	cli.Println("")
	cli.Println("-----Packages modified resources-----")
	for name, packagePath := range packagePathMap {
		cli.Println("---%s - %s---", name, packagePath)
		timeline, err = s.someCoolNameForResTimelineItems(allUpdatedResources, resourcesMap[name], timeline, packagePath)
		if err != nil {
			return nil, nil, err
		}
	}

	timeline, err = s.someCoolNameForVarTimelineItems(buildInv, modifiedFiles, timeline, s.domainDir)
	if err != nil {
		return nil, nil, err
	}

	//cli.Println("-----Gathering Changed Variables-----")
	//updatedVariables, deletedVariables, err := buildInv.GetChangedVariables(modifiedFiles, s.comparisonDir, s.vaultPass)
	//if err != nil {
	//	return timeline, nil, err
	//}
	//
	//// find all new versions of updated resources to propagate (commit)
	//cli.Println("Updated or New Variables list:")
	//for _, variable := range updatedVariables {
	//	cli.Println("%s - %s", variable.GetName(), variable.GetPath())
	//
	//	vhi, err := s.FindVariableUpdateTime(variable)
	//	handleError(err)
	//	timeline = sync.AddToTimeline(timeline, vhi)
	//
	//	cli.Println("version of %s from %s is %s, %s", variable.GetName(), variable.GetPath(), vhi.GetVersion(), vhi.GetDate())
	//}
	//
	//cli.Println("-----Gathering Deleted Variables-----")
	//for _, variable := range deletedVariables {
	//	cli.Println("%s - %s", variable.GetName(), variable.GetPath())
	//
	//	vhi, err := s.FindVariableDeletionTime(variable)
	//	handleError(err)
	//	timeline = sync.AddToTimeline(timeline, vhi)
	//
	//	cli.Println("version of %s from %s is %s, %s", variable.GetName(), variable.GetPath(), vhi.GetVersion(), vhi.GetDate())
	//}

	return timeline, allUpdatedResources, nil
}

func (s *SyncAction) someCoolNameForResTimelineItems(allUpdatedResources *sync.OrderedResourceMap, namespaceResources map[string]bool, timeline []sync.TimelineItem, gitPath string) ([]sync.TimelineItem, error) {
	cli.Println("-----Namespace X resources-----")
	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return nil, err
	}

	for resourceName := range namespaceResources {
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
			return timeline, err
		}

		// If resource doesn't exist in artifact, consider it as new and add to timeline.
		artifactResource := sync.NewResource(buildResource.GetName(), s.comparisonDir)
		if !artifactResource.IsValidResource() {
			ti, errTi := s.FindResourceChangeTime(buildBaseVersion, buildResource.BuildMetaPath(), repo)
			if errTi != nil {
				return timeline, errTi
			}
			ti.AddResource(buildResource)
			timeline = sync.AddToTimeline(timeline, ti)

			allUpdatedResources.Unset(buildResource.GetName())
			continue
		}

		artifactBaseVersion, artifactFullVersion, err := artifactResource.GetBaseVersion()
		if err != nil {
			return timeline, err
		}

		// If domain and artifact resource have different versions, consider it as an update. Push update into timeline.
		if buildBaseVersion != artifactBaseVersion {
			cli.Println("- %s - Base versions are different, to propagate", buildResource.GetName())

			ti, errTi := s.FindResourceChangeTime(buildBaseVersion, buildResource.BuildMetaPath(), repo)
			if errTi != nil {
				return timeline, errTi
			}
			ti.AddResource(buildResource)

			timeline = sync.AddToTimeline(timeline, ti)
			allUpdatedResources.Unset(buildResource.GetName())

			continue
		}

		if buildFullVersion == artifactFullVersion {
			log.Warn("resource %s is marked as updated, but version in build (%s) and artifact (%s) are identical. Forgot to bump or non-bump files are updated.", resourceName, buildFullVersion, artifactFullVersion)
			allUpdatedResources.Unset(buildResource.GetName())
		}
	}

	return timeline, nil
}

func (s *SyncAction) someCoolNameForVarTimelineItems(buildInv *sync.Inventory, modifiedFiles []string, timeline []sync.TimelineItem, gitPath string) ([]sync.TimelineItem, error) {
	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return nil, err
	}

	cli.Println("-----Gathering Changed Variables-----")
	updatedVariables, deletedVariables, err := buildInv.GetChangedVariables(modifiedFiles, s.comparisonDir, s.vaultPass)
	if err != nil {
		return timeline, err
	}

	cli.Println("New or Updated Variables list:")
	for _, variable := range updatedVariables {
		cli.Println("%s - %s", variable.GetName(), variable.GetPath())

		ti, errTi := s.FindVariableUpdateTime(variable, repo)
		if errTi != nil {
			return timeline, errTi
		}
		ti.AddVariable(variable)
		timeline = sync.AddToTimeline(timeline, ti)

		cli.Println("version of %s from %s is %s, %s", variable.GetName(), variable.GetPath(), ti.GetVersion(), ti.GetDate())
	}

	cli.Println("-----Gathering Deleted Variables-----")
	for _, variable := range deletedVariables {
		cli.Println("%s - %s", variable.GetName(), variable.GetPath())

		ti, errTi := s.FindVariableDeletionTime(variable, repo)
		if errTi != nil {
			return timeline, errTi
		}
		ti.AddVariable(variable)
		timeline = sync.AddToTimeline(timeline, ti)

		cli.Println("version of %s from %s is %s, %s", variable.GetName(), variable.GetPath(), ti.GetVersion(), ti.GetDate())
	}

	return timeline, nil
}

func (s *SyncAction) buildPropagationMap(buildInv *sync.Inventory, timeline []sync.TimelineItem) (*sync.OrderedResourceMap, map[string]string, error) {
	resourceVersionMap := make(map[string]string)
	toPropagate := sync.NewOrderedResourceMap()

	cli.Println("------TIMELINE-SORTED------")
	sync.SortTimeline(timeline)
	for _, item := range timeline {
		item.Print()
		cli.Println("")
	}
	cli.Println("")

	cli.Println("------Building versions map-----")
	for _, item := range timeline {
		switch i := item.(type) {
		case *sync.TimelineResourcesItem:
			for _, r := range i.GetResources() {
				_, ok := toPropagate.Get(r.GetName())
				if ok {
					// Ensure new version removes previous propagation for that resource.
					toPropagate.Unset(r.GetName())
					delete(resourceVersionMap, r.GetName())
				}

				err := s.collectResourceDependenciesWithVersion(r, i.GetVersion(), toPropagate, buildInv.GetRequiredMap(), resourceVersionMap)
				if err != nil {
					return toPropagate, resourceVersionMap, err
				}
			}
		case *sync.TimelineVariablesItem:
			resources, _, err := buildInv.SearchVariablesAffectedResources(i.GetVariables())
			if err != nil {
				return toPropagate, resourceVersionMap, err
			}
			version := i.GetVersion()[:13]
			for _, key := range resources.OrderedKeys() {
				r, ok := resources.Get(key)
				if !ok {
					continue
				}
				// @todo just apply version ? no need to collect anything because they are already collected.
				s.collectDependenciesRecursively(r, version, toPropagate, buildInv.GetRequiredMap(), resourceVersionMap)
			}
		}
	}

	return toPropagate, resourceVersionMap, nil
}

//func (s *SyncAction) propagateOld(modifiedFiles []string, vaultpass string, hash *plumbing.Hash) error {
//	keyValueItem, errGet := s.keyring.GetForKey(vaultpassKey)
//	if errGet != nil {
//		if errors.Is(errGet, keyring.ErrEmptyPass) {
//			return errGet
//		} else if !errors.Is(errGet, keyring.ErrNotFound) {
//			log.Debug("%s", errGet)
//			return errMalformedKeyring
//		}
//
//		keyValueItem.Key = vaultpassKey
//		keyValueItem.Value = vaultpass
//
//		if keyValueItem.Value == "" {
//			cli.Println("- Ansible vault password")
//			err := keyring.RequestKeyValueFromTty(&keyValueItem)
//			if err != nil {
//				return err
//			}
//		}
//
//		err := s.keyring.AddItem(keyValueItem)
//		if err != nil {
//			return err
//		}
//		s.saveKeyring = true
//	}
//
//	inv, err := sync.NewInventory(keyValueItem.Value, s.buildDir, s.comparisonDir)
//	if err != nil {
//		return err
//	}
//
//	// @todo
//	//    determine which resources are coming from artifact and copy their versions from artifact
//	//    propagate resources that were changes in commits.
//	//    ideally commit by commit.
//	allUpdatedResources := inv.GetChangedResources(modifiedFiles)
//
//	cli.Println("-----All modified resources-----")
//	for _, key := range allUpdatedResources.OrderedKeys() {
//		r, ok := allUpdatedResources.Get(key)
//		if !ok {
//			continue
//		}
//		cli.Println(r.GetName())
//	}
//
//	//commits := testGitChanges(hash)
//	//var commitsmodifiedFiles []string
//	//for i := len(commits) - 1; i >= 0; i-- {
//	//	commit := commits[i]
//	//	commitsmodifiedFiles = append(commit.ChangedFiles)
//	//}
//
//	commitsmodifiedFiles := []string{}
//
//	cli.Println("-----Commits modified resources-----")
//	updatedResources := inv.GetChangedResources(commitsmodifiedFiles)
//	for _, key := range updatedResources.OrderedKeys() {
//		r, ok := updatedResources.Get(key)
//		if !ok {
//			continue
//		}
//
//		if _, okArt := allUpdatedResources.Get(key); okArt {
//			allUpdatedResources.Unset(key)
//		}
//
//		cli.Println(r.GetName())
//	}
//
//	cli.Println("-----Artifact modified resources-----")
//	for _, key := range allUpdatedResources.OrderedKeys() {
//		r, ok := allUpdatedResources.Get(key)
//		if !ok {
//			continue
//		}
//
//		cli.Println(r.GetName())
//		// @todo temporary
//		// set version from artifact to build dir
//
//		artifactResource := sync.NewResource(r.GetName(), s.comparisonDir)
//		if artifactResource.IsValidResource() {
//			artifactVersion, err := artifactResource.GetVersion()
//			if err != nil {
//				return err
//			}
//			cli.Println("Artifact version of %s is %s", artifactResource.GetName(), artifactVersion)
//			if s.dryRun {
//				continue
//			}
//			err = r.UpdateVersion(artifactVersion)
//			cli.Println("Copy version %s", artifactVersion)
//			if err != nil {
//				return err
//			}
//		}
//	}
//
//	updatedVarResources, resourceVarsMap, err := inv.GetChangedVarsResources(modifiedFiles)
//	if err != nil {
//		return err
//	}
//
//	if updatedResources.Len() == 0 && updatedVarResources.Len() == 0 {
//		cli.Println("WARNING: No resources were found for propagation")
//		return nil
//	}
//
//	if updatedResources.Len() > 0 {
//		updatedResources.OrderBy(inv.GetResourcesOrder())
//		s.printResources("Resources whose version need to be propagated:", updatedResources)
//	}
//
//	if updatedVarResources.Len() > 0 {
//		updatedVarResources.OrderBy(inv.GetResourcesOrder())
//		s.printResources("Resources whose version need to be updated and propagated:", updatedVarResources)
//	}
//
//	log.Info("Collecting resources dependencies:")
//	toPropagate := sync.NewOrderedResourceMap()
//	resourceVersionMap := make(map[string]string)
//
//	for _, key := range updatedResources.OrderedKeys() {
//		r, ok := updatedResources.Get(key)
//		if !ok {
//			continue
//		}
//
//		errCollectDeps := s.collectResourceDependencies(r, toPropagate, inv.GetRequiredMap(), resourceVersionMap)
//		if errCollectDeps != nil {
//			return errCollectDeps
//		}
//	}
//
//	for _, key := range updatedVarResources.OrderedKeys() {
//		r, ok := updatedVarResources.Get(key)
//		if !ok {
//			continue
//		}
//
//		// hash tactics
//		// 1. changed var -> resources [ ... ], set changed var hash to all resources
//		// 2. changed var -> dep. var -> resources [ ... ], set changed var hash to all resources
//		// 3. changed var 1 \ -> dep. var -> go to 2
//		//                    -> dev. var -> resources [ ... ] set var_1__var_2 hash to all resources
//		//    changed var 2 / -> dev. var -> go to 2
//		// 4  changer var 1 -> changed dep.var -> resources [ ... ], set changed var1 hash to all resources
//		vars := resourceVarsMap[r.GetName()]
//		version := s.generateVariablesHash(vars)
//		s.collectDependenciesRecursively(r, version, toPropagate, inv.GetRequiredMap(), resourceVersionMap)
//	}
//
//	s.printVariablesInfo(resourceVarsMap)
//	err = s.updateResources(toPropagate, resourceVersionMap)
//
//	return err
//}

func (s *SyncAction) copyHistory(history *sync.OrderedResourceMap) error {
	cli.Println("-----Artifact resources to copy (probably)-----")
	for _, key := range history.OrderedKeys() {
		r, ok := history.Get(key)
		if !ok {
			continue
		}

		cli.Println(r.GetName())

		// @todo temporary
		// set version from artifact to build dir
		artifactResource := sync.NewResource(r.GetName(), s.comparisonDir)
		if artifactResource.IsValidResource() {
			buildVersion, err := r.GetVersion()
			if err != nil {
				return err
			}
			artifactVersion, err := artifactResource.GetVersion()
			if err != nil {
				return err
			}

			cli.Println("- Build version    - %s", buildVersion)
			cli.Println("- Artifact version - %s", artifactVersion)
			if s.dryRun {
				cli.Println("")
				continue
			}

			cli.Println("- Copy version %s", artifactVersion)
			cli.Println("")
			err = r.UpdateVersion(artifactVersion)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *SyncAction) updateResources(toPropagate *sync.OrderedResourceMap, resourceVersionMap map[string]string) error {
	var sortList []string
	updateMap := make(map[string]map[string]string)
	stopPropagation := false

	for _, key := range toPropagate.OrderedKeys() {
		r, _ := toPropagate.Get(key)
		currentVersion, errVersion := r.GetVersion()
		if errVersion != nil {
			return errVersion
		}

		if currentVersion == "" {
			log.Debug("resource %s has no version", r.GetName())
			stopPropagation = true
		}

		newVersion := s.composeVersion(currentVersion, resourceVersionMap[r.GetName()])
		if currentVersion == resourceVersionMap[r.GetName()] {
			log.Debug("- skip %s (identical versions)", r.GetName())
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
		cli.Println("No version to propagate")
		return nil
	}

	sort.Strings(sortList)
	cli.Println("Propagating versions:")
	for _, key := range sortList {
		val := updateMap[key]

		r, _ := toPropagate.Get(key)
		currentVersion := val["current"]
		newVersion := val["new"]

		cli.Println("- %s from %s to %s", r.GetName(), currentVersion, newVersion)
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

func (s *SyncAction) printResources(message string, resources *sync.OrderedResourceMap) {
	if message != "" {
		log.Info(message)
	}

	for _, key := range resources.OrderedKeys() {
		value, _ := resources.Get(key)
		log.Info("- %s", value.GetName())
	}
}

func (s *SyncAction) printVariablesInfo(rvm map[string]map[string]bool) {
	if len(rvm) == 0 {
		return
	}

	info := make(map[string][]string)
	for resourceName, vars := range rvm {
		for variable := range vars {
			info[variable] = append(info[variable], resourceName)
		}
	}

	log.Info("Modified variables in diff (group_vars and vaults):")
	for k, v := range info {
		log.Info("- %s used in: %s", k, strings.Join(v, ", "))
	}
}

//func (s *SyncAction) getLatestVersionBetweenVars(vars map[string]bool, allVars map[string]*sync.Variable) (latestVar string, latestVersion string) {
//	latestDate := time.Time{}
//
//	for v := range vars {
//		if variable, ok := allVars[v]; ok {
//			if variable.version.Date.After(latestDate) {
//				latestDate = variable.version.Date
//				latestVar = v
//				latestVersion = variable.version.Version
//			}
//		}
//	}
//
//	return latestVar, latestVersion
//}
//func (s *SyncAction) generateVariablesHash(vars map[string]bool) string {
//	keys := make([]string, 0, len(vars))
//	for k := range vars {
//		keys = append(keys, k)
//	}
//
//	key := strings.Join(keys, "__")
//	hash := sync.HashString(key)
//	base16Hash := strconv.FormatUint(hash, 16)
//	return "." + base16Hash[:13]
//}

//func (s *SyncAction) collectResourceDependencies(resource *sync.Resource, toPropagate *sync.OrderedResourceMap, resourcesGraph map[string]map[string]bool, resourceVersionMap map[string]string) error {
//	version, err := resource.GetVersion()
//	if err != nil {
//		return err
//	}
//	if version == "" {
//		log.Debug("- %s has an empty version", resource.GetName())
//		return errEmptyVersions
//	}
//
//	if items, ok := resourcesGraph[resource.GetName()]; ok {
//		for resourceName := range items {
//			depResource := sync.NewResource(resourceName, s.buildDir)
//			if !depResource.IsValidResource() {
//				continue
//			}
//			s.collectDependenciesRecursively(depResource, version, toPropagate, resourcesGraph, resourceVersionMap)
//		}
//	}
//
//	return nil
//}

func (s *SyncAction) collectResourceDependenciesWithVersion(resource *sync.Resource, version string, toPropagate *sync.OrderedResourceMap, resourcesGraph map[string]map[string]bool, resourceVersionMap map[string]string) error {
	if items, ok := resourcesGraph[resource.GetName()]; ok {
		for resourceName := range items {
			depResource, resourceExists := toPropagate.Get(resourceName)
			if !resourceExists {
				depResource = sync.NewResource(resourceName, s.buildDir)
				if !depResource.IsValidResource() {
					continue
				}
			}

			s.collectDependenciesRecursively(depResource, version, toPropagate, resourcesGraph, resourceVersionMap)
		}
	}

	return nil
}

func (s *SyncAction) collectDependenciesRecursively(resource *sync.Resource, version string, toPropagate *sync.OrderedResourceMap, resourcesGraph map[string]map[string]bool, resourceVersionMap map[string]string) {
	//_, add := toPropagate.Get(resource.GetName())
	//if add {
	//	return
	//}

	if _, ok := toPropagate.Get(resource.GetName()); !ok {
		log.Info("- Adding %s", resource.GetName())
		toPropagate.Set(resource.GetName(), resource)
	}
	resourceVersionMap[resource.GetName()] = version

	if items, ok := resourcesGraph[resource.GetName()]; ok {
		for resourceName := range items {
			depResource, resourceExists := toPropagate.Get(resourceName)
			if !resourceExists {
				depResource = sync.NewResource(resourceName, s.buildDir)
				if !depResource.IsValidResource() {
					continue
				}
			}
			s.collectDependenciesRecursively(depResource, version, toPropagate, resourcesGraph, resourceVersionMap)
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

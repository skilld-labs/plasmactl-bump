package plasmactlbump

import (
	"errors"
	"fmt"
	vault "github.com/sosedoff/ansible-vault-go"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/launchrctl/compose/compose"
	"gopkg.in/yaml.v3"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/launchrctl/keyring"
	"github.com/launchrctl/launchr/pkg/cli"
	"github.com/launchrctl/launchr/pkg/log"
)

var (
	errEmptyVersions    = errors.New("empty version has been detected, please check log")
	errMalformedKeyring = errors.New("the keyring is malformed or wrong passphrase provided")
)

const (
	vaultpassKey = "vaultpass"
)

// SyncAction is a type representing a resources version synchronization action.
type SyncAction struct {
	sourceDir     string
	comparisonDir string
	packagesDir   string
	dryRun        bool
	keyring       keyring.Keyring
	saveKeyring   bool
	vaultPass     string
}

// Execute executes the sync action by following these steps:
// - Calls the prepareArtifact method to prepare the comparison artifact.
// - Calls the GetDiffFiles function to get the modified files.
// - Prints the modified files.
// - Calls the propagate method to propagate resources' versions.
// - Returns any error that occurs during the execution of the sync action.
func (s *SyncAction) Execute(username, password, override, vaultpass string) error {
	hash, err := s.prepareArtifact(username, password, override)
	if err != nil {
		return err
	}

	modifiedFiles, err := GetDiffFiles(s.sourceDir+"/", s.comparisonDir+"/")
	if err != nil {
		return err
	}

	sort.Strings(modifiedFiles)
	log.Info("Modified files:")
	for _, file := range modifiedFiles {
		log.Info("- %s", file)
	}

	err = s.propagate(modifiedFiles, vaultpass, hash)
	if err != nil {
		return err
	}

	if s.saveKeyring {
		err = s.keyring.Save()
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *SyncAction) prepareArtifact(username, password, override string) (*plumbing.Hash, error) {
	repo, err := getRepo()
	if err != nil {
		return nil, err
	}

	ci, errGet := s.keyring.GetForURL(artifactsRepositoryDomain)
	if errGet != nil {
		if errors.Is(errGet, keyring.ErrEmptyPass) {
			return nil, errGet
		} else if !errors.Is(errGet, keyring.ErrNotFound) {
			log.Debug("%s", errGet)
			return nil, errMalformedKeyring
		}

		ci.URL = artifactsRepositoryDomain
		ci.Username = username
		ci.Password = password

		if ci.Username == "" || ci.Password == "" {
			fmt.Printf("Please add login and password for URL - %s\n", ci.URL)
			err = keyring.RequestCredentialsFromTty(&ci)
			if err != nil {
				return nil, err
			}
		}

		err = s.keyring.AddItem(ci)
		if err != nil {
			return nil, err
		}
		s.saveKeyring = true
	}

	storage := ArtifactStorage{
		repo:     repo,
		username: ci.Username,
		password: ci.Password,
		override: override,
	}

	hash, err := storage.PrepareComparisonArtifact(s.comparisonDir)
	if err != nil {
		return nil, err
	}

	return hash, nil
}

type CommitInfo struct {
	Hash         string
	ChangedFiles []string
	timestamp    time.Time
}

type VersionHistoryItem interface {
	GetVersion() string
	GetDate() time.Time
	Merge(item VersionHistoryItem)
	Print()
}

type ResourceHistoryItem struct {
	version   string
	commit    string
	resources map[string]*Resource
	date      time.Time
}

func (i *ResourceHistoryItem) GetVersion() string {
	return i.version
}

func (i *ResourceHistoryItem) GetDate() time.Time {
	return i.date
}

func (i *ResourceHistoryItem) addResource(r *Resource) {
	i.resources[r.GetName()] = r
}

func (i *ResourceHistoryItem) Merge(item VersionHistoryItem) {
	if r2, ok := item.(*ResourceHistoryItem); ok {
		for k, r := range r2.resources {
			if _, ok = i.resources[k]; !ok {
				i.resources[k] = r
			}
		}
	}
}

func (i *ResourceHistoryItem) Print() {
	cli.Println("Version: %s, Date: %s, Commit: %s", i.GetVersion(), i.GetDate(), i.commit)
	cli.Println("Resource List:")
	for resource, _ := range i.resources {
		cli.Println("- %s", resource)
	}
}

type VariableHistoryItem struct {
	version   string
	commit    string
	variables map[string]*Variable
	date      time.Time
}

func (i *VariableHistoryItem) GetVersion() string {
	return i.version
}

func (i *VariableHistoryItem) GetDate() time.Time {
	return i.date
}

func (i *VariableHistoryItem) addVariable(v *Variable) {
	i.variables[v.name] = v
}

func (i *VariableHistoryItem) Merge(item VersionHistoryItem) {
	if v2, ok := item.(*VariableHistoryItem); ok {
		for k, v := range v2.variables {
			if _, ok = i.variables[k]; !ok {
				i.variables[k] = v
			}
		}
	}
}

func (i *VariableHistoryItem) Print() {
	cli.Println("Version: %s, Date: %s, Commit: %s", i.GetVersion(), i.GetDate(), i.commit)
	cli.Println("Variable List:")
	for variable, _ := range i.variables {
		cli.Println("- %s", variable)
	}
}

func AddToTimeline(list []VersionHistoryItem, item VersionHistoryItem) []VersionHistoryItem {
	for _, i := range list {
		switch i.(type) {
		case *VariableHistoryItem:
			if _, ok := item.(*VariableHistoryItem); !ok {
				continue
			}
		case *ResourceHistoryItem:
			if _, ok := item.(*ResourceHistoryItem); !ok {
				continue
			}
		default:
			continue
		}

		if i.GetVersion() == item.GetVersion() && i.GetDate().Equal(item.GetDate()) {
			i.Merge(item)
			return list
		}
	}

	return append(list, item)
}

func SortListByDate(list []VersionHistoryItem) {
	sort.Slice(list, func(i, j int) bool {
		return list[i].GetDate().Before(list[j].GetDate())
	})
}

func testGitChanges(hash *plumbing.Hash) []*CommitInfo {
	r, err := getRepo()
	handleError(err)

	// Get the HEAD reference
	ref, err := r.git.Head()
	handleError(err)

	// Get an iterator to the commit history
	cIter, err := r.git.Log(&git.LogOptions{From: ref.Hash()})
	handleError(err)

	var commits []*CommitInfo

	err = cIter.ForEach(func(c *object.Commit) error {
		if c.Hash.String() == hash.String() {
			return storer.ErrStop
		}

		if strings.Contains(c.Message, "versions bump") {
			//fmt.Println(c.Hash)
			stats, err := c.Stats()
			handleError(err)

			var files []string
			for _, stat := range stats {
				files = append(files, stat.Name)
			}

			ci := &CommitInfo{
				Hash:         c.Hash.String(),
				ChangedFiles: files,
				timestamp:    c.Author.When,
			}

			commits = append(commits, ci)
		}

		return nil
	})
	handleError(err)

	return commits
}

// handleError is a helper function to handle errors
func handleError(err error) {
	if err != nil {
		panic(err)
	}
}

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

func (s *SyncAction) getResourcesToPropagate(composeInventory *Inventory, modifiedFiles []string, hash *plumbing.Hash) (*OrderedResourceMap, error) {
	allUpdatedResources := composeInventory.GetChangedResources(modifiedFiles)
	cli.Println("-----All modified resources-----")
	for _, key := range allUpdatedResources.OrderedKeys() {
		r, ok := allUpdatedResources.Get(key)
		if !ok {
			continue
		}
		cli.Println(r.GetName())
	}

	commits := testGitChanges(hash)
	var commitsModifiedFiles []string
	for i := len(commits) - 1; i >= 0; i-- {
		commit := commits[i]
		commitsModifiedFiles = append(commit.ChangedFiles)
	}

	updatedResources := NewOrderedResourceMap()

	cli.Println("-----Platform modified resources-----")
	// @todo filter later by compose results (merge strategies)
	platformUpdatedResources := composeInventory.GetChangedResources(commitsModifiedFiles)
	for _, key := range platformUpdatedResources.OrderedKeys() {
		r, ok := platformUpdatedResources.Get(key)
		if !ok {
			continue
		}

		if _, okBld := allUpdatedResources.Get(key); okBld {
			allUpdatedResources.Unset(key)
		}

		// @todo to merge properly in the end instead of creating many OrderedResourceMap?
		updatedResources.Set(key, r)
		cli.Println(r.GetName())
	}

	// @todo compose update?
	//   prepare compose result file with selected package versions, resolved conflicts between files, dir
	//   so we can determine from which place resources came (platform code or package code)

	// For now parse compose yaml, collect packages and their versions.
	// It will allow us to prepare inventories for each entry.

	plasmaCompose, err := composeLookup(os.DirFS("."))
	if err != nil {
		panic("error getting plasma compose")
	}

	// Get list of packages from plasma-compose with their versions.
	packagePathMap := make(map[string]string)
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
			panic("can't find package version")
		}

		packagePathMap[dep.Name] = filepath.Join(s.packagesDir, pkg.GetName(), version)
	}

	// Iterate each package, generate inventory, check if there was change between artifact and build.
	cli.Println("-----Packages modified resources-----")
	for name, packagePath := range packagePathMap {
		cli.Println("---%s - %s---", name, packagePath)
		packageResources, _ := s.getResourcesFrom(packagePath)

		for resourceName := range packageResources {
			// if resource not in global updated list -> skip it
			// if updated:
			// 1) check if resource exists in composed repo, if not - skip
			// 2) check if version is different
			//   a) if base version is the same, copy artifact version to build
			//   b) if base version is different, put resource to propagation
			if _, okBld := allUpdatedResources.Get(resourceName); !okBld {
				continue
			}

			buildResource, _ := allUpdatedResources.Get(resourceName)
			if !buildResource.isValidResource() {
				//  @TODO to recheck Deleted resources skipped.
				cli.Println("- Deleted resource, to skip %s", buildResource.GetName())
				allUpdatedResources.Unset(buildResource.GetName())
				continue
			}

			artifactResource := newResource(buildResource.GetName(), s.comparisonDir)
			if !artifactResource.isValidResource() {
				// @TODO to recheck New resource, propagate it
				cli.Println("- New resource, to propagate %s", buildResource.GetName())
				updatedResources.Set(buildResource.GetName(), buildResource)
				allUpdatedResources.Unset(buildResource.GetName())
				continue
			}

			buildVersion, err := buildResource.GetBaseVersion()
			if err != nil {
				panic(err)
			}

			artifactVersion, err := artifactResource.GetBaseVersion()
			if err != nil {
				panic(err)
			}

			if buildVersion != artifactVersion {
				cli.Println("- Base versions are different, to propagate %s", buildResource.GetName())
				updatedResources.Set(buildResource.GetName(), buildResource)
				allUpdatedResources.Unset(buildResource.GetName())
			}
		}

		cli.Println("")
	}

	cli.Println("-----Artifact modified resources-----")
	for _, key := range allUpdatedResources.OrderedKeys() {
		r, ok := allUpdatedResources.Get(key)
		if !ok {
			continue
		}

		cli.Println(r.GetName())
		// @todo temporary
		// set version from artifact to build dir

		artifactResource := newResource(r.GetName(), s.comparisonDir)
		if artifactResource.isValidResource() {
			artifactVersion, err := artifactResource.GetVersion()
			if err != nil {
				return nil, err
			}
			cli.Println("- Artifact version of %s is %s", artifactResource.GetName(), artifactVersion)
			if s.dryRun {
				continue
			}

			cli.Println("- Copy version %s", artifactVersion)
			err = r.UpdateVersion(artifactVersion)
			if err != nil {
				return nil, err
			}
		}
	}

	return updatedResources, nil
}

func (s *SyncAction) getVarResourcesToPropagate(composeInventory *Inventory, modifiedFiles []string) (*OrderedResourceMap, map[string]map[string]bool, map[string]*Variable, error) {
	variables, _, err := composeInventory.GetChangedVariables(modifiedFiles)
	if err != nil {
		panic(err)
	}

	// find all new versions of updated resources to propagate (commit)
	cli.Println("\nVariables list:")
	for _, variable := range variables {
		cli.Println("%s - %s - %s", variable.name, variable.platform, variable.filepath)

		_, err := s.FindUpdatedVariableVersion(variable)
		if err != nil {
			panic(err)
		}

		cli.Println("version of %s from %s is %s, %s", variable.name, variable.filepath, variable.version.Version, variable.version.Date)
	}

	updatedVarResources, resourceVarsMap, err := composeInventory.GetChangedVarsResources(modifiedFiles)
	if err != nil {
		return updatedVarResources, resourceVarsMap, variables, err
	}

	for _, key := range updatedVarResources.OrderedKeys() {
		r, ok := updatedVarResources.Get(key)
		if !ok {
			continue
		}

		cli.Println("%s", r.GetName())
	}

	for outerKey, innerMap := range resourceVarsMap {
		fmt.Println("Resource: ", outerKey)
		for innerKey := range innerMap {
			fmt.Println("    Variable: ", innerKey)
		}
	}

	return updatedVarResources, resourceVarsMap, variables, err
}

func (s *SyncAction) FindResourceVersion(resource *Resource, path string) (*ResourceHistoryItem, error) {
	repo, err := getRepoByPath(path)
	if err != nil {
		return nil, err
	}

	ref, _ := repo.git.Head()
	// start from the latest commit and iterate to the past
	// @todo iterate bumps ?
	cIter, _ := repo.git.Log(&git.LogOptions{From: ref.Hash()})
	cli.Println("!!!Getting resource history item, iterations are not by BUMP COMMITS, yet")

	//currentHash := variable.hash
	resourceVersion, err := resource.GetVersion()
	handleError(err)
	var prevHash string
	var prevHashTime time.Time
	err = cIter.ForEach(func(c *object.Commit) error {
		//cli.Println("commit hash11 - %s", c.Hash.String())
		file, err := c.File(resource.buildRelativePath())
		if errors.Is(err, object.ErrFileNotFound) {
			cli.Println("File didn't exist before, take current hash as version")
			return storer.ErrStop
		}

		reader, _ := file.Blob.Reader()
		contents, _ := io.ReadAll(reader)
		metaFile, err := s.LoadYamlFileFromBytes(contents)
		if err != nil {
			panic(err)
		}

		prevVer := GetMetaVersion(metaFile)
		if resourceVersion != prevVer {
			//cli.Println("Version don't match, stop iterating")
			return storer.ErrStop
		}

		prevHashTime = c.Author.When
		prevHash = c.Hash.String()
		return nil
	})

	rhi := &ResourceHistoryItem{
		version:   resourceVersion,
		commit:    prevHash,
		date:      prevHashTime,
		resources: make(map[string]*Resource),
	}
	rhi.addResource(resource)

	return rhi, err
}

func (s *SyncAction) FindUpdatedVariableVersion(variable *Variable) (*VariableHistoryItem, error) {
	repo, err := getRepo()
	if err != nil {
		return nil, err
	}

	ref, _ := repo.git.Head()
	cIter, _ := repo.git.Log(&git.LogOptions{From: ref.Hash()})

	//currentHash := variable.hash
	var currentHash string
	var currentHashTime time.Time
	err = cIter.ForEach(func(c *object.Commit) error {
		//cli.Println("commit hash - %s", currentHash)
		file, err := c.File(variable.filepath)
		if errors.Is(err, object.ErrFileNotFound) {
			cli.Println("File didn't exist before, take current hash as version")
			return storer.ErrStop
		}

		reader, _ := file.Blob.Reader()
		contents, _ := io.ReadAll(reader)
		varFile, err := s.LoadVariablesFileFromBytes(contents, variable.isVault)
		if err != nil {
			panic(err)
		}

		prevVar, exists := varFile[variable.name]
		if !exists {
			cli.Println("Variable didn't exist before, take current hash as version")
			return storer.ErrStop
		}

		prevVarHash := HashString(fmt.Sprint(prevVar))
		if variable.hash != prevVarHash {
			cli.Println("Variable exists, hashes don't match, stop iterating")
			return storer.ErrStop

		}

		currentHash = c.Hash.String()
		currentHashTime = c.Author.When
		return nil
	})

	variable.version = &VarVersion{Version: currentHash, Date: currentHashTime}

	vhi := &VariableHistoryItem{
		version:   currentHash,
		commit:    currentHash,
		date:      currentHashTime,
		variables: make(map[string]*Variable),
	}
	vhi.addVariable(variable)

	return vhi, err
}

func (s *SyncAction) FindDeletedVariableVersion(variable *Variable) (*VariableHistoryItem, error) {
	repo, err := getRepo()
	if err != nil {
		return nil, err
	}

	ref, _ := repo.git.Head()
	cIter, _ := repo.git.Log(&git.LogOptions{From: ref.Hash()})

	var currentHash string
	var currentHashTime time.Time
	err = cIter.ForEach(func(c *object.Commit) error {
		//cli.Println("commit hash - %s", currentHash)
		file, err := c.File(variable.filepath)
		if errors.Is(err, object.ErrFileNotFound) {
			cli.Println("File doesn't exists, continue searching for file with variable")
			currentHash = c.Hash.String()
			currentHashTime = c.Author.When
			return nil
		}

		reader, _ := file.Blob.Reader()
		contents, _ := io.ReadAll(reader)
		varFile, err := s.LoadVariablesFileFromBytes(contents, variable.isVault)
		if err != nil {
			panic(err)
		}

		_, exists := varFile[variable.name]
		if !exists {
			cli.Println("Variable doesn't exist %s, continue search", currentHash)
			currentHash = c.Hash.String()
			currentHashTime = c.Author.When
			return nil
		}

		cli.Println("Variable exists, stop search")

		return storer.ErrStop
	})

	//@todo if hash or date is empty, panic or return error

	variable.version = &VarVersion{Version: currentHash, Date: currentHashTime}

	vhi := &VariableHistoryItem{
		version:   currentHash,
		commit:    currentHash,
		date:      currentHashTime,
		variables: make(map[string]*Variable),
	}
	vhi.addVariable(variable)

	return vhi, err
}

func (s *SyncAction) LoadYamlFileFromBytes(input []byte) (map[string]interface{}, error) {
	var data map[string]interface{}
	var rawData []byte
	var err error

	rawData = input
	err = yaml.Unmarshal(rawData, &data)
	return data, err
}

// @todo move to inventory
func (s *SyncAction) LoadVariablesFileFromBytes(input []byte, isVault bool) (map[string]interface{}, error) {
	var data map[string]interface{}
	var rawData []byte
	var err error

	if isVault {
		sourceVault, errDecrypt := vault.Decrypt(string(input), s.vaultPass)
		if errDecrypt != nil {
			if errors.Is(errDecrypt, vault.ErrEmptyPassword) {
				return data, fmt.Errorf("error decrypting vaults, password is blank")
			} else if errors.Is(errDecrypt, vault.ErrInvalidFormat) {
				return data, fmt.Errorf("error decrypting vault, invalid secret format")
			} else if errDecrypt.Error() == "invalid password" {
				return data, fmt.Errorf("invalid password for vault")
			}

			return data, errDecrypt
		}
		rawData = []byte(sourceVault)
	} else {
		rawData = input
	}

	err = yaml.Unmarshal(rawData, &data)
	return data, err
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

func (s *SyncAction) getResourcesFrom(dir string) (map[string]bool, error) {
	inv, err := NewInventory("empty", dir, "")
	if err != nil {
		return nil, err
	}

	resources := inv.GetResourcesMap()
	//for k := range resources {
	//	cli.Println("%s", k)
	//}

	return resources, nil
}

func (s *SyncAction) buildPropagationMap(buildInv *Inventory, modifiedFiles []string) (*OrderedResourceMap, map[string]string) {
	// @TODO
	//   Find list of changed resources
	//   Find and build commits list there changes were introduced
	//   a) domain resources
	//   b) package resources
	//   c) variables change
	//   Copy previously propagated versions from artifact
	//   Iterate commits and create propagation map
	//   return map for real update

	// @todo compose update?
	//   prepare compose result file with selected package versions, resolved conflicts between files, dir
	//   so we can determine from which place resources came (platform code or package code)

	// @todo handle case when variables file was deleted

	var timeline []VersionHistoryItem

	allUpdatedResources := buildInv.GetChangedResources(modifiedFiles)
	cli.Println("-----All different resources between build and artifact-----")
	for _, key := range allUpdatedResources.OrderedKeys() {
		r, ok := allUpdatedResources.Get(key)
		if !ok {
			continue
		}
		cli.Println("- %s", r.GetName())
	}

	//updatedResources := NewOrderedResourceMap()

	cli.Println("")
	domainResources, _ := s.getResourcesFrom(".")
	cli.Println("-----Domain modified resources-----")
	for resourceName := range domainResources {
		// if resource not in global updated list -> skip it
		// if updated:
		// 1) check if resource exists in composed repo, if not - skip
		// 2) check if version is different
		//   a) if base version is the same, copy artifact version to build
		//   b) if base version is different, put resource to propagation
		if _, okDmn := allUpdatedResources.Get(resourceName); !okDmn {
			continue
		}

		domainResource, _ := allUpdatedResources.Get(resourceName)
		if !domainResource.isValidResource() {
			//  @TODO to recheck Deleted resources skipped.
			cli.Println("- %s - Deleted resource, to skip", domainResource.GetName())
			allUpdatedResources.Unset(domainResource.GetName())
			continue
		}

		artifactResource := newResource(domainResource.GetName(), s.comparisonDir)
		if !artifactResource.isValidResource() {
			// @TODO to recheck New resource, propagate it
			cli.Println("- %s - New resource, to propagate", domainResource.GetName())

			rhi, err := s.FindResourceVersion(domainResource, ".")
			handleError(err)
			timeline = AddToTimeline(timeline, rhi)

			//updatedResources.Set(domainResource.GetName(), domainResource)
			allUpdatedResources.Unset(domainResource.GetName())
			continue
		}

		domainVersion, err := domainResource.GetBaseVersion()
		if err != nil {
			panic(err)
		}

		artifactVersion, err := artifactResource.GetBaseVersion()
		if err != nil {
			panic(err)
		}

		if domainVersion != artifactVersion {
			cli.Println("- %s - Base versions are different, to propagate", domainResource.GetName())

			rhi, err := s.FindResourceVersion(domainResource, ".")
			handleError(err)
			timeline = AddToTimeline(timeline, rhi)

			//updatedResources.Set(domainResource.GetName(), domainResource)
			allUpdatedResources.Unset(domainResource.GetName())
		}
	}

	// For now parse compose yaml, collect packages and their versions.
	// It will allow us to prepare inventories for each entry.
	plasmaCompose, err := composeLookup(os.DirFS("."))
	if err != nil {
		panic("error getting plasma compose")
	}

	// Get list of packages from plasma-compose with their versions.
	packagePathMap := make(map[string]string)
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
			panic("can't find package version")
		}

		packagePathMap[dep.Name] = filepath.Join(s.packagesDir, pkg.GetName(), version)
	}

	// Iterate each package, generate inventory, check if there was change between artifact and build.
	cli.Println("")
	cli.Println("-----Packages modified resources-----")
	for name, packagePath := range packagePathMap {
		cli.Println("---%s - %s---", name, packagePath)
		packageResources, _ := s.getResourcesFrom(packagePath)

		for resourceName := range packageResources {
			// if resource not in global updated list -> skip it
			// if updated:
			// 1) check if resource exists in composed repo, if not - skip
			// 2) check if version is different
			//   a) if base version is the same, copy artifact version to build
			//   b) if base version is different, put resource to propagation
			if _, okBld := allUpdatedResources.Get(resourceName); !okBld {
				continue
			}

			buildResource, _ := allUpdatedResources.Get(resourceName)
			if !buildResource.isValidResource() {
				//  @TODO to recheck Deleted resources skipped.
				cli.Println("- %s - Deleted resource, to skip", buildResource.GetName())
				allUpdatedResources.Unset(buildResource.GetName())
				continue
			}

			artifactResource := newResource(buildResource.GetName(), s.comparisonDir)
			if !artifactResource.isValidResource() {
				// @TODO to recheck New resource, propagate it
				cli.Println("- %s - New resource, to propagate", buildResource.GetName())

				rhi, err := s.FindResourceVersion(buildResource, packagePath)
				handleError(err)
				timeline = AddToTimeline(timeline, rhi)

				//updatedResources.Set(buildResource.GetName(), buildResource)
				allUpdatedResources.Unset(buildResource.GetName())
				continue
			}

			buildVersion, err := buildResource.GetBaseVersion()
			if err != nil {
				panic(err)
			}

			artifactVersion, err := artifactResource.GetBaseVersion()
			if err != nil {
				panic(err)
			}

			if buildVersion != artifactVersion {
				cli.Println("- %s - Base versions are different, to propagate", buildResource.GetName())

				rhi, err := s.FindResourceVersion(buildResource, packagePath)
				handleError(err)
				timeline = AddToTimeline(timeline, rhi)

				//updatedResources.Set(buildResource.GetName(), buildResource)
				allUpdatedResources.Unset(buildResource.GetName())
			}
		}

		cli.Println("")
	}

	cli.Println("-----Artifact resources to copy (probably)-----")
	for _, key := range allUpdatedResources.OrderedKeys() {
		r, ok := allUpdatedResources.Get(key)
		if !ok {
			continue
		}

		cli.Println(r.GetName())

		// @todo temporary
		// set version from artifact to build dir
		artifactResource := newResource(r.GetName(), s.comparisonDir)
		if artifactResource.isValidResource() {
			buildVersion, err := r.GetVersion()
			handleError(err)
			artifactVersion, err := artifactResource.GetVersion()
			handleError(err)

			cli.Println("- Build version    - %s", buildVersion)
			cli.Println("- Artifact version - %s", artifactVersion)
			if s.dryRun {
				cli.Println("")
				continue
			}

			cli.Println("- Copy version %s", artifactVersion)
			cli.Println("")
			err = r.UpdateVersion(artifactVersion)
			handleError(err)
		}
	}

	cli.Println("-----Gathering Changed Variables-----")
	updatedVariables, deletedVariables, err := buildInv.GetChangedVariables(modifiedFiles)
	if err != nil {
		panic(err)
	}

	// find all new versions of updated resources to propagate (commit)
	cli.Println("Updated or New Variables list:")
	for _, variable := range updatedVariables {
		cli.Println("%s - %s", variable.name, variable.filepath)

		vhi, err := s.FindUpdatedVariableVersion(variable)
		handleError(err)
		timeline = AddToTimeline(timeline, vhi)

		cli.Println("version of %s from %s is %s, %s", variable.name, variable.filepath, variable.version.Version, variable.version.Date)
	}

	cli.Println("-----Gathering Deleted Variables-----")
	for _, variable := range deletedVariables {
		cli.Println("%s - %s", variable.name, variable.filepath)

		vhi, err := s.FindDeletedVariableVersion(variable)
		handleError(err)
		timeline = AddToTimeline(timeline, vhi)

		cli.Println("version of %s from %s is %s, %s", variable.name, variable.filepath, variable.version.Version, variable.version.Date)
	}

	//cli.Println("------TIMELINE------")
	//for _, item := range timeline {
	//	item.Print()
	//	cli.Println("")
	//}
	//cli.Println("")

	cli.Println("------TIMELINE-SORTED------")
	SortListByDate(timeline)
	for _, item := range timeline {
		item.Print()
		cli.Println("")
	}
	cli.Println("")

	if len(timeline) == 0 {
		return nil, nil
	}

	cli.Println("------Building versions map-----")
	resourceVersionMap := make(map[string]string)
	toPropagate := NewOrderedResourceMap()
	for _, item := range timeline {
		switch i := item.(type) {
		case *ResourceHistoryItem:
			for _, r := range i.resources {
				errCollectDeps := s.collectResourceDependenciesWithVersion(r, i.GetVersion(), toPropagate, buildInv.GetRequiredMap(), resourceVersionMap)
				handleError(errCollectDeps)
			}
		case *VariableHistoryItem:
			resources, _, err := buildInv.SearchVariablesResources(i.variables)
			version := i.GetVersion()[:13]
			for _, key := range resources.OrderedKeys() {
				r, ok := resources.Get(key)
				if !ok {
					continue
				}
				// @todo just apply version ? no need to collect anything because they are already collected.
				s.collectDependenciesRecursively(r, version, toPropagate, buildInv.GetRequiredMap(), resourceVersionMap)
			}
			handleError(err)
		}
	}

	return toPropagate, resourceVersionMap
}

func (s *SyncAction) propagate(modifiedFiles []string, vaultpass string, hash *plumbing.Hash) error {
	keyValueItem, errGet := s.getVaultPass(vaultpass)
	if errGet != nil {
		return errGet
	}

	s.vaultPass = keyValueItem.Value

	inv, err := NewInventory(s.vaultPass, s.sourceDir, s.comparisonDir)
	if err != nil {
		return err
	}

	toPropagate, resourceVersionMap := s.buildPropagationMap(inv, modifiedFiles)

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
	err = s.updateResources(toPropagate, resourceVersionMap)

	return err
}

func (s *SyncAction) propagateOld(modifiedFiles []string, vaultpass string, hash *plumbing.Hash) error {
	keyValueItem, errGet := s.keyring.GetForKey(vaultpassKey)
	if errGet != nil {
		if errors.Is(errGet, keyring.ErrEmptyPass) {
			return errGet
		} else if !errors.Is(errGet, keyring.ErrNotFound) {
			log.Debug("%s", errGet)
			return errMalformedKeyring
		}

		keyValueItem.Key = vaultpassKey
		keyValueItem.Value = vaultpass

		if keyValueItem.Value == "" {
			cli.Println("- Ansible vault password")
			err := keyring.RequestKeyValueFromTty(&keyValueItem)
			if err != nil {
				return err
			}
		}

		err := s.keyring.AddItem(keyValueItem)
		if err != nil {
			return err
		}
		s.saveKeyring = true
	}

	inv, err := NewInventory(keyValueItem.Value, s.sourceDir, s.comparisonDir)
	if err != nil {
		return err
	}

	// @todo
	//    determine which resources are coming from artifact and copy their versions from artifact
	//    propagate resources that were changes in commits.
	//    ideally commit by commit.
	allUpdatedResources := inv.GetChangedResources(modifiedFiles)

	cli.Println("-----All modified resources-----")
	for _, key := range allUpdatedResources.OrderedKeys() {
		r, ok := allUpdatedResources.Get(key)
		if !ok {
			continue
		}
		cli.Println(r.GetName())
	}

	commits := testGitChanges(hash)
	var commitsmodifiedFiles []string
	for i := len(commits) - 1; i >= 0; i-- {
		commit := commits[i]
		commitsmodifiedFiles = append(commit.ChangedFiles)
	}

	cli.Println("-----Commits modified resources-----")
	updatedResources := inv.GetChangedResources(commitsmodifiedFiles)
	for _, key := range updatedResources.OrderedKeys() {
		r, ok := updatedResources.Get(key)
		if !ok {
			continue
		}

		if _, okArt := allUpdatedResources.Get(key); okArt {
			allUpdatedResources.Unset(key)
		}

		cli.Println(r.GetName())
	}

	cli.Println("-----Artifact modified resources-----")
	for _, key := range allUpdatedResources.OrderedKeys() {
		r, ok := allUpdatedResources.Get(key)
		if !ok {
			continue
		}

		cli.Println(r.GetName())
		// @todo temporary
		// set version from artifact to build dir

		artifactResource := newResource(r.GetName(), s.comparisonDir)
		if artifactResource.isValidResource() {
			artifactVersion, err := artifactResource.GetVersion()
			if err != nil {
				return err
			}
			cli.Println("Artifact version of %s is %s", artifactResource.GetName(), artifactVersion)
			if s.dryRun {
				continue
			}
			err = r.UpdateVersion(artifactVersion)
			cli.Println("Copy version %s", artifactVersion)
			if err != nil {
				return err
			}
		}
	}

	updatedVarResources, resourceVarsMap, err := inv.GetChangedVarsResources(modifiedFiles)
	if err != nil {
		return err
	}

	if updatedResources.Len() == 0 && updatedVarResources.Len() == 0 {
		cli.Println("WARNING: No resources were found for propagation")
		return nil
	}

	if updatedResources.Len() > 0 {
		updatedResources.OrderBy(inv.GetResourcesOrder())
		s.printResources("Resources whose version need to be propagated:", updatedResources)
	}

	if updatedVarResources.Len() > 0 {
		updatedVarResources.OrderBy(inv.GetResourcesOrder())
		s.printResources("Resources whose version need to be updated and propagated:", updatedVarResources)
	}

	log.Info("Collecting resources dependencies:")
	toPropagate := NewOrderedResourceMap()
	resourceVersionMap := make(map[string]string)

	for _, key := range updatedResources.OrderedKeys() {
		r, ok := updatedResources.Get(key)
		if !ok {
			continue
		}

		errCollectDeps := s.collectResourceDependencies(r, toPropagate, inv.GetRequiredMap(), resourceVersionMap)
		if errCollectDeps != nil {
			return errCollectDeps
		}
	}

	for _, key := range updatedVarResources.OrderedKeys() {
		r, ok := updatedVarResources.Get(key)
		if !ok {
			continue
		}

		// hash tactics
		// 1. changed var -> resources [ ... ], set changed var hash to all resources
		// 2. changed var -> dep. var -> resources [ ... ], set changed var hash to all resources
		// 3. changed var 1 \ -> dep. var -> go to 2
		//                    -> dev. var -> resources [ ... ] set var_1__var_2 hash to all resources
		//    changed var 2 / -> dev. var -> go to 2
		// 4  changer var 1 -> changed dep.var -> resources [ ... ], set changed var1 hash to all resources
		vars := resourceVarsMap[r.GetName()]
		version := s.generateVariablesHash(vars)
		s.collectDependenciesRecursively(r, version, toPropagate, inv.GetRequiredMap(), resourceVersionMap)
	}

	s.printVariablesInfo(resourceVarsMap)
	err = s.updateResources(toPropagate, resourceVersionMap)

	return err
}

func (s *SyncAction) updateResources(toPropagate *OrderedResourceMap, resourceVersionMap map[string]string) error {
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
		return errEmptyVersions
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

func (s *SyncAction) printResources(message string, resources *OrderedResourceMap) {
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

func (s *SyncAction) getLatestVersionBetweenVars(vars map[string]bool, allVars map[string]*Variable) (latestVar string, latestVersion string) {
	latestDate := time.Time{}

	for v := range vars {
		if variable, ok := allVars[v]; ok {
			if variable.version.Date.After(latestDate) {
				latestDate = variable.version.Date
				latestVar = v
				latestVersion = variable.version.Version
			}
		}
	}

	return latestVar, latestVersion
}

func (s *SyncAction) generateVariablesHash(vars map[string]bool) string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}

	key := strings.Join(keys, "__")
	hash := HashString(key)
	base16Hash := strconv.FormatUint(hash, 16)
	return "." + base16Hash[:13]
}

func (s *SyncAction) collectResourceDependencies(resource *Resource, toPropagate *OrderedResourceMap, resourcesGraph map[string]map[string]bool, resourceVersionMap map[string]string) error {
	version, err := resource.GetVersion()
	if err != nil {
		return err
	}
	if version == "" {
		log.Debug("- %s has an empty version", resource.GetName())
		return errEmptyVersions
	}

	if items, ok := resourcesGraph[resource.GetName()]; ok {
		for resourceName := range items {
			depResource := newResource(resourceName, s.sourceDir)
			if !depResource.isValidResource() {
				continue
			}
			s.collectDependenciesRecursively(depResource, version, toPropagate, resourcesGraph, resourceVersionMap)
		}
	}

	return nil
}

func (s *SyncAction) collectResourceDependenciesWithVersion(resource *Resource, version string, toPropagate *OrderedResourceMap, resourcesGraph map[string]map[string]bool, resourceVersionMap map[string]string) error {
	if items, ok := resourcesGraph[resource.GetName()]; ok {
		for resourceName := range items {
			depResource, resourceExists := toPropagate.Get(resourceName)
			if !resourceExists {
				depResource = newResource(resourceName, s.sourceDir)
				if !depResource.isValidResource() {
					continue
				}
			}

			s.collectDependenciesRecursively(depResource, version, toPropagate, resourcesGraph, resourceVersionMap)
		}
	}

	return nil
}

func (s *SyncAction) collectDependenciesRecursively(resource *Resource, version string, toPropagate *OrderedResourceMap, resourcesGraph map[string]map[string]bool, resourceVersionMap map[string]string) {
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
				depResource = newResource(resourceName, s.sourceDir)
				if !depResource.isValidResource() {
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

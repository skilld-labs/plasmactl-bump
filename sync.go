package plasmactlbump

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

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
	dryRun        bool
	keyring       keyring.Keyring
	saveKeyring   bool
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
			fmt.Println(c.Hash)
			stats, err := c.Stats()
			if err != nil {
				panic(err)
			}

			var files []string
			for _, stat := range stats {
				files = append(files, stat.Name)
			}

			ci := &CommitInfo{
				Hash:         c.Hash.String(),
				ChangedFiles: files,
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

func (s *SyncAction) propagate(modifiedFiles []string, vaultpass string, hash *plumbing.Hash) error {
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

func (s *SyncAction) generateVariablesHash(vars map[string]bool) string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}

	key := strings.Join(keys, "__")
	hash := hashString(key)
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

func (s *SyncAction) collectDependenciesRecursively(resource *Resource, version string, toPropagate *OrderedResourceMap, resourcesGraph map[string]map[string]bool, resourceVersionMap map[string]string) {
	_, add := toPropagate.Get(resource.GetName())
	if add {
		return
	}

	log.Info("- Adding %s", resource.GetName())
	toPropagate.Set(resource.GetName(), resource)
	resourceVersionMap[resource.GetName()] = version

	if items, ok := resourcesGraph[resource.GetName()]; ok {
		for resourceName := range items {
			depResource := newResource(resourceName, s.sourceDir)
			if !depResource.isValidResource() {
				continue
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

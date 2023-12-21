package bumpupdated

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/launchrctl/launchr"
	"gopkg.in/yaml.v3"
)

var (
	errEmptyPath     = errors.New("empty resource path")
	errNoUpdate      = errors.New("no resources to update")
	errSkipBadCommit = errors.New("skipping bump, as the latest commit is already by the bumper tool")
	errFailedMeta    = errors.New("failed to open plasma.yaml")
)

var kinds = map[string]string{
	"application": "applications",
	"service":     "services",
	"software":    "softwares",
	"executor":    "executors",
	"flow":        "flows",
	"skill":       "skills",
	"function":    "functions",
	"library":     "libraries",
	"entity":      "entities",
}

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

// Resource represents a ansible resource
type Resource struct {
	name string
}

func newResource(name string) *Resource {
	return &Resource{
		name: name,
	}
}

// GetName returns a resource name
func (r *Resource) GetName() string {
	return r.name
}

// GetVersion retrieves the version of the resource from the plasma.yaml
func (r *Resource) GetVersion() (string, error) {
	parts := strings.Split(r.GetName(), "__")
	metaFile := fmt.Sprintf("%s/%s/roles/%s/meta/plasma.yaml", parts[0], parts[1], parts[2])
	if _, err := os.Stat(metaFile); err == nil {
		data, err := os.ReadFile(filepath.Clean(metaFile))
		if err != nil {
			fmt.Printf("Failed to read meta file: %v\n", err)
			return "", errFailedMeta
		}

		var meta map[string]interface{}
		err = yaml.Unmarshal(data, &meta)
		if err != nil {
			fmt.Printf("Failed to unmarshal meta file: %v\n", err)
			return "", errFailedMeta
		}

		if plasma, ok := meta["plasma"].(map[string]interface{}); ok {
			version := plasma["version"]
			return version.(string), nil
		}

		fmt.Printf("Empty meta file, return empty string as version\n")
		return "", nil
	}

	fmt.Printf("Meta file (%s) is missing\n", metaFile)
	return "", errFailedMeta
}

// UpdateVersion updates the version of the resource in the plasma.yaml file
func (r *Resource) UpdateVersion(version string) bool {
	parts := strings.Split(r.GetName(), "__")
	metaFile := fmt.Sprintf("%s/%s/roles/%s/meta/plasma.yaml", parts[0], parts[1], parts[2])
	if _, err := os.Stat(metaFile); err == nil {
		data, err := os.ReadFile(filepath.Clean(metaFile))
		if err != nil {
			fmt.Printf("Failed to read meta file: %v\n", err)
			return false
		}

		var b bytes.Buffer
		var meta map[string]interface{}
		err = yaml.Unmarshal(data, &meta)
		if err != nil {
			fmt.Printf("Failed to unmarshal meta file: %v\n", err)
			return false
		}

		if plasma, ok := meta["plasma"].(map[string]interface{}); ok {
			plasma["version"] = version
		} else {
			meta["plasma"] = map[string]interface{}{"version": version}
		}

		yamlEncoder := yaml.NewEncoder(&b)
		yamlEncoder.SetIndent(2)
		err = yamlEncoder.Encode(&meta)
		if err != nil {
			fmt.Printf("Failed to marshal meta file: %v\n", err)
			return false
		}

		err = os.WriteFile(metaFile, b.Bytes(), 0600)
		if err != nil {
			fmt.Printf("Failed to write meta file: %v\n", err)
			return false
		}

		return true
	}

	fmt.Printf("Meta file (%s) is missing\n", metaFile)
	return false
}

// BumpAction is a launchr.Service providing bumper functionality.
type BumpAction interface {
	launchr.Service
	Bump() error
}

type bumpUpdatedService struct {
	cfg launchr.Config
}

func newBumpUpdatedService(cfg launchr.Config) BumpAction {
	return &bumpUpdatedService{
		cfg: cfg,
	}
}

// ServiceInfo implements launchr.Service interface.
func (k *bumpUpdatedService) ServiceInfo() launchr.ServiceInfo {
	return launchr.ServiceInfo{}
}

func printMemo() {
	fmt.Println("List of unversioned files:")
	for k := range unversionedFiles {
		fmt.Println(k)
	}
	fmt.Print("\n")
}

func (k *bumpUpdatedService) Bump() error {
	fmt.Println("Bump updated versions...")
	printMemo()

	git, err := getRepo()
	if err != nil {
		return err
	}

	if git.IsOwnCommit() {
		return errSkipBadCommit
	}

	files, err := git.getLatestModifiedFiles()
	if err != nil {
		return err
	}

	resources := k.collectResources(files)
	if len(resources) == 0 {
		return errNoUpdate
	}

	version, err := git.GetLastCommitShortHash()
	if err != nil {
		fmt.Println("Can't retrieve commit hash")
		return err
	}

	_, err = k.updateResources(resources, version)
	if err != nil {
		fmt.Println("There is an error during resources update")
		return err
	}

	return git.Commit()
}

func (k *bumpUpdatedService) collectResources(files []string) map[string]*Resource {
	resources := make(map[string]*Resource)
	for _, path := range files {
		if !isVersionableFile(path) {
			continue
		}

		platform, kind, role, err := processResourcePath(path)
		if err != nil {
			continue
		}

		if platform == "" || kind == "" || role == "" {
			continue
		}

		if isUpdatableKind(kind) {
			resource := newResource(platform + "__" + kind + "__" + role)
			fmt.Printf("Processing resource %s\n", resource.GetName())
			resources[resource.GetName()] = resource
		}

	}

	return resources
}

func (k *bumpUpdatedService) updateResources(resources map[string]*Resource, version string) (bool, error) {
	updated := false
	for _, r := range resources {
		currentVersion, err := r.GetVersion()
		if err != nil {
			return false, err
		}
		fmt.Printf("Updating %s version from %s to %s\n", r.GetName(), currentVersion, version)
		updated = r.UpdateVersion(version) || updated
	}

	return updated, nil
}

func isUpdatableKind(kind string) bool {
	if _, ok := kinds[kind]; ok {
		return true
	}

	for _, value := range kinds {
		if value == kind {
			return true
		}
	}

	return false
}

func isVersionableFile(path string) bool {
	name := filepath.Base(path)
	_, ok := unversionedFiles[name]
	return !ok
}

func processResourcePath(path string) (string, string, string, error) {
	parts := strings.Split(path, "/")
	if len(parts) > 3 {
		return parts[0], parts[1], parts[3], nil
	}

	return "", "", "", errEmptyPath
}

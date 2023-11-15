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
	errEmptyPath = errors.New("empty resource path")
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

var unversionableFiles = map[string]bool{
	"README.md": true,
}

// CheckIfError should be used to naively panics if an error is not nil.
func CheckIfError(err error) {
	if err == nil {
		return
	}

	fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("error: %s\n", err))
	os.Exit(1)
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

func (k *bumpUpdatedService) Bump() error {
	fmt.Println("Bump updated versions...")
	git := getRepo()
	if git.IsOwnCommit() {
		fmt.Println("Skipping bump, as the latest commit is already by the bumper tool.")
		return nil
	}

	files, err := git.getLatestModifiedFiles()
	if err != nil {
		return err
	}

	resources := k.collectResources(files)
	if len(resources) == 0 {
		fmt.Println("There are no resources to update.")
		return nil
	}

	k.updateResources(resources, git.GetLastCommitShortHash())
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

func (k *bumpUpdatedService) updateResources(resources map[string]*Resource, version string) bool {
	updated := false
	for _, r := range resources {
		fmt.Printf("Updating %s version\n", r.GetName())
		updated = r.UpdateVersion(version) || updated
	}

	return updated
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
	_, ok := unversionableFiles[name]
	return !ok
}

func processResourcePath(path string) (string, string, string, error) {
	parts := strings.Split(path, "/")
	if len(parts) > 3 {
		return parts[0], parts[1], parts[3], nil
	}

	return "", "", "", errEmptyPath
}

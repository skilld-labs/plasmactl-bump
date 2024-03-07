package plasmactlbump

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/launchrctl/launchr/pkg/log"

	"gopkg.in/yaml.v3"
)

const (
	resourceNamePattern = "%s__%s__%s"
)

func prepareResourceName(platform, kind, role string) string {
	return fmt.Sprintf(resourceNamePattern, platform, kind, role)
}

// Resource represents a platform resource
type Resource struct {
	name       string
	pathPrefix string
}

func newResource(name, prefix string) *Resource {
	return &Resource{
		name:       name,
		pathPrefix: prefix,
	}
}

// GetName returns a resource name
func (r *Resource) GetName() string {
	return r.name
}

func (r *Resource) isValidResource() bool {
	metaPath := r.buildMetaPath()
	_, err := os.Stat(metaPath)

	return !os.IsNotExist(err)
}

func (r *Resource) buildMetaPath() string {
	parts := strings.Split(r.GetName(), "__")

	meta := fmt.Sprintf("%s/%s/roles/%s/meta/plasma.yaml", parts[0], parts[1], parts[2])
	return filepath.Join(r.pathPrefix, meta)
}

// GetVersion retrieves the version of the resource from the plasma.yaml
func (r *Resource) GetVersion() (string, error) {
	metaFile := r.buildMetaPath()
	if _, err := os.Stat(metaFile); err == nil {
		data, errRead := os.ReadFile(filepath.Clean(metaFile))
		if errRead != nil {
			log.Debug("Failed to read meta file: %v", err)
			return "", errFailedMeta
		}

		var meta map[string]interface{}
		errUnmarshal := yaml.Unmarshal(data, &meta)
		if errUnmarshal != nil {
			log.Debug("Failed to unmarshal meta file: %v", err)
			return "", errFailedMeta
		}

		if plasma, ok := meta["plasma"].(map[string]interface{}); ok {
			version := plasma["version"]
			if version == nil {
				version = ""
			}
			val, okConversion := version.(string)
			if okConversion {
				return val, nil
			}

			return fmt.Sprint(version), nil
		}

		log.Debug("Empty meta file, return empty string as version")
		return "", nil
	}

	log.Debug("Meta file (%s) is missing", metaFile)
	return "", errFailedMeta
}

// UpdateVersion updates the version of the resource in the plasma.yaml file
func (r *Resource) UpdateVersion(version string) error {
	metaFile := r.buildMetaPath()
	if _, err := os.Stat(metaFile); err == nil {
		data, errRead := os.ReadFile(filepath.Clean(metaFile))
		if errRead != nil {
			log.Debug("Failed to read meta file: %v", errRead)
			return errRead
		}

		var b bytes.Buffer
		var meta map[string]interface{}
		errUnmarshal := yaml.Unmarshal(data, &meta)
		if errUnmarshal != nil {
			log.Debug("Failed to unmarshal meta file: %v", errUnmarshal)
			return errUnmarshal
		}

		if plasma, ok := meta["plasma"].(map[string]interface{}); ok {
			plasma["version"] = version
		} else {
			meta["plasma"] = map[string]interface{}{"version": version}
		}

		yamlEncoder := yaml.NewEncoder(&b)
		yamlEncoder.SetIndent(2)
		errEncode := yamlEncoder.Encode(&meta)
		if errEncode != nil {
			log.Debug("Failed to marshal meta file: %v", errEncode)
			return errEncode
		}

		errWrite := os.WriteFile(metaFile, b.Bytes(), 0600)
		if errWrite != nil {
			log.Debug("Failed to write meta file: %v", err)
			return errWrite
		}

		return nil
	}

	log.Debug("Meta file (%s) is missing", metaFile)
	return errFailedMeta
}

func buildResourceFromPath(path, pathPrefix string) *Resource {
	if !isVersionableFile(path) {
		return nil
	}

	platform, kind, role, err := processResourcePath(path)
	if err != nil || (platform == "" || kind == "" || role == "") || !isUpdatableKind(kind) {
		return nil
	}

	resource := newResource(prepareResourceName(platform, kind, role), pathPrefix)
	if !resource.isValidResource() {
		return nil
	}
	return resource
}

func processResourcePath(path string) (string, string, string, error) {
	parts := strings.Split(path, "/")
	if len(parts) > 3 {
		return parts[0], parts[1], parts[3], nil
	}

	return "", "", "", errEmptyPath
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

// OrderedResourceMap represents a map of resources with ordered keys
//
//	type OrderedResourceMap struct {
//	  keys []string
//	  dict map[string]*Resource
//	}
//
// The `keys` field is a slice of strings that represents the order of the keys in the map.
// The `dict` field is a map where the key is a string and the value is a pointer to a Resource object.
type OrderedResourceMap struct {
	keys []string
	dict map[string]*Resource
}

// NewOrderedResourceMap returns a new instance of OrderedResourceMap with an empty dictionary.
func NewOrderedResourceMap() *OrderedResourceMap {
	return &OrderedResourceMap{
		dict: make(map[string]*Resource),
	}
}

// Set a value in the OrderedResourceMap.
func (orm *OrderedResourceMap) Set(key string, value *Resource) {
	if _, ok := orm.dict[key]; !ok {
		orm.keys = append(orm.keys, key)
	}

	orm.dict[key] = value
}

// Get a value from the OrderedResourceMap.
func (orm *OrderedResourceMap) Get(key string) (*Resource, bool) {
	val, ok := orm.dict[key]
	return val, ok
}

// OrderedKeys returns the ordered keys from the OrderedResourceMap.
func (orm *OrderedResourceMap) OrderedKeys() []string {
	return orm.keys
}

// OrderBy updates the order of keys in the OrderedResourceMap based on the orderList.
// It removes any keys in orderList that are not present in the OrderedResourceMap.
// The keys are reordered in the OrderedResourceMap according to the order of appearance in orderList.
// The order of keys not present in orderList remains unchanged.
func (orm *OrderedResourceMap) OrderBy(orderList []string) {
	var newKeys []string

	for _, item := range orderList {
		_, ok := orm.Get(item)
		if ok {
			newKeys = append(newKeys, item)
		}
	}

	orm.keys = newKeys
}

// Len returns the length of the OrderedResourceMap.
func (orm *OrderedResourceMap) Len() int {
	return len(orm.keys)
}

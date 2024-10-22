package sync

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
	errResourceMeta = errors.New("failed to open plasma.yaml")
)

// PrepareMachineResourceName concatenates resource platform, kind and role via specific template.
// It allows to have common resource names.
func PrepareMachineResourceName(platform, kind, role string) string {
	return fmt.Sprintf("%s__%s__%s", platform, kind, role)
}

// ConvertMRNtoPath transforms machine resource name to templated path to resource.
func ConvertMRNtoPath(mrn string) (string, error) {
	parts := strings.Split(mrn, "__")
	if len(parts) != 3 {
		return "", errors.New("invalid MRN format")
	}
	return fmt.Sprintf("%s/%s/roles/%s", parts[0], parts[1], parts[2]), nil
}

// Resource represents a platform resource
type Resource struct {
	name       string
	pathPrefix string
}

// NewResource returns new [Resource] instance.
func NewResource(name, prefix string) *Resource {
	return &Resource{
		name:       name,
		pathPrefix: prefix,
	}
}

// GetName returns a resource name
func (r *Resource) GetName() string {
	return r.name
}

// IsValidResource checks if resource has meta file.
func (r *Resource) IsValidResource() bool {
	metaPath := r.getRealMetaPath()
	_, err := os.Stat(metaPath)

	return !os.IsNotExist(err)
}

func (r *Resource) getRealMetaPath() string {
	meta := r.BuildMetaPath()
	return filepath.Join(r.pathPrefix, meta)
}

// BuildMetaPath returns common path to resource meta.
func (r *Resource) BuildMetaPath() string {
	parts := strings.Split(r.GetName(), "__")
	meta := fmt.Sprintf("%s/%s/roles/%s/meta/plasma.yaml", parts[0], parts[1], parts[2])
	return meta
}

// GetVersion retrieves the version of the resource from the plasma.yaml
func (r *Resource) GetVersion() (string, error) {
	metaFile := r.getRealMetaPath()
	if _, err := os.Stat(metaFile); err == nil {
		data, errRead := os.ReadFile(filepath.Clean(metaFile))
		if errRead != nil {
			launchr.Log().Debug(fmt.Sprintf("Failed to read meta file: %v", err))
			return "", errResourceMeta
		}

		var meta map[string]any
		errUnmarshal := yaml.Unmarshal(data, &meta)
		if errUnmarshal != nil {
			launchr.Log().Debug(fmt.Sprintf("Failed to unmarshal meta file: %v", err))
			return "", errResourceMeta
		}

		version := GetMetaVersion(meta)
		if version == "" {
			launchr.Log().Debug("Empty meta file, return empty string as version")
		}

		return version, nil
	}

	return "", errResourceMeta
}

// GetMetaVersion searches for version in meta data.
func GetMetaVersion(meta map[string]any) string {
	if plasma, ok := meta["plasma"].(map[string]any); ok {
		version := plasma["version"]
		if version == nil {
			version = ""
		}
		val, okConversion := version.(string)
		if okConversion {
			return val
		}

		return fmt.Sprint(version)
	}

	return ""
}

// GetBaseVersion returns resource version without `-` if any.
func (r *Resource) GetBaseVersion() (string, string, error) {
	version, err := r.GetVersion()
	if err != nil {
		return "", "", err
	}

	split := strings.Split(version, "-")
	if len(split) > 2 {
		launchr.Term().Warning().Printfln("Resource %s has incorrect version format %s", version, r.GetName())
	}

	return split[0], version, nil
}

// UpdateVersion updates the version of the resource in the plasma.yaml file
func (r *Resource) UpdateVersion(version string) error {
	metaFile := r.getRealMetaPath()
	if _, err := os.Stat(metaFile); err == nil {
		data, errRead := os.ReadFile(filepath.Clean(metaFile))
		if errRead != nil {
			launchr.Log().Debug(fmt.Sprintf("Failed to read meta file: %v", errRead))
			return errRead
		}

		var b bytes.Buffer
		var meta map[string]any
		errUnmarshal := yaml.Unmarshal(data, &meta)
		if errUnmarshal != nil {
			launchr.Log().Debug(fmt.Sprintf("Failed to unmarshal meta file: %v", errUnmarshal))
			return errUnmarshal
		}

		if plasma, ok := meta["plasma"].(map[string]any); ok {
			plasma["version"] = version
		} else {
			meta["plasma"] = map[string]any{"version": version}
		}

		yamlEncoder := yaml.NewEncoder(&b)
		yamlEncoder.SetIndent(2)
		errEncode := yamlEncoder.Encode(&meta)
		if errEncode != nil {
			launchr.Log().Debug(fmt.Sprintf("Failed to marshal meta file: %v", errEncode))
			return errEncode
		}

		errWrite := os.WriteFile(metaFile, b.Bytes(), 0600)
		if errWrite != nil {
			launchr.Log().Debug(fmt.Sprintf("Failed to write meta file: %v", err))
			return errWrite
		}

		return nil
	}

	return errResourceMeta
}

// BuildResourceFromPath builds a new instance of Resource from the given path.
func BuildResourceFromPath(path, pathPrefix string) *Resource {
	platform, kind, role, err := ProcessResourcePath(path)
	if err != nil || (platform == "" || kind == "" || role == "") || !IsUpdatableKind(kind) {
		return nil
	}

	resource := NewResource(PrepareMachineResourceName(platform, kind, role), pathPrefix)
	if !resource.IsValidResource() {
		return nil
	}
	return resource
}

// ProcessResourcePath splits resource path onto platform, kind and role.
func ProcessResourcePath(path string) (string, string, string, error) {
	parts := strings.Split(path, "/")
	if len(parts) > 3 {
		return parts[0], parts[1], parts[3], nil
	}

	return "", "", "", errors.New("empty resource path")
}

// IsUpdatableKind checks if resource kind is in [Kinds] range.
func IsUpdatableKind(kind string) bool {
	if _, ok := Kinds[kind]; ok {
		return true
	}

	for _, value := range Kinds {
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

// Unset a value from the OrderedResourceMap.
func (orm *OrderedResourceMap) Unset(key string) {
	if _, ok := orm.dict[key]; ok {
		index := -1
		for i, item := range orm.keys {
			if item == key {
				index = i
			}
		}
		if index != -1 {
			orm.keys = append(orm.keys[:index], orm.keys[index+1:]...)
		}

	}

	delete(orm.dict, key)
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

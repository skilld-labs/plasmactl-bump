package sync

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/launchrctl/launchr"
	"gopkg.in/yaml.v3"
)

var (
	tplVersionGet = "failed to get resource version (%s)"
	tplVersionSet = "failed to update resource version (%s)"
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
	return filepath.Join(parts[0], parts[1], "roles", parts[2]), nil
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
	meta := filepath.Join(parts[0], parts[1], "roles", parts[2], "meta", "plasma.yaml")
	return meta
}

// GetVersion retrieves the version of the resource from the plasma.yaml
func (r *Resource) GetVersion() (string, error) {
	metaFile := r.getRealMetaPath()
	if _, err := os.Stat(metaFile); err == nil {
		data, errRead := os.ReadFile(filepath.Clean(metaFile))
		if errRead != nil {
			launchr.Log().Debug("error", "error", errRead)
			return "", fmt.Errorf(tplVersionGet, metaFile)
		}

		var meta map[string]any
		errUnmarshal := yaml.Unmarshal(data, &meta)
		if errUnmarshal != nil {
			launchr.Log().Debug("error", "error", errUnmarshal)
			return "", fmt.Errorf(tplVersionGet, metaFile)
		}

		version := GetMetaVersion(meta)
		if version == "" {
			launchr.Log().Warn(fmt.Sprintf("Empty meta file %s version, return empty string as version", metaFile))
		}

		return version, nil
	}

	return "", fmt.Errorf(tplVersionGet, metaFile)
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
	metaFilepath := r.getRealMetaPath()
	if _, err := os.Stat(metaFilepath); err == nil {
		data, errRead := os.ReadFile(filepath.Clean(metaFilepath))
		if errRead != nil {
			launchr.Log().Debug("error", "error", errRead)
			return fmt.Errorf(tplVersionSet, metaFilepath)
		}

		var b bytes.Buffer
		var meta map[string]any
		errUnmarshal := yaml.Unmarshal(data, &meta)
		if errUnmarshal != nil {
			launchr.Log().Debug("error", "error", errRead)
			return fmt.Errorf(tplVersionSet, metaFilepath)
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
			launchr.Log().Debug("error", "error", errEncode)
			return fmt.Errorf(tplVersionSet, metaFilepath)
		}

		errWrite := os.WriteFile(metaFilepath, b.Bytes(), 0600)
		if errWrite != nil {
			launchr.Log().Debug("error", "error", errWrite)
			return fmt.Errorf(tplVersionSet, metaFilepath)
		}

		return nil
	}

	return fmt.Errorf(tplVersionSet, metaFilepath)
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

// OrderedMap represents generic struct with map and order keys.
type OrderedMap[T any] struct {
	keys []string
	dict map[string]T
}

// NewOrderedMap returns a new instance of [OrderedMap].
func NewOrderedMap[T any]() *OrderedMap[T] {
	return &OrderedMap[T]{
		keys: make([]string, 0),
		dict: make(map[string]T),
	}
}

// Set a value in the [OrderedMap].
func (m *OrderedMap[T]) Set(key string, value T) {
	if _, ok := m.dict[key]; !ok {
		m.keys = append(m.keys, key)
	}

	m.dict[key] = value
}

// Unset a value from the [OrderedMap].
func (m *OrderedMap[T]) Unset(key string) {
	if _, ok := m.dict[key]; ok {
		index := -1
		for i, item := range m.keys {
			if item == key {
				index = i
			}
		}
		if index != -1 {
			m.keys = append(m.keys[:index], m.keys[index+1:]...)
		}

	}

	delete(m.dict, key)
}

// Get a value from the [OrderedMap].
func (m *OrderedMap[T]) Get(key string) (T, bool) {
	val, ok := m.dict[key]
	return val, ok
}

// Keys returns the ordered keys from the [OrderedMap].
func (m *OrderedMap[T]) Keys() []string {
	return m.keys
}

// OrderBy updates the order of keys in the [OrderedMap] based on the orderList.
func (m *OrderedMap[T]) OrderBy(orderList []string) {
	var newKeys []string
	var remainingKeys []string

keysLoop:
	for _, key := range m.keys {
		isInOrderList := false
		for _, orderKey := range orderList {
			if key == orderKey {
				isInOrderList = true
				continue keysLoop
			}
		}

		if !isInOrderList {
			remainingKeys = append(remainingKeys, key)
		}
	}

	for _, item := range orderList {
		_, ok := m.Get(item)
		if ok {
			newKeys = append(newKeys, item)
		}
	}

	newKeys = append(newKeys, remainingKeys...)
	m.keys = newKeys
}

// SortKeysAlphabetically sorts internal keys alphabetically.
func (m *OrderedMap[T]) SortKeysAlphabetically() {
	sort.Strings(m.keys)
}

// Len returns the length of the [OrderedMap].
func (m *OrderedMap[T]) Len() int {
	return len(m.keys)
}

// ToList converts map to ordered list [OrderedMap].
func (m *OrderedMap[T]) ToList() []T {
	var list []T
	for _, key := range m.keys {
		list = append(list, m.dict[key])
	}
	return list
}

// ToDict returns copy of [OrderedMap] dictionary.
func (m *OrderedMap[T]) ToDict() map[string]T {
	dict := make(map[string]T)
	for key, value := range m.dict {
		dict[key] = value
	}
	return dict
}

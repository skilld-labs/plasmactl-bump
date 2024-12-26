// Package sync contains tools to provide bump propagation.
package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevenle/topsort"
	"gopkg.in/yaml.v3"
)

const (
	invalidPasswordErrText = "invalid password"

	rootPlatform = "platform"
	vaultFile    = "vault.yaml"
)

// InventoryExcluded is list of excluded files and folders from inventory.
var InventoryExcluded = []string{
	".git",
	".compose",
	".plasmactl",
	".gitlab-ci.yml",
	"ansible_collections",
	"scripts/ci/.gitlab-ci.platform.yaml",
	"venv",
	"__pycache__",
}

// Kinds are list of target resources.
var Kinds = map[string]string{
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

type resourceDependencies struct {
	Name        string `yaml:"name"`
	IncludeRole struct {
		Name string `yaml:"name"`
	} `yaml:"include_role"`
}

// Inventory represents the inventory used in the application to search and collect resources and variable resources.
type Inventory struct {
	// services
	fc *FilesCrawler

	//internal
	resourcesMap *OrderedMap[*Resource]
	requiredBy   map[string]*OrderedMap[bool]
	dependsOn    map[string]*OrderedMap[bool]
	topOrder     []string

	resourcesUsageCalculated bool
	usedResources            map[string]bool

	variablesUsageCalculated       bool
	variableVariablesDependencyMap map[string]map[string]*VariableDependency
	variableResourcesDependencyMap map[string]map[string][]string

	// options
	sourceDir string
}

// NewInventory creates a new instance of Inventory with the provided vault password.
// It then calls the Init method of the Inventory to build the resources graph and returns
// the initialized Inventory or any error that occurred during initialization.
func NewInventory(sourceDir string) (*Inventory, error) {
	inv := &Inventory{
		sourceDir:                      sourceDir,
		fc:                             NewFilesCrawler(sourceDir),
		resourcesMap:                   NewOrderedMap[*Resource](),
		requiredBy:                     make(map[string]*OrderedMap[bool]),
		dependsOn:                      make(map[string]*OrderedMap[bool]),
		variableVariablesDependencyMap: make(map[string]map[string]*VariableDependency),
		variableResourcesDependencyMap: make(map[string]map[string][]string),
	}

	err := inv.Init()

	if err != nil {
		err = fmt.Errorf("inventory init error (%s) > %w", sourceDir, err)
	}

	return inv, err
}

// Init initializes the Inventory by building the resources graph.
// It returns an error if there was an issue while building the graph.
func (i *Inventory) Init() error {
	err := i.buildResourcesGraph()
	return err
}

func (i *Inventory) buildResourcesGraph() error {
	err := filepath.Walk(i.sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
			return nil
		}

		relPath := strings.TrimPrefix(path, i.sourceDir+"/")
		for _, d := range InventoryExcluded {
			if strings.Contains(relPath, d) {
				return nil
			}
		}

		entity := strings.ToLower(filepath.Base(relPath))

		switch entity {
		case "plasma.yaml":
			resource := BuildResourceFromPath(relPath, i.sourceDir)
			if resource == nil {
				break
			}
			if !resource.IsValidResource() {
				break
			}

			resourceName := resource.GetName()
			i.resourcesMap.Set(resourceName, resource)
		case "dependencies.yaml":
			resource := BuildResourceFromPath(relPath, i.sourceDir)
			if resource == nil {
				break
			}
			if !resource.IsValidResource() {
				break
			}

			resourceName := resource.GetName()
			if _, ok := i.resourcesMap.Get(resourceName); !ok {
				i.resourcesMap.Set(resourceName, resource)
			}

			data, errRead := os.ReadFile(filepath.Clean(path))
			if errRead != nil {
				return errRead
			}

			var deps []resourceDependencies
			err = yaml.Unmarshal(data, &deps)
			if err != nil {
				return err
			}

			if len(deps) == 0 {
				return nil
			}

			if i.dependsOn[resourceName] == nil {
				i.dependsOn[resourceName] = NewOrderedMap[bool]()
			}

			for _, dep := range deps {
				if dep.IncludeRole.Name != "" {
					depName := strings.ReplaceAll(dep.IncludeRole.Name, ".", "__")
					if i.requiredBy[depName] == nil {
						i.requiredBy[depName] = NewOrderedMap[bool]()
					}

					i.requiredBy[depName].Set(resourceName, true)
					i.dependsOn[resourceName].Set(depName, true)
				}
			}
		}
		return nil
	})

	if err != nil {
		return err
	}

	platformItems := NewOrderedMap[bool]()
	for resourceName := range i.requiredBy {
		if _, ok := i.dependsOn[resourceName]; !ok {
			platformItems.Set(resourceName, true)
		}
	}

	i.requiredBy[rootPlatform] = platformItems
	graph := topsort.NewGraph()
	for platform, resources := range i.requiredBy {
		for _, resource := range resources.Keys() {
			graph.AddNode(resource)
			edgeErr := graph.AddEdge(platform, resource)
			if edgeErr != nil {
				return edgeErr
			}
		}
	}

	order, err := graph.TopSort(rootPlatform)
	if err != nil {
		return err
	}

	// reverse order to have platform at top
	for y, j := 0, len(order)-1; y < j; y, j = y+1, j-1 {
		order[y], order[j] = order[j], order[y]
	}

	i.topOrder = order
	i.resourcesMap.OrderBy(order)

	return nil
}

// GetResourcesMap returns map of all resources found in source dir.
func (i *Inventory) GetResourcesMap() *OrderedMap[*Resource] {
	return i.resourcesMap
}

// GetResourcesOrder returns the order of resources in the inventory.
func (i *Inventory) GetResourcesOrder() []string {
	return i.topOrder
}

// GetRequiredByMap returns the required by map, which represents the `required by` dependencies between resources in the Inventory.
func (i *Inventory) GetRequiredByMap() map[string]*OrderedMap[bool] {
	return i.requiredBy
}

// GetDependsOnMap returns the map, which represents the 'depends on' dependencies between resources in the Inventory.
func (i *Inventory) GetDependsOnMap() map[string]*OrderedMap[bool] {
	return i.dependsOn
}

// GetRequiredByResources returns list of resources which depend on argument resource (directly or not).
func (i *Inventory) GetRequiredByResources(resourceName string, depth int8) map[string]bool {
	return i.lookupDependencies(resourceName, i.GetRequiredByMap(), depth)
}

// GetDependsOnResources returns list of resources which are used by argument resource (directly or not).
func (i *Inventory) GetDependsOnResources(resourceName string, depth int8) map[string]bool {
	return i.lookupDependencies(resourceName, i.GetDependsOnMap(), depth)
}

func (i *Inventory) lookupDependencies(resourceName string, resourcesMap map[string]*OrderedMap[bool], depth int8) map[string]bool {
	result := make(map[string]bool)
	if m, ok := resourcesMap[resourceName]; ok {
		for _, item := range m.Keys() {
			result[item] = true
			i.lookupDependenciesRecursively(item, resourcesMap, result, 1, depth)
		}
	}

	return result
}

func (i *Inventory) lookupDependenciesRecursively(resourceName string, resourcesMap map[string]*OrderedMap[bool], result map[string]bool, depth, limit int8) {
	if depth == limit {
		return
	}

	if m, ok := resourcesMap[resourceName]; ok {
		for _, item := range m.Keys() {
			result[item] = true
			i.lookupDependenciesRecursively(item, resourcesMap, result, depth+1, limit)
		}
	}
}

// GetChangedResources returns an OrderedResourceMap containing the resources that have been modified, based on the provided list of modified files.
// It iterates over the modified files, builds a resource from each file path, and adds it to the result map if it is not already present.
//func (i *Inventory) GetChangedResources(files []string) *OrderedMap[*Resource] {
//	resources := NewOrderedMap[*Resource]()
//	for _, path := range files {
//		resource := BuildResourceFromPath(path, i.sourceDir)
//		if resource == nil {
//			continue
//		}
//		if _, ok := resources.Get(resource.GetName()); ok {
//			continue
//		}
//		resources.Set(resource.GetName(), resource)
//	}
//
//	return resources
//}

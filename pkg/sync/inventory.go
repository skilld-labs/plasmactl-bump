package sync

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/launchrctl/launchr"
	vault "github.com/sosedoff/ansible-vault-go"
	"github.com/stevenle/topsort"
	"gopkg.in/yaml.v3"
)

const (
	invalidPasswordErrText = "invalid password"

	rootPlatform    = "platform"
	variablePattern = "(?:\\s|{|\\|)(%s)(?:\\s|}|\\|)"
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

// Variable represents a variable used in the application.
// It contains information about the variable filepath, platform, name, hash,
// and whether it is from a vault or not.
type Variable struct {
	filepath string
	platform string
	name     string
	hash     uint64
	isVault  bool
}

// GetPath returns path to variable file.
func (v *Variable) GetPath() string {
	return v.filepath
}

// GetPlatform returns variable platform.
func (v *Variable) GetPlatform() string {
	return v.platform
}

// GetName returns variable name.
func (v *Variable) GetName() string {
	return v.name
}

// GetHash returns variable [Variable.hash]
func (v *Variable) GetHash() uint64 {
	return v.hash
}

// IsVault tells if variable from vault.
func (v *Variable) IsVault() bool {
	return v.isVault
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
	ResourcesCrawler *ResourcesCrawler

	//internal
	resourcesMap *OrderedMap[bool]
	requiredBy   map[string]*OrderedMap[bool]
	dependsOn    map[string]*OrderedMap[bool]
	topOrder     []string

	// options
	sourceDir string
}

// NewInventory creates a new instance of Inventory with the provided vault password.
// It then calls the Init method of the Inventory to build the resources graph and returns
// the initialized Inventory or any error that occurred during initialization.
func NewInventory(sourceDir string) (*Inventory, error) {
	inv := &Inventory{
		sourceDir:        sourceDir,
		ResourcesCrawler: NewResourcesCrawler(sourceDir),
		resourcesMap:     NewOrderedMap[bool](),
		requiredBy:       make(map[string]*OrderedMap[bool]),
		dependsOn:        make(map[string]*OrderedMap[bool]),
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

// GetResourcesMap returns map of all resources found in source dir.
func (i *Inventory) GetResourcesMap() *OrderedMap[bool] {
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
			i.resourcesMap.Set(resourceName, true)
		case "dependencies.yaml":
			resource := BuildResourceFromPath(relPath, i.sourceDir)
			if resource == nil {
				break
			}
			if !resource.IsValidResource() {
				break
			}

			resourceName := resource.GetName()
			i.resourcesMap.Set(resourceName, true)

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

// GetChangedResources returns an OrderedResourceMap containing the resources that have been modified, based on the provided list of modified files.
// It iterates over the modified files, builds a resource from each file path, and adds it to the result map if it is not already present.
func (i *Inventory) GetChangedResources(modifiedFiles []string) *OrderedMap[*Resource] {
	resources := NewOrderedMap[*Resource]()
	for _, path := range modifiedFiles {
		resource := BuildResourceFromPath(path, i.sourceDir)
		if resource == nil {
			continue
		}
		if _, ok := resources.Get(resource.GetName()); ok {
			continue
		}
		resources.Set(resource.GetName(), resource)
	}

	return resources
}

// GetChangedVariables fetches variables file from list, compare them with comparison dir files and fetches changed vars.
func (i *Inventory) GetChangedVariables(modifiedFiles []string, comparisonDir, vaultpass string) (*OrderedMap[*Variable], *OrderedMap[*Variable], error) {
	changedVariables := NewOrderedMap[*Variable]()
	deletedVariables := NewOrderedMap[*Variable]()
	for _, path := range modifiedFiles {
		platform, kind, role, errPath := ProcessResourcePath(path)
		if errPath != nil {
			continue
		}

		if platform == "" || kind == "" || role == "" {
			continue
		}

		if kind != "group_vars" {
			continue
		}

		sourcePath := filepath.Join(i.sourceDir, path)
		artifactPath := filepath.Join(comparisonDir, path)
		isVault := isVaultFile(path)

		_, err1 := os.Stat(sourcePath)
		sourceFileExists := !os.IsNotExist(err1)
		_, err2 := os.Stat(artifactPath)
		artifactFileExists := !os.IsNotExist(err2)

		if sourceFileExists && artifactFileExists {
			sourceData, err := LoadVariablesFile(sourcePath, vaultpass, isVault)
			if err != nil {
				return changedVariables, deletedVariables, err
			}

			artifactData, err := LoadVariablesFile(artifactPath, vaultpass, isVault)
			if err != nil {
				return changedVariables, deletedVariables, err
			}

			// Check for updated vars in existing files
			for k, sv := range sourceData {
				if av, ok := artifactData[k]; ok {
					sourceValue := fmt.Sprint(sv)
					artifactValue := fmt.Sprint(av)

					sourceHash := HashString(sourceValue)
					artifactHash := HashString(artifactValue)

					if sourceHash != artifactHash {
						changedVar := &Variable{
							filepath: path,
							name:     k,
							hash:     sourceHash,
							platform: platform,
							isVault:  isVault,
						}
						changedVariables.Set(changedVar.GetName(), changedVar)
					}
				} else {
					// Handle new variable.
					sourceValue := fmt.Sprint(sv)
					newVar := &Variable{
						filepath: path,
						name:     k,
						hash:     HashString(sourceValue),
						platform: platform,
						isVault:  isVault,
					}

					changedVariables.Set(newVar.GetName(), newVar)
				}
			}

			// Check for deleted vars in existing files
			for k, av := range artifactData {
				if _, ok := sourceData[k]; !ok {
					artifactValue := fmt.Sprint(av)
					deletedVar := &Variable{
						filepath: path,
						name:     k,
						hash:     HashString(artifactValue),
						platform: platform,
						isVault:  isVault,
					}

					deletedVariables.Set(deletedVar.GetName(), deletedVar)
				}
			}

		} else if sourceFileExists && !artifactFileExists {
			// New vars file, add all variable to propagate.
			sourceData, err := LoadVariablesFile(sourcePath, vaultpass, isVault)
			if err != nil {
				return changedVariables, deletedVariables, err
			}
			for k, sv := range sourceData {
				sourceValue := fmt.Sprint(sv)
				newVar := &Variable{
					filepath: path,
					name:     k,
					hash:     HashString(sourceValue),
					platform: platform,
					isVault:  isVault,
				}

				changedVariables.Set(newVar.GetName(), newVar)
			}

		} else if !sourceFileExists && artifactFileExists {
			// Vars file was deleted, find all removed variables.
			artifactData, err := LoadVariablesFile(artifactPath, vaultpass, isVault)
			if err != nil {
				return changedVariables, deletedVariables, err
			}

			for k, av := range artifactData {
				artifactValue := fmt.Sprint(av)
				deletedVar := &Variable{
					filepath: path,
					name:     k,
					hash:     HashString(artifactValue),
					platform: platform,
					isVault:  isVault,
				}

				deletedVariables.Set(deletedVar.GetName(), deletedVar)
			}
		}
	}

	changedVariables.SortKeysAlphabetically()
	deletedVariables.SortKeysAlphabetically()
	return changedVariables, deletedVariables, nil
}

// SearchVariablesAffectedResources crawls inventory to find if variables were used in [Inventory.SourceDir] resources.
func (i *Inventory) SearchVariablesAffectedResources(variables []*Variable) (*OrderedMap[*Resource], map[string]map[string]bool, error) {
	resources := NewOrderedMap[*Resource]()
	resourceVariablesMap := make(map[string]map[string]bool)

	splitByPlatform := make(map[string]map[string]*Variable)
	dependentVariables := make(map[string]map[string]bool)

	for _, v := range variables {
		if _, ok := splitByPlatform[v.platform]; !ok {
			splitByPlatform[v.platform] = make(map[string]*Variable)
		}
		splitByPlatform[v.platform][v.name] = v
		descendents := make(map[string]*Variable)
		err := i.crawlVariableUsage(v, descendents)
		if err != nil {
			return resources, resourceVariablesMap, err
		}

		for _, d := range descendents {
			if _, ok := splitByPlatform[d.platform]; !ok {
				splitByPlatform[d.platform] = make(map[string]*Variable)
			}

			splitByPlatform[d.platform][d.name] = d

			if _, ok := dependentVariables[d.name]; !ok {
				dependentVariables[d.name] = make(map[string]bool)
			}

			dependentVariables[d.name][v.name] = true
		}
	}

	i.pushRequiredVariables(dependentVariables)

	assetsMap := make(map[string]map[string]bool)
	for platform, platformVars := range splitByPlatform {
		err := i.ResourcesCrawler.SearchVariableResources(platform, platformVars, assetsMap)
		if err != nil {
			return resources, resourceVariablesMap, err
		}
	}

	for path, vars := range assetsMap {
		resource := BuildResourceFromPath(path, i.sourceDir)
		if resource == nil {
			continue
		}
		if val, ok := resources.Get(resource.GetName()); !ok {
			launchr.Log().Debug("Processing resource", "resource", resource.GetName())
			resources.Set(resource.GetName(), resource)
		} else {
			resource = val
		}

		if _, ok := resourceVariablesMap[resource.GetName()]; !ok {
			resourceVariablesMap[resource.GetName()] = make(map[string]bool)
		}

		for variable := range vars {
			if _, ok := dependentVariables[variable]; !ok {
				resourceVariablesMap[resource.GetName()][variable] = true
			} else {
				for name := range dependentVariables[variable] {
					resourceVariablesMap[resource.GetName()][name] = true
				}
			}
		}
	}

	return resources, resourceVariablesMap, nil
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

func (i *Inventory) pushRequiredVariables(requiredMap map[string]map[string]bool) {
	for k, v := range requiredMap {
		for name := range v {
			if _, ok := requiredMap[name]; ok {
				delete(v, name)
				for nameParent := range requiredMap[name] {
					requiredMap[k][nameParent] = true
				}
				i.pushRequiredVariables(requiredMap)
			}
		}
	}
}

func (i *Inventory) crawlVariableUsage(variable *Variable, dependents map[string]*Variable) error {
	//@TODO crawl several variables
	var files []string
	var err error

	if variable.isVault || variable.platform != rootPlatform {
		files, err = i.ResourcesCrawler.FindGroupVarsFiles(variable.platform)
		if err != nil {
			return err
		}
	} else if variable.platform == rootPlatform {
		files, err = i.ResourcesCrawler.FindGroupVarsFiles("")
		if err != nil {
			return err
		}
	}

	//if i.variablesTree == nil {
	//	i.variablesTree = make(map[string]map[string]bool)
	//}
	if len(files) > 0 {
		dependencies, err := i.ResourcesCrawler.SearchVariablesInGroupFiles(variable.name, files)
		if err != nil {
			return err
		}

		for _, dep := range dependencies {
			if _, ok := dependents[dep.name]; ok {
				continue
			}
			dependents[dep.name] = dep

			//if _, ok := i.variablesTree[variable.name]; !ok {
			//	i.variablesTree[variable.name] = make(map[string]bool)
			//}
			//i.variablesTree[variable.name][dep.name] = true

			errUsage := i.crawlVariableUsage(dep, dependents)
			if errUsage != nil {
				return errUsage
			}
		}
	}

	return nil
}

// ResourcesCrawler is a type that represents a crawler for resources in a given directory.
type ResourcesCrawler struct {
	taskSources     map[string][]string
	templateSources map[string][]string
	defaultsSources map[string][]string
	rootDir         string
}

// NewResourcesCrawler creates a new instance of ResourcesCrawler with initialized taskSources and templateSources maps.
func NewResourcesCrawler(directory string) *ResourcesCrawler {
	return &ResourcesCrawler{
		taskSources:     make(map[string][]string),
		templateSources: make(map[string][]string),
		defaultsSources: make(map[string][]string),
		rootDir:         directory,
	}
}

func (cr *ResourcesCrawler) findFilesByPattern(filenamePattern, kind, platform string, partsCount, platformPart, rolePart, kindPart int) ([]string, error) {
	var files []string
	dir := filepath.Join(cr.rootDir, platform)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath := strings.TrimPrefix(path, cr.rootDir+"/")
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		if info.IsDir() || strings.Contains(path, "scripts") {
			return nil
		}

		match, err := filepath.Match(filenamePattern, filepath.Base(path))
		if err != nil {
			return err
		}
		if !match {
			return nil
		}

		parts := strings.Split(relPath, "/")
		if len(parts) >= partsCount && (platform == "" || parts[platformPart] == platform) &&
			(rolePart == 0 || parts[rolePart] == "roles") && parts[kindPart] == kind {
			files = append(files, relPath)
		}

		return nil
	})

	return files, err
}

// FindTaskFiles returns a slice of file paths that match certain criteria for tasks on a specific platform.
func (cr *ResourcesCrawler) FindTaskFiles(platform string) ([]string, error) {
	return cr.findFilesByPattern(
		"*.yaml",
		"tasks",
		platform,
		4,
		0,
		2,
		4,
	)
}

// FindDefaultFiles returns a slice of file paths that match certain criteria for defaults on a specific platform.
func (cr *ResourcesCrawler) FindDefaultFiles(platform string) ([]string, error) {
	return cr.findFilesByPattern(
		"*.yaml",
		"defaults",
		platform,
		4,
		0,
		2,
		4,
	)
}

// FindTemplateFiles returns a slice of file paths that match certain criteria for templates on a specific platform.
func (cr *ResourcesCrawler) FindTemplateFiles(platform string) ([]string, error) {
	return cr.findFilesByPattern(
		"*.j2",
		"templates",
		platform,
		4,
		0,
		2,
		4,
	)
}

// FindGroupVarsFiles returns a slice of file paths that match certain criteria for group vars on a specific platform.
func (cr *ResourcesCrawler) FindGroupVarsFiles(platform string) ([]string, error) {
	return cr.findFilesByPattern(
		"vars.yaml",
		"group_vars",
		platform,
		3,
		0,
		0,
		1,
	)
}

// SearchVariablesInGroupFiles searches for variables in a group of files that match the specified name.
// It takes a name string and a slice of files.
// It returns a slice of Variable pointers that contain information about each found variable.
// The function iterates over each file in the files slice, read it and if the string representation
// of the value contains the specified name, it creates a Variable object and appends it to the variables slice.
// Finally, it returns the variables slice.
// Example usage:
//
//	crawler := &ResourcesCrawler{}
//	files := crawler.FindGroupVarsFiles("platform")
//	variables := crawler.SearchVariablesInGroupFiles("variable", files)
func (cr *ResourcesCrawler) SearchVariablesInGroupFiles(name string, files []string) ([]*Variable, error) {
	var variables []*Variable
	regexName := regexp.QuoteMeta(name)
	searchRegexp, err := regexp.Compile(fmt.Sprintf(variablePattern, regexName))
	if err != nil {
		return variables, err
	}

	for _, path := range files {
		platform, _, _, errPath := ProcessResourcePath(path)
		if errPath != nil {
			return variables, errPath
		}

		sourcePath := filepath.Clean(filepath.Join(cr.rootDir, path))
		sourceVariables, errRead := os.ReadFile(sourcePath)
		if errRead != nil {
			return variables, errRead
		}

		var sourceData map[string]any
		errMarshal := yaml.Unmarshal(sourceVariables, &sourceData)
		if errMarshal != nil {
			if !strings.Contains(errMarshal.Error(), "already defined at line") {
				return variables, errMarshal
			}

			sourceData, errMarshal = UnmarshallFixDuplicates(sourceVariables)
			if err != nil {
				return variables, errMarshal
			}
		}

		for k, v := range sourceData {
			sourceValue := fmt.Sprint(v)
			if k == name {
				continue
			}

			if searchRegexp.MatchString(sourceValue) {
				variables = append(variables, &Variable{
					filepath: path,
					name:     k,
					hash:     HashString(sourceValue),
					isVault:  isVaultFile(path),
					platform: platform,
				})
			}
		}
	}

	return variables, nil
}

// SearchVariableResources searches for variable resources on a specific platform using the provided names and resources map.
// If the platform is rootPlatform, an empty string is used as the search platform.
// It retrieves task and template files for the platform if they are not already stored in the taskSources and templateSources maps of the ResourcesCrawler.
// It scans each file and checks if the variables in the names map are present in the resources map for that file. If a variable is missing, it adds it to the toIterate slice.
// It then scans each line of the file and checks if it contains any of the variables in the toIterate slice. If a variable is found, it adds it to the foundList map.
// After scanning all the files, it updates the resources map with the variables found in each file.
func (cr *ResourcesCrawler) SearchVariableResources(platform string, variables map[string]*Variable, resources map[string]map[string]bool) error {
	var err error
	searchPlatform := platform
	if searchPlatform == rootPlatform {
		searchPlatform = ""
	}

	if _, ok := cr.taskSources[platform]; !ok {
		cr.taskSources[platform], err = cr.FindTaskFiles(searchPlatform)
		if err != nil {
			return err
		}
	}

	if _, ok := cr.templateSources[platform]; !ok {
		cr.templateSources[platform], err = cr.FindTemplateFiles(searchPlatform)
		if err != nil {
			return err
		}
	}

	if _, ok := cr.defaultsSources[platform]; !ok {
		cr.defaultsSources[platform], err = cr.FindDefaultFiles(searchPlatform)
		if err != nil {
			return err
		}
	}

	files := append(cr.taskSources[platform], cr.templateSources[platform]...)
	files = append(files, cr.defaultsSources[platform]...)

	regExpressions := make(map[string]*regexp.Regexp)

	for name := range variables {
		regexName := regexp.QuoteMeta(name)
		searchRegexp, err := regexp.Compile(fmt.Sprintf(variablePattern, regexName))
		if err != nil {
			return err
		}
		regExpressions[name] = searchRegexp
	}

	for _, file := range files {
		sourcePath := filepath.Clean(filepath.Join(cr.rootDir, file))
		f, err := os.Open(sourcePath)
		if err != nil {
			launchr.Log().Debug("error opening file for variables usage", "file", file, "msg", err)
			return err
		}

		s := bufio.NewScanner(f)

		var toIterate []string
		for name := range variables {
			if _, ok := resources[file][name]; !ok {
				toIterate = append(toIterate, name)
			}
		}

		if len(toIterate) == 0 {
			continue
		}

		foundList := make(map[string]bool)
		for s.Scan() {
			for _, name := range toIterate {
				if _, ok := foundList[name]; ok {
					continue
				}

				regExpressions[name].MatchString(s.Text())
				if regExpressions[name].MatchString(s.Text()) {
					foundList[name] = true
				}

				if len(foundList) == len(toIterate) {
					break
				}
			}
		}
		errClose := f.Close()
		if errClose != nil {
			return errClose
		}

		if err = s.Err(); err != nil {
			launchr.Log().Debug("Error reading file: msg", "file", file, "msg", err)
			continue
		}

		for n := range foundList {
			if resources[file] == nil {
				resources[file] = make(map[string]bool)
			}
			resources[file][n] = true
		}
	}

	return nil
}

func isVaultFile(path string) bool {
	return filepath.Base(path) == "vault.yaml"
}

// LoadVariablesFile loads vars yaml file from path.
func LoadVariablesFile(path, vaultPassword string, isVault bool) (map[string]any, error) {
	var data map[string]any
	var rawData []byte
	var err error

	cleanPath := filepath.Clean(path)
	if isVault {
		sourceVault, errDecrypt := vault.DecryptFile(cleanPath, vaultPassword)
		if errDecrypt != nil {
			if errors.Is(errDecrypt, vault.ErrEmptyPassword) {
				return data, fmt.Errorf("error decrypting vault %s, password is blank", cleanPath)
			} else if errors.Is(errDecrypt, vault.ErrInvalidFormat) {
				return data, fmt.Errorf("error decrypting vault %s, invalid secret format", cleanPath)
			} else if errDecrypt.Error() == invalidPasswordErrText {
				return data, fmt.Errorf("invalid password for vault '%s'", cleanPath)
			}

			return data, errDecrypt
		}
		rawData = []byte(sourceVault)
	} else {
		rawData, err = os.ReadFile(cleanPath)
		if err != nil {
			return data, err
		}
	}

	err = yaml.Unmarshal(rawData, &data)
	if err != nil {
		if !strings.Contains(err.Error(), "already defined at line") {
			return data, err
		}

		launchr.Log().Warn("duplicate found, parsing YAML file manually")
		data, err = UnmarshallFixDuplicates(rawData)
		if err != nil {
			return data, err
		}
	}

	return data, err
}

// LoadVariablesFileFromBytes loads vars yaml file from bytes input.
func LoadVariablesFileFromBytes(input []byte, vaultPassword string, isVault bool) (map[string]any, error) {
	var data map[string]any
	var rawData []byte
	var err error

	if isVault {
		sourceVault, errDecrypt := vault.Decrypt(string(input), vaultPassword)
		if errDecrypt != nil {
			if errors.Is(errDecrypt, vault.ErrEmptyPassword) {
				return data, fmt.Errorf("error decrypting vaults, password is blank")
			} else if errors.Is(errDecrypt, vault.ErrInvalidFormat) {
				return data, fmt.Errorf("error decrypting vault, invalid secret format")
			} else if errDecrypt.Error() == invalidPasswordErrText {
				return data, fmt.Errorf("invalid password for vault")
			}

			return data, errDecrypt
		}
		rawData = []byte(sourceVault)
	} else {
		rawData = input
	}

	err = yaml.Unmarshal(rawData, &data)
	if err != nil {
		if !strings.Contains(err.Error(), "already defined at line") {
			return data, err
		}

		launchr.Log().Warn("duplicate found, parsing YAML file manually")
		data, err = UnmarshallFixDuplicates(rawData)
		if err != nil {
			return data, err
		}
	}

	return data, err
}

// LoadYamlFileFromBytes loads yaml file from bytes input.
func LoadYamlFileFromBytes(input []byte) (map[string]any, error) {
	var data map[string]any
	var rawData []byte
	var err error

	rawData = input
	err = yaml.Unmarshal(rawData, &data)
	return data, err
}

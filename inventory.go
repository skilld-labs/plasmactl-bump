package plasmactlbump

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/launchrctl/launchr/pkg/log"
	vault "github.com/sosedoff/ansible-vault-go"
	"github.com/stevenle/topsort"
	"gopkg.in/yaml.v3"
)

const (
	rootPlatform    = "platform"
	variablePattern = "(?:\\s|{|\\|)(%s)(?:\\s|}|\\|)"
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

type resourceDependencies struct {
	Name        string `yaml:"name"`
	IncludeRole struct {
		Name string `yaml:"name"`
	} `yaml:"include_role"`
}

// Inventory represents the inventory used in the application to search and collect resources and variable resources.
type Inventory struct {
	vaultPassword    string
	ResourcesCrawler *ResourcesCrawler
	requiredMap      map[string]map[string]bool
	dependencyMap    map[string]map[string]bool
	resourcesOrder   []string
	sourceDir        string
	comparisonDir    string
}

// NewInventory creates a new instance of Inventory with the provided vault password.
// It then calls the Init method of the Inventory to build the resources graph and returns
// the initialized Inventory or any error that occurred during initialization.
func NewInventory(vaultpass, sourceDir, comparisonDir string) (*Inventory, error) {
	inv := &Inventory{
		vaultPassword:    vaultpass,
		ResourcesCrawler: NewResourcesCrawler(sourceDir),
		requiredMap:      make(map[string]map[string]bool),
		dependencyMap:    make(map[string]map[string]bool),
		sourceDir:        sourceDir,
		comparisonDir:    comparisonDir,
	}

	err := inv.Init()

	return inv, err
}

// Init initializes the Inventory by building the resources graph.
// It returns an error if there was an issue while building the graph.
func (i *Inventory) Init() error {
	err := i.buildResourcesGraph()
	return err
}

// GetResourcesOrder returns the order of resources in the inventory.
// It returns a slice of strings representing the order of the resources.
func (i *Inventory) GetResourcesOrder() []string {
	return i.resourcesOrder
}

// GetRequiredMap returns the required map, which represents the dependencies between resources in the Inventory.
// The map is of type `map[string]map[string]bool`, where the keys are the resource names and the values are maps of dependent resource names.
func (i *Inventory) GetRequiredMap() map[string]map[string]bool {
	return i.requiredMap
}

// GetDependenciesMap returns the map, which represents the dependencies between resources in the Inventory.
// The map is of type `map[string]map[string]bool`, where the keys are the resource names and the values are maps of dependent resource names.
func (i *Inventory) GetDependenciesMap() map[string]map[string]bool {
	return i.dependencyMap
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
		for _, d := range excludeSubDirs {
			if strings.Contains(relPath, d) {
				return nil
			}
		}

		entity := strings.ToLower(filepath.Base(relPath))

		switch entity {
		case "dependencies.yaml":
			platform, kind, role, errPath := processResourcePath(relPath)
			if errPath != nil {
				break
			}

			if platform == "" || kind == "" || role == "" {
				break
			}

			resourceName := prepareResourceName(platform, kind, role)
			if !isUpdatableKind(kind) {
				break
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

			if i.dependencyMap[resourceName] == nil {
				i.dependencyMap[resourceName] = make(map[string]bool)
			}

			for _, dep := range deps {
				if dep.IncludeRole.Name != "" {
					depName := strings.ReplaceAll(dep.IncludeRole.Name, ".", "__")
					if i.requiredMap[depName] == nil {
						i.requiredMap[depName] = make(map[string]bool)
					}

					i.requiredMap[depName][resourceName] = true
					i.dependencyMap[resourceName][depName] = true
				}
			}
		}
		return nil
	})

	if err != nil {
		return err
	}

	platformItems := make(map[string]bool)
	for resourceName := range i.requiredMap {
		if _, ok := i.dependencyMap[resourceName]; !ok {
			platformItems[resourceName] = true
		}
	}

	i.requiredMap[rootPlatform] = platformItems
	graph := topsort.NewGraph()
	for platform, resources := range i.requiredMap {
		for resource := range resources {
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

	i.resourcesOrder = order

	return nil
}

// GetChangedResources returns an OrderedResourceMap containing the resources that have been modified, based on the provided list of modified files.
// It iterates over the modified files, builds a resource from each file path, and adds it to the result map if it is not already present.
func (i *Inventory) GetChangedResources(modifiedFiles []string) *OrderedResourceMap {
	resources := NewOrderedResourceMap()
	for _, path := range modifiedFiles {
		resource := BuildResourceFromPath(path, i.sourceDir)
		if resource == nil {
			continue
		}
		if _, ok := resources.Get(resource.GetName()); ok {
			continue
		}
		log.Debug("Processing resource %s", resource.GetName())
		resources.Set(resource.GetName(), resource)
	}

	return resources
}

// GetChangedVarsResources returns the resources and variable maps associated with the changed variables in the modified files.
// It takes a slice of modified file paths as input.
// It returns the resources as an OrderedResourceMap and the variable maps as a map[string]map[string]bool.
// If there are no changed variables, it returns nil for both the resources and variable maps.
func (i *Inventory) GetChangedVarsResources(modifiedFiles []string) (*OrderedResourceMap, map[string]map[string]*Variable, error) {
	variables, err := i.getChangedVariables(modifiedFiles)
	if len(variables) == 0 || err != nil {
		return NewOrderedResourceMap(), make(map[string]map[string]*Variable), err
	}

	resources, resourceVariablesMap, err := i.searchVariablesResources(variables)
	return resources, resourceVariablesMap, err
}

func (i *Inventory) getChangedVariables(modifiedFiles []string) (map[string]*Variable, error) {
	changedVariables := make(map[string]*Variable)
	for _, path := range modifiedFiles {
		platform, kind, role, errPath := processResourcePath(path)
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
		artifactPath := filepath.Join(i.comparisonDir, path)
		isVault := isVaultFile(path)

		sourceData, err := i.loadVariablesFile(sourcePath, i.vaultPassword, isVault)
		if err != nil {
			return changedVariables, err
		}

		artifactData, err := i.loadVariablesFile(artifactPath, i.vaultPassword, isVault)
		if err != nil {
			return changedVariables, err
		}

		for k, sv := range sourceData {
			if av, ok := artifactData[k]; ok {
				sourceValue := fmt.Sprint(sv)
				artifactValue := fmt.Sprint(av)

				sourceHash := hashString(sourceValue)
				artifactHash := hashString(artifactValue)

				if sourceHash != artifactHash {
					changedVar := &Variable{
						filepath: path,
						name:     k,
						hash:     sourceHash,
						platform: platform,
						isVault:  isVault,
					}

					changedVariables[changedVar.name] = changedVar
				}
			}
		}

	}

	return changedVariables, nil
}

func (i *Inventory) loadVariablesFile(path, vaultPassword string, isVault bool) (map[string]interface{}, error) {
	var data map[string]interface{}
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
			} else if errDecrypt.Error() == "invalid password" {
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
	return data, err
}

func (i *Inventory) searchVariablesResources(variables map[string]*Variable) (*OrderedResourceMap, map[string]map[string]*Variable, error) {
	resources := NewOrderedResourceMap()
	resourceVariablesMap := make(map[string]map[string]*Variable)

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
			log.Debug("Processing resource %s", resource.GetName())
			resources.Set(resource.GetName(), resource)
		} else {
			resource = val
		}

		if _, ok := resourceVariablesMap[resource.GetName()]; !ok {
			resourceVariablesMap[resource.GetName()] = make(map[string]*Variable)
		}

		for variable := range vars {
			if _, ok := dependentVariables[variable]; !ok {
				resourceVariablesMap[resource.GetName()][variable] = variables[variable]
			} else {
				for name := range dependentVariables[variable] {
					resourceVariablesMap[resource.GetName()][name] = variables[variable]
				}
			}
		}
	}

	return resources, resourceVariablesMap, nil
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
	var files []string

	if variable.isVault || variable.platform != rootPlatform {
		files = i.ResourcesCrawler.FindGroupVarsFiles(variable.platform)
	} else if variable.platform == rootPlatform {
		files = i.ResourcesCrawler.FindGroupVarsFiles("")
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

func (cr *ResourcesCrawler) findFilesByPattern(filenamePattern, kind, platform string, partsCount, platformPart, rolePart, kindPart int) []string {
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

	if err != nil {
		panic(err)
	}

	return files
}

// FindTaskFiles returns a slice of file paths that match certain criteria for tasks on a specific platform.
func (cr *ResourcesCrawler) FindTaskFiles(platform string) []string {
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
func (cr *ResourcesCrawler) FindDefaultFiles(platform string) []string {
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
func (cr *ResourcesCrawler) FindTemplateFiles(platform string) []string {
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
func (cr *ResourcesCrawler) FindGroupVarsFiles(platform string) []string {
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
		platform, _, _, errPath := processResourcePath(path)
		if errPath != nil {
			return variables, errPath
		}

		sourcePath := filepath.Clean(filepath.Join(cr.rootDir, path))
		sourceVariables, errRead := os.ReadFile(sourcePath)
		if errRead != nil {
			log.Debug("Error reading YAML file: %s\n", errRead)
			return variables, errRead
		}

		var sourceData map[string]interface{}
		errMarshal := yaml.Unmarshal(sourceVariables, &sourceData)
		if errMarshal != nil {
			log.Debug("Unable to unmarshal YAML file: %s", errMarshal)
			return variables, errRead
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
					hash:     hashString(sourceValue),
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
func (cr *ResourcesCrawler) SearchVariableResources(platform string, names map[string]*Variable, resources map[string]map[string]bool) error {
	searchPlatform := platform
	if searchPlatform == rootPlatform {
		searchPlatform = ""
	}

	if _, ok := cr.taskSources[platform]; !ok {
		cr.taskSources[platform] = cr.FindTaskFiles(searchPlatform)
	}

	if _, ok := cr.templateSources[platform]; !ok {
		cr.templateSources[platform] = cr.FindTemplateFiles(searchPlatform)
	}

	if _, ok := cr.defaultsSources[platform]; !ok {
		cr.defaultsSources[platform] = cr.FindDefaultFiles(searchPlatform)
	}

	files := append(cr.taskSources[platform], cr.templateSources[platform]...)
	files = append(files, cr.defaultsSources[platform]...)

	regExpressions := make(map[string]*regexp.Regexp)

	for name := range names {
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
			log.Debug("Error opening file %s: %v", file, err)
			return err
		}

		s := bufio.NewScanner(f)

		var toIterate []string
		for name := range names {
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
			log.Debug("Error reading file %s: %v", file, err)
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

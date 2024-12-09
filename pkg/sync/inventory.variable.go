package sync

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// IsVaultFile is helper to determine if file is vault file.
func IsVaultFile(path string) bool {
	return filepath.Base(path) == vaultFile
}

// Variable represents a variable used in the application.
type Variable struct {
	filepath string
	name     string
	hash     uint64
	isVault  bool
}

// NewVariable returns instance of [Variable] struct.
func NewVariable(filepath, name string, hash uint64, isVault bool) *Variable {
	return &Variable{filepath, name, hash, isVault}
}

// GetPath returns path to variable file.
func (v *Variable) GetPath() string {
	return v.filepath
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

// GetVariableResources returns list of resources which depends on variable.
func (i *Inventory) GetVariableResources(variableName string) ([]string, error) {
	var result []string

	if len(i.variableVariablesDependencyMap) == 0 || len(i.variableResourcesDependencyMap) == 0 {
		panic("use inventory.CalculateVariablesUsage first")
	}

	variablesList := make(map[string]bool)
	i.getVariableVariables(variableName, variablesList)

	for v := range variablesList {
		items, ok := i.variableResourcesDependencyMap[v]
		if !ok {
			continue
		}
		result = append(result, items...)
	}

	return result, nil
}

// GetVariableResources returns list of variables which depends on variable.
func (i *Inventory) getVariableVariables(variableName string, result map[string]bool) {
	result[variableName] = true
	if _, ok := i.variableVariablesDependencyMap[variableName]; ok {
		for v := range i.variableVariablesDependencyMap[variableName] {
			result[v] = true
			i.getVariableVariables(v, result)
		}
	}
}

// CalculateVariablesUsage precalculates all variables dependencies across platform.
func (i *Inventory) CalculateVariablesUsage(vaultpass string) error {
	// Find all variables files
	// Find all variables
	// Determine from variables values list of potential variables which may use other variables
	// Build variables usage map using previous list
	// - all variables from the same playbook can be used only on that playbook
	// - variables from platform playbook can be used in others
	keys, vars, err := i.buildVarsGroups(vaultpass)
	if err != nil {
		return err
	}

	variableVariablesDependencyMap := i.buildVariableDependencies(keys, vars)

	// Find all resources related template (templates/*.j2) and config (tasks/configuration.yaml) files. split by playbook
	// Iterate all files to find {{ and/or }}, get these lines
	// iterate potential lines with vars usage and check each variable in it.

	variableResourcesDependencyMap, err := i.buildVariableResourcesDependencies(keys, false)
	if err != nil {
		return err
	}

	i.variableVariablesDependencyMap = variableVariablesDependencyMap
	i.variableResourcesDependencyMap = variableResourcesDependencyMap

	return nil
}

func (i *Inventory) buildVarsGroups(vaultPass string) (map[string]map[string]bool, map[string]map[string]string, error) {
	groups, err := i.fc.FindVarsFiles("")
	if err != nil {
		return nil, nil, err
	}

	// Output maps
	groupKeys := make(map[string]map[string]bool)   // group -> keys
	groupVars := make(map[string]map[string]string) // group -> key -> string that contains {{ or }}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	var mx sync.Mutex

	maxWorkers := min(runtime.NumCPU(), len(groups))
	groupChan := make(chan string, len(groups))
	errorChan := make(chan error, 1)

	for w := 0; w < maxWorkers; w++ {
		go func(workerID int) {
			for {
				select {
				case <-ctx.Done():
					return
				case group, ok := <-groupChan:
					if !ok {
						return
					}
					if err = i.processGroup(ctx, vaultPass, group, groups[group], groupKeys, groupVars, &mx); err != nil {
						select {
						case errorChan <- fmt.Errorf("worker %d error processing %s: %w", workerID, group, err):
							cancel()
						default:
						}
						return
					}
					wg.Done()
				}
			}
		}(w)
	}

	for group := range groups {
		wg.Add(1)
		groupChan <- group
	}
	close(groupChan)

	go func() {
		wg.Wait()
		close(errorChan)
	}()

	for err = range errorChan {
		if err != nil {
			return nil, nil, err
		}
	}

	return groupKeys, groupVars, nil
}

func (i *Inventory) buildVariableDependencies(groupKeys map[string]map[string]bool, groupVars map[string]map[string]string) map[string]map[string]bool {
	// Map to store the result: key -> map of keys that use it
	reverseDependencyMap := make(map[string]map[string]bool)

	var mx sync.Mutex
	var wg sync.WaitGroup
	for group, vars := range groupVars {
		wg.Add(1)
		go func(group string, vars map[string]string) {
			defer wg.Done()
			processGroupDependencies(group, vars, groupKeys, reverseDependencyMap, &mx)
		}(group, vars)
	}

	wg.Wait()

	return reverseDependencyMap
}

// Helper function to process each group's dependencies
func processGroupDependencies(
	group string,
	vars map[string]string,
	groupKeys map[string]map[string]bool,
	reverseDependencyMap map[string]map[string]bool,
	mx *sync.Mutex,
) {
	currentGroupKeys := groupKeys[group]
	platformKeys := groupKeys["platform"]

	for varKey, varValue := range vars {
		deps := findDependencies(varValue, currentGroupKeys, platformKeys)

		mx.Lock()
		for _, dep := range deps {
			if reverseDependencyMap[dep] == nil {
				reverseDependencyMap[dep] = make(map[string]bool)
			}
			reverseDependencyMap[dep][varKey] = true
		}
		mx.Unlock()
	}
}

// Helper function to find dependencies in a string
func findDependencies(value string, currentGroupKeys, platformKeys map[string]bool) []string {
	var dependencies []string

	// Check current group keys
	for key := range currentGroupKeys {
		if strings.Contains(value, " "+key+" ") {
			dependencies = append(dependencies, key)
		}
	}

	// Check platform keys
	for key := range platformKeys {
		if strings.Contains(value, " "+key+" ") {
			dependencies = append(dependencies, key)
		}
	}

	return dependencies
}

// Helper function to convert map[string]bool to []string
//func getMapKeys(m map[string]bool) []string {
//	keys := make([]string, 0, len(m))
//	for key := range m {
//		keys = append(keys, key)
//	}
//	return keys
//}

func (i *Inventory) processGroup(ctx context.Context, vaultPass, group string, files []string, groupKeys map[string]map[string]bool, groupVars map[string]map[string]string, mx *sync.Mutex) error {
	mx.Lock()
	if _, exists := groupKeys[group]; !exists {
		groupKeys[group] = make(map[string]bool)
	}
	if _, exists := groupVars[group]; !exists {
		groupVars[group] = make(map[string]string)
	}
	mx.Unlock()

	const fileWorkers = 2
	var wg sync.WaitGroup
	fileChan := make(chan string, len(files))
	errChan := make(chan error, 1)

	for w := 0; w < fileWorkers; w++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case file, ok := <-fileChan:
					if !ok {
						return
					}
					if err := i.processFile(file, group, vaultPass, groupKeys, groupVars, mx); err != nil {
						select {
						case errChan <- err:
						default:
						}
					}
					wg.Done()
				}
			}
		}()
	}

	for _, file := range files {
		wg.Add(1)
		fileChan <- file
	}
	close(fileChan)

	go func() {
		wg.Wait()
		close(errChan)
	}()

	for err := range errChan {
		if err != nil {
			return err
		}
	}

	return nil
}

func (i *Inventory) processFile(file, group, vaultPass string, groupKeys map[string]map[string]bool, groupVars map[string]map[string]string, mx *sync.Mutex) error {
	data, err := LoadVariablesFile(filepath.Join(i.sourceDir, file), vaultPass, IsVaultFile(file))
	if err != nil {
		return err
	}

	i.extractKeysAndVars(data, group, groupKeys, groupVars, "", 0, mx)
	return nil
}

func (i *Inventory) extractKeysAndVars(data interface{}, group string, groupKeys map[string]map[string]bool, groupVars map[string]map[string]string, currentKey string, level int, mx *sync.Mutex) {
	switch v := data.(type) {
	case map[string]interface{}:
		for key, value := range v {
			if level == 0 {
				fullKey := key
				if currentKey != "" {
					fullKey = currentKey
					//fullKey = currentKey + "." + key // Nested keys
				}

				mx.Lock()
				if _, exists := groupKeys[group]; !exists {
					groupKeys[group] = make(map[string]bool)
				}
				groupKeys[group][key] = true
				mx.Unlock()

				// Recurse for nested structures
				i.extractKeysAndVars(value, group, groupKeys, groupVars, fullKey, level+1, mx)
			} else {
				mapStr := fmt.Sprintf("%v", v)
				mx.Lock()
				if strings.Contains(mapStr, "{{") || strings.Contains(mapStr, "}}") {
					if _, exists := groupVars[group]; !exists {
						groupVars[group] = make(map[string]string)
					}
					groupVars[group][currentKey] = mapStr
				}
				mx.Unlock()
			}

		}
	case []interface{}:
		listStr := fmt.Sprintf("%v", v)
		mx.Lock()
		if strings.Contains(listStr, "{{") || strings.Contains(listStr, "}}") {
			if _, exists := groupVars[group]; !exists {
				groupVars[group] = make(map[string]string)
			}
			groupVars[group][currentKey] = listStr
		}
		mx.Unlock()

		// Recurse into list items
		//for _, item := range v {
		//	if test {
		//		panic(currentKey)
		//	}
		//	i.extractKeysAndVars(item, group, groupKeys, groupVars, currentKey, mx)
		//}
	case string:
		if strings.Contains(v, "{{") || strings.Contains(v, "}}") {
			mx.Lock()
			if _, exists := groupVars[group]; !exists {
				groupVars[group] = make(map[string]string)
			}
			groupVars[group][currentKey] = v
			mx.Unlock()
		}
	}
}

func (i *Inventory) buildVariableResourcesDependencies(groupKeys map[string]map[string]bool, filesOnly bool) (map[string][]string, error) {
	groupFiles, err := i.fc.FindResourcesFiles("")
	if err != nil {
		return nil, err
	}

	// Map to store the result: key -> list of files
	reverseDependencyMap := make(map[string][]string)

	errChan := make(chan error, 1)
	var wg sync.WaitGroup
	var mx sync.Mutex
	for group, files := range groupFiles {
		wg.Add(1)
		go func(group string, files []string) {
			defer wg.Done()
			if err = i.processGroupFiles(group, files, groupKeys, reverseDependencyMap, &mx); err != nil {
				errChan <- fmt.Errorf("group %s: %w", group, err)
			}
		}(group, files)
	}

	go func() {
		wg.Wait()
		close(errChan)
	}()

	for err = range errChan {
		if err != nil {
			return nil, err
		}
	}

	if filesOnly {
		return reverseDependencyMap, nil
	}

	varToResourcesDependencyMap := make(map[string][]string)
	for v, files := range reverseDependencyMap {
		var res []string
		for _, path := range files {
			platform, kind, role, err := ProcessResourcePath(path)
			if err != nil || (platform == "" || kind == "" || role == "") || !IsUpdatableKind(kind) {
				continue
			}

			resourceName := PrepareMachineResourceName(platform, kind, role)
			res = append(res, resourceName)
		}
		varToResourcesDependencyMap[v] = res
	}

	return varToResourcesDependencyMap, nil
}

func (i *Inventory) processGroupFiles(
	group string,
	files []string,
	groupKeys map[string]map[string]bool,
	reverseDependencyMap map[string][]string,
	mx *sync.Mutex,
) error {
	// Get keys for the current group and the platform group
	currentGroupKeys := groupKeys[group]
	platformKeys := groupKeys["platform"]

	// Combined keys to check
	keysToCheck := combineKeys(currentGroupKeys, platformKeys)

	// Extract relevant lines from all files for the current group
	linesWithVariablesByFile := make(map[string][]string)
	for _, filePath := range files {
		lines, err := extractLinesWithVariables(filepath.Join(i.sourceDir, filePath))
		if err != nil {
			return fmt.Errorf("failed to process file %s: %w", filePath, err)
		}
		linesWithVariablesByFile[filePath] = lines
	}

	// Iterate over each key to find its usage in the relevant lines
	for key := range keysToCheck {
		for filePath, lines := range linesWithVariablesByFile {
			// Check if the file path prefix has been processed for the key
			filePrefix := getPathPrefix(filePath, 4)
			mx.Lock()
			isProcessed := isProcessedFile(key, filePrefix, reverseDependencyMap)
			mx.Unlock()
			if isProcessed {
				continue
			}

			// Check if any of the lines contain the key
			for _, line := range lines {
				if strings.Contains(line, key) {
					mx.Lock()
					reverseDependencyMap[key] = append(reverseDependencyMap[key], filePath)
					mx.Unlock()
					break
				}
			}
		}
	}
	return nil
}

func combineKeys(current, platform map[string]bool) map[string]bool {
	combined := make(map[string]bool)
	for k := range current {
		combined[k] = true
	}
	for k := range platform {
		combined[k] = true
	}
	return combined
}

func isProcessedFile(key, filePrefix string, reverseDependencyMap map[string][]string) bool {
	if filePaths, exists := reverseDependencyMap[key]; exists {
		for _, path := range filePaths {
			if getPathPrefix(path, 4) == filePrefix {
				return true
			}
		}
	}
	return false
}

func getPathPrefix(filePath string, parts int) string {
	pathParts := strings.Split(filePath, string(filepath.Separator))
	if len(pathParts) > parts {
		pathParts = pathParts[:parts]
	}
	return strings.Join(pathParts, string(filepath.Separator))
}

func extractLinesWithVariables(filePath string) ([]string, error) {
	file, err := os.Open(filepath.Clean(filePath))
	if err != nil {
		return nil, fmt.Errorf("error opening file %s: %w", filePath, err)
	}

	defer file.Close()

	var linesWithVariables []string
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "{{") || strings.Contains(line, "}}") {
			linesWithVariables = append(linesWithVariables, line)
		}
	}
	if err = scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading file %s: %w", filePath, err)
	}

	return linesWithVariables, nil
}

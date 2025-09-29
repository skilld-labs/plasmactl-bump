package plasmactlbump

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	async "sync"
	"time"

	"github.com/launchrctl/compose/compose"
	"github.com/launchrctl/keyring"
	"github.com/launchrctl/launchr"
	"github.com/launchrctl/launchr/pkg/action"
	"github.com/pterm/pterm"

	"github.com/skilld-labs/plasmactl-bump/v2/pkg/sync"
)

var (
	errMalformedKeyring = errors.New("the keyring is malformed or wrong passphrase provided")
)

const (
	vaultpassKey    = "vaultpass"
	domainNamespace = "domain"
	buildHackAuthor = "override"
)

// syncAction is a type representing a resources version synchronization action.
type syncAction struct {
	action.WithLogger
	action.WithTerm

	// services.
	keyring keyring.Keyring
	streams launchr.Streams

	// target dirs.
	buildDir    string
	packagesDir string
	domainDir   string

	// internal.
	saveKeyring bool
	timeline    []sync.TimelineItem

	// options.
	dryRun                bool
	allowOverride         bool
	filterByResourceUsage bool
	timeDepth             string
	vaultPass             string
	showProgress          bool
}

type hashStruct struct {
	hash     string
	hashTime time.Time
	author   string
}

// Execute the sync action to propagate resources' versions.
func (s *syncAction) Execute() error {
	s.Term().Info().Println("Processing propagation...")

	err := s.ensureVaultpassExists()
	if err != nil {
		return err
	}

	err = s.propagate()
	if err != nil {
		return err
	}
	s.Term().Info().Println("Propagation has been finished")

	if s.saveKeyring {
		err = s.keyring.Save()
	}

	return err
}

func (s *syncAction) ensureVaultpassExists() error {
	keyValueItem, errGet := s.keyring.GetForKey(vaultpassKey)
	if errGet != nil {
		if errors.Is(errGet, keyring.ErrEmptyPass) {
			return errGet
		} else if !errors.Is(errGet, keyring.ErrNotFound) {
			s.Log().Debug("keyring error", "error", errGet)
			return errMalformedKeyring
		}

		keyValueItem.Key = vaultpassKey
		keyValueItem.Value = s.vaultPass

		if keyValueItem.Value == "" {
			s.Term().Printf("- Ansible vault password\n")
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

	s.vaultPass = keyValueItem.Value.(string)

	return nil
}

func (s *syncAction) propagate() error {
	s.timeline = sync.CreateTimeline()

	s.Log().Info("Initializing build inventory")
	inv, err := sync.NewInventory(s.buildDir, s.Log())
	if err != nil {
		return err
	}

	if s.filterByResourceUsage {
		s.Log().Info("Calculating resources usage")
		err = inv.CalculateResourcesUsage()
		if err != nil {
			return fmt.Errorf("calculate resources usage > %w", err)
		}
	}

	s.Log().Info("Calculating variables usage")
	err = inv.CalculateVariablesUsage(s.vaultPass)
	if err != nil {
		return fmt.Errorf("calculate variables usage > %w", err)
	}

	err = s.buildTimeline(inv)
	if err != nil {
		return fmt.Errorf("building timeline > %w", err)
	}

	if len(s.timeline) == 0 {
		s.Term().Warning().Println("No resources were found for propagation")
		return nil
	}

	toPropagate, resourceVersionMap, err := s.buildPropagationMap(inv, s.timeline)
	if err != nil {
		return fmt.Errorf("building propagation map > %w", err)
	}

	err = s.updateResources(resourceVersionMap, toPropagate)
	if err != nil {
		return fmt.Errorf("propagate > %w", err)
	}

	return nil
}

func (s *syncAction) buildTimeline(buildInv *sync.Inventory) error {
	s.Log().Info("Gathering domain and packages resources")
	resourcesMap, packagePathMap, err := s.getResourcesMaps(buildInv)
	if err != nil {
		return fmt.Errorf("build resource map > %w", err)
	}

	s.Log().Info("Populate timeline with resources")
	err = s.populateTimelineResources(resourcesMap, packagePathMap)
	if err != nil {
		return fmt.Errorf("iteraring resources > %w", err)
	}

	s.Log().Info("Populate timeline with variables")
	err = s.populateTimelineVars(buildInv)
	if err != nil {
		return fmt.Errorf("iteraring variables > %w", err)
	}

	return nil
}

func (s *syncAction) getResourcesMaps(buildInv *sync.Inventory) (map[string]*sync.OrderedMap[*sync.Resource], map[string]string, error) {
	resourcesMap := make(map[string]*sync.OrderedMap[*sync.Resource])
	packagePathMap := make(map[string]string)

	plasmaCompose, err := compose.Lookup(os.DirFS(s.domainDir))
	if err != nil {
		return nil, nil, err
	}

	var priorityOrder []string
	for _, dep := range plasmaCompose.Dependencies {
		pkg := dep.ToPackage(dep.Name)
		packagePathMap[dep.Name] = filepath.Join(s.packagesDir, pkg.GetName(), pkg.GetTarget())
		priorityOrder = append(priorityOrder, dep.Name)
	}

	packagePathMap[domainNamespace] = s.domainDir

	priorityOrder = append(priorityOrder, domainNamespace)

	var wg async.WaitGroup
	var mx async.Mutex

	maxWorkers := min(runtime.NumCPU(), len(packagePathMap))
	workChan := make(chan map[string]string, len(packagePathMap))
	errorChan := make(chan error, 1)

	for i := 0; i < maxWorkers; i++ {
		go func() {
			for repo := range workChan {
				resources, errRes := s.getResourcesMapFrom(repo["path"])
				if errRes != nil {
					errorChan <- errRes
					return
				}

				mx.Lock()
				resourcesMap[repo["package"]] = resources
				mx.Unlock()
				wg.Done()
			}
		}()
	}

	for pkg, path := range packagePathMap {
		wg.Add(1)
		workChan <- map[string]string{"path": path, "package": pkg}
	}

	close(workChan)

	go func() {
		wg.Wait()
		close(errorChan)
	}()

	for err = range errorChan {
		if err != nil {
			return nil, nil, err
		}
	}

	// Remove unused resources from packages maps.
	if s.filterByResourceUsage {
		usedResources := buildInv.GetUsedResources()
		if len(usedResources) == 0 {
			// Empty maps and return, as no resources are used in build.
			resourcesMap = make(map[string]*sync.OrderedMap[*sync.Resource])
			packagePathMap = make(map[string]string)

			return resourcesMap, packagePathMap, nil
		}

		s.Log().Info("List of used resources:")
		var ur []string
		for r := range usedResources {
			ur = append(ur, r)
		}

		sort.Strings(ur)
		for _, r := range ur {
			s.Log().Info(fmt.Sprintf("- %s", r))
		}

		s.Log().Info("List of unused resources:")
		for p, resources := range resourcesMap {
			s.Log().Info(fmt.Sprintf("- Package - %s -", p))
			for _, k := range resources.Keys() {
				if _, ok := usedResources[k]; !ok {
					s.Log().Info(fmt.Sprintf("- %s", k))
					resources.Unset(k)
				}
			}
		}
	}

	buildResources := buildInv.GetResourcesMap()
	for _, resourceName := range buildResources.Keys() {
		conflicts := make(map[string]string)
		for name, resources := range resourcesMap {
			if _, ok := resources.Get(resourceName); ok {
				conflicts[name] = ""
			}
		}

		if len(conflicts) < 2 {
			continue
		}

		buildResourceEntity := sync.NewResource(resourceName, s.buildDir)
		buildVersion, debug, err := buildResourceEntity.GetVersion()
		for _, d := range debug {
			s.Log().Debug("error", "message", d)
		}
		if err != nil {
			return nil, nil, err
		}

		var sameVersionNamespaces []string
		for conflictingNamespace := range conflicts {
			conflictEntity := sync.NewResource(resourceName, packagePathMap[conflictingNamespace])

			baseVersion, _, debug, err := conflictEntity.GetBaseVersion()
			for _, d := range debug {
				s.Log().Debug("error", "message", d)
			}

			if err != nil {
				return nil, nil, err
			}

			if baseVersion != buildVersion {
				s.Log().Debug("removing resource from namespace because of composition strategy",
					"resource", resourceName, "version", baseVersion, "buildVersion", buildVersion, "namespace", conflictingNamespace)
				resourcesMap[conflictingNamespace].Unset(resourceName)
			} else {
				sameVersionNamespaces = append(sameVersionNamespaces, conflictingNamespace)
			}
		}

		if len(sameVersionNamespaces) > 1 {
			s.Log().Debug("resolving additional strategies conflict for resource", "resource", resourceName)
			var highest string
			for i := len(priorityOrder) - 1; i >= 0; i-- {
				if _, ok := resourcesMap[priorityOrder[i]]; ok {
					highest = priorityOrder[i]
					break
				}
			}

			for i := len(priorityOrder) - 1; i >= 0; i-- {
				if priorityOrder[i] != highest {
					if _, ok := resourcesMap[priorityOrder[i]]; ok {
						resourcesMap[priorityOrder[i]].Unset(resourceName)
					}
				}
			}
		}
	}

	return resourcesMap, packagePathMap, nil
}

func (s *syncAction) buildPropagationMap(buildInv *sync.Inventory, timeline []sync.TimelineItem) (*sync.OrderedMap[*sync.Resource], map[string]string, error) {
	resourceVersionMap := make(map[string]string)
	toPropagate := sync.NewOrderedMap[*sync.Resource]()
	resourcesMap := buildInv.GetResourcesMap()
	processed := make(map[string]bool)
	sync.SortTimeline(timeline, sync.SortDesc)

	usedResources := make(map[string]bool)
	if s.filterByResourceUsage {
		usedResources = buildInv.GetUsedResources()
	}

	s.Log().Info("Iterating timeline")
	for _, item := range timeline {
		dependenciesLog := sync.NewOrderedMap[bool]()

		switch i := item.(type) {
		case *sync.TimelineResourcesItem:
			resources := i.GetResources()
			resources.SortKeysAlphabetically()

			var toProcess []string
			for _, key := range resources.Keys() {
				// Skip resource if it was processed by previous timeline item or previous resource (via deps).
				if processed[key] {
					continue
				}

				r, _ := resources.Get(key)

				if !sync.IsUpdatableKind(r.GetKind()) {
					s.Log().Warn(fmt.Sprintf("%s is not allowed to propagate", key))
					continue
				}

				toProcess = append(toProcess, key)
			}

			if len(toProcess) == 0 {
				continue
			}

			for _, key := range toProcess {
				r, ok := resources.Get(key)
				if !ok {
					return nil, nil, fmt.Errorf("unknown key %s detected during timeline iteration", key)
				}

				processed[key] = true

				dependentResources := buildInv.GetRequiredByResources(r.GetName(), -1)
				if s.filterByResourceUsage {
					for dr := range dependentResources {
						if _, okU := usedResources[dr]; !okU {
							delete(dependentResources, dr)
						}
					}
				}

				for dep := range dependentResources {
					depResource, okR := resourcesMap.Get(dep)
					if !okR {
						continue
					}

					// Skip resource if it was processed by previous timeline item or previous resource (via deps).
					if processed[dep] {
						continue
					}

					processed[dep] = true

					if !sync.IsUpdatableKind(depResource.GetKind()) {
						s.Log().Warn(fmt.Sprintf("%s is not allowed to propagate", dep))
						continue
					}

					toPropagate.Set(dep, depResource)
					resourceVersionMap[dep] = i.GetVersion()

					if _, okD := resources.Get(dep); !okD {
						dependenciesLog.Set(dep, true)
					}
				}
			}

			for _, key := range toProcess {
				// Ensure new version removes previous propagation for that resource.
				toPropagate.Unset(key)
				delete(resourceVersionMap, key)
			}

			if dependenciesLog.Len() > 0 {
				s.Log().Debug("timeline item (resources)",
					slog.String("version", item.GetVersion()),
					slog.Time("date", item.GetDate()),
					slog.String("resources", fmt.Sprintf("%v", toProcess)),
					slog.String("dependencies", fmt.Sprintf("%v", dependenciesLog.Keys())),
				)
			}

		case *sync.TimelineVariablesItem:
			variables := i.GetVariables()
			variables.SortKeysAlphabetically()

			var resources []string
			for _, v := range variables.Keys() {
				variable, _ := variables.Get(v)
				vr := buildInv.GetVariableResources(variable.GetName(), variable.GetPlatform())

				if len(usedResources) == 0 {
					resources = append(resources, vr...)
				} else {
					var usedVr []string
					for _, r := range vr {
						if _, ok := usedResources[r]; ok {
							usedVr = append(usedVr, r)
						}
					}
					resources = append(resources, usedVr...)
				}
			}

			slices.Sort(resources)
			resources = slices.Compact(resources)

			var toProcess []string
			for _, key := range resources {
				if processed[key] {
					continue
				}
				toProcess = append(toProcess, key)
			}

			if len(toProcess) == 0 {
				continue
			}

			for _, r := range toProcess {
				// First set version for main resource.
				mainResource, okM := resourcesMap.Get(r)
				if !okM {
					s.Log().Warn(fmt.Sprintf("skipping not valid resource %s (direct vars dependency)", r))
					continue
				}

				processed[r] = true

				if sync.IsUpdatableKind(mainResource.GetKind()) {
					toPropagate.Set(r, mainResource)
					resourceVersionMap[r] = i.GetVersion()
					dependenciesLog.Set(r, true)
				}

				// Set versions for dependent resources.
				dependentResources := buildInv.GetRequiredByResources(r, -1)
				for dep := range dependentResources {
					depResource, okR := resourcesMap.Get(dep)
					if !okR {
						s.Log().Warn(fmt.Sprintf("skipping not valid resource %s (dependency of %s)", dep, r))
						continue
					}

					// Skip resource if it was processed by previous timeline item or previous resource (via deps).
					if processed[dep] {
						continue
					}

					processed[dep] = true

					if !sync.IsUpdatableKind(depResource.GetKind()) {
						s.Log().Warn(fmt.Sprintf("%s is not allowed to propagate", dep))
						continue
					}

					toPropagate.Set(dep, depResource)
					resourceVersionMap[dep] = i.GetVersion()

					dependenciesLog.Set(dep, true)
				}
			}

			if dependenciesLog.Len() > 0 {
				s.Log().Debug("timeline item (variables)",
					slog.String("version", item.GetVersion()),
					slog.Time("date", item.GetDate()),
					slog.String("variables", fmt.Sprintf("%v", variables.Keys())),
					slog.String("resources", fmt.Sprintf("%v", dependenciesLog.Keys())),
				)
			}
		}
	}

	return toPropagate, resourceVersionMap, nil
}

func (s *syncAction) updateResources(resourceVersionMap map[string]string, toPropagate *sync.OrderedMap[*sync.Resource]) error {
	var sortList []string
	updateMap := make(map[string]map[string]string)
	stopPropagation := false

	s.Log().Info("Sorting resources before update")
	for _, key := range toPropagate.Keys() {
		r, _ := toPropagate.Get(key)
		baseVersion, currentVersion, debug, errVersion := r.GetBaseVersion()
		for _, d := range debug {
			s.Log().Debug("error", "message", d)
		}
		if errVersion != nil {
			return errVersion
		}

		if currentVersion == "" {
			s.Term().Warning().Printfln("resource %s has no version", r.GetName())
			stopPropagation = true
		}

		newVersion := composeVersion(currentVersion, resourceVersionMap[r.GetName()])
		if baseVersion == resourceVersionMap[r.GetName()] {
			s.Log().Debug("skip identical",
				"baseVersion", baseVersion, "currentVersion", currentVersion, "propagateVersion", resourceVersionMap[r.GetName()], "newVersion", newVersion)
			s.Term().Warning().Printfln("- skip %s (identical versions)", r.GetName())
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
		return errors.New("empty version has been detected, please check log")
	}

	if len(updateMap) == 0 {
		s.Term().Printfln("No version to propagate")
		return nil
	}

	sort.Strings(sortList)
	s.Log().Info("Propagating versions")

	var p *pterm.ProgressbarPrinter
	if s.showProgress {
		p, _ = pterm.DefaultProgressbar.WithWriter(s.Term()).WithTotal(len(sortList)).WithTitle("Updating resources").Start()
	}
	for _, key := range sortList {
		if p != nil {
			p.Increment()
		}

		val := updateMap[key]

		r, ok := toPropagate.Get(key)
		currentVersion := val["current"]
		newVersion := val["new"]
		if !ok {
			return fmt.Errorf("unidentified resource found during update %s", key)
		}

		s.Log().Info(fmt.Sprintf("%s from %s to %s", r.GetName(), currentVersion, newVersion))
		if s.dryRun {
			continue
		}

		debug, err := r.UpdateVersion(newVersion)
		for _, d := range debug {
			s.Log().Debug("error", "message", d)
		}
		if err != nil {
			return err
		}
	}

	return nil
}

func composeVersion(oldVersion string, newVersion string) string {
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

func (s *syncAction) getResourcesMapFrom(dir string) (*sync.OrderedMap[*sync.Resource], error) {
	inv, err := sync.NewInventory(dir, s.Log())
	if err != nil {
		return nil, err
	}

	rm := inv.GetResourcesMap()
	rm.SortKeysAlphabetically()
	return rm, nil
}

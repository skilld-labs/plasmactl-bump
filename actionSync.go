package plasmactlbump

import (
	"errors"
	"fmt"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"log/slog"
	"math"
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

// SyncAction is a type representing a resources version synchronization action.
type SyncAction struct {
	// services.
	keyring keyring.Keyring

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
	commitsAfter          string
	vaultPass             string
	showProgress          bool
}

type hashStruct struct {
	hash     string
	hashTime time.Time
	author   string
}

// Execute the sync action to propagate resources' versions.
func (s *SyncAction) Execute() error {
	launchr.Term().Info().Println("Processing propagation...")

	err := s.ensureVaultpassExists()
	if err != nil {
		return err
	}

	err = s.propagate()
	if err != nil {
		return err
	}
	launchr.Term().Info().Println("Propagation has been finished")

	if s.saveKeyring {
		err = s.keyring.Save()
	}

	return err
}

func (s *SyncAction) ensureVaultpassExists() error {
	keyValueItem, errGet := s.keyring.GetForKey(vaultpassKey)
	if errGet != nil {
		if errors.Is(errGet, keyring.ErrEmptyPass) {
			return errGet
		} else if !errors.Is(errGet, keyring.ErrNotFound) {
			launchr.Log().Debug("keyring error", "error", errGet)
			return errMalformedKeyring
		}

		keyValueItem.Key = vaultpassKey
		keyValueItem.Value = s.vaultPass

		if keyValueItem.Value == "" {
			launchr.Term().Printf("- Ansible vault password\n")
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

	s.vaultPass = keyValueItem.Value

	return nil
}

func (s *SyncAction) propagate() error {
	s.timeline = sync.CreateTimeline()

	launchr.Log().Info("Initializing build inventory")
	inv, err := sync.NewInventory(s.buildDir)
	if err != nil {
		return err
	}

	if s.filterByResourceUsage {
		launchr.Log().Info("Calculating resources usage")
		err = inv.CalculateResourcesUsage()
		if err != nil {
			return fmt.Errorf("calculate resources usage > %w", err)
		}
	}

	launchr.Log().Info("Calculating variables usage")
	err = inv.CalculateVariablesUsage(s.vaultPass)
	if err != nil {
		return fmt.Errorf("calculate variables usage > %w", err)
	}

	err = s.buildTimeline(inv)
	if err != nil {
		return fmt.Errorf("building timeline > %w", err)
	}

	if len(s.timeline) == 0 {
		launchr.Term().Warning().Println("No resources were found for propagation")
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

func (s *SyncAction) buildTimeline(buildInv *sync.Inventory) error {
	launchr.Log().Info("Gathering domain and packages resources")
	resourcesMap, packagePathMap, err := s.getResourcesMaps(buildInv)
	if err != nil {
		return fmt.Errorf("build resource map > %w", err)
	}

	launchr.Log().Info("Populate timeline with resources")
	err = s.populateTimelineResources(resourcesMap, packagePathMap)
	if err != nil {
		return fmt.Errorf("iteraring resources > %w", err)
	}

	launchr.Log().Info("Populate timeline with variables")
	err = s.populateTimelineVars(buildInv)
	if err != nil {
		return fmt.Errorf("iteraring variables > %w", err)
	}

	return nil
}

func (s *SyncAction) getResourcesMaps(buildInv *sync.Inventory) (map[string]*sync.OrderedMap[*sync.Resource], map[string]string, error) {
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
				resources, errRes := getResourcesMapFrom(repo["path"])
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

		launchr.Log().Info("List of used resources:")
		var ur []string
		for r := range usedResources {
			ur = append(ur, r)
		}

		sort.Strings(ur)
		for _, r := range ur {
			launchr.Log().Info(fmt.Sprintf("- %s", r))
		}

		launchr.Log().Info("List of unused resources:")
		for p, resources := range resourcesMap {
			launchr.Log().Info(fmt.Sprintf("- Package - %s -", p))
			for _, k := range resources.Keys() {
				if _, ok := usedResources[k]; !ok {
					launchr.Log().Info(fmt.Sprintf("- %s", k))
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
		buildVersion, err := buildResourceEntity.GetVersion()
		if err != nil {
			return nil, nil, err
		}

		var sameVersionNamespaces []string
		for conflictingNamespace := range conflicts {
			conflictEntity := sync.NewResource(resourceName, packagePathMap[conflictingNamespace])

			baseVersion, _, err := conflictEntity.GetBaseVersion()
			if err != nil {
				return nil, nil, err
			}

			if baseVersion != buildVersion {
				launchr.Log().Debug("removing resource from namespace as other version was used during composition",
					"resource", resourceName, "version", baseVersion, "buildVersion", buildVersion, "namespace", conflictingNamespace)
				resourcesMap[conflictingNamespace].Unset(resourceName)
			} else {
				sameVersionNamespaces = append(sameVersionNamespaces, conflictingNamespace)
			}
		}

		if len(sameVersionNamespaces) > 1 {
			launchr.Log().Debug("resolving additional strategies conflict for resource", "resource", resourceName)
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

func (s *SyncAction) buildPropagationMap(buildInv *sync.Inventory, timeline []sync.TimelineItem) (*sync.OrderedMap[*sync.Resource], map[string]string, error) {
	resourceVersionMap := make(map[string]string)
	toPropagate := sync.NewOrderedMap[*sync.Resource]()
	resourcesMap := buildInv.GetResourcesMap()
	processed := make(map[string]bool)
	sync.SortTimeline(timeline, sync.SortDesc)

	usedResources := make(map[string]bool)
	if s.filterByResourceUsage {
		usedResources = buildInv.GetUsedResources()
	}

	launchr.Log().Info("Iterating timeline")
	for _, item := range timeline {
		dependenciesLog := sync.NewOrderedMap[bool]()

		switch i := item.(type) {
		case *sync.TimelineResourcesItem:
			resources := i.GetResources()
			resources.SortKeysAlphabetically()

			var toProcess []string
			for _, key := range resources.Keys() {
				if processed[key] {
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
				launchr.Log().Debug("timeline item (resources)",
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
					launchr.Log().Warn(fmt.Sprintf("skipping not valid resource %s (direct vars dependency)", r))
					continue
				}

				processed[r] = true
				toPropagate.Set(r, mainResource)
				resourceVersionMap[r] = i.GetVersion()
				dependenciesLog.Set(r, true)

				// Set versions for dependent resources.
				dependentResources := buildInv.GetRequiredByResources(r, -1)
				for dep := range dependentResources {
					depResource, okR := resourcesMap.Get(dep)
					if !okR {
						launchr.Log().Warn(fmt.Sprintf("skipping not valid resource %s (dependency of %s)", dep, r))
						continue
					}

					// Skip resource if it was processed by previous timeline item or previous resource (via deps).
					if processed[dep] {
						continue
					}

					processed[dep] = true

					toPropagate.Set(dep, depResource)
					resourceVersionMap[dep] = i.GetVersion()

					dependenciesLog.Set(dep, true)
				}
			}

			if dependenciesLog.Len() > 0 {
				launchr.Log().Debug("timeline item (variables)",
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

func (s *SyncAction) updateResources(resourceVersionMap map[string]string, toPropagate *sync.OrderedMap[*sync.Resource]) error {
	var sortList []string
	updateMap := make(map[string]map[string]string)
	stopPropagation := false

	launchr.Log().Info("Sorting resources before update")
	for _, key := range toPropagate.Keys() {
		r, _ := toPropagate.Get(key)
		baseVersion, currentVersion, errVersion := r.GetBaseVersion()
		if errVersion != nil {
			return errVersion
		}

		if currentVersion == "" {
			launchr.Term().Warning().Printfln("resource %s has no version", r.GetName())
			stopPropagation = true
		}

		newVersion := composeVersion(currentVersion, resourceVersionMap[r.GetName()])
		if baseVersion == resourceVersionMap[r.GetName()] {
			launchr.Log().Debug("skip identical",
				"baseVersion", baseVersion, "currentVersion", currentVersion, "propagateVersion", resourceVersionMap[r.GetName()], "newVersion", newVersion)
			launchr.Term().Warning().Printfln("- skip %s (identical versions)", r.GetName())
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
		launchr.Term().Printfln("No version to propagate")
		return nil
	}

	sort.Strings(sortList)
	launchr.Log().Info("Propagating versions")

	//var p *pterm.ProgressbarPrinter
	var p *mpb.Progress
	var b *mpb.Bar
	if s.showProgress {
		//p, _ = pterm.DefaultProgressbar.WithTotal(len(sortList)).WithTitle("Updating resources").Start()
		p = mpb.New(mpb.WithWidth(64))
		b = p.New(int64(len(sortList)),
			//mpb.BarStyle().Lbound("╢").Filler("▌").Tip("▌").Padding("░").Rbound("╟"),
			mpb.BarStyle(),
			mpb.PrependDecorators(
				decor.Name("Updating resources:", decor.WC{C: decor.DindentRight | decor.DextraSpace}),
				//decor.OnComplete(decor.AverageETA(decor.ET_STYLE_GO), "done in "),
				decor.OnComplete(decor.Name("processing... "), "done in "),
				mpbTimeDecorator(time.Now()),
				decor.CountersNoUnit(" [%d/%d] "),
			),
			mpb.AppendDecorators(decor.Percentage()),
		)
	}
	for _, key := range sortList {
		if b != nil {
			//p.Increment()
			b.Increment()
		}

		val := updateMap[key]

		r, ok := toPropagate.Get(key)
		currentVersion := val["current"]
		newVersion := val["new"]
		if !ok {
			return fmt.Errorf("unidentified resource found during update %s", key)
		}

		launchr.Log().Info(fmt.Sprintf("%s from %s to %s", r.GetName(), currentVersion, newVersion))

		//time.Sleep(100 * time.Millisecond)
		if s.dryRun {
			continue
		}

		err := r.UpdateVersion(newVersion)
		if err != nil {
			return err
		}
	}
	if p != nil {
		p.Wait()
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

func getResourcesMapFrom(dir string) (*sync.OrderedMap[*sync.Resource], error) {
	inv, err := sync.NewInventory(dir)
	if err != nil {
		return nil, err
	}

	rm := inv.GetResourcesMap()
	rm.SortKeysAlphabetically()
	return rm, nil
}

func mpbTimeDecorator(t time.Time, wcc ...decor.WC) decor.Decorator {
	return decor.Any(func(decor.Statistics) string { return fmt.Sprintf("%.2fs", math.RoundToEven(time.Since(t).Seconds())) }, wcc...)
}

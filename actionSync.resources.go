package plasmactlbump

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	async "sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/launchrctl/launchr"
	"github.com/pterm/pterm"

	"github.com/skilld-labs/plasmactl-bump/v2/pkg/repository"
	"github.com/skilld-labs/plasmactl-bump/v2/pkg/sync"
)

var errRunBruteProcess = fmt.Errorf("run brute")

const (
	headGroupName = "head"
)

// CommitsGroup is simple struct that contains list of commits under some group. Group has name, date and parent commit.
type CommitsGroup struct {
	name   string
	commit string
	items  []string
	date   time.Time
}

func (s *SyncAction) populateTimelineResources(resources map[string]*sync.OrderedMap[*sync.Resource], packagePathMap map[string]string) error {
	var wg async.WaitGroup
	var mx async.Mutex

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errorChan := make(chan error, 1)
	maxWorkers := min(runtime.NumCPU(), len(packagePathMap))
	workChan := make(chan map[string]any, len(packagePathMap))

	multi := pterm.DefaultMultiPrinter

	for i := 0; i < maxWorkers; i++ {
		go func(workerID int) {
			for {
				select {
				case <-ctx.Done():
					return
				case domain, ok := <-workChan:
					if !ok {
						return
					}

					name := domain["name"].(string)
					path := domain["path"].(string)
					pb := domain["pb"].(*pterm.ProgressbarPrinter)

					if err := s.findResourcesChangeTime(ctx, resources[name], path, &mx, pb); err != nil {
						select {
						case errorChan <- fmt.Errorf("worker %d error processing %s: %w", workerID, name, err):
							cancel()
						default:
						}
						return
					}
					wg.Done()
				}
			}
		}(i)
	}

	for name, path := range packagePathMap {
		if resources[name].Len() == 0 {
			// Skipping packages with 0 composed resources.
			continue
		}

		wg.Add(1)

		var p *pterm.ProgressbarPrinter
		var err error
		if s.showProgress {
			p, err = pterm.DefaultProgressbar.WithTotal(resources[name].Len()).WithWriter(multi.NewWriter()).Start(fmt.Sprintf("Collecting resources from %s", name))
			if err != nil {
				return err
			}
		}

		workChan <- map[string]any{"name": name, "path": path, "pb": p}
	}
	close(workChan)
	go func() {
		if s.showProgress {
			_, err := multi.Start()
			if err != nil {
				errorChan <- fmt.Errorf("error starting multi progress bar: %w", err)
			}
		}

		wg.Wait()
		close(errorChan)
	}()

	for err := range errorChan {
		if err != nil {
			return err
		}
	}

	// Sleep to re-render progress bar. Needed to achieve latest state.
	if s.showProgress {
		time.Sleep(multi.UpdateDelay)
		_, _ = multi.Stop()
	}

	return nil
}

func collectResourcesCommits(r *git.Repository, beforeDate string) (*sync.OrderedMap[*CommitsGroup], map[string]map[string]string, error) {
	ref, err := r.Head()
	if err != nil {
		return nil, nil, fmt.Errorf("can't get HEAD ref > %w", err)
	}

	hashes := make(map[string]map[string]string)
	var commits []string
	var section string
	var sectionName string
	var sectionDate time.Time

	// start from the latest commit and iterate to the past
	cIter, err := r.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil, nil, fmt.Errorf("git log error > %w", err)
	}

	var before time.Time

	if beforeDate != "" {
		before, err = time.Parse(time.DateOnly, beforeDate)
		if err != nil {
			return nil, nil, fmt.Errorf("can't parse date %s, format should be %s > %w", beforeDate, time.DateOnly, err)
		}
	}

	groups := sync.NewOrderedMap[*CommitsGroup]()

	_ = cIter.ForEach(func(c *object.Commit) error {
		if c.Author.When.Before(before) {
			return storer.ErrStop
		}

		hash := c.Hash.String()
		hash = hash[:13]
		if _, ok := hashes[hash]; !ok {
			hashes[hash] = make(map[string]string)
			hashes[hash]["original"] = c.Hash.String()
			hashes[hash]["section"] = ""
		} else {
			panic(fmt.Sprintf("dupicate version hash has been met %s during commits iteration", hash))
		}

		if ref.Hash() == c.Hash {
			commits = []string{}
			sectionDate = c.Author.When
			if c.Author.Name == repository.Author {
				section = c.Hash.String()
				sectionName = section
				hashes[hash]["section"] = sectionName
			} else {
				section = ref.Hash().String()
				sectionName = headGroupName
				hashes[hash]["section"] = sectionName
				commits = append(commits, c.Hash.String())
			}

			return nil
		}

		// create new group when bump commits appears and store previous one.
		if c.Author.Name == repository.Author {
			group := &CommitsGroup{
				name:   sectionName,
				commit: section,
				date:   sectionDate,
				items:  commits,
			}

			groups.Set(section, group)

			section = c.Hash.String()
			sectionName = c.Hash.String()
			sectionDate = c.Author.When
			commits = []string{}
		} else {
			hashes[hash]["section"] = section
			commits = append(commits, c.Hash.String())
		}

		return nil
	})

	if _, ok := groups.Get(section); !ok {
		group := &CommitsGroup{
			name:   sectionName,
			commit: section,
			date:   sectionDate,
			items:  commits,
		}

		groups.Set(section, group)
	}

	return groups, hashes, nil
}

func (s *SyncAction) findResourcesChangeTime(ctx context.Context, namespaceResources *sync.OrderedMap[*sync.Resource], gitPath string, mx *async.Mutex, p *pterm.ProgressbarPrinter) error {
	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return fmt.Errorf("%s - %w", gitPath, err)
	}

	groups, commitsMap, err := collectResourcesCommits(repo, s.timeDepth)
	if err != nil {
		return fmt.Errorf("collect resources commits > %w", err)
	}

	var wg async.WaitGroup
	errorChan := make(chan error, 1)
	//maxWorkers := 3
	maxWorkers := runtime.NumCPU()
	resourcesChan := make(chan *sync.Resource, namespaceResources.Len())

	for w := 0; w < maxWorkers; w++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case r, ok := <-resourcesChan:
					if !ok {
						return
					}
					if err = s.processResource(r, groups, commitsMap, repo, gitPath, mx); err != nil {
						if p != nil {
							_, _ = p.Stop()
						}

						select {
						case errorChan <- err:
						default:
						}
					}
					if p != nil {
						p.Increment()
					}

					wg.Done()
				}
			}
		}()
	}

	for _, k := range namespaceResources.Keys() {
		r, ok := namespaceResources.Get(k)
		if !ok {
			continue
		}

		wg.Add(1)
		resourcesChan <- r
	}
	close(resourcesChan)

	go func() {
		wg.Wait()
		close(errorChan)
	}()

	for err = range errorChan {
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *SyncAction) processResource(resource *sync.Resource, commitsGroups *sync.OrderedMap[*CommitsGroup], commitsMap map[string]map[string]string, _ *git.Repository, gitPath string, mx *async.Mutex) error {
	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return fmt.Errorf("%s - %w", gitPath, err)
	}

	buildResource := sync.NewResource(resource.GetName(), s.buildDir)
	currentVersion, err := buildResource.GetVersion()
	if err != nil {
		return err
	}

	versionHash := &hashStruct{
		hash:     buildHackAuthor,
		hashTime: time.Now(),
		author:   buildHackAuthor,
	}

	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("can't get HEAD ref > %w", err)
	}

	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return fmt.Errorf("can't get HEAD commit object > %w", err)
	}

	resourceMetaPath := resource.BuildMetaPath()

	file, err := headCommit.File(resourceMetaPath)
	if err != nil {
		return fmt.Errorf("opening file %s in commit %s > %w", resourceMetaPath, headCommit.Hash, err)
	}

	metaFile, err := loadYamlFileFromBytes(file, resourceMetaPath)
	if err != nil {
		return fmt.Errorf("YAML load commit %s > %w", headCommit.Hash, err)
	}

	currentMetaHash := file.Hash.String()
	headVersion := sync.GetMetaVersion(metaFile)

	// Ensure actual version and head versions match.
	// If actual version doesn't match head commit. Ensure override is allowed.
	// If override is not allowed, return error.
	// In other case add new timeline item with overridden version.

	overridden := false
	if currentVersion != headVersion {
		msg := fmt.Sprintf("Version of `%s` doesn't match HEAD commit", resource.GetName())
		if !s.allowOverride {
			return errors.New(msg)
		}

		launchr.Log().Warn(msg)
		overridden = true
	} else {
		versionHash.hash = headCommit.Hash.String()
		versionHash.hashTime = headCommit.Author.When
		versionHash.author = headCommit.Author.Name
	}

	if !overridden {
		// @todo rewrite to concurrent map ?
		//mx.Lock()
		item, ok := commitsMap[currentVersion]
		//mx.Unlock()
		if !ok {
			launchr.Log().Warn(fmt.Sprintf("Latest version of `%s` doesn't match any existing commit", resource.GetName()))
		}

		var commit *object.Commit
		var errProcess error

		if len(item) == 0 {
			commit, errProcess = s.processUnknownSection(commitsGroups, resourceMetaPath, currentVersion, repo, currentMetaHash)
		} else {
			group, okSection := commitsGroups.Get(item["section"])
			if !okSection {
				panic(fmt.Sprintf("Requested group %s doesn't exist", item["section"]))
			}

			commit, errProcess = s.processBumpSection(group, resourceMetaPath, currentVersion, repo, currentMetaHash)
		}

		if errors.Is(errProcess, errRunBruteProcess) {
			commit, errProcess = s.processAllSections(commitsGroups, resourceMetaPath, currentVersion, repo, currentMetaHash)
		}

		if errProcess != nil {
			return errProcess
		}

		if commit == nil {
			return fmt.Errorf("couldn't find version commit for %s", resource.GetName())
		}

		versionHash.hash = commit.Hash.String()
		versionHash.hashTime = commit.Author.When
		versionHash.author = commit.Author.Name
	}

	mx.Lock()
	defer mx.Unlock()

	launchr.Log().Debug("add resource to timeline",
		slog.String("mrn", resource.GetName()),
		slog.String("commit", versionHash.hash),
		slog.String("version", currentVersion),
		slog.Time("date", versionHash.hashTime),
	)

	if versionHash.author != repository.Author && versionHash.author != buildHackAuthor {
		launchr.Log().Warn(fmt.Sprintf("Latest commit of %s is not a bump commit", resource.GetName()))
	}

	tri := sync.NewTimelineResourcesItem(currentVersion, versionHash.hash, versionHash.hashTime)
	tri.AddResource(resource)

	s.timeline = sync.AddToTimeline(s.timeline, tri)

	return nil
}

func (s *SyncAction) processAllSections(commitsGroups *sync.OrderedMap[*CommitsGroup], resourceMetaPath, currentVersion string, repo *git.Repository, originalHash string) (*object.Commit, error) {
	keys := commitsGroups.Keys()
	for i := commitsGroups.Len() - 1; i >= 0; i-- {
		group, _ := commitsGroups.Get(keys[i])
		sectionCommit, errGr := repo.CommitObject(plumbing.NewHash(group.commit))
		if errGr != nil {
			return nil, fmt.Errorf("can't get group commit object %s > %w", group.commit, errGr)
		}

		var commitWeNeed *object.Commit
		var fileHash string

		if group.name == headGroupName {
			// Well, if we are in head, it's the final line of defense.
			fileHash = originalHash
			commitWeNeed = sectionCommit
		} else {
			sectionMetaHash, sectionMetaFile, err := getFileHashFromCommit(sectionCommit, resourceMetaPath)
			if err != nil {
				// Iterate until we find group which contains resource with current version.
				if errors.Is(err, object.ErrFileNotFound) {
					continue
				}

				return nil, fmt.Errorf("can't hash meta file from commit %s - %w", group.commit, err)
			}

			sectionMetaYaml, err := loadYamlFileFromBytes(sectionMetaFile, resourceMetaPath)
			if err != nil {
				return nil, fmt.Errorf("YAML load group commit %s > %w", group.commit, err)
			}

			sectionVersion := sync.GetMetaVersion(sectionMetaYaml)
			if sectionVersion != currentVersion {
				continue
			}

			commitWeNeed = sectionCommit
			fileHash = sectionMetaHash
		}

		for _, item := range group.items {
			itemCommit, errItm := repo.CommitObject(plumbing.NewHash(item))
			if errItm != nil {
				return nil, errItm
			}

			itemMetaHash, itemMetaFile, errItm := getFileHashFromCommit(itemCommit, resourceMetaPath)
			if errItm != nil {
				// Files don't exist, it means they were created in previous commit.
				if errors.Is(errItm, object.ErrFileNotFound) {
					break
				}

				return nil, fmt.Errorf("can't hash meta file from commit %s > %w", itemCommit.Hash.String(), errItm)
			}

			if fileHash == itemMetaHash {
				commitWeNeed = itemCommit
				continue
			}

			itemMetaYaml, errItm := loadYamlFileFromBytes(itemMetaFile, resourceMetaPath)
			if errItm != nil {
				return nil, fmt.Errorf("YAML load item commit %s > %w", itemCommit.Hash, errItm)
			}

			prevVer := sync.GetMetaVersion(itemMetaYaml)
			if prevVer != currentVersion {
				break
			}

			fileHash = itemMetaHash
			commitWeNeed = itemCommit
		}

		return commitWeNeed, nil
	}

	return nil, nil
}

func (s *SyncAction) processUnknownSection(commitsGroups *sync.OrderedMap[*CommitsGroup], resourceMetaPath, currentVersion string, repo *git.Repository, originalHash string) (*object.Commit, error) {
	keys := commitsGroups.Keys()
	for i := commitsGroups.Len() - 1; i >= 0; i-- {
		group, _ := commitsGroups.Get(keys[i])

		if group.name == headGroupName {
			// Well, you should have bumped your results, because we can't be sure that version was actually set in
			// head.
			// i.e. someone updated meta file (changed author), didn't bump, but version came from previous bump and in
			// this function first comparison done by file hash.
			return nil, errRunBruteProcess
		}
		sectionCommit, err := repo.CommitObject(plumbing.NewHash(group.commit))
		if err != nil {
			return nil, fmt.Errorf("can't get group commit object %s > %w", group.commit, err)
		}

		sectionMetaHash, _, err := getFileHashFromCommit(sectionCommit, resourceMetaPath)
		if err != nil {
			if errors.Is(err, object.ErrFileNotFound) {
				continue
			}

			return nil, fmt.Errorf("can't hash meta file from commit %s - %w", group.commit, err)
		}

		if originalHash != sectionMetaHash {
			continue
		}

		if len(group.items) == 0 {
			// Something wrong with process in this case. It's not possible to have version from head commits group.
			// Either someone can predict future or git history was manipulated. Send to manual search in this case.
			return nil, errRunBruteProcess
		}

		item := group.items[0]
		itemCommit, errItem := repo.CommitObject(plumbing.NewHash(item))
		if errItem != nil {
			return nil, fmt.Errorf("can't get item commit object %s > %w", itemCommit.Hash.String(), errItem)
		}

		itemMetaHash, itemMetaFile, errItem := getFileHashFromCommit(itemCommit, resourceMetaPath)
		if errItem != nil {
			// How it's possible to not have meta file in commit before bump ?
			// @todo case looks impossible, maybe makes sense to panic here
			if errors.Is(err, object.ErrFileNotFound) {
				return nil, errRunBruteProcess
			}

			return nil, fmt.Errorf("can't hash meta file from commit %s > %w", itemCommit.Hash.String(), err)
		}

		// Hashes don't match, as expected
		if originalHash != itemMetaHash {
			// Ensure real version is different
			itemMetaYaml, errMeta := loadYamlFileFromBytes(itemMetaFile, resourceMetaPath)
			if errMeta != nil {
				return nil, fmt.Errorf("YAML load item commit %s > %w", itemCommit.Hash, errMeta)
			}

			itemVer := sync.GetMetaVersion(itemMetaYaml)

			// Version match when shouldn't
			if itemVer == currentVersion {
				return nil, errRunBruteProcess
			}
		} else {
			// File hashes match when shouldn't
			return nil, errRunBruteProcess
		}

		return sectionCommit, nil
	}

	return nil, nil
}

func (s *SyncAction) processBumpSection(group *CommitsGroup, resourceMetaPath, currentVersion string, repo *git.Repository, originalHash string) (*object.Commit, error) {
	if group.name == headGroupName || len(group.items) == 0 {
		// Something wrong with process in this case. It's not possible to have version from head commits group.
		// Either someone can predict future or git history was manipulated. Send to manual search in this case.
		//panic(fmt.Sprintf("zero section items: %s %s", group.name, group.date))
		return nil, errRunBruteProcess
	}

	// Ensure bump commit has the same file hash
	sectionCommit, err := repo.CommitObject(plumbing.NewHash(group.commit))
	if err != nil {
		return nil, fmt.Errorf("can't get group commit object %s > %w", group.commit, err)
	}

	sectionMetaHash, _, err := getFileHashFromCommit(sectionCommit, resourceMetaPath)
	if err != nil {
		// 'Bad' resource version was used and assigned to group. Requires manual search.
		if errors.Is(err, object.ErrFileNotFound) {
			return nil, errRunBruteProcess
		}

		return nil, fmt.Errorf("can't hash meta file from commit %s > %w", group.commit, err)
	}

	if originalHash != sectionMetaHash {
		// 'Bad' resource version was used and assigned to group, but file exists. Requires manual search.
		return nil, errRunBruteProcess
	}

	// Ensure version from next item commit is different from bump commit.
	item := group.items[0]
	itemCommit, errItem := repo.CommitObject(plumbing.NewHash(item))
	if errItem != nil {
		return nil, fmt.Errorf("can't get item commit object %s > %w", itemCommit.Hash.String(), errItem)
	}

	itemMetaHash, itemMetaFile, errItem := getFileHashFromCommit(itemCommit, resourceMetaPath)
	if errItem != nil {
		// How it's possible to not have meta file in commit before bump ?
		// @todo case looks impossible, maybe makes sense to panic here
		if errors.Is(err, object.ErrFileNotFound) {
			return nil, errRunBruteProcess
		}

		return nil, fmt.Errorf("can't hash meta file from commit %s - %w", itemCommit.Hash.String(), err)
	}

	// Hashes don't match, as expected
	if originalHash != itemMetaHash {
		// ensure real version is different
		itemMetaYaml, errMeta := loadYamlFileFromBytes(itemMetaFile, resourceMetaPath)
		if errMeta != nil {
			return nil, fmt.Errorf("YAML load item commit %s > %w", itemCommit.Hash.String(), errMeta)
		}

		itemVersion := sync.GetMetaVersion(itemMetaYaml)
		// Version match when shouldn't
		if itemVersion == currentVersion {
			return nil, errRunBruteProcess
		}
	} else {
		// File hashes match when shouldn't
		return nil, errRunBruteProcess
	}

	return sectionCommit, nil
}

func getFileHashFromCommit(c *object.Commit, path string) (string, *object.File, error) {
	file, err := c.File(path)
	if err != nil {
		return "", nil, err
	}

	hash := file.Hash.String()

	return hash, file, err
}

func loadYamlFileFromBytes(file *object.File, path string) (map[string]any, error) {
	reader, errIt := file.Blob.Reader()
	if errIt != nil {
		return nil, fmt.Errorf("can't read %s > %w", path, errIt)
	}

	contents, errIt := io.ReadAll(reader)
	if errIt != nil {
		return nil, fmt.Errorf("can't read %s > %w", path, errIt)
	}

	yamlFile, errIt := sync.LoadYamlFileFromBytes(contents)
	if errIt != nil {
		return nil, fmt.Errorf("YAML load %s > %w", path, errIt)
	}

	return yamlFile, nil
}

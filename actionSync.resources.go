package plasmactlbump

import (
	"context"
	"errors"
	"fmt"
	"github.com/cespare/xxhash/v2"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"io"
	"os"
	"path/filepath"
	"runtime"
	async "sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/launchrctl/launchr"
	"github.com/pterm/pterm"

	"github.com/skilld-labs/plasmactl-bump/v2/pkg/repository"
	"github.com/skilld-labs/plasmactl-bump/v2/pkg/sync"
)

func (s *SyncAction) populateTimelineResources(resources map[string]*sync.OrderedMap[*sync.Resource], packagePathMap map[string]string) error {
	var wg async.WaitGroup
	var mx async.Mutex

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errorChan := make(chan error, 1)
	maxWorkers := min(runtime.NumCPU(), len(packagePathMap))
	workChan := make(chan map[string]any, len(packagePathMap))

	launchr.Term().Error().Println("1")

	multi := pterm.DefaultMultiPrinter

	for i := 0; i < maxWorkers; i++ {
		go func(workerID int) {
			for {
				select {
				case <-ctx.Done():
					return
				case domain, ok := <-workChan:
					if !ok {
						launchr.Term().Error().Println("1.1")
						return
					}

					name := domain["name"].(string)
					path := domain["path"].(string)
					pb := domain["pb"].(*pterm.ProgressbarPrinter)

					launchr.Term().Error().Println("2")
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

	delete(packagePathMap, domainNamespace)
	//delete(packagePathMap, "plasma-core")
	delete(packagePathMap, "plasma-work")

	for name, path := range packagePathMap {
		if resources[name].Len() == 0 {
			// Skipping packages with 0 composed resources.
			continue
		}

		wg.Add(1)

		var p *pterm.ProgressbarPrinter
		var err error
		if s.verbosity < 1 {
			p, err = pterm.DefaultProgressbar.WithTotal(resources[name].Len()).WithWriter(multi.NewWriter()).Start(fmt.Sprintf("Collecting resources from %s", name))
			if err != nil {
				return err
			}
		}

		workChan <- map[string]any{"name": name, "path": path, "pb": p}
	}
	close(workChan)
	go func() {
		if s.verbosity < 1 {
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
	if s.verbosity < 1 {
		time.Sleep(multi.UpdateDelay)
		multi.Stop() //nolint
	}

	return fmt.Errorf("emergency exit 2")
	return nil
}

type CommitsGroup struct {
	name   string
	commit string
	//items  map[string]bool
	items []string
	date  time.Time
}

func HashFileByPath(path string) (uint64, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return 0, err
	}
	defer file.Close()

	hash := xxhash.New()
	_, err = io.Copy(hash, file)
	if err != nil {
		return 0, err
	}

	return hash.Sum64(), nil
}

func HashFile(file io.ReadCloser) (uint64, error) {
	hash := xxhash.New()
	_, err := io.Copy(hash, file)
	if err != nil {
		return 0, err
	}

	return hash.Sum64(), nil
}

func HashFileFromCommit(c *object.Commit, path string) (uint64, *object.File, error) {
	sectionMetaFile, err := c.File(path)
	if err != nil {
		return 0, nil, nil
	}

	reader, err := sectionMetaFile.Reader()
	if err != nil {
		return 0, nil, nil
	}

	hash, err := HashFile(reader)
	if err != nil {
		return 0, nil, nil
	}

	return hash, sectionMetaFile, err
}

func collectResourcesCommits(r *git.Repository, path string, resources *sync.OrderedMap[*sync.Resource]) (*sync.OrderedMap[*CommitsGroup], map[string]map[string]string, error) {
	launchr.Term().Error().Println("4")
	hashes := make(map[string]map[string]string)
	//temp := make(map[string]map[string]any)

	//for mrn, entity := range resources.ToDict() {
	//	_, ok := temp[mrn]
	//	if !ok {
	//		temp[mrn] = make(map[string]any)
	//	}
	//
	//	version, err := entity.GetVersion()
	//	if err != nil {
	//		return nil, nil, err
	//	}
	//
	//	fileHash, err := HashFileByPath(filepath.Join(path, entity.BuildMetaPath()))
	//	if err != nil {
	//		return nil, nil, err
	//	}
	//
	//	temp[mrn]["hash"] = fileHash
	//	temp[mrn]["version"] = version
	//}

	ref, err := r.Head()
	if err != nil {
		return nil, nil, err
	}

	var commits []string
	var section string
	var sectionName string
	var sectionDate time.Time

	// start from the latest commit and iterate to the past
	cIter, err := r.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil, nil, err
	}

	before := time.Date(2022, time.January, 1, 0, 0, 0, 0, time.UTC)
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
			panic(fmt.Sprintf("%s duplicate", hash))
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
				sectionName = "head"
				hashes[hash]["section"] = sectionName
				commits = append(commits, c.Hash.String())
				//commits[c.Hash.String()] = true
			}

			return nil
		}

		// new bump commit
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
			//commits = make(map[string]bool)
			commits = []string{}
		} else {
			//if section != "head" {
			hashes[hash]["section"] = section
			//}

			//commits[c.Hash.String()] = true
			commits = append(commits, c.Hash.String())
		}

		return nil
	})

	if len(commits) != 0 {
		group := &CommitsGroup{
			name:   section,
			commit: section,
			date:   sectionDate,
			items:  commits,
		}

		groups.Set(section, group)
	}

	for _, k := range groups.Keys() {
		group, _ := groups.Get(k)
		launchr.Term().Info().Printfln("%s %s %s", group.date, group.name, group.commit)
		for _, hs := range group.items {
			launchr.Term().Warning().Printfln("- %s", hs)
		}

	}
	//launchr.Term().Warning().Printfln("%v", temp)
	launchr.Term().Warning().Printfln("%v", hashes)

	return groups, hashes, nil
}

func CopyMap(origMap map[string]bool) map[string]bool {
	newMap := make(map[string]bool)
	for key, value := range origMap {
		newMap[key] = value
	}
	return newMap
}

func (s *SyncAction) findResourcesChangeTime(ctx context.Context, namespaceResources *sync.OrderedMap[*sync.Resource], gitPath string, mx *async.Mutex, p *pterm.ProgressbarPrinter) error {
	launchr.Term().Error().Println("3")
	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return fmt.Errorf("%s - %w", gitPath, err)
	}

	groups, commitsMap, err := collectResourcesCommits(repo, gitPath, namespaceResources)
	if err != nil {
		return err
	}

	//return fmt.Errorf("emergency exit")

	var wg async.WaitGroup
	errorChan := make(chan error, 1)
	maxWorkers := 2
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
					if err = s.processResource(r, groups, commitsMap, repo, gitPath, mx, p); err != nil {
						select {
						case errorChan <- err:
						default:
						}
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

func (s *SyncAction) processResource(resource *sync.Resource, commitsGroups *sync.OrderedMap[*CommitsGroup], commitsMap map[string]map[string]string, _ *git.Repository, gitPath string, mx *async.Mutex, p *pterm.ProgressbarPrinter) error {
	mx.Lock()

	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return err
	}

	launchr.Term().Info().Printfln("processResource %s", resource.GetName())
	if resource == nil {
		panic("Can't process resource, item is empty")
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
		return err
	}

	c, err := repo.CommitObject(head.Hash())
	if err != nil {
		return err
	}

	resourceMetaPath := resource.BuildMetaPath()

	file, err := c.File(resourceMetaPath)
	if err != nil {
		return fmt.Errorf("open file %s in commit %s > %w", resourceMetaPath, c.Hash, err)
	}

	metaFile, err := s.loadYamlFileFromBytes(file, resourceMetaPath)
	if err != nil {
		return fmt.Errorf("commit %s > %w", c.Hash, err)
	}

	reader, err := file.Reader()
	if err != nil {
		return err
	}
	currentMetaHash, err := HashFile(reader)
	if err != nil {
		return err
	}

	headVersion := sync.GetMetaVersion(metaFile)

	// Ensure actual version and head versions match.
	// If actual version doesn't match head commit. Ensure override is allowed.
	// If override is not allowed, return error.
	// In other case add new timeline item with overridden version.
	if currentVersion != headVersion {
		msg := fmt.Sprintf("Version of `%s` doesn't match HEAD commit", resource.GetName())
		if !s.allowOverride {
			return errors.New(msg)
		}

		launchr.Log().Warn(msg)
	} else {
		versionHash.hash = c.Hash.String()
		versionHash.hashTime = c.Author.When
		versionHash.author = c.Author.Name
	}

	//mx.Lock()
	item, ok := commitsMap[currentVersion]
	//mx.Unlock()
	if !ok {
		launchr.Log().Warn(fmt.Sprintf("Latest version of `%s` doesn't match any existing commit", resource.GetName()))
	}

	launchr.Term().Info().Printfln("hash %s", currentMetaHash)
	launchr.Term().Info().Printfln("meta path - %s", resourceMetaPath)
	launchr.Term().Info().Printfln("item for %s %s %v", resource.GetName(), currentVersion, item)

	if len(item) == 0 {
		commit, err := s.processUnknownSection(commitsGroups, resource.GetName(), resourceMetaPath, currentVersion, repo, currentMetaHash)
		if err != nil {
			return err
		}

		launchr.Term().Info().Printfln("%s - version set in %s - %s", resource.GetName(), commit.Author.Name, commit.Hash.String())
	} else {
		// ensure bump commit has the same file hash
		section := item["section"]
		if section == "head" {
			panic("head?")
		}
		group, ok := commitsGroups.Get(section)
		if !ok {
			panic("Group not found")
		}
		commit, err := s.processBumpSection(group, resource.GetName(), resourceMetaPath, currentVersion, repo, currentMetaHash)
		if err != nil {
			return err
		}

		launchr.Term().Info().Printfln("%s - version set in %s - %s", resource.GetName(), commit.Author.Name, commit.Hash.String())
	}

	mx.Unlock()

	return nil

	//for _, hs := range history {
	//	mx.Lock()
	//	c, errIt := repo.CommitObject(plumbing.NewHash(hs.hash))
	//	if errIt != nil {
	//		return errIt
	//	}
	//	mx.Unlock()
	//
	//	resourceMetaPath := resource.BuildMetaPath()
	//
	//	file, errIt := c.File(resourceMetaPath)
	//	file.Contents()
	//	if errIt != nil {
	//		return fmt.Errorf("open file %s in commit %s > %w", resourceMetaPath, c.Hash, errIt)
	//	}
	//
	//	metaFile, errIt := s.loadYamlFileFromBytes(file, resourceMetaPath)
	//	if errIt != nil {
	//		return fmt.Errorf("commit %s > %w", c.Hash, errIt)
	//	}
	//
	//	prevVer := sync.GetMetaVersion(metaFile)
	//	if currentVersion != prevVer {
	//		break
	//	}
	//
	//	versionHash.hash = c.Hash.String()
	//	versionHash.hashTime = c.Author.When
	//	versionHash.author = c.Author.Name
	//
	//	if c.Author.Name == repository.Author {
	//		// No need to iterate more as version match and author is bumper
	//		break
	//	}
	//}

	// Ensure progress bar showing correct progress.
	//if p != nil && p.Total != p.Current {
	//	p.Add(p.Total - p.Current)
	//}

	//mx.Lock()
	//defer mx.Unlock()
	//
	//launchr.Log().Debug("add resource to timeline",
	//	slog.String("mrn", resource.GetName()),
	//	slog.String("commit", versionHash.hash),
	//	slog.String("version", currentVersion),
	//	slog.Time("date", versionHash.hashTime),
	//)
	//
	//if versionHash.author == buildHackAuthor {
	//	msg := fmt.Sprintf("Version of `%s` doesn't match HEAD commit", resource.GetName())
	//	if !s.allowOverride {
	//		return errors.New(msg)
	//	}
	//
	//	launchr.Log().Warn(msg)
	//} else if versionHash.author != repository.Author {
	//	launchr.Log().Warn(fmt.Sprintf("Latest commit of %s is not a bump commit", resource.GetName()))
	//}
	//
	//if _, ok := commitsMap[currentVersion]; !ok {
	//	launchr.Log().Warn(fmt.Sprintf("Latest version of `%s` doesn't match any existing commit", resource.GetName()))
	//}
	//
	//tri := sync.NewTimelineResourcesItem(currentVersion, versionHash.hash, versionHash.hashTime)
	//tri.AddResource(resource)
	//
	//s.timeline = sync.AddToTimeline(s.timeline, tri)
	//
	//if p != nil {
	//	p.Increment()
	//}

	return nil
}

func (s *SyncAction) findFromAll(commitsGroups *sync.OrderedMap[*CommitsGroup], resourceMetaPath, currentVersion string, repo *git.Repository, originalHash uint64) (*object.Commit, error) {
	keys := commitsGroups.Keys()
	for i := commitsGroups.Len() - 1; i >= 0; i-- {
		var commitWeNeed *object.Commit
		var fileHash uint64
		group, _ := commitsGroups.Get(keys[i])
		if group.name == "head" {
			fileHash = originalHash
			panic("Head group Not implemented yet.")
		} else {
			sectionCommit, err := repo.CommitObject(plumbing.NewHash(group.commit))
			if err != nil {
				return nil, err
			}

			sectionMetaHash, sectionMetaFile, err := HashFileFromCommit(sectionCommit, resourceMetaPath)
			if err != nil {
				return nil, err
			}

			sectionMetaYaml, err := s.loadYamlFileFromBytes(sectionMetaFile, resourceMetaPath)
			if err != nil {
				return nil, fmt.Errorf("commit %s > %w", sectionCommit.Hash, err)
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

			itemMetaHash, itemMetaFile, errItm := HashFileFromCommit(itemCommit, resourceMetaPath)
			if errItm != nil {
				return nil, errItm
			}

			if fileHash == itemMetaHash {
				continue
			}

			itemMetaYaml, errItm := s.loadYamlFileFromBytes(itemMetaFile, resourceMetaPath)
			if errItm != nil {
				return nil, fmt.Errorf("commit %s > %w", itemCommit.Hash, errItm)
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

	return nil, fmt.Errorf("empty groups")
}

func (s *SyncAction) processUnknownSection(commitsGroups *sync.OrderedMap[*CommitsGroup], mrn, resourceMetaPath, currentVersion string, repo *git.Repository, originalHash uint64) (*object.Commit, error) {
	keys := commitsGroups.Keys()
	for i := commitsGroups.Len() - 1; i >= 0; i-- {
		group, ok := commitsGroups.Get(keys[i])
		if !ok {
			panic("unknown group provided")
		}

		if group.name == "head" {
			panic("Head group Not implemented yet.")
		} else {
			sectionCommit, err := repo.CommitObject(plumbing.NewHash(group.commit))
			if err != nil {
				return nil, err
			}

			sectionMetaHash, _, err := HashFileFromCommit(sectionCommit, resourceMetaPath)
			if err != nil {
				return nil, err
			}

			if originalHash != sectionMetaHash {
				continue
			}

			item := group.items[0]

			launchr.Term().Warning().Printfln("%s", group.name)
			launchr.Term().Warning().Printfln("%v", group.items)
			launchr.Term().Info().Printfln("%s", item)

			itemCommit, errItm := repo.CommitObject(plumbing.NewHash(item))
			if errItm != nil {
				return nil, errItm
			}

			itemMetaHash, itemMetaFile, errItm := HashFileFromCommit(itemCommit, resourceMetaPath)
			if errItm != nil {
				return nil, errItm
			}

			// Hashes don't match, as expected
			if originalHash != itemMetaHash {
				launchr.Term().Info().Printfln("%s version is ok", mrn)
				// ensure real version is different
				itemMetaYaml, errItm := s.loadYamlFileFromBytes(itemMetaFile, resourceMetaPath)
				if errItm != nil {
					return nil, fmt.Errorf("commit %s > %w", itemCommit.Hash, errItm)
				}

				prevVer := sync.GetMetaVersion(itemMetaYaml)
				if prevVer == currentVersion {
					panic("version match when shouldn't")
				}
			} else {
				launchr.Term().Warning().Printfln("hashes match, need to manually iterate - 2 %s %s", sectionCommit, itemCommit)
				panic("hashes match, need to manually iterate - 2")
			}

			return sectionCommit, nil
		}
	}

	return nil, fmt.Errorf("empty groups provided")
}

func (s *SyncAction) processBumpSection(group *CommitsGroup, mrn, resourceMetaPath, currentVersion string, repo *git.Repository, originalHash uint64) (*object.Commit, error) {
	// ensure bump commit has the same file hash

	sectionCommit, err := repo.CommitObject(plumbing.NewHash(group.commit))
	if err != nil {
		return nil, err
	}

	sectionMetaHash, _, err := HashFileFromCommit(sectionCommit, resourceMetaPath)
	if err != nil {
		return nil, err
	}

	if originalHash != sectionMetaHash {
		// Send to manual search
		panic("unhandled yet")
	}

	launchr.Term().Error().Printfln("hashes for %s match", mrn)

	// ensure version from next item commit is different from bump commit.
	item := group.items[0]
	itemCommit, errItm := repo.CommitObject(plumbing.NewHash(item))
	if errItm != nil {
		return nil, errItm
	}

	itemMetaHash, itemMetaFile, errItm := HashFileFromCommit(itemCommit, resourceMetaPath)
	if errItm != nil {
		return nil, errItm
	}

	// Hashes don't match, as expected
	if originalHash != itemMetaHash {
		launchr.Term().Info().Printfln("%s version is ok", mrn)
		// ensure real version is different
		itemMetaYaml, errItm := s.loadYamlFileFromBytes(itemMetaFile, resourceMetaPath)
		if errItm != nil {
			return nil, fmt.Errorf("commit %s > %w", itemCommit.Hash, errItm)
		}

		prevVer := sync.GetMetaVersion(itemMetaYaml)
		if prevVer == currentVersion {
			panic("version doesn't match")
		}
	} else {
		panic("hashes match, need to manually iterate - 1")
	}

	return sectionCommit, nil
}

//type ManualHandleRequiredError struct {
//	code int
//	msg  string
//}
//
//func NewExitError(code int, msg string) error {
//	return ExitError{code, msg}
//}
//
//func (e ExitError) Error() string {
//	return e.msg
//}
//
//func (e ExitError) ExitCode() int {
//	return e.code
//}

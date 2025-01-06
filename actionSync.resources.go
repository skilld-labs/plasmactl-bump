package plasmactlbump

import (
	"context"
	"errors"
	"fmt"
	"github.com/cespare/xxhash/v2"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"io"
	"log/slog"
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
	delete(packagePathMap, "plasma-core")
	//delete(packagePathMap, "plasma-work")

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
	items  map[string]bool
	date   time.Time
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

	var commits map[string]bool
	var section string
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
			hashes[hash]["bump"] = ""
		} else {
			panic(fmt.Sprintf("%s duplicate", hash))
		}

		if ref.Hash() == c.Hash {
			commits = make(map[string]bool)
			sectionDate = c.Author.When
			if c.Author.Name == repository.Author {
				section = c.Hash.String()
				hashes[hash]["bump"] = section
			} else {
				section = "head"
				commits[c.Hash.String()] = true
			}

			return nil
		}

		// new bump commit
		if c.Author.Name == repository.Author {
			group := &CommitsGroup{
				name:   section,
				commit: section,
				date:   sectionDate,
				items:  commits,
			}

			groups.Set(section, group)

			section = c.Hash.String()
			sectionDate = c.Author.When
			commits = make(map[string]bool)
		} else {
			if section != "head" {
				hashes[hash]["bump"] = section
			}

			commits[c.Hash.String()] = true
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
		for hs := range group.items {
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
	launchr.Term().Printfln("%s", resourceMetaPath)

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
	hash, err := HashFile(reader)
	if err != nil {
		return err
	}
	launchr.Term().Warning().Printfln("hash %s", hash)

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

	mx.Lock()
	item, ok := commitsMap[currentVersion]
	mx.Unlock()
	if !ok {
		launchr.Log().Warn(fmt.Sprintf("Latest version of `%s` doesn't match any existing commit", resource.GetName()))
	}

	launchr.Term().Error().Println("item for %s %s %v", resource.GetName(), currentVersion, item)

	if len(item) == 0 {
		keys := commitsGroups.Keys()
		length := len(keys)
		for i := length - 1; i >= 0; i-- {
			group, _ := commitsGroups.Get(keys[i])
			launchr.Term().Warning().Printfln("%s %s %s", group.name, group.date, group.commit)
		}
	} else {
		// ensure bump commit has the same file hash
		bumpCommit := item["bump"]
		c2, err := repo.CommitObject(plumbing.NewHash(bumpCommit))
		if err != nil {
			return err
		}
		file2, err := c2.File(resourceMetaPath)
		if err != nil {
			return fmt.Errorf("open file %s in commit %s > %w", resourceMetaPath, c.Hash, err)
		}

		reader2, err := file2.Reader()
		if err != nil {
			return err
		}
		hash2, err := HashFile(reader2)
		if err != nil {
			return err
		}

		if hash == hash2 {
			launchr.Term().Error().Printfln("hashes for %s match", resource.GetName())

			group, ok := commitsGroups.Get(bumpCommit)
			if !ok {
				panic("Group not found")
			}

			for subcommit := range group.items {
				sc, err := repo.CommitObject(plumbing.NewHash(subcommit))
				if err != nil {
					return err
				}
				filesc, err := sc.File(resourceMetaPath)
				if err != nil {
					return fmt.Errorf("open file %s in commit %s > %w", resourceMetaPath, c.Hash, err)
				}

				readersc, err := filesc.Reader()
				if err != nil {
					return err
				}
				hashsc, err := HashFile(readersc)
				if err != nil {
					return err
				}

				if hashsc != hash {
					launchr.Term().Info().Printfln("%s version is ok", resource.GetName())
				}

				break
			}

		}
	}

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

	mx.Lock()
	defer mx.Unlock()

	launchr.Log().Debug("add resource to timeline",
		slog.String("mrn", resource.GetName()),
		slog.String("commit", versionHash.hash),
		slog.String("version", currentVersion),
		slog.Time("date", versionHash.hashTime),
	)

	if versionHash.author == buildHackAuthor {
		msg := fmt.Sprintf("Version of `%s` doesn't match HEAD commit", resource.GetName())
		if !s.allowOverride {
			return errors.New(msg)
		}

		launchr.Log().Warn(msg)
	} else if versionHash.author != repository.Author {
		launchr.Log().Warn(fmt.Sprintf("Latest commit of %s is not a bump commit", resource.GetName()))
	}

	if _, ok := commitsMap[currentVersion]; !ok {
		launchr.Log().Warn(fmt.Sprintf("Latest version of `%s` doesn't match any existing commit", resource.GetName()))
	}

	tri := sync.NewTimelineResourcesItem(currentVersion, versionHash.hash, versionHash.hashTime)
	tri.AddResource(resource)

	s.timeline = sync.AddToTimeline(s.timeline, tri)

	if p != nil {
		p.Increment()
	}

	return nil
}

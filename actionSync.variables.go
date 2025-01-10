package plasmactlbump

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	async "sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/launchrctl/launchr"
	"github.com/pterm/pterm"

	"github.com/skilld-labs/plasmactl-bump/v2/pkg/sync"
)

//func collectVarsFilesCommits(r *git.Repository, varsFiles []string) (map[string][]*hashStruct, error) {
//	temp := make(map[string][]*hashStruct)
//	for _, path := range varsFiles {
//		_, ok := temp[path]
//		if !ok {
//			temp[path] = []*hashStruct{}
//		}
//	}
//
//	ref, err := r.Head()
//	if err != nil {
//		return temp, err
//	}
//
//	// start from the latest commit and iterate to the past
//	cIter, err := r.Log(&git.LogOptions{From: ref.Hash()})
//	if err != nil {
//		return temp, err
//	}
//
//	_ = cIter.ForEach(func(c *object.Commit) error {
//		// Get the tree of the current commit
//		tree, err := c.Tree()
//		if err != nil {
//			return fmt.Errorf("error getting tree for commit %s: %v", c.Hash, err)
//		}
//
//		// Get the parent tree (if it exists)
//		var parentTree *object.Tree
//		if c.NumParents() > 0 {
//			parentCommit, err := c.Parents().Next()
//			if err != nil {
//				return fmt.Errorf("error getting parent commit: %v", err)
//			}
//			parentTree, err = parentCommit.Tree()
//			if err != nil {
//				return fmt.Errorf("error getting parent tree: %v", err)
//			}
//		}
//
//		// Get the changes between the two trees
//		if parentTree != nil {
//			changes, err := object.DiffTree(parentTree, tree)
//			if err != nil {
//				return fmt.Errorf("error diffing trees: %v", err)
//			}
//
//			hs := &hashStruct{
//				hash:     c.Hash.String(),
//				author:   c.Author.Name,
//				hashTime: c.Author.When,
//			}
//
//			for _, change := range changes {
//				action, err := change.Action()
//				if err != nil {
//					return fmt.Errorf("error getting change action: %v", err)
//				}
//				var path string
//
//				switch action {
//				case merkletrie.Delete:
//					path = change.From.Name
//				case merkletrie.Modify:
//					path = change.From.Name
//				case merkletrie.Insert:
//					path = change.To.Name
//				}
//
//				if _, ok := temp[path]; ok {
//					temp[path] = append(temp[path], hs)
//				}
//			}
//		}
//
//		return nil
//	})
//
//	tests := 0
//	for k, v := range temp {
//		tests = tests + 1
//		launchr.Term().Info().Printfln(k)
//		for _, hs := range v {
//			launchr.Term().Warning().Printfln("%s %s %s", hs.hashTime, hs.author, hs.hash)
//		}
//
//	}
//
//	return temp, nil
//}

func (s *SyncAction) populateTimelineVars() error {
	filesCrawler := sync.NewFilesCrawler(s.domainDir)
	groupedFiles, err := filesCrawler.FindVarsFiles("")
	if err != nil {
		return fmt.Errorf("can't get vars files > %w", err)
	}

	var varsFiles []string
	for _, paths := range groupedFiles {
		varsFiles = append(varsFiles, paths...)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg async.WaitGroup
	var mx async.Mutex

	maxWorkers := min(runtime.NumCPU(), len(varsFiles))
	workChan := make(chan string, len(varsFiles))
	errorChan := make(chan error, 1)

	var p *pterm.ProgressbarPrinter
	if s.verbosity < 1 {
		p, _ = pterm.DefaultProgressbar.WithTotal(len(varsFiles)).WithTitle("Processing variables files").Start()
	}

	for i := 0; i < maxWorkers; i++ {
		go func(workerID int) {
			for {
				select {
				case <-ctx.Done():
					return
				case varsFile, ok := <-workChan:
					if !ok {
						return
					}
					if err = s.findVariableUpdateTime(varsFile, s.domainDir, &mx); err != nil {
						if p != nil {
							_, _ = p.Stop()
						}

						select {
						case errorChan <- fmt.Errorf("worker %d error processing %s: %w", workerID, varsFile, err):
							cancel()
						default:
						}
						return
					}

					if p != nil {
						p.Increment()
					}

					wg.Done()
				}
			}
		}(i)
	}

	for _, f := range varsFiles {
		wg.Add(1)
		workChan <- f
	}
	close(workChan)

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

func (s *SyncAction) findVariableUpdateTime(varsFile string, gitPath string, mx *async.Mutex) error {
	repo, err := git.PlainOpen(gitPath)
	if err != nil {
		return fmt.Errorf("%s - %w", gitPath, err)
	}

	ref, err := repo.Head()
	if err != nil {
		return fmt.Errorf("can't get HEAD ref > %w", err)
	}

	var varsYaml map[string]any
	hashesMap := make(map[string]*hashStruct)
	variablesMap := sync.NewOrderedMap[*sync.Variable]()
	isVault := sync.IsVaultFile(varsFile)

	varsYaml, err = sync.LoadVariablesFile(filepath.Join(s.buildDir, varsFile), s.vaultPass, isVault)
	if err != nil {
		return err
	}

	for k, value := range varsYaml {
		v := sync.NewVariable(varsFile, k, HashString(fmt.Sprint(value)), isVault)
		variablesMap.Set(k, v)

		if _, ok := hashesMap[k]; !ok {
			hashesMap[k] = &hashStruct{}
		}

		hashesMap[k].hash = fmt.Sprint(v.GetHash())
		hashesMap[k].hashTime = time.Now()
		hashesMap[k].author = buildHackAuthor
	}

	//varsFileHash, err := HashFileFromPath(filepath.Join(s.buildDir, varsFile))
	//if err != nil {
	//	return fmt.Errorf("can't hash vars file %s > %w", varsFile, err)
	//}

	varsFileHash := ""

	// Used to set versions after difference found.
	// To prevent assigning same versions multiple times.
	var danglingCommit *object.Commit

	toIterate := variablesMap.ToDict()
	cIter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return err
	}

	remainingDebug := len(toIterate)
	err = cIter.ForEach(func(c *object.Commit) error {
		if len(toIterate) == 0 {
			return storer.ErrStop
		}

		if len(toIterate) != remainingDebug {
			remainingDebug = len(toIterate)
			launchr.Log().Debug(fmt.Sprintf("Remaining unidentified variables, %s - %d", varsFile, remainingDebug))
		}

		//commitFileHash, file, errIt := HashFileFromCommit(c, varsFile)
		//if errIt != nil {
		//	if !errors.Is(errIt, object.ErrFileNotFound) {
		//		return fmt.Errorf("opening file %s in commit %s > %w", varsFile, c.Hash, errIt)
		//	}
		//
		//	return storer.ErrStop
		//}

		file, errIt := c.File(varsFile)
		if errIt != nil {
			if !errors.Is(errIt, object.ErrFileNotFound) {
				return fmt.Errorf("opening file %s in commit %s > %w", varsFile, c.Hash, errIt)
			}

			return storer.ErrStop
		}

		commitFileHash := file.Hash.String()

		// No need to iterate file if it's the same between commits.
		if varsFileHash == commitFileHash {
			danglingCommit = c

			return nil
		}

		if danglingCommit != nil {
			for k := range toIterate {
				hashesMap[k].hash = danglingCommit.Hash.String()
				hashesMap[k].hashTime = danglingCommit.Author.When
				hashesMap[k].author = danglingCommit.Author.Name
			}

			danglingCommit = nil
		}

		varFile, errIt := s.loadVariablesFileFromBytes(file, varsFile, isVault)
		if errIt != nil {
			if strings.Contains(errIt.Error(), "did not find expected key") ||
				strings.Contains(errIt.Error(), "did not find expected comment or line break") ||
				strings.Contains(errIt.Error(), "could not find expected") {
				launchr.Log().Warn("Bad YAML structured detected",
					slog.String("file", varsFile),
					slog.String("commit", c.Hash.String()),
					slog.String("error", errIt.Error()),
				)

				return nil
			}

			if strings.Contains(errIt.Error(), "invalid password for vault") {
				launchr.Log().Warn("Invalid password for vault",
					slog.String("file", varsFile),
					slog.String("commit", c.Hash.String()),
				)

				return storer.ErrStop
			}

			if strings.Contains(errIt.Error(), "invalid secret format") {
				launchr.Log().Warn("invalid secret format for vault",
					slog.String("file", varsFile),
					slog.String("commit", c.Hash.String()),
				)
				return nil
			}

			return fmt.Errorf("YAML load commit %s > %w", c.Hash, errIt)
		}

		varsFileHash = commitFileHash

		for k, v := range toIterate {
			prevVar, exists := varFile[k]
			if !exists {
				// Variable didn't exist before, take current hash as version
				delete(toIterate, k)
				continue
			}

			prevVarHash := HashString(fmt.Sprint(prevVar))
			if v.GetHash() != prevVarHash {
				// Variable exists, hashes don't match, stop iterating
				delete(toIterate, k)
				continue
			}

			hashesMap[k].hash = c.Hash.String()
			hashesMap[k].hashTime = c.Author.When
			hashesMap[k].author = c.Author.Name
		}

		return nil
	})

	if err != nil {
		return err
	}

	if danglingCommit != nil {
		for k := range toIterate {
			hashesMap[k].hash = danglingCommit.Hash.String()
			hashesMap[k].hashTime = danglingCommit.Author.When
			hashesMap[k].author = danglingCommit.Author.Name
		}

		danglingCommit = nil
	}

	mx.Lock()
	defer mx.Unlock()

	for n, hm := range hashesMap {
		v, _ := variablesMap.Get(n)
		version := hm.hash[:13]
		launchr.Log().Debug("add variable to timeline",
			slog.String("variable", v.GetName()),
			slog.String("version", version),
			slog.Time("date", hm.hashTime),
			slog.String("path", v.GetPath()),
		)

		if hm.author == buildHackAuthor {
			msg := fmt.Sprintf("Value of `%s` doesn't match HEAD commit", n)
			if !s.allowOverride {
				return errors.New(msg)
			}

			launchr.Log().Warn(msg)
		}

		tri := sync.NewTimelineVariablesItem(version, hm.hash, hm.hashTime)
		tri.AddVariable(v)

		s.timeline = sync.AddToTimeline(s.timeline, tri)
	}

	return err
}

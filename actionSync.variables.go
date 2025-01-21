package plasmactlbump

import (
	"context"
	"errors"
	"fmt"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	async "sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/launchrctl/launchr"
	"github.com/skilld-labs/plasmactl-bump/v2/pkg/sync"
)

func (s *SyncAction) populateTimelineVars(buildInv *sync.Inventory) error {
	if s.filterByResourceUsage {
		// Quick return in case of empty usage pool.
		usedResources := buildInv.GetUsedResources()
		if len(usedResources) == 0 {
			return nil
		}
	}

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

	//var p *pterm.ProgressbarPrinter
	var p *mpb.Progress
	var b *mpb.Bar
	if s.showProgress {
		//p, _ = pterm.DefaultProgressbar.WithTotal(len(varsFiles)).WithTitle("Processing variables files").Start()
		p = mpb.New(mpb.WithWidth(64))
		b = p.New(int64(len(varsFiles)),
			//mpb.BarStyle().Lbound("╢").Filler("▌").Tip("▌").Padding("░").Rbound("╟"),
			mpb.BarStyle(),
			mpb.PrependDecorators(
				decor.Name("Processing variables files:", decor.WC{C: decor.DindentRight | decor.DextraSpace}),
				decor.OnComplete(decor.AverageETA(decor.ET_STYLE_GO), "done in "),
				mpbTimeDecorator(time.Now()),
				decor.CountersNoUnit(" [%d/%d] "),
			),
			mpb.AppendDecorators(decor.Percentage()),
		)
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
					if err = s.findVariableUpdateTime(varsFile, buildInv, s.domainDir, &mx); err != nil {
						if p != nil {
							p.Shutdown()
							//_, _ = p.Stop()
						}

						select {
						case errorChan <- fmt.Errorf("worker %d error processing %s: %w", workerID, varsFile, err):
							cancel()
						default:
						}
						return
					}

					if b != nil {
						b.Increment()
						//p.Increment()
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
		p.Wait()
	}()

	for err = range errorChan {
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *SyncAction) findVariableUpdateTime(varsFile string, inv *sync.Inventory, gitPath string, mx *async.Mutex) error {
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
		v := sync.NewVariable(varsFile, k, hashString(fmt.Sprint(value)), isVault)
		isUsed := inv.IsUsedVariable(s.filterByResourceUsage, v.GetName(), v.GetPlatform())
		if !isUsed {
			// launchr.Term().Warning().Printfln("Unused variable %s - %s", v.GetName(), v.GetPath())
			continue
		}

		variablesMap.Set(k, v)

		if _, ok := hashesMap[k]; !ok {
			hashesMap[k] = &hashStruct{}
		}

		hashesMap[k].hash = fmt.Sprint(v.GetHash())
		hashesMap[k].hashTime = time.Now()
		hashesMap[k].author = buildHackAuthor
	}

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

		varFile, errIt := loadVariablesFileFromBytes(file, varsFile, s.vaultPass, isVault)
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

			prevVarHash := hashString(fmt.Sprint(prevVar))
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
		return fmt.Errorf("git log error > %w", err)
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

func hashString(item string) uint64 {
	return xxhash.Sum64String(item)
}

func loadVariablesFileFromBytes(file *object.File, path, vaultPass string, isVault bool) (map[string]any, error) {
	reader, errIt := file.Blob.Reader()
	if errIt != nil {
		return nil, fmt.Errorf("can't read %s > %w", path, errIt)
	}

	contents, errIt := io.ReadAll(reader)
	if errIt != nil {
		return nil, fmt.Errorf("can't read %s > %w", path, errIt)
	}

	varFile, errIt := sync.LoadVariablesFileFromBytes(contents, vaultPass, isVault)
	if errIt != nil {
		return nil, fmt.Errorf("YAML load %s > %w", path, errIt)
	}

	return varFile, nil
}

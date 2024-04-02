package plasmactlbump

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"
)

// BumperRepo encapsulates Git-related operations for bumping versions in a Git repository.
type BumperRepo struct {
	git           *git.Repository
	name          string
	mail          string
	commitMessage string
}

func getRepo() (*BumperRepo, error) {
	r, err := git.PlainOpen("./")
	if err != nil {
		return nil, err
	}

	return &BumperRepo{
		git:           r,
		name:          "Bumper",
		mail:          "no-reply@skilld.cloud",
		commitMessage: bumpSearchText,
	}, nil
}

// IsOwnCommit checks if the latest commit in the Git repository was made by the bumper.
func (r *BumperRepo) IsOwnCommit() bool {
	ref, err := r.git.Head()
	if err != nil {
		PromptError(err)
		return false
	}

	commit, err := r.git.CommitObject(ref.Hash())
	if err != nil {
		PromptError(err)
		return false
	}

	return r.name == commit.Author.Name && r.mail == commit.Author.Email
}

// GetLastCommitShortHash gets the short hash of the latest commit in the Git repository.
func (r *BumperRepo) GetLastCommitShortHash() (string, error) {
	ref, err := r.git.Head()
	if err != nil {
		return "", err
	}

	return ref.Hash().String()[:13], nil
}

// getModifiedFiles gets a list of files modified in the Git repository commits after last Bump.
func (r *BumperRepo) getModifiedFiles(last bool) ([]string, error) {
	var modifiedFiles []string

	headRef, err := r.git.Head()
	if err != nil {
		return nil, err
	}

	headCommit, err := r.git.CommitObject(headRef.Hash())
	if err != nil {
		return nil, err
	}

	parentCommit, err := headCommit.Parent(0)
	if err != nil {
		return nil, err
	}

	commitsNum := 0
	for {
		if commitsNum > 0 {
			headCommit = parentCommit
		}

		headTree, _ := headCommit.Tree()

		parentCommit, err = headCommit.Parent(0)
		if err != nil {
			return nil, err
		}

		parentTree, _ := parentCommit.Tree()

		diff, err := parentTree.Diff(headTree)
		if err != nil {
			return nil, err
		}

		for _, ch := range diff {
			action, _ := ch.Action()
			var path string

			switch action {
			case merkletrie.Modify:
				path = ch.From.Name
			case merkletrie.Insert:
				path = ch.To.Name
			}

			if path != "" {
				modifiedFiles = append(modifiedFiles, path)
			}
		}

		if last {
			break
		}

		commitsNum++
		if strings.TrimSpace(parentCommit.Author.Name) == r.name {
			break
		}
	}

	return modifiedFiles, nil
}

// Commit stores the current changes to the Git repository with the default commit message and author.
func (r *BumperRepo) Commit() error {
	fmt.Println("Commit changes to updated resources")
	w, _ := r.git.Worktree()
	status, _ := w.Status()

	if status.IsClean() {
		fmt.Println("Nothing to commit")
		return nil
	}

	for path, s := range status {
		if s.Worktree == git.Modified {
			err := w.AddWithOptions(&git.AddOptions{
				Path:       path,
				SkipStatus: true,
			})
			if err != nil {
				fmt.Printf("Failed to add file to git: %v\n", err)
			}
		}
	}

	_, err := w.Commit(r.commitMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  r.name,
			Email: r.mail,
			When:  time.Now(),
		},
	})

	return err
}

// GetRepoName retrieves the name of the remote repository.
// It looks for the remote named "origin" and extracts the repository name from the remote's URL.
// It returns the repository name as a string and an error if the remote is not found or the repository name cannot be extracted.
func (r *BumperRepo) GetRepoName() (string, error) {
	remote, err := r.git.Remote("origin")
	if err != nil {
		return "", err
	}

	remoteItems := strings.Split(remote.String(), "\n")
	fetchRegexp, _ := regexp.Compile(`^origin.*[a-z]+ \(fetch\)$`)
	repoRegexp, _ := regexp.Compile(`.+/(.+)\.git`)
	for _, item := range remoteItems {
		if fetchRegexp.MatchString(strings.TrimSpace(item)) {
			return repoRegexp.FindStringSubmatch(item)[1], nil
		}
	}

	return "", errors.New("no remote was found")
}

// GetComparisonRef returns the short hash of the commit that contains the specified search message.
// If the commit is found, the function will return the short hash. Otherwise, if the commit is not found in the search,
// or if there is an error resolving the revision or fetching the commit log, an error will be returned.
func (r *BumperRepo) GetComparisonRef(searchMessage string) (string, error) {
	hash, err := r.git.ResolveRevision("HEAD~1")
	if err != nil {
		return "", err
	}

	cIter, err := r.git.Log(&git.LogOptions{From: *hash})
	if err != nil {
		return "", err
	}

	var commit []rune
	err = cIter.ForEach(func(c *object.Commit) error {
		if strings.Contains(c.Message, searchMessage) {
			commit = []rune(c.Hash.String())
			return storer.ErrStop
		}

		return nil
	})

	if err != nil {
		return "", err
	}

	if len(commit) == 0 {
		return "", errors.New("unable to determine comparison ref (couldn't compute it)")
	}

	shortCommit := string(commit[:7])
	return shortCommit, nil
}

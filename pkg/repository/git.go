// Package repository stores tools to work with git repository.
package repository

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/utils/merkletrie"
)

const (
	// BumpMessage is bump commit message.
	BumpMessage = "versions bump"
	// Author is the name of bump commit author.
	Author = "Bumper"
)

// Bumper encapsulates Git-related operations for bumping versions in a Git repository.
type Bumper struct {
	git           *git.Repository
	name          string
	mail          string
	commitMessage string
}

// NewBumper returns new instance of [Bumper].
func NewBumper() (*Bumper, error) {
	r, err := git.PlainOpen("./")
	if err != nil {
		return nil, err
	}

	return &Bumper{
		git:           r,
		name:          Author,
		mail:          "no-reply@skilld.cloud",
		commitMessage: BumpMessage,
	}, nil
}

// GetGit returns internal [*git.Repository]
func (r *Bumper) GetGit() *git.Repository {
	return r.git
}

// IsOwnCommit checks if the latest commit in the Git repository was made by the bumper.
func (r *Bumper) IsOwnCommit() bool {
	ref, err := r.git.Head()
	if err != nil {
		//plasmactlbump.PromptError(err)
		return false
	}

	commit, err := r.git.CommitObject(ref.Hash())
	if err != nil {
		//plasmactlbump.PromptError(err)
		return false
	}

	return r.name == commit.Author.Name && r.mail == commit.Author.Email
}

// GetLastCommitShortHash gets the short hash of the latest commit in the Git repository.
func (r *Bumper) GetLastCommitShortHash() (string, error) {
	ref, err := r.git.Head()
	if err != nil {
		return "", err
	}

	return ref.Hash().String()[:13], nil
}

// GetModifiedFiles gets a list of files modified in the Git repository commits after last Bump.
func (r *Bumper) GetModifiedFiles(last bool) ([]string, error) {
	var modifiedFiles []string

	headRef, err := r.git.Head()
	if err != nil {
		return nil, err
	}

	headCommit, err := r.git.CommitObject(headRef.Hash())
	if err != nil {
		return nil, err
	}

	commits, err := r.git.Log(&git.LogOptions{From: headRef.Hash()})
	if err != nil {
		return nil, err
	}

	var prevCommit *object.Commit
	commitsNum := 0

	err = commits.ForEach(func(commit *object.Commit) error {
		commitsNum++

		// Process last commit.
		if prevCommit == nil {
			prevCommit = commit
			return nil
		}

		// Process previous commits.
		prevTree, _ := prevCommit.Tree()
		currentTree, _ := commit.Tree()

		diff, diffErr := currentTree.Diff(prevTree)
		if diffErr != nil {
			return diffErr
		}

		for _, ch := range diff {
			action, _ := ch.Action()
			var path string

			switch action {
			case merkletrie.Delete:
				path = ch.From.Name
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
			return storer.ErrStop
		}
		if strings.TrimSpace(commit.Author.Name) == r.name {
			return storer.ErrStop
		}

		prevCommit = commit
		return nil
	})
	if err != nil {
		return nil, err
	}

	if commitsNum == 1 {
		headTree, _ := headCommit.Tree()
		err = headTree.Files().ForEach(func(file *object.File) error {
			modifiedFiles = append(modifiedFiles, file.Name)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return modifiedFiles, nil
}

// Commit stores the current changes to the Git repository with the default commit message and author.
func (r *Bumper) Commit() error {
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
func (r *Bumper) GetRepoName() (string, error) {
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

// GetComparisonCommit returns the commit that contains the specified search message.
func (r *Bumper) GetComparisonCommit(from plumbing.Hash, searchMessage string) (*plumbing.Hash, error) {
	cIter, err := r.git.Log(&git.LogOptions{From: from})
	if err != nil {
		return nil, err
	}

	var commit *plumbing.Hash
	err = cIter.ForEach(func(c *object.Commit) error {
		if c.Hash == from {
			return nil
		}

		if strings.TrimSpace(c.Message) == searchMessage {
			commit = &c.Hash
			return storer.ErrStop
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	if commit == nil {
		return nil, errors.New("unable to determine comparison commit (couldn't compute it)")
	}

	return commit, nil
}

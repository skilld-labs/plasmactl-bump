package plasmactlbump

import (
	"fmt"
	"time"

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
		commitMessage: "versions bump",
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

// getLatestModifiedFiles gets a list of files modified in the latest commit in the Git repository.
func (r *BumperRepo) getLatestModifiedFiles() ([]string, error) {
	var modifiedFiles []string

	headRef, err := r.git.Head()
	if err != nil {
		return nil, err
	}

	headCommit, err := r.git.CommitObject(headRef.Hash())
	if err != nil {
		return nil, err
	}

	headTree, _ := headCommit.Tree()

	parentCommit, err := headCommit.Parent(0)
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
			_, err := w.Add(path)
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

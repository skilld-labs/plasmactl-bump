package bumpupdated

import (
	"fmt"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// BumperRepo encapsulates Git-related operations for bumping versions in a Git repository.
type BumperRepo struct {
	git           *git.Repository
	name          string
	mail          string
	commitMessage string
}

func getRepo() *BumperRepo {
	r, err := git.PlainOpen("./")
	CheckIfError(err)

	return &BumperRepo{
		git:           r,
		name:          "Bumper",
		mail:          "no-reply@skilld.cloud",
		commitMessage: "versions bump",
	}
}

// IsOwnCommit checks if the latest commit in the Git repository was made by the bumper.
func (r *BumperRepo) IsOwnCommit() bool {
	ref, err := r.git.Head()
	CheckIfError(err)

	commit, err := r.git.CommitObject(ref.Hash())
	CheckIfError(err)

	return r.name == commit.Author.Name && r.mail == commit.Author.Email
}

// GetLastCommitShortHash gets the short hash of the latest commit in the Git repository.
func (r *BumperRepo) GetLastCommitShortHash() string {
	ref, err := r.git.Head()
	CheckIfError(err)

	return ref.Hash().String()[:13]
}

// getLatestModifiedFiles gets a list of files modified in the latest commit in the Git repository.
func (r *BumperRepo) getLatestModifiedFiles() ([]string, error) {
	var modifiedFiles []string

	headRef, err := r.git.Head()
	CheckIfError(err)

	headCommit, err := r.git.CommitObject(headRef.Hash())
	CheckIfError(err)
	headTree, _ := headCommit.Tree()

	parentCommit, err := headCommit.Parent(0)
	CheckIfError(err)
	parentTree, _ := parentCommit.Tree()

	diff, err := parentTree.Diff(headTree)
	CheckIfError(err)

	for _, ch := range diff {
		action, _ := ch.Action()
		if action.String() == "Modify" {
			modifiedFiles = append(modifiedFiles, ch.From.Name)
		}
	}

	return modifiedFiles, nil
}

// Commit stores the current changes to the Git repository with the default commit message and author.
func (r *BumperRepo) Commit() error {
	fmt.Println("Commits changes to updated resources")
	w, _ := r.git.Worktree()
	status, _ := w.Status()

	if status.IsClean() {
		fmt.Println("Nothing to commit")
		return nil
	}

	for path, s := range status {
		if s.Worktree == 'M' {
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

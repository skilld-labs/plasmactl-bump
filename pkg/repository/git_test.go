package repository

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// initTestRepo creates a temporary git repository with one commit.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	if err = os.WriteFile(filepath.Join(dir, "README.md"), []byte("test"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err = w.Add("README.md"); err != nil {
		t.Fatalf("git add: %v", err)
	}

	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	return dir
}

// initTestRepoWithResource creates a git repo with an Ansible role resource
// structure and multiple commits, including a bump commit.
func initTestRepoWithResource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	// Commit 1: initial resource with meta/plasma.yaml.
	metaDir := filepath.Join(dir, "interaction", "softwares", "roles", "grafana", "meta")
	if err = os.MkdirAll(metaDir, 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	tasksDir := filepath.Join(dir, "interaction", "softwares", "roles", "grafana", "tasks")
	if err = os.MkdirAll(tasksDir, 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	plasmaYaml := "plasma:\n  version: \"aaa1111111111\"\n  description: Test resource\n"
	if err = os.WriteFile(filepath.Join(metaDir, "plasma.yaml"), []byte(plasmaYaml), 0600); err != nil {
		t.Fatalf("write plasma.yaml: %v", err)
	}

	configYaml := "- include_role:\n    name: interaction.softwares.postgres\n"
	if err = os.WriteFile(filepath.Join(tasksDir, "configuration.yaml"), []byte(configYaml), 0600); err != nil {
		t.Fatalf("write configuration.yaml: %v", err)
	}

	if _, err = w.Add("."); err != nil {
		t.Fatalf("git add: %v", err)
	}

	_, err = w.Commit("add grafana resource", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Developer",
			Email: "dev@test.com",
			When:  time.Now().Add(-2 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("commit 1: %v", err)
	}

	// Commit 2: bump commit (by Bumper author).
	plasmaYaml = "plasma:\n  version: \"bbb2222222222\"\n  description: Test resource\n"
	if err = os.WriteFile(filepath.Join(metaDir, "plasma.yaml"), []byte(plasmaYaml), 0600); err != nil {
		t.Fatalf("write plasma.yaml: %v", err)
	}

	if _, err = w.Add("."); err != nil {
		t.Fatalf("git add: %v", err)
	}

	_, err = w.Commit(BumpMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  Author,
			Email: "no-reply@skilld.cloud",
			When:  time.Now().Add(-1 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("commit 2: %v", err)
	}

	// Commit 3: a regular change after bump.
	templateDir := filepath.Join(dir, "interaction", "softwares", "roles", "grafana", "templates")
	if err = os.MkdirAll(templateDir, 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err = os.WriteFile(filepath.Join(templateDir, "config.j2"), []byte("port={{ grafana_port }}"), 0600); err != nil {
		t.Fatalf("write template: %v", err)
	}

	if _, err = w.Add("."); err != nil {
		t.Fatalf("git add: %v", err)
	}

	_, err = w.Commit("update grafana template", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Developer",
			Email: "dev@test.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("commit 3: %v", err)
	}

	return dir
}

// createWorktree creates a git worktree using the git CLI.
func createWorktree(t *testing.T, repoDir string) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git CLI not available")
	}

	cmd := exec.Command("git", "branch", "test-wt")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch: %s: %v", out, err)
	}

	wtDir := filepath.Join(t.TempDir(), "wt")
	cmd = exec.Command("git", "worktree", "add", wtDir, "test-wt")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %s: %v", out, err)
	}

	return wtDir
}

func TestNewBumper(t *testing.T) {
	repoDir := initTestRepo(t)

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	if err = os.Chdir(repoDir); err != nil {
		t.Fatal(err)
	}

	bumper, err := NewBumper()
	if err != nil {
		t.Fatalf("NewBumper: %v", err)
	}

	if bumper.IsOwnCommit() {
		t.Error("expected IsOwnCommit() to be false")
	}

	commits, err := bumper.GetCommits(false)
	if err != nil {
		t.Fatalf("GetCommits: %v", err)
	}

	if len(commits) == 0 {
		t.Error("expected at least one commit")
	}
}

func TestNewBumperWorktree(t *testing.T) {
	repoDir := initTestRepo(t)
	wtDir := createWorktree(t, repoDir)

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	if err = os.Chdir(wtDir); err != nil {
		t.Fatal(err)
	}

	bumper, err := NewBumper()
	if err != nil {
		t.Fatalf("NewBumper in worktree: %v", err)
	}

	if bumper.IsOwnCommit() {
		t.Error("expected IsOwnCommit() to be false")
	}

	commits, err := bumper.GetCommits(false)
	if err != nil {
		t.Fatalf("GetCommits in worktree: %v", err)
	}

	if len(commits) == 0 {
		t.Error("expected at least one commit")
	}
}

func TestPlainOpenWithOptionsRegularRepo(t *testing.T) {
	repoDir := initTestRepo(t)

	repo, err := git.PlainOpenWithOptions(repoDir, &git.PlainOpenOptions{EnableDotGitCommonDir: true})
	if err != nil {
		t.Fatalf("PlainOpenWithOptions on regular repo: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}

	_, err = repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("CommitObject: %v", err)
	}
}

func TestPlainOpenWithOptionsWorktree(t *testing.T) {
	repoDir := initTestRepo(t)
	wtDir := createWorktree(t, repoDir)

	repo, err := git.PlainOpenWithOptions(wtDir, &git.PlainOpenOptions{EnableDotGitCommonDir: true})
	if err != nil {
		t.Fatalf("PlainOpenWithOptions on worktree: %v", err)
	}

	// Verify commit objects are accessible (requires shared objects database from main repo).
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}

	_, err = repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("CommitObject in worktree: %v", err)
	}

	// Verify log iteration works (used by sync timeline population).
	cIter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		t.Fatalf("Log in worktree: %v", err)
	}

	count := 0
	err = cIter.ForEach(func(c *object.Commit) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("Log.ForEach in worktree: %v", err)
	}

	if count == 0 {
		t.Error("expected at least one commit from log iteration")
	}
}

// TestBumpWorkflowInWorktree is an integration test that exercises the full
// bump workflow from a git worktree: opening the repo, detecting the bumper
// commit, collecting commits with changed files, and accessing file contents
// from commit objects (the pattern used by sync/propagation).
func TestBumpWorkflowInWorktree(t *testing.T) {
	repoDir := initTestRepoWithResource(t)
	wtDir := createWorktree(t, repoDir)

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	if err = os.Chdir(wtDir); err != nil {
		t.Fatal(err)
	}

	// Phase 1: Bumper detects own commit correctly.
	bumper, err := NewBumper()
	if err != nil {
		t.Fatalf("NewBumper in worktree: %v", err)
	}

	// HEAD is a developer commit (commit 3), not a bump commit.
	if bumper.IsOwnCommit() {
		t.Error("expected IsOwnCommit() to be false for developer commit")
	}

	// Phase 2: GetCommits returns only the post-bump commit (stops at Bumper author).
	commits, err := bumper.GetCommits(false)
	if err != nil {
		t.Fatalf("GetCommits: %v", err)
	}

	if len(commits) == 0 {
		t.Fatal("expected at least one commit")
	}

	// The commit after bump should contain the template file change.
	foundTemplate := false
	for _, c := range commits {
		for _, f := range c.Files {
			if filepath.Base(f) == "config.j2" {
				foundTemplate = true
			}
		}
	}

	if !foundTemplate {
		t.Error("expected to find config.j2 in post-bump commits")
	}

	// Phase 3: Sync patterns - open by path, access commit objects and files.
	// This mirrors what findResourcesChangeTime and processResource do.
	repo, err := git.PlainOpenWithOptions(wtDir, &git.PlainOpenOptions{EnableDotGitCommonDir: true})
	if err != nil {
		t.Fatalf("PlainOpenWithOptions: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}

	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("CommitObject: %v", err)
	}

	// Access resource file from commit tree (pattern from processResource).
	metaPath := filepath.Join("interaction", "softwares", "roles", "grafana", "meta", "plasma.yaml")
	file, err := headCommit.File(metaPath)
	if err != nil {
		t.Fatalf("File(%s) from commit: %v", metaPath, err)
	}

	contents, err := file.Contents()
	if err != nil {
		t.Fatalf("file.Contents: %v", err)
	}

	if contents == "" {
		t.Error("expected non-empty plasma.yaml contents")
	}

	// Phase 4: Log iteration with author detection (pattern from collectResourcesCommits).
	cIter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}

	var bumperCommits, devCommits int
	err = cIter.ForEach(func(c *object.Commit) error {
		if c.Author.Name == Author {
			bumperCommits++
		} else {
			devCommits++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Log.ForEach: %v", err)
	}

	if bumperCommits != 1 {
		t.Errorf("expected 1 bumper commit, got %d", bumperCommits)
	}

	if devCommits != 2 {
		t.Errorf("expected 2 developer commits, got %d", devCommits)
	}

	// Phase 5: Tree diff between commits (pattern from GetCommits).
	cIter, err = repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}

	var prevCommit *object.Commit
	diffFound := false

	err = cIter.ForEach(func(c *object.Commit) error {
		if prevCommit == nil {
			prevCommit = c
			return nil
		}

		prevTree, errT := prevCommit.Tree()
		if errT != nil {
			return errT
		}

		currentTree, errT := c.Tree()
		if errT != nil {
			return errT
		}

		diff, errT := currentTree.Diff(prevTree)
		if errT != nil {
			return errT
		}

		if len(diff) > 0 {
			diffFound = true
		}

		prevCommit = c
		return storer.ErrStop
	})
	if err != nil {
		t.Fatalf("Log.ForEach diff: %v", err)
	}

	if !diffFound {
		t.Error("expected to find diffs between commits")
	}
}

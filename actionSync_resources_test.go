package plasmactlbump

import (
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/skilld-labs/plasmactl-bump/v2/pkg/repository"
)

// commitOpts defines a single commit to add to the test repo.
type commitOpts struct {
	message string
	author  string
	date    time.Time
}

// buildTestRepo creates an in-memory git repo with the given sequence of commits (oldest first).
// Returns the repo and the commit hashes in the same order.
func buildTestRepo(t *testing.T, commits []commitOpts) (*git.Repository, []string) {
	t.Helper()

	r, err := git.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatalf("git.Init: %v", err)
	}

	wt, err := r.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	var hashes []string
	for i, c := range commits {
		// Write a unique file so each commit has a non-empty tree diff.
		f, errF := wt.Filesystem.Create("file" + string(rune('a'+i)))
		if errF != nil {
			t.Fatalf("create file: %v", errF)
		}
		_, _ = f.Write([]byte(c.message))
		_ = f.Close()

		_, _ = wt.Add(".")

		sig := &object.Signature{Name: c.author, Email: c.author + "@test", When: c.date}
		hash, errC := wt.Commit(c.message, &git.CommitOptions{Author: sig, Committer: sig})
		if errC != nil {
			t.Fatalf("commit: %v", errC)
		}
		hashes = append(hashes, hash.String())
	}

	return r, hashes
}

func TestCollectResourcesCommits_NoBumps(t *testing.T) {
	// No bump commits → single "head" group containing all commits.
	d := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	r, hashes := buildTestRepo(t, []commitOpts{
		{message: "initial", author: "Developer", date: d},
		{message: "change A", author: "Developer", date: d.Add(time.Hour)},
		{message: "change B", author: "Developer", date: d.Add(2 * time.Hour)},
	})

	groups, hashesMap, err := collectResourcesCommits(r, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if groups.Len() != 1 {
		t.Fatalf("expected 1 group, got %d", groups.Len())
	}

	keys := groups.Keys()
	g, _ := groups.Get(keys[0])
	if g.name != headGroupName {
		t.Errorf("group name = %q, want %q", g.name, headGroupName)
	}
	// All 3 commits belong to the head group (HEAD + 2 earlier).
	if len(g.items) != 3 {
		t.Errorf("items count = %d, want 3", len(g.items))
	}
	// The group is keyed by the HEAD commit's full hash.
	headHash := hashes[2]
	if keys[0] != headHash {
		t.Errorf("group key = %q, want HEAD hash %s", keys[0], headHash[:13])
	}
	// HEAD commit itself is assigned sectionName = headGroupName.
	if hashesMap[headHash[:13]]["section"] != headGroupName {
		t.Errorf("HEAD hash section = %q, want %q", hashesMap[headHash[:13]]["section"], headGroupName)
	}
	// Earlier commits are assigned to the HEAD full hash as the section key.
	for _, h := range hashes[:2] {
		short := h[:13]
		if hashesMap[short]["section"] != headHash {
			t.Errorf("hash %s section = %q, want HEAD hash %s", short, hashesMap[short]["section"], headHash[:13])
		}
	}
}

func TestCollectResourcesCommits_HeadIsBump(t *testing.T) {
	// HEAD is a bump commit → it starts its own section, no items yet in head.
	d := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	r, hashes := buildTestRepo(t, []commitOpts{
		{message: "initial", author: "Developer", date: d},
		{message: "change", author: "Developer", date: d.Add(time.Hour)},
		{message: "[bump]", author: repository.Author, date: d.Add(2 * time.Hour)},
	})

	groups, hashesMap, err := collectResourcesCommits(r, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// One group: the bump section containing the two developer commits.
	if groups.Len() != 1 {
		t.Fatalf("expected 1 group, got %d", groups.Len())
	}

	bumpHash := hashes[2]
	g, ok := groups.Get(bumpHash)
	if !ok {
		t.Fatalf("expected group keyed by bump commit %s", bumpHash[:13])
	}
	if len(g.items) != 2 {
		t.Errorf("bump section items = %d, want 2 (the two developer commits)", len(g.items))
	}
	// HEAD bump hash is assigned to its own section name.
	if hashesMap[bumpHash[:13]]["section"] != bumpHash {
		t.Errorf("bump hash section = %q, want %q", hashesMap[bumpHash[:13]]["section"], bumpHash)
	}
	// Developer commits are in the bump section.
	for _, h := range hashes[:2] {
		if hashesMap[h[:13]]["section"] != bumpHash {
			t.Errorf("dev commit %s section = %q, want bump %s", h[:13], hashesMap[h[:13]]["section"], bumpHash[:13])
		}
	}
}

func TestCollectResourcesCommits_TwoBumps(t *testing.T) {
	// Two bump commits → two sections plus a head group.
	// Timeline (oldest→newest):
	//   initial (dev) → change-1 (dev) → bump-1 (Bumper) → change-2 (dev) → bump-2 (Bumper) → change-3 (dev)
	d := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	r, hashes := buildTestRepo(t, []commitOpts{
		{message: "initial", author: "Developer", date: d},
		{message: "change-1", author: "Developer", date: d.Add(time.Hour)},
		{message: "[bump]", author: repository.Author, date: d.Add(2 * time.Hour)},
		{message: "change-2", author: "Developer", date: d.Add(3 * time.Hour)},
		{message: "[bump]", author: repository.Author, date: d.Add(4 * time.Hour)},
		{message: "change-3", author: "Developer", date: d.Add(5 * time.Hour)},
	})

	groups, hashesMap, err := collectResourcesCommits(r, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 3 groups: head, bump-2, bump-1.
	if groups.Len() != 3 {
		t.Fatalf("expected 3 groups, got %d", groups.Len())
	}

	bump1Hash := hashes[2]
	bump2Hash := hashes[4]

	// bump-1 section contains: initial + change-1.
	g1, ok := groups.Get(bump1Hash)
	if !ok {
		t.Fatalf("bump-1 group not found")
	}
	if len(g1.items) != 2 {
		t.Errorf("bump-1 items = %d, want 2", len(g1.items))
	}

	// bump-2 section contains: change-2.
	g2, ok := groups.Get(bump2Hash)
	if !ok {
		t.Fatalf("bump-2 group not found")
	}
	if len(g2.items) != 1 {
		t.Errorf("bump-2 items = %d, want 1", len(g2.items))
	}

	// Head section: keyed by HEAD hash (hashes[5]), contains change-3.
	headKey := hashes[5]
	g3, ok := groups.Get(headKey)
	if !ok {
		t.Fatalf("head group not found")
	}
	if g3.name != headGroupName {
		t.Errorf("head group name = %q, want %q", g3.name, headGroupName)
	}
	if len(g3.items) != 1 {
		t.Errorf("head items = %d, want 1", len(g3.items))
	}

	// Verify section assignments in hashesMap.
	for _, h := range hashes[:2] {
		if hashesMap[h[:13]]["section"] != bump1Hash {
			t.Errorf("commit %s should be in bump-1 section", h[:13])
		}
	}
	if hashesMap[hashes[3][:13]]["section"] != bump2Hash {
		t.Errorf("change-2 should be in bump-2 section")
	}
	// HEAD commit (change-3) gets sectionName = headGroupName in hashesMap.
	if hashesMap[hashes[5][:13]]["section"] != headGroupName {
		t.Errorf("change-3 should be in head section")
	}
}

func TestCollectResourcesCommits_BeforeDate(t *testing.T) {
	// beforeDate filters out commits older than the cutoff.
	d := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	r, hashes := buildTestRepo(t, []commitOpts{
		{message: "old commit", author: "Developer", date: d},
		{message: "new commit", author: "Developer", date: d.Add(48 * time.Hour)},
	})

	// Cut off before d+48h → only "new commit" visible.
	groups, hashesMap, err := collectResourcesCommits(r, "2000-01-02")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if groups.Len() != 1 {
		t.Fatalf("expected 1 group, got %d", groups.Len())
	}

	// Old commit should not appear in hashesMap.
	if _, ok := hashesMap[hashes[0][:13]]; ok {
		t.Error("old commit should be filtered out by beforeDate")
	}
	// New commit should be present.
	if _, ok := hashesMap[hashes[1][:13]]; !ok {
		t.Error("new commit should be present")
	}
}
